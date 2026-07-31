# facts.go

## 1. 概述

`facts.go` 定义 report 包的事实收集契约 —— `ReportFacts` 是 reporter agent 的确定性预计算输入（HLD-014 §数据收集）。agent 只叙述这些事实，**永不计算数字**。

2026-06-06 review 重设计：报告以 fleet 资源趋势 + 监控覆盖为主而非 incident-only，让平静期也有实质内容。每个数字来自 Prometheus（period avg/peak）或 SQL 查询；agent 唯一自由度是 prose（narrative + advice）。

文件还含 `Scope`（解析 `ScopeJSON`）+ `FactsCollector` 接口（实现在 `data/report/store`）。

## 2. 包信息

- 包名：`report`
- 路径：`internal/manager/biz/report`

## 3. 关键类型与接口

### ReportFacts

```go
type ReportFacts struct {
    Period     Period `json:"-"`   // 不进 LLM prompt JSON
    PrevPeriod Period `json:"-"`

    Hero       []HeroStat       `json:"hero"`        // 预计算大数字集
    Resource   ResourceFacts    `json:"resource"`    // fleet 资源趋势
    Fleet      FleetFacts       `json:"fleet"`       // 监控覆盖快照
    Incidents  []IncidentFact   `json:"incidents"`   // 次要 section
    Actions    ActionsSummary   `json:"actions"`     // agent 透明度
    AlertCounts map[string]int  `json:"alert_counts"`
    Changes    []ChangeFact     `json:"changes"`     // 产品侧变更日志
    Assets     AssetFacts       `json:"assets"`      // 新增资产
    Usage      UsageFacts       `json:"usage"`       // 平台使用信号
}
```

注释：Hero 重设计为非 incident 信号为主（devices monitored / fleet CPU/mem avg / disk peak），让卡片在无 incident 时也有意义。

### ResourceFacts

```go
type ResourceFacts struct {
    Available bool    // Prom 不可达或无数据时 false，渲染 "no data" 而非假零
    CPUAvg, CPUPeak   float64
    MemAvg, MemPeak   float64
    DiskAvg, DiskPeak float64
}
```

注释：fleet-aggregate（非 per-device，per-device 读起来是噪声，review 决策）。

### FleetFacts

```go
type FleetFacts struct {
    Total  int
    Online int
    Roles  map[string]int  // role name → device count
}
```

### IncidentFact

```go
type IncidentFact struct {
    ID          uint64
    Title       string
    Severity    string
    Status      string
    DeviceID    uint64
    DurationMin int  // resolved_at - first_fired_at（或 now - first_fired_at 若仍 open）
}
```

### ChangeFact

```go
type ChangeFact struct {
    At           time.Time
    Action       string  // e.g. rule_update
    ResourceType string  // e.g. alert_rule
    ResourceName string
    Actor        string  // user email
}
```

period 内产品侧变更（audit_logs），"谁改了什么"。

### AssetFacts / UsageFacts

```go
type AssetFacts struct {
    NewAgents, NewSkills, NewRepos int
}
type UsageFacts struct {
    Sessions         int
    PromptTokens     int64
    CompletionTokens int64
}
```

### Scope

```go
type Scope struct {
    FleetTags   []string
    EdgeIDs     []uint64
    SeverityMin string
}
```

v1 只 honor `EdgeIDs` + `SeverityMin`；`FleetTags` 解析但 no-op 直到 G.2.6 edge tags。

### FactsCollector 接口

```go
type FactsCollector interface {
    Collect(ctx, period, prev Period, scope Scope) (*ReportFacts, error)
}
```

实现在 `data/report/store.FactsCollector`。Period 与 prev 由 Usecase 预计算（`PeriodFor`）。

## 4. 关键函数与流程

### ParseScope

```go
func ParseScope(raw string) Scope
```

读 `ScopeJSON`。空 / `"{}"` / 无效 → 零值 Scope（全覆盖）而非 error。注释：`_ = json.Unmarshal` 容错。

## 5. 依赖关系

### 外部包

- `context` / `encoding/json` / `time`

### 内部类型（同包）

- `Period`（在 `period.go`）
- `HeroStat` / `ActionsSummary`（在 `content.go`）

### 被谁实现

- `FactsCollector`：`data/report/store.FactsCollector`

### 被谁调用

- `generator.go` 的 `generate` 调 `facts.Collect(ctx, period, prev, scope)` 取事实
- `generator.go` 调 `ParseScope(rpt.ScopeJSON)`
- `usecase.go` 调 `PeriodFor` 算 period + prev 传给 collector

## 6. 并发与资源管理

- 无锁、无 goroutine，纯数据类型与接口声明
- `FactsCollector.Collect` 由实现方保证并发安全

## 7. 设计模式与亮点

### 反幻觉契约的根

`ReportFacts` 注释明确：agent 只叙述这些事实，永不计算数字。配合 `generator.go` 的 post-LLM 覆盖（`content.Hero = facts.Hero` 等），双重防御模型篡改。

### Period/PrevPeriod 不进 JSON

`Period` 与 `PrevPeriod` 字段 `json:"-"`，不进 LLM prompt JSON。它们是收集器的输入参数，不应让 LLM 看到"上一周期"原始数据。

### Available flag 防 fake zero

`ResourceFacts.Available = false` 时渲染 "no data" 而非假零。注释：Prom 不可达或数据超 retention 时明确表达"无数据"。

### Fleet-aggregate 非 per-device

注释：per-device 读起来是噪声（review 决策）。fleet 聚合让趋势更清晰。

### Hero 重设计非 incident 为主

注释：Hero 重设计为 devices monitored / fleet CPU/mem avg / disk peak，让平静期卡片也有意义。这是 2026-06-06 review 的决策。

### Scope 容错

`ParseScope` 对坏 JSON 返回零值 Scope（全覆盖）而非 error。注释：让坏行不阻断报告生成。

### FleetTags 解析但 no-op

`Scope.FleetTags` 解析出来但 v1 不用。注释：等 G.2.6 edge tags 落地。这是前向兼容设计。

## 8. 注意事项

- **`ReportFacts` 是 LLM 输入**：所有字段会进 LLM prompt JSON（除 Period/PrevPeriod）。敏感数据（如 user email in `ChangeFact.Actor`）会暴露给 LLM —— 这是报告必需信息
- **`Available` flag 必须正确**：Prom 不可达时 `Available = false`，否则 fake zero 会误导
- **`DurationMin` 仍 open 用 now**：`IncidentFact.DurationMin` 若 incident 仍 open，用 `now - first_fired_at`。这是动态值，报告生成时刻不同结果不同
- **`FactsCollector.Collect` 应纯 SQL/Prom**：不调 LLM、不做复杂逻辑。确定性输入是反幻觉契约的前提
- **`Scope.EdgeIDs` v1 honor**：`SeverityMin` 也 honor。`FleetTags` no-op
- **`ParseScope` 容错**：坏 JSON 返回零值 Scope。调试时若 scope 行为异常，先查 `ScopeJSON` 是否被写坏
- **`AssetFacts` / `UsageFacts` 是新增资产与使用**：period 内新增的 agent/skill/repo 数 + chat session/token 数。让报告体现"我们建设了什么" + "用了多少"
