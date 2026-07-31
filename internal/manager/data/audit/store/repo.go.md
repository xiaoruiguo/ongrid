# `repo.go` 技术实现文档

> 源文件：`internal/manager/data/audit/store/repo.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/audit/store`

## 1. 概述

本文件实现 HLD-010 audit_logs 的持久化层。核心约束：**append-only**——唯一 mutation 是 retention sweep（`DeleteOlderThan`）。`Insert` 失败不得传播到业务代码（caller 用 warn-log 吞掉，遵循 HLD-010 "audit write failure must never block the request"）。`List` 同时返回 matching rows 与 total count，让 UI 渲染 "showing N of total"。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/audit`
- **依赖方向**：被 `internal/manager/biz/audit` 装配；依赖 `internal/manager/model/audit`、`gorm.io/gorm`。

## 3. 关键类型与接口

```go
// Repo 是 audit_logs 的薄封装 —— audit 无业务规则，仅"写一行"+"按 filter 列"+"删旧"。
type Repo struct {
    db *gorm.DB
}

// ListFilters 集中 GET /v1/admin/audit-logs 的所有 querystring 参数，
// 避免	http 层传 7 个 arg。
type ListFilters struct {
    UserEmail    string
    Action       string
    ResourceType string
    Status       string
    From         time.Time
    To           time.Time
    Limit        int // repo 上限 500，防止 payload 过大
    Offset       int
}
```

## 4. 关键函数与流程

### `New`
- **签名**：`func New(db *gorm.DB) *Repo`
- **职责**：构造 Repo；db 不可为 nil。

### `Insert`
- **签名**：`func (r *Repo) Insert(ctx, log *model.Log) error`
- **职责**：追加一行。`OccurredAt` + `CreatedAt` 由 caller（biz.Usecase）与 gorm autoCreate 分别 stamp。
- **关键约束**：**失败不得传播到业务代码**——caller（`biz.Usecase.Emit`）用 warn-log 吞掉，遵循 HLD-010 "audit write failure must never block the request"。

### `List`
- **签名**：`func (r *Repo) List(ctx, f ListFilters) ([]model.Log, int64, error)`
- **职责**：按 filter 列出 matching rows（newest-first）+ total count（同 filter，不含 limit/offset），让 UI 渲染 "showing N of total"。
- **流程**：
  1. 构造 q with all Where（UserEmail / Action / ResourceType / Status / From / To）
  2. `q.Count(&total)`
  3. limit 规范化：`<= 0 || > 500` → 50；offset `< 0` → 0
  4. `q.Order("occurred_at DESC, id DESC").Limit(limit).Offset(offset).Find(&rows)`
- **关键约束**：Count 与 Find 必须用同一 q（含全部 Where），否则 total 与实际行数不一致。

### `DeleteOlderThan`
- **签名**：`func (r *Repo) DeleteOlderThan(ctx, cutoff time.Time) (int64, error)`
- **职责**：retention sweep，删除 `occurred_at < cutoff` 的行，返回删除数。
- **被调用方**：biz 层每日 retention goroutine。

## 5. 依赖关系

- **内部包**：`internal/manager/model/audit`
- **外部库**：`gorm.io/gorm`、`context`、`time`
- **被调用方**：`internal/manager/biz/audit` usecase（Emit + List + 每日 retention goroutine）

## 6. 并发与资源管理

- **无锁**：append-only 表，并发 INSERT 安全；retention sweep 由 biz 层串行调度。
- **ctx 透传**：所有方法首参 ctx。

## 7. 设计模式与亮点

- **append-only 原则**：注释明示"Inserts are append-only; the only mutation is the retention sweep"，符合审计日志不可篡改要求。
- **Insert 失败不阻塞业务**：HLD-010 红线，caller 用 warn-log 吞掉。
- **List + Count 同 filter**：避免 total 与实际行数不一致的常见 bug。
- **ListFilters 集中参数**：避免 http 层传 7 个 arg，保持签名清晰。
- **Limit 上限 500**：防止 payload 过大；`<= 0 || > 500` → 默认 50。
- **Order 双字段**：`occurred_at DESC, id DESC` 避免同时间戳行顺序不确定。

## 8. 注意事项

- **Insert 失败处理**：caller 必须 warn-log 吞掉，不得传播到业务路径；否则违反 HLD-010。
- **append-only**：禁止 UPDATE 操作；retention sweep 是唯一 DELETE。
- **Limit 上限 500**：caller 传更大值会被截断为 50；如需更大 payload 需调上限。
- **ListFilters 零值语义**：零值表示"不过滤该列"；From/To 用 `IsZero()` 判断。
- **时区**：`occurred_at` 由 caller stamp，建议统一 UTC。
- **retention 调度**：biz 层每日 goroutine 调用 `DeleteOlderThan`，repo 不主动调度。
