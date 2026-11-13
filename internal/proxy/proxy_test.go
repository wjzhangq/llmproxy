package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wjzhangq/llmproxy/internal/metrics"
)

type captureSink struct {
	mu   sync.Mutex
	recs []*metrics.Record
	reqs []*metrics.RequestLog
	ch   chan struct{}
}

func newSink() *captureSink { return &captureSink{ch: make(chan struct{}, 100)} }

func (s *captureSink) Submit(r *metrics.Record, q *metrics.RequestLog) {
	s.mu.Lock()
	s.recs = append(s.recs, r)
	s.reqs = append(s.reqs, q)
	s.mu.Unlock()
	s.ch <- struct{}{}
}

func (s *captureSink) wait(t *testing.T) *metrics.Record {
	t.Helper()
	select {
	case <-s.ch:
	case <-time.After(3 * time.Second):
		t.Fatal("no record submitted")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recs[len(s.recs)-1]
}

type upstreamReq struct {
	body   map[string]any
	header http.Header
}

// fakeUpstream mimics vLLM: SSE when stream=true, usage chunk only when
// stream_options.include_usage is true.
func fakeUpstream(t *testing.T, got chan<- upstreamReq) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"backend-model"}]}`)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if got != nil {
			got <- upstreamReq{body, r.Header.Clone()}
		}
		if body["model"] != "backend-model" {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"error":{"message":"model not found"}}`)
			return
		}
		if r.Header.Get("X-Fail") != "" {
			w.WriteHeader(500)
			fmt.Fprint(w, `{"error":{"message":"boom"}}`)
			return
		}
		if body["stream"] != true {
			time.Sleep(5 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"x","model":"backend-model","choices":[{"message":{"role":"assistant","content":"hello"}}],"usage":{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		send := func(s string) { fmt.Fprintf(w, "data: %s\n\n", s); fl.Flush() }
		send(`{"id":"c","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`)
		n := 3
		if r.Header.Get("X-Slow") != "" {
			n = 1000
		}
		for i := 0; i < n; i++ {
			if r.Context().Err() != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
			send(`{"id":"c","choices":[{"index":0,"delta":{"content":"ab"}}]}`)
		}
		send(`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		if so, ok := body["stream_options"].(map[string]any); ok && so["include_usage"] == true {
			send(`{"id":"c","choices":[],"usage":{"prompt_tokens":9,"completion_tokens":3,"total_tokens":12}}`)
		}
		send("[DONE]")
	}))
}

func newTestProxy(t *testing.T, up *httptest.Server, sink Sink) *httptest.Server {
	u, _ := url.Parse(up.URL)
	p := New(Options{Upstream: u, UpstreamModel: "backend-model", InjectUsage: true, UpstreamTimeout: 10 * time.Second, Sink: sink})
	return httptest.NewServer(p)
}

func post(t *testing.T, url, body string, hdr map[string]string) *http.Response {
	req, _ := http.NewRequest("POST", url+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestNonStreamModelRewrite(t *testing.T) {
	got := make(chan upstreamReq, 1)
	up := fakeUpstream(t, got)
	defer up.Close()
	sink := newSink()
	px := newTestProxy(t, up, sink)
	defer px.Close()

	resp := post(t, px.URL, `{"model":"whatever","messages":[{"role":"user","content":"héllo"}],"temperature":0.1}`, nil)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "hello") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if resp.Header.Get(HeaderRequestID) == "" {
		t.Fatal("missing request id")
	}
	u := <-got
	if u.body["model"] != "backend-model" || u.body["temperature"] != 0.1 {
		t.Fatalf("upstream body %v", u.body)
	}
	if _, ok := u.body["stream_options"]; ok {
		t.Fatal("must not inject for non-stream")
	}
	rec := sink.wait(t)
	if rec.ClientModel != "whatever" || rec.Model != "backend-model" || rec.StatusCode != 200 || rec.Stream {
		t.Fatalf("%+v", rec)
	}
	if rec.TTFTMs != nil || *rec.PromptTokens != 5 || *rec.CompletionTokens != 7 || rec.TokensPerSec == nil {
		t.Fatalf("%+v", rec)
	}
	if rec.ReqContentBytes != int64(len("héllo")) || rec.RespContentBytes != 5 {
		t.Fatalf("bytes %d %d", rec.ReqContentBytes, rec.RespContentBytes)
	}
	if rec.RequestID != resp.Header.Get(HeaderRequestID) {
		t.Fatal("request id mismatch")
	}
	if string(sink.reqs[0].Body) != `{"model":"whatever","messages":[{"role":"user","content":"héllo"}],"temperature":0.1}` {
		t.Fatalf("logged body %s", sink.reqs[0].Body)
	}
}

func TestMissingModelAndRequestIDReuse(t *testing.T) {
	got := make(chan upstreamReq, 1)
	up := fakeUpstream(t, got)
	defer up.Close()
	sink := newSink()
	px := newTestProxy(t, up, sink)
	defer px.Close()
	resp := post(t, px.URL, `{"messages":[]}`, map[string]string{HeaderRequestID: "my-id-123"})
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get(HeaderRequestID) != "my-id-123" {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
	u := <-got
	if u.header.Get(HeaderRequestID) != "my-id-123" || u.body["model"] != "backend-model" {
		t.Fatal(u)
	}
	rec := sink.wait(t)
	if rec.ClientModel != "" || rec.RequestID != "my-id-123" {
		t.Fatalf("%+v", rec)
	}
}

func readSSE(t *testing.T, resp *http.Response) []string {
	var lines []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if l := sc.Text(); l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

func TestStreamInjectAndStrip(t *testing.T) {
	got := make(chan upstreamReq, 1)
	up := fakeUpstream(t, got)
	defer up.Close()
	sink := newSink()
	px := newTestProxy(t, up, sink)
	defer px.Close()

	resp := post(t, px.URL, `{"model":"m","stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"abc"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`, nil)
	lines := readSSE(t, resp)
	resp.Body.Close()
	for _, l := range lines {
		if strings.Contains(l, "usage") {
			t.Fatalf("usage chunk leaked to client: %s", l)
		}
	}
	if lines[len(lines)-1] != "data: [DONE]" || len(lines) != 6 {
		t.Fatalf("lines %v", lines)
	}
	u := <-got
	so, _ := u.body["stream_options"].(map[string]any)
	if so["include_usage"] != true {
		t.Fatalf("not injected: %v", u.body)
	}
	rec := sink.wait(t)
	if !rec.Stream || rec.TTFTMs == nil || *rec.TTFTMs < 15 || *rec.CompletionTokens != 3 || *rec.PromptTokens != 9 {
		t.Fatalf("%+v", rec)
	}
	if rec.ReqContentBytes != 3 || rec.ReqImageCount != 1 || rec.RespContentBytes != 6 || rec.DecodeTPS == nil {
		t.Fatalf("%+v", rec)
	}
	if strings.Contains(string(sink.reqs[0].Body), "stream_options") {
		t.Fatal("logged body must be the original")
	}
}

func TestStreamClientRequestedUsage(t *testing.T) {
	up := fakeUpstream(t, nil)
	defer up.Close()
	sink := newSink()
	px := newTestProxy(t, up, sink)
	defer px.Close()
	resp := post(t, px.URL, `{"stream":true,"stream_options":{"include_usage":true},"messages":[]}`, nil)
	lines := readSSE(t, resp)
	resp.Body.Close()
	found := false
	for _, l := range lines {
		found = found || strings.Contains(l, `"usage"`)
	}
	if !found {
		t.Fatal("client-requested usage must be passed through")
	}
	if rec := sink.wait(t); *rec.CompletionTokens != 3 {
		t.Fatalf("%+v", rec)
	}
}

func TestClientAbort(t *testing.T) {
	up := fakeUpstream(t, nil)
	defer up.Close()
	sink := newSink()
	px := newTestProxy(t, up, sink)
	defer px.Close()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", px.URL+"/v1/chat/completions", strings.NewReader(`{"stream":true,"messages":[]}`))
	req.Header.Set("X-Slow", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(resp.Body)
	br.ReadString('\n')
	cancel()
	resp.Body.Close()
	rec := sink.wait(t)
	if !rec.ClientAborted || rec.CompletionTokens != nil || rec.TokensPerSec != nil {
		t.Fatalf("%+v", rec)
	}
	if rec.TotalMs > 2000 {
		t.Fatalf("upstream not cancelled promptly: %d", rec.TotalMs)
	}
}

func TestUpstreamError(t *testing.T) {
	up := fakeUpstream(t, nil)
	defer up.Close()
	sink := newSink()
	px := newTestProxy(t, up, sink)
	defer px.Close()
	resp := post(t, px.URL, `{"messages":[]}`, map[string]string{"X-Fail": "1"})
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 500 || !strings.Contains(string(b), "boom") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if rec := sink.wait(t); rec.Error != "boom" || rec.StatusCode != 500 || rec.PromptTokens != nil {
		t.Fatalf("%+v", rec)
	}
}

func TestUpstreamDown(t *testing.T) {
	sink := newSink()
	u, _ := url.Parse("http://127.0.0.1:1")
	px := httptest.NewServer(New(Options{Upstream: u, UpstreamModel: "m", Sink: sink}))
	defer px.Close()
	resp := post(t, px.URL, `{"messages":[]}`, nil)
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Fatal(resp.StatusCode)
	}
	if rec := sink.wait(t); rec.StatusCode != 502 || rec.Error == "" || rec.ClientAborted {
		t.Fatalf("%+v", rec)
	}
}

func TestPassthroughAndHealth(t *testing.T) {
	up := fakeUpstream(t, nil)
	defer up.Close()
	sink := newSink()
	px := newTestProxy(t, up, sink)
	defer px.Close()
	resp, _ := http.Get(px.URL + "/v1/models")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "backend-model") {
		t.Fatal(string(b))
	}
	resp, _ = http.Get(px.URL + "/healthz")
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"status":"ok"`) {
		t.Fatal(string(b))
	}
	select {
	case <-sink.ch:
		t.Fatal("untracked paths must not be recorded")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestPrepareRequestKeepsOrder(t *testing.T) {
	in := `{"stream":true,"model":"x","stream_options":{"foo":1},"z":[1,2]}`
	info := prepareRequest([]byte(in), "B", true)
	want := `{"stream":true,"model":"B","stream_options":{"foo":1,"include_usage":true},"z":[1,2]}`
	if string(info.forwardedBody) != want || !info.injected {
		t.Fatalf("%s", info.forwardedBody)
	}
	info = prepareRequest([]byte(`{"prompt":["ab","c"]}`), "B", true)
	if string(info.forwardedBody) != `{"model":"B","prompt":["ab","c"]}` || info.contentBytes != 3 {
		t.Fatalf("%s", info.forwardedBody)
	}
	info = prepareRequest([]byte(`not json`), "B", true)
	if string(info.forwardedBody) != "not json" || info.parsed {
		t.Fatal("unparseable body must be forwarded as-is")
	}
}

func TestSSESplitAcrossReads(t *testing.T) {
	src := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\r\n\r\ndata: {\"choices\":[],\"usage\":{\"completion_tokens\":2}}\n\ndata: [DONE]\n\n"
	obs := &observation{}
	r := newSSEReader(io.NopCloser(&slowReader{s: src}), obs, true)
	out, _ := io.ReadAll(r)
	if strings.Contains(string(out), "usage") || !strings.Contains(string(out), "[DONE]") || !strings.Contains(string(out), "hi") {
		t.Fatalf("%q", out)
	}
	if obs.usage == nil || *obs.usage.CompletionTokens != 2 || obs.respBytes != 2 {
		t.Fatal(obs)
	}
}

type slowReader struct{ s string }

func (r *slowReader) Read(p []byte) (int, error) {
	if r.s == "" {
		return 0, io.EOF
	}
	n := copy(p[:1], r.s)
	r.s = r.s[n:]
	return n, nil
}

func TestUpstreamIgnoresEnvProxy(t *testing.T) {
	u, _ := url.Parse("http://10.0.0.5:8000")
	p := New(Options{Upstream: u, UpstreamModel: "m"})
	if tr := p.rp.Transport.(*http.Transport); tr.Proxy != nil {
		t.Fatal("upstream transport must not use HTTP(S)_PROXY")
	}
}

type chunkReader struct {
	s string
	n int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.s == "" {
		return 0, io.EOF
	}
	k := min(r.n, len(r.s), len(p))
	copy(p, r.s[:k])
	r.s = r.s[k:]
	return k, nil
}

func TestSSESeparatorSplitAtEveryOffset(t *testing.T) {
	src := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\r\n\r\n" +
		"data: {\"choices\":[],\"usage\":{\"completion_tokens\":2}}\n\ndata: [DONE]\n\n"
	for n := 1; n <= len(src); n++ {
		obs := &observation{}
		out, _ := io.ReadAll(newSSEReader(io.NopCloser(&chunkReader{s: src, n: n}), obs, true))
		want := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\r\n\r\ndata: [DONE]\n\n"
		if string(out) != want || obs.usage == nil || obs.respBytes != 2 {
			t.Fatalf("chunk %d: %q", n, out)
		}
	}
}

// A single huge event delivered in small reads must be processed in linear
// time (previously every read rescanned the whole pending buffer).
func TestSSELargeEventLinear(t *testing.T) {
	big := "data: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("x", 8<<20) + "\"}}]}\n\n"
	start := time.Now()
	obs := &observation{}
	out, _ := io.ReadAll(newSSEReader(io.NopCloser(&chunkReader{s: big, n: 4096}), obs, false))
	if len(out) != len(big) || obs.respBytes != 8<<20 {
		t.Fatal(len(out), obs.respBytes)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("too slow: %v", d)
	}
}
