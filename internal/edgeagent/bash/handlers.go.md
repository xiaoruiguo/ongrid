# `handlers.go` 技术实现文档

> 源文件：`internal/edgeagent/bash/handlers.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/bash`

## 1. 概述

本文件为 edge agent 注册 `MethodBashExec` 隧道处理器，将云端 LLM 发起的 bash 执行请求路由到 `cmdpolicy.Sandbox`。它是一个薄壳层：解析 wire 请求、调用 sandbox、把 `ShellResult` 翻译回 `BashExecResponse` 返回。在启动期装配策略（DefaultReadOnly + 可选的 `/etc/ongrid-edge/bash-policy.yaml` 覆盖）和路径校验器（复用 `host_files.SandboxConfig`）。

## 2. 包信息

- **包名**：`bash`
- **所属模块**：edgeagent 业务能力层（与 host_files、cmdpolicy 同级）
- **依赖方向**：被 `cmd/ongrid-edge` 主程序在启动时调用 `Register`；调用 `cmdpolicy`、`host_files`、`tunnel`

## 3. 关键类型与接口

无显著类型定义。仅常量 `DefaultPolicyOverridePath = "/etc/ongrid-edge/bash-policy.yaml"` 指定 operator 覆盖路径。

## 4. 关键函数与流程

### `Register`
- **签名**：`func Register(client tunnel.Client, log *slog.Logger) error`
- **职责**：装配 Sandbox 并向隧道注册 `MethodBashExec` handler
- **流程**：
  1. 取 `cmdpolicy.DefaultReadOnly()` 作为基线策略
  2. 若 `/etc/ongrid-edge/bash-policy.yaml` 存在 → `LoadFromYAML` 合并；失败仅告警并回退基线（保证拼写错误不会拖垮 agent）
  3. 取 `host_files.DefaultSandboxConfig()` 作为 PathValidator；validate 失败时降级为 nil（绝对路径参数校验被禁用，但仍可执行纯系统读命令如 ps/df）
  4. 构造 `cmdpolicy.Sandbox` 并通过 `client.RegisterHandler` 注册 `makeHandler` 返回的 `tunnel.Handler`
- **错误处理**：始终返回 nil（DefaultReadOnly 必成功；path validator 失败时降级而非报错）

### `makeHandler`
- **签名**：`func makeHandler(sandbox *cmdpolicy.Sandbox, log *slog.Logger) tunnel.Handler`
- **职责**：返回闭包 handler，按 wire 协议处理单次 bash 请求
- **流程**：
  1. JSON 解码 `BashExecRequest`；空 body 跳过解码
  2. 校验 `req.Cmd` 非空
  3. 处理可选超时覆盖：`req.Timeout` > 0 时建立 `context.WithTimeout`，上限 5 分钟，防止卡住隧道 slot
  4. 根据 `req.Unrestricted` 分流：
     - true（admin write gate 开启）→ `sandbox.ExecRaw`，WARN 级别记录审计轨迹
     - false → `sandbox.Exec`
  5. 将 `ShellResult` 序列化为 `BashExecResponse` 返回
- **错误处理**：所有错误通过 `fmt.Errorf("bash: ...: %w", err)` 包装上抛到隧道层

## 5. 依赖关系

- **内部包**：`internal/edgeagent/cmdpolicy`、`internal/edgeagent/host_files`、`internal/pkg/tunnel`
- **外部库**：标准库 `encoding/json`、`log/slog`、`os`、`time`
- **被调用方**：`cmd/ongrid-edge` 主程序

## 6. 并发与资源管理

handler 闭包本身无状态，每个请求独立 `context.Context`。`req.Timeout` 触发的 `context.WithTimeout` 通过 `defer cancel()` 释放。Sandbox 是值类型共享，其内部 Policy 不可变（构造后只读），PathValidator 同样只读。

## 7. 设计模式与亮点

- **降级而非崩溃**：策略覆盖加载失败、PathValidator 不健康时都继续注册 handler，保证 agent 启动路径在任何环境下不崩溃；缺失能力会以 `Allowed=false` + Reason 透明暴露给审计日志
- **双路径分流**：`Unrestricted` 标志区分「受策略约束的 LLM 调用」和「operator 开启 write gate 后的全权限调用」，后者以 WARN 日志形成审计轨迹
- **共享 PathValidator**：复用 `host_files` 的允许列表，让 bash、find_large_files、du_summary 看到同一组 `/var /opt /home /tmp /srv /data` 前缀
- **测试钩子分离**：`makeHandler` 独立于 `Register`，便于单测直接注入 sandbox 而不依赖真实文件系统

## 8. 注意事项

- `bundleDownloadClient` 类似的 `http.DefaultClient` 在 `ExecRaw` 路径下仍受 5 分钟硬上限保护
- PathValidator 降级为 nil 时绝对路径参数不再校验，纯系统读命令（无 `/` 前缀参数）不受影响
- `/etc/ongrid-edge/bash-policy.yaml` 解析错误只记 Warn 不中断启动；运维需关注日志中的 `policy override load failed`
- `Unrestricted=true` 路径完全绕过 cmdpolicy（含二进制 allowlist、路径 allowlist、shell 元字符语法），仅 caps + per-call timeout 生效——这是 write gate 的设计意图
