// Package version holds build information injected via -ldflags.
package version

import "fmt"

var (
	Version   = "dev"
	Commit    = "none"
	BuildTime = "unknown"
)

// String returns a human readable version line.
func String() string {
	return fmt.Sprintf("llmproxy %s (commit %s, built %s)", Version, Commit, BuildTime)
}
