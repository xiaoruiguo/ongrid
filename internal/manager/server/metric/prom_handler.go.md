# metric/prom_handler.go 技术实现文档

## 1. 概述

`prom_handler.go` 是 `/v1/edges/{id}/metrics` 的 post-pivot Prom 后端实现，替代 MySQL fast path（已在 main.go 注释掉）。响应 wire shape 与旧 handler 完全一致，让 Monitor.tsx / EdgeDetail.tsx 无需改动。每请求 fan-out 一组固定 PromQL range query，把 matrix 结果 zip 成旧 handler 产生的 `pointDTO` 桶。同时提供通用 PromQL passthrough 端点 `/v1/metrics/query_range`。

## 2. 包信息

- **包名**：`metric`（与 `http.go` 同包）
- **导入路径**：`github.com/ongridio/ongrid/internal/manager/server/metric`
- **路由**：`/v1/edges/{id}/metrics` + `/v1/metrics/query_range`（同一 Register 挂载）
- **文件定位**：HTTP 适配层 + PromQL 构造 + matrix → pointDTO 归一化

## 3. 关键类型与接口

### PromQuerier —— 窄接口

```go
type PromQuerier interface {
    QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) (*promquery.InstantResult, error)
}
```

由 `*promquery.Client` 满足；测试可 stub。

### HostDeviceResolver —— edge → host device 映射

```go
type HostDeviceResolver interface {
    ResolveHostDeviceID(ctx context.Context, edgeID uint64) (uint64, error)
}
```

**关键背景**：post-Phase-B 后 Prom 样本带 `device_id` 而非 `edge_id` 标签。`edge_devices` 表（type=host）是真相源——每个 edge 恰好有一个 host device，但 edge_id 与 device_id 在 pre-launch backfill（假设 edge_id==device_id）之后创建的 edge 上会发散。

### PromHandler

```go
type PromHandler struct {
    q   PromQuerier
    dev HostDeviceResolver
}
```

`dev` 可为 nil（degraded 模式，回退到 `edge_id` 标签查询）。

### rangeResp —— passthrough 响应

```go
type rangeResp struct {
    Resolution string          `json:"resolution"`
    From       string          `json:"from"`
    To         string          `json:"to"`
    Matrix     json.RawMessage `json:"matrix"`
}
```

`Matrix` 用 `json.RawMessage` 透传 Prom 原始 matrix，让 SPA 自行 reshape，后端不感知 per-panel 知识。

## 4. 关键函数与流程

### Register

```go
func (h *PromHandler) Register(r chi.Router)
```

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/v1/edges/{id}/metrics` | 与旧 MySQL handler 同路径，调用方无感切换 |
| GET | `/v1/metrics/query_range` | 通用 PromQL passthrough（EdgeDetail 多维面板用） |

### queryMetrics —— 主流程

```go
func (h *PromHandler) queryMetrics(w http.ResponseWriter, r *http.Request)
```

1. `parseID(r)` 取 edge_id
2. `parseTime(from)` / `parseTime(to)`，校验 `to.After(from)`
3. **resolution → step**：`resolution` 参数支持任意 duration 字符串（"15s"/"30s"/"1m"/"5m"/"30m"/"1h"），范围 `[5s, 24h]`；缺失/无效回退 `5m`
4. `h.q == nil` → `errs.ErrNotWiredYet`（503）
5. **edge_id → device_id 解析**：
   - `dev != nil` 且 `ResolveHostDeviceID` 成功 → 用 `device_id` 标签
   - 否则回退 `edge_id` 标签（覆盖 edge 1..N 的 backfill 兼容 case）
6. **构造 9 个 PromQL 表达式**（CPU avg/max、Mem avg、Load1/5/15、Disk used pct worst mount、Net rx/tx bytes/sec sum），每个 inline 嵌入 `{device_id="..."}` 或 `{edge_id="..."}` 标签
7. **30s context timeout**
8. **fan-out 并发查询**：每个 expr 起一个 goroutine，结果通过 buffered channel 回收；**单 series 失败非致命**，跳过该 channel，UI 仍渲染其它
9. **bucket-align**：取所有 series 时间戳并集，排序
10. 遍历时间戳，用 `pickPtr` / `pickU` 取每个 series 在该时间戳的值（缺失返 nil → JSON null）
11. 200 + `queryResp{Resolution: step.String(), From, To, Points}`

### runMatrix —— 单 series 查询

```go
func (h *PromHandler) runMatrix(ctx context.Context, expr string, start, end time.Time, step time.Duration) (map[int64]float64, error)
```

调 `h.q.QueryRange`，解码 matrix JSON `{metric, values:[[ts, "val"], ...]}` 到 `map[int64]float64`。多 series 时取第一个（PromQL 都 `by (edge_id)` 实际单 series）。

### queryRange —— 通用 PromQL passthrough

```go
func (h *PromHandler) queryRange(w http.ResponseWriter, r *http.Request)
```

参数：`expr`（必填，≤4 KB）、`start`/`end`（RFC3339）、`step`（duration，必填，>0）。30s timeout。返 `rangeResp`，matrix 透传 Prom 原始 JSON；非 matrix 结果返空数组 `[]`。

### helpers

- `pickPtr(m, key, ts)` —— 取 float64 指针，缺失返 nil
- `pickU(m, key, ts)` —— 取 uint64 指针，缺失返 nil
- `maxExprBytes = 4 * 1024` —— PromQL 表达式大小上限

## 5. 依赖关系

**外部**：
- `chi` —— URL 参数
- `net/http`、`encoding/json`、`strconv`、`sort`、`strings`、`time`、`fmt`、`context`

**内部**：
- `internal/pkg/promquery`（Prom 客户端 + InstantResult）
- `internal/pkg/errs`

## 6. 并发与资源管理

- **fan-out 并发查询**：9 个 PromQL 各起一个 goroutine，buffered channel `make(chan seriesData, len(exprs))` 回收
- **30s context timeout**：所有 goroutine 共享同一 ctx，超时全部取消
- **单 series 失败非致命**：`if s.err != nil { continue }`，不影响其它 series
- **无 WaitGroup**：用 `for range exprs { s := <-out }` 同步等待所有 goroutine 完成
- **无 goroutine 泄漏**：channel buffered 容量等于 goroutine 数，所有 goroutine 都能发送后退出

## 7. 设计模式与亮点

1. **wire shape 保持兼容**：响应 DTO 与旧 MySQL handler 完全一致，SPA 零改动切换后端
2. **edge_id → device_id 解析 + 回退**：处理 post-Phase-B 标签迁移，resolver 失败时回退 `edge_id` 兜底 backfill 兼容
3. **PromQL inline 标签嵌入**：所有表达式 inline 嵌入 `{device_id="..."}` 标签，server-side 强制 per-edge 隔离，SPA 无法跨 edge 泄漏
4. **fan-out 并发 + buffered channel**：9 个 PromQL 并发查询，buffered channel 避免 goroutine 阻塞，单 series 失败非致命
5. **bucket-align 取并集**：不同 PromQL 可能返回不同时间戳集合，取并集排序后统一对齐，缺失桶返 nil
6. **`*float64` 指针 null gap**：与 `http.go` 同设计，缺失桶 → JSON null → recharts 断线，避免 0 桥接
7. **`json.RawMessage` 透传 matrix**：passthrough 端点不重定义 Prom schema，让 SPA 自行 reshape per-panel
8. **`maxExprBytes = 4 KB`**：防 authenticated client 用大 PromQL pin Prom
9. **`step` 范围校验 `[5s, 24h]`**：避免极端 step 拖垮 Prom 或返空结果
10. **`h.q == nil` → 503**：graceful degradation，Prom 未配置时明确告知前端

## 8. 注意事项

1. **`dev == nil` degraded 模式**：回退 `edge_id` 标签查询，仅对 backfill 期 edge（edge_id==device_id）有效；新 edge 在 degraded 模式下指标面板会空
2. **`resolution` 参数语义**：本 handler 把 `resolution` 当作 `step` duration 处理，与 `http.go` 的 `auto|raw|5m|1h` 语义不同——前端调用前需确认后端是 PromHandler 还是旧 Handler
3. **9 个 PromQL fan-out**：每请求 9 个并发 Prom 查询，Prom 实例压力 ×9；高 QPS 下需评估 Prom 容量
4. **`runMatrix` 取第一个 series**：PromQL 都 `by (edge_id)` 实际单 series，但若 PromQL 改为 `by (cpu, edge_id)` 会丢失多 series 数据
5. **30s timeout 是硬上限**：Prom 慢查询会被取消，前端收到 503/502
6. **`queryRange` passthrough 不解析 expr**：Prom 自身校验 PromQL 合法性；本层只做大小限制
7. **`maxExprBytes = 4 KB`**：手写 PromQL 通常远小于此，但复杂聚合可能接近；如需支持更大表达式需调整常量
8. **CPU `max` 用 `min by (edge_id) (rate(idle))`**：取最忙 CPU（最低 idle），与 `http.go` 旧 handler 语义一致
9. **Disk `max by (edge_id)` 取 worst mount**：与旧 handler 一致，不是平均
10. **`Matrix` 字段非 matrix 时返 `[]`**：避免前端解析 null 报错；但 `ResultType != "matrix"` 的情况不应在 query_range 发生
