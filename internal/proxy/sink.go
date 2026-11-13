package proxy

import "github.com/wjzhangq/llmproxy/internal/metrics"

// Sink receives completed request records. Implementations must not block.
type Sink interface {
	Submit(rec *metrics.Record, req *metrics.RequestLog)
}

type nopSink struct{}

func (nopSink) Submit(*metrics.Record, *metrics.RequestLog) {}
