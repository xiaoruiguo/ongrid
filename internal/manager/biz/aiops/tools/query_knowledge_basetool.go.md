# query_knowledge_basetool.go

## 1. 概述

本文件实现 `query_knowledge` 工具，是 LLM-callable 的知识库搜索工具。agent 用它检索 operator-curated docs + repo-imported markdown / config，回答"我们之前怎么处理 X"或"<service> 的部署文档说什么"。

**RAG-lite**：当前是 keyword search，Phase-2 上 embeddings。

**强 nudge**：WhenToUse 明示"回答任何运维 / 故障排查 / 部署 / 配置 / 网络 / 系统类问题前都先调一次本工具"——KB 是团队精选的中文 playbook（DNS / conntrack / MTU / eBPF / TLS / netshoot / netns 等），比通用知识更贴近本系统的命令偏好和处置惯例。

只提供 BaseTool 形态，无闭包路径。

## 2. 包信息

- **包名**：`tools`
- **路径**：`internal/manager/biz/aiops/tools/query_knowledge_basetool.go`
- **导入**：
  - `basetool`
  - `knowledgebiz`（`internal/manager/biz/knowledge`）—— `SearchOptions` / `SearchHit`
- **Class**：`read`

## 3. 关键类型与接口

### `KnowledgeSearcher`（接口）

```go
type KnowledgeSearcher interface {
    Search(ctx context.Context, q string, opts knowledgebiz.SearchOptions) ([]knowledgebiz.SearchHit, error)
}
```

窄 biz seam，`*knowledge.Usecase` 满足。

### `queryKnowledgeArgs`

```go
type queryKnowledgeArgs struct {
    Query      string   `json:"query"`
    Path       string   `json:"path,omitempty"`        // 精确 path 过滤，与 path_prefix 互斥
    PathPrefix string   `json:"path_prefix,omitempty"` // path 前缀过滤（子树）
    Tags       []string `json:"tags,omitempty"`        // any-match
    MaxResults int      `json:"max_results"`           // 默认 5，cap 20
}
```

### `queryKnowledgeHit`

```go
type queryKnowledgeHit struct {
    ID         uint64   `json:"id"`
    Title      string   `json:"title"`
    SourceType string   `json:"source_type"`
    URL        string   `json:"url,omitempty"`
    Path       string   `json:"path,omitempty"`
    Tags       []string `json:"tags,omitempty"`
    Score      float64  `json:"score"`
    Preview    string   `json:"preview"`
}
```

### `queryKnowledgeResponse`

```go
type queryKnowledgeResponse struct {
    Items     []queryKnowledgeHit `json:"items"`
    Total     int                 `json:"total"`
    Query     string              `json:"query"`
    Truncated bool                `json:"truncated,omitempty"`
}
```

### `QueryKnowledgeTool`

```go
type QueryKnowledgeTool struct {
    svc KnowledgeSearcher
    log *slog.Logger
}
```

## 4. 关键函数与流程

### `NewQueryKnowledgeTool(svc, log)`

`log == nil` → `slog.Default()`。

### `queryKnowledgeWhenToUse`（常量）

中文 LLM-facing 文案，强 nudge：

- **首要指令**：回答任何运维 / 故障排查 / 部署 / 配置 / 网络 / 系统类问题前都先调一次本工具
- **KB 内容**：团队精选的中文 playbook（DNS / conntrack / MTU / eBPF / TLS / netshoot / netns 等）
- **触发示例**：'X 怎么排查' / 'Y 怎么部署' / 'Z 报错怎么处理' / '我们之前怎么做 W'
- **命中策略**：top score ≥ 0.6 就基于 playbook 步骤回答；未命中再走通用诊断或实时数据工具
- **path 分目录**：KB 用 "/" 分隔的路径（'网络/DNS'、'网络/TLS'、'K8s/网络' 等）；先 `GET /v1/knowledge/paths` 看目录，再用 `path_prefix` 收窄
- **tags any-match**：例 `['dns','tls']`，任一命中即可
- **NOT for**：实时指标 / 告警 / 设备状态（用 query_promql / query_logql / get_edge_summary）/ 对话式 config 创建（用 draft_config_change/apply_config_change）
- **query 形式**：自然语言整句（不必拆词）；同一主题同一会话只查一次

### `Info(_ context.Context)`

返回 `ToolInfo`：Name=`query_knowledge`，Class=`read`，Parameters 是 `queryKnowledgeSchema`（注意是 string 常量，不是 `json.RawMessage`）。

### `InvokableRun(ctx, argsJSON, _ ...)`

1. 校验 `svc` 非 nil。
2. Unmarshal args。
3. `Query` trim 后空 → error。
4. `MaxResults ≤ 0 → 5`，`> 20 → 20`。
5. 调 `svc.Search(ctx, query, SearchOptions{Path, PathPrefix, Tags, Limit: MaxResults})`。
6. 遍历 hits 构造 `queryKnowledgeHit`：
   - `Preview = h.Doc.Content`，**cap 800 字符**（超了截断 + "…"，`Truncated = true`）
   - 注释明示 cap 目的：max_results=5 × 800 chars ≈ 4k tokens，控制 LLM 上下文成本；LLM 需要全文可未来用 doc-fetch tool 再查
7. Marshal `queryKnowledgeResponse{Items, Total, Query, Truncated}` 返回。

## 5. 依赖关系

| 方向 | 依赖 | 说明 |
|------|------|------|
| 上游 | `basetool.BaseTool` / `ToolInfo` | 实现 BaseTool 接口 |
| 下游 | `KnowledgeSearcher.Search`（接口） | 数据源 |
| 类型 | `knowledgebiz.SearchOptions` / `SearchHit` | wire 协议 |

## 6. 并发与资源管理

- **无 per-call timeout**：依赖外层 ctx（caller 控制）——这与多数工具（10s/15s/20s/30s）不同，可能因为 KB 搜索是本地 DB 操作，预期快；但 embeddings 上线后可能需要补 timeout。
- `MaxResults` cap 20 + `Preview` cap 800 字符双重 bound，防止 KB 大文档撑爆 LLM 上下文。
- 无共享状态，并发安全。

## 7. 设计模式与亮点

- **强 nudge WhenToUse**：明示"回答任何运维类问题前都先调一次"——这是 ongrid agent 设计的核心策略：KB > 通用知识 > 实时数据。top score ≥ 0.6 命中阈值让 LLM 有明确判断标准。
- **path 分目录 + tags any-match**：KB 用 "/" 分隔的目录树（中文 path 如 '网络/DNS'），`path_prefix` 收窄子树，`tags` 跨目录过滤——两种维度正交，LLM 可组合使用。
- **`Path` 与 `PathPrefix` 互斥**：schema 描述明示，但代码没强制校验（都传时行为未定义，依赖 biz 层处理）——这是 trade-off，简化工具层逻辑。
- **Preview 800 字符 cap**：max_results=5 × 800 ≈ 4k tokens，是 LLM 上下文成本控制的标准做法；`Truncated` 字段让 LLM 知道是否有更多内容可查。
- **`Score` 透传**：返回 score 让 LLM 自行判断命中质量（配合 ≥0.6 阈值）。
- **schema 用 string 常量**：与多数工具的 `json.RawMessage` 不同，这里 `queryKnowledgeSchema` 是 string，`json.RawMessage(queryKnowledgeSchema)` 在 Info 里转——可能是历史遗留，不影响功能。
- **"同一主题同一会话只查一次"**：WhenToUse 明示这个去重 nudge，避免 LLM 反复查同一 query 浪费 token。

## 8. 注意事项

- **无 per-call timeout**：依赖外层 ctx，embeddings 上线后搜索变慢可能阻塞 caller；建议补 10-15s timeout。
- **`Path` / `PathPrefix` 互斥未强制**：都传时行为依赖 biz 层，可能 silently 取其一——LLM 误传时结果不可预期。
- **Preview 截断字符可能切断中文**：`preview[:800]` 是字节切片，800 字节可能切到中文 UTF-8 中间（中文 3 字节/字符）；实际效果是 800/3 ≈ 266 个中文字符，LLM 看到的 preview 比预期短。
- **无闭包路径**：只 BaseTool 形态，意味着这个工具晚于 PR-7 闭包路径清理期加入，或从一开始就走 BaseTool。
- **`Truncated` 语义**：是"任一 hit 的 preview 被截断"而非"hits 列表被 max_results 截断"——后者通过 `Total` 字段表达（Total == len(Items) 表示未截断）。
- **无 `GET /v1/knowledge/paths` 工具**：WhenToUse 提到用这个 API 看目录，但工具本身没实现——LLM 需要走 HTTP API（如果 flow 里有暴露），或靠 path_prefix 试探。
- **`Score ≥ 0.6` 阈值是软约定**：LLM 自行判断，工具不强制；低 score 命中时 LLM 仍可能基于 playbook 回答（误导风险）。
