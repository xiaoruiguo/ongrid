# `prom.go` 技术实现文档

> 源文件：`internal/pkg/prom/prom.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/prom`

## 1. 概述

该文件是 prom 包的核心入口，构建进程级 Prometheus registry 与 `/metrics` HTTP handler。`NewRegistry` 返回带 Go runtime + process collector 的新 registry；`Handler` 返回 `/metrics` HTTP handler。各 BC 在自身构造函数中向 registry 注册 collector。包级文档明确 label cardinality 红线——禁止用 org_id / user_id / edge_id / URL full-path 作为 label。

## 2. 包信息

- **包名**：`prom`
- **所属模块**：`internal/pkg/`（基础设施层）
- 依赖方向：被 `cmd/ongrid` / `cmd/ongrid-edge` 启动时调用；依赖 `prometheus/client_golang`。

## 3. 关键类型与接口

无显著类型定义（仅顶层函数）。

## 4. 关键函数与流程

### `NewRegistry`
- **签名**：`func NewRegistry() *prometheus.Registry`
- **职责**：返回带 Go runtime + process collector 的新 registry。
- **流程**：
  1. `prometheus.NewRegistry()` 创建空 registry。
  2. `reg.MustRegister(collectors.NewGoCollector())` 注册 Go runtime collector（goroutines / heap / GC / etc.）。
  3. `reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))` 注册进程 collector（CPU / memory / fd / etc.）。
  4. 返回 registry。
- **设计理由**：每个 BC 自治注册自身 metrics，registry 仅提供基础 collector；避免全局 DefaultRegistry 跨测试污染。

### `Handler`
- **签名**：`func Handler(reg *prometheus.Registry) http.Handler`
- **职责**：返回 `/metrics` HTTP handler。
- **流程**：`promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg})`。
- **设计理由**：`HandlerFor` 接受显式 registry，避免依赖全局 DefaultRegisterer；`HandlerOpts` 仅显式传 Registry（其他用默认，如错误处理 / 编码）。

## 5. 依赖关系

- **内部包**：无。
- **外部库**：`github.com/prometheus/client_golang/prometheus` + `prometheus/collectors` + `prometheus/promhttp`；标准库 `net/http`。
- **被调用方**：`cmd/ongrid` / `cmd/ongrid-edge` 启动装配；`Handler` 通常挂载到 metrics 端口（如 `:9100/metrics`）。

## 6. 并发与资源管理

无显式锁。`prometheus.Registry` 自身并发安全；`Handler` 返回的 `http.Handler` 通过 promhttp 内部加锁保证并发采集安全。多 goroutine 同时 scrape `/metrics` 由 promhttp 内部 mutex 串行化 gather。

## 7. 设计模式与亮点

- **非全局 registry**：`NewRegistry` 每次返回新 registry，避免 `prometheus.DefaultRegisterer` 全局 state 跨测试污染，符合"避免全局可变变量"红线。
- **基础 collector 预装**：Go runtime + process collector 在 `NewRegistry` 时预装，调用方无需重复，免费获得 goroutines / heap / GC / CPU / fd 等基础指标。
- **显式 registry handler**：`Handler` 用 `promhttp.HandlerFor` 接受显式 registry，避免依赖全局 DefaultRegisterer，测试隔离性好。
- **cardinality 红线文档化**：包注释明确禁止 org_id / user_id / edge_id / URL full-path 作为 label，列出允许 label 集（method / code / status / model / direction / result / plan_bucket），是 ongrid Prometheus 实践的强制规范。
- **`MustRegister` 用于基础 collector**：Go + process collector 注册失败应直接 panic（库问题而非业务问题），fail-fast 暴露问题。

## 8. 注意事项

- **`MustRegister` panic 风险**：若 Go / process collector 注册失败（如重复注册或库 bug），`MustRegister` panic；正常情况不会发生。
- **`Handler` 不带压缩**：`HandlerOpts` 未启用 gzip 压缩；大 scrape 可能带宽高，可考虑 `HandlerOpts{EnableOpenMetrics: true}` 或反向代理压缩。
- **无采集超时**：`HandlerOpts` 未设 `Timeout`；慢采集（如大量 series）可能挂死，可设 `HandlerOpts{Timeout: ...}`。
- **无错误日志钩子**：`HandlerOpts` 未设 `ErrorLog` / `ErrorHandler`；采集错误静默丢弃，调试困难。
- **registry 隔离 vs DefaultRegisterer**：本包坚持显式 registry，但部分第三方库（如 mysql driver）会向 DefaultRegisterer 注册；若需统一需手工迁移或使用 `prometheus.PedanticRegistry`。
- **cardinality 红线依赖人工遵守**：注释明确红线但无工具强制；新增 metric 需 review label 集合，避免高基数字段（如 user_id）混入。
- **`HandlerOpts.Registry` 字段冗余**：`promhttp.HandlerFor` 第一个参数已是 registry，`HandlerOpts.Registry` 再次传同一 registry 是 promhttp 的特殊要求（用于错误处理路径），不可省略。
