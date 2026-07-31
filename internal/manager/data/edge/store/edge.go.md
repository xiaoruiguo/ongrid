# `edge.go` 技术实现文档

> 源文件：`internal/manager/data/edge/store/edge.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/edge/store`

## 1. 概述

本文件实现 `biz/edge.Repo` 的 GORM 落地，覆盖 edge 实体的生命周期：创建、按 id/access_key/name 查找、列表、secret_hash 更新、status + last_seen_at 状态机、name 自动回填、device_id 链接（post-split）、agent_version 记录、软删、计数。关键设计：post-split（May 2026）role 过滤移至 device repo，caller 按 role 过滤需查 device 再通过 edge_devices junction 反查 edge。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/edge`
- **依赖方向**：被 `internal/manager/biz/edge` 装配；依赖 `internal/manager/biz/edge`（接口与 `ListFilter`）、`internal/manager/model/edge`、`internal/pkg/errs`、`gorm.io/gorm`。

## 3. 关键类型与接口

```go
// Repo 是 biz/edge.Repo 的 GORM 实现。
type Repo struct {
    db *gorm.DB
}

var _ biz.Repo = (*Repo)(nil)
```

## 4. 关键函数与流程

### 构造与查找

#### `NewRepo`
- **签名**：`func NewRepo(db *gorm.DB) *Repo`
- 裸构造器，返回具体类型（供测试 introspection）；wire-ready 入口在 `provider.go::NewBizRepo`。

#### `Create`
- `e == nil` → `ErrInvalid`；Create。insert-side 失败（unique violation 等）不包装返回，caller 可 `errors.Is(gorm.ErrDuplicatedKey)`。

#### `GetByID` / `GetByAccessKey` / `GetByName`
- 按 PK / access_key_id / name 取 edge；`gorm.ErrRecordNotFound` → `ErrNotFound`。不返回软删行（gorm 默认 DeletedAt scope）。

### 列表

#### `List`
- 按 `biz.ListFilter` 过滤：Status / Name(LIKE) / CreatedBy / Limit / Offset。
- Order `id DESC`（最新注册优先）。
- **post-split 关键约束**：role 过滤移至 device repo；caller 按 role 过滤需查 device + edge_devices junction 反查 edge。

### 字段更新

#### `UpdateSecretHash`
- 替换 `secret_key_hash`。`RowsAffected == 0` → `ErrNotFound`。

#### `UpdateStatus`
- 同时设置 `status` + `last_seen_at`。`RowsAffected == 0` → `ErrNotFound`。

#### `UpdateName`
- 覆盖 operator-friendly display name。
- **用途**：`edge.HandleRegister` 在首次 tunnel handshake 时用 host 上报的 hostname 回填空白 name——admin 创建 edge 未填 name 时，agent boot 自动填充。

#### `SetDeviceID` / `ClearDeviceID`
- post-split 数据模型：link/clear edge 行的 host device pointer。
- `SetDeviceID`：`HandleRegister` 在 host Device upsert 后调用，让后续 read 可 join Device 取 host facts。
- `ClearDeviceID`：清空 host device pointer。

#### `SetAgentVersion`
- 记录 agent 自报的二进制版本（register_edge 时）。
- **关键约束**：caller 上游过滤空值，避免 buggy build 报告空值时清空列。

### 删除与计数

#### `Delete`
- 软删（gorm DeletedAt）。后续 Get/List 隐藏行。`RowsAffected == 0` → `ErrNotFound`。

#### `Count`
- 非软删 edge 数。

## 5. 依赖关系

- **内部包**：`internal/manager/biz/edge`（接口、`ListFilter`）、`internal/manager/model/edge`、`internal/pkg/errs`
- **外部库**：`gorm.io/gorm`
- **被调用方**：`internal/manager/biz/edge` usecase + Authenticator；`cmd/ongrid` 通过 `provider.go::NewBizRepo` 装配。

## 6. 并发与资源管理

- **无显式锁**：依赖 gorm 与 DB 索引。
- **ctx 透传**：所有方法首参 ctx。
- **软删**：Delete 用 gorm DeletedAt，保留审计。

## 7. 设计模式与亮点

- **NewRepo vs NewBizRepo 职责分离**：NewRepo 返回具体类型供测试 introspection；NewBizRepo 返回 biz 接口供 wiring，隔离装配层与具体类型。
- **UpdateName 自动回填**：首次 handshake 时用 host hostname 填空白 name，operator 创建 edge 未填 name 也能看到自动填充。
- **SetAgentVersion caller 过滤空值**：避免 buggy build 清空列，repo 层不重复校验。
- **post-split role 过滤转移**：role 不再属于 edge，移至 device repo；edge List 不支持 role filter，caller 通过 junction 反查。

## 8. 注意事项

- **Create 不包装 unique violation**：caller 需 `errors.Is(gorm.ErrDuplicatedKey)` 自行判断。
- **role 过滤已转移**：post-split 后 edge 无 role 列；按 role 过滤需查 device + edge_devices junction。
- **UpdateName 自动回填**：仅填空白 name，不覆盖 operator 已设值；caller 需检查 name 是否为空。
- **SetAgentVersion caller 过滤**：repo 不校验空值，caller 必须过滤 buggy build 空报告。
- **软删**：Delete 用 gorm DeletedAt；硬删需 Unscoped。
- **GetByAccessKey 不返回软删行**：gorm 默认 scope 隐藏软删行；如需查软删行（如审计）需 Unscoped。
