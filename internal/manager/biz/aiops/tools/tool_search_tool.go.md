# tool_search_tool.go

## 1. 概述

本文件实现 `ToolSearchTool` —— AIOps 工具体系中"延迟 schema 加载"（deferred schema loading）的入口工具。当 `ToolBag` 因工具数量超过阈值而进入 deferral 模式时，specialty 层工具的 JSON Schema 被 `redactedTool` 替换为空 schema，LLM 必须先调用 `ToolSearch` 拉取真实 schema 才能调用这些工具。

本工具镜像 Anthropic harness 的同名约定（wire name `"ToolSearch"`），使 LLM 训练先验中关于 `select:...` 与 keyword 两种查询形式的认知可以直接复用。本工具是纯读操作，对内存中的工具列表进行过滤，无外部 I/O，无值得追踪的每次调用分配。

## 2. 包信息

- **包名**：`tools`（`internal/manager/biz/aiops/tools`）
- **文件**：`tool_search_tool.go`
- **行数**：约 276 行
- **导入**：`context`、`encoding/json`、`fmt`、`log/slog`、`strings`，以及 `basetool` 子包
- **导出符号**：`ToolSearchToolName`、`DeferredToolBagProvider`、`ToolSearchTool`、`NewToolSearchTool`

## 3. 关键类型与接口

### `DeferredToolBagProvider`（接口）

```go
type DeferredToolBagProvider interface {
    DeferredTools() []basetool.BaseTool
    AllTools() []basetool.BaseTool
}
```

`ToolSearchTool` 与底层 `ToolBag` 之间的解耦 seam。定义为接口而非具体类型依赖，是为了让 `chatruntime` 可以在不引入循环依赖（`tools → chatruntime → tools`）的前提下，本地镜像同样的形状。

- `DeferredTools()`：返回 specialty 层（被 redact 的工具），用于 keyword 查询时偏向"真正需要拉 schema"的工具
- `AllTools()`：返回全部工具，用于 `select:...` 精确名匹配（即便工具已经是 core 层也能命中）

### `ToolSearchTool`（结构体）

```go
type ToolSearchTool struct {
    bag DeferredToolBagProvider
    log *slog.Logger
}
```

仅持有 bag provider 与 logger。无状态、无可变字段，可被并发调用。

### 参数与响应类型

- `toolSearchArgs`：解析后的参数（`Query`、`MaxResults`）
- `toolSearchEntry`：单条匹配结果，`Parameters` 字段为 `json.RawMessage` 以原样透传 schema 字符串，避免 re-encode round-trip
- `toolSearchResponse`：返回给 LLM 的 JSON envelope，包含 `query`（echo）与 `tools` 数组

## 4. 关键函数与流程

### `NewToolSearchTool(p DeferredToolBagProvider, log *slog.Logger)`

构造器。`p` 必须非空——若为 nil，工具将永远返回空结果，构造方应"显式失败而非静默降级"。`log` 为 nil 时 fallback 到 `slog.Default()`。

### `Info(_ context.Context)`

返回 `ToolInfo`：
- `Name = "ToolSearch"`（`ToolSearchToolName` 常量）
- `Class = "read"`（ToolSearch 永不修改状态，bag 在 boot 期一次性构造完毕）
- `Parameters` 为内联的 JSON Schema 字符串（`toolSearchSchema`），声明 `query`（required）与 `max_results`（default 5）

### `InvokableRun(ctx, argsJSON, opts...)`

主流程：

1. 校验 `t.bag` 非 nil，否则报 `"ToolSearch: bag provider not wired"`
2. `json.Unmarshal` args，空字符串跳过解析
3. `query` 为空时报 `"query required"`
4. `max_results` clamp 到 `[1, 20]`，default 5（当 `≤ 0`）
5. **persona-filtered view**：优先使用 `basetool.FilteredToolsFromContext(ctx)`（由 chatruntime 在 `graph.Invoke` 前注入），fallback 到 `t.bag.AllTools()`。这保证 coordinator/worker 不同 persona 看到不同的工具集
6. 调 `matchTools(ctx, searchSet, args.Query, maxResults)`
7. 序列化 `toolSearchResponse` 返回；空匹配返回 `{"query":"...","tools":[]}` 而非 error
8. `t.log.Debug` 记录 query / matches 数 / max_results

### `matchTools(ctx, all, query, maxResults)`

两种匹配路径：

**`select:foo,bar` 精确名匹配**：
- `strings.CutPrefix(q, "select:")` 检测
- `splitNonEmpty(rest, ",")` 切分 CSV，空 token 丢弃
- 用 `map[string]struct{}` 做 O(1) 查找
- **按 `all` 顺序**遍历命中，保证结果确定性（与输入 CSV 顺序无关）
- 命中数达 `maxResults` 即 break

**keyword 子串匹配**：
- `splitNonEmpty(strings.ToLower(q), " ")` 切分多 token
- 每个 token 必须出现在 `name + description + when_to_use`（lowercased）中——**multi-token AND 语义**
- 这保证 "find files" 不会匹配每个 read-only 工具
- 无 scoring / fuzzy ranking（v1 设计：LLM 通常心中有明确名字）

### `toolSearchEntryFromInfo(info)`

将 `ToolInfo` 扁平化为响应 shape。防御性处理 nil `Parameters`——转为 `"{}"`，保证 LLM 在 `.parameters` 位置永远看到合法 JSON。

### `splitNonEmpty(s, sep)`

`strings.Split` 的包装，trim 空白并丢弃空 token。被 `select:...` CSV 切分与 keyword tokenization 共用。

## 5. 依赖关系

- **`basetool`**：`BaseTool`、`ToolInfo`、`InvokeOption`、`FilteredToolsFromContext`
- **`ToolBag`**（同包 `toolbag.go`）：实现 `DeferredToolBagProvider` 接口
- **`registry_basetool.go`**：`BuildBaseTools` 通过 `WithExtra` 把 `ToolSearchTool` 注入 ToolBag（处理 chicken-and-egg：ToolSearch 需要 bag 作为 provider，bag 又必须先存在才能构造 ToolSearch）
- **chatruntime**：通过 ctx 注入 persona-filtered tool slice（运行期解耦，无编译期依赖）

## 6. 并发与资源管理

- `ToolSearchTool` 无可变状态，**线程安全**
- `bag DeferredToolBagProvider` 在 boot 期构造后只读
- `matchTools` 内部使用局部 slice 与 map，无共享状态
- 无 per-call timeout（纯内存操作，亚毫秒级）
- 无 goroutine、无锁、无 IO

## 7. 设计模式与亮点

### Anthropic harness 约定对齐
wire name `"ToolSearch"` 与 `select:...` / keyword 两种查询形式均与 Anthropic 训练数据中的 harness 约定一致，使导入的 skills/personas 提示词可移植，无需重写。

### Persona-filtered view via ctx
通过 `basetool.FilteredToolsFromContext(ctx)` 实现 coordinator/worker 视图隔离——同一个 `ToolSearchTool` 实例在不同 persona 的 ctx 下返回不同的工具集。Fallback 到 `AllTools()` 保证 legacy agent 路径与无白名单 persona 仍能工作。

### 确定性 ordering
两条路径都按 `all` 切片的原始顺序输出，不引入 scoring 或排序。LLM 通常心中有明确名字，fuzzy ranking 是过度设计。

### 空匹配是合法结果
返回 `{"tools":[]}` 而非 error——让 LLM 可以解析结果并自行决策（重写 query / 放弃 / 切换工具）。

### Defensive nil Parameters
`toolSearchEntryFromInfo` 把 nil `Parameters` 转为 `"{}"`——某些工具可能在 `Info()` 中忘记填 Parameters，LLM 仍能看到合法 JSON 而非解析错误。

### Interface seam 解耦
`DeferredToolBagProvider` 接口在 producer 侧定义，避免 `tools → chatruntime` 的循环依赖，同时让 chatruntime 可以本地实现同形状接口。

## 8. 注意事项

- **依赖 ToolBag 实际进入 deferral 模式**：当工具数 ≤ threshold 时，ToolBag 不分区，所有工具都暴露完整 schema，`ToolSearch` 仍可被调用但意义有限（被 `BuildBaseTools` 无条件通过 `WithExtra` 注册，deferral off 时作为 no-op affordance 存在）
- **`select:` 前缀大小写敏感**：`strings.CutPrefix` 区分大小写，LLM 必须用小写 `select:`。keyword 路径下 lowercase 比较则不敏感
- **`max_results` clamp 上限 20**：防止 LLM 一次拉过多 schema 撑爆 prompt budget
- **无 schema cache**：每次调用都重新 `Info()` 与序列化。未来若引入 schema cache 层，可以缓存 `toolSearchEntryFromInfo` 结果
- **`InvokableRun` 第三参数 `opts` 被忽略**：与其他 BaseTool 一致，签名对齐 `basetool.BaseTool` 接口
- **wire name 必须保持 `"ToolSearch"`**：与 Anthropic harness 约定一致是本工具的核心价值，改名会破坏 prompt 可移植性
