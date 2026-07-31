# `repo.go` 技术实现文档

> 源文件：`internal/manager/data/knowledge/store/repo.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/knowledge/store`

## 1. 概述

本文件是 knowledge base 关系型部分（git repo 注册 + SSH identity）的持久化层。Phase-2 后 doc 存储迁至 qdrant 向量库，本包仅管关系型元数据。核心设计：`Migrate` 仅创建 `knowledge_repos` + `ssh_identities`（不再创建 `knowledge_docs`）；`GetRepoByURL` 用 URL 唯一索引做幂等 boot-time seeding；`UpdateSSHIdentity` 不更新 private key（immutable post-create，轮换需删 + 建）；`TouchSSHIdentityUsage` best-effort 不失败 clone。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/knowledge`
- **依赖方向**：被 `internal/manager/biz/knowledge` 装配；依赖 `internal/manager/model/knowledge`、`internal/pkg/errs`、`gorm.io/gorm`。doc 存储在 `internal/pkg/qdrantx` + biz/knowledge usecase。

## 3. 关键类型与接口

```go
// Repo 是关系型 repo（仅 git repo 注册）。
type Repo struct {
    db *gorm.DB
}
```

## 4. 关键函数与流程

### `Migrate`
- **签名**：`func Migrate(db *gorm.DB) error`
- **职责**：AutoMigrate `knowledge_repos` + `ssh_identities`。
- **关键约束**：`knowledge_docs` 不再创建——Phase-2 移 doc 存储到 qdrant。

### Repository（git repo 注册）

#### `New`
- **签名**：`func New(db *gorm.DB) *Repo`

#### `ListRepos`
- 返回全部注册 git repo，`url ASC` 排序。

#### `GetRepo` / `GetRepoByURL`
- `GetRepo`：按 id；`gorm.ErrRecordNotFound` → `ErrNotFound`。
- `GetRepoByURL`：按 URL 唯一索引取；用于幂等 boot-time seeding。

#### `CreateRepo`
- 插入新 repo 注册。

#### `UpdateRepoSync`
- **签名**：`func (r *Repo) UpdateRepoSync(ctx, id uint64, fileCount int, syncErr string) error`
- **职责**：刷新 `last_synced_at` + `last_sync_error` + `file_count`。
- **实现**：`gorm.Expr("CURRENT_TIMESTAMP")` 让 DB 生成时间戳，避免 caller 时区问题。`RowsAffected == 0` → `ErrNotFound`。

#### `DeleteRepo`
- 删注册行。**caller 需另删 qdrant points**（biz.Usecase.DeleteRepo 做两步）。`RowsAffected == 0` → `ErrNotFound`。

### SSHIdentity

#### `ListSSHIdentities` / `GetSSHIdentity`
- `ListSSHIdentities`：全部 SSH identity，`name ASC`。
- `GetSSHIdentity`：按 id；`gorm.ErrRecordNotFound` → `ErrNotFound`。

#### `CreateSSHIdentity`
- 插入新 identity。**caller 负责计算 fingerprint 与校验 PEM 形状**。

#### `UpdateSSHIdentity`
- **签名**：`func (r *Repo) UpdateSSHIdentity(ctx, id uint64, name, hostsJSON, knownHosts string) error`
- **职责**：更新可编辑字段（name / hosts / known_hosts）。
- **关键约束**：**private key immutable post-create**——轮换需删 + 建新 identity。`RowsAffected == 0` → `ErrNotFound`。

#### `TouchSSHIdentityUsage`
- **签名**：`func (r *Repo) TouchSSHIdentityUsage(ctx, id uint64) error`
- **职责**：成功 clone 后 bump `last_used_at`。best-effort——错误在 biz 层 log 但不失败 clone。
- **实现**：`gorm.Expr("CURRENT_TIMESTAMP")` 让 DB 生成时间戳。

#### `DeleteSSHIdentity`
- 按 id 删。`RowsAffected == 0` → `ErrNotFound`。

## 5. 依赖关系

- **内部包**：`internal/manager/model/knowledge`、`internal/pkg/errs`
- **外部库**：`gorm.io/gorm`、`context`、`errors`
- **被调用方**：`internal/manager/biz/knowledge` usecase（同时驱动关系型 repo 与 qdrant 向量库）

## 6. 并发与资源管理

- **无显式锁**：依赖 gorm 与 DB 唯一索引（URL 唯一）。
- **ctx 透传**：所有方法首参 ctx。
- **TouchSSHIdentityUsage best-effort**：错误不传播，caller 需 log 但不 fail。

## 7. 设计模式与亮点

- **Phase-2 doc 存储迁移**：`knowledge_docs` 不再创建，doc 存储在 qdrant；关系型仅留元数据。
- **GetRepoByURL 幂等 seeding**：URL 唯一索引 + GetRepoByURL 让 boot-time seeding 幂等。
- **private key immutable**：UpdateSSHIdentity 不更新 private key，轮换需删 + 建，安全约束。
- **TouchSSHIdentityUsage best-effort**：timestamp 纯运维用途，不失败 clone。
- **gorm.Expr CURRENT_TIMESTAMP**：让 DB 生成时间戳，避免 caller 时区问题。
- **DeleteRepo caller 清 qdrant**：repo 仅删关系型行，qdrant points 由 biz 层清，职责分离。

## 8. 注意事项

- **knowledge_docs 不再创建**：Phase-2 后 doc 在 qdrant；如需迁回关系型需新增 migration。
- **private key immutable**：UpdateSSHIdentity 不更新 private key；caller 需提示用户轮换需删 + 建。
- **TouchSSHIdentityUsage best-effort**：caller 需 log 错误但不 fail clone。
- **DeleteRepo 不清 qdrant**：caller（biz.Usecase.DeleteRepo）需另删 qdrant points。
- **CreateSSHIdentity caller 校验**：caller 需计算 fingerprint + 校验 PEM 形状，repo 不校验。
- **gorm.Expr CURRENT_TIMESTAMP**：DB 生成时间戳；跨时区部署时保证一致。
