# `k8s_host_runtime_linux.go` 技术实现文档

> 源文件：`cmd/ongrid-edge/k8s_host_runtime_linux.go`
> 包路径：`github.com/ongridio/ongrid/cmd/ongrid-edge`

## 1. 概述

本文件是 `enterK8sHost` 函数在 Linux 平台下的实现。它将当前进程从容器命名空间切换到宿主机命名空间（chroot + 可选 setns 进入主机 mount namespace），按最小权限原则丢弃绝大部分 Linux capabilities，仅保留 `CAP_DAC_READ_SEARCH` 与 `CAP_NET_ADMIN`，然后以指定的 edge uid/gid `exec` 主机上的 edge 二进制。这是"容器部署、主机运行"模式的核心安全边界。

## 2. 包信息

- **包名**：`main`
- **所属模块**：`cmd/ongrid-edge`（命令入口层）
- **依赖方向**：被同包 `k8s_host_runtime.go` 的 `runK8sHostCommand` 调用；依赖 `golang.org/x/sys/unix` 系统调用封装

## 3. 关键类型与接口

无显著类型定义（仅常量与包级变量）。

```go
const (
    defaultLinuxLastCapability = 63                      // /proc/sys/kernel/cap_last_cap 读取失败时的兜底值
    procHostRoot               = "/proc/1/root"          // 旧版 chart 通过 hostPID 访问主机根
    procHostMountNamespace     = "/proc/1/ns/mnt"        // 主机 mount namespace 文件描述符
)

// 需要保留的 capability 列表（其余全部丢弃）
var k8sHostCapabilities = []int{
    unix.CAP_DAC_READ_SEARCH,  // 读取任意文件（绕过 DAC），用于读主机 /proc /sys 等
    unix.CAP_NET_ADMIN,        // 网络管理（重启服务、网络诊断等）
}
```

## 4. 关键函数与流程

### `enterK8sHost`
- **签名**：`func enterK8sHost(ctx context.Context, hostRoot string, uid, gid int) error`
- **职责**：将当前进程切换到主机命名空间并 exec 主机 edge 二进制
- **流程**：
  1. `ctx.Err()` 早退
  2. `unix.Open(hostRoot, O_RDONLY|O_DIRECTORY|O_CLOEXEC)` 打开主机根目录，获得文件描述符 `rootFD`
  3. 若 `hostRoot == /proc/1/root`（旧版 chart 路径），则打开 `/proc/1/ns/mnt` 获取主机 mount namespace 描述符 `mountNSFD`；否则跳过 setns 步骤
  4. `linuxLastCapability()` 读取 `/proc/sys/kernel/cap_last_cap` 获取本机最大 capability 编号
  5. `runtime.LockOSThread()` 锁定当前 goroutine 到 OS 线程——`Unshare` / `Setns` / `Chroot` / `Capset` 都影响整个线程而非进程，必须固定线程
  6. `unix.Unshare(CLONE_FS)` 隔离文件系统上下文（root/cwd/umask），避免影响父线程
  7. 若 `mountNSFD >= 0`，`unix.Setns(mountNSFD, CLONE_NEWNS)` 进入主机 mount namespace
  8. `unix.Fchdir(rootFD)` → `unix.Chroot(".")` → `unix.Chdir("/")` 切换根目录到主机
  9. `dropToHostEdgeUser(uid, gid, lastCapability)` 降权并保留必要 capabilities
  10. `unix.Exec(k8sHostEdgeBinary, []string{k8sHostEdgeBinary}, os.Environ())` 替换进程映像，启动主机 edge
- **错误处理**：每一步都用 `fmt.Errorf("...: %w", err)` 包装上下文；`defer` 关闭两个 FD；`runtime.UnlockOSThread` 在 `defer` 中释放线程锁定

### `requiresHostMountNamespace`
- **签名**：`func requiresHostMountNamespace(hostRoot string) bool`
- **职责**：判断是否需要 `setns` 进入主机 mount namespace
- **逻辑**：仅当 `hostRoot` 清洗后等于 `/proc/1/root` 时返回 true——这是旧版 chart 通过 hostPID 访问主机的路径，需要额外的 `setns`；新版 chart 用显式 hostPath 挂载，可直接 chroot

### `linuxLastCapability`
- **签名**：`func linuxLastCapability() int`
- **职责**：读取本机最大 capability 编号
- **流程**：读 `/proc/sys/kernel/cap_last_cap`；解析失败或负值时返回兜底值 63

### `dropToHostEdgeUser`
- **签名**：`func dropToHostEdgeUser(uid, gid, lastCapability int) error`
- **职责**：降权到 edge uid/gid，同时保留 `k8sHostCapabilities` 中列出的 capabilities
- **流程**（capability 降权是 Linux 安全的核心，步骤顺序敏感）：
  1. **丢弃 bounding set**：循环 `0..lastCapability`，对不在保留列表的 capability 调 `PR_CAPBSET_DROP`，使其无法在未来重新获取（`EINVAL` 忽略——某些编号在当前内核不存在）
  2. `PR_SET_KEEPCAPS=1`：在 `setresuid` 之后保留 permitted/effective 集合（默认 setuid 会清空 capabilities）
  3. `Setgroups(nil)` 清空附加组
  4. `Setresgid(gid, gid, gid)` 设置实际/有效/保存 gid
  5. `Setresuid(uid, uid, uid)` 设置实际/有效/保存 uid
  6. **重新声明保留 capabilities**：用 `Capset` 设置 effective/permitted/inheritable 三组，仅保留 `CAP_DAC_READ_SEARCH` + `CAP_NET_ADMIN`
  7. **ambient set**：`PR_CAP_AMBIENT + PR_CAP_AMBIENT_RAISE` 把保留的 capabilities 提升为 ambient，使 exec 出来的 edge 二进制（非 root、无 file capability）也能继承这些权限
- **错误处理**：每步都用 `fmt.Errorf("...: %w", err)` 包装

### `isK8sHostCapability`
- **签名**：`func isK8sHostCapability(capability int) bool`
- **职责**：线性查找某 capability 是否在保留列表内

## 5. 依赖关系

- **内部包**：无
- **外部库**：
  - `golang.org/x/sys/unix`：Linux 系统调用封装（Open / Close / Unshare / Setns / Fchdir / Chroot / Chdir / Prctl / Setgroups / Setresgid / Setresuid / Capset / Exec）
  - `context`、`errors`、`fmt`、`os`、`path/filepath`、`runtime`、`strconv`、`strings`：标准库
- **被调用方**：`runK8sHostCommand`（`k8s_host_runtime.go`）

## 6. 并发与资源管理

- **`runtime.LockOSThread`**：整个命名空间切换过程锁定在单个 OS 线程上，`defer runtime.UnlockOSThread()` 释放。这是 Go 中执行线程级系统调用的标准模式——`Unshare` / `Setns` / `Chroot` 只影响调用线程，Go 调度器可能随时把 goroutine 迁移到其他线程，不锁定会导致切换无效
- **文件描述符**：`rootFD` 与 `mountNSFD` 都通过 `defer unix.Close(fd)` 释放；`mountNSFD` 在不需要时为 -1，`Close(-1)` 会失败但 `defer` 中已用 `>= 0` 判断
- **无 channel / 锁**：本函数是单线程同步执行，exec 成功后原进程映像被替换，所有资源自动释放

## 7. 设计模式与亮点

- **最小权限原则**：通过 bounding set + effective/permitted/inheritable + ambient 三层 capability 控制，确保 exec 出来的 edge 进程只能用 `CAP_DAC_READ_SEARCH` + `CAP_NET_ADMIN`，无法提权
- **向后兼容**：`requiresHostMountNamespace` 区分新旧 chart 部署模式——旧版通过 `/proc/1/root` 访问主机（需要 setns），新版用显式 hostPath（直接 chroot）
- **`PR_SET_KEEPCAPS` 时序**：先 `PR_CAPBSET_DROP`，再 `PR_SET_KEEPCAPS=1`，最后 `Setresuid` + `Capset`——这是 Linux capability 降权的标准时序，顺序错了 capabilities 会被 setuid 清空
- **Ambient capability**：Linux 4.3+ 引入的机制，使非特权程序 exec 后自动继承指定 capabilities，无需 setuid 或 file capability

## 8. 注意事项

- **安全敏感**：本函数是整个 edge 主机运行模式的安全基石。任何对 capability 列表或降权步骤的修改都可能引入提权风险，修改前应做安全审查
- **`CAP_DAC_READ_SEARCH` 的范围**：该 capability 允许读取任意文件（绕过目录权限检查），edge 用它访问主机 `/proc /sys /var/log` 等只读诊断路径；但若 edge 被攻破，攻击者可读任意主机文件——这是设计上的权衡
- **`CAP_NET_ADMIN`**：用于重启网络服务、配置网络诊断；同样有滥用风险
- **`/proc/1/root` 兼容路径**：需要容器具备 `hostPID: true` 且能 ptrace init 进程（PTRACE_MODE_READ），新部署建议改用 hostPath 挂载
- **`runtime.LockOSThread` 的代价**：被锁定的 OS 线程在 exec 之前不能被其他 goroutine 复用；但 exec 成功后进程映像被替换，无残留
- **错误传播**：`defer unix.Close(rootFD)` 在 `unix.Exec` 成功时不会执行（进程已替换），仅在失败路径执行——这是预期行为
- **测试限制**：本函数涉及 chroot + capability 操作，无法在普通单元测试中验证；CI 应在特权容器或 VM 中做集成测试
