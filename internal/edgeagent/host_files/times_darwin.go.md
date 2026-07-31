# `times_darwin.go` 技术实现文档

> 源文件：`internal/edgeagent/host_files/times_darwin.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/host_files`

## 1. 概述

Darwin 专用的 `fileTimes` 实现：从 `syscall.Stat_t` 取 mtime 和 atime。BSD 内核的 atime 字段名为 `Atimespec`（vs Linux 的 `Atim`），通过 build tag `darwin` 隔离，让 `handlers.go::runStatOnePath` 跨 OS 可移植。

## 2. 包信息

- **包名**：`host_files`
- **所属模块**：edgeagent 文件系统能力层（OS 特定子模块）
- **依赖方向**：被同包 `handlers.go::runStatOnePath` 调用

## 3. 关键类型与接口

无类型定义。

## 4. 关键函数与流程

### `fileTimes`
- **签名**：`func fileTimes(fi os.FileInfo) (time.Time, time.Time)`
- **职责**：返回 (mtime, atime)，均为 UTC
- **流程**：
  1. `mtime := fi.ModTime().UTC()`
  2. `atime := mtime`（fallback）
  3. `fi.Sys().(*syscall.Stat_t)` 类型断言成功时：`atime = time.Unix(st.Atimespec.Sec, st.Atimespec.Nsec).UTC()`
  4. 返回 (mtime, atime)
- **错误处理**：类型断言失败时 atime 回退为 mtime——保证总有值

## 5. 依赖关系

- **内部包**：无
- **外部库**：标准库 `os`、`syscall`、`time`
- **被调用方**：同包 `handlers.go::runStatOnePath`

## 6. 并发与资源管理

无并发控制。纯函数，无 IO，无状态。

## 7. 设计模式与亮点

- **build tag 隔离 OS 差异**：`//go:build darwin` 让此文件仅在 Darwin 编译；与 `times_linux.go` 互补
- **字段名差异封装**：BSD `Atimespec` vs Linux `Atim`——通过两个 build-tagged 文件把差异隐藏在 `fileTimes` 函数内，handler 调用点 OS 无关
- **UTC 时区**：所有时间返回 UTC，避免 `time.Local` 让测试输出不稳定
- **fallback 安全**：类型断言失败时 atime 回退 mtime——保证总有可用值，宁可失真也不报错

## 8. 注意事项

- 此文件仅 Darwin 编译；Linux / Windows 上 `fileTimes` 由 `times_linux.go` 提供
- `syscall.Stat_t` 在 Darwin 上字段是 `Atimespec`（timespec 结构 {Sec, Nsec}）；与 Linux `Atim` 同结构但字段名不同
- 添加新 OS 支持（如 FreeBSD / OpenBSD）需新建 `times_<os>.go` 并实现对应字段访问
- macOS `/var` → `/private/var` 的 canonical 差异不影响此函数——`fi.ModTime()` 和 `fi.Sys()` 已是 OS 抽象
- 若未来需要 ctime（inode change time），Darwin 是 `Ctimespec`，Linux 是 `Ctim`——同样需双文件实现
