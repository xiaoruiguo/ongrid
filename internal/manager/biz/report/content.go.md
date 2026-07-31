# content.go

## 1. 概述

`content.go` 定义 report 包的结构化报告体 `Content`（HLD-014 §ContentJSON）—— reporter agent 产生、SPA 渲染富视图的数据源。`ContentMD` 是从 `Content` 渲染的 markdown fallback，用于导出 / IM / 搜索。

**反幻觉契约**：数字字段（Hero 值/delta/sparkline、per-tool action counts）由 `FactsCollector` 纯 SQL 计算，generator（PR-2b）在 LLM 返回后**覆盖** `Content.Hero` 和 `Content.Actions` 为收集到的事实。LLM 只拥有 `Narrative` / `KeyIncidents` 排序评论 / `Advice`。模型就算篡改数字也泄漏不到报告里。

## 2. 包信息

- 包名：`report`
- 路径：`internal/manager/biz/report`

## 3. 关键类型与接口

### Content（结构化报告体）

```go
type Content struct {
    Version   string
    Hero      []HeroStat
    Narrative Narrative
    Resource     ResourceFacts      // facts 注入
    Fleet        FleetFacts         // facts 注入
    KeyIncidents []KeyIncident
    Actions      ActionsSummary     // facts 覆盖
    Changes      []ChangeFact
    Assets       AssetFacts
    Usage        UsageFacts
    Advice       []Advice
    Metadata     ContentMeta
}
```

### HeroStat（大数字卡片）

```go
type HeroStat struct {
    Key       string
    Label     string
    Value     float64
    Unit      string    // "min", "%" 等；空 = 裸计数
    DeltaPct  *float64  // 周期同比变化；nil = 无可比（渲染为 "new"）
    Sparkline []int
}
```

`Value` / `DeltaPct` / `Sparkline` 全部 SQL 计算，永不 LLM。

### Narrative（LLM 散文）

```go
type Narrative struct {
    Headline   string
    Paragraphs []Paragraph
}
type Paragraph struct {
    Text     string
    Entities []EntityRef  // SPA 渲染为可点击 chip
}
type EntityRef struct {
    Key  string  // "edge:7" | "incident:1234"
    Name string  // display name
}
```

`Paragraph.Text` 可嵌入实体 token `{{entity:kind:id|name}}`，SPA 渲染为 chip；markdown 导出时 `flattenEntities` 把 token 拍平为 display name。

### KeyIncident

```go
type KeyIncident struct {
    ID               uint64
    Title            string
    Severity         string
    DurationMin      int
    Status           string
    RootCauseSnippet string  // LLM 可从 RCA report 设
}
```

ids/durations/status 是 SQL 真；LLM 可设 `RootCauseSnippet`。

### ActionsSummary（agent 透明度面板）

```go
type ActionsSummary struct {
    MutatingTotal    int
    MutatingApproved int
    SafeTotal        int
    ByTool           []ToolCount
}
type ToolCount struct {
    Tool  string
    Count int
}
```

全部 SQL 真。

### Advice

```go
type Advice struct {
    Text string  // LLM 创作，可嵌实体 token
}
```

### ContentMeta

```go
type ContentMeta struct {
    PeriodStart string
    PeriodEnd   string
    DataSources []string
}
```

### ContentVersion

```go
const ContentVersion = "1"
```

schema 版本戳，让 SPA 在 schema 演进时分支。

## 4. 关键函数与流程

### ParseContent

```go
func ParseContent(raw string) (*Content, error)
```

`json.Unmarshal` + `Validate()`。generator（PR-2b）调它检查 LLM 输出再持久化。

### Validate

最小 spine 校验：
- `Narrative.Headline` 非空
- 每个 `Hero[i]` 的 `Key` + `Label` 非空

注释：对可选 section（KeyIncidents / Advice）宽容 —— 平静报告可能空。

### MustJSON

```go
func (c *Content) MustJSON() string
```

`json.Marshal`，error 才 panic（注释：unmarshalable 类型不可能出现在这些具体 struct）。

### RenderMarkdown

```go
func (c *Content) RenderMarkdown(title, locale string) string
```

从结构化内容渲染 markdown fallback。`locale == "en"` 用英文标签，否则中文。流程：
1. `# title`
2. `## headline` + 段落（`flattenEntities` 拍平 token）
3. `## 资源使用` （若 `Resource.Available`）：CPU/内存/磁盘的 avg/peak
4. `## 监控覆盖`：设备数 / 在线数 / 角色
5. `## 知识资产新增`：助理/技能/仓库数
6. `## 使用情况`：会话/token 数
7. `## 告警与处置`（若非空）：每个 incident 一行 + agent 动作统计
8. `## 变更记录`（若非空）：每条变更一行
9. `## 建议`（若非空）：每条 advice 一行

### formatNum

```go
func formatNum(v float64) string
```

整数打印无 `.0`，否则 1 位小数。

### flattenEntities

```go
func flattenEntities(s string) string
```

循环把 `{{entity:kind:id|name}}` 替换为 `name`。注释：markdown fallback 专用（chip 是 SPA-only）。畸形 token（无 `}}`）保留原样。

## 5. 依赖关系

### 外部包

- `encoding/json` / `fmt` / `strings`

### 内部类型（同包其它文件定义）

- `ResourceFacts` / `FleetFacts` / `ChangeFact` / `AssetFacts` / `UsageFacts`（在 `facts.go`）

### 被谁调用

- `generator.go` 调 `ParseContent` 校验 LLM 输出 + `MustJSON` 持久化
- HTTP handler 调 `RenderMarkdown` 生成导出/IM fallback
- SPA 直接消费 `Content` JSON

## 6. 并发与资源管理

- 无锁、无 goroutine，纯数据类型与函数
- `RenderMarkdown` 用 `strings.Builder` 高效拼接
- `flattenEntities` 循环改写原串（不可变 string 重新赋值）

## 7. 设计模式与亮点

### 反幻觉契约

注释明确：`Hero` / `Actions` 由 generator 在 LLM 返回后用 facts 覆盖。LLM 只拥有 `Narrative` / `KeyIncidents` 排序评论 / `Advice`。这是产品安全设计 —— 防模型篡改数字泄漏到报告。

### 实体 token 双 surface

`Paragraph.Text` 嵌 `{{entity:kind:id|name}}`：SPA 渲染为可点击 chip；markdown 导出 `flattenEntities` 拍平为 display name。一份文本两种渲染。

### locale 感知 markdown

`RenderMarkdown(title, locale)` 按 locale 选标签。`mtr(zh, eng)` helper 简化中英切换。注释中所有标签都双语列出。

### 宽容 Validate

`Validate` 只校验 spine（headline + hero key/label），对可选 section 宽容。这让"平静报告"（无 incident / 无 advice）也能通过校验。

### ContentVersion schema 版本

`ContentVersion = "1"` 戳入新生成报告。SPA 可按 version 分支处理 schema 演进。

### MustJSON panic 仅编程错误

`MustJSON` 注释：unmarshalable 类型不可能出现在这些具体 struct，panic 仅编程错误。这是 Go idiomatic 的 Must 模式。

### formatNum 整数优先

`formatNum` 整数打印无 `.0`，否则 1 位小数。让 hero 卡片显示 "5" 而非 "5.0"。

## 8. 注意事项

- **反幻觉契约依赖 generator 强制覆盖**：`Content.Hero` / `Actions` 必须在 generator 内被 facts 覆盖，不能直接信任 LLM 输出。改 generator 时不能漏这一步
- **`flattenEntities` 畸形 token 保留**：无 `}}` 的 token 原样保留。LLM 若生成畸形 token 会出现在 markdown 里
- **`RenderMarkdown` 双语硬编码**：`mtr(zh, eng)` 调用散布全文。新增 section 要同步加双语
- **`Validate` 不校验数字字段**：Hero 的 `Value` / `DeltaPct` 不校验。LLM 篡改的数字会被 facts 覆盖，但若 facts 收集失败，篡改值会泄漏
- **`ContentVersion` 是字符串**：`"1"`。未来 schema 变化应升 `"2"` 并让 SPA 分支
- **`Paragraph.Entities` 是冗余**：`Text` 已含 token，`Entities` 列表为 SPA 渲染便利。两者可能不一致 —— SPA 应以 `Text` 为准解析
- **`KeyIncident.RootCauseSnippet` 是 LLM 可设**：唯一 LLM 可改的非 narrative 字段。generator 应限制其长度
- **`Changes` 字段 omitempty**：`json:"changes,omitempty"`。空时 JSON 中不出现，SPA 要兼容
