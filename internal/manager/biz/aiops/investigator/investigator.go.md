# `investigator.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/investigator/investigator.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/investigator`

## 1. 概述

本文件实现 P2 proactive AI investigation：incident 触发时，alert usecase 调 `InvestigateAsync` 入队，后台 worker pool 直接调 `correlate_incident` 工具采集 bundle + 单轮 LLM（无多轮 agent loop）生成初查报告，persist 为 `event_type=ai_initial_diagnosis` 的 Event 行。是 "reactive AIOps → proactive AIOps" 的关键——系统在 incident 诞生瞬间自动调查，不等 on-call 手动 /chat。设计：可选服务（LLM 未配置时 no-op）、worker pool 限流（默认 3 workers + 100 buffer）、ctx 解耦（`context.Background()` 派生 60s/120s deadline）、失败静默（纯增量，不影响 incident 主路径）。

## 2. 包信息

- **包名**：`investigator`
- **所属模块**：`internal/manager/biz/aiops/investigator`
- **依赖方向**：被 alert usecase 调用（InvestigateAsync）；调用 llm.Client / aiopstools.Registry / alert model.EventWriter

## 3. 关键类型与接口

```go
const (
    defaultWorkers    = 3
    defaultQueueDepth = 100
    defaultLLMTimeout = 120 * time.Second  // 与 llm.client.go defaultTimeout 对齐
    defaultUserMsgCap = 30 * 1024          // ~30KB bundle 上限
)

const systemPrompt = `你是 ongrid AIOps 平台的资深 SRE 助手...3 段话...不要复述原始数据，要给判断和动作。简洁。`

type EventWriter interface {
    CreateEvent(ctx, ev *model.Event) error
}

type ToolInvoker interface {
    Invoke(ctx, name string, args json.RawMessage) (aiopstools.ExecuteResult, error)
}

type Config struct {
    Model string
    Workers int
    QueueDepth int
    LLMTimeout time.Duration
    UserMsgCap int
}

type Investigator struct {
    llmClient llm.Client
    tools     ToolInvoker
    events    EventWriter
    cfg       Config
    log       *slog.Logger
    jobs      chan job
    wg        sync.WaitGroup
    stopOnce  sync.Once
    stopped   chan struct{}
}

type job struct{ incident *model.Incident }
```

## 4. 关键函数与流程

### `New`
- **签名**：`func New(llmClient llm.Client, tools ToolInvoker, events EventWriter, cfg Config, log *slog.Logger) *Investigator`
- **职责**：构造 Investigator + 启动 worker pool
- **流程**：
  1. cfg 零值填默认（Workers=3, QueueDepth=100, LLMTimeout=120s, UserMsgCap=30KB）
  2. log nil → `slog.Default()`；否则 `log.With("comp", "ai-investigator")`
  3. `jobs := make(chan job, cfg.QueueDepth)`
  4. 启动 `cfg.Workers` 个 `workerLoop` goroutine
- **nil-safe**：llmClient/tools/events 任一 nil → InvestigateAsync 静默丢弃（no-op）

### `InvestigateAsync`
- **签名**：`func (i *Investigator) InvestigateAsync(incident *model.Incident)`
- **职责**：非阻塞入队 incident 调查
- **流程**：
  1. nil receiver / nil incident → return
  2. llmClient/tools/events 任一 nil → return（no-op）
  3. `stopped` channel 已关闭 → return
  4. `select { case jobs <- job: default: warn backlog full }`——**队列满直接丢弃**，不阻塞 alert ingestion 路径
- **关键不变量**：alert firing path 绝不因 AI 调查慢而等待

### `Close`
- **签名**：`func (i *Investigator) Close()`
- **职责**：drain worker pool 优雅关闭
- **流程**：`stopOnce.Do(close(stopped); close(jobs))` → `wg.Wait()`
- **幂等**：多次调用安全

### `workerLoop`（内部）
- **签名**：`func (i *Investigator) workerLoop()`
- **职责**：消费 jobs channel，每个 job 调 runOne
- **流程**：`defer wg.Done(); for j := range jobs { runOne(j.incident) }`

### `runOne`（内部）
- **签名**：`func (i *Investigator) runOne(incident *model.Incident)`
- **职责**：执行单次调查——gather bundle + LLM + persist event
- **流程**：
  1. `ctx, cancel := context.WithTimeout(context.Background(), cfg.LLMTimeout)`——**ctx 解耦**，与 firing 请求 ctx 无关
  2. logCtx 带 incident_id / rule_key / severity
  3. `gatherBundle(ctx, incident.ID)` → bundleJSON；失败/空 → warn + return
  4. `capUserMessage(bundleJSON, cfg.UserMsgCap)` 截断到 30KB
  5. `llmClient.Chat(ctx, ChatReq{Model, Messages:[system, user=bundle], Temperature:0.2})`
  6. err 处理：
     - `errors.Is(err, llm.ErrNoAPIKey)` → **Info 级**（LLM 未配置，benign）
     - 其他 → Warn
  7. resp 空 / content 空 → Warn + return
  8. 构造 `model.Event{EventType: AIInitialDiagnosis, ActorType: System, StatusAfter: incident.Status, Severity, Title:"AI 初查", Message: &msg, OccurredAt: now}`
  9. `events.CreateEvent(ctx, ev)` 失败 → Warn + return
  10. Info log（prompt_tokens / completion_tokens / response_chars）
- **失败语义**：所有错误 log + return，不 panic，不写 partial event

### `gatherBundle`（内部）
- **签名**：`func (i *Investigator) gatherBundle(ctx, incidentID uint64) ([]byte, error)`
- **职责**：直接调 `correlate_incident` 工具采集 bundle（无 chat round-trip）
- **流程**：
  1. `json.Marshal({"incident_id": incidentID})`
  2. `tools.Invoke(ctx, ToolNameCorrelateIncident, args)`
  3. `res.ResultJSON` 空 → error
  4. 返回 `[]byte(res.ResultJSON)`
- **错误处理**：marshal / invoke / empty 均 `%w` 包装

### `capUserMessage`（内部）
- **签名**：`func capUserMessage(b []byte, cap int) []byte`
- **职责**：截断 bundle JSON 到 cap 字节
- **流程**：超 cap → 取前 cap 字节 + `"\n...(truncated)"` 标记
- **不保 JSON 完整性**：注释明示"LLM 能读轻微截断的 JSON tail"，避免 parse + re-trim 过度工程

## 5. 依赖关系

- **标准库**：`context`、`encoding/json`、`errors`、`fmt`、`log/slog`、`sync`、`time`
- **内部包**：`biz/aiops/tools`（ToolInvoker / ToolNameCorrelateIncident / ExecuteResult）、`manager/model/alert`（Event / Incident / EventTypeAIInitialDiagnosis / ActorTypeSystem）、`pkg/llm`（Client / ChatReq / Message / ErrNoAPIKey）
- **被调用方**：alert usecase（InvestigateAsync + Close）

## 6. 并发与资源管理

- **`jobs chan job`**：buffered（默认 100），worker pool 消费
- **`wg sync.WaitGroup`**：跟踪 worker goroutine，Close 时 Wait drain
- **`stopOnce sync.Once`**：保证 Close 幂等，stopped/jobs 仅关闭一次
- **`stopped chan struct{}`**：广播关闭信号，InvestigateAsync select 检测
- **ctx 解耦**：runOne 用 `context.Background()` 派生 120s deadline，与 firing 请求 ctx 无关——firing HTTP/gRPC handler 早已返回
- **worker pool 限流**：默认 3 并发 LLM round-trip，burst > 100 buffer 丢弃

## 7. 设计模式与亮点

- **proactive AIOps**：incident 诞生即调查，不等 on-call 手动 /chat——系统主动调查
- **worker pool + buffered channel**：限流 + 削峰，burst 丢弃不阻塞主路径
- **ctx 解耦**：`context.Background()` 派生，firing handler 不被 LLM 延迟拖累
- **nil-safe InvestigateAsync**：receiver nil / deps nil / incident nil 全静默返回——alert usecase 无需 nil-check
- **LLM 未配置 benign**：`ErrNoAPIKey` 走 Info 级而非 Warn——known case，不噪音化日志
- **单轮 LLM 无 agent loop**：bundle + system prompt 单次 round-trip，成本可控
- **固定 systemPrompt 中文**：3 段话结构（定性/根因/排障步骤），terse 防 LLM 发挥
- **Temperature 0.2**：偏确定性，避免调查报告发散
- **capUserMessage 不保 JSON 完整性**：注释明示 LLM 能读截断 tail，避免过度工程
- **Event 持久化**：LLM 回复写 Event 行，IncidentDetail SPA 顶部展示——operator 第一时间看到
- **StatusAfter / Severity 冗余存储**：Event 行冗余 incident 状态，便于 audit 无需 join
- **ActorTypeSystem**：标记系统行为，与人类 operator 行为区分
- **失败静默**：纯增量，correlate/LLM/persist 任一失败仅 log，不改 incident 状态
- **defaultLLMTimeout 120s**：注释明示与 llm.client.go defaultTimeout 对齐，60s 之前对 reasoning model false-fail

## 8. 注意事项

- **可选服务**：LLM 未配置时 main.go 跳过 wiring，alert usecase 容忍 nil investigator
- **队列满丢弃**：burst > 100 直接 drop + warn，不阻塞——alert 风暴时部分 incident 不调查
- **无重试**：gatherBundle / LLM / persist 任一失败即放弃，不重试——纯增量语义
- **`capUserMessage` 截断不保 JSON**：LLM 能读但若 bundle 关键信息在尾部可能丢失
- **systemPrompt 中文硬编码**：匹配 operator 受众，跨语言场景需扩展
- **`Temperature 0.2`**：偏确定，但非 0——极端情况下 LLM 仍可能发散
- **worker pool 不动态扩缩**：固定 cfg.Workers，突发流量靠 buffer + drop
- **`Close` 阻塞 drain**：每个 in-flight 最长 120s，Close 总耗时 ≤ 120s
- **`runOne` 无 panic recovery**：注释明示"never panic out of a worker"，但实际无 recover——若 correlate_incident 工具 panic 仍会 crash worker goroutine（仅影响该 worker，wg.Done 已 defer）
- **Event title "AI 初查" 硬编码**：SPA 根据 event_type 识别，title 仅展示用
- **`UserMsgCap 30KB`**：correlate_incident 已 trim 到 ~100KB，此处二次 cap 防 LLM context 溢出
