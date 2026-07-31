# `client.go` 技术实现文档

> 源文件：`internal/pkg/promwrite/client.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/promwrite`

## 1. 概述

本文件实现 Prometheus remote_write 协议的客户端：将 `[]Sample` 通过 POST `/api/v1/write` 推送到 Prometheus（或兼容 TSDB），请求体为 snappy 压缩的 protobuf。客户端通过 `EndpointResolver` 支持静态与动态两种端点解析方式，便于运维 UI 修改配置后无需重启即可生效。

## 2. 包信息

- **包名**：`promwrite`
- **所属模块**：`internal/pkg/`（基础工具层，无 manager 业务依赖）
- **依赖方向**：被 manager 侧 biz 包装（如 metrics 推送器）调用；调用 `github.com/golang/snappy` 与标准库 `net/http`、`log/slog`

## 3. 关键类型与接口

```go
type Label struct {
    Name  string
    Value string
}

type Sample struct {
    Labels []Label
    Value  float64
    TsMs   int64
}

// 动态端点解析接口：每次 Write 调用都询问一次，实现方应自行缓存
type EndpointResolver interface {
    ResolveWriteURL(ctx context.Context) (string, error)
}

type Client struct {
    endpoint   EndpointResolver
    httpClient *http.Client
    log        *slog.Logger
}
```

`staticEndpoint` 与 `staticBase` 是两种内置的 `EndpointResolver` 实现：前者适配精确 URL，后者在 base URL 后拼接 `/api/v1/write`。

## 4. 关键函数与流程

### `New / NewWithWriteURL / NewWithHTTPClient / NewWithWriteURLAndHTTPClient / NewWithResolverAndHTTPClient`
- **签名**：多种构造函数变体，最终汇入私有 `newClient`
- **职责**：根据传入形式（base URL / 精确 URL / resolver）和 `http.Client`、logger 组装 `Client`
- **流程**：`newClient` 对 nil logger / nil httpClient 给出默认值（`slog.Default()` 与 10s 超时客户端）
- **错误处理**：构造期不返回错误；URL 错误推迟到 Write 时

### `Client.Write`
- **签名**：`func (c *Client) Write(ctx context.Context, samples []Sample) error`
- **职责**：把 samples 编码为 snappy+protobuf 推送到 remote_write 端点
- **流程**：
  1. 空切片直接返回 nil（no-op）
  2. 解析 endpoint URL；空 URL 返回错误
  3. 每个 Sample 单独编码为一个 TimeSeries（一个 series 一个 sample），调用 `encodeTimeSeries`
  4. 用 `encodeWriteRequest` 聚合后 `snappy.Encode`
  5. 构造 POST 请求，设置 `Content-Type: application/x-protobuf`、`Content-Encoding: snappy`、`X-Prometheus-Remote-Write-Version: 0.1.0`、`User-Agent: ongrid-promwrite/0.1`
  6. 发送请求；`defer` 关闭 body
- **错误处理**：HTTP 200/204 视为成功；其他状态读取最多 1024 字节 body 拼入错误信息，并 Warn 日志；不做内部重试（与 prometheus/common 行为一致，留给调用方）

## 5. 依赖关系

- **内部包**：无（doc.go 明确强调 dependency-free）
- **外部库**：`github.com/golang/snappy`
- **被调用方**：manager 侧的 metrics 推送 biz 包装层

## 6. 并发与资源管理

`Client` 字段均为只读，安全用于并发调用。`http.Client` 由调用方决定是否共享。`context.Context` 透传到 `http.NewRequestWithContext`，可被 caller 取消或设置 deadline。`defaultTimeout = 10 * time.Second` 是单次 round-trip 上限（默认 client 注入时设置）。

## 7. 设计模式与亮点

- **依赖倒置**：通过 `EndpointResolver` 接口把"配置变更如何传播"留给上层，本包不感知 system_settings
- **多种构造函数**：保留 legacy `New` / `NewWithWriteURL` 不破坏调用方，新增 `NewWithResolverAndHTTPClient` 支持动态配置
- **wire 兼容性**：每个 Sample 独立成一个 TimeSeries，省去 manager 端按 label hash 分组的负担；Prom 接受任一形态
- **错误体有界**：非 2xx 时用 `io.LimitReader(resp.Body, 1024)` 限制错误体大小，防止 OOM

## 8. 注意事项

- **无重试**：调用方需自行实现 backoff；本客户端不感知临时性失败
- **每 sample 一 series**：当样本量大时 HTTP body 会膨胀；目前 manager 场景是 1 sample/series 的小批量，可接受
- **defaultTimeout 10s**：比 Prometheus 推荐的 30s 紧，因为本系统批量小；caller 可注入自定义 `http.Client` 覆盖
- **proto3 默认值**：`appendDouble` 注释提到对 `value == 0` 仍写出字段以保证 round-trip（Prom 容忍默认值省略，但这里更保守）
