# `times_linux.go` 技术实现文档

> 源文件：`internal/edgeagent/host_files/times_linux.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/host_files`

## 1. 概述

Linux 专用的 `fileTimes` 实现：从 `syscall.Stat_t` 取 mtime 和 atime。Linux 内核的 atime 字段名为 `Atim`（vs Darwin 的 `Atimespec`），通过 build tag `linux` 隔离，让 `handlers.go::runStatOnePath` 跨 OS 可移植。

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
  3. `fi.Sys().(*syscall.Stat_t)` 类型断言成功时：`atime = time.Unix(st.Atim.Sec, st.Atim.Nsec).UTC()`
  4. 返回 (mtime, atime)
- **错误处理**：类型断言失败时 atime 回退为 mtime——保证总有值

## 5. 依赖关系

- **内部包**：无
- **外部库**：标准库 `os`、`syscall`、`time`
- **被调用方**：同包 `handlers.go::runStatOnePath`

## 6. 并发与资源管理

无并发控制。纯函数，无 IO，无状态。

## 7. 设计模式与亮点

- **build tag 隔离 OS 差异**：`//go:build linux` 让此文件仅在 Linux 编译；与 `times_darwin.go` 互补
- **字段名差异封装**：Linux `Atim` vs BSD `Atimespec`——通过两个 build-tagged 文件把差异隐藏在 `fileTimes` 函数内，handler 调用点 OS 无关
- **UTC 时区**：所有时间返回 UTC，避免 `time.Local` 让测试输出不稳定
- **fallback 安全**：类型断言失败时 atime 回退 mtime——保证总有可用值

## 8. 注意事项

- 此文件仅 Linux 编译；Darwin / Windows 上 `fileTimes` 由 `times_darwin.go` 提供
- `syscall.Stat_t` 在 Linux 上字段是 `Atim`（timespec 结构 {Sec, Nsec}）；与 BSD `Atimespec` 同结构但字段名不同
- Linux 上 `Atim` 在某些架构（如 MIPS）可能字段名不同——若需支持需额外 build tag
- 添加新 OS 支持（如 FreeBSD）需新建 `times_<os>.go` 并实现对应字段访问
- 若未来需要 ctime（inode change time），Linux 是 `Ctim`，Darwin 是 `Ctimespec`——同样需双文件实现
- `fileTimes` 是同包私有函数；外部不应直接调用，应通过 `runStatOnePath` 间接使用
