# `client.go` 技术实现文档

> 源文件：`internal/pkg/qdrantx/client.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/qdrantx`

## 1. 概述

本文件是 qdrant 向量数据库 REST API 的轻量 HTTP 封装。提供 4 类核心操作：collection 管理（创建/校验维度）、payload index 管理、point CRUD（upsert / delete / get）、向量搜索（top-K cosine + 过滤）与 scroll 列表。所有 IO 函数第一个参数为 `context.Context`，HTTP 错误体有界读取防止日志膨胀。

## 2. 包信息

- **包名**：`qdrantx`
- **所属模块**：`internal/pkg/`
- **依赖方向**：被 knowledge 服务（biz）调用；仅依赖标准库 `net/http`、`encoding/json`、`log/slog`

## 3. 关键类型与接口

```go
type Client struct {
    base string
    hc   *http.Client
    log  *slog.Logger
}

type Point struct {
    ID      uint64
    Vector  []float32
    Payload map[string]any
}

type SearchHit struct {
    ID      uint64
    Score   float64
    Payload map[string]any
}

// 过滤器 sentinel：包成 PrefixMatch 走 match.text（前缀/全文）而非精确匹配
type PrefixMatch struct{ Prefix string }

type SearchOpts struct {
    Limit     int
    MustMatch map[string]any
}

type ScrollOpts struct {
    MustMatch map[string]any
    Limit     int
    Offset    *uint64
}
```

## 4. 关键函数与流程

### `New`
- **签名**：`func New(baseURL string, log *slog.Logger) *Client`
- **职责**：构造 Client，30s HTTP 超时
- **流程**：trim 末尾 `/`，nil logger 用 `slog.Default()`

### `EnsureCollection`
- **签名**：`func (c *Client) EnsureCollection(ctx, name string, dim int) error`
- **职责**：幂等创建 collection；存在但维度不匹配时智能处理
- **流程**：
  1. GET `/collections/{name}` 检查存在性与维度
  2. 维度匹配 → 返回 nil
  3. 维度不匹配 + 有 points → 拒绝（返回明确错误并提示手动 DROP 命令）
  4. 维度不匹配 + 空 collection → DROP 后重建
  5. 不存在 → PUT `/collections/{name}` 创建（Cosine distance）
  6. 409 视为成功（已存在）
- **错误处理**：非 2xx 且非 409 时读取最多 256 字节 body 拼入错误

### `EnsurePayloadIndex`
- **签名**：`func (c *Client) EnsurePayloadIndex(ctx, collection, field, schema string) error`
- **职责**：在 field 上建 payload index；schema 选 `keyword`（默认）/ `text` / `integer` / `float` / `bool` / `geo`
- **流程**：PUT `/collections/{collection}/index?wait=true`，2xx 视为成功（包含已存在）
- **错误处理**：非 2xx 读取 256 字节 body 报错

### `Upsert / DeleteByFilter / GetPoints / DeleteByID`
- **签名**：分别对应 point 写入与删除
- **职责**：
  - `Upsert`：按 id 替换 point；`?wait=true` 同步等待
  - `DeleteByFilter`：按 payload 过滤批量删除；空 filter 拒绝（防误删全表）
  - `GetPoints`：按 uint64 id 列表获取（适用于 >2^63 的 id；Search/Scroll filter 不支持）
  - `DeleteByID`：单点删除
- **错误处理**：统一模式 — 非 2xx 读取 256 字节 body 报错

### `Search`
- **签名**：`func (c *Client) Search(ctx, collection string, vector []float32, opts SearchOpts) ([]SearchHit, error)`
- **职责**：top-K cosine 搜索 + 可选 payload 过滤
- **流程**：limit 默认 10；构造 body（vector/limit/with_payload）；通过 `buildFilter` 渲染 MustMatch 为 qdrant `must` 子句
- **错误处理**：非 2xx 报错；JSON 解码失败用 `%w` 包装

### `Scroll`
- **签名**：`func (c *Client) Scroll(ctx, collection string, opts ScrollOpts) (*ScrollResult, error)`
- **职责**：按 filter 翻页列表 point（用于 SPA `/knowledge/docs`）
- **流程**：limit 默认 100；支持 `Offset` 翻页；返回 `NextOffset` 用于下一页
- **错误处理**：同 Search

### `buildFilter`
- **签名**：`func buildFilter(must map[string]any) map[string]any`
- **职责**：把扁平 must-match map 渲染为 qdrant filter
- **流程**：按值类型分发：
  - `PrefixMatch` → `{"match": {"text": ...}}`
  - `[]string` → `{"match": {"any": [...]}}`
  - 其他 → `{"match": {"value": v}}`（exact）
- **错误处理**：空 map / 全空值 → 返回 nil（caller 不设 filter）

### `do`
- **签名**：`func (c *Client) do(ctx, method, path string, body any) (*http.Response, error)`
- **职责**：私有 HTTP 执行层
- **流程**：body 非 nil 时 JSON marshal；构造 `http.NewRequestWithContext`；设置 Content-Type；调用 `hc.Do`

### `truncate`
- **签名**：`func truncate(s string, n int) string`
- **职责**：错误体截断到 n 字节并加 `…` 后缀

## 5. 依赖关系

- **内部包**：无
- **外部库**：无（仅标准库）
- **被调用方**：knowledge biz 服务（向量存储 / 检索）

## 6. 并发与资源管理

`Client` 字段只读，安全用于并发调用。`http.Client` 内部维护连接池，可被多 goroutine 共享。所有 IO 函数第一参为 `context.Context`，可取消与超时。HTTP 30s 超时为 `New` 默认值，可通过自行构造 `Client` 字段覆盖（但本包未暴露 `NewWithHTTPClient`，需调用方自行扩展）。

## 7. 设计模式与亮点

- **维度不匹配的智能处理**：`EnsureCollection` 区分"空 collection 可重建"与"有 points 必须人工干预"，避免静默数据丢失
- **PrefixMatch sentinel**：用类型而非字符串约定语义，把"前缀匹配 vs 精确匹配"的选择交给调用方显式声明
- **空 filter 防御**：`DeleteByFilter` 拒绝空 mustMatch，防止误删全表
- **GetPoints vs Search 的 id 边界注释**：明示 uint64 >2^63 在 filter API 不安全，必须用 point-id API
- **错误体统一有界**：所有非 2xx 路径走 `truncate(..., 256)`，防止 qdrant 错误页膨胀日志

## 8. 注意事项

- **30s HTTP 超时**：大向量批量 upsert 可能不够；调用方需自行评估
- **`EnsureCollection` 维度重建**：仅在空 collection 时自动 drop+recreate；有 points 时返回错误，运维需手动介入（注释给出具体 curl 命令）
- **filter 顺序不稳定**：`buildFilter` 遍历 map，qdrant 不要求顺序但测试需注意
- **GetPoints 不带 vector**：`with_vector: false` 节省带宽；若需要原始向量需扩展 API
- **错误信息中可能含 collection 名**：在多租户场景下 collection 名可能是敏感信息，日志需注意
