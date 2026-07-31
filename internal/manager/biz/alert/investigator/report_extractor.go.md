# `report_extractor.go` 技术实现文档

> 源文件：`internal/manager/biz/alert/investigator/report_extractor.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/alert/investigator`

## 1. 概述

本文件是 investigator 子系统的 LLM Pass-2 结构化抽取层。investigator worker 跑完 ReAct loop 后产生 markdown 形式的 `finalAnswer`，`extractStructured` 把它喂给 LLM 让其返回 JSON 结构化报告（root_cause / affected_window / pinpoint_target / evidence / suggested_actions / confidence），映射到 `ReadyFields` 持久化。失败时（LLM 错误 / JSON parse 失败 / 空 answer）静默回退到 `firstParagraphOneLine` 启发式——仍 ship findings_md + root_cause 给操作员。文件还提供 `buildRelatedAlertsJSON`（DB 查询 co-occurring incidents，与 Pass-2 无关，独立运行）、`extractJSONBlob`（平衡大括号解析器，容忍 ```json fence 与转义字符）、prompt 构造、confidence clamp 等工具。

## 2. 包信息

- **包名**：`investigator`
- **所属模块**：`internal/manager/biz/alert/`
- **依赖方向**：被本包 `usecase.go::run` 调用；依赖 `internal/manager/model/alert` + `internal/pkg/llm`

## 3. 关键类型与接口

```go
// LLMSummarizer 是 report-extractor 步骤用的窄 seam。
// *llm.MultiClient 满足；测试可注入确定性响应 fake。
type LLMSummarizer interface {
    Chat(ctx context.Context, req llm.ChatReq) (*llm.ChatResp, error)
}

// extractedReport 是要求 LLM 返回的 JSON schema。镜像 ReadyFields + 中间类型。
type extractedReport struct {
    RootCause      string           `json:"root_cause"`
    AffectedWindow string           `json:"affected_window,omitempty"`
    PinpointTarget map[string]any   `json:"pinpoint_target,omitempty"`
    Evidence       []map[string]any `json:"evidence,omitempty"`
    Suggested      []map[string]any `json:"suggested_actions,omitempty"`
    Confidence     *float64         `json:"confidence,omitempty"`
}

// relatedAlertWire 是 related_alerts_json 的 per-row shape。
type relatedAlertWire struct {
    IncidentID  uint64 `json:"incident_id"`
    Rule        string `json:"rule"`
    RuleName    string `json:"rule_name"`
    Severity    string `json:"severity"`
    Status      string `json:"status"`
    FiredAt     string `json:"fired_at"`
    LastFiredAt string `json:"last_fired_at"`
}

const (
    relatedAlertHalfWindow = 5 * time.Minute
    relatedAlertLimit      = 10
)
```

`ReadyFields` 在 `usecase.go` 声明；`extractorSystemPrompt` 是包级常量。

## 4. 关键函数与流程

### `extractStructured`

- **签名**：`func (uc *Usecase) extractStructured(ctx, incident alertmodel.Incident, finalAnswer string, toolCallCount int, locale string) ReadyFields`
- **职责**：Pass-2 LLM 抽取结构化报告；失败回退启发式
- **流程**：
  1. `relatedJSON := uc.buildRelatedAlertsJSON(ctx, incident)`（DB 查询，独立于 Pass-2，无条件运行）
  2. 构造 `fallback := ReadyFields{RootCause: firstParagraphOneLine(finalAnswer, 200), RelatedAlertsJSON: relatedJSON, FindingsMD: finalAnswer, ...}`
  3. `uc.summarizer == nil` → return fallback
  4. `prompt := buildExtractorPrompt(incident, finalAnswer, locale)`
  5. `cctx, cancel := context.WithTimeout(ctx, uc.cfg.SummarizerTimeout)` + defer cancel
  6. `resp, err := uc.summarizer.Chat(cctx, ChatReq{Model, Provider, Temperature: 0, Messages: [system, user]})`；err → Info log + return fallback
  7. `rawAnswer := strings.TrimSpace(resp.Assistant.Content)`
  8. `jsonBlob := extractJSONBlob(rawAnswer)`；空 → Info log + return fallback
  9. `json.Unmarshal(jsonBlob, &ex)`；err → Info log + return fallback
  10. 提升 ex 字段到 `out := ReadyFields{FindingsMD: finalAnswer, ToolCallCount}`：
      - `RootCause`：ex 非空 → `clampRunes(ex.RootCause, 200)`；否则用 fallback.RootCause
      - `AffectedWindow`：TrimSpace
      - `PinpointedTargetJSON` / `EvidenceJSON` / `SuggestedActionsJSON`：`marshalOrDefault` 序列化
      - `RelatedAlertsJSON`：复用 relatedJSON（避免二次 DB 查询）
      - `Confidence`：ex 非空时 clamp 到 [0, 1]（容忍 0~100 / 0~10 输入）
      - `ConfidenceFactorsJSON`：`buildConfidenceFactors(ex, toolCallCount)`
  11. 返回 out
- **错误处理**：所有失败静默回退 fallback；Info log（不 Warn——Pass-2 失败是预期路径）

### `buildRelatedAlertsJSON`

- **签名**：`func (uc *Usecase) buildRelatedAlertsJSON(ctx, target alertmodel.Incident) string`
- **职责**：查询 co-occurring incidents，序列化为 JSON
- **流程**：
  1. `uc.related == nil` → `"[]"`
  2. `qctx, cancel := context.WithTimeout(ctx, 3*time.Second)` + defer cancel
  3. `rows, err := uc.related.RelatedToIncident(qctx, &target, relatedAlertHalfWindow, relatedAlertLimit)`；err → Info log + `"[]"`
  4. 空 → `"[]"`
  5. per-row（跳过 nil 与 target.ID 自身）→ `relatedAlertWire{IncidentID, Rule, RuleName, Severity, Status, FiredAt: RFC3339, LastFiredAt: RFC3339}`
  6. `marshalOrDefault(out, "[]")`
- **错误处理**：DB 失败 Info log + `"[]"`（RelatedAlertsJSON 列 NOT NULL 无 DEFAULT，必须非空——MySQL Error 1101 trap）

### `buildExtractorPrompt`

- **签名**：`func buildExtractorPrompt(incident, finalAnswer, locale string) string`
- **职责**：构造 Pass-2 user prompt
- **流程**：
  1. `# Alert` + incident 元数据（rule/severity/first_fired_at/last_fired_at/device_id/value/threshold/summary）
  2. `# Investigator narrative (markdown)` + finalAnswer
  3. `# Now output the JSON described in the system prompt.`
  4. `localeDirective(locale)` 非空时追加 + "every string value in the JSON MUST be in the specified language"
- **错误处理**：无

### `extractJSONBlob`

- **签名**：`func extractJSONBlob(s string) string`
- **职责**：从模型回复中提取首个平衡顶层 JSON 对象
- **流程**：
  1. TrimSpace
  2. 去 ```json / ```JSON / ``` fence 标记
  3. TrimSpace
  4. 找首个 `{`；找不到 → `""`
  5. 深度计数平衡大括号：
     - 引号内大括号无效（`inStr` 标志）
     - 转义字符跳过（`escape` 标志，仅引号内有效）
     - `{` → depth++；`}` → depth--；depth==0 时返回 `s[start:i+1]`
  6. 不平衡 → `""`
- **错误处理**：无；返回空字符串让调用方回退

### `marshalOrDefault`

- **签名**：`func marshalOrDefault(v any, def string) string`
- **职责**：序列化 v；nil / 错误 / "null" → def
- **流程**：`json.Marshal`；nil/空/"null" → def

### `buildConfidenceFactors`

- **签名**：`func buildConfidenceFactors(ex extractedReport, toolCallCount int) string`
- **职责**：构造 confidence_factors_json
- **流程**：`factors := {evidence_steps, tool_call_count, has_pinpoint, has_affected_win, has_suggested, narrative_present: true}`；`json.Marshal`

### `clampRunes` / `truncate`

- `clampRunes(s, max)`：按 rune 截断（中文友好）；超长 → `s[:max-1] + "…"`
- `truncate(s, max)`：按 rune 截断；超长 → `s[:max] + "…"`

## 5. 依赖关系

- **内部包**：
  - `internal/manager/model/alert`（`Incident` 类型）
  - `internal/pkg/llm`（`ChatReq` / `ChatResp` / `Message`）
- **外部库**：`context` / `encoding/json` / `fmt` / `strings` / `time`
- **被调用方**：本包 `usecase.go::run`（worker 完成后调 `extractStructured`）
- **依赖**：本包 `usecase.go`（`Usecase` / `ReadyFields` / `cfg` / `logger` / `related` / `summarizer`）、`usecase.go::localeDirective`（locale 指令共享）

## 6. 并发与资源管理

- `extractStructured` 在 worker goroutine 内同步调用，无内部并发
- LLM 调用走 `cctx`（`cfg.SummarizerTimeout`，默认 120s）
- `buildRelatedAlertsJSON` 走独立 3s timeout ctx
- 无共享状态、无锁

## 7. 设计模式与亮点

- **Pass-2 独立 LLM 调用**：与 Pass-1 worker 调用分离，独立 timeout、独立 model（`SummarizerModel`）、独立 provider（`SummarizerProvider`）
- **静默回退**：所有失败（LLM 错误 / JSON parse 失败 / 空 answer）Info log + 回退 `firstParagraphOneLine` 启发式——操作员仍看到 findings_md + root_cause
- **`extractJSONBlob` 平衡大括号解析**：容忍 ```json fence、前导 prose、trailing commentary；引号内大括号无效；转义字符处理
- **Confidence clamp**：模型可能返 0~100 或 0~10，统一 clamp 到 [0, 1]——`>1 && <=100` 除以 100
- **RelatedAlertsJSON 独立查询**：与 Pass-2 无关，无条件运行；失败返 `"[]"`（列 NOT NULL 无 DEFAULT）
- **locale 覆盖**：`localeDirective` 追加到 prompt 末尾，要求 JSON 字符串值用指定语言——AI 输出跟随用户 UI locale
- **`clampRunes` 中文友好**：按 rune 而非 byte 截断，避免中文截断到半个字符
- **`marshalOrDefault` 防御 NULL**：nil / 错误 / "null" 返 def，避免 NOT NULL 列写入失败
- **`buildConfidenceFactors` 透明**：记录 evidence_steps / tool_call_count / has_pinpoint 等因素，让操作员理解 confidence 评分依据
- **`firstParagraphOneLine` 启发式**：跳过 markdown 标题、分隔线、纯 bold 短行（section header），落到首个 prose 段落

## 8. 注意事项

- **`SummarizerTimeout` 默认 120s**：注释提到从 30s 提到 120s 是因 cluster default 移到慢 reasoning model，结构化 JSON pass 不再 false-fail
- **`relatedAlertHalfWindow = 5 * time.Minute`**：co-occurring incident 窗口；`relatedAlertLimit = 10` 上限
- **`extractJSONBlob` 不识别数组**：只识别 `{...}` 对象；模型若返数组会回退 fallback
- **Confidence clamp 容忍 0~100**：`>1 && <=100` 除以 100；`>100` 截到 1；`<0` 截到 0
- **`buildRelatedAlertsJSON` 跳过 target.ID 自身**：避免 incident 把自己列为 related
- **`extractorSystemPrompt` 英文**：要求 JSON 输出，规则严格（"Output ONLY the JSON, no prose, no markdown fences"）
- **locale 仅影响 JSON 字符串值**：`buildExtractorPrompt` 末尾追加"every string value MUST be in the specified language"——root_cause / evidence[].summary / suggested_actions[].label 等
- **`firstParagraphOneLine` max=200**：root_cause fallback 截到 200 rune
- **Pass-2 失败是预期路径**：用 Info 而非 Warn log——避免误导告警
- **`marshalOrDefault` 防御 "null"**：`json.Marshal(nil)` 返 `"null"`，列 NOT NULL 会失败；显式检查返 def
