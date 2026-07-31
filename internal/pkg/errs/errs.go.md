# `errs.go` 技术实现文档

> 源文件：`internal/pkg/errs/errs.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/errs`

## 1. 概述

该文件定义跨 BC 共享的 sentinel error 集合与到 HTTP 状态码的映射函数 `HTTPStatus`。这是 ongrid HTTP 错误响应的单一真相源——所有 HTTP handler 通过 `errs.HTTPStatus(err)` 决定状态码。包刻意保持极小：BC 特定错误应在各 BC 的 biz 包定义，仅跨 BC 通用的语义级 sentinel 才入此包。

## 2. 包信息

- **包名**：`errs`
- **所属模块**：`internal/pkg/`（基础设施层，无业务依赖）
- **依赖方向**：被几乎所有 BC 的 handler / middleware 调用；仅依赖标准库。

## 3. 关键类型与接口

无显著类型定义。仅有 sentinel error 变量与一个映射函数。

### Sentinel errors

```go
var (
    ErrNotFound       = errors.New("not found")
    ErrUnauthorized   = errors.New("unauthorized")
    ErrForbidden      = errors.New("forbidden")
    ErrConflict       = errors.New("conflict")
    ErrInvalid        = errors.New("invalid argument")
    ErrTenantMismatch = errors.New("tenant mismatch")
    ErrEdgeOffline    = errors.New("edge offline")
    ErrBudgetExceeded = errors.New("budget exceeded")
    ErrNotWiredYet    = errors.New("not wired yet")
    ErrTooManyAttempts = errors.New("too many attempts")
)
```

- `ErrTooManyAttempts` 与 `ErrBudgetExceeded` 均映射 429 但语义不同——前者是反 bruteforce 短窗限流，后者是配额耗尽；区分便于日志 / metrics 分桶。

## 4. 关键函数与流程

### `HTTPStatus`
- **签名**：`func HTTPStatus(err error) int`
- **职责**：把已知 sentinel 映射到 HTTP 状态码；未知 → 500。
- **流程**（switch 顺序）：
  1. `err == nil` → 200。
  2. `errors.Is(err, ErrNotFound)` → 404。
  3. `errors.Is(err, ErrUnauthorized)` → 401。
  4. `errors.Is(err, ErrForbidden)` 或 `ErrTenantMismatch` → 403。
  5. `errors.Is(err, ErrConflict)` → 409。
  6. `errors.Is(err, ErrInvalid)` → 400。
  7. `errors.Is(err, ErrBudgetExceeded)` 或 `ErrTooManyAttempts` → 429。
  8. `errors.Is(err, ErrEdgeOffline)` → 503。
  9. `errors.Is(err, ErrNotWiredYet)` → 501。
  10. default → 500。
- **设计理由**：用 `errors.Is` 而非 `==`，支持 BC 用 `fmt.Errorf("...: %w", errs.ErrNotFound)` 包装后仍能命中映射。

## 5. 依赖关系

- **内部包**：无。
- **外部库**：标准库 `errors` / `net/http`。
- **被调用方**：所有 HTTP handler / middleware（authzmw、iam、manager、edge 等）；错误响应统一框架。

## 6. 并发与资源管理

无并发控制。sentinel error 为包级 `var`，构造后不可变；`HTTPStatus` 是纯函数，并发安全。

## 7. 设计模式与亮点

- **sentinel + errors.Is**：sentinel error 配合 `errors.Is` 支持 `%w` 包装链，BC 可在保留自身错误上下文的同时复用通用映射。
- **单一真相源**：HTTP 状态码映射集中在一处，避免各 handler 各自 switch 导致不一致。
- **极简克制**：包刻意小，注释明确"BC 特定错误应在各 BC biz 包定义"，防止演化成大杂烩。
- **语义区分**：`ErrTooManyAttempts` vs `ErrBudgetExceeded` 同 429 但语义不同，便于日志 / metrics 区分反 bruteforce 与配额耗尽。
- **`ErrTenantMismatch` 复用 403**：租户不匹配本质是禁止访问，复用 403 而非引入新状态码。
- **`ErrNotWiredYet` → 501**：明确"功能未接线"语义，区分于 500（未知错误）。

## 8. 注意事项

- **未知错误统一 500**：default 分支无日志，调用方应在 handler 层先 log 再返回，避免错误被吞。
- **`errors.Is` 链深度**：若 BC 多层包装后链中无 sentinel，会落到 default 500；包装时务必用 `%w` 而非 `%v`。
- **新增 sentinel 需同步映射**：增加新 error 必须在 `HTTPStatus` 加 case，否则永远 500。
- **不区分原因**：同一 sentinel 可能由多种原因触发（如 `ErrNotFound` 可能是 user / edge / doc 任意资源缺失），handler 若需更细粒度需自行包装上下文。
- **`ErrTooManyAttempts` 与 `ErrBudgetExceeded` 同码**：客户端仅看 HTTP code 无法区分；需在 response body 或 header 额外区分。
- **无错误体规范**：本包仅定义状态码，response body 格式（`{code, message, data}`）由 handler 层另行实现。
