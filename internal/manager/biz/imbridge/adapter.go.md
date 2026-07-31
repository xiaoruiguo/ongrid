# `adapter.go` 技术实现文档

> 源文件：`internal/manager/biz/imbridge/adapter.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/imbridge`

## 1. 概述

本文件是 imbridge 包与现有 aiops.Service 之间的适配层。它把 bridge 定义的窄接口 `AgentSession`（仅 `EnsureSession` + `StreamMessage` 两个方法）适配到 service 层的 `*svcaiops.Service`（`CreateSession` + `PostMessageStreamWithOpts`），并负责解析集群默认 LLM provider + model，使 IM 路径与 SPA picker 选用同一 LLM。核心约束：两个方法均以固定 "service account" user 身份认证（per-IM-user binding 落地前的临时方案）；resolver 失败软回退到 agent 层硬编码默认（gpt-5.4）而非报错，避免 IM 因 LLM 配置问题完全不可用。

## 2. 包信息

- **包名**：`imbridge`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 `bridge.go`（作为 `AgentSession` 注入 `Bridge`）调用；依赖 `internal/manager/service/aiops`、`internal/manager/biz/aiops/agent`、`internal/pkg/llm`、`internal/manager/biz/setting.LLMSettingsResolver`（实现 `LLMDefaultProvider`）

## 3. 关键类型与接口

```go
// LLMDefaultProvider 返回集群默认 LLM provider id + model。
// 实现方：managerbizsetting.LLMSettingsResolver —— 同一个 resolver
// 也注入 multi-provider LLM router（见 main.go llmSettingsResolver），
// 使 IM 与 HTTP 路径收敛于 "default model" 的单一真相源。
type LLMDefaultProvider interface {
    ResolveProviders(ctx context.Context) ([]llm.ProviderConfig, string, error)
}

// AiopsServiceAdapter 把 imbridge 的 AgentSession 接口适配到 aiops.Service。
// 两个方法均以固定 service account user 身份认证，直到 per-IM-user binding 落地。
type AiopsServiceAdapter struct {
    svc           *svcaiops.Service
    serviceUserID uint64
    defaults      LLMDefaultProvider // 可选；nil 时回退 agent.RunOptions{}
    log           *slog.Logger
}
```

**红线（注释明示）**：若不传 `LLMDefaultProvider`，IM 流量会走 `agent.New` 的 `"" → "gpt-5.4"` 硬编码 fallback，而该 model 不在 catalog 中，LLM router 会以 "this model has beta-limitations..." 拒绝，最终在 chat app 里表现为 "助手执行失败"。因此生产部署必须注入 `LLMSettingsResolver`。

## 4. 关键函数与流程

### `NewAiopsAdapter`
- **签名**：`func NewAiopsAdapter(svc *svcaiops.Service, serviceUserID uint64, defaults LLMDefaultProvider, log *slog.Logger) *AiopsServiceAdapter`
- **职责**：构造 adapter；defaults 可选（nil 走 agent fallback）；log nil → `slog.Default()`
- **流程**：log nil 检查 → 返回 `&AiopsServiceAdapter{svc, serviceUserID, defaults, log.With("comp", "imbridge.adapter")}`

### `caller`
- **签名**：`func (a *AiopsServiceAdapter) caller() svcaiops.Caller`
- **职责**：构造 service 层 Caller；Role 留空（IM 路径不做 admin gating，只做 ownership 检查）
- **流程**：返回 `svcaiops.Caller{UserID: a.serviceUserID}`

### `EnsureSession`
- **签名**：`func (a *AiopsServiceAdapter) EnsureSession(ctx context.Context, ownerUserID uint64, label string) (string, error)`
- **职责**：为每个 inbound thread 创建新 session；**不按 label 去重**——bridge 已通过 `im_threads` 表 memoise，duplicate 仅发生在 manager 重启后首次，可接受
- **流程**：
  1. `caller := a.caller()`（serviceUserID）
  2. `ownerUserID != 0` → 覆盖 `caller.UserID = ownerUserID`（预留 per-IM-user binding 钩子；当前 bridge 总传 `b.serviceUserID`，故 0 分支不触发）
  3. `a.svc.CreateSession(ctx, caller, svcaiops.CreateSessionInput{Title: label})`
  4. 失败 → `fmt.Errorf("imbridge adapter: create session: %w", err)`
  5. 返回 `sess.ID`
- **错误处理**：CreateSession 失败 `%w` 包装；label 直接作为 session Title（如 "feishu · abcd…wxyz"）

### `StreamMessage`
- **签名**：`func (a *AiopsServiceAdapter) StreamMessage(ctx context.Context, sessionID string, userContent string, emit agent.Emit) error`
- **职责**：把 user content 投递到 session，转发每个 `agent.Event` 给 emit
- **流程**：
  1. `opts := a.runOptions(ctx)` 解析集群默认 provider+model
  2. `a.svc.PostMessageStreamWithOpts(ctx, a.caller(), sessionID, userContent, emit, opts)`
  3. 返回 err（不包装——service 层错误已足够清晰）
- **同步语义**：agent loop 在 caller goroutine 同步运行；bridge 从自己的 goroutine 调用（webhook handler 立即返回 200）
- **错误处理**：直接返回 service 层 err；caller 决定是否 `editor.OnFatal(err)`

### `runOptions`
- **签名**：`func (a *AiopsServiceAdapter) runOptions(ctx context.Context) agent.RunOptions`
- **职责**：解析集群默认 provider+model，使 IM 流量与 SPA picker 一致；任何 resolver 错误或 catalog 缺失返回零 RunOptions，让 agent 层 fallback
- **流程**：
  1. `a.defaults == nil` → 返回 `agent.RunOptions{}`（agent.New 的硬编码 gpt-5.4 接管）
  2. `providers, defID, err := a.defaults.ResolveProviders(ctx)`
  3. `err != nil || len(providers) == 0` → err 非 nil 时 Warn "llm resolver failed; using agent fallback model"；返回零 RunOptions
  4. **默认 provider 优先**：`defID != ""` 时遍历 providers 找 `ID == defID`；找不到 fall through
  5. **fall back 到首项**：`pick == nil` → `pick = &providers[0]`（catalog 字母序，与 SPA 显示顺序一致）
  6. 返回 `agent.RunOptions{Provider: pick.ID, Model: pick.Model}`
- **错误处理**：resolver 失败 Warn log + 零 RunOptions；配置的 default provider 被移除时静默回退首项，不报错
- **设计意图**：注释明示 "configured default points at a provider that was removed" 也回退首项——避免 IM 因配置漂移完全不可用

## 5. 依赖关系

- **内部包**：
  - `internal/manager/service/aiops`（`Service`、`Caller`、`CreateSessionInput`、`PostMessageStreamWithOpts`）
  - `internal/manager/biz/aiops/agent`（`Emit` 类型、`RunOptions`）
  - `internal/pkg/llm`（`ProviderConfig`：ID + Model + ...）
- **实现方**：`internal/manager/biz/setting.LLMSettingsResolver` 实现 `LLMDefaultProvider`（同一个 resolver 也注入 multi-provider LLM router，见 main.go `llmSettingsResolver`）
- **被调用方**：`bridge.go` 的 `NewBridge(repo, agent AgentSession, ...)` 接收 `*AiopsServiceAdapter` 作为 `agent` 参数

## 6. 并发与资源管理

- **无共享可变状态**：`AiopsServiceAdapter` 字段全部启动期注入、运行期只读；`caller()` 每次构造值类型 `svcaiops.Caller`，无锁安全
- **同步执行**：`StreamMessage` 在 caller goroutine 同步运行 agent loop；bridge 从自己的 goroutine 调用，webhook handler 立即返回 200
- **ctx 透传**：所有外部调用带 ctx；agent loop 内部的 LLM 调用、tool 执行均受 ctx 控制
- **无 goroutine**：本文件不 spawn goroutine；并发由 bridge 调用方管理
- **`agent.Emit` 回调**：`StreamMessage` 的 `emit` 由 bridge 构造（`newStreamEditor` 的 `OnEvent`），在 agent loop 内同步调用；editor 内部有 mutex + 节流

## 7. 设计模式与亮点

- **窄接口适配**：`AgentSession`（bridge.go 定义）仅 2 方法，`AiopsServiceAdapter` 实现之；bridge 不依赖 `*svcaiops.Service` 具体类型，可测试性高（fake agent）
- **接口在消费方定义**：`LLMDefaultProvider` 在 imbridge 包定义（消费方），`LLMSettingsResolver` 实现之——遵循 gospec "接口在消费方定义" 原则，避免 service 反向依赖 imbridge
- **单一真相源**：注释明示 `LLMSettingsResolver` 同时注入 multi-provider LLM router 与本 adapter，IM 与 HTTP 路径收敛于同一 "default model" 决策，避免漂移
- **软回退策略**：resolver 失败 / catalog 空 / default provider 被移除 → 全部静默回退到 agent 层硬编码或 catalog 首项，**绝不报错**——IM 可用性优先于配置一致性
- **`caller()` 抽象**：集中构造 `svcaiops.Caller`，便于未来添加 role/tenant 等字段；当前 Role 留空，注释明示 "admin gating doesn't apply on the IM path"
- **`ownerUserID != 0` 分支**：预留 per-IM-user binding 钩子；当前 bridge 总传 `b.serviceUserID`，该分支不触发，但接口已留好
- **Warn 而非 Error**：resolver 失败仅 Warn，因为 agent fallback 仍能工作；不污染 ERROR 日志
- **`log.With("comp", "imbridge.adapter")`**：组件 tag 便于在日志中区分 adapter 与 bridge 本身的日志
- **不按 label 去重 session**：注释明示 bridge 已通过 `im_threads` 表 memoise，adapter 层重复去重是冗余；duplicate 仅发生在 manager 重启后首次，可接受

## 8. 注意事项

- **必须注入 `LLMDefaultProvider`**：注释明示 nil 时走 `agent.New` 的 `gpt-5.4` 硬编码 fallback，该 model 不在 catalog，LLM router 拒绝（"beta-limitations"），IM 表现为 "助手执行失败"；生产部署必须传 `LLMSettingsResolver`
- **service account 认证**：当前所有 IM session 以 `serviceUserID` 创建，**所有 IM 用户共享同一 owner**；per-IM-user binding（S3）落地后需改 `caller.UserID` 逻辑
- **`ownerUserID != 0` 分支预留**：当前 bridge 传 `b.serviceUserID`（非 0），故 `caller.UserID` 不会被覆盖；S3 落地时 bridge 应传真实 IM user 映射的 ongrid user id
- **Role 留空**：`caller()` 不设 Role，service 层仅用 `caller.UserID` 做 ownership 检查；admin gating 不适用于 IM 路径
- **`StreamMessage` 不包装错误**：直接返回 service 层 err；caller（bridge）决定是否 `editor.OnFatal(err)` + 报错
- **`runOptions` 软回退**：resolver 失败 / catalog 空 / default 被移除 → 零 RunOptions 或 catalog 首项；IM 可用性优先
- **catalog 字母序**：`providers[0]` 是字母序首项，与 SPA picker 显示顺序一致；操作员改默认时两端一致
- **`agent.RunOptions{Provider, Model}`**：仅传 provider id + model；其他 agent 选项（temperature、max_tokens 等）走 agent 层默认
- **同步 agent loop**：`StreamMessage` 在 caller goroutine 同步运行；bridge 必须从自己的 goroutine 调用，否则会阻塞 webhook handler（platform ack deadline 3s，agent run 30s+）
- **`emit` 回调在 agent loop 内同步调用**：editor 的 mutex + 节流（800ms / 200 字符）在 agent loop 内生效；若 agent loop 不释放 goroutine，editor 不会过期
- **`a.defaults` 可选**：测试时可传 nil 走 agent fallback；生产必须传 `LLMSettingsResolver`
