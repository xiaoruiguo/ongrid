# `doc.go` 技术实现文档

> 源文件：`internal/edgeagent/biz/collector/doc.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/biz/collector`

## 1. 概述

Go 包级文档文件，描述 `biz/collector` 包的职责：从 `/proc` 和 `/sys` 采集主机指标；每个文件暴露一个 `Collect` 函数返回对应的 model 类型。Phase 1 返回零值让 agent 主体可被 exercise。

## 2. 包信息

- **包名**：`collector`
- **所属模块**：edgeagent biz 下的 metric 采集子模块（Phase 1 桩）
- **依赖方向**：被同 biz 包其他代码可能引用

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

无特殊设计模式。Phase 1 桩包的文档说明，强调「返回零值让 agent 主体逻辑可被独立 exercise」的设计意图。

## 8. 注意事项

- 注释明确说明 Phase 1 返回零值；若调用方误以为有真实数据会导致下游零值告警
- 真实采集在 `internal/edgeagent/collector/embedded.go`（无 `biz` 前缀）；这两个包不应混淆
- 修改包注释时保持与同包三个 Collect 函数的实际行为一致
