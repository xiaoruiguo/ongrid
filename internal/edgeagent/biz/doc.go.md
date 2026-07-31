# `doc.go` 技术实现文档

> 源文件：`internal/edgeagent/biz/doc.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/biz`

## 1. 概述

Go 包级文档文件，仅一行注释描述 `biz` 包的职责：edgeagent BC（business capability）的运行循环。声明包拥有隧道拨号、带指数退避的重连、心跳、周期指标采集 + 推送、优雅关闭五项职责。

## 2. 包信息

- **包名**：`biz`
- **所属模块**：edgeagent 顶层运行循环层
- **依赖方向**：被 `cmd/ongrid-edge` 构造；调用 `collector`、`skill`、`tunnel`

## 3. 关键类型与接口

无类型定义。仅是包注释。

## 4. 关键函数与流程

无函数。

## 5. 依赖关系

- **内部包**：无
- **外部库**：无
- **被调用方**：无（仅 Go doc 工具与 godoc 渲染消费）

## 6. 并发与资源管理

无并发控制。

## 7. 设计模式与亮点

无特殊设计模式。包注释为 Go 惯例，紧邻 `package biz` 声明，被 godoc / pkg.go.dev 作为包概述显示。

## 8. 注意事项

- 注释中提到的「exponential backoff reconnect」实际由 tunnel 层（`tunnel.Client.OnReconnect`）负责，agent 层仅注册回调；文档描述的职责边界在 `agent.go` 中已下沉到隧道层
- 修改包注释时需保持简短准确，godoc 会将其作为包首屏
