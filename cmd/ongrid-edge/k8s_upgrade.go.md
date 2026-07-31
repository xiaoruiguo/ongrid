# `k8s_upgrade.go` 技术实现文档

> 源文件：`cmd/ongrid-edge/k8s_upgrade.go`
> 包路径：`github.com/ongridio/ongrid/cmd/ongrid-edge`

## 1. 概述

本文件实现 `ongrid-edge` 二进制的 `prepare-k8s-upgrade` 子命令入口。该子命令用于 Kubernetes 部署模式下的滚动升级前置准备（如等待 Deployment 就绪、切换 telemetry gateway / metrics 模式等），实际逻辑由 `internal/edgeagent/k8s.PrepareUpgrade` 承担，本文件只负责命令行参数解析与上下文构造。

## 2. 包信息

- **包名**：`main`
- **所属模块**：`cmd/ongrid-edge`（命令入口层）
- **依赖方向**：被同包 `main.go` 的 `main()` 在启动早期调用；调用 `internal/edgeagent/k8s` 子包

## 3. 关键类型与接口

无显著类型定义（仅使用标准库 `flag.FlagSet`）。

## 4. 关键函数与流程

### `runK8sUpgradeCommand`
- **签名**：`func runK8sUpgradeCommand(ctx context.Context, args []string) (bool, error)`
- **职责**：识别 `prepare-k8s-upgrade` 子命令并解析其参数，构造 `UpgradePreparationConfig` 后委托 `edgek8s.PrepareUpgrade` 执行
- **流程**：
  1. `args[0] != "prepare-k8s-upgrade"` → 返回 `(false, nil)` 表示未处理，由调用方继续走其他子命令分支
  2. 使用 `flag.NewFlagSet("prepare-k8s-upgrade", flag.ContinueOnError)` 解析以下参数：
     - `-namespace`：发布命名空间
     - `-controller`：controller Deployment 名称
     - `-metrics-scraper`：metrics scraper Deployment 名称
     - `-gateway-mode`：目标 telemetry gateway 模式
     - `-metrics-mode`：目标 Kubernetes metrics 模式
     - `-timeout`：准备阶段总超时，默认 8 分钟
  3. `flags.SetOutput(io.Discard)` 抑制 flag 包自带的错误输出
  4. 校验：`NArg() != 0` 拒绝多余位置参数；`timeout <= 0` 拒绝非正超时
  5. 用 `context.WithTimeout(ctx, *timeout)` 派生准备上下文
  6. 调用 `edgek8s.PrepareUpgrade(prepareCtx, edgek8s.UpgradePreparationConfig{...})`
- **错误处理**：
  - flag 解析失败 → `fmt.Errorf("parse prepare-k8s-upgrade arguments: %w", err)`
  - 多余参数 → `fmt.Errorf("prepare-k8s-upgrade: unexpected arguments %q", ...)`
  - 非正超时 → `fmt.Errorf("prepare-k8s-upgrade: timeout must be positive")`
  - `PrepareUpgrade` 的错误原样向上传播；调用方（`main.go`）在 `err != nil` 时打印并 `os.Exit(1)`

## 5. 依赖关系

- **内部包**：
  - `github.com/ongridio/ongrid/internal/edgeagent/k8s`（别名 `edgek8s`）：调用 `PrepareUpgrade` 与 `UpgradePreparationConfig`
- **外部库**：
  - `flag`：命令行参数解析
  - `context`：超时上下文
  - `fmt`、`io`、`strings`、`time`：辅助
- **被调用方**：`main.go` 的 `main()` 函数

## 6. 并发与资源管理

无并发控制。`context.WithTimeout` 派生的 `prepareCtx` 通过 `defer cancel()` 释放，确保 `PrepareUpgrade` 即使长耗时也不会泄漏定时器。

## 7. 设计模式与亮点

- **子命令分发模式**：返回 `(handled bool, err error)` 二元组，让 `main()` 用顺序 `if` 链尝试多个子命令分发器（`runK8sHostCommand` → `runK8sUpgradeCommand` → 正常 agent 启动），避免在 main 函数里堆叠 flag 解析
- **关注点分离**：本文件仅做参数解析与上下文构造，真正的升级编排逻辑放在 `internal/edgeagent/k8s`，符合 `cmd → ... → controlplane/repo` 分层

## 8. 注意事项

- 默认 8 分钟超时是经验值，覆盖典型 K8s 滚动更新时长；复杂环境可通过 `-timeout` 显式延长
- `flags.SetOutput(io.Discard)` 抑制了 flag 包默认的错误输出，所有错误都通过返回值传递，便于 `main()` 统一格式化打印
- 如需新增升级前置步骤，应修改 `edgek8s.PrepareUpgrade`，而非在此处堆积逻辑
