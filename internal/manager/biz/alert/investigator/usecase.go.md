# `usecase.go` 技术实现文档

> 源文件：`internal/manager/biz/alert/investigator/usecase.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/alert/investigator`

## 1. 概述

本文件是 investigator 子系统的 orchestrator——alert incident 触发时自动 spawn LLM-driven RCA worker。`InvestigateAsync`（实现 `alert.Investigator` 接口）是 firing-path 入口，三层 gate：(1) severity floor；(2) per-incident_id inflight 去重；(3) 全局并发 cap（`MaxConcurrent`，默认 5，sem 信号量）。通过 gate 后创建 pending 行 + goroutine 跑 `run`：`context.Background()` + `WorkerTimeout`，spawn chatruntime worker，等终态；MaxStep 错误走 salvage 路径（拼接 tool 结果作 partial answer）；成功后调 `extractStructured` Pass-2 抽取结构化报告，`MarkReady` 持久化。`ForceEnqueue` 是手动触发：stop+delete prior、release inflight、再 Enqueue。`BackfillUnstartedIncidents` 是启动补偿（fresh install 未配 LLM 期间漏掉的 incidents）。

## 2. 包信息

- **包名**：`investigator`
- **所属模块**：`internal/manager/biz/alert/`
- **依赖方向**：被 `alert/usecase.go::RecordFiring`（isNew 时）+ HTTP handler（手动触发）+ `cmd` 装配（Backfill）调用；依赖 `internal/manager/biz/aiops/chatruntime` + `internal/manager/model/aiops` + `internal/manager/model/alert`

## 3. 关键类型与接口

```go
type Repo interface {
    Create(ctx, rep *alertmodel.InvestigationReport) error
    UpdateStatus(ctx, id, status, reason string) error
    AttachWorker(ctx, id, workerID, auditSessionID string) error
    MarkReady(ctx, id string, fields ReadyFields) error
    RecentlySpawnedFor(ctx, ruleName string, deviceID *uint64, window time.Duration) (bool, error)
    GetByIncident(ctx, incidentID uint64) (*alertmodel.InvestigationReport, error)
    DeleteByIncident(ctx, incidentID uint64) error
    ListIncidentsWithoutReport(ctx, since time.Time, limit int) ([]uint64, error)
}

type IncidentLoader func(ctx, id uint64) (*alertmodel.Incident, error)
type WorkerSpawner interface {
    SpawnWorker(ctx, req chatruntime.SpawnRequest) (*chatruntime.Worker, error)
    StopWorker(ctx, workerID string) error
}
type MessageReader interface { ListMessages(ctx, sessionID string, limit int) ([]*aiopsmodel.Message, error) }
type RelatedAlertQuerier interface { RelatedToIncident(ctx, target *alertmodel.Incident, halfWindow time.Duration, limit int) ([]*alertmodel.Incident, error) }

type ReadyFields struct {
    RootCause, AffectedWindow, PinpointedTargetJSON, RelatedAlertsJSON, EvidenceJSON, SuggestedActionsJSON, FindingsMD string
    Confidence *float64
    ConfidenceFactorsJSON string
    ToolCallCount int
}

type Config struct {
    Enabled bool
    MinSeverity string  // 默认 "warning"
    DedupWindow time.Duration  // 默认 5min（旧字段，gate 改为 per-incident_id 后保留兼容）
    WorkerTimeout time.Duration  // 默认 5min
    AgentName string  // 默认 "incident-investigator"
    SummarizerModel, SummarizerProvider string
    SummarizerTimeout time.Duration  // 默认 120s
    MaxConcurrent int  // 默认 5
    DefaultLocale string
}

type Usecase struct {
    repo Repo
    spawner WorkerSpawner
    summarizer LLMSummarizer
    related RelatedAlertQuerier
    messages MessageReader
    cfg Config
    log *slog.Logger
    inflightMu sync.Mutex
    inflight map[string]bool  // key = incident_id
    sem chan struct{}  // nil = uncapped
}
```

## 4. 关键函数与流程

### `NewUsecase`

- **签名**：`func NewUsecase(repo, spawner, summarizer, cfg, log) *Usecase`
- **职责**：构造 orchestrator，应用默认值
- **流程**：
  1. log nil → `slog.Default()`
  2. DedupWindow=0 → 5min
  3. WorkerTimeout=0 → 5min
  4. SummarizerTimeout=0 → 120s（注释：从 30s 提到 120s 因 cluster default 移到慢 reasoning model）
  5. MinSeverity="" → "warning"
  6. AgentName="" → "incident-investigator"
  7. MaxConcurrent=0 → 5
  8. log 加 `comp=investigator` 字段
  9. `MaxConcurrent > 0` → `uc.sem = make(chan struct{}, MaxConcurrent)`
- **错误处理**：无

### `WithRelatedQuerier` / `WithMessageReader`

- 链式 setter；可选注入；返回 `*Usecase`

### `tryAcquireSem` / `releaseSem`

- **签名**：`func (uc *Usecase) tryAcquireSem() bool` + `func (uc *Usecase) releaseSem()`
- **职责**：非阻塞计数信号量
- **流程**：
  - acquire：`sem == nil` → true；`select { case sem <- {}: true; default: false }`
  - release：`sem == nil` → return；`select { case <-sem: ; default: }`（防 double-release）
- **错误处理**：无；over-cap 调用方返回 false

### `InvestigateAsync`

- **签名**：`func (uc *Usecase) InvestigateAsync(incident *alertmodel.Incident)`
- **职责**：实现 `alert.Investigator` 接口；firing-path 入口
- **流程**：`uc.Enqueue(context.Background(), incident)`
- **契约**：永不阻塞 caller；gate 失败 log + 写 skipped 行，不上抛

### `Enqueue` / `EnqueueWith`

- **签名**：`func (uc *Usecase) Enqueue(ctx, incident)` + `EnqueueWith(ctx, incident, opts EnqueueOpts)`
- **职责**：lower-level 入口，三层 gate
- **流程**：
  1. `!cfg.Enabled || incident == nil` → return
  2. **Gate 1 severity**：`!severityAtLeast(incident.Severity, cfg.MinSeverity)` → return（info/debug 静默跳过）
  3. **Gate 2 per-incident_id inflight**：
     - `key := strconv.FormatUint(incident.ID, 10)`
     - `inflightMu.Lock`；`inflight[key]` 已存在 → Unlock + return；否则 `inflight[key] = true` + Unlock
     - 注释：旧 (rule, device, 5m) dedup 过宽，suppress 了同设备不同 incident 的分析；改为 per-incident_id 让每个失败事件独立分析
  4. **Gate 3 concurrency cap**：`acquired := tryAcquireSem()`；未拿到：
     - 清 inflight
     - 写 skipped 行 `"concurrency limit reached (N workers in flight)"`
     - Info log
     - return
  5. 创建 pending 行 `repo.Create(ctx, rep)`；err → releaseSem + 清 inflight + Info log + return
  6. `locale := opts.Locale ?? cfg.DefaultLocale`
  7. `go uc.run(rep.ID, *incident, key, locale)`
- **错误处理**：所有 gate 失败静默或写 skipped 行；pending 行创建失败 Info log

### `ForceEnqueue` / `ForceEnqueueWith`

- **签名**：`func (uc *Usecase) ForceEnqueue(ctx, incident) error` + `ForceEnqueueWith(ctx, incident, opts) error`
- **职责**：手动触发；stop+delete prior + re-enqueue
- **流程**：
  1. nil 守卫 + Enabled + incident nil + severity floor 校验
  2. `prior := repo.GetByIncident(incident.ID)`；prior 有 WorkerID → `spawner.StopWorker`（best-effort，Warn 不阻断）
  3. `repo.DeleteByIncident(incident.ID)` 硬删除（覆盖 visible + soft-deleted；unique index over incident_id 无 deleted_at filter，soft-deleted 会阻塞 Create）
  4. 清 inflight key
  5. `uc.EnqueueWith(ctx, incident, opts)`
- **错误处理**：delete prior 失败 return err；stop worker 失败 Warn 不阻断

### `BackfillUnstartedIncidents`

- **签名**：`func (uc *Usecase) BackfillUnstartedIncidents(ctx, since time.Time, limit int, loadIncident IncidentLoader) (int, error)`
- **职责**：启动补偿——fresh install 未配 LLM 期间漏掉的 incidents
- **流程**：
  1. `!Enabled || loadIncident == nil` → return 0, nil
  2. `ids := repo.ListIncidentsWithoutReport(ctx, since, limit)`；err → return
  3. per-id：`loadIncident(ctx, id)`；失败 Info log + skip
  4. 跳过 resolved incidents（调查已关闭 alert 产 stale findings 烧 LLM）
  5. `uc.Enqueue(ctx, inc)`；dispatched++
  6. 返回 dispatched
- **错误处理**：list 失败 return err；单 incident 加载失败 skip

### `run`

- **签名**：`func (uc *Usecase) run(reportID string, incident alertmodel.Incident, dedupKeyVal string, locale string)`
- **职责**：per-investigation goroutine；`context.Background()` + WorkerTimeout
- **流程**：
  1. defer：清 inflight + releaseSem
  2. `spawner == nil` → `UpdateStatus(Skipped, "spawner not wired at boot")` + return
  3. `ctx, cancel := context.WithTimeout(context.Background(), cfg.WorkerTimeout)` + defer cancel
  4. `prompt := renderAlertPrompt(&incident, locale)`
  5. `worker, err := spawner.SpawnWorker(ctx, SpawnRequest{AgentName, Prompt, Background: false, SessionKind: "investigation"})`；err → `UpdateStatus(Failed, "spawn: ...")` + return
  6. `worker == nil` → `UpdateStatus(Failed, "spawn: nil worker")` + return
  7. `repo.AttachWorker(reportID, worker.ID, worker.SessionID)`；err → Warn 不阻断
  8. `worker.Err != ""`：
     - `uc.messages != nil && isMaxStepsError(workerErr)`：
       - `salvaged := uc.salvagePartialAnswer(worker.SessionID)`；非空 → `worker.Result = salvageNote + salvaged`，fall through 到成功路径
       - 空 → `UpdateStatus(Failed, workerErr)` + return
     - 否则 → `UpdateStatus(Failed, workerErr)` + return
  9. `finalAnswer := TrimSpace(worker.Result)`；空 → `UpdateStatus(Failed, "worker returned empty final answer")` + return
  10. `toolCalls := uc.countToolCalls(worker.SessionID)`
  11. `fields := uc.extractStructured(context.Background(), incident, finalAnswer, toolCalls, locale)`（Pass-2，含 fallback）
  12. `repo.MarkReady(reportID, fields)`；err → Warn + return
  13. Info log "investigation finished" + elapsed + root_cause
- **错误处理**：每个失败路径写 Failed/Skipped 行 + return；salvage 路径让 MaxStep 不至完全失败

### `renderAlertPrompt`

- **签名**：`func renderAlertPrompt(in *alertmodel.Incident, locale string) string`
- **职责**：构造 worker 初始 user message
- **流程**：
  1. "An alert fired. Investigate the root cause and report back."
  2. Incident 元数据（incident_id / rule / severity / first/last_fired_at / device_id / value / threshold / summary / description）
  3. "Start with correlate_incident to pull metrics + logs + traces + topology around the fire window. Then drill in..."
  4. **HARD budget**："BUDGET: hard cap 10 tool calls. By tool call #7 you MUST start writing the final report; by #10 you MUST emit it..."（注释：放 user prompt 而非 system，因 GLM/非前沿模型对 user 约束更严）
  5. `localeDirective(locale)` 追加（末尾，sticky）
- **错误处理**：无

### `localeDirective`

- **签名**：`func localeDirective(locale string) string`
- **职责**：locale → 语言指令
- **流程**：取 primary subtag（`en` / `zh`）；en → "LANGUAGE: Write the entire final report in English..."；zh → "LANGUAGE: 全程用简体中文撰写最终报告..."；其他 → `""`

### `salvagePartialAnswer`

- **签名**：`func (uc *Usecase) salvagePartialAnswer(sessionID string) string`
- **职责**：MaxStep 错误时拼接 worker chat trail 作 partial answer
- **流程**：
  1. `messages == nil || sessionID == ""` → `""`
  2. `ctx, cancel := WithTimeout(3s)` + defer cancel
  3. `msgs := messages.ListMessages(ctx, sessionID, 100)`（cap 100，覆盖最长 ReAct loop）
  4. per-msg：
     - assistant + 非空 content → `**Assistant:** <content>`
     - tool + 非空 content → `**Tool [<name>]:** <body截600字符>…`
  5. 返回 TrimSpace 的拼接
- **错误处理**：失败返 `""`，让 run 走 Failed 路径

### `countToolCalls`

- **签名**：`func (uc *Usecase) countToolCalls(sessionID string) int`
- **职责**：数 worker transcript 的 tool-result turns
- **流程**：`messages == nil || sessionID == ""` → 0；`ListMessages(ctx, sessionID, 200)`；数 `Role == "tool"` 的消息
- **错误处理**：失败返 0

### `severityAtLeast` / `severityRank`

- `severityRank`：critical/crit/page=4、error/high=3、warning/warn=2、info/notice=1、debug=0、unknown=2（注释：unknown 当 warning，让无 severity 的旧规则仍触发）
- `severityAtLeast(have, min)`：`severityRank(have) >= severityRank(min)`

### `isMaxStepsError`

- **签名**：`func isMaxStepsError(s string) bool`
- **职责**：匹配 eino graph runtime MaxStep 超限错误
- **流程**：`strings.ToLower`；含 "exceeds max steps" / "exceededmaxsteps" / "graphrunerror" → true
- **注释**：substring match 因 wrapped error 格式含 node path + bracket fluff，跨版本不稳定

### `firstParagraphOneLine`

- **签名**：`func firstParagraphOneLine(s string, max int) string`
- **职责**：fallback root_cause 启发式
- **流程**：
  1. per-line：
     - 空行 → 跳过
     - `#` 开头（标题）→ 跳过
     - `onlyChars(line, "-=")`（分隔线）→ 跳过
     - `**...**` 短（≤24 rune）→ section header，跳过；长 → 去 `**` 保留内容
     - 去首部 `*-> \t` 列表/引用标记
     - TrimSpace；非空 → 截到 max rune 返回
  2. 全部跳过 → `""`
- **错误处理**：无

## 5. 依赖关系

- **内部包**：
  - `internal/manager/biz/aiops/chatruntime`（`SpawnRequest` / `Worker`）
  - `internal/manager/model/aiops`（`Message`）
  - `internal/manager/model/alert`（`Incident` / `InvestigationReport` / 状态常量）
- **外部库**：`context` / `fmt` / `log/slog` / `strconv` / `strings` / `sync` / `time`
- **被调用方**：`alert/usecase.go::RecordFiring`（isNew 时 `InvestigateAsync`）、HTTP handler（`ForceEnqueue`）、`cmd` 装配（`BackfillUnstartedIncidents`）
- **依赖**：本包 `report_extractor.go`（`extractStructured` / `LLMSummarizer` / `firstParagraphOneLine` / `localeDirective`——后者实际在本文件）

## 6. 并发与资源管理

- **`inflightMu`（Mutex）**：保护 `inflight` map——per-incident_id 去重，毫秒级 belt-and-braces（DB unique 是持久检查）
- **`sem`（buffered chan）**：计数信号量，cap=`MaxConcurrent`；send acquire / recv release；nil 时 uncapped
- **`run` goroutine**：`context.Background()` + WorkerTimeout——alert pipeline ctx 是 request-scoped 短的，investigation 是分钟级
- **defer release**：`run` 顶部 defer 清 inflight + releaseSem，覆盖 success / error / panic / timeout 所有路径
- **`salvagePartialAnswer` / `countToolCalls` 独立 3s ctx**：不继承 run 的 ctx（可能已超时）

## 7. 设计模式与亮点

- **三层 gate**：severity floor → per-incident_id inflight → 全局并发 cap；每层失败不同处理（静默 / 写 skipped / 写 skipped）
- **per-incident_id 去重**：旧 (rule, device, 5m) 过宽，suppress 同设备不同 incident；改为 per-incident_id 让每个失败事件独立分析
- **非阻塞 sem**：`tryAcquireSem` 用 `select default`；over-cap 写 skipped 行而非 queueing，操作员立即看到 cap 命中
- **`context.Background()` for run**：alert ctx 太短；investigation 用自有 WorkerTimeout
- **MaxStep salvage 路径**：eino ReAct 跑满 step budget 时通常已调 10+ tool 收集数据，只是没写最终综合；拼接 tool trail 作 partial answer + Pass-2 抽取——操作员得低置信度 partial 报告而非空失败卡
- **HARD budget in user prompt**：注释提到 GLM/非前沿模型对 user 约束比 system 严；放 user prompt 让模型更可能遵守 10 tool cap
- **locale 末尾 sticky**：persona 文件中文试图设语言；locale 指令放 prompt 末尾覆盖 persona——AI 输出跟随用户 UI locale
- **`BackfillUnstartedIncidents` 启动补偿**：fresh install 未配 LLM 期间 incident 无 investigator；operator 加 provider + 重启后此 pass 修复窗口
- **跳过 resolved incidents**：backfill 不调查已关闭 alert——产 stale findings 烧 LLM
- **`ForceEnqueue` 硬删除**：unique index over incident_id 无 deleted_at filter；soft-deleted 行会阻塞 Create——必须硬删除
- **`severityRank` unknown=2**：让无 severity 的旧规则仍触发（当 warning）——保守兼容
- **`firstParagraphOneLine` 启发式**：跳过 markdown 标题/分隔线/短 bold header，落到首个 prose 段落——root_cause 读起来像句子而非 section title

## 8. 注意事项

- **`cfg.Enabled` 默认 OFF**：注释提到 first-version default OFF 避免现有部署突然烧 LLM token；operator 通过 `ONGRID_INVESTIGATOR_ENABLED=true` 开启
- **`MaxConcurrent` 默认 5**：over-cap 写 skipped 行； defends LLM provider rate-limits + bounded RAM
- **`WorkerTimeout` 默认 5min**：更长 run 被 kill + 标 Failed
- **`SummarizerTimeout` 默认 120s**：从 30s 提到 120s 因慢 reasoning model
- **`isMaxStepsError` substring match**：跨 eino 版本不稳定；新增错误形态需扩展匹配
- **`salvagePartialAnswer` cap 100 消息**：覆盖最长 ReAct loop（25 turns × 2 msg + tool ≈ 75）
- **`countToolCalls` cap 200 消息**：比 salvage 多，因 tool result 可能更多
- **`run` 用 `context.Background()`**：不继承 caller ctx；WorkerTimeout 是唯一上限
- **`ForceEnqueue` 不跳 severity floor**：operator 显式请求仍受 MinSeverity 门控
- **`BackfillUnstartedIncidents` 仍走 Enqueue gate**：severity / inflight / cap 都生效；dispatched 不等于 will-produce-report
- **`inflight` key = incident_id**：不是 (rule, device)；同设备不同 incident 并行分析
- **`sem` nil = uncapped**：`MaxConcurrent <= 0` 时不限并发（legacy 行为）
- **`renderAlertPrompt` HARD budget**：10 tool cap、#7 必须开始写报告、#10 必须 emit；同一 tool 不超 3 次
