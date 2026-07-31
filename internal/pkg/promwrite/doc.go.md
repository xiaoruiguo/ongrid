# `doc.go` 技术实现文档

> 源文件：`internal/pkg/promwrite/doc.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/promwrite`

## 1. 概述

本文件仅为 `promwrite` 包的包级文档注释，不含可执行代码。说明该包是 Prometheus remote_write 协议的极简客户端，并解释为何手写 protobuf 编码而非引入 `github.com/prometheus/prometheus/prompb`。

## 2. 包信息

- **包名**：`promwrite`
- **所属模块**：`internal/pkg/`
- **依赖方向**：被 manager 侧 biz 包装与测试代码调用；本文件不引入任何依赖

## 3. 关键类型与接口

无显著类型定义（仅包注释）。

## 4. 关键函数与流程

无（纯文档文件）。

文档要点：
- **协议形态**：POST `/api/v1/write`，snappy 压缩的 protobuf body
- **手写 proto 原因**：引入 `github.com/prometheus/prometheus/prompb` 会因传递依赖把 Prometheus server 的大部分模块带入 go.mod，膨胀数百个间接依赖
- **schema 固定**：四个简单 message — `Label { name=1; value=2; }`、`Sample { value=1; timestamp=2; }`、`TimeSeries { repeated Label labels=1; repeated Sample samples=2; }`、`WriteRequest { repeated TimeSeries timeseries=1; }`
- **只编码不解码**：manager 只 push 不 read，因此 decode 路径省略
- **跨 BC 边界**：本包不 import 任何 `manager/*`，置于 `internal/pkg/` 便于 manager biz 包装与未来其他内部用户复用

## 5. 依赖关系

- **内部包**：无
- **外部库**：无（注释中提到 protobuf schema，但不引入 prompb）
- **被调用方**：N/A（仅文档）

## 6. 并发与资源管理

无并发控制（纯文档文件）。

## 7. 设计模式与亮点

- **决策留痕**：把"为什么不依赖 prometheus/prometheus"的架构取舍写进包注释，便于未来维护者评估是否切换为生成代码
- **schema 显式列出**：proto3 wire 文本在注释中明示，与 `proto.go` 的字段编号一一对应，便于对照排查 wire 问题

## 8. 注意事项

- 若未来需要支持 decode（例如读 remote_read 响应），需重新评估是否继续手写：decode 比 encode 复杂（需处理 packed repeated、未知字段跳过等）
- 若 Prometheus 上游 schema 演进（如增加 exemplars 字段），需同步更新 `proto.go` 编码器；当前 schema 稳定多年
