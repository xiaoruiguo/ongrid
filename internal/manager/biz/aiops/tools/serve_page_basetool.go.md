# serve_page_basetool.go

## 1. 概述

本文件实现 `serve_page` 工具，让 assistant 把生成的 HTML 页面（诊断报告、临时状态面板）托管成可访问的内网链接。典型场景："把这次巡检结果做成一个网页给我"。

只提供 BaseTool 形态，无闭包路径。Class="write"——发布页面是 side-effecting，但不是 destructive。

**URL 设计**：返回 `/pages/<id>` 而非 raw `/api/pages` read endpoint——pages 是 private（需登录），bare API path 顶层 click 会 401。`/pages/<id>` 在 产物 viewer 里打开（用户已认证），「分享」按钮可生成公开链接。

## 2. 包信息

- **包名**：`tools`
- **路径**：`internal/manager/biz/aiops/tools/serve_page_basetool.go`
- **导入**：
  - `basetool`
  - `log/slog` / `strings`
- **Class**：`write`（side-effecting，非 destructive）

## 3. 关键类型与接口

### `PageStore`（接口）

```go
type PageStore interface {
    // SavePage 存储 HTML 页面，返回 id + relative URL（如 /pages/<token>）
    SavePage(ctx context.Context, title, html string) (id string, url string, err error)
}
```

hosted-pages store 的 seam。在 cmd/main.go 实现；persistence HTML + manager serve at returned URL。

注意：接口返回 `url` 但 `InvokableRun` 用 `id` 构造 `/pages/<id>`——`url` 字段被忽略，可能是历史遗留或为未来 raw API path 预留。

### `ServePageTool`

```go
type ServePageTool struct {
    store PageStore
    log   *slog.Logger
}
```

### `servePageArgs`

```go
type servePageArgs struct {
    HTML  string `json:"html"`  // 完整 HTML 文档字符串（自带 <html><body>…）
    Title string `json:"title"` // 可选标题，用于页面列表展示
}
```

## 4. 关键函数与流程

### `NewServePageTool(s, log)`

`log == nil` → `slog.Default()`。

### `servePageWhenToUse`（常量）

中文 LLM-facing 文案：

- 用途：把生成的 HTML 报告 / 临时状态面板托管成可访问内网链接
- 典型："把这次巡检结果做成一个网页给我"
- 传完整 HTML，返回 url 直接发给用户（内网可打开）
- 适合一次性报告页，**不是长期应用**

### `Info(_ context.Context)`

返回 `ToolInfo`：Name=`serve_page`，Class=`write`，含 WhenToUse。

### `InvokableRun(ctx, argsJSON, _ ...)`

1. 校验 `store` 非 nil。
2. Unmarshal `servePageArgs`。
3. `HTML` trim 后空 → error。
4. `store.SavePage(ctx, Title, HTML)` 返回 `(id, url, err)`。
5. **构造 viewURL = "/pages/" + id**——不用返回的 `url`，用 id 构造 产物 viewer 路径。
6. log Info（id + url）。
7. Marshal `{id, url: viewURL, note}` 返回。

`note` 字段是 LLM-facing instruction：

> "页面已生成。请用 Markdown 链接形式把它给用户（例如 [查看页面](/pages/<id>)），不要只贴路径文本。页面需登录查看；要发给未登录的人，让用户去「产物」里点该页的「分享」按钮生成公开链接。"

这是 prompt 工程：显式告诉 LLM 用 Markdown 链接（clickable）而非 bare path，并说明分享机制。

## 5. 依赖关系

| 方向 | 依赖 | 说明 |
|------|------|------|
| 上游 | `basetool.BaseTool` / `ToolInfo` | 实现 BaseTool 接口 |
| 下游 | `PageStore.SavePage` | hosted-pages store |
| 实现者 | cmd/main.go | persistence + manager serve |

## 6. 并发与资源管理

- **无 per-call timeout**：依赖外层 ctx——`SavePage` 是 DB + 文件存储，预期快。
- 无共享状态，并发安全（依赖 `PageStore` 实现的线程安全）。
- HTML 大小无 cap——LLM 可能生成大页面，要注意存储成本。

## 7. 设计模式与亮点

- **URL 设计：`/pages/<id>` 而非 raw API path**：注释详细解释——pages 是 private（login required），bare `/api/pages/<id>` 顶层 click 会 401；`/pages/<id>` 在 产物 viewer 里打开（用户已认证），「分享」按钮生成公开链接。这是 UX 驱动的设计。
- **`note` 字段是 LLM-facing instruction**：显式告诉 LLM 用 Markdown 链接（clickable）而非 bare path，并说明分享机制——prompt 工程的典型做法，引导 LLM 正确呈现 URL。
- **`Title` 可选**：用于页面列表展示，LLM 可省略——降低 args 复杂度。
- **HTML 原样托管**：`SavePage` 不处理 HTML，原样存储——LLM 生成的 HTML 直接生效，包括内联 CSS/JS（受限于 manager 的 CSP）。
- **Class="write" 而非 "destructive"**：发布页面是 side-effecting 但可逆（可删除页面），不是不可逆操作。
- **`viewURL` 用 id 构造**：不用 `SavePage` 返回的 `url`，而是 `/pages/<id>`——这是因为 产物 viewer 路径与 raw API path 不同，工具层知道正确路径。
- **"适合一次性报告页，不是长期应用"**：WhenToUse 明示定位，引导 LLM 不把它当通用 web hosting。

## 8. 注意事项

- **无 per-call timeout**：`SavePage` 是 DB + 文件存储，预期快但无上限；大 HTML 可能慢。
- **HTML 大小无 cap**：LLM 可能生成 MB 级 HTML（含 base64 图片等），存储成本要注意。
- **`url` 字段被忽略**：`SavePage` 返回 `(id, url, err)` 但 `InvokableRun` 只用 `id`——`url` 可能是历史遗留或为未来预留，当前无作用。
- **HTML 原样托管的安全风险**：LLM 生成的 HTML 可能含恶意 JS（如 XSS payload）——依赖 manager 的 CSP + sandbox 防护，工具层不 sanitization。
- **`/pages/<id>` 是相对路径**：LLM 给用户的是相对路径，用户点击时浏览器解析为当前 host——若用户在 manager 外部（如 IM）看到路径，需要完整 URL。LLM 应该补全 host 或提示用户在 manager 内打开。
- **无闭包路径**：只 BaseTool 形态，晚于 PR-7 闭包路径清理期加入。
- **Class="write" 走 ReviewGate**：发布页面风险低，reviewer 通常 approve；但若 reviewer 配置过严会影响 LLM 生成报告能力。
- **「分享」按钮是用户操作**：LLM 不能 mint 公开链接，只能让用户去 产物 里点——这是有意的权限边界，避免 LLM 自动公开敏感报告。
