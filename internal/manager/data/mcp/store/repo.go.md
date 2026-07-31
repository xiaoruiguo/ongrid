# `repo.go` 技术实现文档

> 源文件：`internal/manager/data/mcp/store/repo.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/mcp/store`

## 1. 概述

本文件实现 external MCP server 注册表（HLD-018）的持久化层。核心约束：**dumb storage**——credential 解析与 header-template 展开在 biz/mcp，repo 仅存原值。关键设计：`Update` 不写 status / last_error / tools_cache（归 probe path 的 SetStatus / SetToolsCache）；`Create` 用 `isDup` 跨方言检测唯一冲突翻译为 `ErrConflict`；probe path 独立 SetStatus / SetToolsCache 避免 generic edit 覆盖运行时状态。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/mcp`
- **依赖方向**：被 `internal/manager/biz/mcp` 装配；依赖 `internal/manager/model/mcp`、`internal/pkg/errs`、`gorm.io/gorm`。

## 3. 关键类型与接口

```go
// Repo 是 MCP server 的 GORM 持久化层。并发安全。
type Repo struct{ db *gorm.DB }
```

## 4. 关键函数与流程

### `NewRepo`
- **签名**：`func NewRepo(db *gorm.DB) *Repo`

### `Create`
- **签名**：`func (r *Repo) Create(ctx, s *model.Server) error`
- **职责**：插入新 server 注册。Name 必须唯一。
- **流程**：
  1. `s == nil || strings.TrimSpace(s.Name) == ""` → `ErrInvalid`
  2. Create
  3. `isDup(err)` → `ErrConflict`（"mcp server %q already exists"）
- **错误处理**：`isDup` 检测 gorm.ErrDuplicatedKey + 字符串 "UNIQUE" / "Duplicate" / "duplicate"。

### `Get` / `GetByName`
- `Get`：按 id；`gorm.ErrRecordNotFound` → `ErrNotFound`。
- `GetByName`：按唯一 name；同上。

### `List`
- 返回全部注册 server，`name ASC` 排序。

### `Update`
- **签名**：`func (r *Repo) Update(ctx, id uint64, patch *model.Server) error`
- **职责**：写可编辑字段（transport / endpoint / command / args_json / credential / header_template_json / trusted / enabled）。
- **关键约束**：**不写 status / last_error / tools_cache**——这些归 probe path 的 SetStatus / SetToolsCache，避免 generic edit 覆盖运行时状态。
- **错误处理**：`patch == nil` → `ErrInvalid`；`RowsAffected == 0` → `ErrNotFound`。

### `Delete`
- 按 id 删。`RowsAffected == 0` → `ErrNotFound`。

### `SetStatus`
- **签名**：`func (r *Repo) SetStatus(ctx, id uint64, status, lastErr string) error`
- **职责**：记录 connection probe 结果（status + last_error）。
- **被调用方**：probe path（连接探测）。
- **错误处理**：`RowsAffected == 0` → `ErrNotFound`。

### `SetToolsCache`
- **签名**：`func (r *Repo) SetToolsCache(ctx, id uint64, toolsJSON string) error`
- **职责**：存储成功 probe 的 tools 快照 JSON。
- **被调用方**：probe path。
- **错误处理**：`RowsAffected == 0` → `ErrNotFound`。

### `isDup`
- **签名**：`func isDup(err error) bool`
- **职责**：跨方言检测唯一键冲突。`errors.Is(err, gorm.ErrDuplicatedKey)` + 字符串 "UNIQUE" / "Duplicate" / "duplicate"。

## 5. 依赖关系

- **内部包**：`internal/manager/model/mcp`、`internal/pkg/errs`
- **外部库**：`gorm.io/gorm`、`context`、`errors`、`fmt`、`strings`
- **被调用方**：`internal/manager/biz/mcp` usecase（含 probe path）

## 6. 并发与资源管理

- **无显式锁**：依赖 gorm 与 DB name 唯一索引。
- **ctx 透传**：所有方法首参 ctx。
- **generic edit vs probe path 分离**：Update 不写运行时状态，probe path 独立 SetStatus / SetToolsCache，避免覆盖。

## 7. 设计模式与亮点

- **dumb storage 原则**：注释明示 credential 解析与 header-template 展开在 biz/mcp，repo 仅存原值。
- **generic edit vs probe path 分离**：Update 写可编辑字段，SetStatus / SetToolsCache 写运行时状态，职责分离避免覆盖。
- **isDup 跨方言检测**：gorm.ErrDuplicatedKey + 字符串匹配，覆盖新 + 旧 gorm 版本。
- **name 唯一索引 + friendly 错误**：Create 冲突翻译为 `ErrConflict` 含 server name。

## 8. 注意事项

- **dumb storage**：repo 不做 credential 解析 / header-template 展开；caller (biz/mcp) 负责。
- **Update 不写运行时状态**：status / last_error / tools_cache 由 probe path 维护；扩展可编辑字段需同步 Update map。
- **isDup 字符串匹配**：新 DB 方言需扩展匹配；gorm ErrDuplicatedKey 优先。
- **SetStatus / SetToolsCache 独立**：probe path 专用，caller 需区分 generic edit 与 probe update。
- **name 唯一**：Create 冲突返回 ErrConflict；Update 不改 name（patch.Name 未在 updates map）。
- **Delete 软删或硬删**：代码用 `Delete(&model.Server{})`，依赖 model 是否有 DeletedAt 决定软/硬删。
