# `migrate.go` 技术实现文档

> 源文件：`internal/manager/data/aiops/store/migrate.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/aiops/store`

## 1. 概述

本文件使用 GORM `AutoMigrate` 注册 AIOps chat 相关五张表，是 aiops store 的 schema 入口。`AutoMigrate` 仅新增列、不删改既有列，CHECK 约束（Role / Status / Decision）由 model 层 gorm tag 承载并在 MySQL 与 SQLite 双方言下保持一致。红线：生产 schema 演进应迁至版本化 SQL migration 文件，AutoMigrate 仅适合预生产阶段。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/aiops`
- **依赖方向**：被 `cmd/ongrid` 启动时调用；依赖 `gorm.io/gorm`、`internal/manager/model/aiops`。

## 3. 关键类型与接口

无自定义类型；仅使用 model 包定义的 GORM 模型。

```go
// 注册的五张表对应的 model 类型
model.Session{}              // chat_sessions
model.Message{}              // chat_messages
model.ToolCall{}             // chat_tool_calls
model.MutatingProposal{}     // chat_mutating_proposals
model.UserAgent{}            // chat_user_agents
```

## 4. 关键函数与流程

### `Migrate`

- **签名**：`func Migrate(db *gorm.DB) error`
- **职责**：通过 `db.AutoMigrate` 注册上述五张表。
- **流程**：一次性调用 `AutoMigrate(...)` 传入全部五个 model 指针，错误直接上抛。
- **错误处理**：返回 AutoMigrate 错误，由调用方（启动器）决定是否中止启动。

## 5. 依赖关系

- **内部包**：`internal/manager/model/aiops`（GORM 模型定义）
- **外部库**：`gorm.io/gorm`
- **被调用方**：`cmd/ongrid` 启动序列中的 DB 初始化逻辑

## 6. 并发与资源管理

- 无显式锁；由 GORM 内部管理连接。建议调用方在启动期串行执行迁移，避免与请求路径并发。

## 7. 设计模式与亮点

- **CHECK 约束跨方言一致**：注释指出 Role / Status / Decision 字段的 CHECK 约束由 model gorm tag 表达，在 MySQL 与 SQLite 双方言下表现一致，避免方言差异导致的脏数据。
- **`chat_mutating_proposals` 是审计源**：每个 mutating 工具调用无论 approve / reject 都会留下一条 row，作为 reviewer 审计的事实表。
- **idempotent AutoMigrate**：GORM AutoMigrate 天然幂等，二次启动找不到新增列时跳过。

## 8. 注意事项

- **预生产阶段适用**：注释明示"Production schema evolution should still move to versioned SQL migrations"。生产控制面接入滚动发布后需切换至 migration 文件 + expand-contract 模式。
- **不删列**：AutoMigrate 永不删除既有列；废弃列需通过显式 migration 文件清理。
- **PR-7 reviewer reality-check 来源**：`chat_mutating_proposals` 表的引入对应 PR-7 的 reviewer 阶段，文档变更需同步说明此背景。
