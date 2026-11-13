package logstore

import (
	"bytes"
	"encoding/json"
	"net/http"
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

// StripImages replaces base64 data URL strings inside a JSON document with a
// short placeholder carrying the MIME type and original length. The input
// must be valid JSON; whitespace outside strings is removed as well.
func StripImages(b []byte) []byte { return compactJSON(nil, b, true) }

// compactJSON appends src (valid JSON) to dst without insignificant
// whitespace, optionally replacing base64 data URL strings.
func compactJSON(dst, src []byte, strip bool) []byte {
	for i := 0; i < len(src); {
		switch c := src[i]; c {
		case ' ', '\t', '\n', '\r':
			i++
		case '"':
			end := stringEnd(src, i)
			str := src[i:end]
			if strip {
				if mime, ok := dataURLMime(str); ok {
					dst = append(dst, `"<base64 omitted mime=`...)
					dst = append(dst, mime...)
					dst = append(dst, ` len=`...)
					dst = strconv.AppendInt(dst, int64(len(str)-2), 10)
					dst = append(dst, `>"`...)
					i = end
					continue
				}
			}
			dst = append(dst, str...)
			i = end
		default:
			j := i + 1
			for j < len(src) {
				c := src[j]
				if c == '"' || c == ' ' || c == '\t' || c == '\n' || c == '\r' {
					break
				}
				j++
			}
			dst = append(dst, src[i:j]...)
			i = j
		}
	}
	return dst
}

// stringEnd returns the index just past the string starting at src[i].
func stringEnd(src []byte, i int) int {
	j := i + 1
	for {
		k := bytes.IndexByte(src[j:], '"')
		if k < 0 {
			return len(src)
		}
		j += k
		// The quote is escaped if preceded by an odd number of backslashes.
		bs := 0
		for p := j - 1; p > i && src[p] == '\\'; p-- {
			bs++
		}
		j++
		if bs%2 == 0 {
			return j
		}
	}
}

var (
	dataPrefix   = []byte(`"data:`)
	base64Marker = []byte(";base64,")
)

// dataURLMime reports whether the quoted JSON string str is a base64 data URL
// and returns its MIME type.
func dataURLMime(str []byte) ([]byte, bool) {
	if !bytes.HasPrefix(str, dataPrefix) {
		return nil, false
	}
	head := str[len(dataPrefix):]
	if len(head) > 128 {
		head = head[:128]
	}
	k := bytes.Index(head, base64Marker)
	if k < 0 {
		return nil, false
	}
	mime := head[:k]
	for _, c := range mime {
		if !mimeChar(c) {
			return nil, false
		}
	}
	return mime, true
}

// requestLine holds the fields written before "body".
type requestLine struct {
	RequestID string            `json:"request_id"`
	StartTime string            `json:"start_time"`
	ClientIP  string            `json:"client_ip"`
	Method    string            `json:"method"`
	Path      string            `json:"path"`
	Headers   map[string]string `json:"headers"`
	BodyBytes int               `json:"body_bytes"`
	Truncated bool              `json:"truncated"`
}

// encodeRequest appends one requests/*.jsonl line (without newline) to dst.
// The body is scanned at most twice (validation, then compaction with
// optional image stripping) and embedded without re-encoding.
func (s *Store) encodeRequest(dst []byte, r *metrics.RequestLog) []byte {
	line := requestLine{
		RequestID: r.RequestID,
		StartTime: r.StartTime.In(s.opts.Location).Format(metrics.TimeFormat),
		ClientIP:  r.ClientIP,
		Method:    r.Method,
		Path:      r.Path,
		Headers:   MaskHeaders(r.Header),
		BodyBytes: len(r.Body),
	}
	body := r.Body
	valid := len(body) > 0 && json.Valid(body)
	max := s.opts.MaxBodyBytes
	var compact []byte
	if valid {
		compact = compactJSON(make([]byte, 0, min(len(body), int(max)+64)), body, s.opts.StripImages)
		body = compact
	}
	if max > 0 && int64(len(body)) > max {
		line.Truncated = true
		valid = false
		body = body[:max]
		for len(body) > 0 && !utf8.Valid(body) {
			body = body[:len(body)-1]
		}
	}
	head, _ := json.Marshal(&line)
	dst = append(dst, head[:len(head)-1]...)
	dst = append(dst, `,"body":`...)
	switch {
	case len(r.Body) == 0:
		dst = append(dst, "null"...)
	case valid:
		dst = append(dst, body...)
	default:
		str, _ := json.Marshal(string(body))
		dst = append(dst, str...)
	}
	return append(dst, '}')
}

func mimeChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return c == '.' || c == '+' || c == '-' || c == '/'
}
