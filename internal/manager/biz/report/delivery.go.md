# delivery.go

## 1. 概述

`delivery.go` 实现 report 包的 IM 投递抽象 —— 把 ready 报告 fan-out 到通知通道。`Deliverer` 是 seam（在 main.go over notify router + channel store 实现），让 `biz/report` 不依赖 notify/alert 包。

锁定决策：**只投递 ready 报告**，failed 报告永不推送（无半成品发送）。`Deliverer.Deliver` 必须有 timeout，inline 跑在 `MarkReady` 之后。

文件还含 `DeliverySummary`（channel-agnostic payload）、`DeliveryRecord`（每通道结果，持久化到 `delivery_json`）、`MarkdownSummary`（渲染 markdown body）。

## 2. 包信息

- 包名：`report`
- 路径：`internal/manager/biz/report`

## 3. 关键类型与接口

### Deliverer 接口

```go
type Deliverer interface {
    Deliver(ctx, summary DeliverySummary, channelIDs []uint64) []DeliveryRecord
}
```

注释：不阻塞 indefinitely；caller inline 跑在 MarkReady 后；errors 捕获在 records 而非返回。nil Deliverer = in-app only（报告仍可看，不推送）。

### DeliverySummary

```go
type DeliverySummary struct {
    Title    string
    Headline string
    Hero     []HeroStat
    DeepLink string  // 绝对或相对；impl 从 PublicURL 解析
    ReportID string
}
```

channel-agnostic payload。具体 Deliverer 渲染成每通道原生格式（v1 markdown 文本；Feishu interactive card 是未来增强）。

### DeliveryRecord

```go
type DeliveryRecord struct {
    ChannelID    uint64
    ChannelType  string
    Status       string  // "sent" | "failed"
    SentAt       time.Time
    Error        string
    FallbackUsed bool
}
```

持久化到 `reports.delivery_json`，detail 页面展示。

## 4. 关键函数与流程

### MarkdownSummary

```go
func (s DeliverySummary) MarkdownSummary() string
```

渲染 channel-agnostic markdown body：
1. `**title**`
2. Hero 行：每个 hero `label value unit` + delta 箭头（`→` / `↓` / `↑`），用 ` · ` 连接
3. headline（`flattenEntities` 拍平 token）
4. `[查看完整报告 →](deeplink)`

注释：不渲染 markdown 的通道仍能显示可读纯文本。

### deliveryFor

```go
func deliveryFor(rpt *model.Report, deepLink string) DeliverySummary
```

从 ready 报告构造 summary。流程：
1. 默认 `Headline = rpt.SummaryText`
2. 若 `rpt.ContentJSON` 可解析：取 `c.Hero`，若 `c.Narrative.Headline` 非空覆盖 Headline
3. 注释：content 解析失败回退到 SummaryText，永不因 content quirk 阻断投递

### recordDelivery

```go
func recordDelivery(rpt *model.Report, records []DeliveryRecord)
```

`json.Marshal(records)` → `rpt.DeliveryJSON`。空 records 直接返回（不覆盖已有）。

### abs

```go
func abs(f float64) float64
```

辅助：float 绝对值，用于 delta 箭头显示百分比。

## 5. 依赖关系

### 外部包

- `context` / `encoding/json` / `fmt` / `strings` / `time`

### 内部类型（同包其它文件）

- `HeroStat`（在 `content.go`）
- `flattenEntities` / `formatNum`（在 `content.go`）
- `ParseContent`（在 `content.go`）

### 被谁调用

- `generator.go` 的 `deliver` 调 `deliveryFor` + `Deliverer.Deliver` + `recordDelivery`
- 具体 `Deliverer` 实现在 `cmd/ongrid` over notify router + channel store

## 6. 并发与资源管理

- 无锁、无 goroutine，纯数据类型与函数
- `Deliverer.Deliver` 由实现方保证并发安全与 timeout

## 7. 设计模式与亮点

### Seam 隔离 notify/alert

`Deliverer` 是 seam 接口，在 main.go 实现 over notify router + channel store。让 `biz/report` 不 import notify/alert，符合 gospec monorepo 边界红线。

### 只投递 ready 报告

注释明确锁定决策：failed 报告永不推送。这避免半成品发送（如 LLM 失败但 hero 已写一半）。

### Channel-agnostic payload

`DeliverySummary` 是通道无关结构，具体 Deliverer 渲染成每通道原生格式。这让新增通道类型不需改 biz 层。

### Content 解析失败回退

`deliveryFor` 若 `ContentJSON` 解析失败，回退到 `SummaryText`。注释："never blocks delivery on a content quirk"。

### MarkdownSummary 纯文本可读

注释：不渲染 markdown 的通道仍能显示可读纯文本。这是 markdown fallback 的设计目标。

### Delta 箭头中性化

`MarkdownSummary` 的 delta 箭头：`*h.DeltaPct < 0` → `↓`，`> 0` → `↑`，`== 0` → `→`。用 `abs` 显示百分比绝对值，箭头方向单独表达。

## 8. 注意事项

- **`Deliverer.Deliver` 必须有 timeout**：caller inline 跑，无 timeout 会阻塞 generator。实现方负责
- **`DeliveryRecord.Status` 字面量**：`"sent"` / `"failed"` 是稳定 wire shape。SPA 按此渲染
- **`FallbackUsed` 字段**：标记是否用了 fallback 通道（如 IM 失败回退邮件）。当前未用，预留
- **`recordDelivery` 空 records 不覆盖**：若 Deliverer 返回空切片，`rpt.DeliveryJSON` 保留原值。这是有意的 —— 避免空投递覆盖之前成功记录
- **`deliveryFor` 不传 ReportID**：`DeliverySummary.ReportID` 从 `rpt.ID` 取，调用方无需传
- **`MarkdownSummary` delta 用 `%.0f%%`**：百分比无小数。若需小数精度改 format string
- **`Deliverer` nil = in-app only**：generator 检查 nil，不投递。报告仍可看，只是不推送
