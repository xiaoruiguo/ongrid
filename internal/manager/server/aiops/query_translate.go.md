# `query_translate.go` 技术实现文档

> 源文件：`internal/manager/server/aiops/query_translate.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/server/aiops`

## 1. 概述

本文件实现 `POST /v1/aiops/query-translate` —— 把用户输入的自然语言（中/英）翻译成 LogQL / TraceQL / PromQL 的辅助端点。设计要点：**helper 而非硬依赖**，SPA 查询页在端点不可用时仍完全可用；翻译失败不阻塞用户，结果只回填到输入框、不自动提交。关键红线：120s 超时与 `internal/pkg/llm/client.go::defaultTimeout` 对齐；dialect 走白名单（`logql|traceql|promql`），无 fallthrough 到任意 LLM chat；强制 JSON-only 输出 + 容错解析（剥 ```json fences、裁剪到首尾 `{}`），即便 chatty 模型也能产出可用结果。

## 2. 包信息

- **包名**：`aiops`（与 `http.go` 同包）
- **所属模块**：`internal/manager/server/aiops`
- **依赖方向**：被 `Handler.Register` 注册；依赖 `pkg/llm`、`pkg/errs`、`pkg/tenantctx`

## 3. 关键类型与接口

```go
const queryTranslateTimeout = 120 * time.Second

type queryTranslateReq struct {
    Dialect string         `json:"dialect"`           // "logql" | "traceql" | "promql"
    Prompt  string         `json:"prompt"`            // 自然语言查询
    Context map[string]any `json:"context,omitempty"` // 可选 hint（device / 时间窗等）
}

type queryTranslateResp struct {
    Query       string `json:"query"`
    Explanation string `json:"explanation,omitempty"`
    Dialect     string `json:"dialect"`
}

// 方言 → 提示词脚手架；硬编码在二进制里，新部署无需 admin 配置
var dialectGuide = map[string]string{
    "logql":   `LogQL（Loki 查询语言）。规则：...`,
    "traceql": `TraceQL（Tempo 查询语言）。规则：...`,
    "promql":  `PromQL（Prometheus 查询语言）。规则：...`,
}

var fenceRe = regexp.MustCompile("(?s)```(?:json)?\\s*(\\{.*?\\})\\s*```")
```

每个 dialect guide 都列了 ongrid 实际的 label / metric 白名单（如 `device_id`/`service`/`level`、`node_cpu_seconds_total`/`host_mem_pct`），明示「严禁编造标签」。

## 4. 关键函数与流程

### `Handler.queryTranslate`
- **签名**：`func (h *Handler) queryTranslate(w http.ResponseWriter, r *http.Request)`
- **职责**：执行一次 NL → dialect 翻译
- **流程**：
  1. `tenantctx.From(ctx)` 校验已认证 caller；缺失 → 401
  2. `h.llmClient == nil` → 503 `llm client not configured`（SPA 隐藏 ✨ 按钮，不弹错）
  3. decode body；失败 → 400 join ErrInvalid
  4. `dialect = TrimSpace(ToLower(req.Dialect))`；查 `dialectGuide` 未命中 → 400「unsupported dialect」
  5. `prompt = TrimSpace(req.Prompt)`；空 → 400「prompt required」
  6. 拼 systemPrompt：`你是一个查询语言专家...` + guide + 严格 JSON 输出指令（shape: `{query, explanation}`）
  7. 拼 userPrompt：`用户需求：<prompt>`；有 context → 追加 `\n\n上下文：<json>`
  8. `ctx, cancel := context.WithTimeout(r.Context(), queryTranslateTimeout)`；defer cancel
  9. `h.llmClient.Chat(ctx, llm.ChatReq{Messages: [system, user], Temperature: 0.1})`
  10. err → 502 `llm: <err>`
  11. `parseTranslateOutput(resp.Assistant.Content)`；失败 → 502 `parse: <err> raw=<truncate 200>`
  12. `parsed.Dialect = dialect` → 200
- **错误处理**：401 / 400 / 503 / 502，全部走 `http.Error` 或 `writeErr`；503 时 SPA 静默降级

### `parseTranslateOutput`
- **签名**：`func parseTranslateOutput(raw string) (*queryTranslateResp, error)`
- **职责**：从 LLM 输出中救出 JSON
- **流程**：
  1. TrimSpace
  2. `fenceRe.FindStringSubmatch` 命中 ```json fences → 取 group 1
  3. 裁掉首 `{` 之前、末 `}` 之后的内容
  4. `json.Unmarshal` → `queryTranslateResp`
  5. `out.Query = TrimSpace(out.Query)`；空 → error「empty query in response」
- **错误处理**：Unmarshal 失败或 query 空 → error，由 caller 包成 502

### `truncate`
- **签名**：`func truncate(s string, n int) string`
- **职责**：截断字符串用于错误日志；超长追加 `…`

## 5. 依赖关系

- **内部包**：`pkg/llm`（`Client`/`ChatReq`/`Message`）、`pkg/errs`（`ErrInvalid`/`ErrUnauthorized`）、`pkg/tenantctx`
- **外部库**：标准库 `regexp`
- **被调用方**：`Handler.Register`（在 `http.go`）注册 `/v1/aiops/query-translate`

## 6. 并发与资源管理

- **无共享状态**：handler 内只读 `h.llmClient`；`dialectGuide` 是包级 const map，只读
- **ctx 透传**：`context.WithTimeout(r.Context(), 120s)` 显式兜底，cancel defer 释放
- **无缓存**：每次请求都打 LLM；翻译结果不缓存（用户可能想微调后重试）

## 7. 设计模式与亮点

- **Helper 而非硬依赖**：注释明示「this is a *helper*, not a hard dependency」；端点不可用时 SPA 查询页完全可用
- **不自动提交**：注释明示「Result populates the main query box (does NOT auto-submit)」——用户必须 review 后再跑，避免 LLM 幻觉的查询直接打到 Prometheus / Loki
- **Dialect 白名单 + 硬编码 guide**：硬编码在二进制里，新部署开箱即用；明示「严禁编造标签 / metric」，列了 ongrid 实际可用的 label / metric
- **120s 超时统一**：注释解释从 6s（Haiku 默认）→ 20s（DeepSeek 默认）→ 120s（reasoning 模型 + 工具调用）的演进，与 `llm/client.go::defaultTimeout` 对齐
- **JSON-only + 容错解析**：system prompt 强制 JSON；`parseTranslateOutput` 三段容错（fences → 裁首尾 → Unmarshal），chatty 模型也能用
- **503 静默降级**：llmClient 未注入时返 503 + 干净 message，SPA 隐藏 ✨ 按钮而非弹错
- **错误回显 raw**：parse 失败时把截断的 raw 输出包进 error，方便 SPA 显示「翻译失败：<raw>」让用户决定重试还是手输
- **Temperature 0.1**：注释明示「deterministic-ish; we want a precise query」

## 8. 注意事项

- **`queryTranslateTimeout = 120s`**：项目级 LLM 调用超时下限；caller 无法覆盖，需更短超时由 client 侧 ctx 控制
- **`dialectGuide` 写死在代码**：新增 dialect 需改代码 + 重新发版；好处是新部署无需 admin 配置
- **`parseTranslateOutput` 容错有上限**：若 LLM 输出多个 JSON 对象，只会取首 `{` 到末 `}` 之间，可能拼出非法 JSON；这种边缘 case 依赖 LLM 遵守 system prompt
- **无认证 caller 检查走 tenantctx**：与 aiops 其他端点一致；不依赖 `callerFromCtx`，因为本端点不需要 caller 身份，只需认证过
- **无审计**：翻译请求不写 audit log（helper 性质，不算业务操作）
- **Temperature 0.1 不是 0**：注释明示「deterministic-ish」——完全不归零是为了让模型仍能选词，但希望输出稳定
