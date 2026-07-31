# `doc.go` 技术实现文档（promquery）

> 源文件：`internal/pkg/promquery/doc.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/promquery`

## 1. 概述

该文件是 `promquery` 包的文档声明文件，无任何代码实现，仅以 Go 包注释说明包的定位：Prometheus HTTP 查询 API 的轻量客户端，封装 `/api/v1/query` 与 `/api/v1/query_range`。被 manager AI 工具注册表消费，响应以 raw JSON 透传回 LLM 保留原始形状。包刻意位于 `internal/pkg/` 且无 `manager/*` 导入，符合 gospec 跨 BC 边界红线。

## 2. 包信息

- **包名**：`promquery`
- **所属模块**：`internal/pkg/`（基础设施层）
- 依赖方向：纯文档，无依赖。

## 3. 关键类型与接口

无显著类型定义（文件仅含包注释，无任何声明）。

## 4. 关键函数与流程

无关键函数。文档明确了：
- **包内职责**：封装 Prom HTTP 查询 API（`/api/v1/query` + `/api/v1/query_range`）。
- **消费方**：manager AI 工具注册表。
- **响应处理**：raw JSON 透传 LLM，保留原始形状。
- **跨 BC 边界**：位于 `internal/pkg/` 且无 `manager/*` 导入。

## 5. 依赖关系

- **内部包**：无。
- **外部库**：无。
- **被调用方**：无（仅作为包文档被 `go doc` / IDE 读取）。

## 6. 并发与资源管理

无并发控制（无代码）。

## 7. 设计模式与亮点

- **跨 BC 边界文档化**：包注释明确"位于 `internal/pkg/` 且无 `manager/*` 导入"，是 gospec monorepo BC 隔离红线的具体体现——`internal/pkg/` 是共享基础设施层，不依赖任何 BC。
- **raw JSON 透传理念**：注释说明响应以 raw JSON 传 LLM，避免客户端模型与 Prom 演进脱节——这是 promquery 设计的核心 trade-off。
- **极简文档**：仅 8 行注释，聚焦包定位与跨 BC 边界，不重复 `client.go` 已有的实现细节。
- **消费方明确**：注释指出"manager AI 工具注册表"是唯一消费方，让维护者理清依赖链。

## 8. 注意事项

- **文档与实现可能漂移**：注释提到的端点（`/api/v1/query` + `/api/v1/query_range`）需与 `client.go` 实现同步；新增端点需更新本注释。
- **不覆盖 promauth 集成**：本注释未提及 auth 注入；实际使用需配合 `promauth.BuildClient` 构造带 auth 的 `*http.Client`。
- **不覆盖 Resolver 模式**：本注释未提及 `BaseURLResolver` 动态 URL 模式；详见 `client.go`。
- **消费方可能扩展**：当前仅 AI 工具注册表消费，未来 alert evaluator 等也可能使用；扩展时需评估性能与超时配置。
- **不覆盖重试策略**：本注释未提及重试；实际无重试，调用方需自行实现。
- **`raw JSON 透传` 的局限**：透传 LLM 可能导致 token 消耗大（matrix 响应可能很大）；调用方应考虑截断或摘要。
