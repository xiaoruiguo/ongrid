# `service.go` 技术实现文档

> 源文件：`internal/manager/service/metric/service.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/service/metric`

## 1. 概述

本文件是 metric 领域的 manager 服务层 thin shim，桥接 HTTP / tunnel handler 与 `biz.IngestService` + `biz.QueryUsecase`。Service 只做校验 + 委托，业务逻辑（聚合、存储、查询窗口校验）在 biz。红线：本包 MUST NOT import `manager/data/**` —— gospec 架构分层强制；ingester 以接口形式注入让测试可替换 fake。

## 2. 包信息

- **包名**：`metric`
- **所属模块**：`internal/manager/service/metric`
- **依赖方向**：被 HTTP handler / frontierbound handlers 调用；依赖 `biz/metric`、`internal/pkg/tunnel`

## 3. 关键类型与接口

```go
type Service struct {
    ingester biz.IngestService  // 接口类型，非具体 *Ingester
    query    *biz.QueryUsecase
    log      *slog.Logger
}
```

`biz.IngestService` 是接口（非具体类型），让 tunnel-side handler 组合 agent 看到统一类型，测试可注入 fake。

## 4. 关键函数与流程

### `New(ingester, q, log)`

- log nil → `slog.Default()`；返回 `&Service{ingester, query: q, log}`。

### `Push(ctx, edgeID, points []tunnel.HostMetricPoint)`

- **职责**：tunnel-side host metric 入口。
- **流程**：直接 `s.ingester.Push(ctx, edgeID, points)`；ingester 内部把 `tunnel.HostMetricPoint` 转换为 domain Points，service API 不泄露 model 类型。
- **错误处理**：err 透传。

### `Query(ctx, q biz.RangeQuery) (*biz.Series, error)`

- **职责**：HTTP-side 时间范围 metric 查询。
- **流程**：`s.query.Query(ctx, q)`；注释明示校验（window size、from<to、edge_id>0）在 QueryUsecase。
- **错误处理**：err 透传。

## 5. 依赖关系

- **内部包**：`biz/metric`、`internal/pkg/tunnel`
- **外部库**：`log/slog`、`context`
- **被调用方**：HTTP handler（Query）；frontierbound handlers（Push 经 MetricIngester 接口）

## 6. 并发与资源管理

- **无共享可变状态**：Service 字段在 New 后只读；并发安全依赖 biz 层。
- **ctx 透传**：两个方法首参 `context.Context`。

## 7. 设计模式与亮点

- **thin shim**：注释明示"validates + delegates, business logic stays in biz"。
- **ingester 用接口**：`biz.IngestService` 是接口，让 tunnel-side handler 组合 agent 与测试 fake 看到同一类型。
- **API 不泄露 model 类型**：Push 接受 `tunnel.HostMetricPoint`（wire 类型），ingester 内部转 domain Point；Query 返回 `biz.Series`（biz 类型），不暴露 model.Series。
- **Push vs Query 双入口**：Push 是 tunnel-side（edge 推数据），Query 是 HTTP-side（UI 查数据）；同一 Service 暴露两端，避免拆包。

## 8. 注意事项

- **无校验**：注释明示校验在 QueryUsecase；service 层不重复校验。
- **log 字段当前未使用**：New 接受 log 但 Service 方法未引用；保留供未来 debug 日志。
- **ingester 可 nil**：当前未防御；调用方应确保注入。
- **Push 的 edgeID 是 tunnel-side edge_id**：注释未说明是否已 canonicalize；实际由 frontierbound handlers 调用时已 resolve device_id（见 handlers.go 的 `resolveDeviceID`），此处 edgeID 实际是 device_id。
- **分层强制**：MUST NOT import `manager/data/**`；gospec §architecture/layering。
