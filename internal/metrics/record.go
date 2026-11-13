// Package metrics defines the per-request metric and request-log records.
package metrics

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// TimeFormat is RFC3339 with millisecond precision.
const TimeFormat = "2006-01-02T15:04:05.000Z07:00"

// Record is one line of metrics/YYYY-MM-DD.jsonl.
type Record struct {
	RequestID        string    `json:"request_id"`
	StartTime        time.Time `json:"-"`
	ClientModel      string    `json:"client_model"`
	Model            string    `json:"model"`
	Path             string    `json:"path"`
	Stream           bool      `json:"stream"`
	StatusCode       int       `json:"status_code"`
	TotalMs          int64     `json:"total_ms"`
	TTFTMs           *int64    `json:"ttft_ms"`
	ReqContentBytes  int64     `json:"req_content_bytes"`
	ReqImageCount    int       `json:"req_image_count"`
	RespContentBytes int64     `json:"resp_content_bytes"`
	PromptTokens     *int64    `json:"prompt_tokens"`
	CompletionTokens *int64    `json:"completion_tokens"`
	TokensPerSec     *float64  `json:"tokens_per_sec"`
	// DecodeTPS is completion_tokens / (total_ms - ttft_ms); streaming only.
	DecodeTPS     *float64 `json:"decode_tps"`
	ClientAborted bool     `json:"client_aborted"`
	Error         string   `json:"error"`
}

type recordAlias Record

// MarshalJSON renders start_time as RFC3339 with milliseconds.
func (r *Record) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		StartTime string `json:"start_time"`
		*recordAlias
	}{r.StartTime.Format(TimeFormat), (*recordAlias)(r)})
}

// UnmarshalJSON parses start_time.
func (r *Record) UnmarshalJSON(b []byte) error {
	aux := struct {
		StartTime string `json:"start_time"`
		*recordAlias
	}{recordAlias: (*recordAlias)(r)}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	if aux.StartTime != "" {
		t, err := time.Parse(time.RFC3339Nano, aux.StartTime)
		if err != nil {
			return err
		}
		r.StartTime = t
	}
	return nil
}

// Compute fills the derived throughput fields.
func (r *Record) Compute() {
	r.TokensPerSec, r.DecodeTPS = nil, nil
	if r.CompletionTokens == nil || r.TotalMs <= 0 {
		return
	}
	tps := round2(float64(*r.CompletionTokens) / (float64(r.TotalMs) / 1000))
	r.TokensPerSec = &tps
	if r.TTFTMs != nil && r.TotalMs > *r.TTFTMs {
		d := round2(float64(*r.CompletionTokens) / (float64(r.TotalMs-*r.TTFTMs) / 1000))
		r.DecodeTPS = &d
	}
}

func round2(f float64) float64 { return float64(int64(f*100+0.5)) / 100 }

// RequestLog is one line of requests/YYYY-MM-DD.jsonl. Body holds the
// client's original request body; it is processed (redaction, truncation,
// image stripping) by the log writer, off the request path.
type RequestLog struct {
	RequestID string
	StartTime time.Time
	ClientIP  string
	Method    string
	Path      string
	Header    http.Header
	Body      []byte
}

// ConsoleLine formats the one-line console summary of r, in loc.
func ConsoleLine(r *Record, end time.Time, loc *time.Location) string {
	id := r.RequestID
	if len(id) > 8 {
		id = id[:8]
	}
	kind := "JSON"
	if r.Stream {
		kind = "SSE"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %s %s %d total=%.2fs ttft=%s in=%s out=%s tps=%s",
		end.In(loc).Format("2006-01-02 15:04:05"), id, r.Model, kind, r.StatusCode,
		float64(r.TotalMs)/1000, secs(r.TTFTMs), intOr(r.PromptTokens), intOr(r.CompletionTokens), floatOr(r.TokensPerSec))
	if r.ClientAborted {
		b.WriteString(" aborted")
	}
	if r.Error != "" {
		e := strings.Join(strings.Fields(r.Error), " ")
		if len(e) > 120 {
			e = e[:120] + "..."
		}
		b.WriteString(" error=" + e)
	}
	return b.String()
}

func secs(p *int64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%.2fs", float64(*p)/1000)
}

func intOr(p *int64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprint(*p)
}

func floatOr(p *float64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f", *p)
}
