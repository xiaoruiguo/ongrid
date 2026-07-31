# `host_netns_inspect.go` 技术实现文档

> 源文件：`internal/skill/builtin/host_netns_inspect.go`
> 包路径：`github.com/ongridio/ongrid/internal/skill/builtin`

## 1. 概述

`host_netns_inspect.go` 实现 `host_netns_inspect` skill：列出 Linux network namespace 并对每个 ns 报告 IP 地址/路由/接口状态。它填补了 `host_bash` 沙箱无法表达 `ip -n <ns>` 语法的盲区（全局选项 `-n` 偏移 argv 导致 cmdpolicy 匹配失败），通过内部构造 argv 调用 `ip -j` 拿到结构化 JSON，供 LLM 直接消费。

## 2. 包信息

- **包名**：`builtin`
- **所属模块**：`internal/skill/builtin`（内置 skill 实现层）
- **依赖方向**：被 `builtin` 包 `init()` 自注册到 `skill.Registry`；依赖 `internal/skill` 框架类型

## 3. 关键类型与接口

```go
// skill 实现，无状态
type HostNetnsInspect struct{}

// 输入参数（IncludeRoutes 用 *bool 区分 unset 与 false）
type netnsParams struct {
    Namespace     string `json:"namespace"`
    IncludeRoutes *bool  `json:"include_routes"`
    IncludeLinks  bool   `json:"include_links"`
}

// 单个 ns 的结果记录
type netnsRecord struct {
    Name   string       `json:"name"`
    Addrs  []netnsAddr  `json:"addrs,omitempty"`
    Routes []netnsRoute `json:"routes,omitempty"`
    Links  []netnsLink  `json:"links,omitempty"`
    Error  string       `json:"error,omitempty"`
}

// 完整结果
type netnsResult struct {
    Namespaces []netnsRecord `json:"namespaces"`
    Error      string        `json:"error,omitempty"`
}

// namespace 名字白名单正则
var nameRE = regexp.MustCompile(`^[a-zA-Z0-9_.\-]{1,64}$`)
```

## 4. 关键函数与流程

### `init()`
- **签名**：`func init() { skill.Register(&HostNetnsInspect{}) }`
- **职责**：自注册到全局 Registry。

### `HostNetnsInspect.Metadata`
- **签名**：`func (HostNetnsInspect) Metadata() skill.Metadata`
- **职责**：返回元数据。Key=`host_netns_inspect`，Class=`ClassSafe`，Scope=`ScopeHost`，Category=`network`。
- **参数**：`namespace`（可选 string，留空=列全部）、`include_routes`（bool，默认 true）、`include_links`（bool，默认 false）。

### `HostNetnsInspect.Execute`
- **签名**：`func (HostNetnsInspect) Execute(ctx context.Context, params json.RawMessage) (json.RawMessage, error)`
- **职责**：列出/筛选 ns，对每个 ns 读 addr/route/link，返回结构化 JSON。
- **流程**：
  1. 解码 params（空 params 跳过）；
  2. **安全边界**：`namespace != ""` 时先 `validateNetnsName`（即使非 linux 也跑，让 darwin 测试覆盖拒绝路径）；
  3. `runtime.GOOS != "linux"` → 返回 `{Error: "only linux supported"}`（不报 Go error，让 LLM 看到原因）；
  4. `include_routes` 默认 true，`*bool` 区分 unset 与 false；
  5. `context.WithTimeout(ctx, 15s)`；
  6. `namespace` 空 → `listNetns` 列出全部；非空 → 单元素 slice；
  7. 遍历 ns：`readAddrs` → 失败记 `rec.Error` 并 append；成功则按 `include_routes`/`include_links` 调用对应 reader；
  8. `json.Marshal(result)` 返回。
- **错误处理**：参数解码失败返回 Go error；ns 列举失败返回 `{Error: ...}` JSON；单 ns 失败记 `rec.Error` 但继续其他 ns；所有错误均带前缀 `host_netns_inspect:` 便于审计。

### `listNetns`
- **签名**：`func listNetns(ctx context.Context) ([]string, error)`
- **职责**：解析 `ip netns list` 文本输出。
- **流程**：`exec.CommandContext("ip", "netns", "list").Output()` → 容忍 ExitError（无 ns 时常见）→ 按行 split → 取每行首字段 → `validateNetnsName` 过滤 → 返回。
- **错误处理**：ExitError 且 stdout 空 → 视为"无 ns"返回 error；其他错误 `%w` 包装。

### `readAddrs`
- **签名**：`func readAddrs(ctx context.Context, ns string) ([]netnsAddr, error)`
- **职责**：跑 `ip -j -n <ns> addr show` 解析 JSON。
- **流程**：`exec.CommandContext` → 解码 `[{ifname, addr_info:[{family, local, prefixlen}]}]` → 展平为 `[]netnsAddr`。

### `readRoutes`
- **签名**：`func readRoutes(ctx context.Context, ns string) ([]netnsRoute, error)`
- **职责**：跑 `ip -j -n <ns> route show` 解析 JSON。
- **流程**：解码 `[{dst, gateway, dev}]` → dst 空时填 `"default"` → 转换为 `[]netnsRoute`。

### `readLinks`
- **签名**：`func readLinks(ctx context.Context, ns string) ([]netnsLink, error)`
- **职责**：跑 `ip -j -n <ns> link show` 解析 JSON。
- **流程**：解码 `[{ifname, operstate, address}]` → 转换为 `[]netnsLink`。

### `validateNetnsName`
- **签名**：`func validateNetnsName(s string) error`
- **职责**：用 `nameRE` 校验 ns 名（`[a-zA-Z0-9_.-]{1,64}`）。
- **设计意图**：即便 os/exec 不走 shell，也拒绝 shell 元字符，双保险防注入。

## 5. 依赖关系

- **内部包**：`github.com/ongridio/ongrid/internal/skill`
- **外部库**：`context`、`encoding/json`、`errors`、`fmt`、`os/exec`、`regexp`、`runtime`、`strings`、`time`
- **被调用方**：通过 `skill.Registry` 被 `internal/edgeagent` 调度派发（ScopeHost，跑在 edge 上）

## 6. 并发与资源管理

- **`context.WithTimeout(ctx, 15s)`** 限制整体执行时长，防 ip 命令卡住 dispatcher。
- 每次 `Execute` 独立构造 `exec.Cmd`，无跨调用共享状态，天然并发安全。
- 无显式锁；`HostNetnsInspect` 是无状态值类型，多 goroutine 并发调用安全。

## 7. 设计模式与亮点

- **填空白模式**：bash 沙箱无法表达 `ip -n <ns>` 语法（全局选项偏移 argv），本 skill 内部构造 argv 绕过沙箱限制，同时仍走 os/exec 无 shell 的安全路径。
- **结构化 JSON 优先**：用 `ip -j` 拿 JSON 而非解析文本，避免 iproute2 不同版本输出格式差异，LLM 可直接消费。
- **`*bool` 区分 unset 与 false**：`IncludeRoutes *bool` 让"用户未传"与"用户显式传 false"语义分离，配合 `Metadata` 中 `Default: true` 的 UI 表单初始值。
- **per-ns 容错**：单 ns 失败记 `rec.Error` 继续其他 ns，避免一个坏 ns 拖垮整次探查。
- **安全边界前置**：`validateNetnsName` 在 OS 检查之前执行，让非 linux 平台的测试也能覆盖拒绝路径。
- **默认路由 dst 兜底**：iproute2 对默认路由省略 `dst` 字段，本 skill 主动填 `"default"` 让结果 schema 一致。

## 8. 注意事项

- **仅 Linux 支持**：`runtime.GOOS != "linux"` 时返回结构化 error JSON，不报 Go error；调用方应在 edge（Linux）上调度。
- **namespace 名字注入防御**：即便用 os/exec（无 shell），仍用正则白名单拒绝 shell 元字符，双保险；正则限制 1-64 字符匹配 Linux NAME_MAX 约定。
- **`ip -j` 需要 iproute2 较新版本**：老旧系统（如 CentOS 7 默认 iproute2）可能不支持 `-j`，会返回非零退出 + 空 stdout，被 `readAddrs` 等函数捕获为 error。
- **15s 超时硬编码**：多 ns 场景下每个 ns 串行执行，ns 数量多时可能超时；当前未做并行化，因 ip 命令本身很快。
- **`listNetns` 容忍 ExitError**：无 ns 时 `ip netns list` 退出码非 0 但 stdout 空，本函数将其视为 error 返回（让上层返回 `{Error: ...}`），调用方需处理。
- **`include_links` 默认 false**：多 ns 时 link 数据量大，默认不带回；用户可显式传 true 获取 MAC/state 信息。
- **结果 JSON 用 `omitempty`**：空 slice 字段被省略，减少 LLM token 消耗；调用方不应假设字段必存在。
