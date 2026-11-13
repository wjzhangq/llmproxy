// Package config loads llmproxy configuration from defaults, a YAML file,
// LLMPROXY_* environment variables and command line flags (in increasing
// priority).
package config

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration accepts either a Go duration string ("600s", "10m") or a plain
// number of seconds.
type Duration time.Duration

func (d *Duration) parse(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		*d = Duration(time.Duration(n * float64(time.Second)))
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q", s)
	}
	*d = Duration(v)
	return nil
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error { return d.parse(n.Value) }

// Std returns the value as time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// ByteSize accepts a plain number of bytes or a value with a KB/MB/GB suffix.
type ByteSize int64

func (b *ByteSize) parse(s string) error {
	s = strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), " ", ""))
	if s == "" {
		return nil
	}
	mult := int64(1)
	for _, u := range []struct {
		suf string
		m   int64
	}{{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(s, u.suf) {
			mult = u.m
			s = strings.TrimSuffix(s, u.suf)
			break
		}
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || n < 0 {
		return fmt.Errorf("invalid byte size %q", s)
	}
	*b = ByteSize(n * float64(mult))
	return nil
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (b *ByteSize) UnmarshalYAML(n *yaml.Node) error { return b.parse(n.Value) }

// Report holds daily report settings.
type Report struct {
	Enabled    bool   `yaml:"enabled"`
	Cron       string `yaml:"cron"`
	LLMBaseURL string `yaml:"llm_base_url"`
	LLMModel   string `yaml:"llm_model"`
	LLMAPIKey  string `yaml:"llm_api_key"`
	PromptFile string `yaml:"prompt_file"`
}

// Config is the full llmproxy configuration.
type Config struct {
	Listen          string   `yaml:"listen"`
	Upstream        string   `yaml:"upstream"`
	UpstreamModel   string   `yaml:"upstream_model"`
	ValidateModel   bool     `yaml:"validate_model"`
	ConsoleStats    bool     `yaml:"console_stats"`
	LogDir          string   `yaml:"log_dir"`
	Timezone        string   `yaml:"timezone"`
	RetentionDays   int      `yaml:"retention_days"`
	MaxBodyBytes    ByteSize `yaml:"max_body_bytes"`
	StripImages     bool     `yaml:"strip_images"`
	InjectUsage     bool     `yaml:"inject_usage"`
	LogQueueSize    int      `yaml:"log_queue_size"`
	LogQueueBytes   ByteSize `yaml:"log_queue_bytes"`
	UpstreamTimeout Duration `yaml:"upstream_timeout"`
	Report          Report   `yaml:"report"`

	// Location is the resolved Timezone.
	Location *time.Location `yaml:"-"`
}

// Default returns the default configuration.
func Default() *Config {
	return &Config{
		Listen:          ":8080",
		ValidateModel:   true,
		ConsoleStats:    true,
		LogDir:          "./data",
		Timezone:        "Asia/Shanghai",
		RetentionDays:   30,
		MaxBodyBytes:    10 << 20,
		InjectUsage:     true,
		LogQueueSize:    10000,
		LogQueueBytes:   512 << 20,
		UpstreamTimeout: Duration(600 * time.Second),
		Report: Report{
			Enabled: true,
			Cron:    "10 0 * * *",
		},
	}
}

// DefaultConfigPaths are searched when no -c flag is given.
var DefaultConfigPaths = []string{"./config.yaml", "/etc/llmproxy/config.yaml"}

// LoadFile merges a YAML file into c. If path is empty, the default paths are
// tried and a missing file is not an error. It returns the path actually used.
func (c *Config) LoadFile(path string) (string, error) {
	explicit := path != ""
	candidates := []string{path}
	if !explicit {
		candidates = DefaultConfigPaths
	}
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) && !explicit {
				continue
			}
			return "", fmt.Errorf("read config %s: %w", p, err)
		}
		if err := yaml.Unmarshal(data, c); err != nil {
			return "", fmt.Errorf("parse config %s: %w", p, err)
		}
		return p, nil
	}
	return "", nil
}

// LoadEnv overrides fields from LLMPROXY_<NAME> environment variables, where
// NAME is the upper-cased YAML key; nested keys are joined with "_", e.g.
// LLMPROXY_REPORT_LLM_BASE_URL.
func (c *Config) LoadEnv(lookup func(string) (string, bool)) error {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	return applyEnv(reflect.ValueOf(c).Elem(), "LLMPROXY_", lookup)
}

func applyEnv(v reflect.Value, prefix string, lookup func(string) (string, bool)) error {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		name := prefix + strings.ToUpper(tag)
		fv := v.Field(i)
		if f.Type.Kind() == reflect.Struct {
			if err := applyEnv(fv, name+"_", lookup); err != nil {
				return err
			}
			continue
		}
		s, ok := lookup(name)
		if !ok {
			continue
		}
		if err := setField(fv, s); err != nil {
			return fmt.Errorf("env %s: %w", name, err)
		}
	}
	return nil
}

func setField(fv reflect.Value, s string) error {
	switch p := fv.Addr().Interface().(type) {
	case *Duration:
		return p.parse(s)
	case *ByteSize:
		return p.parse(s)
	}
	switch fv.Kind() {
	case reflect.String:
		fv.SetString(s)
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return err
		}
		fv.SetBool(b)
	case reflect.Int, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return err
		}
		fv.SetInt(n)
	default:
		return fmt.Errorf("unsupported type %s", fv.Kind())
	}
	return nil
}

// Finalize fills derived defaults and validates the configuration. When
// requireUpstream is false (e.g. for the report command) missing upstream
// settings are tolerated.
func (c *Config) Finalize(requireUpstream bool) error {
	var missing []string
	if c.Upstream == "" {
		missing = append(missing, "upstream (--upstream / LLMPROXY_UPSTREAM)")
	}
	if c.UpstreamModel == "" {
		missing = append(missing, "upstream_model (--model / LLMPROXY_UPSTREAM_MODEL)")
	}
	if len(missing) > 0 && requireUpstream {
		return &MissingError{Fields: missing}
	}
	c.Upstream = strings.TrimRight(c.Upstream, "/")
	if c.Report.LLMBaseURL == "" {
		c.Report.LLMBaseURL = c.Upstream
	}
	if c.Report.LLMModel == "" {
		c.Report.LLMModel = c.UpstreamModel
	}
	if c.Timezone == "" {
		c.Timezone = "Asia/Shanghai"
	}
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return fmt.Errorf("invalid timezone %q: %w", c.Timezone, err)
	}
	c.Location = loc
	if c.LogQueueSize <= 0 {
		c.LogQueueSize = 10000
	}
	if c.LogQueueBytes <= 0 {
		c.LogQueueBytes = 512 << 20
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = 10 << 20
	}
	return nil
}

// MissingError reports required settings that were not provided.
type MissingError struct{ Fields []string }

func (e *MissingError) Error() string {
	return "missing required settings: " + strings.Join(e.Fields, ", ")
}
