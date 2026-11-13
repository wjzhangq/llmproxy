package proxy

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wjzhangq/llmproxy/internal/logstore"
)

var loadN = 500

// TestLoad500Streams runs loadN concurrent streaming requests directly and
// through the proxy and compares client-side TTFT and total time.
func TestLoad500Streams(t *testing.T) {
	if testing.Short() {
		t.Skip("load test")
	}
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		time.Sleep(50 * time.Millisecond)
		for i := 0; i < 20; i++ {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"tok"}}]}`+"\n\n")
			fl.Flush()
			time.Sleep(10 * time.Millisecond)
		}
		fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":20}}`+"\n\ndata: [DONE]\n\n")
	}))
	up.Listener, _ = net.Listen("tcp", "127.0.0.1:0")
	up.Config.SetKeepAlivesEnabled(true)
	up.Start()
	defer up.Close()

	dir := t.TempDir()
	store, err := logstore.Open(logstore.Options{Dir: dir, QueueSize: 10000})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(up.URL)
	px := httptest.NewServer(New(Options{Upstream: u, UpstreamModel: "m", InjectUsage: true, Sink: store}))
	defer px.Close()

	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 1000, MaxConnsPerHost: 0}}
	run := func(base string) (ttft, total []time.Duration) {
		var mu sync.Mutex
		var wg sync.WaitGroup
		for i := 0; i < loadN; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				start := time.Now()
				resp, err := client.Post(base+"/v1/chat/completions", "application/json",
					strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
				if err != nil {
					t.Error(err)
					return
				}
				defer resp.Body.Close()
				sc := bufio.NewScanner(resp.Body)
				var first time.Duration
				for sc.Scan() {
					if first == 0 && strings.Contains(sc.Text(), "tok") {
						first = time.Since(start)
					}
				}
				mu.Lock()
				ttft = append(ttft, first)
				total = append(total, time.Since(start))
				mu.Unlock()
			}()
		}
		wg.Wait()
		return
	}
	p99 := func(d []time.Duration) time.Duration {
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		return d[len(d)*99/100]
	}
	p50 := func(d []time.Duration) time.Duration { return d[len(d)/2] }
	run(up.URL) // warm up connections
	run(px.URL)
	dt, dT := run(up.URL)
	pt, pT := run(px.URL)
	store.Close()
	t.Logf("direct  ttft p50=%v p99=%v total p50=%v p99=%v", p50(dt), p99(dt), p50(dT), p99(dT))
	t.Logf("proxied ttft p50=%v p99=%v total p50=%v p99=%v", p50(pt), p99(pt), p50(pT), p99(pT))
	if store.Dropped() != 0 {
		t.Fatalf("dropped %d log records", store.Dropped())
	}
	recs := 0
	rc, _ := logstore.OpenDay(dir, logstore.MetricsDir, time.Now().Format("2006-01-02"))
	sc := bufio.NewScanner(rc)
	for sc.Scan() {
		recs++
	}
	rc.Close()
	if recs != 2*loadN {
		t.Fatalf("want 4*loadN metric lines, got %d", recs)
	}
}
