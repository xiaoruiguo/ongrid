# `locale.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/basetool/locale.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools/basetool`

## 1. 概述

本文件实现 UI locale 的 ctx 传播。chatruntime 在 coordinator graph 的 ctx 上 set locale；`tools/agent_tool.go` 在 `InvokableRun` 内 read 并 forward 到 sub-agent 的 `SpawnWorkerRequest`。chatruntime 无法 import tools/agent_tool（cmd-side 装配方向相反，反向 import 会闭依赖环），所以需要 basetool 作为两边都依赖的叶子包。无此 seam 时，处理英文问题的 coordinator 会交给默认中文（GLM）回答的 specialist——见 feedback_ai_output_locale.md 2026-06-02 regression。

## 2. 包信息

- **包名**：`basetool`
- **所属模块**：叶子包
- **依赖方向**：被 chatruntime（set）、`tools/agent_tool`（read）调用；依赖标准库 `context`

## 3. 关键类型与接口

```go
type localeCtxKeyT struct{}
var localeCtxKey = localeCtxKeyT{}
```

## 4. 关键函数与流程

### `WithLocale`
- **签名**：`func WithLocale(ctx context.Context, locale string) context.Context`
- **职责**：attach UI locale（如 "en"、"zh-CN"）到 ctx
- **流程**：`locale == ""` → no-op 保留 back-compat；否则 `context.WithValue` attach
- **错误处理**：无

### `LocaleFromContext`
- **签名**：`func LocaleFromContext(ctx context.Context) string`
- **职责**：取 locale，无则返回 `""`
- **流程**：类型断言 `ctx.Value(key).(string)`，失败返回零值

## 5. 依赖关系

- **内部包**：无
- **外部库**：标准库 `context`
- **被调用方**：`chatruntime`（producer）、`tools/agent_tool.go`（consumer）

## 6. 并发与资源管理

- 不可变字符串 ctx value，并发安全
- 无锁

## 7. 设计模式与亮点

- **叶子包作为反循环依赖 seam**：chatruntime 已为 wire-test 依赖 basetool，agent_tool 可零成本拾取
- **空 locale no-op**：保留 investigator auto-spawn 路径的 back-compat
- **解决跨语言 mismatch**：之前 coordinator 处理英文问题时 specialist 用 GLM 默认中文回答

## 8. 注意事项

- **locale 字符串格式**：跟随 UI 传值（"en"、"zh-CN" 等），由 sub-agent 端解读
- **空值 = 无指令**：sub-agent 用自身默认 locale
- **stamp 时机**：runtime 在 graph.Invoke 之前必须 set
