# `doc.go` 技术实现文档

> 源文件：`internal/pkg/llm/doc.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/llm`

## 1. 概述

本文件仅为 `llm` 包的包级文档注释，不含可执行代码。说明该包是 OpenAI 风格的 chat / tool-calling 客户端，带 per-day token 预算钩子与 Prometheus 监控。明示三条红线：无 provider 抽象、Prom label 禁含高基数字段、永不记录用户消息内容。

## 2. 包信息

- **包名**：`llm`
- **所属模块**：`internal/pkg/`
- **依赖方向**：被 AIOps agent loop 调用；本文件不引入任何依赖

## 3. 关键类型与接口

无显著类型定义（仅包注释）。

## 4. 关键函数与流程

无（纯文档文件）。

文档要点（三条红线）：
- **无 provider 抽象**：接口形状跟随 OpenAI；SDK 是 `github.com/sashabaranov/go-openai`；无自动重试（tools 非幂等）；无 streaming
- **Prom label 禁含高基数**：`user_id` / `org_id` / `session_id` 禁止作为 label；仅 `model` / `kind` / `result`
- **永不记录用户消息内容**：仅记录 token 计数、tool call 名与计数、duration、user_id（非 PII）

## 5. 依赖关系

- **内部包**：无
- **外部库**：无（文档文件）
- **被调用方**：N/A（仅文档）

## 6. 并发与资源管理

无并发控制（纯文档文件）。

## 7. 设计模式与亮点

- **红线显式化**：把三条架构红线写在包注释顶部，让维护者第一时间看到约束
- **决策留痕**：解释"无 provider 抽象"是为了接口跟随 OpenAI 形状，避免抽象泄漏
- **安全姿态明示**：明确 user_id 是"safe — not PII"，但消息内容永不记录

## 8. 注意事项

- **红线是硬约束**：任何新增代码不得违反三条红线；新增 Prom label 需评估基数
- **无 streaming 当前限制**：注释明示"no streaming"；未来若引入 streaming 需评估预算门控与监控
- **无自动重试**：注释明示"tools are not idempotent"；caller 不应假设重试安全
