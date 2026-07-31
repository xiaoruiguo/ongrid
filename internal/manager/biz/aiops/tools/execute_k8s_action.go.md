# `execute_k8s_action.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/execute_k8s_action.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools`

## 1. 概述

本文件实现 `execute_k8s_action` 工具：通过集群 controller edge 执行**经过审计的 Kubernetes 写操作**（rollout_restart / scale / delete_pod / evict_pod / cordon / uncordon / drain）。**MUTATING**——`Class="write"`，触发 Reviewer 审批，强制 **dry-run preflight → 真实写** 两步流程：第一次调用必须 `dry_run=true`，controller 返回 `preflight_token` + `expected_resource_version`；第二次调用带上该 token 才能落地。Token 5min TTL、一次性、绑定 user+session+fingerprint（sha256(action+rule+draftID 思路的同源设计）。Admin-only 鉴权。30s 调用超时（drain 视 `drain_timeout_seconds` 动态加 15s slack）。

## 2. 包信息

- **包名**：`tools`
- **所属模块**：`internal/manager/biz/aiops/tools`
- **依赖方向**：被 Registry 注册与闭包路径调用；依赖 `basetool`、`pkg/errs`（`ErrForbidden`）、`pkg/tenantctx`（caller 提取）、`pkg/tunnel`（`KubernetesActionRequest/Response`、`MethodExecuteK8sAction`）、`Caller`、`K8sSnapshotReader`、`crypto/rand` + `crypto/sha256` + `encoding/base64`。

## 3. 关键类型与接口

```go
type ExecuteK8sActionTool struct {
    caller Caller
    reader K8sSnapshotReader
    log    *slog.Logger

    preflightMu sync.Mutex
    preflights  map[string]executeK8sActionPreflightGrant // token → grant
}

// 一次性 preflight 凭证。
type executeK8sActionPreflightGrant struct {
    Fingerprint [sha256.Size]byte // sha256(请求体)，确保 dry-run 与真实写参数一致
    UserID      uint64
    SessionID   string
    ExpiresAt   time.Time         // 5min TTL
}

type ExecuteK8sActionArgs struct {
    ClusterID               uint64
    Action                  string  // rollout_restart|scale|delete_pod|evict_pod|cordon|uncordon|drain
    Kind, APIVersion, Namespace, Name string
    Replicas                *int     // scale 必填，[0,10000]
    ExpectedResourceVersion string   // dry-run 返回后必填
    DryRun                  bool
    PreflightToken          string   // dry_run=false 时必填
    Reason                  string
    GracePeriodSeconds      *int     // [0,3600]
    DrainTimeoutSeconds     int      // [1,600]，默认 120
    DrainRetrySeconds       int      // [1,30]，默认 2
    IgnoreDaemonSets        *bool    // drain 默认 true
    DeleteEmptyDirData, Force, DisableEviction bool // drain 风险开关
}

type executeK8sActionResponse struct {
    Source, ControllerEdgeID, Result tunnel.KubernetesActionResponse
    PreflightToken, PreflightExpiresAt, ExpectedResourceVersion string
}
```

## 4. 关键函数与流程

```go
func (t *ExecuteK8sActionTool) InvokableRun(ctx, argsJSON, _ ...InvokeOption) (string, error)
func (t *ExecuteK8sActionTool) issuePreflight(req, userID, sessionID) (token string, expiresAt time.Time, err error)
func (t *ExecuteK8sActionTool) consumePreflight(token, req, userID, sessionID) error
func (t *ExecuteK8sActionTool) deleteExpiredPreflightsLocked(now) // 加速 GC
func executeK8sActionFingerprint(req) ([sha256.Size]byte, error)  // sha256(marshal(req))
func validateExecuteK8sActionResponse(req, resp) error             // 防止 controller 返回的目标错位
func normalizeExecuteK8sActionArgs(in) (tunnel.KubernetesActionRequest, error)
func executeK8sActionTimeout(req) time.Duration                    // drain 动态加 15s slack
func normalizeExecuteK8sActionName(action) (string, error)         // 容错大小写/下划线/连字符
func actionRequiresExplicitKind(action) bool                       // rollout_restart|scale 必须 kind
```

**`InvokableRun` 主流程**：
1. **Admin 鉴权**：`tenantctx.From(ctx)` 取 caller，非 admin 且非 superuser → `errs.ErrForbidden`。
2. 守门检查 `caller` / `reader` 注入。
3. Unmarshal + `normalizeExecuteK8sActionArgs`：cluster_id/action/name 必填；action 容错（`restart`/`rollout-restart` 都归一为 `rollout_restart`）；`rollout_restart`/`scale` 必须显式 kind；`delete_pod`/`evict_pod` 默认 kind=Pod；`cordon`/`uncordon`/`drain` 默认 kind=Node；`scale` 必须传 replicas ∈ [0,10000]；`grace_period_seconds` ∈ [0,3600]；drain 专属参数 `drain_timeout_seconds` ∈ [1,600] 默认 120、`drain_retry_seconds` ∈ [1,30] 默认 2、`ignore_daemonsets` 默认 true。
4. **非 dry-run 时消费 preflight**：`consumePreflight(token, req, userID, sessionID)`——token 为空 → 报错 "run dry_run=true first"；重新计算 fingerprint 与 grant 对比；token 一次性（命中即 `delete`）；过期 / user 不匹配 / session 不匹配 / fingerprint 不一致 → 报错。
5. `context.WithTimeout(ctx, executeK8sActionTimeout(req))`（drain 时为 `DrainTimeoutSeconds+15s`，否则 30s）。
6. `reader.GetCluster` + 校验 `ControllerEdgeID` 非空非零。
7. Marshal req → `caller.Call(callCtx, *cluster.ControllerEdgeID, tunnel.MethodExecuteK8sAction, body)`。
8. Unmarshal resp + `validateExecuteK8sActionResponse`（cluster_id/action/name/namespace 必须与 req 一致，preflight target 也必须匹配）。
9. **分支处理**：
   - `req.DryRun`：校验 resp 确实是 dry-run（`DryRun==true && !Applied && Preflight.Exists && ResourceVersion != ""`），否则报 "invalid dry-run preflight"；构造 `approved = req`，`approved.DryRun=false`，`approved.ExpectedResourceVersion = resp.Preflight.ResourceVersion`；`issuePreflight(approved, userID, sessionID)` 颁发 token；返回带 `preflight_token` / `preflight_expires_at` / `expected_resource_version` 的响应。
   - 非 dry-run：校验 `!resp.DryRun && resp.Applied`，否则报 "controller did not confirm write applied"。
10. Marshal `executeK8sActionResponse` 返回。

**`issuePreflight`**：sha256(marshal(req)) 算 fingerprint；`crypto/rand` 生成 32 字节 → base64.RawURL token；加锁 `deleteExpiredPreflightsLocked(now)` 顺手 GC；写入 map。TTL `executeK8sActionPreflightTTL = 5 * time.Minute`。

**`consumePreflight`**：token 空 → 报错；重算 fingerprint；加锁 → `delete(preflights, token)`（一次性，无论后续校验是否通过）→ `deleteExpiredPreflightsLocked` → 检查存在 / 未过期 / user / session / fingerprint 五项，任一不符报错。

## 5. 依赖关系

- **basetool**：`ToolInfo`（`Class="write"`）、`InvokeOption`（忽略）、`SessionIDFromContext(ctx)`（preflight 绑定 session）。
- **pkg/errs**：`ErrForbidden` 用于 admin 鉴权失败，让上层 graph/audit 能识别语义。
- **pkg/tenantctx**：`From(ctx)` 取 caller（含 UserID / Role / IsSuperuser）。
- **pkg/tunnel**：`KubernetesActionRequest/Response`、`MethodExecuteK8sAction`、`Preflight` 子结构。
- **Caller / K8sSnapshotReader**：包内窄接口，生产实现由 Registry 注入。
- 不依赖 alertbiz / edgebiz。

## 6. 并发与资源管理

- **preflight map 互斥访问**：`preflightMu sync.Mutex` 保护 `preflights map[string]grant`。`issuePreflight` / `consumePreflight` 都在锁内读写，`deleteExpiredPreflightsLocked` 在锁内 GC。
- **token 一次性**：`consumePreflight` 在校验前即 `delete(preflights, token)`，即便后续 fingerprint 比对失败，token 也已失效——防止 "试错式重放"。
- **GC 时机**：每次 `issuePreflight` 与 `consumePreflight` 都调 `deleteExpiredPreflightsLocked(now)`，无需后台 goroutine 定时清理。
- **无 ctx 超时泄漏**：`callCtx, cancel := context.WithTimeout(...)` + `defer cancel()`。
- **struct 并发安全**：`caller` / `reader` / `log` 不变，`preflights` 由 mutex 保护，`ExecuteK8sActionTool` 可被多 goroutine 共享调用。

## 7. 设计模式与亮点

- **Two-step propose-confirm（HLD-021）**：dry-run 颁发 token → 真实写消费 token，强制 LLM 分两轮调用。token 绑定 user+session+fingerprint，杜绝 "dry-run A 然后写 B" 的参数偷换。这与 `config_tools.go` 的 draft_hash 同源设计，是 ongrid 对所有 mutating 工具的红线。
- **Fingerprint 防 parameter drift**：`sha256(marshal(req))` 覆盖 req 所有字段，任何参数变化（replicas / namespace / kind / drain 选项）都会让 fingerprint 不匹配，强制重跑 dry-run。
- **Admin gate 在工具内强制**：不依赖装饰器，`InvokableRun` 第一行就 `tenantctx.From(ctx)` 校验 admin/superuser。即便装饰器漏配，工具自身仍守红线。
- **响应校验防 controller 错位**：`validateExecuteK8sActionResponse` 检查 resp 的 cluster_id/action/name/namespace 与 req 一致、preflight target 也匹配，防止 controller bug 把 action 派发到错误对象。
- **Drain 动态超时**：`executeK8sActionTimeout` 为 drain 加 `DrainTimeoutSeconds+15s` slack（PDB 重试需要时间），其他 action 固定 30s。
- **action 名归一化容错**：`normalizeExecuteK8sActionName` 接受 `restart` / `rollout-restart` / `rolloutrestart` 都归一为 `rollout_restart`，对 LLM 不规范输入友好。
- **Class=write + WhenToUse 警告**：明确 "MUTATING"、"Never use this for kubectl exec, Secret reads, arbitrary patches, or host service restarts"，引导 LLM 不滥用。

## 8. 注意事项

- **Reviewer 审批在装饰器层**：本工具 `Class="write"` 会触发 `review_gate` 装饰器 spawn reviewer worker（SOP double-sign），工具内不再做 reviewer 逻辑。两层防护：装饰器 reviewer + 工具内 dry-run preflight。
- **preflight map 是进程内内存**：重启即失效，所有进行中的 dry-run token 失效，LLM 需重跑 dry-run。多副本部署下 token 不跨实例共享（每个实例独立 map）——若 LLM dry-run 命中实例 A、真实写命中实例 B，token 会被判 "invalid"。当前部署假设 sticky session（`basetool.SessionIDFromContext` 也依赖此）。
- **`disable_eviction` 红线**：WhenToUse 明确 "avoid disable_eviction unless bypassing PDB is explicitly requested"——绕过 PDB 直接 delete，风险极高，需 operator 显式同意。
- **`force` 与 `delete_emptydir_data`**：drain 时若 Pod 用 emptyDir 或无 controller owner，必须显式设这些 flag，否则 controller 拒绝 drain。WhenToUse 提示 "set only when the user explicitly accepts that risk"。
- **fingerprint 包含 `DryRun` 字段**：`approved = req; approved.DryRun = false` 后重算 fingerprint，因此 grant 存的是 "approved 后的 fingerprint"，consume 时 req 也是 `DryRun=false`，两者匹配。若调用方在 dry-run 与真实写之间改了别的字段，fingerprint 不匹配即报错。
- **`executeK8sActionCallTimeout` 与 `executeK8sActionTimeout(req)`**：前者是常量 30s，后者是函数，drain 时动态扩展。`InvokableRun` 用后者。
- **token 用 `crypto/rand`**：非 `math/rand`，32 字节 base64.RawURL，足够防猜测。
