# `http.go` 技术实现文档

> 源文件：`tests/e2e/testenv/http.go`
> 包路径：`github.com/ongridio/ongrid/tests/e2e/testenv`

## 1. 概述

本文件是 e2e 测试环境的端口分配辅助，提供 `freePort()` 通过绑定 `127.0.0.1:0` 获取操作系统分配的空闲 TCP 端口并立即关闭。用于让每个测试 Env 绑定独立随机端口，避免并行测试端口冲突。设计上接受 Close 与 manager Listen 之间的微小竞态（用 waitReady 兜底）。

## 2. 包信息

- **包名**：`testenv`（与 env.go 同包）
- **所属模块**：`tests/e2e/testenv`
- **依赖方向**：被 env.go 的 `Start` 调用；仅依赖标准库 `net`

## 3. 关键类型与接口

无类型、接口、常量、变量定义。仅一个未导出函数 `freePort`。

## 4. 关键函数与流程

### `freePort() (int, error)`

- **职责**：获取一个空闲 TCP 端口。
- **流程**：
  1. `net.Listen("tcp", "127.0.0.1:0")` —— 绑定到随机端口。
  2. err → 返回 (0, err)。
  3. `defer l.Close()` —— 立即关闭 listener 释放端口。
  4. `l.Addr().(*net.TCPAddr).Port` —— 取端口号返回。
- **错误处理**：Listen 失败返回 error；caller（env.go 的 `Start`）`t.Fatalf`。
- **竞态说明**：注释明示 Close 与 manager 的 Listen 之间有微小 race；替代方案（传 *net.Listener 给 manager）需 manager 支持 fd 继承，而它不支持；race 良性 —— 若端口被占，manager Listen 报错，waitReady 在 20s 内检测到。

## 5. 依赖关系

- **内部包**：无
- **外部库**：仅 `net`
- **被调用方**：env.go 的 `Start`（调用两次：HTTP 端口 + metrics 端口）

## 6. 并发与资源管理

- **无共享状态**：纯函数，无全局变量。
- **listener 立即 Close**：defer 保证释放；端口回到 OS free pool。
- **竞态窗口**：Close 后到 manager Listen 之间的微小窗口；waitReady 兜底检测。

## 7. 设计模式与亮点

- **`:0` 绑定取端口**：标准 Go 模式；OS 分配未用端口。
- **接受微小竞态**：注释详述替代方案（fd 继承）不可行；waitReady 20s 兜底让竞态可接受。
- **127.0.0.1 而非 :0**：绑定 loopback 防外部网络暴露；e2e 测试隔离。
- **未导出**：仅 env.go 内部使用；测试用例不直接调。

## 8. 注意事项

- **竞态存在但良性**：若端口被占，manager Listen 失败，`waitReady` 检测 `cmd.ProcessState.Exited()` 后立即返回"manager exited before ready"。
- **仅返回 int**：不返回 *net.Listener；manager 必须自己 Listen。
- **TCP only**：不支持 Unix socket；e2e 用 TCP。
- **127.0.0.1 硬编码**：不绑定 ::1 或其他接口；IPv6 部署需扩展。
- **无重试**：Listen 失败直接返回 error；caller t.Fatal。
- **两次调用独立**：env.go 分别取 HTTP 端口 + metrics 端口；两次调用间无关联。
- **端口可能被其他进程占用**：除 manager 外的其他进程若抢同端口会失败；概率极低（OS 分配未用端口）。
