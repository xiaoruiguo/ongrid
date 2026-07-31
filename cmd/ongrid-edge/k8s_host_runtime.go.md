# `k8s_host_runtime.go` 技术实现文档

> 源文件：`cmd/ongrid-edge/k8s_host_runtime.go`
> 包路径：`github.com/ongridio/ongrid/cmd/ongrid-edge`

## 1. 概述

本文件实现 `ongrid-edge` 二进制的两个 K8s 主机运行时子命令：`install-k8s-host-runtime`（在主机上铺设 edge 二进制、插件、serviceaccount 文件）与 `enter-k8s-host`（容器内进程切换到主机命名空间后 exec 主机上的 edge 二进制）。两者配合实现"容器化部署但 edge 进程跑在主机上"的运行模式，常见于需要 edge 直接访问主机 systemd / 进程 / 文件系统的场景。

## 2. 包信息

- **包名**：`main`
- **所属模块**：`cmd/ongrid-edge`（命令入口层）
- **依赖方向**：被同包 `main.go` 的 `main()` 在启动早期调用；不依赖项目内业务包，仅用标准库

## 3. 关键类型与接口

```go
// 描述一次主机运行时安装所需的所有源/目标路径与归属 uid/gid
type k8sHostInstallPaths struct {
    hostRoot             string // 主机根挂载点（容器内的 /host/root 之类）
    edgeSource           string // 容器内 edge 二进制路径（os.Executable()）
    pluginSourceDir      string // 容器内插件目录 /usr/local/lib/ongrid-edge
    serviceAccountSource string // 容器内 serviceaccount 目录 /var/run/secrets/...
    uid, gid             int    // 主机 edge 运行账户
}
```

## 4. 关键函数与流程

### `runK8sHostCommand`
- **签名**：`func runK8sHostCommand(ctx context.Context, args []string) (bool, error)`
- **职责**：子命令分发器，识别 `install-k8s-host-runtime` / `enter-k8s-host`
- **流程**：
  1. `args` 为空 → `(false, nil)`，让 `main()` 走正常 agent 启动分支
  2. `install-k8s-host-runtime`：校验参数数量（必须 4 个）、解析 uid/gid、`os.Executable()` 取容器内 edge 二进制路径，调用 `installK8sHostRuntime`
  3. `enter-k8s-host`：同样校验 + 解析 uid/gid，调用 `enterK8sHost`（Linux 实现见 `k8s_host_runtime_linux.go`）
  4. 其他命令 → `(false, nil)`
- **错误处理**：参数数量不符 / uid gid 非法 → 直接返回错误；底层函数错误向上透传

### `installK8sHostRuntime`
- **签名**：`func installK8sHostRuntime(ctx context.Context, paths k8sHostInstallPaths) error`
- **职责**：在主机文件系统上铺设 edge 运行所需目录与文件
- **流程**：
  1. `ctx.Err()` 早退；校验 `hostRoot` 存在且是目录
  2. 在主机上创建 `/var/lib/ongrid-edge/k8s-runtime/{,plugins,serviceaccount}` 目录（0755）
  3. 创建 `/var/lib/ongrid-edge/k8s-state/{,credentials,plugins,.upgrade}` 状态目录，并通过 `ensureOwnedDirectory` 设置 0750 权限 + `Chown(uid, gid)`
  4. `copyFileAtomic` 把 edge 二进制从容器拷到主机 `/var/lib/ongrid-edge/k8s-runtime/ongrid-edge`（0755）
  5. `copyRegularFiles` 把容器内插件目录所有常规文件拷到主机插件目录
  6. 把 `token / ca.crt / namespace` 三个 serviceaccount 文件拷到主机（0400）并 `Chown(uid, gid)`
- **错误处理**：每一步都用 `fmt.Errorf("...: %w", err)` 包装上下文，便于定位是哪一步失败

### `copyFileAtomic`
- **签名**：`func copyFileAtomic(ctx context.Context, source, destination string, mode os.FileMode) (retErr error)`
- **职责**：原子化文件拷贝，避免半成品文件被其他进程读到
- **流程**：在目标目录创建临时文件 → `io.Copy` → `Chmod` → `Sync`（落盘）→ `Close` → `os.Rename` 替换目标；任一步失败由 `defer` 清理临时文件
- **错误处理**：用命名返回值 `retErr` + `defer` 模式确保关闭 / 删除临时文件时也能把错误透传；`defer` 内对 `os.Remove` 的错误用 `!os.IsNotExist(err)` 过滤

### `hostPath`
- **签名**：`func hostPath(root, absolutePath string) string`
- **职责**：把绝对路径前缀替换为 `root` 拼接，实现"容器视角的绝对路径"→"主机视角的绝对路径"

### `ensureOwnedDirectory`
- **签名**：`func ensureOwnedDirectory(path string, uid, gid int, mode os.FileMode) error`
- **职责**：`MkdirAll` + `Chmod` + `Chown` 三步，确保状态目录归 edge 运行账户所有

## 5. 依赖关系

- **内部包**：无（纯标准库）
- **外部库**：
  - `context`、`fmt`、`io`、`os`、`path/filepath`、`strconv`、`strings`：标准库
- **被调用方**：`main.go` 的 `main()`；底层 `enterK8sHost` 由平台分支文件提供

## 6. 并发与资源管理

无并发控制。`copyFileAtomic` 用 `defer` 严格管理临时文件句柄与残留清理；`ctx.Err()` 在循环拷贝（`copyRegularFiles`）中作为可取消点。

## 7. 设计模式与亮点

- **原子写模式**：`copyFileAtomic` 是经典的"写临时文件 + fsync + rename"原子替换模式，保证目标文件要么是旧内容要么是新内容，不会出现半写状态
- **路径前缀重写模式**：`hostPath` 是容器 → 主机路径映射的简洁实现，避免在业务代码里到处拼接
- **命令分发器模式**：与 `k8s_upgrade.go` 一样返回 `(handled, err)`，让 `main()` 用统一方式串联多个子命令

## 8. 注意事项

- **安全敏感**：`install-k8s-host-runtime` 需要容器具备主机文件系统写权限（hostPath 挂载）和 `Chown` 能力，部署时需配合适当的 SecurityContext / PodSecurityPolicy
- **serviceaccount 文件权限**：`token` 设为 0400 且属主为 edge uid，避免其他主机进程读取 K8s 凭据
- **状态目录权限**：0750 比 runtime 目录的 0755 更严格，因为 `credentials` 子目录会存放 edge 凭据
- **跨平台**：`enterK8sHost` 在非 Linux 上由 `k8s_host_runtime_other.go` 提供占位实现，但 `installK8sHostRuntime` 本身不依赖 Linux 特性，理论上可在任意平台运行（实际只用于 Linux 主机场景）
- `copyFileAtomic` 的 `defer` 内若 `Close` 与 `Remove` 同时出错，会优先保留 `Close` 错误（更接近真实失败原因）
