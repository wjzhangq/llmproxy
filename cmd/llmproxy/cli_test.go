package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFlagsOverrideFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte("upstream: http://file:1\nupstream_model: fm\nlisten: ':9'\n"), 0o644)
	cfg, _, err := parseFlags("x", []string{"-c", p, "--upstream", "http://flag:2", "--no-console"}, false, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Upstream != "http://flag:2" || cfg.UpstreamModel != "fm" || cfg.Listen != ":9" || cfg.ConsoleStats {
		t.Fatalf("%+v", cfg)
	}
}

func TestEnvBetweenFileAndFlags(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte("upstream: http://file:1\nupstream_model: fm\n"), 0o644)
	t.Setenv("LLMPROXY_UPSTREAM_MODEL", "envm")
	t.Setenv("LLMPROXY_UPSTREAM", "http://env:1")
	cfg, _, err := parseFlags("x", []string{"-c", p, "-u", "http://flag:2"}, false, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Upstream != "http://flag:2" || cfg.UpstreamModel != "envm" {
		t.Fatalf("%+v", cfg)
	}
}

func TestMissingRequired(t *testing.T) {
	wd, _ := os.Getwd()
	os.Chdir(t.TempDir())
	defer os.Chdir(wd)
	var out, errb bytes.Buffer
	if code := run([]string{"--upstream", "http://x"}, &out, &errb); code == 0 {
		t.Fatal("expected non-zero exit")
	}
	if !strings.Contains(errb.String(), "upstream_model") || !strings.Contains(errb.String(), "Usage") {
		t.Fatal(errb.String())
	}
}

func TestVersion(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"--version"}, &out, &out); code != 0 || !strings.Contains(out.String(), "llmproxy") {
		t.Fatal(out.String())
	}
}
