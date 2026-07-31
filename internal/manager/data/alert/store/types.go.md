# `types.go` 技术实现文档

> 源文件：`internal/manager/data/alert/store/types.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/alert/store`

## 1. 概述

本文件定义 alert store 的 storage 层 filter 类型（IncidentFilter / SilenceFilter / RuleFilter / ChannelFilter / DeliveryFilter）、`Store` 扩展接口（biz.Repo 之上的 row-level CRUD），以及三个 status 校验函数。`Store` 接口让 service 层可注入测试 fake 而不依赖具体 `Repo` 类型；校验函数集中状态机枚举，防止 invalid status 入库。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/alert`
- **依赖方向**：被同包 `Repo` 实现与外部测试 fake 实现；依赖 `internal/manager/model/alert`（status 常量）。

## 3. 关键类型与接口

```go
// IncidentFilter — storage 层 filter，与 biz.IncidentFilter 略有差异
// （RuleID 而非 RuleKey，便于 rule_id 索引）
type IncidentFilter struct {
    Status    string
    DeviceID  *uint64
    RuleID    *uint64
    Severity  string
    Limit, Offset int
}

type SilenceFilter struct {
    Status   string
    ActiveAt *time.Time
    Limit, Offset int
}

type RuleFilter struct {
    Enabled    *bool
    ScopeType  string
    SourceType string
    Limit, Offset int
}

type ChannelFilter struct {
    Enabled     *bool
    ChannelType string
    Limit, Offset int
}

type DeliveryFilter struct {
    IncidentID *uint64
    ChannelID  *uint64
    Status     string
    Limit, Offset int
}

// Store 暴露 biz.Repo 之上的 row-level CRUD 接口
type Store interface {
    CreateAlertIncident(ctx, in *model.Incident) error
    GetIncidentByDedupeKey(ctx, dedupeKey string) (*model.Incident, error)
    ListIncidentRows(ctx, filter IncidentFilter) ([]*model.Incident, error)

    GetSilenceByID(ctx, id uint64) (*model.Silence, error)
    ListSilenceRows(ctx, filter SilenceFilter) ([]*model.Silence, error)
    UpdateSilenceStatus(ctx, id uint64, status string, cancelledBy *uint64, cancelledAt *time.Time) error

    CreateRule(ctx, in *model.Rule) error
    GetRuleByID(ctx, id uint64) (*model.Rule, error)
    ListRuleRows(ctx, filter RuleFilter) ([]*model.Rule, error)
    UpdateRuleEnabled(ctx, id uint64, enabled bool) error

    CreateChannel(ctx, in *model.Channel) error
    GetChannelByID(ctx, id uint64) (*model.Channel, error)
    ListChannelRows(ctx, filter ChannelFilter) ([]*model.Channel, error)
    UpdateChannelEnabled(ctx, id uint64, enabled bool) error

    CreateDelivery(ctx, in *model.Delivery) error
    GetDeliveryByID(ctx, id uint64) (*model.Delivery, error)
    ListDeliveryRows(ctx, filter DeliveryFilter) ([]*model.Delivery, error)
    UpdateDeliveryStatus(ctx, id uint64, status string, attemptCount uint32, providerMessageID, responseJSON, errMsg *string, sentAt, finishedAt *time.Time) error
}

var _ Store = (*Repo)(nil)
```

## 4. 关键函数与流程

### `validateIncidentStatus`
- **签名**：`func validateIncidentStatus(status string) error`
- **职责**：校验 incident status ∈ {Open, Acknowledged, Silenced, Resolved}。
- **错误处理**：未知 status → `fmt.Errorf("unknown incident status %q")`，caller 用 `%w: %v` 包装为 `errs.ErrInvalid`。

### `validateSilenceStatus`
- **签名**：`func validateSilenceStatus(status string) error`
- **职责**：校验 silence status ∈ {Active, Expired, Cancelled}。

### `validateDeliveryStatus`
- **签名**：`func validateDeliveryStatus(status string) error`
- **职责**：校验 delivery status ∈ {Pending, Success, Failed}。

## 5. 依赖关系

- **内部包**：`internal/manager/model/alert`（status 常量）
- **外部库**：`context`、`fmt`、`time`
- **被调用方**：同包 `Repo` 实现；service 层通过 `Store` 接口注入测试 fake。

## 6. 并发与资源管理

不涉及。

## 7. 设计模式与亮点

- **Store 接口与 biz.Repo 分离**：biz.Repo 是 biz 层消费的窄接口，Store 是 storage 层的 row-level CRUD 接口；service 层可通过 Store 接口注入 fake。
- **filter 类型 storage 层独立**：与 biz 层 filter 分离，允许 storage 用 RuleID（索引友好）而 biz 用 RuleKey（语义友好）。
- **状态机枚举集中**：三个 validate 函数集中 status 枚举，防止 invalid status 入库。
- **编译期接口断言**：`var _ Store = (*Repo)(nil)` 保证 Repo 实现 Store 接口。

## 8. 注意事项

- **storage filter 与 biz filter 差异**：IncidentFilter 用 RuleID（storage 索引友好），biz.IncidentFilter 用 RuleKey（语义友好）；service 层负责翻译。
- **status 枚举扩展**：新增 status 需同步更新 validate 函数 + model 层常量。
- **Store 接口扩展**：新增 row-level CRUD 需同步更新 Store 接口与 Repo 实现。
- **limit/offset 零值语义**：filter 中 Limit/Offset 零值表示"不过滤"，与 biz 层语义一致。
