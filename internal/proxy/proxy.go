// Package proxy implements the transparent OpenAI-compatible reverse proxy.
package proxy

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sync/atomic"
	"time"
)

// Options configures a Proxy.
type Options struct {
	Upstream        *url.URL
	UpstreamModel   string
	InjectUsage     bool
	UpstreamTimeout time.Duration
	Sink            Sink
	ErrorLog        *log.Logger
}

// Proxy is an http.Handler forwarding requests to a single upstream.
type Proxy struct {
	opts     Options
	rp       *httputil.ReverseProxy
	started  time.Time
	inflight atomic.Int64
	total    atomic.Int64
}

// HeaderRequestID is the request id header read from and written to clients.
const HeaderRequestID = "X-Request-Id"

type ctxKey struct{}

// New creates a Proxy.
func New(opts Options) *Proxy {
	if opts.Sink == nil {
		opts.Sink = nopSink{}
	}
	if opts.ErrorLog == nil {
		opts.ErrorLog = log.New(os.Stderr, "llmproxy: ", log.LstdFlags)
	}
	transport := &http.Transport{
		// Never route upstream traffic through HTTP(S)_PROXY: the upstream is
		// normally a private inference server.
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          2000,
		MaxIdleConnsPerHost:   1000,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: opts.UpstreamTimeout,
		// Keep upstream responses uncompressed so they can be inspected.
		DisableCompression: true,
	}
	p := &Proxy{opts: opts, started: time.Now()}
	p.rp = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(opts.Upstream)
			pr.Out.Host = opts.Upstream.Host
			// Preserve the incoming X-Forwarded-* headers, if any, untouched.
			if v := pr.In.Header["X-Forwarded-For"]; v != nil {
				pr.Out.Header["X-Forwarded-For"] = v
			}
		},
		Transport:      transport,
		FlushInterval:  -1, // flush every write immediately (SSE friendly)
		ErrorLog:       log.New(io.Discard, "", 0),
		ModifyResponse: p.modifyResponse,
		ErrorHandler:   p.errorHandler,
	}
	return p
}

// ServeHTTP implements http.Handler.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		p.serveHealth(w)
		return
	}
	if r.Method == http.MethodPost && IsTracked(r.URL.Path) {
		p.serveTracked(w, r)
		return
	}
	p.rp.ServeHTTP(w, r)
}

// IsTracked reports whether requests on path are measured and logged.
func IsTracked(path string) bool {
	return path == "/v1/chat/completions" || path == "/v1/completions"
}

func (p *Proxy) serveHealth(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":         "ok",
		"uptime_seconds": int64(time.Since(p.started).Seconds()),
		"inflight":       p.inflight.Load(),
		"total_requests": p.total.Load(),
		"upstream":       p.opts.Upstream.String(),
		"upstream_model": p.opts.UpstreamModel,
	})
}

// Inflight returns the number of tracked requests currently being served.
func (p *Proxy) Inflight() int64 { return p.inflight.Load() }

func (p *Proxy) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	st, _ := r.Context().Value(ctxKey{}).(*state)
	clientGone := r.Context().Err() != nil
	if st != nil {
		clientGone = st.clientCtx.Err() != nil
	}
	if clientGone {
		// Client went away; nothing can be written.
		if st != nil {
			st.clientAborted = true
		}
		return
	}
	status := http.StatusBadGateway
	if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
		status = http.StatusGatewayTimeout
	}
	if st != nil {
		st.err = "upstream: " + err.Error()
		st.statusCode = status
	}
	p.opts.ErrorLog.Printf("upstream error %s %s: %v", r.Method, r.URL.Path, err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":{"message":%q,"type":"proxy_error","code":%d}}`, "upstream error: "+err.Error(), status)
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// requestID returns the client's X-Request-Id or a new UUIDv4.
func requestID(r *http.Request) string {
	if id := r.Header.Get(HeaderRequestID); id != "" {
		return id
	}
	return newUUID()
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
