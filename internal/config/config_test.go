package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	c := Default()
	if c.Listen != ":8080" || !c.ConsoleStats || !c.InjectUsage || c.RetentionDays != 30 {
		t.Fatalf("bad defaults: %+v", c)
	}
	if c.MaxBodyBytes != 10<<20 || c.UpstreamTimeout.Std() != 600*time.Second {
		t.Fatalf("bad defaults: %+v", c)
	}
	if err := c.Finalize(true); err == nil {
		t.Fatal("expected missing error")
	}
}

func TestFileEnvPriority(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	os.WriteFile(p, []byte(`
upstream: http://a:8000/
upstream_model: m1
max_body_bytes: 1MB
upstream_timeout: 30s
console_stats: false
report:
  llm_model: rm
`), 0o644)
	c := Default()
	used, err := c.LoadFile(p)
	if err != nil || used != p {
		t.Fatal(err)
	}
	env := map[string]string{"LLMPROXY_UPSTREAM_MODEL": "m2", "LLMPROXY_REPORT_LLM_API_KEY": "k", "LLMPROXY_RETENTION_DAYS": "7"}
	if err := c.LoadEnv(func(k string) (string, bool) { v, ok := env[k]; return v, ok }); err != nil {
		t.Fatal(err)
	}
	if err := c.Finalize(true); err != nil {
		t.Fatal(err)
	}
	if c.Upstream != "http://a:8000" || c.UpstreamModel != "m2" || c.MaxBodyBytes != 1<<20 ||
		c.UpstreamTimeout.Std() != 30*time.Second || c.ConsoleStats || c.RetentionDays != 7 {
		t.Fatalf("unexpected: %+v", c)
	}
	if c.Report.LLMModel != "rm" || c.Report.LLMAPIKey != "k" || c.Report.LLMBaseURL != "http://a:8000" {
		t.Fatalf("report: %+v", c.Report)
	}
}

func TestMissingExplicitFile(t *testing.T) {
	if _, err := Default().LoadFile("/nonexistent/x.yaml"); err == nil {
		t.Fatal("expected error")
	}
}
