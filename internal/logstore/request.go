package logstore

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/wjzhangq/llmproxy/internal/metrics"
)

// sensitiveHeaders are masked, keeping only their last 4 characters.
var sensitiveHeaders = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"api-key":             true,
	"x-api-key":           true,
	"cookie":              true,
	"set-cookie":          true,
	"x-auth-token":        true,
}

// MaskHeaders returns a copy of h with sensitive values masked.
func MaskHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vs := range h {
		v := strings.Join(vs, ", ")
		if sensitiveHeaders[strings.ToLower(k)] {
			v = mask(v)
		}
		out[k] = v
	}
	return out
}

func mask(v string) string {
	if len(v) <= 4 {
		return "****"
	}
	return "****" + v[len(v)-4:]
}

// base64Image matches a JSON string holding a base64 data URL.
var base64Image = regexp.MustCompile(`"data:([A-Za-z0-9.+\-/]*);base64,(?:[^"\\]|\\.)*"`)

// StripImages replaces base64 data URLs inside a JSON document with a short
// placeholder string carrying the MIME type and original length.
func StripImages(b []byte) []byte {
	if !bytes.Contains(b, []byte(";base64,")) {
		return b
	}
	return base64Image.ReplaceAllFunc(b, func(m []byte) []byte {
		sub := base64Image.FindSubmatch(m)
		return []byte(`"<base64 omitted mime=` + string(sub[1]) + ` len=` + strconv.Itoa(len(m)-2) + `>"`)
	})
}

type requestLine struct {
	RequestID string            `json:"request_id"`
	StartTime string            `json:"start_time"`
	ClientIP  string            `json:"client_ip"`
	Method    string            `json:"method"`
	Path      string            `json:"path"`
	Headers   map[string]string `json:"headers"`
	Body      json.RawMessage   `json:"body"`
	BodyBytes int               `json:"body_bytes"`
	Truncated bool              `json:"truncated"`
}

func (s *Store) encodeRequest(r *metrics.RequestLog) []byte {
	body := r.Body
	if s.opts.StripImages {
		body = StripImages(body)
	}
	line := requestLine{
		RequestID: r.RequestID,
		StartTime: r.StartTime.Format(metrics.TimeFormat),
		ClientIP:  r.ClientIP,
		Method:    r.Method,
		Path:      r.Path,
		Headers:   MaskHeaders(r.Header),
		BodyBytes: len(r.Body),
	}
	max := s.opts.MaxBodyBytes
	switch {
	case max > 0 && int64(len(body)) > max:
		line.Truncated = true
		prefix := body[:max]
		for len(prefix) > 0 && !utf8.Valid(prefix) {
			prefix = prefix[:len(prefix)-1]
		}
		line.Body, _ = json.Marshal(string(prefix))
	case len(body) == 0:
		line.Body = json.RawMessage("null")
	case json.Valid(body):
		var buf bytes.Buffer
		if json.Compact(&buf, body) == nil {
			line.Body = buf.Bytes()
		} else {
			line.Body = body
		}
	default:
		line.Body, _ = json.Marshal(string(body))
	}
	b, _ := json.Marshal(&line)
	return b
}
