# `client.go` 技术实现文档（grafana）

> 源文件：`internal/pkg/grafana/client.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/grafana`

## 1. 概述

该文件实现 Grafana 9+/10+/11 admin HTTP API 的轻量客户端，支持 Service Account token（Bearer）与 Basic Auth 两种鉴权。覆盖三类用例：连通性检查（`Health`）、Prometheus 数据源幂等 upsert（`UpsertDatasource`）、预置 dashboard 推送（`UpsertDashboard`）。还包括 SA / token / folder / dashboard fetch 等辅助方法，主要服务首次启动 bootstrap 与运行时同步。

## 2. 包信息

- **包名**：`grafana`
- **所属模块**：`internal/pkg/`（基础设施层）
- **依赖方向**：被 manager grafana 集成 BC 调用；仅依赖标准库。

## 3. 关键类型与接口

### `Client`
API wrapper，支持 Bearer 与 Basic 两种鉴权。

```go
type Client struct {
    baseURL  string
    token    string // Bearer; basicAuth 时为空
    basicUsr string
    basicPwd string
    hc       *http.Client
}
```

### `Datasource`
最小可 upsert 的数据源形状。

```go
type Datasource struct {
    UID            string
    Name           string
    Type           string // "prometheus"
    URL            string
    Access         string // "proxy"
    IsDefault      bool
    BasicAuth      bool
    BasicAuthUser  string
    JSONData       map[string]any
    SecureJSONData map[string]string
}
```

### `ServiceAccount`
Grafana SA 子集字段（id / name / login / role）。

### `ErrDashboardNotFound` / `notFoundErr`
sentinel error，分别用于 FetchDashboard 404 与 UpsertDatasource / EnsureFolder 的"不存在则创建"分支判断。

## 4. 关键函数与流程

### `New` / `NewWithBasicAuth`
- **签名**：`func New(baseURL, token string, hc *http.Client) *Client` / `func NewWithBasicAuth(...)`
- **职责**：构造客户端；trim trailing `/`；hc 为 nil 时使用 15s 默认 client。

### `Health`
- **签名**：`func (c *Client) Health(ctx context.Context) error`
- **流程**：GET `/api/health`，解码 `{database, version}`，`database != "ok"` → error `unhealthy`。

### `UpsertDatasource`
- **签名**：`func (c *Client) UpsertDatasource(ctx context.Context, ds Datasource) error`
- **流程**（GET-then-PUT/POST 幂等模式）：
  1. `ds.UID` 空 → error。
  2. `ds.Access` 空 → 默认 `"proxy"`。
  3. GET `/api/datasources/uid/<uid>`：
     - 成功 → 解码 `{ID, ReadOnly}`。
     - `ReadOnly=true`（文件 provisioning + `editable:false`）→ 直接 nil（dashboards 按 UID 引用，不可改也无需改）。
     - 否则 PUT `/api/datasources/<id>`；若返回 read-only 错误（防御性回退）→ nil。
  4. GET 返回 `notFoundErr` → POST `/api/datasources` 创建。
  5. GET 返回其他错误 → 透传。
- **设计理由**：Grafana 无"PUT by uid 幂等创建"端点，必须 GET-then-PUT/POST；provisioned 数据源 API 不可改，需特殊处理。

### `EnsureFolder`
- **签名**：`func (c *Client) EnsureFolder(ctx context.Context, uid, title string) error`
- **流程**：GET `/api/folders/<uid>` 已存在 → nil；notFound → POST `/api/folders` 创建。

### `FindServiceAccountByName` / `CreateServiceAccount` / `CreateServiceAccountToken`
- **职责**：bootstrap 路径使用——查找 / 创建 SA、mint token。
- **`FindServiceAccountByName`**：GET `/api/serviceaccounts/search?query=<name>` 后**精确名匹配**过滤（search 是子串匹配）。
- **`CreateServiceAccountToken`**：返回明文 key，Grafana 不再返回第二次，调用方需立即持久化。

### `FetchDashboard`
- **签名**：`func (c *Client) FetchDashboard(ctx context.Context, uid string) ([]byte, error)`
- **职责**：拉取完整 dashboard envelope（`{dashboard, meta}`）原始 JSON。
- **流程**：GET `/api/dashboards/uid/<uid>`；404 → 返回 `ErrDashboardNotFound`（供 handler 映射 404）。
- **设计理由**：返回 raw bytes 让 handler 透传 SPA 不丢字段。

### `UpsertDashboard`
- **签名**：`func (c *Client) UpsertDashboard(ctx context.Context, dashboard []byte, folderUID string, overwrite bool) error`
- **流程**：
  1. 空 payload → error。
  2. `json.Unmarshal` 到 `map[string]any`，删除 `id` 字段（让 Grafana 视为新 dashboard）。
  3. 包装为 `{dashboard, overwrite, message: "synced from ongrid", folderUid?}`。
  4. POST `/api/dashboards/db`。

### `do`（私有）
- **签名**：`func (c *Client) do(ctx context.Context, method, path string, payload any) ([]byte, error)`
- **流程**：
  1. baseURL 空 → error。
  2. payload 非 nil → `json.Marshal`。
  3. `http.NewRequestWithContext` + 设置 Authorization（Bearer 优先 / Basic 回退）+ Accept / Content-Type。
  4. `hc.Do`；`defer resp.Body.Close()`。
  5. `io.ReadAll(io.LimitReader(resp.Body, 1<<20))` 1 MiB 上限。
  6. 404 → `notFoundErr`；非 2xx → error 含 status + body。
- **错误处理**：每步 `%w` 包装并加 `grafana:` 前缀。

### `isReadOnlyError` / `isNotFound`
辅助判断 sentinel 与错误文本。

## 5. 依赖关系

- **内部包**：无。
- **外部库**：标准库 `bytes` / `context` / `encoding/json` / `errors` / `fmt` / `io` / `net/http` / `strings` / `time`。
- **被调用方**：manager grafana 集成 BC（首次启动 bootstrap、dashboard 同步、SPA dashboard fetch 代理）。

## 6. 并发与资源管理

无显式锁。`Client` 字段构造后不变；`http.Client` 自身并发安全，多 goroutine 并发调用方法由连接池承载。

## 7. 设计模式与亮点

- **双鉴权统一**：Bearer 与 Basic 同一 Client，`do` 内部 switch 设置 header，调用方无感。
- **GET-then-PUT/POST 幂等**：`UpsertDatasource` / `EnsureFolder` 用 notFound sentinel 分支创建 vs 更新，避免重复创建。
- **provisioned 数据源防御**：`ReadOnly` 字段 + read-only 错误文本双重检测，避免对 provisioned 资源强改报错。
- **raw JSON 透传**：`FetchDashboard` 返回 `[]byte`，handler 透传 SPA 不丢字段。
- **id 字段清除**：`UpsertDashboard` 删除 `id` 让 Grafana 视为新 dashboard，配合 overwrite + UID 实现幂等 re-push。
- **1 MiB body 上限**：`do` 用 `LimitReader` 防 OOM。
- **token 持久化提示**：`CreateServiceAccountToken` 注释强调明文 key 不再返回，调用方需立即持久化。

## 8. 注意事项

- **15s 默认超时**：dashboard 推送大 payload 可能超时；调用方可传自定义 `*http.Client` 调长。
- **无重试**：单次失败即返回，调用方需自实现重试。
- **`FindServiceAccountByName` 子串 search**：Grafana search 是子串匹配，本实现用精确等值过滤，若 Grafana 未来改 search 语义需回归。
- **`UpsertDashboard` 删除 id**：依赖 Grafana 把缺失 id 视为新 dashboard；若未来 Grafana 行为变化可能需调整。
- **1 MiB body 上限**：超大 dashboard（如 100+ panel）可能超限；需评估提升上限或分块。
- **Basic Auth 仅 bootstrap**：注释明确 Basic 仅首次启动用，之后应轮换到 SA token。
- **错误文本含 body**：非 2xx 错误含原始 body，可能泄露 Grafana 内部信息；日志记录时需注意脱敏。
