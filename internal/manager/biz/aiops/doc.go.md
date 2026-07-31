# `doc.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/doc.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops`

## 1. 概述

本文件是 `aiops` 包的包级文档注释，仅以 `// Package aiops ...` 形式存在，不含可执行代码。它声明 `aiops` 是 manager/aiops 子域的 biz 层入口，承载 AIOps 智能体循环（OpenAI tool-calling loop）及与之协作的告警草案、技能系统、eino graph 内核等子模块；上层 `internal/manager/controlplane/aiops` 调用本包暴露的 usecase。

## 2. 包信息

- **包名**：`aiops`
- **所属模块**：`internal/manager/biz/aiops`
- **职责**：定义 AIOps 会话领域接口（`SessionRepo`、`TokenSums` 等）、聚合 agent/alertdraft/chatruntime/graph/investigator/mentions/toolreplay 等子包
- **导入方向**：仅由 `controlplane/aiops`、`server/aiops` 等上层导入；本包不依赖 server/controlplane

## 3. 关键类型与接口

本文件不含类型定义；包内主要类型定义在 `repo.go`。

## 4. 关键函数与流程

本文件不含函数实现；仅作为包文档。

## 5. 依赖关系

- 无外部依赖（仅包注释）
- 文档说明的子模块依赖关系：agent（legacy loop）→ chatruntime（新运行时）→ graph（eino 内核）→ callbacks（persistence/SSE/audit/metrics/budget）

## 6. 并发与资源管理

不适用（无代码）。

## 7. 设计模式与亮点

- **包级文档**：通过 doc.go 显式标注包作用，方便 `go doc` 与 IDE hover
- **子域分层**：明确 biz 层定位，与 controlplane（编排）、repo（持久化）、model（DTO）形成单服务单向依赖链：`cmd → web → controlplane → repo → model`

## 8. 注意事项

- 修改本包时需同时关注 gospec 架构约束：`internal/<domain>` 之间禁止直接 import，须通过 API / 事件 / `internal/shared/`
- 添加新子包时建议在此 doc.go 中补充说明
