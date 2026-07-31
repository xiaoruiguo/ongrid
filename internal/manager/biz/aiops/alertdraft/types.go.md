# `types.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/alertdraft/types.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/alertdraft`

## 1. 概述

本文件定义 alertdraft 包的**公开 API 类型 + 入口函数**。`RuleConfigInput` 是 LLM 生成告警草案的统一输入形状；`CompileDraft` 是公开入口，串联 action 规范化 + 规则规范化 + summary 生成；`NormalizeRuleConfigInput`/`NormalizeRuleConfigInputForRequest`/`ShouldBlockCreateOnPreviewSkip`/`NormalizeConfigAction`/`Summary` 是对内部函数的公开包装（包级封装）。常量 `DefaultJournaldLogSelector`/`defaultAllLogsSelector` 是日志规则的默认 stream selector。

## 2. 包信息

- **包名**：`alertdraft`
- **所属模块**：`internal/manager/biz/aiops/alertdraft`
- **依赖方向**：被 `alertconfig/alert_rule_manager.go`、`chatruntime/config_confirm.go` 等外部包调用；内部依赖 `compiler.go` 的实现
- **公开 API**：`CompileDraft`、`NormalizeRuleConfigInput`、`NormalizeRuleConfigInputForRequest`、`ShouldBlockCreateOnPreviewSkip`、`NormalizeConfigAction`、`Summary`、`RuleConfigInput`、`RuleCondition`、`CompileInput`、`CompiledDraft`、`DefaultJournaldLogSelector`

## 3. 关键类型与接口

```go
const (
    defaultJournaldLogSelector = `{ongrid_source=~"journald(:.*)?"}`
    defaultAllLogsSelector     = `{ongrid_source=~".+"}`
    DefaultJournaldLogSelector = defaultJournaldLogSelector  // 公开导出
)

type RuleConfigInput struct {
    RuleKey             string                 `json:"rule_key,omitempty"`
    Kind                string                 `json:"kind,omitempty"`
    Name                string                 `json:"name,omitempty"`
    ScopeType           string                 `json:"scope_type,omitempty"`
    JoinMode            string                 `json:"join_mode,omitempty"`
    Window              string                 `json:"window,omitempty"`     // 顶层 alias
    For                 string                 `json:"for,omitempty"`        // 顶层 alias
    Severity            string                 `json:"severity,omitempty"`
    Enabled             *bool                  `json:"enabled,omitempty"`
    Conditions          []RuleCondition        `json:"conditions,omitempty"` // metric_threshold 用
    Spec                map[string]interface{} `json:"spec,omitempty"`       // 其他 kind 用
    Labels              map[string]string      `json:"labels,omitempty"`
    RunbookURL          string                 `json:"runbook_url,omitempty"`
    NotifyChannelIDs    []uint64               `json:"notify_channel_ids,omitempty"`
    NotifyWindowSeconds int                    `json:"notify_window_seconds,omitempty"`
    NotifyMinFires      int                    `json:"notify_min_fires,omitempty"`
}

type RuleCondition struct {
    Metric     string  `json:"metric"`
    Operator   string  `json:"operator"`
    Threshold  float64 `json:"threshold"`
    Window     string  `json:"window,omitempty"`
    For        string  `json:"for,omitempty"`
    Aggregator string  `json:"aggregator,omitempty"`
}

type CompileInput struct {
    Action      string
    Rule        RuleConfigInput
    RequestText string
}

type CompiledDraft struct {
    Action  string
    Rule    RuleConfigInput
    Summary string
}
```

## 4. 关键函数与流程

### `CompileDraft`
- **签名**：`func CompileDraft(in CompileInput) (CompiledDraft, error)`
- **职责**：公开入口；编译草案为可信形状
- **流程**：
  1. `NormalizeConfigAction(in.Action)` → action（v1 仅 create）；失败返回 error
  2. `NormalizeRuleConfigInputForRequest(in.Rule, in.RequestText)` → 规范化 rule
  3. `Summary(action, rule)` → 人类可读摘要
  4. 返回 `CompiledDraft{Action, Rule, Summary}`

### `NormalizeRuleConfigInput`
- **签名**：`func NormalizeRuleConfigInput(in RuleConfigInput) RuleConfigInput`
- **职责**：公开包装，委托 `normalizeAlertRuleConfigInput`（不带请求上下文）

### `NormalizeRuleConfigInputForRequest`
- **签名**：`func NormalizeRuleConfigInputForRequest(in, requestText string) RuleConfigInput`
- **职责**：公开包装，委托 `normalizeAlertRuleConfigInputForRequest`（带请求上下文，做 metric source 清理 + host scope 推断 + log selector hints）

### `ShouldBlockCreateOnPreviewSkip`
- **签名**：`func ShouldBlockCreateOnPreviewSkip(reason string) bool`
- **职责**：公开包装，委托 `shouldBlockAlertRuleCreateOnPreviewSkip`

### `NormalizeConfigAction`
- **签名**：`func NormalizeConfigAction(action string) (string, error)`
- **职责**：公开包装，委托 `normalizeConfigAction`

### `Summary`
- **签名**：`func Summary(action string, in RuleConfigInput) string`
- **职责**：生成摘要 `"<action> alert rule \"<name>\""`；name 取 Name > RuleKey > "new alert rule"
- **流程**：`firstNonEmpty(Name, RuleKey, "new alert rule")` → 拼接

### `firstNonEmpty`
- **职责**：辅助函数，返回首个非空（trim 后）字符串

## 5. 依赖关系

- **内部包**：无外部包依赖（仅标准库 `fmt`、`strings`）
- **包内依赖**：`compiler.go`（所有 normalize 实现）

## 6. 并发与资源管理

- **纯函数**：所有方法无状态、无 IO、无锁
- **无 ctx 参数**：纯计算
- **Spec map 引用语义**：`RuleConfigInput.Spec` 是 `map[string]interface{}`，规范化会原地修改；调用方需注意

## 7. 设计模式与亮点

- **公开/私有分离**：`types.go` 暴露公开 API（PascalCase），`compiler.go` 等持有私有实现（camelCase）；`NormalizeRuleConfigInput` 等是薄包装
- **统一入口 `CompileDraft`**：一站式 action + rule + summary，调用方无需关心内部步骤
- **双 normalize 变体**：`NormalizeRuleConfigInput`（纯规则）vs `NormalizeRuleConfigInputForRequest`（带请求上下文）；前者用于已知可信输入，后者用于 LLM 生成 + 用户原话场景
- **`CompileInput` 三字段**：Action + Rule + RequestText，明确区分规范输入和辅助上下文
- **`CompiledDraft.Summary`**：预生成摘要避免调用方重复拼字符串
- **`DefaultJournaldLogSelector` 导出**：供外部测试 / 文档引用，避免硬编码
- **JSON tag 全 snake_case**：与 SKILL.md / claude-code agent frontmatter 对齐（AGENTS.md 规范）

## 8. 注意事项

- **`RuleConfigInput.Window`/`For` 是顶层 alias**：会被 `normalizeTopLevelAlertRuleAliases` 折叠到 conditions 或 spec；规范化后这两个字段被清空，调用方不应依赖它们持久化
- **`Spec map[string]interface{}`**：JSON 反序列化为 `map[string]interface{}`，数值默认 `float64`；规范化函数需用 `firstSpecNumber` 兼容多种数值类型
- **`Enabled *bool`**：用指针区分"未设"和"false"
- **`CompileDraft` 不做预览**：预览由 alertconfig 包的 `AlertRuleManager` 在 apply 前单独执行
- **`Summary` 仅用 Name/RuleKey**：不包含 metric/threshold 等细节，是简短摘要
- **公开 API 稳定性**：`CompileDraft`/`NormalizeRuleConfigInput` 是外部包依赖的契约，变更需走版本号
