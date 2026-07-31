# `config_tools.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/config_tools.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools`

## 1. 概述

本文件实现两个 BaseTool：`draft_config_change`（创建只读配置 draft，验证后返回 draft_hash）和 `apply_config_change`（apply 已确认 draft）。v1 仅支持 `domain=alert_rule, action=create`。关键红线：draft 不持久化业务配置；apply 必须 `confirmed=true` + admin role + payload + draft_hash + draft_id 校验；draft_hash 用 sha256(action+rule+draftID) 防篡改；输出 scrub 敏感字段。

## 2. 包信息

- **包名**：`tools`
- **所属模块**：`internal/manager/biz/aiops/tools`
- **依赖方向**：被 Registry 注册和 agent loop 调用；依赖 `basetool`、`alertdraft`、`errs`、`tenantctx`

## 3. 关键类型与接口

```go
const (
    ToolNameDraftConfigChange = "draft_config_change"
    ToolNameApplyConfigChange = "apply_config_change"
    ConfigDomainAlertRule            = "alert_rule"
    ConfigResultKindDraft            = "config_draft"
    ConfigResultKindApply            = "config_apply_result"
    ConfigResultKindValidationFailed = "config_validation_failed"
)

type ConfigCaller struct {
    UserID      uint64
    Role        string
    IsSuperuser bool
}

type ConfigManager interface {
    DraftAlertRuleConfig(ctx, caller ConfigCaller, in AlertRuleConfigArgs) (*ConfigDraft, error)
    ApplyAlertRuleConfig(ctx, caller ConfigCaller, in AlertRuleApplyArgs) (*ConfigApplyResult, error)
}

type ConfigTool struct {
    kind    configToolKind  // "draft_change" | "apply_change"
    manager ConfigManager
    log     *slog.Logger
}

type ConfigDraft struct {
    Kind, Domain, Action, Summary string
    Target *ConfigTarget
    Payload, Preview, Diff json.RawMessage
    Validation *ConfigValidationResult
    Warnings []string
    Scope *ConfigScopeSummary
    ConfirmationPrompt, Rollback, ApplyTool, DraftHash string
}

type ConfigApplyResult struct {
    Kind, Domain, Action, Status, Message, Rollback string
    ResourceID uint64
    Resource *ConfigTarget
}

type AlertRuleConfigInput = alertdraft.RuleConfigInput  // type alias
```

## 4. 关键函数与流程

### `NewDraftConfigChangeTool / NewApplyConfigChangeTool`
- 构造对应 kind 的 ConfigTool；log nil → slog.Default()

### `Info`
- draft：Class="read"，提示"call list_metric_catalog first"、"only config_draft is confirmable"、"after one successful draft, stop tool calls"
- apply：Class="write"，提示 "MUTATING. Requires confirmed=true, admin caller, domain=alert_rule, action=create, payload from draft"

### `InvokableRun`
- **签名**：`func (t *ConfigTool) InvokableRun(ctx, argsJSON string, opts ...basetool.InvokeOption) (string, error)`
- **流程**：
  1. manager nil → error
  2. `ResolveOptions(opts)` + `configCallerFromContext(ctx, opts)` 构造 ConfigCaller（带 tenant role/superuser）
  3. **draft 分支**：unmarshal ConfigChangeDraftArgs；RequestText 空时填 resolved.UserText；draftConfigChange → marshalConfigToolResult
  4. **apply 分支**：unmarshal ConfigChangeApplyArgs；`validateApplyGate(caller, Confirmed)` → `applyPayloadDefaults` → `validateDraftHash` → applyConfigChange → marshalConfigToolResult

### `draftConfigChange`
- **流程**：normalizeConfigDomain（仅 alert_rule）；normalizeConfigCreateAction（仅 create）；`isZeroAlertRuleInput(Rule)` → ErrInvalid；调 manager.DraftAlertRuleConfig

### `applyConfigChange`
- **流程**：normalizeConfigDomain + action；调 manager.ApplyAlertRuleConfig 传入 Rule/DraftID/DraftHash/Confirmed/ConfirmationText

### `configCallerFromContext`
- **流程**：ResolveOptions 取 UserID；tenantctx.From 取 Role/IsSuperuser 覆盖

### `validateApplyGate`
- **流程**：!Confirmed → ErrInvalid "confirmed=true required"；Role != "admin" && !IsSuperuser → ErrForbidden "admin role required"

### `applyPayloadDefaults`
- **流程**：Payload 空 → ErrInvalid；unmarshal payload 取 Action/DraftID/Rule/DraftHash 填回 args；Rule 空 → ErrInvalid

### `validateDraftHash`
- **流程**：DraftHash 空 → ErrInvalid；`AlertRuleConfigDraftHashForID(Action, Rule, DraftID)` 算期望 hash；不匹配（EqualFold）→ ErrInvalid "draft_hash does not match"

### `AlertRuleConfigDraftPayload / AlertRuleConfigDraftHash / AlertRuleConfigDraftHashForID`
- **职责**：构造 canonical draft payload + 算 sha256 hash
- **流程**：marshal `{draft_id, action, rule}` → `sha256:hex` 前缀

### `marshalConfigToolResult`
- **流程**：marshal v → unmarshal 成 `interface{}` → `scrubConfigSecrets` 递归替换敏感字段为 `******` → remarsh

### `scrubConfigSecrets / isSensitiveConfigKey`
- 递归 map/slice；命中 secret/app_secret/verify_token/encrypt_key/token/password/api_key/apikey 的 string 字段替换为 `******`

## 5. 依赖关系

- **内部包**：`basetool`、`alertdraft`（RuleConfigInput 类型 alias）、`errs`（ErrInvalid/ErrForbidden）、`tenantctx`
- **外部库**：标准库 `crypto/sha256`、`encoding/hex`、`encoding/json`、`fmt`、`log/slog`、`strings`
- **被调用方**：Registry 注册；ConfigManager 由 alertdraft biz 实现

## 6. 并发与资源管理

- ConfigTool immutable，多 goroutine 共享安全
- 无锁、无 channel

## 7. 设计模式与亮点

- **draft/apply 两阶段**：draft 只读验证返回 draft_hash；apply 用 hash 证明 payload 未被篡改——LLM 无法绕过 draft 直接 apply
- **consumer-owned seam**：ConfigManager 接口在 tools 包定义，concrete manager 实现
- **draft_hash 防篡改**：sha256(action+rule+draftID) 让 LLM 无法伪造 payload——apply 时重算 hash 比对
- **admin-only apply**：apply 强制 admin role / superuser；draft 任何用户可调
- **scrub 敏感字段**：marshal 后递归扫描替换 secret/token/password 等字段为 `******`，防 LLM 看到凭据
- **v1 domain/action 收窄**：仅 alert_rule + create；未来扩展需新增枚举
- **RequestText 透传**：draft 透传用户原始请求文本，backend 用它验证 scope label（log level/unit、显式 database source intent）
- **isZeroAlertRuleInput 全字段检查**：避免空 Rule 进 draft 流程

## 8. 注意事项

- **v1 仅 alert_rule/create**：domain 必须 alert_rule（或 alert/alert_rule_config 归一化）；action 必须 create（或空归一化）
- **draft_hash 必须 match**：apply 时 draft_hash 与重算的期望 hash 不匹配（EqualFold）拒绝
- **payload 是 source of truth**：apply 的 action/rule/draft_id/draft_hash 从 payload 取，top-level 字段是副本
- **Confirmed 必须 true**：apply 前用户显式确认
- **admin role 强制**：非 admin 非 superuser 拒绝 apply
- **lookback_seconds 60..604800**：draft preview 窗口钳位
- **scrub 不区分嵌套深度**：递归扫描所有层级，可能误伤同名业务字段（如 rule.key="token"）
- **deterministic approval**：apply_config_change + alert_rule/create 被 review_gate 装饰器确定性放行（自身已校验）
