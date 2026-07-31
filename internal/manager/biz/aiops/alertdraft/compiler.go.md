# `compiler.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/alertdraft/compiler.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/alertdraft`

## 1. 概述

本文件是 alertdraft 包的**编译器内核**：把 LLM 生成的半结构化 `RuleConfigInput` 规范化为可信的告警规则草案。核心函数 `normalizeAlertRuleConfigInput` 是 11 步流水线（kind 规范化 → alias 折叠 → spec 规范化 → 简单 host metric 表达式重写 → conditions 默认值补全 → kind 推断 → scope 规范化 → severity/ruleKey/name/runbookURL 默认值 → notify policy 规范化）。`shouldBlockAlertRuleCreateOnPreviewSkip` 是预览失败时的硬阻断关键词表，被 alertconfig 校验器复用。`normalizeConfigAction` 强制 v1 只支持 `create`。

## 2. 包信息

- **包名**：`alertdraft`
- **所属模块**：`internal/manager/biz/aiops/alertdraft`
- **依赖方向**：被 `types.go` 的 `CompileDraft`/`NormalizeRuleConfigInputForRequest` 内部调用；依赖 `internal/pkg/errs`

## 3. 关键类型与接口

复用 `types.go` 定义的 `RuleConfigInput`、`RuleCondition`。无新类型定义。

## 4. 关键函数与流程

### `shouldBlockAlertRuleCreateOnPreviewSkip`
- **签名**：`func shouldBlockAlertRuleCreateOnPreviewSkip(reason string) bool`
- **职责**：预览失败原因含"硬阻断关键词"→ 返回 true（→ alertconfig 校验器报 error 阻断 apply）
- **流程**：trim reason；空 → false；遍历 9 个关键词子串匹配：
  - "请补全规则字段"、"为空"、"not in closed-set"、"不在 closed-set"、"no burn windows"、"kind not supported"、"required"、"unsupported"、"$window"、"未发现 service_name"

### `normalizeAlertRuleConfigInput`
- **签名**：`func normalizeAlertRuleConfigInput(in RuleConfigInput) RuleConfigInput`
- **职责**：11 步规范化流水线（核心）
- **流程**：
  1. `normalizeAlertRuleKind` 规范化 kind（threshold→metric_threshold、promql→metric_raw 等）
  2. `normalizeTopLevelAlertRuleAliases` 把顶层 Window/For 折叠到 conditions 或 spec
  3. `normalizeAlertRuleSpec` 按 kind 分支规范化 spec（默认值、selector 合并、log filter 解析）
  4. `rewriteSimpleHostMetricRawExpr` 把 `cpu > 80` 这类简单谓词转 metric_threshold
  5. 遍历 conditions：`canonicalAlertMetric` + 默认 aggregator=avg + 默认 window=5m
  6. kind 空且 conditions 非空 → kind=metric_threshold
  7. kind 仍空 → `inferAlertRuleKind` 从 spec keys 推断
  8. `normalizeAlertScopeType` 规范化 scope（host/device/node → host；global/all/db → global）
  9. `normalizeAlertScopeForKind` 按 kind 调整（log/trace/burn_rate 不允许 monitoring_pipeline）
  10. scope 仍空 → `defaultAlertScope`（metric_threshold 默认 host；metric_raw/log 默认 global）
  11. severity 空 → warning；ruleKey 空 → suggestedAlertRuleKey；name 空 → suggestedAlertRuleName；runbookURL 空 → suggestedAlertRunbookURL
  12. `normalizeNotifyPolicyAliases` 清理 notify window/minFires 不一致

### `normalizeTopLevelAlertRuleAliases`
- **职责**：把顶层 Window/For 下放到 conditions（metric_threshold）或 spec（其他 kind）
- **流程**：
  - window 和 sustainFor 都空 → 直接返回
  - 否则遍历 conditions，空字段补 window/for
  - 非 metric_threshold 或无 conditions：写入 spec.window/spec.for（若 spec 中无对应 key）
  - 清空顶层 Window/For

### `normalizeNotifyPolicyAliases`
- **职责**：notify_window_seconds 和 notify_min_fires 必须同时设或同时不设；不一致 → 全清零
- **流程**：`(a==0) == (b==0)` 异或逻辑：两者一空一非空 → 清零两者

### `normalizeAlertRuleConfigInputForRequest`
- **签名**：`func normalizeAlertRuleConfigInputForRequest(in, requestText string) RuleConfigInput`
- **职责**：带请求上下文的规范化（4 步）
- **流程**：
  1. `normalizeMetricSourceScopeForRequest` 清理 LLM 注入的隐式 metric source 身份标签（若用户请求未显式 scope）
  2. `normalizeAlertRuleConfigInput`（核心 11 步）
  3. `normalizeAlertScopeForRequest` 根据请求文本推断 host 作用域
  4. `applyAlertRuleRequestHints` 应用 log selector / metric_forecast 请求提示

### `normalizeConfigAction`
- **签名**：`func normalizeConfigAction(action string) (string, error)`
- **职责**：v1 仅支持 create；其他 action → `errs.ErrInvalid`
- **流程**：lowercase + trim；"" 或 "create" → "create"；default → error "action must be create; v1 only supports creating alert rules"

## 5. 依赖关系

- **内部包**：`internal/pkg/errs`（`ErrInvalid`）
- **包内调用**：`spec_normalize.go`（`normalizeAlertRuleKind`/`normalizeAlertScopeType`/`normalizeAlertScopeForKind`/`normalizeAlertRuleSpec`/`inferAlertRuleKind`/`defaultAlertScope`）、`defaults.go`（`canonicalAlertMetric`/`suggestedAlertRuleKey/Name/RunbookURL`）、`metric_raw.go`（`rewriteSimpleHostMetricRawExpr`）、`request_hints.go`（`normalizeMetricSourceScopeForRequest`/`normalizeAlertScopeForRequest`/`applyAlertRuleRequestHints`）、`promql.go`（`firstPromMetricName`）

## 6. 并发与资源管理

- **纯函数**：所有方法无状态、无 IO、无锁
- **无 ctx 参数**：纯计算
- **不可变输入**：函数返回新 struct，不修改调用方的 in（但 spec map 是引用类型，会被原地修改）

## 7. 设计模式与亮点

- **流水线式规范化**：11 步顺序执行，每步职责单一，可独立测试
- **kind 双向推断**：先规范化显式 kind，kind 空则从 conditions/spec keys 推断
- **alias 折叠**：顶层 Window/For 下放到 conditions 或 spec，避免数据双写
- **请求上下文感知**：`ForRequest` 变体结合用户原话做语义推断（host scope / log selector hints）
- **notify policy 一致性**：window/minFires 必须成对，避免半配置状态
- **v1 严格 action 白名单**：只允许 create，未来扩展走版本号
- **硬阻断关键词表**：preview 失败原因含结构性缺失关键词 → 阻断 apply；纯运行时问题（如指标未采集）→ 仅 warning

## 8. 注意事项

- **spec map 原地修改**：`normalizeAlertRuleSpec`/`normalizeMetricRawSpec` 等会修改 spec 的 map 字段；调用方若需保留原始 spec 应深拷贝
- **kind 推断启发式**：`inferAlertRuleKind` 按 spec keys 子串匹配，可能误判（如同时含 expr 和 stream_selector 时优先 log_match）
- **scope 推断顺序**：先 `normalizeAlertScopeType` 规范化，再 `normalizeAlertScopeForKind` 按 kind 调整，最后 `defaultAlertScope` 兜底；顺序不能乱
- **`normalizeNotifyPolicyAliases` 异或逻辑**：`(a==0) == (b==0)` 等价于 "两者都零或都非零时跳过"，仅一空一非空时清零
- **v1 action 限制**：扩展 update/delete 需新版本路由
- **`shouldBlockAlertRuleCreateOnPreviewSkip` 关键词表硬编码**：新增场景需手动加关键词
