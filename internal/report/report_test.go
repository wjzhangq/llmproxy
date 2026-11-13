package report

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wjzhangq/llmproxy/internal/metrics"
)

func i64(v int64) *int64 { return &v }

func sampleRecords(loc *time.Location) []*metrics.Record {
	base := time.Date(2026, 11, 12, 9, 0, 0, 0, loc)
	var out []*metrics.Record
	for i := 0; i < 10; i++ {
		r := &metrics.Record{
			RequestID: fmt.Sprintf("req-%d", i), StartTime: base.Add(time.Duration(i) * 30 * time.Minute),
			Model: "m", Stream: i%2 == 0, StatusCode: 200, TotalMs: int64(1000 * (i + 1)),
			ReqContentBytes: int64(i * 3000), PromptTokens: i64(100), CompletionTokens: i64(int64(50 * (i + 1))),
		}
		if r.Stream {
			r.TTFTMs = i64(int64(100 * (i + 1)))
		}
		out = append(out, r)
	}
	out[9].StatusCode, out[9].Error, out[9].CompletionTokens, out[9].PromptTokens = 500, "boom", nil, nil
	out[8].ClientAborted, out[8].CompletionTokens, out[8].PromptTokens = true, nil, nil
	for _, r := range out {
		r.Compute()
	}
	return out
}

func TestCompute(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	s := Compute(sampleRecords(loc), "2026-11-12", loc, 3, time.Now())
	o := s.Overall
	if o.Requests != 10 || o.Success != 8 || o.Errors != 1 || o.ClientAborted != 1 || o.LogDropped != 3 || o.StreamRatio != 0.5 || o.ErrorRate != 0.1 {
		t.Fatalf("%+v", o)
	}
	// every successful request has tps = 50 tokens/s
	if *o.TPSPerRequestAvg != 50 || *o.ThroughputTPS != 50 {
		t.Fatalf("%v %v", *o.TPSPerRequestAvg, *o.ThroughputTPS)
	}
	if len(s.Groups) != 2 || s.Groups[0].Stream || !s.Groups[1].Stream {
		t.Fatalf("%+v", s.Groups)
	}
	g := s.Groups[1] // stream: i = 0,2,4,6,8 → total 1000..9000, ttft 100..900
	if g.Requests != 5 || *g.TotalMs.P50 != 5000 || *g.TotalMs.Max != 9000 || *g.TTFTMs.Avg != 500 || g.CompletionTokensTotal != 50+150+250+350 {
		t.Fatalf("%+v", g)
	}
	if s.Groups[0].TTFTMs != nil {
		t.Fatal("non-stream group must not have ttft")
	}
	if len(s.Hourly) != 5 || s.Hourly[0].Hour != 9 || s.Hourly[0].Requests != 2 {
		t.Fatalf("%+v", s.Hourly)
	}
	if s.SizeBuckets[0].Requests != 1 || s.SizeBuckets[1].Requests != 1 || s.SizeBuckets[2].Requests != 4 || s.SizeBuckets[3].Requests != 4 {
		t.Fatalf("%+v", s.SizeBuckets)
	}
	if s.SlowestTotal[0].RequestID != "req-9" || s.SlowestTTFT[0].RequestID != "req-8" || len(s.SlowestTTFT) != 5 {
		t.Fatalf("%+v", s.SlowestTotal)
	}
}

func writeMetrics(t *testing.T, dir, day string, recs []*metrics.Record) {
	os.MkdirAll(filepath.Join(dir, "metrics"), 0o755)
	var b strings.Builder
	for _, r := range recs {
		j, _ := json.Marshal(r)
		b.Write(j)
		b.WriteByte('\n')
	}
	os.WriteFile(filepath.Join(dir, "metrics", day+".jsonl"), []byte(b.String()), 0o644)
}

func TestGenerateWithAI(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	dir := t.TempDir()
	writeMetrics(t, dir, "2026-11-12", sampleRecords(loc))
	os.MkdirAll(filepath.Join(dir, "reports"), 0o755)
	os.WriteFile(filepath.Join(dir, "reports", "2026-11-10.json"), []byte(`{"date":"2026-11-10","overall":{"requests":7}}`), 0o644)
	var gotUser string
	ai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(400)
			return
		}
		var req struct {
			Model    string                     `json:"model"`
			Messages []struct{ Content string } `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		gotUser = req.Messages[1].Content
		fmt.Fprint(w, `{"choices":[{"message":{"content":"<think>hmm</think>\n## 今日结论\n- ok"}}]}`)
	}))
	defer ai.Close()
	res, err := Generate(context.Background(), "2026-11-12", Options{Dir: dir, Location: loc, LLMBaseURL: ai.URL, LLMModel: "m", LLMAPIKey: "k"})
	if err != nil || res.AIError != nil {
		t.Fatal(err, res.AIError)
	}
	md, _ := os.ReadFile(res.MarkdownPath)
	if !strings.Contains(string(md), "## 今日结论") || strings.Contains(string(md), "think") || strings.Contains(string(md), "AI 分析失败") {
		t.Fatal(string(md))
	}
	if !strings.Contains(gotUser, `"date":"2026-11-10"`) || !strings.Contains(gotUser, `"requests":10`) {
		t.Fatal(gotUser)
	}
	var s Stats
	b, _ := os.ReadFile(res.JSONPath)
	if json.Unmarshal(b, &s) != nil || s.Overall.Requests != 10 {
		t.Fatal(string(b))
	}
}

func TestGenerateAIFailure(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	dir := t.TempDir()
	writeMetrics(t, dir, "2026-11-12", sampleRecords(loc))
	res, err := Generate(context.Background(), "2026-11-12", Options{Dir: dir, Location: loc, LLMBaseURL: "http://127.0.0.1:1", LLMModel: "m"})
	if err != nil || res.AIError == nil {
		t.Fatal(err)
	}
	md, _ := os.ReadFile(res.MarkdownPath)
	if !strings.Contains(string(md), "AI 分析失败") || !strings.Contains(string(md), "req-9") {
		t.Fatal(string(md))
	}
	if _, err := os.Stat(res.JSONPath); err != nil {
		t.Fatal(err)
	}
}

func TestChatURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://h:8000":     "http://h:8000/v1/chat/completions",
		"http://h:8000/v1/": "http://h:8000/v1/chat/completions",
		"https://x/api/v1":  "https://x/api/v1/chat/completions",
	} {
		if got := ChatURL(in); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
}

func TestCron(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	c, err := ParseCron("10 0 * * *")
	if err != nil {
		t.Fatal(err)
	}
	n := c.Next(time.Date(2026, 11, 13, 0, 10, 0, 0, loc))
	if !n.Equal(time.Date(2026, 11, 14, 0, 10, 0, 0, loc)) {
		t.Fatal(n)
	}
	c, _ = ParseCron("*/15 9-17 * * 1-5")
	n = c.Next(time.Date(2026, 11, 14, 12, 0, 0, 0, loc)) // Saturday
	if !n.Equal(time.Date(2026, 11, 16, 9, 0, 0, 0, loc)) {
		t.Fatal(n)
	}
	for _, bad := range []string{"", "60 * * * *", "* * * *", "a * * * *"} {
		if _, err := ParseCron(bad); err == nil {
			t.Error("expected error for", bad)
		}
	}
}
