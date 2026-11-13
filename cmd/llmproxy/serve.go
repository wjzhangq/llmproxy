package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wjzhangq/llmproxy/internal/config"
	"github.com/wjzhangq/llmproxy/internal/logstore"
	"github.com/wjzhangq/llmproxy/internal/proxy"
	"github.com/wjzhangq/llmproxy/internal/version"
)

const shutdownGrace = 30 * time.Second

func runServe(cfg *config.Config, stdout, stderr io.Writer) int {
	logf := func(format string, a ...any) { fmt.Fprintf(stderr, "llmproxy: "+format+"\n", a...) }
	up, err := url.Parse(cfg.Upstream)
	if err != nil || up.Scheme == "" || up.Host == "" {
		logf("invalid upstream %q", cfg.Upstream)
		return 2
	}

	var console io.Writer
	if cfg.ConsoleStats {
		console = stdout
	}
	store, err := logstore.Open(logstore.Options{
		Dir:           cfg.LogDir,
		Location:      cfg.Location,
		QueueSize:     cfg.LogQueueSize,
		QueueBytes:    int64(cfg.LogQueueBytes),
		MaxBodyBytes:  int64(cfg.MaxBodyBytes),
		StripImages:   cfg.StripImages,
		RetentionDays: cfg.RetentionDays,
		Console:       console,
		ErrorLog:      logf,
	})
	if err != nil {
		logf("open log dir: %v", err)
		return 1
	}

	p := proxy.New(proxy.Options{
		Upstream:        up,
		UpstreamModel:   cfg.UpstreamModel,
		InjectUsage:     cfg.InjectUsage,
		UpstreamTimeout: cfg.UpstreamTimeout.Std(),
		Sink:            store,
	})
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           p,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	fmt.Fprintf(stdout, "%s | listen=%s upstream=%s upstream_model=%s log_dir=%s\n",
		version.String(), cfg.Listen, cfg.Upstream, cfg.UpstreamModel, cfg.LogDir)
	if cfg.ValidateModel {
		go checkUpstreamModel(cfg, logf)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	sched := startScheduler(ctx, cfg, stdout, logf)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	code := 0
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			logf("listen: %v", err)
			code = 1
		}
	case <-ctx.Done():
		logf("shutting down, waiting up to %s for %d in-flight requests", shutdownGrace, p.Inflight())
		sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		if err := srv.Shutdown(sctx); err != nil {
			logf("shutdown: %v", err)
			srv.Close()
		}
		cancel()
	}
	sched.Wait()
	store.Close()
	if d := store.Dropped(); d > 0 {
		logf("%d log records dropped since start", d)
	}
	if d := store.ConsoleDropped(); d > 0 {
		logf("%d console stats lines skipped (stdout too slow)", d)
	}
	return code
}

// checkUpstreamModel warns when upstream_model is not served by the upstream.
func checkUpstreamModel(cfg *config.Config, logf func(string, ...any)) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, cfg.Upstream+"/v1/models", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logf("warning: cannot list upstream models: %v", err)
		return
	}
	defer resp.Body.Close()
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&body) != nil {
		logf("warning: cannot list upstream models: HTTP %d", resp.StatusCode)
		return
	}
	var ids []string
	for _, m := range body.Data {
		if m.ID == cfg.UpstreamModel {
			return
		}
		ids = append(ids, m.ID)
	}
	logf("warning: upstream_model %q not found in upstream /v1/models %v", cfg.UpstreamModel, ids)
}
