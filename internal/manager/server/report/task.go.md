# report/task.go 技术实现文档

## 1. 概述

`task.go` 实现 HLD-022 Phase 2 的统一任务（任务）surface。把两类任务源 union 成一个列表：
1. **recurring**（定时任务）—— 来自 `report_schedules` 表，ID = `report-schedule:<n>`
2. **oneoff**（一次性任务）—— 来自 `tasks` 表，ID = `oneoff:<uuid>`

提供 list / create / get / rerun / delete 5 个端点。recurring 的 CRUD 仍在 `/v1/report-schedules`（http.go），本文件只处理 oneoff 创建/重跑/删除 + 统一列表/详情。

## 2. 包信息

- **包名**：`report`（与 `http.go` / `dto.go` 同包）
- **导入路径**：`github.com/ongridio/ongrid/internal/manager/server/report`
- **路由**：在 `http.go.Register` 中挂载
  - `GET /v1/tasks` —— 统一列表
  - `POST /v1/tasks/oneoff` —— 创建 oneoff + 立即生成
  - `GET /v1/tasks/{id}` —— 统一详情
  - `POST /v1/tasks/{id}/run` —— 重跑 oneoff
  - `DELETE /v1/tasks/{id}` —— 删除 oneoff
- **文件定位**：HTTP handler + DTO + 任务 ID 解析

## 3. 关键类型与接口

### taskDTO —— 统一任务形状

```go
type taskDTO struct {
    ID         string     `json:"id"`                   // "report-schedule:<n>" | "oneoff:<uuid>"
    Kind       string     `json:"kind"`                 // "recurring_report" | "oneoff"
    Title      string     `json:"title"`
    ReportKind string     `json:"report_kind"`          // daily/weekly/monthly
    Trigger    string     `json:"trigger"`              // "cron · tz" | "oneoff"
    Enabled    bool       `json:"enabled"`              // recurring on/off; oneoff always true
    Status     string     `json:"status"`               // oneoff status; recurring derived
    NextFireAt *time.Time `json:"next_fire_at,omitempty"`
    ScheduleID *uint64    `json:"schedule_id,omitempty"` // recurring: numeric id for CRUD
    CreatedAt  time.Time  `json:"created_at"`
}
```

**关键设计**：
- `ID` 带 prefix 区分来源（`report-schedule:` / `oneoff:`）
- `Trigger` 是人类可读字符串（`cron · tz` 或 `oneoff`），前端直接展示
- `ScheduleID` 仅 recurring 有，前端用此 ID 跳转到 `/v1/report-schedules/{id}` CRUD
- `Enabled` oneoff 永远 true（一次性任务无启用/禁用概念）

## 4. 关键函数与流程

### taskIDParam —— URL 解析 + percent-decode

```go
func taskIDParam(r *http.Request) string
```

**关键**：统一任务 ID 带冒号（`oneoff:<uuid>` / `report-schedule:<n>`），客户端 URL 编码 `%3A`，chi 留 encoded，本函数 `url.PathUnescape` 解码后再 prefix 匹配。

### taskFromSchedule / taskFromOneoff —— 转换函数

```go
func taskFromSchedule(s *model.ReportSchedule) taskDTO
func taskFromOneoff(t *model.Task) taskDTO
```

- `taskFromSchedule`：`ID = "report-schedule:" + ID`，`Trigger = CronSpec + " · " + Timezone`，`Status` 从 `Enabled` 映射（active/disabled）
- `taskFromOneoff`：`ID = model.OneoffTaskRef(t.ID)`（即 `oneoff:<uuid>`），`Trigger = "oneoff"`，`Status` 用 task 自身 status

### listTasks —— 统一列表

```go
func (h *Handler) listTasks(w http.ResponseWriter, r *http.Request)
```

1. `tenantctx.From` 取 caller
2. `uc.ListSchedules(ctx, userID, role != viewer)` 取 recurring
3. `uc.ListOneoffTasks(ctx)` 取 oneoff
4. 合并 + 按 `CreatedAt` 倒序排序
5. 200 + `{tasks}`

### createOneoffTask —— 创建 + 立即生成

```go
func (h *Handler) createOneoffTask(w http.ResponseWriter, r *http.Request)
```

1. `tenantctx.From` 取 caller
2. **best-effort decode**：`_ = json.Decode(MaxBytesReader(32 KiB))` —— 空 body 也可（用默认值）
3. 默认 `Kind = "weekly"`、`ScopeJSON = "{}"`
4. `uc.CreateOneoffTaskAndRun(ctx, userID, title, kind, tz, scope, locale, now)` —— 创建任务 + 立即触发生成
5. 201 + taskDTO（即使生成失败但 task 已创建也返 201）

**关键**：`if err != nil && task == nil` 才报错——task 创建成功但生成失败仍返 task，前端轮询状态。

### getTask —— 统一详情

```go
func (h *Handler) getTask(w http.ResponseWriter, r *http.Request)
```

`taskIDParam` 解码 ID 后：
- `CutPrefix("report-schedule:")` → 解析数字 ID → `uc.GetSchedule` → `taskFromSchedule`
- `CutPrefix("oneoff:")` → `uc.GetTask(ctx, uuid)` → `taskFromOneoff`
- 都不匹配 → `errs.ErrInvalid`

### rerunTask —— 重跑 oneoff

```go
func (h *Handler) rerunTask(w http.ResponseWriter, r *http.Request)
```

仅支持 `oneoff:` prefix（recurring 的 run-now 在 `/v1/report-schedules/{id}/run-now`）。`uc.RerunOneoffTask(ctx, uuid, locale, now)`，`if err != nil && task == nil` 才报错。

### deleteTask —— 删除 oneoff

```go
func (h *Handler) deleteTask(w http.ResponseWriter, r *http.Request)
```

仅支持 `oneoff:` prefix。`uc.DeleteTask(ctx, uuid)` → 204 No Content。

## 5. 依赖关系

**外部**：
- `chi` —— URL 参数
- `net/http`、`encoding/json`、`net/url`、`strconv`、`strings`、`sort`、`time`

**内部**：
- `internal/manager/model/report`（ReportSchedule / Task / TaskKind 常量 / OneoffTaskRef）
- `internal/pkg/errs`
- `internal/pkg/tenantctx`

## 6. 并发与资源管理

- **`uc *bizreport.Usecase` 共享**：与 http.go 同 handler 实例，无独立状态
- **请求级隔离**：每请求独立 ctx
- **`MaxBytesReader(32 KiB)`**：createOneoffTask body 上限，防恶意大 body
- **无 goroutine 启动**：所有调用同步，异步生成在 biz 层

## 7. 设计模式与亮点

1. **统一任务 surface**（HLD-022 Phase 2）：recurring + oneoff union 成单一列表，前端任务页一个视图
2. **prefix ID 区分来源**：`report-schedule:<n>` / `oneoff:<uuid>` 让一个 endpoint 处理两类资源
3. **`taskIDParam` percent-decode**：chi 留 URL 编码，本函数 `url.PathUnescape` 解码后再 prefix 匹配——关键细节，否则 `CutPrefix` 失败
4. **`taskFromSchedule` / `taskFromOneoff` 双转换函数**：把两个 model 类型归一到 taskDTO，前端无需感知后端表结构
5. **`Trigger` 人类可读字符串**：`cron · tz` / `oneoff`，前端直接展示无需格式化
6. **`Status` 派生 vs 直接**：recurring 从 `Enabled` 派生（active/disabled），oneoff 用自身 status——统一字段但语义不同
7. **best-effort decode**：createOneoffTask 空 body 也可，用默认值——降低前端调用门槛
8. **`if err != nil && task == nil` 才报错**：task 创建成功但生成失败仍返 task，前端轮询状态
9. **`sort.Slice` 按 CreatedAt 倒序**：最新任务在前，符合列表页惯例
10. **rerun/delete 仅支持 oneoff**：recurring 的对应操作在 `/v1/report-schedules/*`，避免重复端点

## 8. 注意事项

1. **`taskIDParam` 必须 percent-decode**：客户端发 `%3A`，chi 不解码，忘记这一步 `CutPrefix` 会失败
2. **`OneoffTaskRef` 格式由 model 层决定**：本文件不硬编码 `oneoff:` prefix，用 `model.OneoffTaskRef(t.ID)` 构造
3. **`createOneoffTask` 空 body OK**：用默认 weekly + "{}" scope，前端可发空 POST
4. **`rerunTask` 不支持 recurring**：recurring 重跑用 `/v1/report-schedules/{id}/run-now`，本端点返 `errs.ErrInvalid`
5. **`deleteTask` 不支持 recurring**：recurring 删除用 `/v1/report-schedules/{id}` DELETE
6. **`listTasks` 不分页**：union 两个源后全量返回，大用户量需评估
7. **`ListSchedules` 受 RBAC 影响**：`role != viewer` 才返全部，viewer 仅返自己的（biz 层逻辑）
8. **`ListOneoffTasks` 不受 RBAC**：返所有 oneoff，与 schedules 不对称；如需 RBAC 需 biz 层调整
9. **`createOneoffTask` 返 201 即使生成失败**：只要 task 行创建成功就返，前端按 status 字段判断生成结果
10. **`taskDTO.ScheduleID *uint64`**：仅 recurring 有，前端用此跳转 schedule CRUD；oneoff 该字段 nil
11. **`Trigger` 字符串格式**：`CronSpec + " · " + Timezone`，前端直接展示；如 CronSpec 为空（kind 派生）会显示 ` · tz`，需评估
12. **无审计**：oneoff 创建/重跑/删除未调 `SetAuditEvent`，如需追溯需补 hook
