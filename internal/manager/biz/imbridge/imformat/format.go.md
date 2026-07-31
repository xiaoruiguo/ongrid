# `format.go` 技术实现文档

> 源文件：`internal/manager/biz/imbridge/imformat/format.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/imbridge/imformat`

## 1. 概述

本文件是 IM 桥接器的"格式转换层"：把 agent 输出的 CommonMark/GFM markdown 翻译成各 IM 平台的原生方言（Slack mrkdwn、Telegram HTML、纯文本）。Markdown 始终作为输入/回退格式；provider 的 JSON payload 由受信代码构造。红线：LLM 生成的原始 HTML 不得作为 provider parse mode 的输入——必须剥成纯文本后转义。

## 2. 包信息

- **包名**：`imformat`
- **所属模块**：`internal/manager/biz/imbridge/imformat`
- **依赖方向**：被 `imbridge/provider/{slack,telegram,feishu,dingtalk}` 调用；依赖 `goldmark`（含 GFM 扩展）、`golang.org/x/net/html`

## 3. 关键类型与接口

```go
type dialect uint8

const (
    dialectPlain dialect = iota        // 纯文本回退
    dialectSlack                        // Slack mrkdwn
    dialectTelegramHTML                 // Telegram HTML parse mode
)

type renderer struct {
    source   []byte
    dialect  dialect
    maxRunes int             // 0 = 不截断；>0 = 按 rune 数截断
    out      strings.Builder
    stopped  bool            // 达到 maxRunes 后停止追加
}

type inlineStyle struct {
    bold, italic, strike, code bool
    link                       string
}
```

常量：
- `telegramMaxRunes = 4096`（Telegram 单消息上限）
- `slackSectionMaxRunes = 2900`（Block Kit 单 section 上限）
- `slackMaxSections = 45`（单消息 section 数上限）

## 4. 关键函数与流程

### `Plain / PlainExcerpt`
- **签名**：`func Plain(markdown string) string` / `func PlainExcerpt(markdown string, maxRunes int) string`
- **职责**：剥成可读的纯文本结构；`PlainExcerpt` 在 `maxRunes` 之外追加省略号 "…"
- **流程**：`render(markdown, dialectPlain, 0)`；超过 `maxRunes` 时 `truncateRunes(plain, maxRunes-1) + "…"`

### `Slack / SlackSections`
- **签名**：`func Slack(markdown string) string` / `func SlackSections(markdown string) []string`
- **职责**：转 Slack mrkdwn；`SlackSections` 按段落聚合到 ≤2900 rune 的 section，最多 45 个
- **流程**：`Slack` 直接 `render(markdown, dialectSlack, 0)`；`SlackSections` 按 `\n\n` 切段，逐段判断是否能塞进当前 section（加 separator），超长段落先转 plain 再按 `slackSectionMaxRunes` 切片
- **错误处理**：空 body 返回 nil；超长段落转 plain 切分（避免切坏 mrkdwn 链接/强调标记）

### `TelegramHTML`
- **签名**：`func TelegramHTML(markdown string) string`
- **职责**：转 Telegram HTML，带 4096 rune 上限，保持标签平衡
- **流程**：`render(markdown, dialectTelegramHTML, telegramMaxRunes)`

### `render`
- **签名**：`func render(markdown string, d dialect, maxRunes int) string`
- **职责**：goldmark 解析 → AST → renderer 遍历
- **流程**：
  1. `TrimSpace`；空 → 返回 ""
  2. `goldmark.New(WithExtensions(GFM))` 解析
  3. `renderer{...}` 遍历 `renderChildren(doc, 0)`
  4. `TrimSpace` 后返回

### `renderer.renderBlock`
- **职责**：分派 AST 节点类型到对应渲染逻辑
- **支持节点**：Heading、Paragraph/TextBlock、Blockquote（quoteDepth+1）、List、FencedCodeBlock/CodeBlock、ThematicBreak、Table、HTMLBlock、默认递归
- **HTMLBlock 处理**：`stripTags` 剥成纯文本后 `escapeText`，杜绝 LLM HTML 变成 provider 标记

### `renderer.renderInline / writeStyled`
- **职责**：内联节点（Text/Emphasis/Strikethrough/CodeSpan/Link/AutoLink/Image/RawHTML）的样式叠加与方言输出
- **RawHTML**：直接 `return`，不透传到 IM parse mode
- **writeStyled**：按 dialect 包裹 `*`/`_`/`~`（Slack）或 `<b>`/`<i>`/`<s>`/`<code>`（Telegram），链接按 `<url|label>` / `<a href>` 输出

### `renderer.writeBlock`（截断逻辑）
- **maxRunes ≤ 0**：直接写入
- **maxRunes > 0**：计算 remaining；整块塞得下就写；塞不下时 `truncateRunes(stripTags(block), remaining-1)` + "…"，并标记 `stopped=true`（避免切断 HTML 标签）

### 辅助函数
- `escapeText` / `escapeSlack` / `escapeSlackURL`：方言转义
- `safeLink`：仅允许 http/https/mailto，其余返回 ""（防 javascript: 等）
- `safeLanguage`：代码块语言白名单（仅 `[a-z0-9_-]`）
- `stripTags`：用 `x/net/html` tokenizer 提取纯文本
- `truncateRunes`：按 rune 计数截断（CJK 友好）
- `plainInline`：AST Walk 提取纯文本（跳过 RawHTML）
- `itoa`：手写 int→string，避免 strconv 引入

## 5. 依赖关系

- **外部库**：`github.com/yuin/goldmark` + `extension.GFM`、`golang.org/x/net/html`
- **被调用方**：`imbridge/provider/slack`（Slack/SlackSections/PlainExcerpt）、`telegram`（TelegramHTML）、`feishu`、`dingtalk`

## 6. 并发与资源管理

- **无状态**：所有导出函数纯函数式，无共享状态；`renderer` 是栈上结构，每次 `render` 新建
- **无 ctx**：纯 CPU 计算，无 IO，不需要取消
- **strings.Builder**：避免字符串拼接的二次拷贝

## 7. 设计模式与亮点

- **dialect 枚举驱动**：单一遍历器按 `r.dialect` 切换输出形态，避免为每个 IM 维护独立 renderer
- **maxRunes 截断防标签切断**：整块塞不下时回退到 `stripTags + 截断 + …`，保证 Telegram HTML 标签平衡
- **RawHTML 丢弃**：注释明示"Do not pass model-authored HTML through to an IM parse mode"——LLM 生成的 HTML 一律剥成纯文本后转义
- **safeLink 白名单**：仅 http/https/mailto，挡掉 `javascript:` 等 XSS 向量
- **SlackSections 段落聚合**：按 `\n\n` 切段后贪心塞进 section，超长段落降级 plain 再切，保证不切坏 mrkdwn 语法
- **CJK 友好**：所有长度限制按 rune 计数（`utf8.RuneCountInString`、`[]rune` 切片）

## 8. 注意事项

- **Telegram 4096 上限**：注释明示 HTML parse mode 选用原因——转义面比 MarkdownV2 小且编辑稳定
- **Slack 2900/45 上限**：Block Kit section 限制；超长不可分割块降级 plain
- **`sortedKeys` 占位**：`var _ = sortedKeys` 仅供测试使用，未导出
- **goldmark GFM 扩展**：支持表格、删除线、任务列表、自动链接
- **HTMLBlock 处理**：LLM 偶发输出 `<div>` 等 HTML 块，剥成纯文本后按方言转义，绝不透传
- **代码块语言**：Telegram 用 `<pre><code class="language-xxx">`，Slack 用 ```` ``` ```` 包裹并把内嵌 ```` ``` ```` 换成 `'''`
