# metric/http.go 技术实现文档

## 1. 概述

`http.go` 是 ongrid manager 指标查询子域（`/v1/edges/{id}/metrics`）的 HTTP 路由层。提供单个 GET 端点，按时间范围 + 分辨率查询 edge 主机指标（CPU/内存/负载/网络/磁盘）。响应通过 `pointDTO` tagged-union 形状统一三种分辨率（raw / 5m / 1h），让 SPA 单一渲染路径。

## 2. 包信息

- **包名**：`metric`
- **导入路径**：`github.com/ongridio/ongrid/internal/manager/server/metric`
- **路由前缀**：`/v1/edges/{id}/metrics`（由 `cmd/ongrid` 挂载，auth 中间件由上游注入，任何已认证用户可读）
- **文件定位**：HTTP 适配层 + DTO 归一化

## 3. 关键类型与接口

### MetricService —— 窄接口

```go
type MetricService interface {
    Query(ctx context.Context, q biz.RangeQuery) (*biz.Series, error)
}
```

由 `*service/metric.Service` 通过结构化类型满足。返回的 `biz.Series` 包含 `Resolution` 字段决定走哪个 points 分支。

### Handler

```go
type Handler struct {
    svc MetricService
}
```

### DTO —— tagged-union 归一化

```go
type pointDTO struct {
    Ts          time.Time `json:"ts"`
    CPU         minMax    `json:"cpu"`
    Mem         minMax    `json:"mem"`
    Load1       minMax    `json:"load1"`
    Load5       minMax    `json:"load5"`
    Load15      minMax    `json:"load15"`
    NetRxBps    *uint64   `json:"net_rx_bps,omitempty"`
    NetTxBps    *uint64   `json:"net_tx_bps,omitempty"`
    DiskUsedPct minMax    `json:"disk_used_pct"`
}

type minMax struct {
    Avg *float64 `json:"avg,omitempty"`
    Max *float64 `json:"max,omitempty"`
}

type queryResp struct {
    Resolution string     `json:"resolution"`
    From       time.Time  `json:"from"`
    To         time.Time  `json:"to"`
    Points     []pointDTO `json:"points"`
}
```

**关键设计**：`minMax` 用 `*float64` 指针，缺失桶返 `null` 而非 0——让 recharts `connectNulls=false` 在故障时段断线，避免"0 静默桥接"误导运维。

## 4. 关键函数与流程

### Register

```go
func (h *Handler) Register(r chi.Router)
```

仅一个路由：`GET /v1/edges/{id}/metrics?from=RFC3339&to=RFC3339&resolution=auto|raw|5m|1h`。

### queryMetrics

```go
func (h *Handler) queryMetrics(w http.ResponseWriter, r *http.Request)
```

流程：
1. `parseID(r)` 取 `id` URL 参数（必须 > 0）
2. `parseTime(from)` / `parseTime(to)` 解析 RFC3339 时间
3. `biz.Resolution(q.Get("resolution"))`，空字符串默认 `ResAuto`
4. 调 `svc.Query(ctx, RangeQuery{EdgeID, From, To, Resolution})`
5. `toPointDTOs(series)` 把 biz.Series 三种 resolution 分支归一为 `[]pointDTO`
6. 200 + `queryResp{Resolution, From, To, Points}`

### toPointDTOs —— 三分支归一

```go
func toPointDTOs(s *biz.Series) []pointDTO
```

按 `s.Resolution` switch：
- **`ResRaw`**：每个 raw point 的 avg == max（同一个值 boxing 两次），`NetRxBps/TxBps` 用 `ptrU`
- **`Res5m`**：bucket 带 `Avg` + `Max` 两个字段（gauge），counter 字段用 `Sum`
- **`Res1h`**：同 5m 但用 1h bucket
- **default**：返 nil

### helpers

- `parseID(r)` —— `ParseUint` + `id == 0` 拒绝
- `parseTime(s)` —— 仅 RFC3339（比 logs/http.go 简单，不接受 unix 数字）
- `ptrF(v)` / `ptrU(v)` —— 把标量 box 成指针，统一 wire shape
- `writeJSON` / `writeErr` / `errCode` —— 错误映射，含 `ErrEdgeOffline` / `ErrNotWiredYet` 等业务特定 code

### errCode —— 业务感知错误映射

```go
func errCode(err error) string
```

返 slug：
- `ErrNotFound` → `not-found`
- `ErrUnauthorized` → `unauthorized`
- `ErrForbidden` → `forbidden`
- `ErrInvalid` → `invalid`
- `ErrEdgeOffline` → `edge-offline`
- `ErrNotWiredYet` → `not-wired-yet`
- 其它 → `internal`

## 5. 依赖关系

**外部**：
- `chi` —— URL 参数
- `net/http`、`encoding/json`、`strconv`、`time`、`fmt`、`errors`

**内部**：
- `internal/manager/biz/metric`（RangeQuery/Series/Resolution 常量）
- `internal/pkg/errs`（错误哨兵 + `HTTPStatus`）

## 6. 并发与资源管理

- **无共享可变状态**：`Handler.svc` 启动时装配
- **请求级隔离**：每请求独立 ctx
- **无 goroutine 启动**：同步调 svc.Query
- **无显式 timeout**：依赖 svc 层 / 上游 ctx 控制

## 7. 设计模式与亮点

1. **tagged-union DTO 归一三种分辨率**：`pointDTO` 单一形状容纳 raw/5m/1h，避免 SPA 按 resolution 分三套渲染逻辑
2. **`*float64` 指针支持 null gap**：`minMax` 用指针，缺失桶 → JSON null → recharts 断线，避免 0 静默桥接故障时段（这是经过运维反馈才加的——对比 Grafana 同范围图表发现 0 桥接误导）
3. **raw sample avg==max**：raw 点没有 max 概念，把同一个值 boxing 两次保持 wire shape 一致
4. **counter vs gauge 区分**：raw 网络字节是瞬时值，5m/1h 是 sum；DTO 用相同字段名 `net_rx_bps` 但语义按 resolution 切换
5. **`resolution=auto` 默认**：前端不指定时由 biz 层按时间范围自动选 raw/5m/1h
6. **业务感知 errCode**：含 `edge-offline` / `not-wired-yet` 等 ongrid 特定 slug，前端可精准提示
7. **`HTTPStatus(err)` 集中映射**：用 errs 包提供的 `HTTPStatus` 函数，避免本文件重复 if-else

## 8. 注意事项

1. **`parseTime` 仅 RFC3339**：比 logs/http.go 严格，不接受 unix 数字；前端必须发 RFC3339 格式
2. **`id == 0` 拒绝**：避免 0 作为合法 ID 误命中
3. **`NetRxBps/TxBps` 用 `*uint64`**：raw 模式下若 Prom 数据缺失返 null；MySQL 后端总是有值但保持 wire shape 一致
4. **无显式 timeout**：依赖 svc 层；若 svc 层也无 timeout 可能拖死请求，应评估是否在 handler 层加 `context.WithTimeout`
5. **`toPointDTOs` default 返 nil**：未知 resolution 返空 points 数组，前端会渲染空图；应评估是否报错
6. **`DiskUsedPct` 是 worst mount**：biz 层取所有 mountpoint 中使用率最高的，不是平均——前端文案需对齐
7. **`errorBody` 与 edge 子域同步**：注释明示"kept in sync with server/edge"，跨 BC 错误契约统一
8. **post-pivot 路径扁平**：路径不含 `org_id`，所有认证用户可读所有 edge 指标（多租户隔离需在 biz 层做）
