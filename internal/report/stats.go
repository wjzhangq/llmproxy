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

// ReadRecords loads all metric records of day.
func ReadRecords(dir, day string) ([]*metrics.Record, error) {
	rc, err := logstore.OpenDay(dir, logstore.MetricsDir, day)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNoData
		}
		return nil, err
	}
	defer rc.Close()
	return parseRecords(rc)
}

func parseRecords(r io.Reader) ([]*metrics.Record, error) {
	var out []*metrics.Record
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		rec := &metrics.Record{}
		if err := json.Unmarshal(sc.Bytes(), rec); err != nil {
			continue // skip a torn line rather than failing the whole day
		}
		out = append(out, rec)
	}
	return out, sc.Err()
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

// Compute builds the statistics for day from recs.
func Compute(recs []*metrics.Record, day string, loc *time.Location, dropped int64, now time.Time) *Stats {
	s := &Stats{Date: day, Timezone: loc.String(), GeneratedAt: now.In(loc).Format(time.RFC3339)}
	o := &s.Overall
	o.Requests = len(recs)
	o.LogDropped = dropped
	streams := 0
	for _, r := range recs {
		switch {
		case r.ClientAborted:
			o.ClientAborted++
		case isError(r):
			o.Errors++
		default:
			o.Success++
		}
		if r.Stream {
			streams++
		}
	}
	if o.Requests > 0 {
		o.ErrorRate = round(float64(o.Errors)/float64(o.Requests), 4)
		o.StreamRatio = round(float64(streams)/float64(o.Requests), 4)
	}
	o.TPSPerRequestAvg, o.ThroughputTPS = tpsBoth(recs)

	// Groups by model × stream.
	type key struct {
		model  string
		stream bool
	}
	groups := map[key][]*metrics.Record{}
	for _, r := range recs {
		k := key{r.Model, r.Stream}
		groups[k] = append(groups[k], r)
	}
	keys := make([]key, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].model != keys[j].model {
			return keys[i].model < keys[j].model
		}
		return !keys[i].stream && keys[j].stream
	})
	for _, k := range keys {
		s.Groups = append(s.Groups, buildGroup(k.model, k.stream, groups[k]))
	}

	// Hourly.
	byHour := map[int][]*metrics.Record{}
	for _, r := range recs {
		h := r.StartTime.In(loc).Hour()
		byHour[h] = append(byHour[h], r)
	}
	for h := 0; h < 24; h++ {
		rs := byHour[h]
		if len(rs) == 0 {
			continue
		}
		s.Hourly = append(s.Hourly, Hour{Hour: h, Requests: len(rs), AvgTTFTMs: avgTTFT(rs), AvgTokensPerS: avgOf(rs, tpsVal)})
	}

	// Size buckets.
	for i, b := range bucketBounds {
		var rs []*metrics.Record
		lo := int64(0)
		if i > 0 {
			lo = bucketBounds[i-1].max
		}
		for _, r := range recs {
			if r.ReqContentBytes >= lo && r.ReqContentBytes < b.max {
				rs = append(rs, r)
			}
		}
		s.SizeBuckets = append(s.SizeBuckets, Bucket{Bucket: b.name, Requests: len(rs), AvgTTFTMs: avgTTFT(rs), AvgTotal: avgOf(rs, func(r *metrics.Record) *float64 { v := float64(r.TotalMs); return &v })})
	}

	s.SlowestTotal = topN(recs, 10, func(r *metrics.Record) (int64, bool) { return r.TotalMs, true })
	s.SlowestTTFT = topN(recs, 10, func(r *metrics.Record) (int64, bool) {
		if r.TTFTMs == nil {
			return 0, false
		}
		return *r.TTFTMs, true
	})
	return s
}

func buildGroup(model string, stream bool, rs []*metrics.Record) Group {
	g := Group{Model: model, Stream: stream, Requests: len(rs)}
	var total, ttft, tps, decode, req, resp []float64
	for _, r := range rs {
		if r.ClientAborted {
			g.ClientAborted++
		} else if isError(r) {
			g.Errors++
		}
		total = append(total, float64(r.TotalMs))
		if r.TTFTMs != nil {
			ttft = append(ttft, float64(*r.TTFTMs))
		}
		if r.TokensPerSec != nil {
			tps = append(tps, *r.TokensPerSec)
		}
		if r.DecodeTPS != nil {
			decode = append(decode, *r.DecodeTPS)
		}
		if r.PromptTokens != nil {
			g.PromptTokensTotal += *r.PromptTokens
		}
		if r.CompletionTokens != nil {
			g.CompletionTokensTotal += *r.CompletionTokens
		}
		req = append(req, float64(r.ReqContentBytes))
		resp = append(resp, float64(r.RespContentBytes))
	}
	g.TotalMs = dist(total, "avg", "p50", "p95", "p99", "max")
	if stream {
		d := dist(ttft, "avg", "p50", "p95", "p99", "max")
		g.TTFTMs = &d
		if len(decode) > 0 {
			dd := dist(decode, "avg", "p50", "p5")
			g.DecodeTPS = &dd
		}
	}
	g.TokensPerSec = dist(tps, "avg", "p50", "p5")
	g.TPSPerRequestAvg, g.ThroughputTPS = tpsBoth(rs)
	g.ReqBytes = dist(req, "avg", "p95")
	g.RespBytes = dist(resp, "avg", "p95")
	return g
}

// tpsBoth returns the per-request mean of tokens_per_sec and the aggregate
// throughput sum(completion_tokens)/sum(total seconds), both over requests
// that reported usage.
func tpsBoth(rs []*metrics.Record) (*float64, *float64) {
	var sum float64
	var n int
	var tokens, ms int64
	for _, r := range rs {
		if r.TokensPerSec == nil || r.CompletionTokens == nil {
			continue
		}
		sum += *r.TokensPerSec
		n++
		tokens += *r.CompletionTokens
		ms += r.TotalMs
	}
	if n == 0 {
		return nil, nil
	}
	avg := round(sum/float64(n), 2)
	var thr *float64
	if ms > 0 {
		v := round(float64(tokens)/(float64(ms)/1000), 2)
		thr = &v
	}
	return &avg, thr
}

func tpsVal(r *metrics.Record) *float64 { return r.TokensPerSec }

func avgTTFT(rs []*metrics.Record) *float64 {
	return avgOf(rs, func(r *metrics.Record) *float64 {
		if r.TTFTMs == nil {
			return nil
		}
		v := float64(*r.TTFTMs)
		return &v
	})
}

func avgOf(rs []*metrics.Record, f func(*metrics.Record) *float64) *float64 {
	var sum float64
	n := 0
	for _, r := range rs {
		if v := f(r); v != nil {
			sum += *v
			n++
		}
	}
	if n == 0 {
		return nil
	}
	v := round(sum/float64(n), 2)
	return &v
}

func dist(vals []float64, fields ...string) Dist {
	d := Dist{Count: len(vals)}
	if len(vals) == 0 {
		return d
	}
	sorted := append([]float64(nil), vals...)
	sort.Float64s(sorted)
	p := func(q float64) *float64 {
		// nearest-rank percentile
		idx := int(math.Ceil(q/100*float64(len(sorted)))) - 1
		if idx < 0 {
			idx = 0
		}
		v := round(sorted[idx], 2)
		return &v
	}
	for _, f := range fields {
		switch f {
		case "avg":
			var sum float64
			for _, v := range sorted {
				sum += v
			}
			a := round(sum/float64(len(sorted)), 2)
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
			m := round(sorted[len(sorted)-1], 2)
			d.Max = &m
		}
	}
	return d
}

func topN(recs []*metrics.Record, n int, val func(*metrics.Record) (int64, bool)) []Slow {
	type kv struct {
		v int64
		r *metrics.Record
	}
	var list []kv
	for _, r := range recs {
		if v, ok := val(r); ok {
			list = append(list, kv{v, r})
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].v > list[j].v })
	if len(list) > n {
		list = list[:n]
	}
	out := make([]Slow, 0, len(list))
	for _, e := range list {
		r := e.r
		out = append(out, Slow{
			RequestID: r.RequestID, StartTime: r.StartTime.Format(metrics.TimeFormat), Model: r.Model, Stream: r.Stream,
			StatusCode: r.StatusCode, TotalMs: r.TotalMs, TTFTMs: r.TTFTMs, ReqContentBytes: r.ReqContentBytes,
			PromptTokens: r.PromptTokens, CompletionTokens: r.CompletionTokens,
		})
	}
	return out
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
