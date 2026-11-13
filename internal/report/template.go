package report

import (
	"fmt"
	"strings"
)

// TemplateReport renders a Markdown summary without AI interpretation, used
// when the AI call fails.
func TemplateReport(s *Stats, aiErr error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# LLM 推理日报 %s\n\n", s.Date)
	fmt.Fprintf(&b, "> ⚠️ **AI 分析失败**：%s\n>\n> 以下为程序生成的统计摘要，不含解读。完整数据见 `reports/%s.json`。\n\n", oneLine(aiErr.Error()), s.Date)
	b.WriteString(TemplateTables(s))
	return b.String()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// TemplateTables renders the statistics as Markdown tables.
func TemplateTables(s *Stats) string {
	var b strings.Builder
	o := s.Overall
	b.WriteString("## 统计摘要\n\n### 总体\n\n")
	b.WriteString("| 请求数 | 成功 | 错误 | 错误率 | 客户端中断 | 流式占比 | 逐请求平均 token/s | 整体吞吐 token/s | 日志丢弃 |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|\n")
	fmt.Fprintf(&b, "| %d | %d | %d | %.2f%% | %d | %.1f%% | %s | %s | %d |\n\n",
		o.Requests, o.Success, o.Errors, o.ErrorRate*100, o.ClientAborted, o.StreamRatio*100,
		fmtF(o.TPSPerRequestAvg, ""), fmtF(o.ThroughputTPS, ""), o.LogDropped)

	if len(s.Groups) > 0 {
		b.WriteString("### 按模型 × 是否流式\n\n")
		b.WriteString("| 模型 | 流式 | 请求数 | 总耗时 avg / P50 / P95 / P99 / max (ms) | TTFT avg / P50 / P95 / P99 (ms) | token/s avg / P50 / P5 | 整体吞吐 | decode token/s avg | prompt / completion tokens |\n")
		b.WriteString("|---|---|---|---|---|---|---|---|---|\n")
		for _, g := range s.Groups {
			ttft := "-"
			if g.TTFTMs != nil && g.TTFTMs.Count > 0 {
				t := g.TTFTMs
				ttft = fmt.Sprintf("%s / %s / %s / %s", fmtF(t.Avg, ""), fmtF(t.P50, ""), fmtF(t.P95, ""), fmtF(t.P99, ""))
			}
			decode := "-"
			if g.DecodeTPS != nil {
				decode = fmtF(g.DecodeTPS.Avg, "")
			}
			t := g.TotalMs
			p := g.TokensPerSec
			fmt.Fprintf(&b, "| %s | %s | %d | %s / %s / %s / %s / %s | %s | %s / %s / %s | %s | %s | %d / %d |\n",
				g.Model, yesNo(g.Stream), g.Requests,
				fmtF(t.Avg, ""), fmtF(t.P50, ""), fmtF(t.P95, ""), fmtF(t.P99, ""), fmtF(t.Max, ""),
				ttft, fmtF(p.Avg, ""), fmtF(p.P50, ""), fmtF(p.P5, ""), fmtF(g.ThroughputTPS, ""), decode,
				g.PromptTokensTotal, g.CompletionTokensTotal)
		}
		b.WriteString("\n")
	}

	if len(s.Hourly) > 0 {
		b.WriteString("### 按小时\n\n| 小时 | 请求数 | 平均 TTFT (ms) | 平均 token/s |\n|---|---|---|---|\n")
		for _, h := range s.Hourly {
			fmt.Fprintf(&b, "| %02d | %d | %s | %s |\n", h.Hour, h.Requests, fmtF(h.AvgTTFTMs, ""), fmtF(h.AvgTokensPerS, ""))
		}
		b.WriteString("\n")
	}

	b.WriteString("### 按请求大小\n\n| 大小 | 请求数 | 平均 TTFT (ms) | 平均总耗时 (ms) |\n|---|---|---|---|\n")
	for _, k := range s.SizeBuckets {
		fmt.Fprintf(&b, "| %s | %d | %s | %s |\n", k.Bucket, k.Requests, fmtF(k.AvgTTFTMs, ""), fmtF(k.AvgTotal, ""))
	}
	b.WriteString("\n")

	slowTable := func(title string, list []Slow) {
		if len(list) == 0 {
			return
		}
		fmt.Fprintf(&b, "### %s\n\n| request_id | 开始时间 | 流式 | 状态 | 总耗时 (ms) | TTFT (ms) | 请求字节 | prompt / completion |\n|---|---|---|---|---|---|---|---|\n", title)
		for _, r := range list {
			ttft := "-"
			if r.TTFTMs != nil {
				ttft = fmt.Sprint(*r.TTFTMs)
			}
			fmt.Fprintf(&b, "| `%s` | %s | %s | %d | %d | %s | %d | %s / %s |\n",
				r.RequestID, r.StartTime, yesNo(r.Stream), r.StatusCode, r.TotalMs, ttft, r.ReqContentBytes, intS(r.PromptTokens), intS(r.CompletionTokens))
		}
		b.WriteString("\n")
	}
	slowTable("Top 10 最慢（总耗时）", s.SlowestTotal)
	slowTable("Top 10 最慢（TTFT）", s.SlowestTTFT)
	return b.String()
}

func yesNo(b bool) string {
	if b {
		return "是"
	}
	return "否"
}

func intS(p *int64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprint(*p)
}
