# `client.go` 技术实现文档（mcpclient）

> 源文件：`internal/pkg/mcpclient/client.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/mcpclient`

## 1. 概述

该文件实现 Model Context Protocol (MCP) 客户端的最小切片（HLD-018）：基于 Streamable HTTP 传输，覆盖 `initialize` → `tools/list` → `tools/call` 三步流程。JSON-RPC 2.0 帧；服务器响应可以是 `application/json` 或 `text/event-stream` SSE，客户端同时处理。无外部依赖——协议表面小而稳定。Session id 通过 `Mcp-Session-Id` header 在 initialize 后捕获并在后续调用回显。

## 2. 包信息

- **包名**：`mcpclient`
- **所属模块**：`internal/pkg/`（基础设施层）
- **依赖方向**：被 manager MCP 集成 BC 调用；仅依赖标准库。

## 3. 关键类型与接口

### `Client`
绑定单一 server endpoint 的 MCP 客户端。

```go
type Client struct {
    endpoint  string
    headers   map[string]string // 静态 auth headers（credential 注入）
    http      *http.Client
    sessionID string // initialize 捕获，后续回显
    nextID    int
}
```

### `Tool`
`tools/list` 单项；`InputSchema` 为 raw JSON Schema，直接传给 LLM 作为 tool 参数。

```go
type Tool struct {
    Name        string
    Description string
    InputSchema json.RawMessage
}
```

### `CallResult` / `ContentBlock`
`tools/call` 返回；`Content` 是 content block 列表，`TextContent()` 把 text block 用换行拼接为 agent 友好字符串。

```go
type CallResult struct {
    Content []ContentBlock
    IsError bool
}
type ContentBlock struct {
    Type string
    Text string
}
```

## 4. 关键函数与流程

### `NewHTTP`
- **签名**：`func NewHTTP(endpoint string, headers map[string]string, timeout time.Duration) *Client`
- **职责**：构造 Streamable-HTTP MCP 客户端。timeout <= 0 → 默认 30s。

### `Initialize`
- **签名**：`func (c *Client) Initialize(ctx context.Context) error`
- **职责**：执行 MCP 握手 + 必需的 initialized 通知。
- **流程**：
  1. `c.call(ctx, "initialize", {protocolVersion: "2024-11-05", capabilities: {}, clientInfo: {name: "ongrid", version: "1"}})`。
  2. `c.notify(ctx, "notifications/initialized", {})`（notification 无 id 无响应）。

### `ListTools`
- **签名**：`func (c *Client) ListTools(ctx context.Context) ([]Tool, error)`
- **流程**：`c.call(ctx, "tools/list", {})` → 反序列化 `{tools: []}`。

### `CallTool`
- **签名**：`func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (*CallResult, error)`
- **流程**：`c.call(ctx, "tools/call", {name, arguments})` → 反序列化 `CallResult`。args nil 替换为空 map。

### `TextContent`
- **签名**：`func (r *CallResult) TextContent() string`
- **职责**：把所有 text block 用换行拼接；空 text 用 `[type]` 占位。

### `call`（私有）
- **签名**：`func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error)`
- **流程**：
  1. `c.nextID++` 自增。
  2. body = `{jsonrpc: "2.0", id, method, params}`。
  3. `c.post(ctx, body)` 返回 `respBytes, sessionID`。
  4. sessionID 非空 → 更新 `c.sessionID`。
  5. 反序列化 `{result, error}`；`error != nil` → error 含 code + message。
  6. 返回 `result`。
- **错误处理**：decode 失败 → error 含 method 名。

### `notify`（私有）
- **签名**：`func (c *Client) notify(ctx context.Context, method string, params any) error`
- **职责**：发送 notification（无 id），忽略响应 body。
- **流程**：body = `{jsonrpc, method, params}`（无 id）；`c.post` 忽略返回 body。

### `post`（私有）
- **签名**：`func (c *Client) post(ctx context.Context, body map[string]any) ([]byte, string, error)`
- **职责**：发送一个 JSON-RPC 消息，返回解码后的单响应（SSE 时 unwrap）+ Mcp-Session-Id header。
- **流程**：
  1. `json.Marshal(body)`。
  2. `http.NewRequestWithContext(ctx, POST, endpoint, body)`。
  3. 设置 `Content-Type: application/json` / `Accept: application/json, text/event-stream`。
  4. 注入 `c.headers`（credential-derived auth）。
  5. `c.sessionID != ""` → 设置 `Mcp-Session-Id` header 回显。
  6. `c.http.Do(req)`；`defer resp.Body.Close()`。
  7. 捕获 `Mcp-Session-Id` header。
  8. 202 Accepted → 返回空 JSON `{"jsonrpc":"2.0"}`（notification 响应）。
  9. 非 2xx → error 含 status + 截断 4KB body。
  10. `Content-Type` 含 `text/event-stream` → `firstSSEData` 解析。
  11. 否则 `io.ReadAll(LimitReader(8MB))`，空 body → 返回空 JSON。
- **错误处理**：每步 `%w` 包装并加 `mcp:` 前缀。

### `firstSSEData`（私有）
- **签名**：`func firstSSEData(r io.Reader) ([]byte, error)`
- **职责**：读 SSE 流，返回第一个含 JSON-RPC 响应（有 `result` 或 `error`）的 `data:` payload。
- **流程**：
  1. `bufio.Scanner` buffer 64KB 初始 / 8MB 最大。
  2. 维护 `data strings.Builder`，遇空行（event boundary）flush。
  3. flush：检查内容含 `"result"` 或 `"error"` → 返回；否则重置继续。
  4. `data:` 行追加（trim prefix + trim space）。
  5. 流结束 flush 一次。
  6. 无响应 → error `no JSON-RPC response in SSE stream`。

## 5. 依赖关系

- **内部包**：无。
- **外部库**：标准库 `bufio` / `bytes` / `context` / `encoding/json` / `fmt` / `io` / `net/http` / `strings` / `time`。
- **被调用方**：manager MCP 集成 BC（skill loader 把 MCP server 注册为 agent tool）。

## 6. 并发与资源管理

无显式锁。`Client` 字段中 `nextID` 与 `sessionID` 可变——并发调用 `call` 会导致 `nextID` 竞态。**当前实现假设单 goroutine 串行调用**（initialize 后 ListTools / CallTool 顺序）。多 goroutine 并发需调用方加锁或包装。

## 7. 设计模式与亮点

- **零外部依赖**：MCP 协议表面小，纯标准库实现，避免引入 JSON-RPC 库。
- **双响应形态处理**：`post` 同时处理 `application/json` 与 `text/event-stream`，覆盖 MCP 服务器的两种响应模式。
- **Session 自动捕获**：`initialize` 响应的 `Mcp-Session-Id` header 自动捕获并在后续调用回显，调用方无感。
- **notification 区分**：`notify` 不带 id，对应 MCP 的 notification 语义；202 Accepted 视为成功。
- **SSE 流式 unwrap**：`firstSSEData` 在 SSE 流中找含 `result` / `error` 的 data 事件，跳过 ping / progress 等非响应事件。
- **8MB body 上限**：`LimitReader` 防 OOM。
- **InputSchema 透传**：`Tool.InputSchema` 保留 raw JSON，直接传给 LLM 作为 tool 参数定义，避免客户端重新建模。
- **`TextContent` 友好降级**：非 text block 用 `[type]` 占位，保证 agent 拿到非空字符串。

## 8. 注意事项

- **非线程安全**：`nextID` 与 `sessionID` 无锁，并发调用会竞态；当前假设单 goroutine 使用。
- **无重试**：单次 HTTP 失败即返回，调用方需自实现重试。
- **SSE 仅取首个响应**：`firstSSEData` 找到第一个含 result/error 的 data 就返回，忽略后续事件（如 progress 通知）；若需 progress 回调需扩展。
- **`ProtocolVersion` 硬编码**：`"2024-11-05"` 写死；MCP 协议演进需同步更新。
- **sessionID 失效无处理**：服务器若重置 session（重启），客户端 sessionID 失效，后续调用 4xx；需调用方重新 Initialize。
- **8MB body 上限**：超大 tool 响应（如返回大文件）可能被截断；需评估提升或分页。
- **timeout 全局**：`http.Client.Timeout` 覆盖整个请求包括 SSE 流读取；长 SSE 流可能超时。
- **resources / prompts / OAuth / stdio 未实现**：MVP 范围外，未来在 `pkg/runner` subprocess 路径补齐。
