# restart_service_basetool.go

## 1. 概述

本文件实现 `host_restart_service`——ongrid 第一个 MUTATING-class BaseTool（Class="write"，SOP double-sign）。是 `ReviewGate` decorator（`decorators/review_gate.go`）正确拦截 mutating tool calls 的 proof：LLLM 发 restart_service call → ReviewGate 看到 Class=="write" → spawn reviewer worker（`agents/reviewer.md`）→ 只在 "Decision: approve" 时本 BaseTool 才 dispatch 到 edge。

**Class 选择**：用 "write" 而非 "destructive"。"destructive" 留给不可逆操作（rm、drop table、reboot）。service restart 是可逆的（`systemctl start` 拉回）。blast radius 是"single device"。ReviewGate decorator 对两个 class 同样拦截，所以选择是 documentation-only，但为 `kill_process` / `drop_silence` 等 PR-N follow-ups 设 precedent。

**为何不走 skill_bridge**：

- skill_bridge 把每个 safe-class skill 包在一个 wire method 后；mutating skills 需要专门 wire method（`MethodRestartService`）让 audit log 看到 typed call name，让 edge handler 重新验证 unit allow-list 而不解析 generic params blob
- reviewer flow 需要稳定 BaseTool name + Class 来 gate；skill_bridge-wrapped tool 都显示为 "execute_skill"，ReviewGate decorator 无法 class without parsing args
- 未来 SPA approval UI 会按 tool name 列 mutating proposals；typed wire 让 listing trivial

**defense in depth**：allow-list 在 manager（本文件）和 edge sandbox 都校验。manager reject 时 LLM 拿 fast clean error 不烧 tunnel round-trip；edge reject 时 manager's reviewer 已 approve stale config——两条路径都有意存在。

## 2. 包信息

- **包名**：`tools`
- **路径**：`internal/manager/biz/aiops/tools/restart_service_basetool.go`
- **导入**：
  - `basetool`
  - `devicebiz` / `edgebiz`
  - `tunnel`（`internal/pkg/tunnel`）—— `MethodRestartService` / `RestartServiceRequest/Response`
- **Class**：`write`（MUTATING）

## 3. 关键类型与接口

### 常量

```go
const ToolNameRestartService = "host_restart_service"  // 等于 skill key in internal/skill/builtin/restart_service
const restartServiceCallTimeout = 30 * time.Second
```

### `AllowedRestartServices`（exported）

```go
var AllowedRestartServices = []string{
    "nginx", "redis", "prometheus", "loki", "tempo", "grafana", "mysql", "ongrid",
}
```

canonical allow-list，mirrored from `skills/restart-service/SKILL.md` 和 edge sandbox。exported 让 reviewer / SPA approval UI 渲染同一列表而不重复 literal。

### `allowedRestartServicesSet`

lookup form，`init()` 时 compute 一次，让 `InvokableRun` O(1) per check。

注意：`init()` 仅做 map 构建注册，无 IO，符合 AGENTS.md 的 `init()` 规则。

### `restartServiceArgs`

```go
type restartServiceArgs struct {
    DeviceID uint64 `json:"device_id"`
    Service  string `json:"service"`  // systemd unit short name (no .service suffix)
    Reason   string `json:"reason"`   // 进 audit row，鼓励 post-mortem trail
}
```

### `restartServiceResultEnvelope`

```go
type restartServiceResultEnvelope struct {
    DeviceID  uint64    `json:"device_id"`
    Service   string    `json:"service"`
    Restarted bool      `json:"restarted"`
    Mocked    bool      `json:"mocked"`  // edge handler 是否 mocked
    StartedAt time.Time `json:"started_at"`
    EndedAt   time.Time `json:"ended_at"`
    Error     string    `json:"error,omitempty"`
}
```

### `RestartServiceTool`

```go
type RestartServiceTool struct {
    caller   Caller
    resolver hostFilesDeviceResolver  // 复用 host_files 的 resolver interface
    log      *slog.Logger
}
```

注释明示：本工具**不**自己 spawn reviewer worker——ReviewGate decorator wraps 本工具并负责 spawn。`RestartServiceTool` 只在 approved path 运行；到达 `InvokableRun` 时 reviewer 已 yes（或 wrap misconfigured）。

## 4. 关键函数与流程

### `NewRestartServiceTool(c, e, d, log)`

构造器。`log == nil` → `slog.Default()`。`resolver = deviceResolverAdapter{inner: NewDeviceResolver(d, e)}`——复用 host_files 的 DeviceResolver + adapter。

### `restartServiceWhenToUse`（常量）

英文 LLM-facing 文案，关键指令：

- **USE ONLY** when user explicitly asks to restart one of allow-listed services
- **DO NOT proactively suggest restart**——diagnose first with query_logql / get_edge_summary
- **MUTATING**：call spawns reviewer worker（SOP gating）；on reject, do NOT retry—convey reviewer's reason verbatim

注释明示"DO NOT propose restart"是最重要的 bit：runaway agent 不应该 suggest restart-as-a-fix；user 应该先 ask。reviewer 是第二道防线，explicit guidance 是第一道。

### `Info(_ context.Context)`

返回 `ToolInfo`：Name=`host_restart_service`，Class=`write`，含 WhenToUse。

### `InvokableRun(ctx, argsJSON, _ ...)`

**重要前置**：到达此方法时 ReviewGate decorator 已 approve。Production wiring 必须 wrap 本工具 in ReviewGate（chain.go's Wrap 默认如此）。test 直接调此方法 bypass review——unit test 可以，production 不行。

流程：

1. 校验 `caller` 非 nil。
2. Unmarshal `restartServiceArgs`。
3. `DeviceID == 0` → error。
4. **service 归一化**：`strings.TrimSpace` + `strings.ToLower` + `strings.TrimSuffix(canonical, ".service")`——让 LLM 写 `NGINX.service` 或 `nginx ` 都能匹配。
5. **allow-list 校验**：`allowedRestartServicesSet[canonical]` 不存在 → error（带完整 allow-list 提示）。
6. **device_id → edge_id 解析**：`resolver.LookupHostEdge(ctx, DeviceID)`。
   - `err != nil` → error
   - `edgeID == 0` → error（带 "try query_devices to list available device ids" 提示）
7. 构造 `tunnel.RestartServiceRequest{Service: canonical, Reason}`，Marshal body。
8. `context.WithTimeout(ctx, 30s)`，调 `caller.Call(callCtx, edgeID, tunnel.MethodRestartService, body)`。
9. Unmarshal `tunnel.RestartServiceResponse`，包成 `restartServiceResultEnvelope`，Marshal 返回。

### `AppendRestartServiceTool(out, c, e, d, log)`

注册器。任一 dep nil 返回 out 不变（graceful degradation）。caller 负责 wrap decorator chain（chain.go's Wrap），自动 apply ReviewGate when Class="write"|"destructive"。

## 5. 依赖关系

| 方向 | 依赖 | 说明 |
|------|------|------|
| 上游 | `basetool.BaseTool` / `ToolInfo` | 实现 BaseTool 接口 |
| 上游 | `decorators.ReviewGate`（chain.go Wrap） | 拦截 + spawn reviewer |
| 下游 | `Caller.Call`（tunnel） | dispatch to edge |
| 下游 | `hostFilesDeviceResolver`（DeviceResolver adapter） | device_id → edge_id |
| 类型 | `tunnel.RestartServiceRequest/Response` | wire 协议 |
| 共享 | `AllowedRestartServices` 与 edge sandbox / SKILL.md 同步 | defense in depth |

## 6. 并发与资源管理

- `context.WithTimeout(ctx, 30s)` per call——mock handler 立即返回；预算给真实 systemctl shell-out 留空间。
- `allowedRestartServicesSet` 在 `init()` 一次性构建，运行期只读，并发安全。
- 无共享可变状态。

## 7. 设计模式与亮点

- **SOP double-sign via ReviewGate**：本工具不自己 spawn reviewer，而是依赖 `ReviewGate` decorator 拦截——这是装饰器链模式的核心：业务工具纯净，gating 逻辑在 decorator。
- **Class="write" vs "destructive"**：注释详细解释为何用 "write"——restart 可逆，destructive 留给不可逆操作。为后续 mutating tools 设 precedent。
- **defense in depth allow-list**：manager 和 edge sandbox 都校验——manager reject 省 tunnel round-trip，edge reject 防 stale config。两条路径都有意存在。
- **service 归一化**：trim + lower + trim `.service` suffix——让 LLM 写 `NGINX.service` 或 `nginx ` 都能匹配，降低 LLM 出错率。
- **`Reason` 字段进 audit row**：鼓励 post-mortem trail，reviewer 看到的 reason 与 audit 一致。
- **`Mocked` 字段 in envelope**：edge handler 是否 mocked——让 LLM 知道这是真重启还是 mock，便于测试场景判断。
- **`AllowedRestartServices` exported**：reviewer / SPA approval UI 渲染同一列表不重复 literal——single source of truth。
- **`init()` 仅做 map 构建**：符合 AGENTS.md 的 `init()` 规则（仅注册，无 IO，无 panic）。
- **`AppendRestartServiceTool` Gating pattern**：任一 dep nil 返回 out 不变——与 `AppendHostFilesTools` 同模式，graceful degradation。
- **不复用 skill_bridge 的 rationale**：注释详细解释为何走专门 BaseTool 而非 skill_bridge——typed wire name 让 audit 看到 `MethodRestartService`，reviewer flow 需要稳定 Class，SPA UI 按 tool name 列 proposals。

## 8. 注意事项

- **Class="write" 是 documentation-only**：ReviewGate 对 write/destructive 同样拦截，选择不影响行为——但为后续工具设 precedent，要慎重。
- **`init()` 构建 map**：`allowedRestartServicesSet` 在 init 构建，若未来 allow-list 从 DB / config 读，需要改运行期构建——目前是 compile-time constant。
- **allow-list 同步**：`AllowedRestartServices` 必须与 edge sandbox / SKILL.md 同步——drift 会让 manager approve 但 edge reject（或反之）。无编译期保证。
- **30s 超时**：mock 立即返回，真实 systemctl 可能慢（service stop + start）；30s 给 systemctl 留空间但不算宽。
- **`Reason` 不强制**：args 里 `reason` 可空，LLM 可能省略——audit trail 会有空 reason，post-mortem 时信息不全。
- **`device_id` 必填**：与 host_files batch 协议（`device_ids[]`）不同，这个工具是 single-device（restart 一台机器上的 service）——无 batch 协议。
- **reviewer reject 后不重试**：WhenToUse 明示"on reject, do NOT retry—convey reviewer's reason verbatim"——LLM 应该把 reason 转给 user，不要试图改 args 重试。
- **`AppendRestartServiceTool` 不 wrap decorator**：注释明示 caller 负责 wrap decorator chain——若 caller 忘记 wrap，本工具会无 gating 直接执行，这是 production wiring risk。
