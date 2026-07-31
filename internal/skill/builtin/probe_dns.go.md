# `probe_dns.go` 技术实现文档

> 源文件：`internal/skill/builtin/probe_dns.go`
> 包路径：`github.com/ongridio/ongrid/internal/skill/builtin`

## 1. 概述

`probe_dns.go` 实现 `host_probe_dns` skill：通过系统 resolver 解析主机名为 A/AAAA 地址，返回地址列表 + 延迟。属于只读、无副作用的 safe 类 skill，跑在 edge 上，是网络排障的基础工具之一。

## 2. 包信息

- **包名**：`builtin`
- **所属模块**：`internal/skill/builtin`（内置 skill 实现层）
- **依赖方向**：被 `builtin` 包 `init()` 自注册；依赖 `internal/skill` 框架类型

## 3. 关键类型与接口

```go
// skill 实现，无状态
type ProbeDNS struct{}

// 输入参数
type probeDNSParams struct {
    Host      string `json:"host"`
    TimeoutMS int    `json:"timeout_ms"`
}

// 输出结果
type probeDNSResult struct {
    Addrs     []string `json:"addrs"`
    LatencyMS int64    `json:"latency_ms"`
    Error     string   `json:"error,omitempty"`
}
```

## 4. 关键函数与流程

### `init()`
- **签名**：`func init() { skill.Register(&ProbeDNS{}) }`
- **职责**：自注册到全局 Registry。

### `ProbeDNS.Metadata`
- **签名**：`func (ProbeDNS) Metadata() skill.Metadata`
- **职责**：返回元数据。Key=`host_probe_dns`，Class=`ClassSafe`，Category=`network`。
- **参数**：`host`（必填 string）、`timeout_ms`（int，默认 3000）。
- **Scope**：零值 = `ScopeHost`（跑 edge）。

### `ProbeDNS.Execute`
- **签名**：`func (ProbeDNS) Execute(ctx context.Context, params json.RawMessage) (json.RawMessage, error)`
- **职责**：调用系统 resolver 解析 host，返回地址列表 + 延迟。
- **流程**：
  1. 解码 params（空 params 跳过）；
  2. `host` 非空校验，空则返回 Go error `probe_dns: host required`；
  3. `timeout_ms <= 0` → 默认 3000；
  4. 初始化 `res := probeDNSResult{Addrs: []string{}}`（避免 null）；
  5. `context.WithTimeout(ctx, timeout)`；
  6. `start := time.Now()`；
  7. `net.DefaultResolver.LookupIPAddr(cctx, p.Host)`；
  8. `res.LatencyMS = time.Since(start).Milliseconds()`；
  9. 出错 → `res.Error = err.Error()`，`json.Marshal(res)` 返回（不报 Go error）；
  10. 成功 → 遍历 ips 填 `Addrs`，`json.Marshal(res)` 返回。
- **错误处理**：参数解码/缺失返回 Go error；解析失败返回带 `Error` 字段的 JSON，保持审计一致。

## 5. 依赖关系

- **内部包**：`github.com/ongridio/ongrid/internal/skill`
- **外部库**：`context`、`encoding/json`、`fmt`、`net`、`time`
- **被调用方**：通过 `skill.Registry` 被 `internal/edgeagent` 调度派发（ScopeHost）

## 6. 并发与资源管理

- **`context.WithTimeout`** 限制 resolver 调用时长，防慢解析器卡住 dispatcher goroutine。
- `net.DefaultResolver` 是 goroutine 安全的全局实例，并发调用安全。
- `ProbeDNS` 无状态，多 goroutine 并发调用安全。

## 7. 设计模式与亮点

- **错误进结果而非 Go error**：解析失败时返回 `{Error: ...}` JSON 而非 Go error，让审计日志与 LLM 上下文保持"skill 总有结果"的一致语义。
- **延迟测量内置**：`start`/`time.Since` 直接测解析耗时，运维排障无需额外工具。
- **`Addrs` 初始化为空 slice**：避免 JSON 序列化出 `null`，调用方解析时无需 nil 检查。
- **零值默认值**：`timeout_ms <= 0` 时默认 3000，UI 表单与 API 调用都能省略该参数。

## 8. 注意事项

- **使用系统 resolver**：`net.DefaultResolver` 走系统 DNS 配置（`/etc/resolv.conf`），不直连 DNS 服务器；若 edge 的 resolver 配置异常，结果不可信。
- **无 cache**：每次调用都重新解析，频繁调用同一 host 会重复请求；上层可自行缓存。
- **`LatencyMS` 含 ctx 开销**：测量从 `LookupIPAddr` 调用到返回的耗时，包含 resolver 内部的重试/超时逻辑。
- **返回 A+AAAA 混合**：`LookupIPAddr` 同时返回 IPv4 与 IPv6，调用方需自行区分（当前结果未带 family 字段）。
- **`timeout_ms` 上限未校验**：用户可传巨大值，受 `ctx` 父级 deadline 约束；建议上层对 LLM 输入做范围校验。
- **空 params 兼容**：`len(params) > 0` 判断让 `params` 为 `nil`/`{}`/`""` 时都能跳过解码，UI 表单空提交不会报错。
