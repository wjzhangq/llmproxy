// Package report computes daily statistics from metrics logs and produces
// the AI-written analysis report.
package report

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"sort"
	"time"

	"github.com/wjzhangq/llmproxy/internal/logstore"
	"github.com/wjzhangq/llmproxy/internal/metrics"
)

// Dist summarises a distribution. Fields not computed for a metric are nil.
type Dist struct {
	Count int      `json:"count"`
	Avg   *float64 `json:"avg"`
	P5    *float64 `json:"p5,omitempty"`
	P50   *float64 `json:"p50,omitempty"`
	P95   *float64 `json:"p95,omitempty"`
	P99   *float64 `json:"p99,omitempty"`
	Max   *float64 `json:"max,omitempty"`
}

// Overall holds day-level totals.
type Overall struct {
	Requests      int     `json:"requests"`
	Success       int     `json:"success"`
	Errors        int     `json:"errors"`
	ErrorRate     float64 `json:"error_rate"`
	ClientAborted int     `json:"client_aborted"`
	StreamRatio   float64 `json:"stream_ratio"`
	LogDropped    int64   `json:"log_dropped"`
	// TPSPerRequestAvg is the arithmetic mean of per-request tokens_per_sec.
	TPSPerRequestAvg *float64 `json:"tps_per_request_avg"`
	// ThroughputTPS is sum(completion_tokens) / sum(total_ms) in seconds.
	ThroughputTPS *float64 `json:"throughput_tps"`
}

// Group is the breakdown for one model × stream combination.
type Group struct {
	Model                 string   `json:"model"`
	Stream                bool     `json:"stream"`
	Requests              int      `json:"requests"`
	Errors                int      `json:"errors"`
	ClientAborted         int      `json:"client_aborted"`
	TotalMs               Dist     `json:"total_ms"`
	TTFTMs                *Dist    `json:"ttft_ms"`
	TokensPerSec          Dist     `json:"tokens_per_sec"`
	TPSPerRequestAvg      *float64 `json:"tps_per_request_avg"`
	ThroughputTPS         *float64 `json:"throughput_tps"`
	DecodeTPS             *Dist    `json:"decode_tps,omitempty"`
	PromptTokensTotal     int64    `json:"prompt_tokens_total"`
	CompletionTokensTotal int64    `json:"completion_tokens_total"`
	ReqBytes              Dist     `json:"req_content_bytes"`
	RespBytes             Dist     `json:"resp_content_bytes"`
}

// Hour is the breakdown for one hour of the day.
type Hour struct {
	Hour          int      `json:"hour"`
	Requests      int      `json:"requests"`
	AvgTTFTMs     *float64 `json:"avg_ttft_ms"`
	AvgTokensPerS *float64 `json:"avg_tokens_per_sec"`
}

// Bucket is the breakdown for one request size bucket.
type Bucket struct {
	Bucket    string   `json:"bucket"`
	Requests  int      `json:"requests"`
	AvgTTFTMs *float64 `json:"avg_ttft_ms"`
	AvgTotal  *float64 `json:"avg_total_ms"`
}

// Slow is one entry of a top-N slowest list.
type Slow struct {
	RequestID        string `json:"request_id"`
	StartTime        string `json:"start_time"`
	Model            string `json:"model"`
	Stream           bool   `json:"stream"`
	StatusCode       int    `json:"status_code"`
	TotalMs          int64  `json:"total_ms"`
	TTFTMs           *int64 `json:"ttft_ms"`
	ReqContentBytes  int64  `json:"req_content_bytes"`
	PromptTokens     *int64 `json:"prompt_tokens"`
	CompletionTokens *int64 `json:"completion_tokens"`
}

// Stats is the content of reports/YYYY-MM-DD.json.
type Stats struct {
	Date         string   `json:"date"`
	Timezone     string   `json:"timezone"`
	GeneratedAt  string   `json:"generated_at"`
	Overall      Overall  `json:"overall"`
	Groups       []Group  `json:"groups"`
	Hourly       []Hour   `json:"hourly"`
	SizeBuckets  []Bucket `json:"size_buckets"`
	SlowestTotal []Slow   `json:"top_slowest_total"`
	SlowestTTFT  []Slow   `json:"top_slowest_ttft"`
}

// ErrNoData is returned when no metrics file exists for the requested day.
var ErrNoData = errors.New("no metrics data")

// ReadRecords loads all metric records of day. It is meant for small days
// and tests; Generate streams the file through an Aggregator instead.
func ReadRecords(dir, day string) ([]*metrics.Record, error) {
	var out []*metrics.Record
	err := scanDay(dir, day, func(r *metrics.Record) {
		c := *r
		out = append(out, &c)
	})
	return out, err
}

// scanDay calls fn for every record of day, reusing one Record value.
func scanDay(dir, day string, fn func(*metrics.Record)) error {
	rc, err := logstore.OpenDay(dir, logstore.MetricsDir, day)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNoData
		}
		return err
	}
	defer rc.Close()
	return parseRecords(rc, fn)
}

func parseRecords(r io.Reader, fn func(*metrics.Record)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	var rec metrics.Record
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		rec = metrics.Record{}
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue // skip a torn line rather than failing the whole day
		}
		fn(&rec)
	}
	return sc.Err()
}

var bucketBounds = []struct {
	name string
	max  int64
}{
	{"<1KB", 1 << 10}, {"1-4KB", 4 << 10}, {"4-16KB", 16 << 10}, {"16-64KB", 64 << 10}, {">64KB", math.MaxInt64},
}

func isError(r *metrics.Record) bool {
	return !r.ClientAborted && (r.StatusCode >= 400 || r.StatusCode == 0 || r.Error != "")
}

// meanAcc accumulates an average of optional values.
type meanAcc struct {
	sum float64
	n   int
}

func (m *meanAcc) add(v float64) { m.sum += v; m.n++ }

func (m *meanAcc) value() *float64 {
	if m.n == 0 {
		return nil
	}
	v := round(m.sum/float64(m.n), 2)
	return &v
}

// tpsAcc accumulates both tokens/s definitions.
type tpsAcc struct {
	perReq meanAcc
	tokens int64
	ms     int64
}

func (t *tpsAcc) add(r *metrics.Record) {
	if r.TokensPerSec == nil || r.CompletionTokens == nil {
		return
	}
	t.perReq.add(*r.TokensPerSec)
	t.tokens += *r.CompletionTokens
	t.ms += r.TotalMs
}

// values returns the per-request mean of tokens_per_sec and the aggregate
// throughput sum(completion_tokens)/sum(total seconds), both over requests
// that reported usage.
func (t *tpsAcc) values() (*float64, *float64) {
	if t.perReq.n == 0 {
		return nil, nil
	}
	var thr *float64
	if t.ms > 0 {
		v := round(float64(t.tokens)/(float64(t.ms)/1000), 2)
		thr = &v
	}
	return t.perReq.value(), thr
}

// series keeps the values of one metric for percentiles. float32 halves the
// memory; the stored values (milliseconds, bytes, rates rounded to 0.01) fit
// with ample precision, and sums are kept in float64.
type series struct {
	v   []float32
	sum float64
}

func (s *series) add(v float64) {
	s.v = append(s.v, float32(v))
	s.sum += v
}

type groupAcc struct {
	model                 string
	stream                bool
	requests, errors, abt int
	total, ttft, tps, dec series
	req, resp             series
	promptTok, complTok   int64
	tpsBoth               tpsAcc
}

type hourAcc struct {
	n         int
	ttft, tps meanAcc
}

type bucketAcc struct {
	n           int
	ttft, total meanAcc
}

// Aggregator computes daily statistics from records fed one at a time, so a
// day never has to be held in memory as records.
type Aggregator struct {
	requests, success, errors, aborted, streams int
	tps                                         tpsAcc
	groups                                      map[groupKey]*groupAcc
	hours                                       [24]hourAcc
	buckets                                     []bucketAcc
	slowTotal, slowTTFT                         topList
	loc                                         *time.Location
	seq                                         int
}

type groupKey struct {
	model  string
	stream bool
}

// NewAggregator returns an empty Aggregator bucketing hours in loc.
func NewAggregator(loc *time.Location) *Aggregator {
	return &Aggregator{
		groups:    map[groupKey]*groupAcc{},
		buckets:   make([]bucketAcc, len(bucketBounds)),
		slowTotal: topList{n: 10},
		slowTTFT:  topList{n: 10},
		loc:       loc,
	}
}

// Add accounts one record. r may be reused by the caller afterwards.
func (a *Aggregator) Add(r *metrics.Record) {
	a.seq++
	a.requests++
	switch {
	case r.ClientAborted:
		a.aborted++
	case isError(r):
		a.errors++
	default:
		a.success++
	}
	if r.Stream {
		a.streams++
	}
	a.tps.add(r)

	k := groupKey{r.Model, r.Stream}
	g := a.groups[k]
	if g == nil {
		g = &groupAcc{model: r.Model, stream: r.Stream}
		a.groups[k] = g
	}
	g.requests++
	if r.ClientAborted {
		g.abt++
	} else if isError(r) {
		g.errors++
	}
	g.total.add(float64(r.TotalMs))
	if r.TTFTMs != nil {
		g.ttft.add(float64(*r.TTFTMs))
	}
	if r.TokensPerSec != nil {
		g.tps.add(*r.TokensPerSec)
	}
	if r.DecodeTPS != nil {
		g.dec.add(*r.DecodeTPS)
	}
	if r.PromptTokens != nil {
		g.promptTok += *r.PromptTokens
	}
	if r.CompletionTokens != nil {
		g.complTok += *r.CompletionTokens
	}
	g.req.add(float64(r.ReqContentBytes))
	g.resp.add(float64(r.RespContentBytes))
	g.tpsBoth.add(r)

	h := &a.hours[r.StartTime.In(a.loc).Hour()]
	h.n++
	if r.TTFTMs != nil {
		h.ttft.add(float64(*r.TTFTMs))
	}
	if r.TokensPerSec != nil {
		h.tps.add(*r.TokensPerSec)
	}

	for i, b := range bucketBounds {
		lo := int64(0)
		if i > 0 {
			lo = bucketBounds[i-1].max
		}
		if r.ReqContentBytes >= lo && r.ReqContentBytes < b.max {
			bk := &a.buckets[i]
			bk.n++
			if r.TTFTMs != nil {
				bk.ttft.add(float64(*r.TTFTMs))
			}
			bk.total.add(float64(r.TotalMs))
			break
		}
	}

	a.slowTotal.offer(r.TotalMs, a.seq, r)
	if r.TTFTMs != nil {
		a.slowTTFT.offer(*r.TTFTMs, a.seq, r)
	}
}

// Stats returns the statistics accumulated so far.
func (a *Aggregator) Stats(day string, dropped int64, now time.Time) *Stats {
	s := &Stats{Date: day, Timezone: a.loc.String(), GeneratedAt: now.In(a.loc).Format(time.RFC3339)}
	o := &s.Overall
	o.Requests, o.Success, o.Errors, o.ClientAborted, o.LogDropped = a.requests, a.success, a.errors, a.aborted, dropped
	if o.Requests > 0 {
		o.ErrorRate = round(float64(o.Errors)/float64(o.Requests), 4)
		o.StreamRatio = round(float64(a.streams)/float64(o.Requests), 4)
	}
	o.TPSPerRequestAvg, o.ThroughputTPS = a.tps.values()

	keys := make([]groupKey, 0, len(a.groups))
	for k := range a.groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].model != keys[j].model {
			return keys[i].model < keys[j].model
		}
		return !keys[i].stream && keys[j].stream
	})
	for _, k := range keys {
		s.Groups = append(s.Groups, a.groups[k].build())
	}
	for h := range a.hours {
		ha := &a.hours[h]
		if ha.n > 0 {
			s.Hourly = append(s.Hourly, Hour{Hour: h, Requests: ha.n, AvgTTFTMs: ha.ttft.value(), AvgTokensPerS: ha.tps.value()})
		}
	}
	for i, b := range bucketBounds {
		bk := &a.buckets[i]
		s.SizeBuckets = append(s.SizeBuckets, Bucket{Bucket: b.name, Requests: bk.n, AvgTTFTMs: bk.ttft.value(), AvgTotal: bk.total.value()})
	}
	s.SlowestTotal = a.slowTotal.sorted()
	s.SlowestTTFT = a.slowTTFT.sorted()
	return s
}

func (g *groupAcc) build() Group {
	out := Group{
		Model: g.model, Stream: g.stream, Requests: g.requests, Errors: g.errors, ClientAborted: g.abt,
		PromptTokensTotal: g.promptTok, CompletionTokensTotal: g.complTok,
	}
	out.TotalMs = g.total.dist("avg", "p50", "p95", "p99", "max")
	if g.stream {
		d := g.ttft.dist("avg", "p50", "p95", "p99", "max")
		out.TTFTMs = &d
		if len(g.dec.v) > 0 {
			dd := g.dec.dist("avg", "p50", "p5")
			out.DecodeTPS = &dd
		}
	}
	out.TokensPerSec = g.tps.dist("avg", "p50", "p5")
	out.TPSPerRequestAvg, out.ThroughputTPS = g.tpsBoth.values()
	out.ReqBytes = g.req.dist("avg", "p95")
	out.RespBytes = g.resp.dist("avg", "p95")
	return out
}

// Compute builds the statistics for day from recs.
func Compute(recs []*metrics.Record, day string, loc *time.Location, dropped int64, now time.Time) *Stats {
	a := NewAggregator(loc)
	for _, r := range recs {
		a.Add(r)
	}
	return a.Stats(day, dropped, now)
}

// dist sorts the series in place and summarises it.
func (s *series) dist(fields ...string) Dist {
	d := Dist{Count: len(s.v)}
	if len(s.v) == 0 {
		return d
	}
	sorted := s.v
	slices.Sort(sorted)
	p := func(q float64) *float64 {
		// nearest-rank percentile
		idx := int(math.Ceil(q/100*float64(len(sorted)))) - 1
		if idx < 0 {
			idx = 0
		}
		v := round(float64(sorted[idx]), 2)
		return &v
	}
	for _, f := range fields {
		switch f {
		case "avg":
			a := round(s.sum/float64(len(sorted)), 2)
			d.Avg = &a
		case "p5":
			d.P5 = p(5)
		case "p50":
			d.P50 = p(50)
		case "p95":
			d.P95 = p(95)
		case "p99":
			d.P99 = p(99)
		case "max":
			m := round(float64(sorted[len(sorted)-1]), 2)
			d.Max = &m
		}
	}
	return d
}

// topList keeps the n largest entries by value; ties keep the earliest.
type topList struct {
	n     int
	items []topItem
}

type topItem struct {
	v    int64
	seq  int
	slow Slow
}

// less orders "smaller" (worse) entries first: lower value, then later seq.
func (t topItem) less(o topItem) bool {
	if t.v != o.v {
		return t.v < o.v
	}
	return t.seq > o.seq
}

func (t *topList) offer(v int64, seq int, r *metrics.Record) {
	item := topItem{v: v, seq: seq}
	if len(t.items) == t.n && !t.items[0].less(item) {
		return
	}
	item.slow = Slow{
		RequestID: r.RequestID, StartTime: r.StartTime.Format(metrics.TimeFormat), Model: r.Model, Stream: r.Stream,
		StatusCode: r.StatusCode, TotalMs: r.TotalMs, TTFTMs: copyInt(r.TTFTMs), ReqContentBytes: r.ReqContentBytes,
		PromptTokens: copyInt(r.PromptTokens), CompletionTokens: copyInt(r.CompletionTokens),
	}
	if len(t.items) < t.n {
		t.items = append(t.items, item)
	} else {
		t.items[0] = item
	}
	// keep items[0] the minimum (n is tiny, a linear pass is enough)
	minI := 0
	for i := range t.items {
		if t.items[i].less(t.items[minI]) {
			minI = i
		}
	}
	t.items[0], t.items[minI] = t.items[minI], t.items[0]
}

func (t *topList) sorted() []Slow {
	items := append([]topItem(nil), t.items...)
	sort.Slice(items, func(i, j int) bool { return items[j].less(items[i]) })
	out := make([]Slow, len(items))
	for i, it := range items {
		out[i] = it.slow
	}
	return out
}

func copyInt(p *int64) *int64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func round(f float64, digits int) float64 {
	p := math.Pow(10, float64(digits))
	return math.Round(f*p) / p
}

// fmtF formats an optional float for Markdown tables.
func fmtF(p *float64, unit string) string {
	if p == nil {
		return "-"
	}
	if *p >= 100 {
		return fmt.Sprintf("%.0f%s", *p, unit)
	}
	return fmt.Sprintf("%.1f%s", *p, unit)
}
