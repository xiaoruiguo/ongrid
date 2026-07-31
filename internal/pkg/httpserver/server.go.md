# `server.go` 技术实现文档（httpserver）

> 源文件：`internal/pkg/httpserver/server.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/httpserver`

## 1. 概述

该文件包装 `net/http.Server`，提供遵循父 context 的优雅关停。被 `cmd/ongrid` / `cmd/ongrid-edge` 用于 API、metrics、debug 三个监听器。`Start` 阻塞直到 `ListenAndServe` 报错或 ctx 被取消；ctx 取消时触发带 10s deadline 的 `Shutdown`，保证在飞行请求完成或超时后干净退出。

## 2. 包信息

- **包名**：`httpserver`
- **所属模块**：`internal/pkg/`（基础设施层）
- **依赖方向**：被 `cmd/ongrid` / `cmd/ongrid-edge` 启动时调用；仅依赖标准库。

## 3. 关键类型与接口

### `Server`
组合 `*http.Server` 与 logger。

```go
type Server struct {
    *http.Server
    log *slog.Logger
}
```

## 4. 关键函数与流程

### `New`
- **签名**：`func New(addr string, h http.Handler, log *slog.Logger) *Server`
- **职责**：构造 Server，预设 `ReadHeaderTimeout: 10s`（防 Slowloris 攻击）。
- **流程**：纯赋值。

### `Start`
- **签名**：`func (s *Server) Start(ctx context.Context) error`
- **职责**：阻塞运行直到出错或 ctx 取消，支持优雅关停。
- **流程**：
  1. 启动 goroutine 跑 `ListenAndServe`：
     - log 记录 `http server listening` + addr。
     - `err != nil && !errors.Is(err, http.ErrServerClosed)` → 写 errCh。
     - 否则写 nil 到 errCh。
  2. 主流程 `select`：
     - `<-ctx.Done()` → 用 `context.WithTimeout(context.Background(), 10s)` 调 `s.Shutdown`：
       - 失败 → log Error + 返回 err。
       - 成功 → log Info `shutdown complete` + 返回 nil。
     - `<-errCh` → 返回 err（ListenAndServe 自身出错）。
- **错误处理**：
  - `http.ErrServerClosed` 视为正常退出，不报错。
  - Shutdown 失败记录 Error 日志含 addr + err。
- **关键设计**：Shutdown 用独立 `context.Background()` 而非父 ctx，因为父 ctx 已取消无法承载 10s 超时。

## 5. 依赖关系

- **内部包**：无。
- **外部库**：标准库 `context` / `errors` / `log/slog` / `net/http` / `time`。
- **被调用方**：`cmd/ongrid` / `cmd/ongrid-edge` 启动装配；典型用法是 main goroutine 持有 ctx，SIGINT/SIGTERM 时 cancel ctx 触发 Shutdown。

## 6. 并发与资源管理

- **goroutine**：`Start` 内部启动一个 goroutine 跑 `ListenAndServe`。
- **channel**：`errCh` buffered 容量 1，保证 goroutine 写入不阻塞即使主流程已从 select 返回。
- **context**：父 ctx 控制生命周期；Shutdown 用独立 10s timeout context。
- **无显式锁**：`*http.Server` 自身并发安全。

## 7. 设计模式与亮点

- **优雅关停标准模式**：goroutine + errCh + select 是 Go HTTP 服务关停的 idiomatic 写法。
- **`http.ErrServerClosed` 屏蔽**：`ListenAndServe` 在 Shutdown 后返回 `ErrServerClosed`，本实现识别并视为正常退出，不冒泡为错误。
- **独立 Shutdown context**：Shutdown 不复用已取消的父 ctx，而是新建 10s timeout ctx，保证关停有时间完成飞行请求。
- **ReadHeaderTimeout 预设**：防 Slowloris 慢速 header 攻击，gospec 安全红线相关。
- **生命周期日志**：listening / shutdown complete / shutdown error 三类日志覆盖关键事件，便于运维追踪。

## 8. 注意事项

- **10s Shutdown deadline**：长轮询 / SSE / WebSocket 连接可能在 10s 内无法完成；如有长连接需评估调长或单独处理。
- **无 WriteTimeout / IdleTimeout 设置**：仅设 ReadHeaderTimeout，其他用零值（无限）；生产环境建议显式设置以防资源耗尽。
- **无 MaxHeaderBytes**：使用默认值；超大 header 攻击可能不被拦截。
- **Start 阻塞**：调用方需在 goroutine 中跑 Start，否则 main 阻塞无法处理 signal。
- **errCh 容量 1**：保证 goroutine 写入不阻塞，但若 goroutine 写入后主流程已返回，错误被丢弃——可接受，因为主流程返回意味着已进入 Shutdown。
- **无 TLS 配置**：仅 HTTP；TLS 由上游 reverse proxy（nginx / Caddy）终止，符合 ongrid 部署架构。
- **Logger nil 不处理**：若 log 为 nil，`s.log.Info` 会 panic；调用方必须传非 nil logger。
