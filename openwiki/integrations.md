# Integrations

> 外部服务与第三方集成。

## LLM 多模型路由

OnGrid 支持多 LLM provider 热路由，用户可在 UI 中选择模型。

| Provider | SDK | 备注 |
|----------|-----|------|
| OpenAI | `github.com/sashabaranov/go-openai` | GPT 系列 |
| Anthropic | (via openai-compat) | Claude 系列 |
| GLM (智谱) | `internal/pkg/zhipuauth/` | 智谱 AI |
| DeepSeek | (via openai-compat) | |
| Gemini | `github.com/singchia/geminio` | Google Gemini |
| Kimi | (via openai-compat) | Moonshot |
| Ollama | 本地 | 默认 qwen3:1.7b，支持本地嵌入 |

**关键文件**：
- `internal/pkg/llm/router.go` — MultiClient，60s TTL 缓存
- `internal/pkg/llm/eino_routing.go` — RoutingChatModel（Eino 适配）
- `internal/pkg/llm/client.go` — LLM 客户端
- `internal/pkg/llm/probe.go` — LLM 探测（20s 超时）
- `internal/manager/biz/setting/llm.go` — LLM 配置（DB → Resolver 60s TTL）

详见 [ongrid_LLM.md](../ongrid_LLM.md) 和 [ongrid_openai.md](../ongrid_openai.md)。

## IM 渠道

双向 IM 集成，支持每渠道独立 locale：

| 渠道 | SDK | 文件 |
|------|-----|------|
| Slack | webhook | `internal/pkg/notify/webhook.go` |
| Telegram | webhook | `internal/pkg/notify/notify.go` |
| 飞书 (Larksuite) | `github.com/larksuite/oapi-sdk-go/v3` | |
| 钉钉 (DingTalk) | `github.com/open-dingtalk/dingtalk-stream-sdk-go` | |
| 企业微信 (WeCom) | webhook | |

**关键文件**：`internal/pkg/notify/notify.go`、`internal/manager/biz/imbridge/dedup.go`

## 可观测性栈

| 组件 | 用途 | 配置 |
|------|------|------|
| Prometheus | 指标采集与告警 | `deploy/prometheus.yml` |
| Loki | 日志聚合 | `deploy/loki-config.yaml` |
| Tempo | 分布式追踪 | `deploy/tempo-config.yaml` |
| Grafana | 可视化面板 | 预置 dashboard |

**OTel**：`internal/pkg/tracing/tracing.go` — OTLP HTTP exporter → Tempo，2s batch timeout，ParentBased sampler。

**Spanmetrics**：Tempo 的 spanmetrics generator 生成 `traces_spanmetrics_latency_bucket` / `traces_spanmetrics_calls_total`。

## 数据库

| 存储 | 用途 | 驱动 |
|------|------|------|
| MySQL | 主业务库 | `gorm.io/driver/mysql` |
| SQLite | IAM user store | `github.com/glebarez/sqlite` |
| Qdrant | 向量检索（RAG） | `internal/pkg/qdrantx/` |
| Redis | 缓存 | |

**GORM 辅助**：`internal/pkg/dbx/` — soft_delete、migrate、分页

## Grafana

OnGrid 集成 Grafana 作为监控面板，通过 `internal/pkg/grafana/client.go` 编程管理 dashboard。

## SearXNG

元搜索引擎，用于 AI Agent 的 web_search 技能。配置：`deploy/searxng/settings.yml`

## Kubernetes

边端支持 K8s 部署，Helm chart 在 `deploy/kubernetes/ongrid-edge/`：
- DaemonSet（边端 agent）
- Telemetry Gateway（Loki/Tempo 网关）
- kube-state-metrics
- metrics-scraper

## Frontier Broker

gRPC broker，边端通过反向隧道连接。独立服务，Dockerfile 在 `deploy/Dockerfile.frontier`。版本 `FRONTIER_VERSION=v1.2.4`。

## Embedding 模型

- **本地嵌入**：BGE 模型，`internal/pkg/embedding/local.go`，预拉 `make fetch-embedding-model`
- **Ollama 嵌入**：nomic-embed-text
- **FastEmbed**：`github.com/anush008/fastembed-go`
