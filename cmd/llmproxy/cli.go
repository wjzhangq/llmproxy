package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/wjzhangq/llmproxy/internal/config"
)

const usageText = `Usage:
  llmproxy [flags]                       start the proxy
  llmproxy report [flags] --date DATE    (re)generate the daily report for DATE (YYYY-MM-DD)

Flags:
  -c, --config PATH     config file (default: ./config.yaml, /etc/llmproxy/config.yaml)
  -u, --upstream URL    upstream vLLM/SGLang address, e.g. http://127.0.0.1:8000
  -m, --model NAME      upstream model name; every request's "model" is rewritten to it
  -l, --listen ADDR     listen address (default :8080)
      --log-dir DIR     log and report directory (default ./data)
      --no-console      disable per-request console stats
  -v, --version         print version and exit

Priority: flags > LLMPROXY_* environment variables > config file > defaults.
`

type cliOptions struct {
	configPath string
	date       string
	version    bool
}

// parseFlags parses args into a Config following the documented priority.
func parseFlags(name string, args []string, withDate bool, stderr io.Writer) (*config.Config, *cliOptions, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	opts := &cliOptions{}
	var upstream, model, listen, logDir string
	var noConsole bool
	for _, n := range []string{"c", "config"} {
		fs.StringVar(&opts.configPath, n, "", "")
	}
	for _, n := range []string{"u", "upstream"} {
		fs.StringVar(&upstream, n, "", "")
	}
	for _, n := range []string{"m", "model"} {
		fs.StringVar(&model, n, "", "")
	}
	for _, n := range []string{"l", "listen"} {
		fs.StringVar(&listen, n, "", "")
	}
	fs.StringVar(&logDir, "log-dir", "", "")
	fs.BoolVar(&noConsole, "no-console", false, "")
	for _, n := range []string{"v", "version"} {
		fs.BoolVar(&opts.version, n, false, "")
	}
	if withDate {
		fs.StringVar(&opts.date, "date", "", "")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stderr, usageText)
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("%w\n\n%s", err, usageText)
	}
	if fs.NArg() > 0 {
		return nil, nil, fmt.Errorf("unexpected arguments: %v\n\n%s", fs.Args(), usageText)
	}
	if opts.version {
		return nil, opts, nil
	}

	cfg := config.Default()
	if _, err := cfg.LoadFile(opts.configPath); err != nil {
		return nil, nil, err
	}
	if err := cfg.LoadEnv(os.LookupEnv); err != nil {
		return nil, nil, err
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "u", "upstream":
			cfg.Upstream = upstream
		case "m", "model":
			cfg.UpstreamModel = model
		case "l", "listen":
			cfg.Listen = listen
		case "log-dir":
			cfg.LogDir = logDir
		case "no-console":
			cfg.ConsoleStats = !noConsole
		}
	})
	return cfg, opts, nil
}
