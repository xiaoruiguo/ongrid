# query.go

## 1. 概述

`query.go` 实现 metric 包的范围查询门面 —— `QueryUsecase` 接收 `RangeQuery`，校验后按时间窗口自动选择分辨率（raw / 5m / 1h），委派给对应 `Reader` 方法。

自动分辨率规则：
- 窗口 ≤ 6h → raw
- 窗口 ≤ 7d → 5m
- 否则 → 1h

校验规则：`EdgeID` 非零、`From < To`、窗口 ≤ 365d、`Resolution` 是已知值（空 = `auto`）。

## 2. 包信息

- 包名：`metric`
- 路径：`internal/manager/biz/metric`

## 3. 关键类型与接口

### Resolution

```go
type Resolution string

const (
    ResAuto Resolution = "auto"
    ResRaw  Resolution = "raw"
    Res5m   Resolution = "5m"
    Res1h   Resolution = "1h"
)
```

### Bounds 常量

```go
const (
    autoRawUpper = 6 * time.Hour
    auto5mUpper  = 7 * 24 * time.Hour
    maxWindow    = 365 * 24 * time.Hour
)
```

### RangeQuery

```go
type RangeQuery struct {
    EdgeID     uint64
    From       time.Time
    To         time.Time
    Resolution Resolution  // 默认 ResAuto
}
```

### Series

```go
type Series struct {
    Resolution Resolution
    RawPoints  []model.Point      // 三选一非 nil
    Buckets5m  []model.Bucket5m
    Buckets1h  []model.Bucket1h
}
```

### QueryUsecase

```go
type QueryUsecase struct {
    reader Reader
    log    *slog.Logger
}
```

## 4. 关键函数与流程

### Query

```go
func (u *QueryUsecase) Query(ctx, q RangeQuery) (*Series, error)
```

1. 校验：
   - `q.EdgeID == 0` → `ErrInvalid: edge_id required`
   - `!q.From.Before(q.To)` → `ErrInvalid: from must be before to`
   - `window > maxWindow` → `ErrInvalid: window exceeds 365d`
2. `res = q.Resolution`，空 → `ResAuto`
3. `ResAuto` → `autoResolution(window)`
4. switch res：
   - `ResRaw` → `reader.QueryRaw(ctx, edgeID, from, to)` → `Series{Resolution: ResRaw, RawPoints: pts}`
   - `Res5m` → `reader.Query5m` → `Series{Resolution: Res5m, Buckets5m: bs}`
   - `Res1h` → `reader.Query1h` → `Series{Resolution: Res1h, Buckets1h: bs}`
   - default → `errors.Join(ErrInvalid, "unknown resolution")`

### autoResolution

```go
func autoResolution(window time.Duration) Resolution {
    switch {
    case window <= autoRawUpper:  return ResRaw
    case window <= auto5mUpper:   return Res5m
    default:                      return Res1h
    }
}
```

## 5. 依赖关系

### 外部包

- `context` / `errors` / `fmt` / `log/slog` / `time`

### 内部包

- `model "github.com/ongridio/ongrid/internal/manager/model/metric"` —— `Point` / `Bucket5m` / `Bucket1h`
- `github.com/ongridio/ongrid/internal/pkg/errs` —— `ErrInvalid`

### 依赖接口（在 repo.go 定义）

- `Reader.QueryRaw` / `Query5m` / `Query1h`

### 被谁调用

- HTTP handler（`/v1/metrics/*`）调 `Query` 返回时序数据给 SPA
- report 包的 `facts.go` 可能调 Reader 直接（绕过 usecase）

## 6. 并发与资源管理

- 无锁、无 goroutine，纯同步
- 无共享可变状态；并发安全由 Reader 实现保证
- 所有 IO 首参 `ctx`

## 7. 设计模式与亮点

### 自动分辨率选择

`ResAuto` 按窗口大小自动选表，让调用方不必关心物理表分层。6h / 7d 边界对应 raw / 5m / 1h 的合理使用范围 —— 短窗口看细节用 raw，长窗口看趋势用 1h。

### 严格校验 + 明确错误

四条校验都返回 `errs.ErrInvalid` wrapped error，消息明确（`edge_id required` / `from must be before to` / `window exceeds 365d`）。让 HTTP handler 能直接 400 + 消息。

### Series 三选一

`Series` 用三个字段（`RawPoints` / `Buckets5m` / `Buckets1h`）而非 oneOf。注释：exactly one non-nil。调用方按 `Resolution` 判断哪个有效。简单直接。

### 365d 窗口上限

`maxWindow = 365 * 24 * time.Hour`。防止恶意超大窗口打爆 DB。配合 retention 的 1h 表 365d TTL，超过 365d 的查询无意义（数据已删）。

### default case 防御

`switch res` 的 default 返回 `errors.Join(ErrInvalid, "unknown resolution")`。即使 `Resolution` 是字符串被外部传任意值，也不会 panic。

## 8. 注意事项

- **`Resolution` 是字符串**：DB 持久化或 API 传值时易拼错。`ResAuto`/`ResRaw`/`Res5m`/`Res1h` 是稳定 wire shape
- **`autoRawUpper` / `auto5mUpper` 是包私有常量**：测试无法覆盖边界。若需测试边界行为，应在同包内写测试
- **`From < To` 严格**：`From == To` 会被拒。若需单点查询，调用方应构造 `[t, t+1ns]` 或单独走 GetByTime 路径
- **`maxWindow` 365d 与 retention 1h TTL 对齐**：超过 365d 的查询即使不拒也会返回空。改 retention TTL 应同步改 maxWindow
- **`Series` 三字段而非 oneOf**：JSON 序列化时三个字段都会出现（nil 的为 null）。SPA 按 `resolution` 判断哪个有效
- **`QueryUsecase.log` 几乎不用**：当前 Query 无 log。保留字段以备未来加慢查询日志
- **无缓存**：每次查询直接打 DB。高频重复查询应由调用方缓存或加 materialized view
