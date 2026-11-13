package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/wjzhangq/llmproxy/internal/metrics"
)

// maxFullCapture bounds how much of a non-streaming response is kept for
// parsing usage and content sizes.
const maxFullCapture = 64 << 20

// state carries per-request data between the handler and ReverseProxy hooks.
type state struct {
	id            string
	info          reqInfo
	obs           observation
	statusCode    int
	streaming     bool
	full          *bytes.Buffer
	clientAborted bool
	err           string
	clientCtx     context.Context
}

func (p *Proxy) serveTracked(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	p.inflight.Add(1)
	p.total.Add(1)
	defer p.inflight.Add(-1)

	st := &state{id: requestID(r), obs: observation{start: start}, clientCtx: r.Context()}
	w.Header().Set(HeaderRequestID, st.id)

	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		// The client failed to send its body; nothing meaningful to forward.
		st.clientAborted = r.Context().Err() != nil
		st.statusCode = http.StatusBadRequest
		st.err = "read request body: " + err.Error()
		if !st.clientAborted {
			http.Error(w, "failed to read request body", http.StatusBadRequest)
		}
		p.finish(st, r, body, start)
		return
	}
	st.info = prepareRequest(body, r.URL.Path, p.opts.UpstreamModel, p.opts.InjectUsage)

	ctx := context.WithValue(r.Context(), ctxKey{}, st)
	var cancel context.CancelFunc = func() {}
	if p.opts.UpstreamTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, p.opts.UpstreamTimeout)
	}
	defer cancel()
	out := r.WithContext(ctx)
	out.Body = io.NopCloser(bytes.NewReader(st.info.forwardedBody))
	out.ContentLength = int64(len(st.info.forwardedBody))
	out.Header = r.Header.Clone()
	out.Header.Set(HeaderRequestID, st.id)
	out.Header.Del("Content-Length")
	out.Header.Del("Accept-Encoding")

	defer func() {
		rec := recover()
		if rec != nil && rec != http.ErrAbortHandler {
			panic(rec)
		}
		if r.Context().Err() != nil {
			st.clientAborted = true
		} else if rec != nil && st.err == "" {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				st.err = "upstream timeout"
			} else {
				st.err = "upstream stream interrupted"
			}
		}
		p.finish(st, r, body, start)
		if rec != nil {
			panic(rec)
		}
	}()
	p.rp.ServeHTTP(w, out)
}

func (p *Proxy) modifyResponse(resp *http.Response) error {
	st, ok := resp.Request.Context().Value(ctxKey{}).(*state)
	if !ok {
		return nil
	}
	st.statusCode = resp.StatusCode
	resp.Header.Set(HeaderRequestID, st.id)
	if resp.Header.Get("Content-Encoding") != "" {
		return nil // cannot inspect; pass through untouched
	}
	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/event-stream") {
		st.streaming = true
		resp.Body = newSSEReader(resp.Body, &st.obs, st.info.injected)
		return nil
	}
	st.full = &bytes.Buffer{}
	resp.Body = &teeReader{src: resp.Body, buf: st.full, limit: maxFullCapture}
	return nil
}

func (p *Proxy) finish(st *state, r *http.Request, body []byte, start time.Time) {
	end := time.Now()
	if st.full != nil {
		observeFull(st.full.Bytes(), &st.obs, st.statusCode)
	}
	rec := &metrics.Record{
		RequestID:        st.id,
		StartTime:        start,
		ClientModel:      st.info.clientModel,
		Model:            p.opts.UpstreamModel,
		Path:             r.URL.Path,
		Stream:           st.info.stream,
		StatusCode:       st.statusCode,
		TotalMs:          end.Sub(start).Milliseconds(),
		ReqContentBytes:  st.info.contentBytes,
		ReqImageCount:    st.info.imageCount,
		RespContentBytes: st.obs.respBytes,
		ClientAborted:    st.clientAborted,
		Error:            st.err,
	}
	if rec.StatusCode == 0 && st.clientAborted {
		rec.StatusCode = 499 // client closed request before a response
	}
	if rec.Error == "" && st.obs.errMsg != "" {
		rec.Error = st.obs.errMsg
	}
	if st.info.stream && !st.obs.firstToken.IsZero() {
		v := st.obs.firstToken.Sub(start).Milliseconds()
		rec.TTFTMs = &v
	}
	if u := st.obs.usage; u != nil && !st.clientAborted {
		rec.PromptTokens = u.PromptTokens
		rec.CompletionTokens = u.CompletionTokens
	}
	rec.Compute()

	p.opts.Sink.Submit(rec, &metrics.RequestLog{
		RequestID: st.id,
		StartTime: start,
		ClientIP:  clientIP(r),
		Method:    r.Method,
		Path:      r.URL.RequestURI(),
		Header:    r.Header,
		Body:      body,
	})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
