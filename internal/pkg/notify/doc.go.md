# `doc.go` 技术实现文档（notify）

> 源文件：`internal/pkg/notify/doc.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/notify`

## 1. 概述

该文件是 `notify` 包的文档声明文件，无任何代码实现，仅以 Go 包注释说明包的定位：提供 outbound 通知的传输适配器，刻意业务无关——alerting、定时任务、AIOps proactive jobs 应传入归一化的 `Message` 给 `Sender`，而非直接 import Slack / Feishu / DingTalk / 通用 webhook 细节。

## 2. 包信息

- **包名**：`notify`
- **所属模块**：`internal/pkg/`（基础设施层）
- **依赖方向**：纯文档，无依赖。

## 3. 关键类型与接口

无显著类型定义（文件仅含包注释，无任何声明）。

## 4. 关键函数与流程

无关键函数。文档明确了包的边界：
- **包内职责**：传输适配器（webhook / Slack / Feishu / DingTalk / WeCom / Telegram）+ Router 路由。
- **包外职责**：业务逻辑（alerting、scheduled tasks、AIOps proactive）由调用方构造 `Message` 后传入。

## 5. 依赖关系

- **内部包**：无。
- **外部库**：无。
- **被调用方**：无（仅作为包文档被 `go doc` / IDE 读取）。

## 6. 并发与资源管理

无并发控制（无代码）。

## 7. 设计模式与亮点

- **业务无关传输层**：包刻意不耦合任何业务（alerting / scheduled / AIOps），仅提供 `Sender` 接口与 `Message` 归一化结构，符合分层架构与 BC 隔离原则。
- **归一化 Message**：所有渠道接收同一 `Message` 结构，渠道差异（payload 形状、签名机制）由各 Sender 内部消化，调用方零感知。
- **扩展点明确**：新增渠道只需实现 `Sender` 接口并注册到 Router，无需改动调用方代码。
- **包名 backend-neutral**：`notify` 而非 `slack` / `feishu`，让多渠道共存自然。

## 8. 注意事项

- **文档与实现可能漂移**：注释列出的渠道（Slack / Feishu / DingTalk / webhook）需与 `webhook.go` 实际实现同步；新增渠道（如 WeCom / Telegram）需同步更新本注释。
- **`Sender` 接口定义在 `notify.go`**：本文件不定义类型，仅声明包定位；具体接口与实现见同包其他文件。
- **业务无关边界**：调用方若绕过 `Message` 直接构造渠道特定 payload，会破坏归一化抽象；review 时需警惕。
- **无传输可靠性保证**：文档未提及重试 / 至少一次投递；当前实现（`webhook.go`）单次 POST 无重试，业务方需自行处理失败。
- **无审计落库**：包仅负责投递，事件审计由调用方写 `alert_events` 表（注释中提到 "log channel 2026-05 移除"，正是这一职责剥离的体现）。
