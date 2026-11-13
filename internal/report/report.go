package report

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/wjzhangq/llmproxy/internal/logstore"
)

//go:embed prompts/default.md
var DefaultPrompt string

// Options configures report generation.
type Options struct {
	Dir        string
	Location   *time.Location
	LLMBaseURL string
	LLMModel   string
	LLMAPIKey  string
	PromptFile string
	// LLMTimeout bounds the AI call; default 5 minutes.
	LLMTimeout time.Duration
	HTTPClient *http.Client
	Now        func() time.Time
}

// Result describes what Generate produced.
type Result struct {
	JSONPath     string
	MarkdownPath string
	Stats        *Stats
	AIError      error
}

// Generate computes statistics for day (YYYY-MM-DD), writes
// reports/DAY.json and reports/DAY.md, and returns the result. A failing AI
// call is reported in Result.AIError; the files are still written.
func Generate(ctx context.Context, day string, o Options) (*Result, error) {
	if o.Location == nil {
		o.Location = time.Local
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if _, err := time.ParseInLocation("2006-01-02", day, o.Location); err != nil {
		return nil, fmt.Errorf("invalid date %q, want YYYY-MM-DD", day)
	}
	recs, err := ReadRecords(o.Dir, day)
	if err != nil && !errors.Is(err, ErrNoData) {
		return nil, err
	}
	stats := Compute(recs, day, o.Location, logstore.ReadDrops(o.Dir, day), o.Now())

	dir := filepath.Join(o.Dir, logstore.ReportsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	res := &Result{
		JSONPath:     filepath.Join(dir, day+".json"),
		MarkdownPath: filepath.Join(dir, day+".md"),
		Stats:        stats,
	}
	js, _ := json.MarshalIndent(stats, "", "  ")
	if err := writeFileAtomic(res.JSONPath, append(js, '\n')); err != nil {
		return nil, err
	}

	var md string
	if stats.Overall.Requests == 0 {
		res.AIError = errors.New("no requests recorded for this day")
	} else {
		history := loadHistory(o.Dir, day, o.Location, 7)
		md, res.AIError = aiReport(ctx, o, stats, history)
	}
	if res.AIError != nil {
		md = TemplateReport(stats, res.AIError)
	} else {
		md = fmt.Sprintf("# LLM 推理日报 %s\n\n> 由 AI 根据 `reports/%s.json` 生成，所有数字以该文件为准。\n\n%s\n\n---\n\n%s", day, day, strings.TrimSpace(md), TemplateTables(stats))
	}
	if err := writeFileAtomic(res.MarkdownPath, []byte(md)); err != nil {
		return nil, err
	}
	return res, nil
}

// loadHistory reads up to n previous days of stats JSON, oldest first.
func loadHistory(dir, day string, loc *time.Location, n int) []*Stats {
	t, _ := time.ParseInLocation("2006-01-02", day, loc)
	var out []*Stats
	for i := n; i >= 1; i-- {
		d := t.AddDate(0, 0, -i).Format("2006-01-02")
		b, err := os.ReadFile(filepath.Join(dir, logstore.ReportsDir, d+".json"))
		if err != nil {
			continue
		}
		var s Stats
		if json.Unmarshal(b, &s) == nil {
			out = append(out, &s)
		}
	}
	return out
}

func (o Options) prompt() (string, error) {
	if o.PromptFile == "" {
		return DefaultPrompt, nil
	}
	b, err := os.ReadFile(o.PromptFile)
	if err != nil {
		return "", fmt.Errorf("read prompt file: %w", err)
	}
	return string(b), nil
}

// ChatURL returns the chat completions endpoint for an OpenAI-compatible
// base URL, accepting both "http://host:8000" and "http://host:8000/v1".
func ChatURL(base string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/chat/completions") {
		return base
	}
	if strings.HasSuffix(base, "/v1") {
		return base + "/chat/completions"
	}
	return base + "/v1/chat/completions"
}

var thinkBlock = regexp.MustCompile(`(?s)<think>.*?</think>`)

func aiReport(ctx context.Context, o Options, stats *Stats, history []*Stats) (string, error) {
	if o.LLMBaseURL == "" || o.LLMModel == "" {
		return "", errors.New("AI endpoint not configured (report.llm_base_url / report.llm_model)")
	}
	prompt, err := o.prompt()
	if err != nil {
		return "", err
	}
	if history == nil {
		history = []*Stats{}
	}
	today, _ := json.Marshal(stats)
	hist, _ := json.Marshal(history)
	user := fmt.Sprintf("今日（%s）统计 JSON：\n```json\n%s\n```\n\n前 7 天统计 JSON（按日期升序）：\n```json\n%s\n```", stats.Date, today, hist)
	reqBody, _ := json.Marshal(map[string]any{
		"model": o.LLMModel,
		"messages": []map[string]string{
			{"role": "system", "content": prompt},
			{"role": "user", "content": user},
		},
		"temperature": 0.2,
		"stream":      false,
	})
	timeout := o.LLMTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ChatURL(o.LLMBaseURL), bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.LLMAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.LLMAPIKey)
	}
	client := o.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("AI endpoint returned HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decode AI response: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", errors.New("AI response has no choices")
	}
	text := strings.TrimSpace(thinkBlock.ReplaceAllString(out.Choices[0].Message.Content, ""))
	if text == "" {
		return "", errors.New("AI response is empty")
	}
	return text, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
