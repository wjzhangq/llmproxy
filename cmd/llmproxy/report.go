package main

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/wjzhangq/llmproxy/internal/config"
	"github.com/wjzhangq/llmproxy/internal/report"
)

func reportOptions(cfg *config.Config) report.Options {
	return report.Options{
		Dir:        cfg.LogDir,
		Location:   cfg.Location,
		LLMBaseURL: cfg.Report.LLMBaseURL,
		LLMModel:   cfg.Report.LLMModel,
		LLMAPIKey:  cfg.Report.LLMAPIKey,
		PromptFile: cfg.Report.PromptFile,
	}
}

// runReport implements `llmproxy report --date YYYY-MM-DD`.
func runReport(cfg *config.Config, opts *cliOptions, stdout, stderr io.Writer) int {
	day := opts.date
	if day == "" {
		day = time.Now().In(cfg.Location).AddDate(0, 0, -1).Format("2006-01-02")
	}
	res, err := report.Generate(context.Background(), day, reportOptions(cfg))
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(stdout, "stats:  %s (%d requests)\nreport: %s\n", res.JSONPath, res.Stats.Overall.Requests, res.MarkdownPath)
	if res.AIError != nil {
		fmt.Fprintf(stderr, "warning: AI analysis failed, wrote template summary: %v\n", res.AIError)
	}
	return 0
}

// scheduler runs the daily report on report.cron.
type scheduler struct{ wg sync.WaitGroup }

func (s *scheduler) Wait() { s.wg.Wait() }

func startScheduler(ctx context.Context, cfg *config.Config, stdout io.Writer, logf func(string, ...any)) *scheduler {
	s := &scheduler{}
	if !cfg.Report.Enabled {
		return s
	}
	cron, err := report.ParseCron(cfg.Report.Cron)
	if err != nil {
		logf("daily report disabled: %v", err)
		return s
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			next := cron.Next(time.Now().In(cfg.Location))
			if next.IsZero() {
				logf("daily report disabled: cron %q never fires", cfg.Report.Cron)
				return
			}
			timer := time.NewTimer(time.Until(next))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			day := next.AddDate(0, 0, -1).Format("2006-01-02")
			res, err := report.Generate(ctx, day, reportOptions(cfg))
			switch {
			case err != nil:
				logf("daily report %s failed: %v", day, err)
			case res.AIError != nil:
				logf("daily report %s written to %s (AI analysis failed: %v)", day, res.MarkdownPath, res.AIError)
			default:
				fmt.Fprintf(stdout, "daily report %s written to %s\n", day, res.MarkdownPath)
			}
		}
	}()
	return s
}
