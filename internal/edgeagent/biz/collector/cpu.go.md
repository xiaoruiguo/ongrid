# `cpu.go` 技术实现文档

> 源文件：`internal/edgeagent/biz/collector/cpu.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/biz/collector`

## 1. 概述

Phase 1 占位实现：`CollectCPU` 函数声明从 `/proc/loadavg` + `/proc/stat` 采样 CPU 与负载平均值的契约，但当前直接返回零值 `model.HostMetric{}`，让 agent 主体逻辑可被独立 exercise。真实实现位于 `internal/edgeagent/collector/embedded.go` 的 gopsutil 路径。

## 2. 包信息

- **包名**：`collector`
- **所属模块**：edgeagent biz 下的 metric 采集子模块（Phase 1 桩）
- **依赖方向**：被同包其他文件可能引用；调用 `model`

## 3. 关键类型与接口

无类型定义。

## 4. 关键函数与流程

### `CollectCPU`
- **签名**：`func CollectCPU(ctx context.Context) (model.HostMetric, error)`
- **职责**：声明从 `/proc/loadavg` + `/proc/stat` 采样的契约
- **流程**：当前实现 `_ = ctx` 忽略 ctx，直接返回 `(model.HostMetric{}, nil)`
- **错误处理**：永远返回 nil

## 5. 依赖关系

- **内部包**：`internal/edgeagent/model`
- **外部库**：标准库 `context`
- **被调用方**：本文件目前没有外部调用方（Phase 1 桩未被 wire）；真实采集走 `internal/edgeagent/collector/embedded.go`

## 6. 并发与资源管理

无并发控制。函数无状态、无 IO。

## 7. 设计模式与亮点

无特殊设计模式。Phase 1 桩模式：保留契约签名让上层可编译运行，真实实现下沉到另一 BC（`internal/edgeagent/collector`）。

## 8. 注意事项

- 该文件位于 `biz/collector/`，与 `internal/edgeagent/collector/`（无 `biz` 前缀）是两个不同包；真实采集走后者，前者保留为历史桩
- 移除前需确认无外部引用；目前 `_ = ctx` 表明未被实际调用
- 若要启用此实现，需补 `/proc/loadavg` + `/proc/stat` 解析逻辑，或直接删除并迁移调用方到 `internal/edgeagent/collector`
