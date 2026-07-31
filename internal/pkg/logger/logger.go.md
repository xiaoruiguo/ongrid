# `logger.go` 技术实现文档

> 源文件：`internal/pkg/logger/logger.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/logger`

## 1. 概述

该文件封装 `log/slog`，提供 ongrid 项目约定的 logger 构造工具。输出 JSON 行到 stderr，支持指定最小级别与添加 `service` 属性。trace_id / org_id 等上下文属性由调用点通过 `slog` 属性注入，本包不负责。

## 2. 包信息

- **包名**：`logger`
- **所属模块**：`internal/pkg/`（基础设施层）
- **依赖方向**：被 `cmd/ongrid` / `cmd/ongrid-edge` 启动时调用；仅依赖标准库。

## 3. 关键类型与接口

无显著类型定义（仅顶层函数）。

## 4. 关键函数与流程

### `New`
- **签名**：`func New(level slog.Level) *slog.Logger`
- **职责**：构造 JSON logger，输出到 stderr，最小级别由参数指定。
- **流程**：
  1. `slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})`。
  2. `slog.New(h)` 返回。
- **设计理由**：JSON 格式便于日志聚合（loki / ELK）解析；stderr 与 stdout 分离，避免与程序正常输出冲突。

### `WithService`
- **签名**：`func WithService(l *slog.Logger, name string) *slog.Logger`
- **职责**：返回带 `service` 属性的子 logger。
- **流程**：`l.With(slog.String("service", name))`。
- **使用场景**：`cmd/ongrid` / `cmd/ongrid-edge` 启动时区分服务来源；后续所有日志自动携带 service 标签。

## 5. 依赖关系

- **内部包**：无。
- **外部库**：标准库 `log/slog` / `os`。
- **被调用方**：`cmd/ongrid` / `cmd/ongrid-edge` 启动装配；几乎所有 BC 通过依赖注入接收 `*slog.Logger`。

## 6. 并发与资源管理

无并发控制。`slog.Logger` 自身并发安全；`New` 与 `WithService` 是纯构造函数。

## 7. 设计模式与亮点

- **slog 标准化**：采用 Go 1.21+ 标准库 `log/slog`，避免第三方日志库依赖，与 gospec "结构化日志（slog / zap）" 红线一致。
- **JSON 输出**：JSON 行格式便于机器解析，符合 gospec "日志结构化 + trace_id" 要求。
- **stderr 输出**：与 stdout 分离，避免与程序正常输出（如命令行工具的结果打印）混淆；容器部署下 stderr 通常被 docker 单独捕获。
- **service 属性装饰**：`WithService` 用 `l.With` 创建子 logger，后续所有日志自动携带 service 字段，无需调用方每次手动添加。
- **极简 API**：仅两个函数，避免过度抽象；trace_id / org_id 等动态属性交给调用点用 `slog.String` / `slog.Uint64` 等添加。

## 8. 注意事项

- **不处理 trace_id 注入**：gospec 要求日志含 trace_id，本包不提供；需在 HTTP middleware 层用 `slog.Logger.With("trace_id", ...)` 创建子 logger 并放入 context 传递。
- **级别由调用方指定**：`New` 不读 env，级别由 `cmd/*` 根据 env（如 `ONGRID_LOG_LEVEL`）决定；本包不内化级别策略。
- **无日志轮转 / 大小控制**：直接写 stderr，由容器日志驱动（docker / journald）管理轮转。
- **无敏感字段过滤**：本包不负责脱敏，调用方需自行确保不记录密钥 / 用户内容（gospec "敏感字段禁止明文入日志" 红线）。
- **JSON 不可读性**：JSON 行对人眼不友好，开发期可考虑用 `slog.NewTextHandler` 替代，但本包未提供选项。
- **无 sample / rate limit**：高频日志会全量输出，可能淹没日志系统；调用方需自行控制频率。
- **`WithService` 不nil安全**：`l == nil` 时 `l.With` panic；调用方需确保传入非 nil logger。
