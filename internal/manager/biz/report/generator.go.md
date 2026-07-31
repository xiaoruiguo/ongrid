# generator.go

## 1. 概述

`generator.go` 实现 report 包的报告生成器 —— 把 pending 报告变成 ready 报告的全流程：收集 facts → 跑 reporter worker → 写 ContentJSON → flip status → 投递 IM。

**反幻觉契约的关键执行点**：`generate` 在 LLM 返回后用 facts 覆盖 `Content.Hero` / `Resource` / `Fleet` / `Actions` / `Changes` / `Assets` / `Usage`。LLM 只拥有 `Narrative` / `KeyIncidents` 排序评论 / `Advice`。

`workerGenerator` 是真实实现；`nopGenerator` / `unavailableGenerator` 是降级占位（PR-1 / LLM 未配置）。

## 2. 包信息

- 包名：`report`
- 路径：`internal/manager/biz/report`

## 3. 关键类型与接口

### WorkerSpawner 接口

```go
type WorkerSpawner interface {
    SpawnWorker(ctx, req chatruntime.SpawnRequest) (*chatruntime.Worker, error)
}
```

窄 seam onto `chatruntime.Runtime`。注释：mirror investigator's WorkerSpawner，让 main.go wiring 一致。测试注入 fake 返回 canned ContentJSON。

### GeneratorConfig

```go
type GeneratorConfig struct {
    Persona        string        // reporter agent name；默认 model.DefaultReporterPersona
    Timeout        time.Duration // 0 → 120s（项目级 LLM timeout floor）
    DefaultLocale  string        // 空 → "en"（feedback_ai_output_locale）
    PublicURL      string        // manager 外部可达 base，build deep link
}
```

### workerGenerator

```go
type workerGenerator struct {
    repo      Repo
    facts     FactsCollector
    spawner   WorkerSpawner
    deliverer Deliverer  // nil = in-app only
    ready     func(context.Context) error
    cfg       GeneratorConfig
    log       *slog.Logger
}
```

### nopGenerator / unavailableGenerator

降级占位。`nopGenerator.Generate` 是 no-op；`unavailableGenerator.Generate` 也是 no-op 但 `Ready` 返回 error。

## 4. 关键函数与流程

### NewWorkerGenerator

构造。`repo == nil || facts == nil || spawner == nil` panic（wiring error）。默认值填充：Persona / Timeout 120s / DefaultLocale "en" / log slog.Default。

### WithDeliverer / WithReadyCheck

Builder 链。`WithDeliverer` nil-safe。`WithReadyCheck` 注入轻量 preflight（manual/scheduled trigger 在创 pending 行前调）。

### Ready

```go
func (g *workerGenerator) Ready(ctx) error
```

`g.ready == nil` → nil；否则调 `g.ready(ctx)`。

### Generate（入口）

```go
func (g *workerGenerator) Generate(ctx, reportID string)
```

1. `repo.GetReport(ctx, reportID)`
2. `rpt.Status != StatusPending` → no-op（double-fire / restart re-attach）
3. `rpt.Status = StatusGenerating` + `repo.UpdateReport`（失败 warn 继续，UI 最坏 lag 一个状态）
4. `g.generate(ctx, rpt)` 失败 → `g.fail(ctx, rpt, err.Error())`

注释：Always reaches terminal state（ready/failed）—— panic 或 early return 留 "generating" 会 UI strand。

### generate（核心）

```go
func (g *workerGenerator) generate(ctx, rpt *model.Report) error
```

1. `context.WithTimeout(context.WithoutCancel(ctx), cfg.Timeout)` —— 用 fresh timeout context，不继承请求 ctx
2. `period = Period{rpt.PeriodStart, rpt.PeriodEnd}` + `prev = previousPeriod(period)`
3. `scope = ParseScope(rpt.ScopeJSON)`
4. `facts, err := g.facts.Collect(ctx, period, prev, scope)` —— 纯 SQL/Prom 收集
5. `prompt = g.buildPrompt(rpt, facts)` —— 渲染 reporter worker 的 user message
6. `g.spawner.SpawnWorker(ctx, SpawnRequest{AgentName, Prompt, Background:false, SessionKind:"report", OwnerUserID, Locale})`
7. `worker.Err` 非空 → return error
8. `content, err := ParseContent(extractJSON(worker.Result))` —— lenient JSON 提取
9. **反幻觉覆盖**：
   ```go
   content.Hero = facts.Hero
   content.Resource = facts.Resource
   content.Fleet = facts.Fleet
   content.Actions = facts.Actions
   content.Changes = facts.Changes
   content.Assets = facts.Assets
   content.Usage = facts.Usage
   content.KeyIncidents = mergeIncidents(facts.Incidents, content.KeyIncidents)
   content.Version = ContentVersion
   content.Metadata = ContentMeta{...}
   ```
10. `rpt.ContentJSON = content.MustJSON()` + `rpt.ContentMD = content.RenderMarkdown(...)` + `rpt.SummaryText = truncate(headline, 510)`
11. `rpt.Status = StatusReady` + `rpt.GeneratedAt = now` + `repo.UpdateReport`
12. `g.deliver(ctx, rpt)` —— best-effort IM 投递

### buildPrompt

渲染 reporter worker 的 user message：
1. locale-aware scaffolding（en/zh 切换）
2. facts JSON（`factsJSON` 用 `MarshalIndent`）
3. period 信息
4. schedule override（若 `rpt.ScheduleID` 非空，取 `schedule.PromptOverride`）
5. explicit output-language directive（`localeDirective`）
6. "按 persona 描述的 ContentJSON schema 输出，只输出 JSON"

注释：localize scaffolding 是为防弱模型（deepseek-v4-flash）在中文 directive 接英文 prompt 时泄漏中文到段落。

### localeDirective

```go
func localeDirective(locale string) string
```

explicit output-language line。`en` → 英文指令；`zh` → 中文指令；其它 → ""（persona implicit language wins）。注释：belt and braces for weak models。

### deliver

```go
func (g *workerGenerator) deliver(ctx, rpt *model.Report)
```

1. `g.deliverer == nil || rpt.ScheduleID == nil` → return
2. `repo.GetSchedule(*rpt.ScheduleID)` 取 schedule 的 `ChannelIDsJSON`
3. `json.Unmarshal` 取 `channelIDs`
4. `summary = deliveryFor(rpt, g.deepLink(rpt.ID))`
5. `records = g.deliverer.Deliver(ctx, summary, channelIDs)`
6. `recordDelivery(rpt, records)` + `repo.UpdateReport`

### fail

```go
func (g *workerGenerator) fail(ctx, rpt, reason)
```

`rpt.Status = StatusFailed` + `rpt.ErrorMsg = truncate(reason, 2000)` + `repo.UpdateReport(context.WithoutCancel(ctx), rpt)`。注释：用 background ctx 让 cancelled/timed-out 请求仍记录失败 terminal state。

### mergeIncidents

```go
func mergeIncidents(facts []IncidentFact, llm []KeyIncident) []KeyIncident
```

从 SQL-true facts 重建 KeyIncidents（top 5 by duration），保留 LLM 提供的 `RootCauseSnippet`（按 id 匹配）。insertion sort by duration desc（小切片）。

### previousPeriod / extractJSON / truncate / factsJSON

辅助函数。`previousPeriod` 返回等长的前一窗口；`extractJSON` lenient 提取 JSON（strip ```json fence + 取首个 `{` 到末尾 `}`）；`truncate` 截断 + "…"；`factsJSON` `MarshalIndent` 失败回退 "{}"。

## 5. 依赖关系

### 外部包

- `context` / `encoding/json` / `fmt` / `log/slog` / `strings` / `time`

### 内部包

- `chatruntime "github.com/ongridio/ongrid/internal/manager/biz/aiops/chatruntime"` —— `SpawnRequest` / `Worker`
- `model "github.com/ongridio/ongrid/internal/manager/model/report"` —— `Report` / `Status*` / `DefaultReporterPersona`
- 同包：`Repo` / `FactsCollector` / `Deliverer` / `Period` / `ParseScope` / `PeriodFor` / `Content` / `ParseContent` / `ContentVersion` / `ContentMeta` / `HeroStat` / `KeyIncident` / `deliveryFor` / `recordDelivery` / `flattenEntities` / `RenderMarkdown`

### 被谁调用

- `usecase.go` 的 `FireSchedule` / `GenerateNow` 用 `go u.gen.Generate(context.WithoutCancel(ctx), rpt.ID)` 异步触发

## 6. 并发与资源管理

- **Generate 异步跑**：caller `go u.gen.Generate(...)`，不阻塞 evaluator/API
- **fresh timeout context**：`context.WithTimeout(context.WithoutCancel(ctx), cfg.Timeout)` —— 不继承请求 ctx，请求返回后不杀生成
- **fail 用 background ctx**：`context.WithoutCancel(ctx)` 让 cancelled 请求仍记录 terminal state
- **无锁**：workerGenerator 无共享可变状态；并发安全由 repo 保证
- **Always terminal state**：Generate 总是 reach ready/failed，防 UI strand

## 7. 设计模式与亮点

### 反幻觉覆盖

`generate` 在 LLM 返回后用 facts 覆盖 `Content` 的所有数字字段。LLM 只拥有 prose。这是产品安全设计 —— 防模型篡改数字泄漏到报告。注释明确："Numbers are overwritten from facts post-LLM (defense-in-depth)"。

### Locale-aware scaffolding

`buildPrompt` localize 整个 scaffolding（不只 directive）。注释：防弱模型在中文 directive 接英文 prompt 时泄漏中文到段落。belt and braces：scaffolding + explicit directive 双重保险。

### WithoutCancel 模式

`generate` 与 `fail` 都用 `context.WithoutCancel(ctx)`。让请求 ctx 取消后仍能完成 terminal state 持久化。这是优雅 shutdown 的关键。

### Always terminal state

注释：Always reaches terminal state（ready/failed）—— panic 或 early return 留 "generating" 会 UI strand。`Generate` 用 `g.fail` 兜底任何 error。

### mergeIncidents 保留 LLM snippet

`mergeIncidents` 从 facts 重建 incidents（top 5 by duration），但保留 LLM 提供的 `RootCauseSnippet`（按 id 匹配）。让 SQL-true 的 id/duration/status 与 LLM 的 RCA snippet 共存。

### extractJSON lenient

`extractJSON` strip ```json fence + 取首个 `{` 到末尾 `}`。注释：让 chatty model 仍 yield parseable content。mirror `query_translate` 的 lenient parse。

### nopGenerator / unavailableGenerator 降级

PR-1（generator 未实现）用 nopGenerator；LLM 未配置用 unavailableGenerator（`Ready` 返回 error）。让路由始终 mount，不因 generator 缺失而 404。

### WorkerSpawner seam

`WorkerSpawner` 是窄接口，测试注入 fake 返回 canned ContentJSON 不需起整个 graph kernel。mirror investigator 的 WorkerSpawner 让 main.go wiring 一致。

## 8. 注意事项

- **反幻觉覆盖不能漏字段**：`generate` 必须覆盖所有数字字段（Hero/Resource/Fleet/Actions/Changes/Assets/Usage）。漏一个就给 LLM 篡改机会
- **`Generate` 异步跑**：caller 用 `go u.gen.Generate(...)`，不等待。报告 ready/failed 状态由 Generate 内部持久化
- **`fail` 用 background ctx**：cancelled 请求仍记录失败。若 `repo.UpdateReport` 也失败，只 warn log，报告可能卡在 generating —— 应有外部监控检测
- **`extractJSON` lenient**：接受 chatty model 输出。但极端情况（无 `{` 或无 `}`）返回原样，`ParseContent` 会失败
- **`mergeIncidents` top 5**：硬编码 5。若需调整报告密度，改此值
- **`buildPrompt` 双语硬编码**：`mtr(zh, eng)` 调用散布。新增 scaffolding section 要同步双语
- **`localeDirective` 仅 en/zh**：其它 locale 返回 ""，persona implicit language wins。若新增 locale 支持需扩展
- **`Generate` 不返回 error**：签名 `Generate(ctx, reportID)` 无返回值。caller 无法知道失败 —— 依赖 `fail` 持久化 status 让 UI 看到
- **`deliver` best-effort**：投递失败记 `delivery_json`，不影响 ready 报告。报告已 persisted，投递是附加
- **`scheduleOverride` 用 context.Background**：`repo.GetSchedule(context.Background(), ...)` 不传 ctx。若 DB 慢会阻塞 —— 应改用 caller ctx
