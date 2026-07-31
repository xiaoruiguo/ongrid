# `probe_tcp.go` 技术实现文档

> 源文件：`internal/skill/builtin/probe_tcp.go`
> 包路径：`github.com/ongridio/ongrid/internal/skill/builtin`

## 1. 概述

`probe_tcp.go` 实现 `host_probe_tcp` skill：对目标 `host:port` 发起 TCP 连接，返回连通状态 + 延迟。属于只读 safe 类 skill，跑在 edge 上，是网络排障最基础的工具——单次出站连接、立即关闭、不发任何 payload。

## 2. 包信息

- **包名**：`builtin`
- **所属模块**：`internal/skill/builtin`（内置 skill 实现层）
- **依赖方向**：被 `builtin` 包 `init()` 自注册；依赖 `internal/skill` 框架类型

## 3. 关键类型与接口

```go
// skill 实现，无状态
type ProbeTCP struct{}

// 输入参数
type probeTCPParams struct {
    Target    string `json:"target"`
    TimeoutMS int    `json:"timeout_ms"`
}

// 输出结果
type probeTCPResult struct {
    OK        bool   `json:"ok"`
    LatencyMS int64  `json:"latency_ms"`
    Error     string `json:"error,omitempty"`
}
```

## 4. 关键函数与流程

### `init()`
- **签名**：`func init() { skill.Register(&ProbeTCP{}) }`
- **职责**：自注册到全局 Registry。

### `ProbeTCP.Metadata`
- **签名**：`func (ProbeTCP) Metadata() skill.Metadata`
- **职责**：返回元数据。Key=`host_probe_tcp`，Class=`ClassSafe`，Category=`network`。
- **参数**：`target`（必填 string，`host:port` 形式）、`timeout_ms`（int，默认 3000）。
- **Scope**：零值 = `ScopeHost`。

### `ProbeTCP.Execute`
- **签名**：`func (ProbeTCP) Execute(ctx context.Context, params json.RawMessage) (json.RawMessage, error)`
- **职责**：对 target 发起 TCP 拨号，返回 OK + 延迟。
- **流程**：
  1. 解码 params（空 params 跳过）；
  2. `target` 非空校验，空则返回 Go error `probe_tcp: target required`；
  3. `timeout_ms <= 0` → 默认 3000；
  4. `timeout := time.Duration(p.TimeoutMS) * time.Millisecond`；
  5. `res := probeTCPResult{}`；
  6. `start := time.Now()`；
  7. `(&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", p.Target)`；
  8. `res.LatencyMS = time.Since(start).Milliseconds()`；
  9. 出错 → `res.OK = false`，`res.Error = err.Error()`；
  10. 成功 → `res.OK = true`，`_ = conn.Close()`；
  11. `json.Marshal(res)` 返回。
- **错误处理**：参数缺失返回 Go error；拨号失败返回 `{OK:false, Error:...}` JSON，保持审计一致。`conn.Close()` 错误被显式忽略（`_ =`），因连接已建立即可视为成功，关闭错误不影响判定。

## 5. 依赖关系

- **内部包**：`github.com/ongridio/ongrid/internal/skill`
- **外部库**：`context`、`encoding/json`、`fmt`、`net`、`time`
- **被调用方**：通过 `skill.Registry` 被 `internal/edgeagent` 调度派发（ScopeHost）

## 6. 并发与资源管理

- **`net.Dialer.Timeout`** 限制拨号时长，防目标不可达时卡住 dispatcher。
- **`DialContext`** 让父 ctx 取消能传播到拨号。
- **`conn.Close()`** 在 `defer` 外立即调用，确保文件描述符及时释放。
- `ProbeTCP` 无状态，并发调用安全。

## 7. 设计模式与亮点

- **错误进结果而非 Go error**：拨号失败返回 `{OK:false, Error:...}` JSON，让审计日志与 LLM 上下文保持"skill 总有结果"的一致语义。
- **延迟测量内置**：`start`/`time.Since` 直接测拨号耗时，运维排障无需额外工具。
- **单次连接立即关闭**：不发 payload，仅测连通性，对目标服务干扰最小。
- **`_ = conn.Close()` 显式忽略错误**：连接已建立即视为成功，关闭错误不影响判定；符合 AGENTS.md "禁止 `_ = fn()` 忽略错误（确实想丢弃必须注释说明）"——此处有隐式语义但无注释，是潜在改进点。

## 8. 注意事项

- **`target` 格式为 `host:port`**：未单独拆分 host/port 参数，调用方需保证格式正确；`net.Dialer` 会处理 DNS 解析。
- **无重试**：单次拨号，瞬时网络抖动可能导致 false negative；上层若需可靠性可自行重试。
- **`_ = conn.Close()` 缺少注释**：根据 AGENTS.md 规范，忽略错误应注释说明；此处虽语义合理（关闭错误不影响连通判定），但无注释，是代码规范的小瑕疵。
- **`timeout_ms` 上限未校验**：用户可传巨大值，受父 ctx 约束；建议上层做范围校验。
- **仅 TCP**：不支持 UDP/ICMP 探测；UDP 探测需另行实现（"connected" UDP socket 模式），ICMP 需 root 权限。
- **不区分拒绝与超时**：`Error` 字段直接放 `err.Error()`，调用方需自行解析字符串区分"connection refused"与"i/o timeout"；可考虑扩展结果字段。
- **空 params 兼容**：`len(params) > 0` 判断让空 params 跳过解码，UI 表单空提交不会报错。
