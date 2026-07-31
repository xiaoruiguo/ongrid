# report/store/facts.go

## 1. 概述

本文件实现 `bizreport.FactsCollector` 接口，为报告生成提供**确定性的事实输入**。它从两个数据源计算报告所需的事实：
- **Prometheus**（通过注入的 `promquery.Client`）：周期内 fleet 资源趋势（CPU/内存/磁盘 avg/peak），报告以这些指标开场，确保「平静期」报告仍有实质内容。
- **SQL**（通过 `*gorm.DB` 查询 `devices` / `alert_incidents` / `chat_mutating_proposals` / `audit_logs` / `user_agents` / `installed_skills` / `knowledge_repos` / `chat_sessions` / `chat_messages`）：监控覆盖、告警、变更日志、资产、用量。

**核心设计原则**：
- **不调 LLM、不写数据**——纯只读计算，便于测试与重放。
- **优雅降级**——任一数据源失败回退到该源的零值（Prometheus 挂 → `Resource.Available=false`），**不会让整个报告失败**。
- **Hero 区以非告警信号开场**——设备数、CPU/mem 均值、磁盘峰值，让平静期卡片仍有意义；仅当 Prom 无数据时才退回 incidents 计数。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/report/store`
- **依赖方向**：`controlplane → biz/report → data/report/store → model + promquery`；接口 `bizreport.FactsCollector` 在消费方 biz 层定义，本文件提供实现，编译期断言 `var _ bizreport.FactsCollector = (*FactsCollector)(nil)`。

## 3. 关键类型与接口

```go
// FactsCollector 实现 bizreport.FactsCollector，从 Prom + SQL 收集报告事实。
type FactsCollector struct {
    db   *gorm.DB
    prom PromQuerier  // nil = 资源趋势不可用，其余 SQL 部分仍渲染
}

// PromQuerier 是 Prometheus 的窄接口，由 *promquery.Client 实现。
type PromQuerier interface {
    Query(ctx context.Context, expr string, ts time.Time) (*promquery.InstantResult, error)
}

func NewFactsCollector(db *gorm.DB, prom PromQuerier) *FactsCollector

// 编译期接口断言
var _ bizreport.FactsCollector = (*FactsCollector)(nil)

// 严重度排序：critical > warning > info，用于 severityMin scope 过滤
var severityRank = map[string]int{"info": 0, "warning": 1, "critical": 2}

// changeActions 是值得在报告中呈现的审计动作白名单（产品侧 mutation），排除 read/login 噪声
var changeActions = []string{
    "rule_create", "rule_update", "rule_delete",
    "setting_update", "setting_delete",
    "device_update", "device_delete",
    "channel_create", "channel_update", "channel_delete",
    "repo_create", "repo_delete",
    "user_create", "user_update", "user_delete",
}

// 单个指标的 fleet 聚合表达式对（avg over period + peak over period）
type resourceExpr struct {
    avgExpr  string
    peakExpr string
}

// 内部行结构：incidentRow
type incidentRow struct {
    ID           uint64
    RuleName     string
    Severity     string
    Status       string
    DeviceID     *uint64
    FirstFiredAt time.Time
    ResolvedAt   *time.Time
}
```

## 4. 关键函数与流程

### `NewFactsCollector(db, prom) *FactsCollector`

构造器，`prom` 可为 nil（资源趋势段降级）。

### `Collect(ctx, period, prev, scope) (*bizreport.ReportFacts, error)`

- **职责**：聚合所有事实段，返回完整 `ReportFacts`。
- **流程**：
  1. 初始化 `facts`，预填 `Period` / `PrevPeriod` / 空 `AlertCounts` map。
  2. `collectIncidents`（**唯一会返回 error 的步骤**——其余均吞掉错误降级）；失败直接 `return nil, err`。
  3. 依次调用：
     - `collectActions` — agent mutation + safe action 计数
     - `collectAlertCounts` — 按 severity 分组
     - `collectFleet` — 设备总数/在线/角色分布
     - `collectChanges` — 最近 20 条产品 mutation 审计
     - `collectAssets` — 本周期新建 agent/skill/repo 数
     - `collectUsage` — chat sessions + LLM token 花费
     - `collectResource` — Prom 资源趋势（avg/peak）
  4. `buildHero` 构造 Hero 区统计（含 prev 周期对比 deltaPct）。
- **错误处理**：仅 `collectIncidents` 失败中断；其余用 `_ = ...` 吞掉错误返回零值，符合「优雅降级」原则。

### `collectResource(ctx, p) bizreport.ResourceFacts`

- **职责**：从 Prom 拉周期内 CPU/mem/disk 的 fleet avg + peak。
- **流程**：
  1. `c.prom == nil` → 返回零值（Available=false）。
  2. 周期长度 `dur = p.End - p.Start`；若 `<=0` 或 `>30d` 则夹到 7d（避免 Prom 子查询无界）。
  3. `durStr := "<hours>h"`。
  4. `resourceExprs(durStr)` 生成 cpu/mem/disk 三组表达式，每组含 `avg_over_time(...[dur:5m])` 与 `max_over_time(...[dur:5m])`。
  5. 对每个指标调 `scalarAt` 取 avg 与 peak，任一成功即 `any=true`。
  6. `r.Available = any`。
- **PromQL 设计**：复用 Monitor 页同款 `node_cpu_seconds_total` / `node_memory_*` / `node_filesystem_*` 表达式（保证 series 存在），外面套 subquery 求周期 avg/max。

### `scalarAt(ctx, expr, ts) (float64, bool)`

- **职责**：执行即时查询，提取单一数值。
- **流程**：
  1. `c.prom.Query(ctx, expr, ts)`；错误或 nil → `return 0, false`。
  2. 按 `ResultType` 分支：
     - `"scalar"`：解析 `[<ts>, "<value>"]` 对的第二个元素。
     - `"vector"`：解析 `[{Value: [<ts>, "<value>"]}]` 第一个样本。
  3. 调 `parseQuotedFloat` 把带引号的字符串转 float64。
- **降级语义**：所有失败路径都返回 `false`，调用方据此跳过该指标。

### `parseQuotedFloat(raw) (float64, bool)`

- 解析 JSON 字符串 → float64。
- **NaN 守卫**：`f != f`（NaN）返回 false。
- **负值夹 0**：`f < 0 → f = 0`（CPU/内存利用率不应为负）。

### `collectFleet(ctx) bizreport.FleetFacts`

- **职责**：从 `devices` 表算 fleet 覆盖。
- **流程**：`SELECT online, roles FROM devices WHERE deleted_at IS NULL`；逐行统计 `Total` / `Online` 与按 bit 拆分的角色分布。
- **角色 bit 映射**：`1=server, 2=storage, 4=network, 8=database`；都不匹配归为 `unknown`。
- **降级**：SQL 错误返回零值 `FleetFacts`。

### `collectChanges(ctx, p) []bizreport.ChangeFact`

- **职责**：从 `audit_logs` 取本周期内最近 20 条产品 mutation。
- **流程**：`SELECT occurred_at, action, resource_type, resource_name, user_email FROM audit_logs WHERE occurred_at IN period AND action IN changeActions AND status='success' ORDER BY occurred_at DESC LIMIT 20`。
- **降级**：错误吞掉，返回空切片。

### `collectAssets(ctx, p) bizreport.AssetFacts`

- **职责**：算本周期新建 agent/skill/repo 数。
- **流程**：三次 `COUNT(*)`：
  - `user_agents WHERE created_at IN period`
  - `installed_skills WHERE installed_at IN period`
  - `knowledge_repos WHERE created_at IN period`
- **降级**：每次 count 错误吞掉返回 0。

### `collectUsage(ctx, p) bizreport.UsageFacts`

- **职责**：算 chat session 数与 LLM token 花费。
- **流程**：
  1. `COUNT(*) FROM chat_sessions WHERE created_at IN period AND kind='user'`。
  2. `SELECT COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0) FROM chat_messages WHERE created_at IN period`。
- **降级**：错误吞掉，零值。

### `collectIncidents(ctx, p, scope) ([]bizreport.IncidentFact, error)`

- **职责**：本周期内的告警 incident 列表（**唯一返回 error 的收集器**）。
- **流程**：
  1. 基础查询：`SELECT id, rule_name, severity, status, device_id, first_fired_at, resolved_at FROM alert_incidents WHERE deleted_at IS NULL AND first_fired_at IN period`。
  2. `applyEdgeScope` 加 `device_id IN scope.EdgeIDs`（若 scope 指定）。
  3. `applySeverityScope` 加 `severity IN allowed`（按 `severityRank >= scope.SeverityMin` 过滤）。
  4. `Order("first_fired_at DESC").Find`。
  5. 逐行转 `IncidentFact`，调 `durationMinutes` 算持续时长（resolved 取 `resolved_at`，未 resolved 取 `period.End`）。
- **错误处理**：返回 error 让上层 `Collect` 中断。

### `collectActions(ctx, p) bizreport.ActionsSummary`

- **职责**：agent mutation + safe action 计数。
- **流程**：
  1. `SELECT tool_name, decision, COUNT(*) FROM chat_mutating_proposals WHERE created_at IN period GROUP BY tool_name, decision`。
  2. 累加 `MutatingTotal`，`decision='approve'` 计入 `MutatingApproved`，按 tool 累加进 `byTool` map。
  3. 转 `[]ToolCount` 后调 `sortToolCounts` 降序插入排序。
  4. `COUNT(*) FROM audit_logs WHERE occurred_at IN period AND status='success'` → `SafeTotal`。
- **降级**：两次查询错误均吞掉，零值。

### `collectAlertCounts(ctx, p, scope) map[string]int`

- **职责**：按 severity 分组的 incident 计数。
- **流程**：`SELECT severity, COUNT(*) FROM alert_incidents WHERE deleted_at IS NULL AND first_fired_at IN period [AND device_id IN scope.EdgeIDs] GROUP BY severity`。
- **降级**：错误吞掉，返回空 map。

### `countIncidents(ctx, p, scope) int`

- 与 `collectAlertCounts` 类似但只返回总数；用于 `buildHero` 的 prev 周期对比 baseline。

### `buildHero(p, prev, scope, incidents, actions, fleet, res, prevIncidents) []bizreport.HeroStat`

- **职责**：构造 Hero 区统计卡片列表。
- **流程**：
  1. 始终包含 `devices`（监控设备数）。
  2. 若 `res.Available`：追加 `cpu_avg` / `mem_avg` / `disk_peak`（带 `%` 单位，调 `round1` 取一位小数）。
  3. 否则（Prom 无数据）：追加 `incidents`（带 prev deltaPct） / `actions`（agent 动作总数） / `online`（在线设备），让卡片不至于半空。
- **设计意图**：避免平静期卡片空荡，是产品体验的兜底。

### 辅助函数

- `resourceExprs(durStr) map[string]resourceExpr`：生成 cpu/mem/disk 三组 PromQL 表达式对。
- `applyEdgeScope(q, scope)`：按 `scope.EdgeIDs` 加 device 过滤。
- `applySeverityScope(q, scope)`：按 `scope.SeverityMin` 等级及以上过滤。
- `durationMinutes(start, resolved, periodEnd)`：算 incident 持续分钟；未 resolved 取 periodEnd；负值夹 0。
- `deltaPct(cur, prev)`：算环比百分比；`prev==0` 返回 nil（避免除零）。
- `round1(f)`：四舍五入到一位小数。
- `sortToolCounts(tc)`：插入排序，按 Count 降序。

## 5. 依赖关系

- **内部包**：
  - `github.com/ongridio/ongrid/internal/manager/biz/report`（bizreport）——接口与 DTO 类型来源（`FactsCollector` 接口、`ReportFacts` / `Period` / `Scope` / `IncidentFact` / `HeroStat` / `ToolCount` 等）。
  - `github.com/ongridio/ongrid/internal/pkg/promquery`——`PromQuerier` 实现、`InstantResult` 类型。
- **外部库**：`gorm.io/gorm`。
- **标准库**：`context` / `encoding/json` / `strconv` / `time`。
- **被调用方**：`biz/report` 的报告生成 usecase（在 LLM 渲染前调 Collect 取事实）。

## 6. 并发与资源管理

- **无共享状态**：`FactsCollector` 仅持有不可变的 `db` 与 `prom` 句柄，可被多 goroutine 并发调用。
- **ctx 透传**：所有方法首参 `context.Context`，符合 gospec 红线；Prom 与 SQL 查询都受 ctx 取消控制。
- **无锁、无 channel、无缓存**：每次 Collect 都是新鲜查询，不缓存结果（报告生成频率低，缓存无必要）。
- **无资源释放**：不开游标、不持连接，连接池由 GORM/Prom client 管理。
- **串行收集**：Collect 内部各收集器**串行**调用（非并发），简化错误处理与日志；如需优化可改为并发 + errgroup，但当前性能足够。

## 7. 设计模式与亮点

- **窄接口注入**：`PromQuerier` 只暴露 `Query` 一个方法，便于 mock 测试，也防止 FactsCollector 滥用 Prom client 的其他能力。
- **优雅降级三层**：
  1. Prom 不可用 → `Resource.Available=false`，其余段仍渲染。
  2. SQL 子查询失败 → 该段零值，不影响其他段。
  3. 仅 `collectIncidents` 失败中断（incidents 是报告核心段，缺失会让报告失去意义）。
- **Hero 区产品兜底**：`buildHero` 在 Prom 无数据时退回 incidents/actions/online，是产品体验设计而非纯技术决策。
- **PromQL 复用**：资源表达式直接复用 Monitor 页同款 `node_*` 指标，保证 series 存在，降低运维风险。
- **subquery 时长夹紧**：`dur > 30d → 7d`，避免长自定义区间让 Prom 子查询爆炸。
- **NaN 守卫 + 负值夹 0**：`parseQuotedFloat` 双重防护，保证利用率指标合理。
- **审计白名单过滤**：`changeActions` 显式列出值得呈现的动作，避免 read/login 噪声淹没变更日志。
- **`_ = ...` 显式吞错**：符合 gospec「确实想丢弃错误必须就近注释」——每个吞错的查询都有「降级」语义注释。
- **scope 双重过滤**：edge scope（device_id）+ severity scope，让报告能按边缘节点子集与严重度门槛裁剪。

## 8. 注意事项

- **唯一会中断 Collect 的是 `collectIncidents`**：如果 alert_incidents 表查询失败，整份报告不会生成。若想进一步降级，可改成返回空切片 + log。
- **`changeActions` 白名单是硬编码**：新增审计动作类型时容易遗漏，需同步更新此切片。
- **角色 bit 映射硬编码**：`roleNames := map[uint8]string{1: "server", ...}` 与 model 层的角色定义必须一致；改 model 时别忘了这里。
- **`durStr` 用 `hours` 单位**：`strconv.FormatInt(int64(dur.Hours()), 10) + "h"`；若周期短于 1 小时会被截断为 0h，需注意。
- **`deltaPct` 在 `prev==0` 时返回 nil**：前端需处理 nil delta（通常显示 `—` 而非 0%）。
- **`collectChanges` LIMIT 20**：报告里只展示最近 20 条变更；若需完整列表要另开接口。
- **时区**：所有时间比较用 UTC（`first_fired_at` / `created_at` 等列默认 UTC 存储），前端展示按用户时区转换。
- **测试 mock**：`PromQuerier` 是窄接口，单测可轻松 mock；SQL 部分用 SQLite in-memory 即可测。
- **并发优化空间**：当前 Collect 串行执行 8+ 查询，单报告生成可能耗时数秒；如成瓶颈可改 errgroup 并发，但需注意 Prom 速率限制。
- **不写数据**：本文件纯只读，符合「事实收集器」语义；任何写副作用应放在报告生成完成后由 biz 层触发。
