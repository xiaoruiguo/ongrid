# `probe_http.go` 技术实现文档

> 源文件：`internal/skill/builtin/probe_http.go`
> 包路径：`github.com/ongridio/ongrid/internal/skill/builtin`

## 1. 概述

`probe_http.go` 实现 `host_probe_http` skill：对目标 URL 发起 HEAD/GET 请求，返回状态码 + 延迟 + 内容长度。属于只读 safe 类 skill，跑在 edge 上。TLS 验证故意跳过，因 edge 常探测自签名内部服务。

## 2. 包信息

- **包名**：`builtin`
- **所属模块**：`internal/skill/builtin`（内置 skill 实现层）
- **依赖方向**：被 `builtin` 包 `init()` 自注册；依赖 `internal/skill` 框架类型

## 3. 关键类型与接口

```go
// skill 实现，无状态
type ProbeHTTP struct{}

// 输入参数
type probeHTTPParams struct {
    URL       string `json:"url"`
    Method    string `json:"method"`
    TimeoutMS int    `json:"timeout_ms"`
}

// 输出结果
type probeHTTPResult struct {
    StatusCode    int    `json:"status_code"`
    LatencyMS     int64  `json:"latency_ms"`
    ContentLength int64  `json:"content_length"`
    Error         string `json:"error,omitempty"`
}
```

## 4. 关键函数与流程

### `init()`
- **签名**：`func init() { skill.Register(&ProbeHTTP{}) }`
- **职责**：自注册到全局 Registry。

### `ProbeHTTP.Metadata`
- **签名**：`func (ProbeHTTP) Metadata() skill.Metadata`
- **职责**：返回元数据。Key=`host_probe_http`，Class=`ClassSafe`，Category=`network`。
- **参数**：`url`（必填 string）、`method`（enum，默认 HEAD，限 GET/HEAD）、`timeout_ms`（int，默认 5000）。
- **Scope**：零值 = `ScopeHost`。

### `ProbeHTTP.Execute`
- **签名**：`func (ProbeHTTP) Execute(ctx context.Context, params json.RawMessage) (json.RawMessage, error)`
- **职责**：发起单次 HTTP 请求，返回状态码/延迟/内容长度。
- **流程**：
  1. 解码 params（空 params 跳过）；
  2. `url` 非空校验；
  3. `method` 校验：空 → HEAD；非 GET/HEAD → Go error；
  4. `timeout_ms <= 0` → 默认 5000；
  5. 构造 `http.Client`：`Timeout: timeout`，`Transport` 带 `InsecureSkipVerify: true`；
  6. `http.NewRequestWithContext(ctx, method, url, nil)`：失败 → `{Error: ...}` JSON；
  7. `start := time.Now()`；
  8. `client.Do(req)`：失败 → `{LatencyMS, Error}` JSON；
  9. `defer resp.Body.Close()`；
  10. `method == "GET"` → `io.Copy(io.Discard, resp.Body)` 数实际字节；HEAD → 用 `resp.ContentLength`；
  11. `res.LatencyMS = time.Since(start).Milliseconds()`；
  12. `json.Marshal(res)` 返回。
- **错误处理**：method 非法返回 Go error；请求构造/执行失败返回带 `Error` 的 JSON；HTTP 4xx/5xx 不视为 error，正常返回状态码。

## 5. 依赖关系

- **内部包**：`github.com/ongridio/ongrid/internal/skill`
- **外部库**：`context`、`crypto/tls`、`encoding/json`、`fmt`、`io`、`net/http`、`time`
- **被调用方**：通过 `skill.Registry` 被 `internal/edgeagent` 调度派发（ScopeHost）

## 6. 并发与资源管理

- **`http.Client.Timeout`** 限制整个请求（含连接 + TLS + body 读取）时长。
- **`http.NewRequestWithContext`** 让父 ctx 取消能传播到请求。
- **`defer resp.Body.Close()`** 确保连接归还连接池。
- `ProbeHTTP` 无状态，每次 `Execute` 独立构造 `http.Client`，无共享状态，并发安全。

## 7. 设计模式与亮点

- **GET 数实际字节，HEAD 用 header**：GET 时 `Content-Length` header 可能缺失/错误，用 `io.Copy(io.Discard, body)` 数真实字节；HEAD 无 body，用 header。
- **TLS 跳过验证**：edge 探测内部自签名服务是常见场景，`InsecureSkipVerify: true` 是有意为之；文档注释明确说明。
- **4xx/5xx 不报 error**：HTTP 错误码是合法响应，正常返回 `StatusCode`，让 LLM 区分"网络不通"与"服务返回 4xx"。
- **错误进结果而非 Go error**：请求失败时返回 `{Error: ...}` JSON，保持审计一致。
- **method 白名单**：仅允许 GET/HEAD（只读），PUT/POST 等修改性方法被拒绝，与 `ClassSafe` 语义一致。

## 8. 注意事项

- **`InsecureSkipVerify: true` 是安全取舍**：探测内部服务必需，但若 url 指向公网恶意站点，存在中间人风险；建议仅在 edge 内网环境使用。
- **无重定向控制**：默认 `http.Client` 跟随重定向（最多 10 次），若需禁止需自定义 `CheckRedirect`；当前实现未暴露该参数。
- **GET body 全量读入丢弃**：`io.Copy(io.Discard, resp.Body)` 会读取完整 body，大文件场景会消耗带宽与内存（虽丢弃）；可考虑加 `MaxBytesReader` 限制。
- **`http.Client` 每次新建**：未复用连接池，频繁调用同 host 会有 TLS 握手开销；当前实现优先简洁，未做连接复用。
- **`timeout_ms` 上限未校验**：用户可传巨大值，受父 ctx 约束；建议上层做范围校验。
- **`method` 大小写敏感**：仅允许大写 `"GET"`/`"HEAD"`，小写会被拒绝；LLM 输入需对齐。
- **HEAD 的 `ContentLength` 可能为 -1**：服务端不返回 `Content-Length` header 时 `resp.ContentLength == -1`，调用方需处理。
