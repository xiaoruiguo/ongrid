# middleware/metrics.go 技术实现文档

## 1. 概述

`metrics.go` 实现了 ongrid manager 的 HTTP 指标采集中间件（ADR-026 self-obs）。它包装每个请求，记录 duration + status 到 Prometheus 指标 `ongrid_http_requests_total` + `ongrid_http_request_duration_seconds`，按 chi 编译期 route pattern 标注 label——**基数受路由表大小限制**，避免随机 scanner URL 创建无限 series。

## 2. 包信息

- **包名**：`middleware`
- **导入路径**：`github.com/ongridio/ongrid/internal/manager/server/middleware`
- **文件定位**：HTTP 中间件（单文件单中间件）
- **使用方**：`cmd/ongrid` 在 chi router 装配时挂载

## 3. 关键类型与接口

本文件无类型定义，仅一个导出函数：

```go
func MetricsMiddleware(next http.Handler) http.Handler
```

## 4. 关键函数与流程

### MetricsMiddleware

```go
func MetricsMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
        start := time.Now()
        next.ServeHTTP(ww, r)

        route := chi.RouteContext(r.Context()).RoutePattern()
        if route == "" {
            route = "unknown"
        }
        prom.ObserveHTTP(r.Method, route, ww.Status(), time.Since(start).Seconds())
    })
}
```

流程：
1. `middleware.NewWrapResponseWriter(w, r.ProtoMajor)` 包装 ResponseWriter 以捕获 status code
2. `start := time.Now()` 记录开始时间
3. `next.ServeHTTP(ww, r)` 执行内层 handler 链
4. **post-handler 取 route pattern**：`chi.RouteContext(r.Context()).RoutePattern()` 取 chi 编译期匹配的路由模板（如 `/v1/edges/{id}/metrics`，而非具体 `/v1/edges/123/metrics`）
5. **空 route → "unknown"**：未匹配路由表（404）的请求归桶到 `route="unknown"`，避免随机 scanner URL 创建新 series 撑爆基数
6. `prom.ObserveHTTP(method, route, status, durationSeconds)` 上报指标

## 5. 依赖关系

**外部**：
- `chi` —— `RouteContext` 取 route pattern
- `chi/middleware` —— `NewWrapResponseWriter` 捕获 status
- `net/http`、`time`

**内部**：
- `internal/pkg/prom`（`ObserveHTTP` 上报函数）

## 6. 并发与资源管理

- **无共享状态**：中间件本身无字段，每请求独立 ww/start
- **`time.Since(start)` 同步取**：无 goroutine，无 channel
- **`prom.ObserveHTTP` 线程安全**：Prometheus client 内部带锁，可并发调用
- **无 ctx 操作**：不依赖 ctx，不取消

## 7. 设计模式与亮点

1. **route pattern 而非 path 作 label**：用 chi 编译期 route 模板（`/v1/edges/{id}/metrics`）而非具体 path（`/v1/edges/123/metrics`），**基数受路由表大小限制**——避免每个 edge_id 创建新 series 撑爆 Prom
2. **404 归桶 "unknown"**：未匹配路由的请求（随机 scanner URL）归到固定 `route="unknown"`，避免无限新 series
3. **`NewWrapResponseWriter` 捕获 status**：标准 `http.ResponseWriter` 不暴露 status 给中间件，必须包装才能在 post-handler 读取
4. **4 个 label 维度**：method + route + status + duration——覆盖 HTTP 自观测核心维度
5. **post-handler 取 pattern**：在 `next.ServeHTTP` 返回后取，确保 chi 已完成路由匹配
6. **极简实现**：单文件 33 行，无配置项，无开关——所有 ongrid HTTP 请求统一采集

## 8. 注意事项

1. **label 基数受路由表限制**：method × route × status 三维，典型规模 < 1000 series；新增路由不会显著膨胀
2. **`route="unknown"` 桶**：404/未匹配请求归此；若 scanner 流量大该桶会膨胀 method × status 维度，但仍 bounded
3. **无 status code 分桶**：200/201/204 等都各自一个 series，可能略显碎片化；如需更粗粒度可在 `ObserveHTTP` 内部映射
4. **duration 是 wall clock**：`time.Since(start)` 包含 handler 全部耗时（含下游 svc 调用）；不含中间件本身的微小开销
5. **无 panic recovery**：本中间件不 recover panic，handler panic 会让进程崩溃；recovery 由 chi 内置 `middleware.Recoverer` 在更外层处理
6. **`ObserveHTTP` 实现细节**：本文件不感知 ObserveHTTP 内部是 histogram 还是 counter，由 `internal/pkg/prom` 决定
7. **无采样**：所有请求都上报；高 QPS 下 Prom scrape 压力需评估
8. **顺序敏感**：必须在 chi 路由匹配之后取 pattern，所以本中间件应在 chi router 内层（路由匹配后）；若装在外层（路由前）route pattern 永远为空 → 全归 "unknown"
9. **不依赖 ctx**：无 cancel/timeout 概念，即使请求被 ctx 取消也会记录到指标
