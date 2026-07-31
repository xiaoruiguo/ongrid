# report/dto.go 技术实现文档

## 1. 概述

`dto.go` 是 report 子域的 DTO（Data Transfer Object）层，定义 SPA ↔ handler 之间的 wire shape，以及 model ↔ DTO 的转换函数。涵盖 report（报告产物）和 schedule（定时任务）两个实体的列表/详情视图，外加 scheduleReq 请求 DTO 及其 `toModel` / `applyTo` 转换方法。同时包含与 `device/http.go` 镜像的 `writeJSON` / `writeErr` / `errCode` 响应助手。

## 2. 包信息

- **包名**：`report`（与 `http.go` / `task.go` 同包）
- **导入路径**：`github.com/ongridio/ongrid/internal/manager/server/report`
- **文件定位**：DTO 定义 + 转换函数 + 响应助手（无 HTTP handler 逻辑）

## 3. 关键类型与接口

### reportListItem —— 列表视图

```go
type reportListItem struct {
    ID          string     `json:"id"`
    Title       string     `json:"title"`
    Kind        string     `json:"kind"`
    Status      string     `json:"status"`
    Summary     string     `json:"summary"`
    PeriodStart time.Time  `json:"period_start"`
    PeriodEnd   time.Time  `json:"period_end"`
    GeneratedAt *time.Time `json:"generated_at,omitempty"`
    CreatedAt   time.Time  `json:"created_at"`
    ScheduleID  *uint64    `json:"schedule_id,omitempty"`
    TaskID      string     `json:"task_id,omitempty"`
    RunID       string     `json:"run_id,omitempty"`
}
```

**关键设计**：
- `ScheduleID` 仅定时生成的报告有，手工生成 nil
- `TaskID`（HLD-022）是 owning-task 反向引用（如 `report-schedule:42`），定时和 run-now 都有，是"任务产物"的规范 key
- `RunID` 是触发/run 反向引用，同触发的产物共享，便于按触发分组

### reportDetail —— 详情视图

```go
type reportDetail struct {
    reportListItem
    Content    json.RawMessage `json:"content"`
    ContentMD  string          `json:"content_md"`
    Timezone   string          `json:"timezone"`
    ErrorMsg   string          `json:"error_msg,omitempty"`
    ShareToken *string         `json:"share_token,omitempty"`
    Delivery   json.RawMessage `json:"delivery,omitempty"`
}
```

**关键设计**：
- `Content` 是 `json.RawMessage`——直接 emit 原始 JSON，SPA 直接渲染结构化卡片，无需二次解析
- `ContentMD` 是 markdown 文本，作为 fallback
- `Delivery` 也是 `json.RawMessage`，投递状态原始 JSON 透传

### scheduleView —— schedule 视图

```go
type scheduleView struct {
    ID             uint64     `json:"id"`
    Name           string     `json:"name"`
    Description    string     `json:"description"`
    Kind           string     `json:"kind"`
    CronSpec       string     `json:"cron_spec"`
    Timezone       string     `json:"timezone"`
    ScopeJSON      string     `json:"scope_json"`
    ChannelIDs     []uint64   `json:"channel_ids"`
    InAppVisible   bool       `json:"in_app_visible"`
    AgentPersona   string     `json:"agent_persona"`
    PromptOverride string     `json:"prompt_override,omitempty"`
    Enabled        bool       `json:"enabled"`
    NextFireAt     *time.Time `json:"next_fire_at,omitempty"`
    LastFireAt     *time.Time `json:"last_fire_at,omitempty"`
    LastReportID   *string    `json:"last_report_id,omitempty"`
    CreatedAt      time.Time  `json:"created_at"`
}
```

`ChannelIDs` 从存储的 `ChannelIDsJSON` 字符串解码为 `[]uint64` 数组，便于前端直接用。

### scheduleReq —— 请求 DTO（定义在 http.go）

```go
type scheduleReq struct {
    Name           string   `json:"name"`
    Description    string   `json:"description"`
    Kind           string   `json:"kind"`
    CronSpec       string   `json:"cron_spec"`
    Timezone       string   `json:"timezone"`
    ScopeJSON      string   `json:"scope_json"`
    ChannelIDs     []uint64 `json:"channel_ids"`
    InAppVisible   *bool    `json:"in_app_visible"`
    PromptOverride string   `json:"prompt_override"`
}
```

`InAppVisible *bool` 用指针区分"未传"与"显式 false"。

## 4. 关键函数与流程

### toReportListItem / toReportList / toReportDetail

```go
func toReportListItem(r *model.Report) reportListItem
func toReportList(rows []*model.Report) []reportListItem
func toReportDetail(r *model.Report) reportDetail
```

model → DTO 转换。`toReportDetail` 把 `ContentJSON` / `DeliveryJSON` 字符串转 `json.RawMessage`——空字符串时不设置（emit `null` 而非 `""`）。

### toScheduleView / toScheduleList

```go
func toScheduleView(s *model.ReportSchedule) scheduleView
func toScheduleList(rows []*model.ReportSchedule) []scheduleView
```

`PromptOverride` 是 `*string`，非 nil 时解引用；`ChannelIDsJSON` 解码为数组。

### scheduleReq.toModel —— 创建

```go
func (req scheduleReq) toModel(ownerID uint64) *model.ReportSchedule
```

构造新 schedule：
- `CreatedBy = ownerID`
- `Timezone` 默认 "UTC"
- `ScopeJSON` 默认 "{}"
- `ChannelIDsJSON` 用 `encodeChannelIDs` 编码
- `Enabled = true`（新建默认启用）
- `InAppVisible = true`（默认 true，req 显式传则覆盖）
- `PromptOverride` 非空时取地址赋值

### scheduleReq.applyTo —— 更新

```go
func (req scheduleReq) applyTo(s *model.ReportSchedule)
```

部分更新语义（PATCH 风格）：
- `Name` / `Kind` / `Timezone` / `ScopeJSON` 非空才覆盖
- `Description` / `CronSpec` / `ChannelIDsJSON` / `InAppVisible` / `PromptOverride` 总是覆盖（可清空）
- `PromptOverride` 空字符串时显式置 nil

### helpers

- `decodeChannelIDs(raw)` —— JSON 字符串 → `[]uint64`，失败返空数组（宽容解码）
- `encodeChannelIDs(ids)` —— `[]uint64` → JSON 字符串，空数组返 `"[]"`
- `firstNonEmpty(vals...)` —— 取第一个非空字符串

### 响应助手

```go
func writeJSON(w http.ResponseWriter, code int, body any)
func writeErr(w http.ResponseWriter, err error)
func errCode(err error) string
```

`writeErr` 用 `errs.HTTPStatus(err)` 取 status，`errCode` 返 slug（含 `not-found` / `not-wired-yet` 等）。

## 5. 依赖关系

**外部**：
- `encoding/json`、`net/http`、`time`、`errors`

**内部**：
- `internal/manager/model/report`（Report / ReportSchedule / Task）
- `internal/pkg/errs`

## 6. 并发与资源管理

- **无共享可变状态**：本文件全是纯函数 + 类型定义，无 handler 状态
- **`applyTo` 就地修改**：`req.applyTo(s)` 直接 mutate 传入的 `*model.ReportSchedule`，调用方需确保独占
- **无 goroutine**：纯转换

## 7. 设计模式与亮点

1. **`json.RawMessage` 透传结构化内容**：`Content` / `Delivery` 直接 emit 原始 JSON，SPA 无需二次解析字符串
2. **列表 vs 详情分离**：`reportListItem` 紧凑（无 content），`reportDetail` 嵌入 list + 完整内容，避免列表传输冗余
3. **`TaskID` 作为规范 key**（HLD-022）：定时和 run-now 都有，是"任务产物"分组的规范字段
4. **`scheduleReq.applyTo` PATCH 语义**：部分字段非空才覆盖，部分字段总是覆盖（可清空），符合前端表单"未改的字段不传"约定
5. **`InAppVisible *bool` 区分未传**：指针类型让"未传"与"显式 false"可区分
6. **`ChannelIDs` 双向编解码**：存储 JSON 字符串，DTO 数组，转换函数集中管理
7. **`firstNonEmpty` 默认值工具**：用于 timezone/scope 默认值，避免散落的 if-else
8. **响应助手与 device/http.go 镜像**：跨子域统一响应 shape
9. **`errCode` 含 `not-wired-yet`**：业务感知 slug，对应 503

## 8. 注意事项

1. **`applyTo` 就地 mutate**：调用方需确保传入的 schedule 独占；并发场景需上层加锁
2. **`PromptOverride` 空字符串置 nil**：`applyTo` 中 `else { s.PromptOverride = nil }`——前端"清空"需显式传空字符串，未传字段不会触发清空
3. **`decodeChannelIDs` 宽容解码**：解析失败返空数组而非报错，避免损坏的存储数据导致整个 schedule 不可读
4. **`encodeChannelIDs` 失败返 `"[]"`**：marshal 失败理论上不会发生（uint64 数组），但兜底防 panic
5. **`reportDetail.Content` 空时为 nil**：`if r.ContentJSON != "" { d.Content = ... }`——空字符串时 `d.Content` 是 nil `json.RawMessage`，emit 为 `null`
6. **`scheduleReq.InAppVisible *bool`**：JSON 反序列化时未传字段为 nil，显式 false 为 `&false`；前端需注意区分
7. **`errCode` 无 `conflict` slug**：与 marketplace/mcp 不同，本子域不返 409；如需冲突检测需扩
8. **`writeErr` 用 `errs.HTTPStatus`**：与 monitor/http.go 同模式，集中映射
9. **`TaskID` / `RunID` 是 HLD-022 Phase 2 新增**：旧报告可能没有这些字段，前端需 fallback
10. **`ScheduleID *uint64` omitempty**：手工生成报告无 schedule，emit 时省略字段
