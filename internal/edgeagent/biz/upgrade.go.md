# `upgrade.go` 技术实现文档

> 源文件：`internal/edgeagent/biz/upgrade.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/biz`

## 1. 概述

本文件实现 `MethodAgentUpgrade` handler：下载二进制到 stage dir、校验 SHA256、原子重命名为 `pending`（连同 `pending.sha256` 配套），并通过 `upgradeRequested` 通道信号让 `Run()` 干净退出，由 systemd 在重启时通过 ExecStartPre 脚本完成二进制切换。失败粘性：任何错误都不留 `pending` 文件，重启仍跑旧二进制。

## 2. 包信息

- **包名**：`biz`
- **所属模块**：edgeagent 升级能力实现层
- **依赖方向**：被同包 `Agent.registerHandlers` 注册为 handler；调用 `tunnel`

## 3. 关键类型与接口

无类型定义。函数全部是 `Agent` 的方法。

## 4. 关键函数与流程

### `handleAgentUpgrade`
- **签名**：`func (a *Agent) handleAgentUpgrade(ctx context.Context, req tunnel.AgentUpgradeRequest) (tunnel.AgentUpgradeResponse, error)`
- **职责**：下载并 stage 新二进制，校验完整性，信号 Run 退出
- **流程**：
  1. 校验 `UpgradeStageDir` 非空（否则报错）；校验 SHA256 是 64 位小写 hex；校验 URL 是 http(s)
  2. `os.MkdirAll(dir, 0o750)` 预建 stage dir（agent 非 root，依赖 install 脚本设权限）
  3. 清理上次失败残留：`pending.tmp` / `pending` / `pending.sha256`
  4. 45 分钟下载超时（弱网生产环境）；`http.NewRequestWithContext` + `http.DefaultClient.Do`
  5. 流式写入 `pending.tmp` 同时 `sha256.New` 在线计算（解耦下载吞吐与 hash 成本）
  6. `f.Sync()` + `f.Close()`；任何 IO 错误删 tmp 并返回
  7. SHA256 比对失败删 tmp 返回
  8. **顺序关键**：先写 `pending.sha256`，再 `os.Rename(tmp, final)`——ExecStartPre 脚本以 `pending.sha256` 存在为信号，并再次校验
  9. 非阻塞发信号到 `a.upgradeRequested`（buffer=1，重复信号 harmless）
- **错误处理**：所有错误用 `fmt.Errorf("agent_upgrade: ...: %w", err)` 包装；下载/同步/重命名失败都清理 tmp

## 5. 依赖关系

- **内部包**：`internal/pkg/tunnel`（仅用其 request/response 类型）
- **外部库**：标准库 `crypto/sha256`、`encoding/hex`、`net/http`、`os`、`path/filepath`、`time`、`io`
- **被调用方**：`Agent.registerHandlers` 注册的 `MethodAgentUpgrade` handler 闭包

## 6. 并发与资源管理

- 单 handler 调用同步执行，无 goroutine
- `context.WithTimeout(ctx, 45*time.Minute)` 长超时；`defer cancel()` 释放
- `defer httpResp.Body.Close()` 关闭响应体
- `upgradeRequested` 通道 buffer=1 + `select { case ...<-: default: }` 非阻塞发送，handler 永不卡在 closed-channel race

## 7. 设计模式与亮点

- **粘性失败**：任何错误路径都删除 `pending.tmp`，保证重启仍跑旧二进制；不尝试在 swap 后回滚——回滚是 install 脚本职责
- **顺序敏感的原子替换**：先写 `pending.sha256`（独立文件），再 `Rename(tmp, final)`；ExecStartPre 脚本以 `pending.sha256` 存在为信号并二次校验 hash。`pending.sha256` 单独存在无害（脚本在 `pending` 缺失时 no-op）
- **流式 hash**：`io.MultiWriter(f, hasher)` 让下载与 hash 计算并行，对 20MB 二进制几乎零成本
- **无 auth 下载**：artifact server 走 manager 已信任的 nginx；SHA256 是 MITM/损坏的唯一闸门
- **健康标记解耦**：handler 只负责 stage + 信号退出；swap 由 systemd ExecStartPre 完成；健康判断由 `writeHealthMarker` 在 register 成功后写入

## 8. 注意事项

- 45 分钟下载超时是生产弱网设定；测试可通过 ctx 覆盖更短超时
- `http.DefaultClient` 不跳过 TLS 校验——与 `upgrade_package.go` 的 `bundleDownloadClient`（自签 nginx 用 InsecureSkipVerify）不同；artifact server 应配正式证书
- handler 仅在 `UpgradeStageDir != ""` 时注册；dev / no-systemd 主机 manager 看见 "method not found"
- `pending.tmp` 模式 0o755 让 systemd 启动脚本可执行；stage dir 0o750 由 install 脚本 chown 给 ongrid-edge 用户
- 哈希校验是 64 字符小写 hex 的硬约束——大写或非 hex 字符直接拒绝
