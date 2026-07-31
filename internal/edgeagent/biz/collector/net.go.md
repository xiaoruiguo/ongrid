# `net.go` 技术实现文档

> 源文件：`internal/edgeagent/biz/collector/net.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/biz/collector`

## 1. 概述

Phase 1 占位实现：`CollectNet` 函数声明从 `/proc/net/dev` 采样网络吞吐的契约，当前直接返回零值 `model.HostMetric{}`。真实实现位于 `internal/edgeagent/collector/embedded.go` 的 gopsutil 路径。

## 2. 包信息

- **包名**：`collector`
- **所属模块**：edgeagent biz 下的 metric 采集子模块（Phase 1 桩）
- **依赖方向**：被同包其他文件可能引用；调用 `model`

## 3. 关键类型与接口

无类型定义。

## 4. 关键函数与流程

### `CollectNet`
- **签名**：`func CollectNet(ctx context.Context) (model.HostMetric, error)`
- **职责**：声明从 `/proc/net/dev` 采样的契约
- **流程**：当前实现 `_ = ctx` 忽略 ctx，直接返回 `(model.HostMetric{}, nil)`
- **错误处理**：永远返回 nil

## 5. 依赖关系

- **内部包**：`internal/edgeagent/model`
- **外部库**：标准库 `context`
- **被调用方**：本文件目前没有外部调用方；真实采集走 `internal/edgeagent/collector/embedded.go`

## 6. 并发与资源管理

无并发控制。函数无状态、无 IO。

## 7. 设计模式与亮点

无特殊设计模式。Phase 1 桩模式，与同包 `cpu.go` / `mem.go` 一致。

## 8. 注意事项

- 与 `cpu.go` / `mem.go` 同样的双包问题
- 启用前需补 `/proc/net/dev` 解析；当前 `_ = ctx` 暗示未被实际调用
- 三个 Collect 函数均接受 `context.Context` 但未使用——是 Phase 1 接口前瞻，便于未来实现 IO 时遵守「所有 IO 函数首参 ctx」的项目约定
