package main

import (
	"fmt"
	"io"

	"github.com/wjzhangq/llmproxy/internal/config"
)

func runServe(cfg *config.Config, stdout, stderr io.Writer) int {
	fmt.Fprintf(stdout, "llmproxy listen=%s upstream=%s upstream_model=%s log_dir=%s\n",
		cfg.Listen, cfg.Upstream, cfg.UpstreamModel, cfg.LogDir)
	return 0
}

func runReport(cfg *config.Config, opts *cliOptions, stdout, stderr io.Writer) int {
	fmt.Fprintln(stderr, "report: not implemented yet")
	return 1
}
