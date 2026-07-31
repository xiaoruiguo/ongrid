# `doc.go` 技术实现文档

> 源文件：`internal/edgeagent/collector/doc.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/collector`

## 1. 概述

Go 包级文档文件，描述 `collector` 包的两种互换主机 metric 源：embedded（进程内 gopsutil，node_exporter 兼容命名，CGO-free）与 scrape（多目标 HTTP scraper，expfmt 解析 Prom 文本格式）。两种模式喂同一管线：每个 tick 产出 `CollectorOutput`（legacy 8 字段 HostMetricPoint 快路径 + flat `PromSample` 切片 for push_prom_samples）。

## 2. 包信息

- **包名**：`collector`
- **所属模块**：edgeagent metric 采集层
- **依赖方向**：被 `biz.Agent` 通过 `Collector` 接口消费

## 3. 关键类型与接口

无类型定义。仅是包注释。

## 4. 关键函数与流程

无函数。

## 5. 依赖关系

- **内部包**：无
- **外部库**：无
- **被调用方**：无（仅 Go doc 工具消费）

## 6. 并发与资源管理

无并发控制。

## 7. 设计模式与亮点

包注释解释了关键架构决策：
- **node_exporter SDK 被拒绝**：kingpin-bound flag state、Linux-only build constraints、>100 transitive deps；gopsutil v3 给同样的 5 个资源（cpu/mem/load/net/disk）但纯 Go 零 CGO
- **metric 命名遵循 node_exporter 惯例**：让 cloud-side mapper / PromQL 查询 byte-for-byte 匹配 embedded 或 scrape 任一来源
- **双路径设计**：legacy 8 字段快路径（push_host_metrics）+ 开放集 rich 路径（push_prom_samples）共存，平滑迁移
- **进程列表也由 gopsutil 提供**：`gopsutil process.Process` 复用同一依赖

## 8. 注意事项

- 包注释提到的「node_exporter SDK」指 prometheus/node_exporter 项目；评估结论已固化在文档中，未来如需重新评估需对照此结论
- metric 命名约定（`node_cpu_seconds_total` 等）是 cloud-side mapper 的契约；新增 metric 必须遵循
- embedded 与 scrape 的 `HostMetricPoint` 字段语义必须一致——mapper 单一实现服务两种来源
