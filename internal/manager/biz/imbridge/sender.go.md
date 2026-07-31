# `sender.go` 技术实现文档

> 源文件：`internal/manager/biz/imbridge/sender.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/imbridge`

## 1. 概述

本文件定义 IM 桥接器的出站抽象 + 流式编辑器：把 agent SSE 事件流转换成 throttled 的 `EditText` 调用。设计要点：假设 assistant 文本单调增长（始终发完整 buffer 而非 delta，因飞书/钉钉 edit 是替换非追加）；throttle 800ms 间隔； DingTalk 等 one-shot provider 无 MessageEditor，走 SendText 一次发完。红线：tool/task 通知在 IM 中丢弃（太碎会扰乱消息）。

## 2. 包信息

- **包名**：`imbridge`
- **所属模块**：`internal/manager/biz/imbridge`
- **依赖方向**：被各 provider 的 stream.go 间接通过 Bridge 使用；依赖 `aiops/agent`（Event）

## 3. 关键类型与接口

```go
// Sender 是最小平台无关出站接口
type Sender interface {
    SendText(ctx context.Context, receiveID, receiveIDType, text string) (messageID string, err error)
}

// MessageEditor 由支持编辑的 provider 实现（飞书/Telegram/Slack）
// DingTalk 的 session webhook 只能创建消息，故意不实现
type MessageEditor interface {
    EditText(ctx context.Context, messageID, text string) error
}

// streamEditor 把 SSE-shape agent.Event 流转成 throttled EditText 调用
type streamEditor struct {
    ctx           context.Context
    sender        Sender
    editor        MessageEditor // nil if sender 不实现（one-shot provider）
    chatID        string
    receiveIDType string
    messageID     string // 初始占位 id；占位发送失败则 ""
    locale        string // 驱动 OnFatal 道歉语言
    log           *slog.Logger

    mu     sync.Mutex
    buf    string
    lastAt time.Time
}

const (
    editIntervalMs   = 800  // throttle 间隔
    editCharsTrigger = 200  // 字符增量触发（注释标记但代码用时间触发）
)
```

## 4. 关键函数与流程

### `newStreamEditor`
- **签名**：`func newStreamEditor(ctx, sender, chatID, receiveIDType, placeholderMessageID, locale string, log) *streamEditor`
- **职责**：构造编辑器；类型断言 sender 是否实现 MessageEditor
- **流程**：`editor, _ := sender.(MessageEditor)` —— 不实现则为 nil

### `streamEditor.OnEvent`
- **签名**：`func (e *streamEditor) OnEvent(ev agent.Event)`
- **职责**：agent 事件回调，按类型分派
- **流程**：
  - `EventAssistant`：
    1. `assistantText(ev)` 提取文本；空 → return
    2. `mu.Lock`；`e.buf = text`（**替换而非追加**，因 agent.Event 携带累积 turn）
    3. `shouldFlush := e.editor != nil && e.shouldFlushLocked()`（仅可编辑 provider 才流式 flush）
    4. Unlock；shouldFlush → `e.flush()`
  - `EventDone`：`e.flush()`（终态，确保最后一块落地）
  - 默认（EventToolStart/EventToolEnd/EventTaskNotification）：丢弃，注释明示"在 IM 中会碎片化消息并困扰用户"
- **注释**：agent.Event.Assistant 携带持久化的完整 assistant turn（非 per-token delta），所以直接取 Content；若 runtime 暴露 per-chunk delta 应切换避免二次 edit 流量

### `streamEditor.OnFatal`
- **签名**：`func (e *streamEditor) OnFatal(err error) error`
- **职责**：agent run 失败时替换占位消息为错误提示
- **流程**：
  1. prefix 按 locale 选 "⚠ 助手执行失败：" / "⚠ Assistant failed: "
  2. `mu.Lock`；`e.buf = prefix + err.Error()`；Unlock
  3. `e.flush()`

### `streamEditor.Flush`
- **签名**：`func (e *streamEditor) Flush() error`
- **职责**：bridge 的 final post-run nudge，确保最后 buf 已写
- **流程**：`return e.flush()`

### `streamEditor.shouldFlushLocked`
- **签名**：`func (e *streamEditor) shouldFlushLocked() bool`
- **职责**：throttle 判定
- **流程**：`time.Since(e.lastAt) >= editIntervalMs * time.Millisecond`

### `streamEditor.flush`
- **签名**：`func (e *streamEditor) flush() error`
- **职责**：实际发送/edit 消息
- **流程**：
  1. `mu.Lock`；取 buf + mid；`e.lastAt = time.Now()`；Unlock
  2. `buf == ""` → return nil
  3. `mid == ""`（无占位）：`sender.SendText(ctx, chatID, receiveIDType, buf)` → 记录 newID；失败 Warn
  4. `e.editor == nil`（one-shot provider）：return nil（注释：已一次发完，EventDone 和 bridge Flush 都到达时不重复）
  5. `e.editor.EditText(ctx, mid, buf)`；失败 Warn
- **错误处理**：SendText/EditText 失败 Warn 并返回 error；不重试

### `assistantText`
- **签名**：`func assistantText(ev agent.Event) string`
- **职责**：从 EventAssistant payload 提取持久化 assistant 文本
- **流程**：`ev.Assistant == nil` → ""；返回 `ev.Assistant.Content`
- **注释**：避免直接类型断言字段形状以保持前向兼容

## 5. 依赖关系

- **内部包**：`biz/aiops/agent`（Event/EventAssistant/EventDone 等）
- **外部库**：仅标准库
- **被调用方**：Bridge.HandleInbound 内部构造 streamEditor 作为 agent.RunStreamWithOpts 的 emit callback

## 6. 并发与资源管理

- **`mu`（Mutex）**：保护 buf / lastAt / messageID
- **ctx 透传**：构造时传入的 ctx 用于 SendText/EditText；agent run 的 ctx
- **单调 buf**：注释明示"we always send the full accumulated buffer, never deltas"
- **无 goroutine**：OnEvent 在 agent run 的 goroutine 中调用；flush 同步

## 7. 设计模式与亮点

- **类型断言探测 MessageEditor**：`editor, _ := sender.(MessageEditor)`，one-shot provider（DingTalk）自然走 SendText 路径
- **单调 buf 替换**：agent.Event 携带完整 turn，直接替换避免 delta 累积的复杂度
- **throttle 仅时间触发**：`shouldFlushLocked` 只看 `editIntervalMs`，注释提到 `editCharsTrigger=200` 但未启用（保留常量）
- **OnFatal locale 感知**：按 app.DefaultLocale 选道歉语言
- **one-shot 防重复**：`e.editor == nil` 时 return nil，EventDone + bridge Flush 双到达不重复发
- **占位失败回退**：mid=="" 时走 SendText 路径，记 newID 后续 edit 复用

## 8. 注意事项

- **editIntervalMs=800**：流式更新间隔；太短撞 API rate limit，太长用户感知卡顿
- **editCharsTrigger=200 未启用**：常量保留但 shouldFlushLocked 仅看时间
- **agent.Event 累积语义**：注释明示若 runtime 改为 per-chunk delta 需切换避免二次 edit 流量
- **tool/task 通知丢弃**：IM 中太碎，仅 EventAssistant + EventDone 处理
- **locale 驱动 OnFatal**：空/非 "en" 用中文，"en" 用英文
- **flush 失败不重试**：Warn 后返回 error，下次 OnEvent 会再尝试
- **messageID 状态机**：初始占位 id → 占位失败 "" → SendText 成功后更新 newID → 后续 EditText 复用
