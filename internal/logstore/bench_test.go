package logstore

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wjzhangq/llmproxy/internal/metrics"
)

func benchRequest() *metrics.RequestLog {
	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"` +
		strings.Repeat("describe this ", 300) + `"},{"type":"image_url","image_url":{"url":"data:image/png;base64,` +
		strings.Repeat("QUJD", 256<<10) + `"}}]}]}`
	return &metrics.RequestLog{
		RequestID: "id", StartTime: time.Now(), ClientIP: "1.2.3.4", Method: "POST", Path: "/v1/chat/completions",
		Header: http.Header{"Authorization": {"Bearer sk-1234567890"}, "Content-Type": {"application/json"}},
		Body:   []byte(body),
	}
}

func benchEncode(b *testing.B, strip bool) {
	s := &Store{opts: Options{MaxBodyBytes: 10 << 20, StripImages: strip, Location: time.UTC}}
	r := benchRequest()
	b.SetBytes(int64(len(r.Body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.encodeRequest(nil, r)
	}
}

func BenchmarkEncodeRequest(b *testing.B)      { benchEncode(b, false) }
func BenchmarkEncodeRequestStrip(b *testing.B) { benchEncode(b, true) }
