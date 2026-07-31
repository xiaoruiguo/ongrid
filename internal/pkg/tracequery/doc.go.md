# `doc.go` 技术实现文档

> 源文件：`internal/pkg/tracequery/doc.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/tracequery`

## 1. 概述

本文件仅为 `tracequery` 包的包级文档注释，不含可执行代码。说明该包是 Tempo HTTP 查询 API 的极简客户端，被 manager AI 工具注册表消费；响应原样透传给 LLM，故保留原始 JSON 形状。包名刻意解耦后端实现。

## 2. 包信息

- **包名**：`tracequery`
- **所属模块**：`internal/pkg/`
- **依赖方向**：被 manager AI 工具注册表调用；本文件不引入任何依赖

## 3. 关键类型与接口

无显著类型定义（仅包注释）。

## 4. 关键函数与流程

无（纯文档文件）。

文档要点：
- **协议形态**：Tempo 的 `/api/search` 与 `/api/traces/<id>`
- **响应透传**：响应原样返回给 LLM，故保留原始 JSON shape
- **包名解耦后端**：`tracequery` 而非 `tempoquery`，未来换 VictoriaTraces 等只需改 import 站点而非 rename 涟漪
- **同款约定**：注释指向 `logquery`（应用了相同约定）
- **跨 BC**：本包位于 `internal/pkg/`，无 `manager/*` import

## 5. 依赖关系

- **内部包**：无
- **外部库**：无
- **被调用方**：N/A（仅文档）

## 6. 并发与资源管理

无并发控制（纯文档文件）。

## 7. 设计模式与亮点

- **后端解耦命名**：把"做什么"（trace query）而非"对谁做"（Tempo）作为包名，是项目内一致约定（logquery 同款）。这让后端替换成为局部变更
- **跨 BC 边界声明**：明示"lives under internal/pkg/ and has no manager/* import"，便于架构审查
- **决策留痕**：把"为什么透传 raw JSON"的取舍（Tempo schema 跨版本变化）写进注释

## 8. 注意事项

- 若未来引入 VictoriaTraces 等后端，需评估其 API 与 Tempo 的差异（如 TraceQL 兼容性）；包名不变但实现可能需分支
- 若 LLM 工具需求扩展（如聚合统计、span 树重建），需评估是否仍能保持 raw JSON 透传，或引入结构化解码
