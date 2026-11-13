package logstore

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wjzhangq/llmproxy/internal/metrics"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

func readLines(t *testing.T, dir, sub, day string) []map[string]any {
	t.Helper()
	rc, err := OpenDay(dir, sub, day)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	var out []map[string]any
	sc := bufio.NewScanner(rc)
	sc.Buffer(nil, 64<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func TestWriteRotateCompress(t *testing.T) {
	dir := t.TempDir()
	loc, _ := time.LoadLocation("Asia/Shanghai")
	c := &clock{t: time.Date(2026, 11, 12, 23, 59, 0, 0, loc)}
	var console bytes.Buffer
	s, err := Open(Options{Dir: dir, Location: loc, MaxBodyBytes: 60, StripImages: true, RetentionDays: 30, Console: &console, now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	tok := int64(10)
	rec := &metrics.Record{RequestID: "abcdef1234", StartTime: c.now(), Model: "m", StatusCode: 200, TotalMs: 1000, CompletionTokens: &tok}
	rec.Compute()
	h := http.Header{"Authorization": {"Bearer sk-123456789"}, "Content-Type": {"application/json"}}
	s.Submit(rec, &metrics.RequestLog{RequestID: "abcdef1234", StartTime: c.now(), Header: h, Body: []byte(`{"a":"data:image/png;base64,AAAA"}`)})
	time.Sleep(50 * time.Millisecond)
	c.set(time.Date(2026, 11, 13, 0, 0, 5, 0, loc))
	s.Submit(rec, &metrics.RequestLog{RequestID: "2", StartTime: c.now(), Header: h, Body: []byte(strings.Repeat("x", 100))})
	s.Close()

	time.Sleep(100 * time.Millisecond) // background compression
	if _, err := os.Stat(filepath.Join(dir, "metrics", "2026-11-12.jsonl.gz")); err != nil {
		t.Fatal("previous day not compressed:", err)
	}
	m := readLines(t, dir, "metrics", "2026-11-12")
	if len(m) != 1 || m[0]["request_id"] != "abcdef1234" || m[0]["tokens_per_sec"] != 10.0 || m[0]["ttft_ms"] != nil {
		t.Fatalf("%v", m)
	}
	if !strings.HasPrefix(m[0]["start_time"].(string), "2026-11-12T23:59:00.000+08:00") {
		t.Fatal(m[0]["start_time"])
	}
	r := readLines(t, dir, "requests", "2026-11-12")
	hd := r[0]["headers"].(map[string]any)
	if hd["Authorization"] != "****6789" || hd["Content-Type"] != "application/json" {
		t.Fatal(hd)
	}
	if body := r[0]["body"].(map[string]any); body["a"] != "<base64 omitted mime=image/png len=26>" {
		t.Fatal(body)
	}
	r = readLines(t, dir, "requests", "2026-11-13")
	if r[0]["truncated"] != true || r[0]["body"] != strings.Repeat("x", 60) || r[0]["body_bytes"] != 100.0 {
		t.Fatal(r[0])
	}
	if !strings.Contains(console.String(), "2026-11-12 23:59:00 abcdef12 m JSON 200 total=1.00s ttft=- in=- out=10 tps=10.0") {
		t.Fatal(console.String())
	}
}

type blockWriter struct{ ch chan struct{} }

func (b *blockWriter) Write(p []byte) (int, error) { <-b.ch; return len(p), nil }

func (b *blockWriter) wait() { <-b.ch }

func TestQueueFullDrops(t *testing.T) {
	dir := t.TempDir()
	bw := &blockWriter{ch: make(chan struct{})}
	s, _ := Open(Options{Dir: dir, QueueSize: 2, beforeWrite: bw.wait})
	rec := &metrics.Record{RequestID: "x", StartTime: time.Now()}
	start := time.Now()
	for i := 0; i < 20; i++ {
		s.Submit(rec, nil)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("submit blocked")
	}
	if s.Dropped() < 15 {
		t.Fatal(s.Dropped())
	}
	close(bw.ch)
	s.Close()
	day := time.Now().Format("2006-01-02")
	if ReadDrops(dir, day) != s.Dropped() {
		t.Fatal(ReadDrops(dir, day), s.Dropped())
	}
}

func TestRetention(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "requests"), 0o755)
	old := filepath.Join(dir, "requests", "2020-01-01.jsonl.gz")
	os.WriteFile(old, nil, 0o644)
	s, _ := Open(Options{Dir: dir, RetentionDays: 30})
	s.Close()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("expired file kept")
	}
}

func TestQueueBytesLimit(t *testing.T) {
	dir := t.TempDir()
	bw := &blockWriter{ch: make(chan struct{})}
	s, _ := Open(Options{Dir: dir, QueueSize: 1000, QueueBytes: 3 << 20, beforeWrite: bw.wait})
	rec := &metrics.Record{RequestID: "x", StartTime: time.Now()}
	big := make([]byte, 1<<20)
	for i := 0; i < 10; i++ {
		s.Submit(rec, &metrics.RequestLog{Body: big})
	}
	// at most 3 MB may be held: 2 queued + possibly 1 in the writer
	if s.Dropped() < 7 || s.QueuedBytes() > 3<<20 {
		t.Fatalf("dropped=%d queued=%d", s.Dropped(), s.QueuedBytes())
	}
	close(bw.ch)
	s.Close()
	if s.QueuedBytes() != 0 {
		t.Fatalf("queued bytes not released: %d", s.QueuedBytes())
	}
}

func TestSlowConsoleDoesNotBlockLogging(t *testing.T) {
	dir := t.TempDir()
	bw := &blockWriter{ch: make(chan struct{})}
	s, _ := Open(Options{Dir: dir, QueueSize: 10, Console: bw})
	rec := &metrics.Record{RequestID: "x", StartTime: time.Now()}
	for i := 0; i < consoleQueue+200; i++ {
		s.Submit(rec, nil)
		for s.QueuedBytes() > 5*entryOverhead { // let the writer keep up
			time.Sleep(time.Millisecond)
		}
	}
	if s.Dropped() != 0 || s.ConsoleDropped() == 0 {
		t.Fatalf("dropped=%d consoleDropped=%d", s.Dropped(), s.ConsoleDropped())
	}
	close(bw.ch)
	s.Close()
	lines := readLines(t, dir, "metrics", time.Now().Format("2006-01-02"))
	if len(lines) != consoleQueue+200 {
		t.Fatalf("metrics lines %d", len(lines))
	}
}
