# `repo.go` 技术实现文档

> 源文件：`internal/manager/data/marketplace/store/repo.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/marketplace/store`

## 1. 概述

本文件实现 `installed_skills` 表（marketplace lock 表）的持久化层。核心设计：`Create` 用字符串匹配检测 UNIQUE 冲突翻译为 `ErrConflict`；`GetByManifestSHA` 防止用户通过 renamed local copy 重复安装同内容 pack；`List` tenantID=0 表示跨 tenant admin 视图；`SetBindings` 存储 slot→credential JSON（HLD-017 凭据绑定）。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/marketplace`
- **依赖方向**：被 `internal/manager/biz/marketplace` 装配；依赖 `internal/manager/model/marketplace`、`internal/pkg/errs`、`gorm.io/gorm`。

## 3. 关键类型与接口

```go
// Repo 是 installed_skills 的 GORM 持久化层。并发安全；gorm session 每次调用独立。
type Repo struct {
    db *gorm.DB
}
```

## 4. 关键函数与流程

### `NewRepo`
- **签名**：`func NewRepo(db *gorm.DB) *Repo`

### `Create`
- **签名**：`func (r *Repo) Create(ctx, p *model.InstalledPack) error`
- **职责**：插入新 installed pack 行。PackID + TenantID 组成唯一键。
- **流程**：
  1. `p == nil` → `ErrInvalid`
  2. `PackID == ""` → `ErrInvalid`
  3. Create
  4. 错误检查：`contains(msg, "UNIQUE") || contains(msg, "Duplicate") || contains(msg, "duplicate")` → `ErrConflict`（"pack already installed"）
- **关键约束**：gorm 不暴露 driver-neutral conflict 检测，用字符串匹配 MySQL + SQLite。

### `GetByPackID`
- **签名**：`func (r *Repo) GetByPackID(ctx, tenantID uint64, packID string) (*model.InstalledPack, error)`
- **职责**：按 (tenant_id, pack_id) 取非软删行。
- **错误处理**：`packID == ""` → `ErrInvalid`；`gorm.ErrRecordNotFound` → `ErrNotFound`。

### `GetByManifestSHA`
- **签名**：`func (r *Repo) GetByManifestSHA(ctx, tenantID uint64, sha string) (*model.InstalledPack, error)`
- **职责**：按 (tenant_id, manifest_sha256) 取非软删行。
- **用途**：install 时拒绝"同内容已在不同 pack_id 下安装"——防止用户通过 renamed local copy 重复安装。
- **错误处理**：`sha == ""` → `ErrInvalid`；`gorm.ErrRecordNotFound` → `ErrNotFound`。

### `List`
- **签名**：`func (r *Repo) List(ctx, tenantID uint64) ([]*model.InstalledPack, error)`
- **职责**：返回非软删 pack，`installed_at DESC` 排序。
- **关键约束**：`tenantID == 0` 返回所有 tenant 的行（admin 跨 tenant 视图；当前单 tenant 时也返回该 tenant 行）。

### `DeleteSoft`
- **签名**：`func (r *Repo) DeleteSoft(ctx, tenantID uint64, packID string) error`
- **职责**：软删 (tenant_id, pack_id) 行。幂等——已删或缺失返回 `ErrNotFound`。
- **错误处理**：`packID == ""` → `ErrInvalid`；`RowsAffected == 0` → `ErrNotFound`。

### `SetBindings`
- **签名**：`func (r *Repo) SetBindings(ctx, tenantID uint64, packID, bindingsJSON string) error`
- **职责**：存储 installed pack 的 slot→credential JSON（HLD-017 凭据绑定）。
- **错误处理**：`packID == ""` → `ErrInvalid`；`RowsAffected == 0` → `ErrNotFound`（空/缺失 pack）。

### `contains`
- **签名**：`func contains(s, sub string) bool`
- **职责**：tiny `strings.Contains` shim，保持 import surface 小（仅用于一处错误消息检查）。

## 5. 依赖关系

- **内部包**：`internal/manager/model/marketplace`、`internal/pkg/errs`
- **外部库**：`gorm.io/gorm`、`context`、`errors`、`fmt`
- **被调用方**：`internal/manager/biz/marketplace` usecase

## 6. 并发与资源管理

- **无显式锁**：依赖 gorm 与 DB 唯一索引（tenant_id + pack_id）。
- **ctx 透传**：所有方法首参 ctx。
- **软删**：DeleteSoft 用 gorm DeletedAt；查询都显式 `deleted_at IS NULL` 过滤。

## 7. 设计模式与亮点

- **字符串匹配检测 UNIQUE 冲突**：gorm 不暴露 driver-neutral conflict 检测，匹配 "UNIQUE" / "Duplicate" / "duplicate" 跨 MySQL + SQLite。
- **GetByManifestSHA 防重复安装**：防止用户通过 renamed local copy 重复安装同内容 pack。
- **List tenantID=0 跨 tenant**：admin 视图；单 tenant 时也工作。
- **contains shim**：避免 import `strings` 仅为一处检查。
- **显式 deleted_at IS NULL 过滤**：所有查询显式过滤软删行，不依赖 gorm 默认 scope（因 installed_skills 用 deleted_at 而非 delete_marker）。

## 8. 注意事项

- **唯一键 tenant_id + pack_id**：同 tenant 内 pack_id 唯一；跨 tenant 可重复 pack_id。
- **manifest_sha256 防重复**：同 tenant 内 manifest sha 唯一（业务约束，非 DB 索引）；caller 需在 install 前检查。
- **字符串匹配 UNIQUE 冲突脆弱**：新 DB 方言需扩展匹配字符串；gorm ErrDuplicatedKey 在新版本可用时优先。
- **软删用 deleted_at**：installed_skills 表用 deleted_at 而非 delete_marker；查询需显式 `deleted_at IS NULL`。
- **DeleteSoft 幂等**：已删行返回 ErrNotFound；caller 需区分"不存在" vs "已删"。
- **SetBindings 仅更新 bindings_json**：其余字段不动；caller 需先确保 pack 存在。
- **List tenantID=0 admin 视图**：多 tenant 部署时需确保 caller 有 admin 权限。
