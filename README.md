# llmproxy

在 vLLM / SGLang 前部署的 Go 反向代理：透明转发 OpenAI 标准接口，记录完整请求日志和逐请求性能指标（TTFT、总耗时、token/s），每天生成统计汇总和 AI 分析报告，用来定位推理瓶颈。

- 透传 `/v1/chat/completions`、`/v1/completions`（SSE 与非流式），统一把请求中的 `model` 改写为 `upstream_model`
- 流式请求自动注入 `stream_options.include_usage`，并从返回流中摘掉多出来的 usage chunk，客户端无感
- 其他路径（`/v1/models`、`/health`、`/metrics` …）只透传；`/healthz` 由代理自己返回状态
- 逐请求指标、完整请求体分别按天写 JSONL；每日统计 JSON + AI Markdown 报告
- 单个静态二进制，无外部依赖

## 快速开始

```bash
# 只用参数启动，不需要配置文件
llmproxy --upstream http://127.0.0.1:8000 --model qwen3-32b

# 指定配置文件，并临时覆盖上游地址
llmproxy -c /etc/llmproxy/config.yaml --upstream http://10.0.0.5:8000

# 补跑某天的统计和报告
llmproxy report -c /etc/llmproxy/config.yaml --date 2026-11-12

# 查看版本
llmproxy --version
```

客户端把 `base_url` 指向代理即可，例如 `http://proxy:8080/v1`。

### 命令行参数

| 参数 | 对应配置项 | 说明 |
|---|---|---|
| `-c`, `--config` | — | 配置文件；不传时依次查找 `./config.yaml`、`/etc/llmproxy/config.yaml`，都没有则只用参数和默认值 |
| `-u`, `--upstream` | upstream | 上游地址 |
| `-m`, `--model` | upstream_model | 后端模型名 |
| `-l`, `--listen` | listen | 监听地址 |
| `--log-dir` | log_dir | 日志目录 |
| `--no-console` | console_stats | 关闭控制台逐请求统计 |
| `-v`, `--version` | — | 打印版本、commit、构建时间 |

优先级：命令行参数 > 环境变量（`LLMPROXY_` 前缀，如 `LLMPROXY_UPSTREAM`、`LLMPROXY_REPORT_LLM_API_KEY`）> 配置文件 > 默认值。全部配置项见 [`config.example.yaml`](config.example.yaml)。

缺少 `upstream` 或 `upstream_model` 时打印缺失项和用法后以非 0 退出。收到 SIGINT/SIGTERM 时停止接收新请求，等待进行中的请求完成（上限 30 s）并刷完日志队列。

## 控制台输出

```
2026-11-13 14:02:31 a1b2c3d4 qwen3-32b SSE 200 total=8.42s ttft=0.31s in=1532 out=612 tps=72.7
2026-11-13 14:02:35 e5f6a7b8 qwen3-32b JSON 200 total=2.10s ttft=- in=420 out=96 tps=45.7
2026-11-13 14:02:40 c9d0e1f2 qwen3-32b SSE 200 total=3.05s ttft=0.28s in=880 out=- tps=- aborted
```

## 日志文件

`log_dir` 下：

| 文件 | 内容 |
|---|---|
| `metrics/YYYY-MM-DD.jsonl` | 逐请求指标，一行一个请求 |
| `metrics/YYYY-MM-DD.dropped` | 当日因队列满丢弃的日志条数（有丢弃时才出现） |
| `requests/YYYY-MM-DD.jsonl` | request_id、start_time、客户端 IP、请求头（敏感头只保留后 4 位）、客户端原始请求体 |
| `reports/YYYY-MM-DD.json` | 当日统计数据 |
| `reports/YYYY-MM-DD.md` | 当日 AI 分析报告 |

- 按 `timezone` 的 0 点切分（按写入时刻归属日期，跨零点的请求记在结束那天）；前一天的 metrics/requests 文件 gzip 压缩，超过 `retention_days` 删除。
- 请求体超过 `max_body_bytes` 时只记前缀（字符串形式）并标 `truncated=true`；`strip_images: true` 时 base64 图片替换为 `<base64 omitted mime=... len=...>`。
- 写日志走异步队列，队列按条数（`log_queue_size`）和内存（`log_queue_bytes`，默认 512 MB）双重限额，超出时丢弃并计入 `metrics/DATE.dropped`，不阻塞转发。
- 用 request_id 关联指标和请求体，例如：`zgrep '"request_id":"a1b2c3d4' data/requests/2026-11-12.jsonl.gz`。

### 指标字段

| 字段 | 口径 |
|---|---|
| request_id | 客户端 `X-Request-Id`，没有则生成 UUID，并回写到响应头 |
| start_time | 代理收到请求头的时刻（RFC3339，毫秒） |
| client_model / model | 客户端原始 `model`（未传为空）/ 实际发给后端的模型名 |
| path / stream / status_code | 路径 / 请求体中的 `stream` / 返回给客户端的状态码（客户端在响应前断开记 499） |
| total_ms | start_time 到最后一个字节发给客户端 |
| ttft_ms | 流式：到第一个含非空 content / reasoning_content / tool_calls（completions 为 text）的 chunk；非流式 null |
| req_content_bytes / req_image_count | messages 文本（数组形式只算 `type=text`）或 prompt 的 UTF-8 字节数 / 图片数 |
| resp_content_bytes | 生成的 content + reasoning_content + tool_calls 参数的 UTF-8 字节数 |
| prompt_tokens / completion_tokens | 后端 usage；拿不到（中断、报错）为 null |
| tokens_per_sec | completion_tokens ÷ (total_ms ÷ 1000) |
| decode_tps | completion_tokens ÷ ((total_ms − ttft_ms) ÷ 1000)，仅流式，用于区分预填充慢和解码慢 |
| client_aborted / error | 客户端中途断开 / 上游错误摘要 |

## 每日统计与 AI 报告

每天 `report.cron`（默认 00:10）统计前一天：先从 metrics 文件算出确定的统计数据 `reports/DATE.json`（总体、模型 × 是否流式的 avg/P50/P95/P99/max、两种 token/s 口径、按小时、按请求大小分桶、Top 10 最慢请求），再把它和前 7 天的统计 JSON 交给大模型写成 `reports/DATE.md`。大模型默认直接用被代理的后端（`report.llm_base_url` / `report.llm_model` 可改），提示词见 [`internal/report/prompts/default.md`](internal/report/prompts/default.md)，可用 `report.prompt_file` 覆盖。调用失败时仍生成统计 JSON，并输出标注“AI 分析失败”的模板摘要。

统计是边读边汇总的，每个请求只保留计算分位数所需的几个数值（50 万请求约 40 MB 内存）。如不希望在代理进程内跑统计，可设 `report.enabled: false`，改由系统 cron 执行 `llmproxy report -c <配置文件>`（不带 `--date` 时统计前一天）。

## 开发

```bash
go test -race ./...
golangci-lint run
go build ./cmd/llmproxy
```

推送 `v*` tag（如 `v0.1.0`）触发 GitHub Actions：vet、golangci-lint、`go test -race` 通过后用 GoReleaser 构建 linux/amd64、linux/arm64、darwin/amd64、darwin/arm64 四个静态二进制包（`llmproxy_<版本>_<os>_<arch>.tar.gz`，附 SHA256 校验和 changelog）发布到 GitHub Release。普通 push 和 PR 只跑 lint 与单元测试。
