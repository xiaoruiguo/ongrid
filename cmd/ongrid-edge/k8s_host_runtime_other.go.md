# `k8s_host_runtime_other.go` 技术实现文档

> 源文件：`cmd/ongrid-edge/k8s_host_runtime_other.go`
> 包路径：`github.com/ongridio/ongrid/cmd/ongrid-edge`

## 1. 概述

本文件是 `enterK8sHost` 函数在非 Linux 平台下的占位实现。通过 Go 的 build tag 机制，在非 Linux 编译目标上提供符号定义，避免因平台差异导致的链接错误；调用方会得到一个明确的"仅在 Linux 上支持"错误。

## 2. 包信息

- **包名**：`main`
- **所属模块**：`cmd/ongrid-edge`（命令入口层，对应 `cmd → ...` 分层中的最顶层）
- **依赖方向**：被同包 `k8s_host_runtime.go` 中的 `runK8sHostCommand` 通过 `enterK8sHost` 调用；不依赖任何项目内业务包

## 3. 关键类型与接口

无显著类型定义。

## 4. 关键函数与流程

### `enterK8sHost`
- **签名**：`func enterK8sHost(context.Context, string, int, int) error`
- **职责**：在非 Linux 平台上拒绝执行进入 K8s 主机操作
- **流程**：直接返回 `fmt.Errorf("entering the kubernetes host is supported only on linux")`
- **错误处理**：返回一个明确错误，调用方据此给出失败语义；与 `k8s_host_runtime_linux.go` 中的实现签名完全一致，由编译器按 build tag 选择

## 5. 依赖关系

- **内部包**：无
- **外部库**：
  - `context`：仅用于占位参数类型
  - `fmt`：构造错误信息
- **被调用方**：`runK8sHostCommand`（`k8s_host_runtime.go`）

## 6. 并发与资源管理

无并发控制。

## 7. 设计模式与亮点

- **Build Tag 模式**：通过 `//go:build !linux` 与 `k8s_host_runtime_linux.go` 的 `//go:build linux` 形成一对平台分支，是 Go 跨平台代码组织的标准做法
- **接口对齐**：与 Linux 版本保持完全相同的函数签名，使调用方代码无需感知平台差异

## 8. 注意事项

- 该文件仅作平台占位；任何对 `enterK8sHost` 签名的修改必须同步修改 `k8s_host_runtime_linux.go`
- 在 Windows / macOS 等平台运行 `ongrid-edge enter-k8s-host` 子命令会得到此错误，符合预期（该操作本质上依赖 Linux 的 chroot / setns / capability）
