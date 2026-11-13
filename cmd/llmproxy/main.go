// Command llmproxy is a logging and performance statistics reverse proxy for
// OpenAI-compatible inference servers such as vLLM and SGLang.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/wjzhangq/llmproxy/internal/config"
	"github.com/wjzhangq/llmproxy/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	cmd := "serve"
	if len(args) > 0 && args[0] == "report" {
		cmd, args = "report", args[1:]
	}
	cfg, opts, err := parseFlags("llmproxy", args, cmd == "report", stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	if opts.version {
		fmt.Fprintln(stdout, version.String())
		return 0
	}
	if err := cfg.Finalize(cmd == "serve"); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		var me *config.MissingError
		if errors.As(err, &me) {
			fmt.Fprint(stderr, "\n"+usageText)
		}
		return 2
	}
	switch cmd {
	case "report":
		return runReport(cfg, opts, stdout, stderr)
	default:
		return runServe(cfg, stdout, stderr)
	}
}
