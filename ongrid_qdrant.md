# OnGrid × Qdrant 集成技术实现说明文档

> 本文档深入分析 OnGrid 系统与 Qdrant 向量数据库集成的全部代码路径：qdrantx HTTP 客户端、knowledge biz 编排层、wire 协议、ID 派生、chunk 分片、payload 索引、过滤渲染、双向映射（payload ↔ Doc）、生命周期回调、aiops tools 的 RAG 消费、systemhealth 探测、配置装配、docker-compose 部署面、Dockerfile ONNX Runtime 依赖、前端 API。
>
> 全部行号引用基于撰写时仓库快照，可能随后续提交漂移；尽量给出文件路径锚点便于跳转。

---

## 目录

1. [架构总览](#1-架构总览)
2. [分层与文件索引](#2-分层与文件索引)
3. [qdrantx.Client：HTTP 客户端](#3-qdrantxclienthttp-客户端)
4. [Wire 协议：Point / SearchHit / 过滤渲染](#4-wire-协议point--searchhit--过滤渲染)
5. [knowledge biz：Usecase 编排核心](#5-knowledge-bizusecase-编排核心)
6. [Collection 与 Payload Index 初始化](#6-collection-与-payload-index-初始化)
7. [Doc CRUD：单点与多 chunk 双路径](#7-doc-crud单点与多-chunk-双路径)
8. [ID 派生：稳定哈希与 chunk 索引](#8-id-派生稳定哈希与-chunk-索引)
9. [Chunk 分片：splitForChunks 与 overlap](#9-chunk-分片splitforchunks-与-overlap)
10. [Repo Sync：git 拉取 → 扫描 → 嵌入 → upsert](#10-repo-syncgit-拉取--扫描--嵌入--upsert)
11. [Built-in Vault：embed.FS + cloud 优先 + embedded fallback](#11-built-in-vaultembedfs--cloud-优先--embedded-fallback)
12. [Search：embedding → 服务端过滤 → 去重](#12-searchembedding--服务端过滤--去重)
13. [ListDocs / ListPaths：Scroll + dedupeByIDAlias](#13-listdocs--listpathsscroll--dedupebyidalias)
14. [Embedder：OpenAI 兼容 + 本地 ONNX 双实现](#14-embedderopenai-兼容--本地-onnx-双实现)
15. [aiops tools：query_knowledge Caller 接口](#15-aiops-toolsquery_knowledge-caller-接口)
16. [HTTP 路由：knowledge.Handler](#16-http-路由knowledgehandler)
17. [systemhealth：Qdrant + Embedding 探测](#17-systemhealthqdrant--embedding-探测)
18. [配置与启动装配](#18-配置与启动装配)
19. [部署面：docker-compose + Dockerfile + .env](#19-部署面docker-compose--dockerfile--env)
20. [前端 API：knowledge.ts](#20-前端-apiknowledgets)
21. [并发、错误与可观测性](#21-并发错误与可观测性)
22. [架构红线与设计要点](#22-架构红线与设计要点)
23. [附录：测试覆盖](#23-附录测试覆盖)

---

## 1. 架构总览

OnGrid 的知识库（RAG）以 Qdrant 为唯一向量存储。三方拓扑：

```
SPA /knowledge ──(HTTP)──▶ manager /v1/knowledge/*
                              │
                              ▼
                       knowledge.Usecase (biz)
                              │
                ┌─────────────┼─────────────┐
                ▼             ▼             ▼
          qdrantx.Client  embedding    RepoStore(MySQL)
          (向量读写)       (Embedder)   (repo 注册表)
                │             │
                ▼             ▼
            Qdrant:6333   OpenAI / 本地 ONNX
            (HTTP REST)   (BGE-small-zh-v1.5)
```

- **MySQL**：仅承载 `knowledge_repos`（git 仓库注册）+ `ssh_identities`（SSH 凭据）两张小关系表。**所有 doc 正文 + 向量只在 Qdrant**，无双写。
- **Qdrant**：单 collection（默认 `ongrid_knowledge`），Cosine distance，point ID 为 `md5(scope||url)` 取高 8 字节的 `uint64`。
- **Embedder**：两路实现——OpenAI 兼容 HTTP（OpenAI / GLM / Qwen / DeepSeek）或本地 ONNX（`fastembed-go` + BGE-small-zh-v1.5，dim=512）。后者用于 air-gapped / 无外网环境（ADR-027）。
- **RAG 消费**：`aiops/tools/query_knowledge` 是 LLM-callable BaseTool，对 Qdrant 做语义检索；前端 `/knowledge/search` 是人工检索入口。

这种"Qdrant 作为唯一向量真源 + MySQL 仅存元数据"的设计，让 RAG 写路径与读路径共享同一份 payload schema，避免关系表与向量库之间的双写一致性陷阱。

---

## 2. 分层与文件索引

| 层 | 文件 | 行数 | 职责 |
|----|------|------|------|
| **pkg 客户端** | [internal/pkg/qdrantx/client.go](file:///d:/claude/ongrid/internal/pkg/qdrantx/client.go) | 431 | Qdrant REST API 薄封装 |
| **pkg 客户端测试** | [internal/pkg/qdrantx/filter_test.go](file:///d:/claude/ongrid/internal/pkg/qdrantx/filter_test.go) | 74 | buildFilter 单测 |
| **embedding 接口** | [internal/pkg/embedding/embedding.go](file:///d:/claude/ongrid/internal/pkg/embedding/embedding.go) | 252 | Embedder 接口 + OpenAI 实现 |
| **embedding 本地** | [internal/pkg/embedding/local.go](file:///d:/claude/ongrid/internal/pkg/embedding/local.go) | 231 | ONNX 本地嵌入 |
| **biz 编排** | [internal/man/biz/knowledge/usecase.go](file:///d:/claude/ongrid/internal/manager/biz/knowledge/usecase.go) | 2226 | Usecase 主流程 |
| **biz 内置 vault** | [internal/manager/biz/knowledge/builtin_vault.go](file:///d:/claude/ongrid/internal/manager/biz/knowledge/builtin_vault.go) | 109 | embed.FS 内置 vault |
| **biz 测试** | [internal/manager/biz/knowledge/chunk_test.go](file:///d:/claude/ongrid/internal/manager/biz/knowledge/chunk_test.go) | 99 | splitForChunks 回归 |
| **biz 测试** | [internal/manager/biz/knowledge/dedupe_test.go](file:///d:/claude/ongrid/internal/manager/biz/knowledge/dedupe_test.go) | 154 | dedupeByIDAlias 回归 |
| **biz 测试** | [internal/manager/biz/knowledge/builtin_vault_test.go](file:///d:/claude/ongrid/internal/manager/biz/knowledge/builtin_vault_test.go) | 117 | 内置 vault 物化 |
| **biz 测试** | [internal/manager/biz/knowledge/upload_edit_test.go](file:///d:/claude/ongrid/internal/manager/biz/knowledge/upload_edit_test.go) | 80+ | ADR-028 upload CRUD |
| **model** | [internal/manager/model/knowledge/model.go](file:///d:/claude/ongrid/internal/manager/model/knowledge/model.go) | 161 | Doc / Repository / SSHIdentity |
| **data store** | [internal/manager/data/knowledge/store/repo.go](file:///d:/claude/ongrid/internal/manager/data/knowledge/store/repo.go) | 168 | MySQL 关系表 CRUD |
| **HTTP handler** | [internal/manager/server/knowledge/http.go](file:///d:/claude/ongrid/internal/manager/server/knowledge/http.go) | 611 | 路由 + DTO |
| **HTTP 测试** | [internal/manager/server/knowledge/http_test.go](file:///d:/claude/ongrid/internal/manager/server/knowledge/http_test.go) | 263+ | in-memory qdrant (memVec) |
| **aiops tool** | [internal/manager/biz/aiops/tools/query_knowledge_basetool.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/query_knowledge_basetool.go) | 181 | LLM RAG 工具 |
| **systemhealth** | [internal/manager/service/systemhealth/service.go](file:///d:/claude/ongrid/internal/manager/service/systemhealth/service.go) | 384 | checkQdrant + checkEmbedding |
| **systemhealth 测试** | [internal/manager/service/systemhealth/service_test.go](file:///d:/claude/ongrid/internal/manager/service/systemhealth/service_test.go) | - | httptest 桩 |
| **装配** | [cmd/ongrid/main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go) | L1225-1333 | knowledgeUC 构造 + vault seed |
| **部署** | [deploy/docker-compose.yml](file:///d:/claude/ongrid/deploy/docker-compose.yml) | L308-315 | qdrant 服务 |
| **部署（install）** | [deploy/install/docker-compose.yml](file:///d:/claude/ongrid/deploy/install/docker-compose.yml) | L400-413 | qdrant 生产定义 |
| **Dockerfile** | [deploy/Dockerfile.ongrid](file:///d:/claude/ongrid/deploy/Dockerfile.ongrid) | L82-113 | ONNX Runtime 安装 |
| **环境变量样例** | [deploy/install/.env.example](file:///d:/claude/ongrid/deploy/install/.env.example) | L90-115 | embedding 配置说明 |
| **模型预下载** | [dist/fetch-embedding-model.sh](file:///d:/claude/ongrid/dist/fetch-embedding-model.sh) | 57 | BGE-small-zh-v1.5 预缓存 |
| **前端 API** | [web/src/api/knowledge.ts](file:///d:/claude/ongrid/web/src/api/knowledge.ts) | 403 | SPA 客户端 + i18n |

---

## 3. qdrantx.Client：HTTP 客户端

源文件：[internal/pkg/qdrantx/client.go](file:///d:/claude/ongrid/internal/pkg/qdrantx/client.go)

### 3.1 包设计哲学（L1-L15）

包注释明确：**不引入 upstream go-client**（gRPC-first 太重），只用 REST API 的 4 类操作：

- `ensure-collection` / `ensure-payload-index`
- `upsert` / `delete-by-filter` / `delete-by-id` / `get-points`
- `search`（top-K cosine + 服务端过滤）
- `scroll`（按 filter 翻页）

约定（L7-L14）：
- 每个 OnGrid 部署一个 collection，默认名 `knowledge`
- 向量 `float32` + Cosine distance
- Point ID 为 `uint64`，由 knowledge biz 用 `md5(payload.url) >> 64` 派生，**跨 sync 稳定**以让重导覆盖旧 point 而非重复
- Payload 必带 `source_type` / `title` / `content` / `url` / `repo_id`（当 source_type=repo）

### 3.2 Client 结构（L29-L46）

```go
type Client struct {
    base string              // trim 末尾 /
    hc   *http.Client        // 30s 超时
    log  *slog.Logger        // nil → slog.Default()
}

func New(baseURL string, log *slog.Logger) *Client {
    // ...
    return &Client{
        base: strings.TrimRight(baseURL, "/"),
        hc:   &http.Client{Timeout: 30 * time.Second},
        log:  log,
    }
}
```

字段全部只读，安全用于并发调用。`http.Client` 内部连接池可被多 goroutine 共享。

### 3.3 EnsureCollection：维度智能处理（L48-L116）

最复杂的写操作，处理三种状态：

1. **不存在** → PUT `/collections/{name}` 创建（Cosine distance）
2. **存在且维度匹配** → 直接返回 nil
3. **存在但维度不匹配**：
   - 有 points → **拒绝**，返回错误并提示 `curl -X DELETE` 命令（L82-L85）防止数据丢失
   - 空 collection → DROP + 重建（L88-L92）

关键代码（L60-L96）：

```go
getResp, err := c.do(ctx, http.MethodGet, "/collections/"+name, nil)
if err == nil && getResp.StatusCode == 200 {
    var info struct {
        Result struct {
            Config struct {
                Params struct {
                    Vectors struct {
                        Size int `json:"size"`
                    } `json:"vectors"`
                } `json:"params"`
            } `json:"config"`
            PointsCount int `json:"points_count"`
        } `json:"result"`
    }
    // ... 解码 + 维度比对 ...
    if info.Result.PointsCount > 0 {
        return fmt.Errorf("qdrant: collection %s has dim %d but caller wants %d, "+
            "and %d points already exist — refusing to drop. "+
            "Drop manually with `curl -X DELETE qdrant:6333/collections/%s` "+
            "after backing up if needed", ...)
    }
    // 空 collection — 安全 drop+recreate
    delResp, _ := c.do(ctx, http.MethodDelete, "/collections/"+name, nil)
    // fall through to recreate
}
```

409 视为成功（已存在），见 L111：`if resp.StatusCode/100 != 2 && resp.StatusCode != 409`。

### 3.4 EnsurePayloadIndex（L118-L150）

PUT `/collections/{collection}/index?wait=true`，schema 选 `keyword`（默认）/ `text` / `integer` / `float` / `bool` / `geo`。

`?wait=true` 让 qdrant 同步建索引完成才返回，避免后续过滤查询时索引尚未生效。2xx 视为成功（含已存在）。

### 3.5 Upsert / DeleteByFilter / GetPoints / DeleteByID（L152-L249）

| 方法 | HTTP | 端点 | 关键约束 |
|------|------|------|----------|
| `Upsert` | PUT | `/collections/{c}/points?wait=true` | 按 id 替换；空切片直接返 nil |
| `DeleteByFilter` | POST | `/collections/{c}/points/delete?wait=true` | 空 mustMatch **拒绝**（防误删全表，L181） |
| `GetPoints` | POST | `/collections/{c}/points` | 用 point-id API 而非 filter，因 ID 是 uint64 可能 >2^63（filter 解析器拒绝，L207-L208 注释） |
| `DeleteByID` | POST | `/collections/{c}/points/delete?wait=true` | 单点删除 |

`GetPoints` 注释（L205-L208）特别强调：point-id API 原生支持 uint64，而 Search/Scroll filter 不支持，因此 doc 按 ID 加载必须走这条路径。

### 3.6 Search（L272-L302）

POST `/collections/{c}/points/search`，body：

```go
body := map[string]any{
    "vector":       vector,
    "limit":        limit,
    "with_payload": true,
}
if f := buildFilter(opts.MustMatch); f != nil {
    body["filter"] = f
}
```

`Limit` 默认 10（L274-L277）。响应解码为 `[]SearchHit`（含 ID/Score/Payload）。

### 3.7 Scroll（L304-L351）

POST `/collections/{c}/points/scroll`，支持 `Offset` 翻页（返回 `NextOffset`）。`Limit` 默认 100。用于 SPA 的 `/knowledge/docs` listing 与 `ListPaths` 全量扫描。

### 3.8 do（私有 HTTP 执行层，L353-L370）

```go
func (c *Client) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
    var rdr io.Reader
    if body != nil {
        b, err := json.Marshal(body)
        // ...
        rdr = bytes.NewReader(b)
    }
    req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
    // ...
    if body != nil {
        req.Header.Set("Content-Type", "application/json")
    }
    return c.hc.Do(req)
}
```

注意：**所有非 2xx 错误体读取走 `truncate(..., 256)`**（L113 / L148 / L172 / L200 / L225 / L246 / L293 / L342），防止 qdrant 错误页（可能含整页 HTML）膨胀日志。

---

## 4. Wire 协议：Point / SearchHit / 过滤渲染

源文件：[internal/pkg/qdrantx/client.go](file:///d:/claude/ongrid/internal/pkg/qdrantx/client.go) L152-L424

### 4.1 Point / SearchHit

```go
type Point struct {
    ID      uint64         `json:"id"`
    Vector  []float32      `json:"vector"`
    Payload map[string]any `json:"payload,omitempty"`
}

type SearchHit struct {
    ID      uint64         `json:"id"`
    Score   float64        `json:"score"`
    Payload map[string]any `json:"payload"`
}
```

### 4.2 SearchOpts / ScrollOpts

```go
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

`MustMatch` 的 value 类型决定渲染分支（见下）。

### 4.3 PrefixMatch sentinel（L372-L376）

```go
// PrefixMatch is a sentinel value for buildFilter — wrap a string in
// it to request `match.text` (prefix / full-text match against a
// `text`-schema payload index) instead of the default exact-value
// match. Used for path-prefix filtering ("网络/" matches "网络/DNS").
type PrefixMatch struct{ Prefix string }
```

用类型而非字符串约定语义，把"前缀匹配 vs 精确匹配"的选择交给调用方显式声明。

### 4.4 buildFilter（L386-L424）

```go
func buildFilter(must map[string]any) map[string]any {
    if len(must) == 0 {
        return nil
    }
    conds := make([]map[string]any, 0, len(must))
    for k, v := range must {
        switch tv := v.(type) {
        case PrefixMatch:
            if tv.Prefix == "" { continue }
            conds = append(conds, map[string]any{
                "key":   k,
                "match": map[string]any{"text": tv.Prefix},
            })
        case []string:
            if len(tv) == 0 { continue }
            anyList := make([]any, 0, len(tv))
            for _, s := range tv { anyList = append(anyList, s) }
            conds = append(conds, map[string]any{
                "key":   k,
                "match": map[string]any{"any": anyList},
            })
        default:
            conds = append(conds, map[string]any{
                "key":   k,
                "match": map[string]any{"value": v},
            })
        }
    }
    if len(conds) == 0 { return nil }
    return map[string]any{"must": conds}
}
```

三种渲染：

| 值类型 | qdrant 子句 | 用途 |
|--------|-------------|------|
| `PrefixMatch` | `{"match": {"text": ...}}` | 前缀 / 全文（需 `text` schema 索引） |
| `[]string` | `{"match": {"any": [...]}}` | any-of（tags 任一匹配） |
| string / number / bool | `{"match": {"value": v}}` | 精确匹配 |

空 map 或全空值 → 返回 nil（caller 不设 filter）。

**注意**：`buildFilter` 遍历 map，qdrant 不要求顺序但单测需注意（见 [filter_test.go](file:///d:/claude/ongrid/internal/pkg/qdrantx/filter_test.go)）。

---

## 5. knowledge biz：Usecase 编排核心

源文件：[internal/manager/biz/knowledge/usecase.go](file:///d:/claude/ongrid/internal/manager/biz/knowledge/usecase.go)

### 5.1 包设计（L1-L15）

包注释明确两个流程：

1. **Manual docs**：用户通过 SPA `/knowledge` 页面粘贴 markdown，嵌入后 upsert 到 qdrant
2. **Repo sync**：用户注册 git URL，`Sync()` shell `git clone --depth=1`（或后续 `git pull`）到 `/var/lib/ongrid/repos/<id>`，遍历 `.md` / `.txt` / `.rst` / `.yaml` / `.yml` / `.toml` / `.json` 文件，嵌入每个，替换该 repo 的 qdrant point 集

**关键设计**：repo 注册存在 MySQL，doc 正文只在 qdrant，**无双写**。

### 5.2 QdrantClient 窄接口（L60-L71）

biz 层不直接依赖 `*qdrantx.Client`，而是定义窄接口：

```go
type QdrantClient interface {
    EnsureCollection(ctx context.Context, name string, dim int) error
    EnsurePayloadIndex(ctx context.Context, collection, field, schema string) error
    Upsert(ctx context.Context, collection string, points []qdrantx.Point) error
    DeleteByFilter(ctx context.Context, collection string, mustMatch map[string]any) error
    DeleteByID(ctx context.Context, collection string, id uint64) error
    GetPoints(ctx context.Context, collection string, ids []uint64) ([]qdrantx.SearchHit, error)
    Search(ctx context.Context, collection string, vector []float32, opts qdrantx.SearchOpts) ([]qdrantx.SearchHit, error)
    Scroll(ctx context.Context, collection string, opts qdrantx.ScrollOpts) (*qdrantx.ScrollResult, error)
}
```

测试可注入 fake（见 [upload_edit_test.go](file:///d:/claude/ongrid/internal/manager/biz/knowledge/upload_edit_test.go) 的 `fakeVec` 与 [http_test.go](file:///d:/claude/ongrid/internal/manager/server/knowledge/http_test.go) 的 `memVec`）。

### 5.3 CollectionName 常量（L73-L74）

```go
// CollectionName is the single qdrant collection ongrid writes to.
const CollectionName = "ongrid_knowledge"
```

systemhealth 配置默认值（[service.go](file:///d:/claude/ongrid/internal/manager/service/systemhealth/service.go) L123-L124）也用这个名字：`if cfg.QdrantCollection == "" { cfg.QdrantCollection = "ongrid_knowledge" }`。

### 5.4 Usecase 结构（L92-L104）

```go
type Usecase struct {
    repo     RepoStore             // MySQL 关系表
    vec      QdrantClient          // qdrant
    embed    embedding.Embedder    // 嵌入器
    cloneDir string                // git clone 落盘点
    log      *slog.Logger
    onRepoDelete func(ctx context.Context, url string)  // 删除后回调
}
```

`onRepoDelete` 用于 main.go 注入"vault_seed_optout sentinel 持久化"钩子（L99-L103 注释）。

### 5.5 New 构造（L121-L167）

```go
func New(ctx context.Context, repo RepoStore, vec QdrantClient, embed embedding.Embedder, cloneDir string, log *slog.Logger) (*Usecase, error) {
    if cloneDir == "" { cloneDir = "/var/lib/ongrid/repos" }
    if vec == nil { return nil, errors.New("knowledge: qdrant client required") }
    // 维度选择：embedder > ONGRID_EMBEDDING_DIM env > 默认 1536
    dim := 1536
    if embed != nil {
        dim = embed.Dim()
    } else if v := os.Getenv("ONGRID_EMBEDDING_DIM"); v != "" {
        if n, err := strconv.Atoi(v); err == nil && n > 0 { dim = n }
    }
    if err := vec.EnsureCollection(ctx, CollectionName, dim); err != nil {
        return nil, fmt.Errorf("knowledge: ensure collection: %w", err)
    }
    // 建 5 个 payload index
    for _, f := range []string{"source_type", "repo_id", "path", "path_prefixes", "tags"} {
        if err := vec.EnsurePayloadIndex(ctx, CollectionName, f, "keyword"); err != nil {
            log.Warn("knowledge: payload index", slog.String("field", f), slog.Any("err", err))
        }
    }
    return &Usecase{repo: repo, vec: vec, embed: embed, cloneDir: cloneDir, log: log}, nil
}
```

**关键设计**：`embed == nil` 仍可构造（用于 fresh install 无 LLM key 时让 SPA 列表渲染，写路径在调用时 gate）。维度从 embedder 取，否则从 env 取（保证读路径能 EnsureCollection）。

### 5.6 SourceType 常量（model.go L24-L38）

```go
const (
    SourceManual = "manual"  // 用户粘贴
    SourceRepo   = "repo"    // git 仓库导入
    SourceURL    = "url"     // Phase-2 未实现
    SourceVault  = "vault"   // 内置平台 vault（非 repo row）
    SourceUpload = "upload"  // 组织上传文件（ADR-028）
)
```

`SourceVault` 与 `SourceUpload` 都不带 `repo_id`，但前者是平台内容（用户不可编辑），后者是组织内容（完整 CRUD）。

---

## 6. Collection 与 Payload Index 初始化

### 6.1 五个 keyword 索引（usecase.go L159-L165）

```go
for _, f := range []string{"source_type", "repo_id", "path", "path_prefixes", "tags"} {
    if err := vec.EnsurePayloadIndex(ctx, CollectionName, f, "keyword"); err != nil {
        log.Warn(...)
    }
}
```

### 6.2 path_prefixes 设计（usecase.go L401-L424）

`pathPrefixes` 函数返回路径的每个累积前缀：

```go
// "网络/DNS/排查" → ["网络", "网络/DNS", "网络/DNS/排查"]
// "网络" → ["网络"]
// "" → nil
func pathPrefixes(p string) []string {
    p = normalizePath(p)
    if p == "" { return nil }
    parts := strings.Split(p, "/")
    out := make([]string, 0, len(parts))
    for i := range parts {
        out = append(out, strings.Join(parts[:i+1], "/"))
    }
    return out
}
```

**为什么不用 qdrant text tokenizer？** usecase.go L153-L155 注释：

> We avoided qdrant's `text` tokenizer because "网络" tokenizes loosely and would match "K8s/网络" by mistake.

所以 prefix 查询走的是 **`path_prefixes` 数组字段的精确 keyword 匹配**——每个 doc 把所有累积前缀都存进去，查 `path_prefix=网络` 就精确匹配数组里有没有 `"网络"` 这一项。

### 6.3 id_alias 不索引（usecase.go L156-L158）

```go
// id_alias intentionally not indexed: doc IDs span the full uint64
// range (md5-derived) and qdrant's filter parser rejects values
// > int64, so we never filter by id (use GetPoints instead).
```

---

## 7. Doc CRUD：单点与多 chunk 双路径

### 7.1 CreateManualDoc（usecase.go L184-L217）

```go
func (u *Usecase) CreateManualDoc(ctx context.Context, in CreateManualDocInput) (*model.Doc, error) {
    if u.embed == nil {
        return nil, fmt.Errorf("%w: embedder not configured (set ONGRID_EMBEDDING_API_KEY)", errs.ErrNotWiredYet)
    }
    // ... validate + trim ...
    id := manualDocID(title)
    d := &model.Doc{
        ID:         id,
        SourceType: model.SourceManual,
        // ... 其他字段
    }
    if err := u.upsertDoc(ctx, d); err != nil { return nil, err }
    return d, nil
}
```

ID 由 `manualDocID(title) = docID("manual||" + title)` 派生（L1872-L1874）——同一 title 重导覆盖原 point。

### 7.2 upsertDoc 单点写入（usecase.go L1339-L1378）

```go
func (u *Usecase) upsertDoc(ctx context.Context, d *model.Doc) error {
    vecs, err := u.embed.Embed(ctx, []string{truncateForEmbedding(d.Title + "\n\n" + d.Content)})
    // ...
    pt := qdrantx.Point{
        ID:     d.ID,
        Vector: vecs[0],
        Payload: map[string]any{
            "source_type":   d.SourceType,
            "title":         d.Title,
            "title_en":      d.TitleEN,
            "content":       d.Content,
            "url":           d.URL,
            "path":          d.Path,
            "chunk_index":   0,           // manual doc 始终单 chunk
            "chunk_total":   1,
            "path_prefixes": pathPrefixes(d.Path),
            "tags":          d.Tags,
            "created_at":    d.CreatedAt.Format(time.RFC3339),
            "updated_at":    d.UpdatedAt.Format(time.RFC3339),
            "id_alias":      d.ID,        // helper field
        },
    }
    if d.RepoID != nil { pt.Payload["repo_id"] = *d.RepoID }
    return u.vec.Upsert(ctx, CollectionName, []qdrantx.Point{pt})
}
```

**关键修复历史**（L1357-L1364 注释）：`chunk_index` / `chunk_total` 是后来加的；早期 manual doc 不带这两个字段，导致 ListDocs MustMatch `chunk_index=0` 把它们全过滤掉（"RAG 又没了" bug）。现在 manual doc 强制写 `chunk_index=0` 保持一致。

### 7.3 UploadDoc + ingestUpload 多 chunk（usecase.go L234-L318）

`UploadDoc` 是 ADR-028 引入的组织文件上传，**多 chunk**：

```go
func (u *Usecase) ingestUpload(ctx context.Context, d model.Doc) (*model.Doc, error) {
    // 1. 删除该 url 的所有旧 chunk
    if err := u.vec.DeleteByFilter(ctx, CollectionName, map[string]any{
        "source_type": model.SourceUpload,
        "url":         d.URL,
    }); err != nil { return nil, fmt.Errorf("knowledge: clear prior upload: %w", err) }

    // 2. chunk + 嵌入 + upsert，batch=32
    parts := splitForChunks(d.Content)
    const batch = 32
    for i := 0; i < len(parts); i += batch {
        // ... embed batch + uploadChunkPoint 构建 ...
        if err := u.vec.Upsert(ctx, CollectionName, points); err != nil { ... }
    }
    out := d
    out.ID = uploadChunkDocID(d.URL, 0)
    return &out, nil
}
```

**幂等性**：先 `DeleteByFilter` 清旧 chunk，再 upsert 新 chunk。处理"文件变短"（chunk 数减少）场景——不清旧 chunk 会留下 stale 高索引 chunk。

### 7.4 UpdateManualDoc 双分支（usecase.go L334-L380）

```go
switch existing.SourceType {
case model.SourceManual:
    // 单点：直接 upsertDoc
case model.SourceUpload:
    // 多 chunk：ingestUpload 重新分片
    return u.ingestUpload(ctx, model.Doc{
        SourceType: model.SourceUpload,
        URL:        existing.URL,  // url 是身份，不可改
        // ... 其他字段从 input 取 ...
        CreatedAt:  existing.CreatedAt,  // 保留创建时间
        UpdatedAt:  time.Now().UTC(),
    })
default:
    return nil, fmt.Errorf("%w: %s docs are read-only — re-sync the source to refresh", errs.ErrInvalid, existing.SourceType)
}
```

vault / repo doc 拒绝编辑——它们由 sync 路径重新生成。

### 7.5 DeleteDoc（usecase.go L647-L667）

```go
switch d.SourceType {
case model.SourceManual:
    return u.vec.DeleteByID(ctx, CollectionName, id)
case model.SourceUpload:
    // 多 chunk：按 url 过滤批量删
    return u.vec.DeleteByFilter(ctx, CollectionName, map[string]any{
        "source_type": model.SourceUpload,
        "url":         d.URL,
    })
default:
    return fmt.Errorf("%w: %s docs can't be deleted individually — unregister the source", errs.ErrInvalid, d.SourceType)
}
```

### 7.6 GetDoc（usecase.go L591-L602）

```go
func (u *Usecase) GetDoc(ctx context.Context, id uint64) (*model.Doc, error) {
    pts, err := u.vec.GetPoints(ctx, CollectionName, []uint64{id})
    // ...
    for _, p := range pts {
        if p.ID == id { return payloadToDoc(p.ID, p.Payload), nil }
    }
    return nil, errs.ErrNotFound
}
```

**用 `GetPoints` 而非 `Search`**：doc ID 是 md5 派生的 uint64，可能 >2^63，qdrant filter 解析器拒绝；point-id API 原生支持。

---

## 8. ID 派生：稳定哈希与 chunk 索引

源文件：[usecase.go](file:///d:/claude/ongrid/internal/manager/biz/knowledge/usecase.go) L1868-L1955

### 8.1 docID 基础函数（L1952-L1955）

```go
func docID(key string) uint64 {
    sum := md5.Sum([]byte(key))
    return binary.BigEndian.Uint64(sum[:8])
}
```

md5 → 取高 8 字节 → uint64。注释（L1868-L1871）：

> Probability of collision at ~10^9 docs is still ~10^-3 — acceptable for our scale.

### 8.2 四种 scope 前缀

```go
func manualDocID(title string) uint64 {
    return docID("manual||" + title)
}
func repoDocID(repoID uint64, url string) uint64 {
    return docID(fmt.Sprintf("repo||%d||%s", repoID, url))
}
func repoChunkDocID(repoID uint64, url string, chunkIndex int) uint64 {
    if chunkIndex == 0 { return repoDocID(repoID, url) }  // chunk 0 = 原 ID
    return docID(fmt.Sprintf("repo||%d||%s||chunk-%d", repoID, url, chunkIndex))
}
func vaultChunkDocID(url string, chunkIndex int) uint64 {
    if chunkIndex == 0 { return docID("vault||" + url) }
    return docID(fmt.Sprintf("vault||%s||chunk-%d", url, chunkIndex))
}
func uploadChunkDocID(url string, chunkIndex int) uint64 {
    if chunkIndex == 0 { return docID("upload||" + url) }
    return docID(fmt.Sprintf("upload||%s||chunk-%d", url, chunkIndex))
}
```

**chunk 0 保持原 ID** 的设计意图（L1880-L1883 注释）：

> Chunk 0 keeps the original `repoDocID(repoID, url)` so existing point IDs (and any operator-saved deep links) survive the move to chunking; higher chunk indices get a derived key.

### 8.3 id_alias 字段：逻辑 doc ID

每个 chunk 的 payload 都带 `id_alias`，指向 head chunk 的 ID：

```go
// repoChunkPoint (L1999-L2038)
parentID := repoChunkDocID(repoID, url, 0)
pt.Payload["id_alias"] = parentID
```

这让 ListDocs 能把同一 doc 的多个 chunk 折叠成一行（见 §13）。

### 8.4 回归测试（chunk_test.go L82-L99）

```go
func TestRepoChunkDocID(t *testing.T) {
    if repoChunkDocID(repoID, url, 0) != repoDocID(repoID, url) {
        t.Fatalf("chunk 0 id diverged from repoDocID")
    }
    ids := map[uint64]int{
        repoChunkDocID(repoID, url, 0): 0,
        repoChunkDocID(repoID, url, 1): 1,
        repoChunkDocID(repoID, url, 2): 2,
        repoChunkDocID(repoID, url, 5): 5,
    }
    if len(ids) != 4 { t.Fatalf("expected 4 distinct chunk ids, got %d (collisions)", len(ids)) }
}
```

---

## 9. Chunk 分片：splitForChunks 与 overlap

源文件：[usecase.go](file:///d:/claude/ongrid/internal/manager/biz/knowledge/usecase.go) L1660-L1682, L1957-L1984

### 9.1 常量（L1660-L1682）

```go
const (
    maxFileBytes     = 2 * 1024 * 1024  // 2 MB 单文件上限
    maxFiles         = 2000             // 单 repo 最多 2000 文件
    chunkChars       = 2500             // 单 chunk 字符数
    chunkOverlap     = 250              // chunk 间重叠
    maxChunksPerFile = 256              // 单文件最多 256 chunk
)
```

`maxChunksPerFile` 注释（L1677-L1681）：

> prevents one pathologically large file from monopolising the embed budget. With chunkChars=2500 and overlap=250, stride is 2250 chars/chunk → 256 chunks ≈ 560 KB of content; the largest sane reference (RFC 9110 = 492 KB) fits inside.

### 9.2 splitForChunks（L1962-L1984）

```go
func splitForChunks(content string) []string {
    runes := []rune(content)
    n := len(runes)
    if n <= chunkChars { return []string{content} }
    stride := chunkChars - chunkOverlap
    chunks := make([]string, 0, (n/stride)+1)
    for start := 0; start < n; start += stride {
        end := start + chunkChars
        if end > n { end = n }
        chunks = append(chunks, string(runes[start:end]))
        if end == n { break }
        if len(chunks) >= maxChunksPerFile { break }
    }
    return chunks
}
```

**按 rune 计数**（非 byte），CJK 正确处理。stride = chunkChars - chunkOverlap = 2250。

### 9.3 chunk 0 前置 title（usecase.go L1019-L1031, L1153-L1160）

```go
for j, p := range parts {
    var body string
    if j == 0 {
        body = files[i].Title + "\n\n" + p  // chunk 0 带 title
    } else {
        body = p  // 后续 chunk 仅内容
    }
    // ...
}
```

理由（L1022-L1025）：

> Chunk 0 prepends the title so the embedding picks up the "what is this doc" signal — same as the pre-chunking behaviour for short docs. Chunks beyond 0 carry only their slice (the title would dominate the vector otherwise).

### 9.4 chunk 0 完整正文（L2010-L2015, L1927-L1929）

```go
contentForPayload := chunkContent
if chunkIndex == 0 {
    // Chunk 0 carries the full body so GET /knowledge/docs/<id> on
    // the parent ID returns the complete document.
    contentForPayload = d.Content
}
```

这让 `GET /knowledge/docs/<parent_id>` 直接返回完整 doc（无需聚合所有 chunk）。

### 9.5 embeddingMaxChars 截断（L1306-L1337）

```go
const embeddingMaxChars = 2500

func truncateForEmbedding(s string) string {
    count := 0
    for i := range s {
        if count >= embeddingMaxChars { return s[:i] }
        count++
    }
    return s
}
```

注释（L1306-L1322）解释 2500 的来源：

> Zhipu's embedding-3 enforces 3072 tokens / single input. For CJK content the BPE tokenizer can output ≥1 token per char (sometimes 1.2 for less-common glyphs), so a "1.5 char/token" assumption breaks on dense Chinese pages. Empirically 2500 chars covers ~95% of our seed corpus without the model 1210 rejecting.

### 9.6 回归测试（chunk_test.go）

| 测试 | 行 | 覆盖点 |
|------|----|----|
| `TestSplitForChunks_ShortDoc` | L12-L21 | 短 doc 单 chunk |
| `TestSplitForChunks_LongDoc` | L27-L49 | 长 doc 多 chunk + overlap 验证 |
| `TestSplitForChunks_CJK` | L54-L65 | CJK rune 计数（5000 中文字 → ≥2 chunk） |
| `TestSplitForChunks_MaxChunkCap` | L71-L77 | 100× chunkChars 不超 maxChunksPerFile |

---

## 10. Repo Sync：git 拉取 → 扫描 → 嵌入 → upsert

源文件：[usecase.go](file:///d:/claude/ongrid/internal/manager/biz/knowledge/usecase.go) L905-L1094

### 10.1 Sync 主流程（L905-L1094）

```
1. 获取 repo row
2. mkdir cloneDir parent
3. 区分 builtin vault vs git repo
   - IsBuiltinVaultURL(repo.URL) → materializeBuiltinVault
   - else → buildGitAuthEnv + syncFastPath 或 syncAtomicReplace
4. scanRepoFiles(dir) → []scannedFile
5. DeleteByFilter(source_type=repo, repo_id=id) 清旧 point
6. 对每个 file: splitForChunks → chunkRef list
7. batch=32 嵌入 + upsert
8. UpdateRepoSync(id, file_count=len(files), "")
```

### 10.2 git 原子替换策略（L945-L975）

旧模型直接写入 `dir`，各种失败模式（half-clone / half-fetch / reset 中断）产生不同 corruption。新模型双路径：

- **Fast path**（健康 .git）：`fetch --depth=1 origin <branch>` + `reset --hard FETCH_HEAD` + `clean -fdx`
- **Slow path / repair**：`clone --depth=1` 到 sibling tmp dir → 成功后 `rm -rf <dir>` + `os.Rename(tmp, dir)`

`os.Rename` 在同一文件系统内原子，crash 后要么旧目录要么新目录，绝不 hybrid。

### 10.3 purgeStaleCloneTmps（L1518-L1530）

crash 在 clone-success 和 rename-completed 之间会留 `.tmp-clone-*` sibling，purgeStaleCloneTmps 在每次 sync 前清理。

### 10.4 git 重试与 transient 错误识别（L1546-L1609）

`runGitWithRetry` 最多 3 次（5s + 15s 退避），仅重试 `isTransientGitErr` 匹配的网络错误：

```go
patterns := []string{
    "ssl_read", "unexpected eof", "early eof", "signal: killed",
    "connection refused", "connection timed out", "connection reset",
    "could not resolve host", "name or service not known", "rpc failed",
    "the remote end hung up", "context deadline exceeded", "deadline exceeded",
    "waitdelay", "timed out", "i/o timeout", "operation timed out",
    "broken pipe", "timeout, server", "client_loop",
}
```

auth / 404 / bad-ref 不重试。

### 10.5 WaitDelay 防 hang（L1395-L1416）

```go
cmd.WaitDelay = 10 * time.Second
```

注释（L1400-L1405）：

> CRITICAL (mainland↔github stall fix): on ctx timeout CommandContext kills `git`, but git's child `ssh`/`git-remote-https` inherits the output pipe and keeps it open on a stalled network read — so CombinedOutput() would block on EOF long past the 60s deadline (we observed clone requests hanging >280s). WaitDelay force-closes the inherited pipes shortly after the kill so runGit actually returns.

### 10.6 scanRepoFiles（L1772-L1814）

`filepath.WalkDir` 遍历，跳过：

- 任何 `.` 开头的目录（`.github` / `.gitee` / `.vscode` 等）
- `skipDirNames` 列表（`_data` / `_posts` / `_drafts` / `_layouts` / `_includes` / `_sass` / `_site` / `_assets` / `vendor` / `node_modules` / `dist` / `build` / `target` / `__pycache__`）

只索引 `.md` / `.txt` / `.rst`（L1621-L1625）。注释（L1614-L1620）：

> Knowledge content is prose-shaped — markdown / reStructuredText / plain text — so we deliberately exclude .yaml / .yml / .toml / .json. Those are configuration / data formats that pollute RAG with non-prose noise.

单文件 ≤2MB，单 repo ≤2000 文件。

### 10.7 extractDocTitle（L1703-L1755）

三级 fallback：

1. YAML front-matter `title:`
2. 第一个 `# H1`
3. 文件名（去扩展名 + 去 `<8-hex>-` 前缀）

### 10.8 DeleteRepo（L868-L900）

```go
// 1. 快照 URL（用于 forensic log + onRepoDelete hook）
// 2. DeleteByFilter(source_type=repo, repo_id=id) 清 qdrant point
// 3. repo.DeleteRepo(id) 删 MySQL row
// 4. os.RemoveAll(repoDir) 删 on-disk clone
// 5. onRepoDelete(ctx, deletedURL) 回调
```

forensic log 注释（L862-L867）：

> operators have complained about silent knowledge loss — without this we can't trace which URL the audit-trail repo_id once referred to

---

## 11. Built-in Vault：embed.FS + cloud 优先 + embedded fallback

源文件：[builtin_vault.go](file:///d:/claude/ongrid/internal/manager/biz/knowledge/builtin_vault.go)

### 11.1 embed.FS（L28-L29）

```go
//go:embed all:builtin_vault
var builtinVaultFS embed.FS
```

平台 vendor 知识 vault 直接嵌入二进制。

### 11.2 BuiltinVaultURL sentinel（L37）

```go
const BuiltinVaultURL = "builtin://vault"
```

不是真实 git remote，标记 legacy repo row 让 `PurgeBuiltinVaultRepo` 能迁移。

### 11.3 BuiltinVaultGitURL（L44-L47）

```go
const (
    BuiltinVaultGitURL = "https://github.com/ongridio/vault.git"
    BuiltinVaultBranch = "main"
)
```

ADR-029：cloud 优先（live github clone），embedded 是 offline fallback。URL **不可由 operator 配置**——vault 永远来自这一个 repo，clone 永不变成 Repos-list row。

### 11.4 materializeBuiltinVault 原子替换（L59-L77）

```go
func (u *Usecase) materializeBuiltinVault(dir string) error {
    tmp, err := newCloneTmpDir(dir)  // sibling tmp
    if err := writeEmbeddedVault(tmp); err != nil { ... }
    if err := os.RemoveAll(dir); err != nil { ... }
    if err := os.Rename(tmp, dir); err != nil { ... }
    return nil
}
```

与 `syncAtomicReplace` 同样的"原子 temp + rename swap"契约——crash 后要么旧 snapshot 要么新 snapshot，绝不 hybrid。

### 11.5 writeEmbeddedVault（L84-L108）

`fs.WalkDir(builtinVaultFS, builtinVaultRoot, ...)`，把 `builtin_vault/concepts/x.md` 落到 `<destRoot>/concepts/x.md`（剥离 `builtin_vault` 前缀，匹配 git clone 期望的目录结构）。

### 11.6 SyncBuiltinVault（usecase.go L1110-L1201）

```
1. mkdir cloneDir/builtin-vault parent
2. fetchCloudVault(dir) — cloud 优先
   - 成功 → source = "cloud"
   - 失败 → materializeBuiltinVault(dir) — embedded fallback
     - 成功 → source = "embedded"
     - 失败 → return error
3. scanRepoFiles(dir)
4. DeleteByFilter(source_type=vault) 清旧 vault point
5. chunk + embed + upsert（batch=32）
6. log Info "knowledge: built-in vault synced"
7. return (fileCount, source, nil)
```

vault **不是** `knowledge_repos` row——它是平台内容，直接 sync 到 qdrant，从不进 Repos list。

### 11.7 fetchCloudVault 重试（usecase.go L1221-L1259）

```go
const (
    cloudVaultAttempts = 3
    cloudVaultPerTry   = 30 * time.Second
)
```

mainland↔github 间歇性失败：同一分钟内一次 2s 成功一次失败。3 次重试 + 2s 退避，捕获好窗口。最坏情况全部失败 → fallback 到 embedded 38-file baseline。

### 11.8 HasVaultDocs（usecase.go L1263-L1269）

```go
func (u *Usecase) HasVaultDocs(ctx context.Context) bool {
    res, err := u.vec.Scroll(ctx, CollectionName, qdrantx.ScrollOpts{
        MustMatch: map[string]any{"source_type": model.SourceVault},
        Limit:     1,
    })
    return err == nil && res != nil && len(res.Points) > 0
}
```

让 boot 跳过冗余 re-embed（main.go L1319）。

### 11.9 PurgeBuiltinVaultRepo 迁移（usecase.go L1278-L1302）

```go
// Pre-refactor installs seeded the vault AS a repo row; after the move to
// source_type=vault that row would linger in the Repos list. This migration
// runs at boot. It calls the store directly (not DeleteRepo) so the
// vault_seed_optout delete-hook does NOT fire — purging the legacy row is a
// migration, not an operator opting out.
```

boot 时遍历 repos，对 `IsBuiltinVaultURL(r.URL)` 的 row：删 qdrant point（source_type=repo, repo_id=r.ID）+ 删 MySQL row + 删 on-disk dir。

### 11.10 测试覆盖（builtin_vault_test.go）

| 测试 | 行 | 覆盖 |
|------|----|----|
| `TestBuiltinVault_Embedded` | L32-L36 | embed.FS 非空 |
| `TestIsBuiltinVaultURL` | L38-L51 | sentinel 匹配（含大小写 / 空格） |
| `TestMaterializeBuiltinVault` | L57-L92 | 物化 + scanRepoFiles 一致性 + 前缀剥离 |
| `TestMaterializeBuiltinVault_Idempotent` | L97-L117 | 二次物化不报"destination exists" + 无 stale tmp |

---

## 12. Search：embedding → 服务端过滤 → 去重

源文件：[usecase.go](file:///d:/claude/ongrid/internal/manager/biz/knowledge/usecase.go) L732-L802

### 12.1 Search 主流程

```go
func (u *Usecase) Search(ctx context.Context, q string, opts SearchOptions) ([]SearchHit, error) {
    if u.embed == nil { return nil, errs.ErrNotWiredYet }
    q = strings.TrimSpace(q)
    if q == "" { return nil, nil }
    limit := opts.Limit
    if limit <= 0 { limit = 10 }
    vecs, err := u.embed.Embed(ctx, []string{q})
    // ...
    must := map[string]any{}
    if p := normalizePath(opts.Path); p != "" {
        must["path"] = p
    } else if p := normalizePath(opts.PathPrefix); p != "" {
        must["path_prefixes"] = p
    }
    if tags := normalizeTags(opts.Tags); len(tags) > 0 {
        must["tags"] = tags  // []string → match.any in qdrantx.buildFilter
    }
    overFetch := limit * 5
    if overFetch > 200 { overFetch = 200 }
    hits, err := u.vec.Search(ctx, CollectionName, vecs[0], qdrantx.SearchOpts{
        Limit:     overFetch,
        MustMatch: must,
    })
    // ... 去重 ...
}
```

### 12.2 Over-fetch 策略（L760-L766）

```go
// Over-fetch so that the dedup-by-parent step still returns `limit`
// unique docs even when a long doc contributes multiple chunks at
// the top of the result list. Cap at 5x to bound the upper end.
overFetch := limit * 5
if overFetch > 200 { overFetch = 200 }
```

长 doc 的多个 chunk 可能都排在 top hits，去重后实际 doc 数 < limit，所以 over-fetch 5x 补偿。

### 12.3 按 parent_url 去重（L774-L800）

```go
out := make([]SearchHit, 0, limit)
seen := make(map[string]bool, limit)
for _, h := range hits {
    key := ""
    if v, ok := h.Payload["parent_url"].(string); ok && v != "" {
        key = v
    } else if v, ok := h.Payload["url"].(string); ok {
        key = v
    } else {
        key = fmt.Sprintf("id:%d", h.ID)
    }
    if seen[key] { continue }
    seen[key] = true
    out = append(out, SearchHit{
        Doc:   payloadToDoc(h.ID, h.Payload),
        Score: h.Score,
    })
    if len(out) >= limit { break }
}
```

manual doc（无 `parent_url`）和单 chunk repo doc（`parent_url == url`）都干净通过。

---

## 13. ListDocs / ListPaths：Scroll + dedupeByIDAlias

源文件：[usecase.go](file:///d:/claude/ongrid/internal/manager/biz/knowledge/usecase.go) L448-L723

### 13.1 ListDocs（L463-L501）

```go
func (u *Usecase) ListDocs(ctx context.Context, f ListDocsFilter) ([]*model.Doc, error) {
    must := map[string]any{}
    if f.SourceType != "" { must["source_type"] = f.SourceType }
    if f.RepoID != nil { must["repo_id"] = *f.RepoID }
    if f.Path != "" {
        must["path"] = f.Path
    } else if f.PathPrefix != "" {
        must["path_prefixes"] = normalizePath(f.PathPrefix)
    }
    if f.Tag != "" { must["tags"] = f.Tag }
    limit := f.Limit
    if limit <= 0 { limit = 200 }
    scanLimit := limit * 8  // 补偿非 head chunk
    if scanLimit > 10000 { scanLimit = 10000 }
    res, err := u.vec.Scroll(ctx, CollectionName, qdrantx.ScrollOpts{
        MustMatch: must,
        Limit:     scanLimit,
    })
    // ...
    return dedupeByIDAlias(res.Points, limit), nil
}
```

### 13.2 历史 wart（L454-L462 注释）

> Historical wart: we used to MustMatch chunk_index=0 at the qdrant layer, but the manual-doc upsert path forgot to set chunk_index in payload (fixed now). Every manual doc inserted before the fix is invisible to that strict filter, even though the vector is fine and RAG search returns it. Operators interpreted this as "RAG 又没了".

新行为：scroll 不带 `chunk_index` filter，在 Go 端 `dedupeByIDAlias` 去重，优先 head chunk。

### 13.3 dedupeByIDAlias（L507-L542）

```go
func dedupeByIDAlias(points []qdrantx.SearchHit, limit int) []*model.Doc {
    type slot struct {
        idx        int
        isHead     bool
        isExplicit bool
    }
    out := make([]*model.Doc, 0, len(points))
    seen := make(map[uint64]*slot, len(points))
    for _, p := range points {
        alias := docIDAlias(p)
        ci, ciPresent := chunkIndexFromPayload(p.Payload)
        isHead := !ciPresent || ci == 0
        if s, ok := seen[alias]; ok {
            // Upgrade if the new point is a head chunk and the existing one wasn't.
            if isHead && !s.isHead {
                out[s.idx] = payloadToDoc(p.ID, p.Payload)
                s.isHead = true
                s.isExplicit = ciPresent
            }
            continue
        }
        out = append(out, payloadToDoc(p.ID, p.Payload))
        seen[alias] = &slot{idx: len(out) - 1, isHead: isHead, isExplicit: ciPresent}
        // ...
    }
    if limit > 0 && len(out) > limit { out = out[:limit] }
    return out
}
```

**关键**：scroll 顺序不保证，head chunk 可能在非 head chunk 之后到达。`isHead && !s.isHead` 时 upgrade 已有 slot。

### 13.4 docIDAlias（L547-L563）

```go
func docIDAlias(p qdrantx.SearchHit) uint64 {
    if v, ok := p.Payload["id_alias"]; ok {
        switch x := v.(type) {
        case float64:  return uint64(x)
        case int64:    return uint64(x)
        case uint64:   return x
        case json.Number:
            if n, err := x.Int64(); err == nil { return uint64(n) }
        }
    }
    return p.ID  // fallback to point id
}
```

JSON 解码默认 `float64`，所以 `case float64` 是最常见的路径。

### 13.5 ListPaths（L697-L723）

```go
func (u *Usecase) ListPaths(ctx context.Context) (map[string]int, error) {
    res, err := u.vec.Scroll(ctx, CollectionName, qdrantx.ScrollOpts{
        Limit: 10000,
    })
    // ...
    docs := dedupeByIDAlias(res.Points, 0)
    out := make(map[string]int)
    for _, d := range docs {
        if v := d.Path; v != "" {
            out[v]++
            continue
        }
        // Repo doc fallback — derive a folder from URL.
        if d.URL != "" {
            dir := filepath.Dir(d.URL)
            if dir != "" && dir != "." && dir != "/" { out[dir]++ }
        }
    }
    return out, nil
}
```

repo doc 没显式 `path`，从 URL 目录部分派生（`reference/external/dns/foo.md` → `reference/external/dns`）。

### 13.6 回归测试（dedupe_test.go）

| 测试 | 行 | 覆盖 |
|------|----|----|
| `TestDedupeByIDAlias_PrechunkingManualDoc` | L14-L32 | 旧 manual doc（无 chunk_index）不丢 |
| `TestDedupeByIDAlias_ChunkedRepoDoc` | L36-L67 | 多 chunk 折叠成 1 行 |
| `TestDedupeByIDAlias_HeadUpgrade` | L72-L98 | scroll 乱序时 head upgrade |
| `TestDedupeByIDAlias_MixedManualAndRepo` | L102-L113 | 混合 source 正确折叠 |
| `TestDedupeByIDAlias_LimitCap` | L117-L127 | limit 截断 |
| `TestChunkIndexFromPayload` | L132-L154 | 多种 JSON number flavor |

---

## 14. Embedder：OpenAI 兼容 + 本地 ONNX 双实现

### 14.1 Embedder 接口（embedding.go L40-L48）

```go
type Embedder interface {
    Dim() int
    Embed(ctx context.Context, texts []string) ([][]float32, error)
}
```

### 14.2 New 分发（embedding.go L72-L85）

```go
func New(cfg Config) (Embedder, error) {
    provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
    if provider == "" { provider = "openai" }
    switch provider {
    case "openai":         return newOpenAI(cfg)
    case "local", "fastembed", "onnx": return newLocal(cfg)
    default:               return nil, fmt.Errorf("embedding: unknown provider %q", cfg.Provider)
    }
}
```

### 14.3 OpenAI 实现（embedding.go L93-L214）

- 默认 base `https://api.openai.com`，默认 model `text-embedding-3-small`，默认 dim 1536
- 30s HTTP 超时
- `embedURL` 智能拼接（L221-L227）：base 已有 `/v<digit>` 后缀（如 GLM 的 `/api/paas/v4`）直接加 `/embeddings`，否则加 `/v1/embeddings`
- **Zhipu JWT 自动签名**（L173-L181）：检测 zhipu URL + key shape，自动用 `zhipuauth.SignJWT` 签 JWT（TTL=1h），其他 OpenAI 兼容端点用 raw Bearer
- 严格校验返回向量数 == 输入数 + 维度匹配（L199-L210）

### 14.4 本地 ONNX 实现（local.go）

#### 14.4.1 设计动机（L1-L17）

> ADR-027 — air-gapped / regulated installs can't hit OpenAI/GLM/Qwen and previously had RAG entirely disabled.

#### 14.4.2 模型选择（L50-L76）

`modelDims` 映射 fastembed-go 常量到维度：

```go
var modelDims = map[fastembed.EmbeddingModel]int{
    fastembed.AllMiniLML6V2: 384,
    fastembed.BGEBaseEN:     768,
    fastembed.BGEBaseENV15:  768,
    fastembed.BGESmallEN:    384,
    fastembed.BGESmallENV15: 384,
    fastembed.BGESmallZH:    512,  // 默认
}
```

默认 `bge-small-zh-v1.5`——中英混合质量最佳 @ ~30MB quantized ONNX，dim=512 适配 qdrant HNSW 默认值。

#### 14.4.3 串行化（L38-L44, L161-L166）

```go
type localEmbedder struct {
    model *fastembed.FlagEmbedding
    dim   int
    name  string
    mu    sync.Mutex  // ONNX 推理非线程安全
    log   *slog.Logger
}

func (e *localEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
    // ...
    e.mu.Lock()
    defer e.mu.Unlock()
    // ...
}
```

注释（L34-L37）：

> ONNX inference is CPU-bound and not thread-safe in fastembed's wrapper; we serialize Embed calls with a mutex so concurrent /knowledge/search + Sync upserts don't crash the underlying tokenizer. Throughput on a single CPU core is the bottleneck regardless — the mutex doesn't make it slower.

#### 14.4.4 maxLocalEmbedChars 截断（L194-L230）

```go
const maxLocalEmbedChars = 350

func clampForLocalEmbed(s string) string {
    s = strings.TrimSpace(s)
    if s == "" { return " " }  // 空输入会 crash tokenizer
    r := []rune(s)
    if len(r) > maxLocalEmbedChars { return string(r[:maxLocalEmbedChars]) }
    return s
}
```

**Lib bug**（L196-L215 注释）：

> sugarme/tokenizer @ v0.2.3 (the version fastembed-go v1.0.0 pins to) has an unchecked pairEncoding.GetIds() dereference in TruncateEncodings' LongestFirst switch case (util.go line 108). When pairEncoding is nil (which is normal for single-text embedding) AND totalLength >= MaxLength (which fastembed pins at 512 tokens), the truncation path runs the switch and SIGSEGVs.

350 chars 覆盖中英：350 zh char ≈ 350 token；350 en char ≈ 70 token，都 < 512。

#### 14.4.5 错误提示（L122-L135）

```go
hint := ""
switch {
case strings.Contains(err.Error(), "onnxruntime"), strings.Contains(err.Error(), "ONNX_PATH"):
    hint = " — install libonnxruntime; on debian: apt install -y libonnxruntime-dev"
case strings.Contains(err.Error(), "download"), strings.Contains(err.Error(), "no such host"):
    hint = " — pre-download the model into " + cacheDir + " (see fetch-embedding-model.sh)"
}
return nil, fmt.Errorf("embedding: init local model %s: %w%s", model, err, hint)
```

---

## 15. aiops tools：query_knowledge Caller 接口

源文件：[query_knowledge_basetool.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/query_knowledge_basetool.go)

### 15.1 KnowledgeSearcher 窄接口（L69-L73）

```go
type KnowledgeSearcher interface {
    Search(ctx context.Context, q string, opts knowledgebiz.SearchOptions) ([]knowledgebiz.SearchHit, error)
}
```

`*knowledge.Usecase` 满足此接口。测试可注入 fake。

### 15.2 queryKnowledgeWhenToUse（L26-L36）

LLM 看到的工具描述（关键部分）：

> **回答任何运维 / 故障排查 / 部署 / 配置 / 网络 / 系统类问题前都先调一次本工具**——KB 是团队精选的中文 playbook（DNS / conntrack / MTU / eBPF / TLS / netshoot / netns 等），比通用知识更贴近本系统的命令偏好和处置惯例。
>
> 命中（top score ≥ 0.6）就基于 playbook 步骤回答；未命中再走通用诊断或实时数据工具。

### 15.3 InvokableRun（L127-L180）

```go
func (t *QueryKnowledgeTool) InvokableRun(ctx context.Context, argsJSON string, _ ...basetool.InvokeOption) (string, error) {
    if t.svc == nil { return "", fmt.Errorf("%s: knowledge service not configured", ToolNameQueryKnowledge) }
    var args queryKnowledgeArgs
    if err := json.Unmarshal([]byte(argsJSON), &args); err != nil { ... }
    args.Query = strings.TrimSpace(args.Query)
    if args.Query == "" { return "", fmt.Errorf("%s: query required", ToolNameQueryKnowledge) }
    if args.MaxResults <= 0 { args.MaxResults = 5 }
    if args.MaxResults > 20 { args.MaxResults = 20 }
    hits, err := t.svc.Search(ctx, args.Query, knowledgebiz.SearchOptions{
        Path:       args.Path,
        PathPrefix: args.PathPrefix,
        Tags:       args.Tags,
        Limit:      args.MaxResults,
    })
    // ...
    for _, h := range hits {
        preview := h.Doc.Content
        if len(preview) > 800 {
            preview = preview[:800] + "…"
            out.Truncated = true
        }
        // ...
    }
    // ...
}
```

**preview 截断 800 chars**（L156-L162）：让 `max_results=5` 的回复 ≤ ~4k tokens。

### 15.4 装配（main.go L1286）

```go
toolsReg.SetKnowledgeSearcher(knowledgeUC)
```

在 `buildAIOpsRuntime` **之前**调用（main.go L1225-L1227 注释）：

> Wire BEFORE buildAIOpsRuntime so the BaseTool bag picks up query_knowledge — SetKnowledgeSearcher only affects subsequent BuildBaseTools calls.

### 15.5 工具分类（main.go L4858-L4881）

```go
case "query_knowledge", "list_repo_sources", "read_source", "grep_source":
    return "knowledge"
// ...
case strings.Contains(name, "source") || strings.Contains(name, "knowledge"):
    return "knowledge"
```

工具元数据（main.go L4922-L4923, L4966）：

```go
"query_knowledge":   "知识库检索",
"query_knowledge":   "在 playbook + 代码仓里做语义检索。",
```

---

## 16. HTTP 路由：knowledge.Handler

源文件：[internal/manager/server/knowledge/http.go](file:///d:/claude/ongrid/internal/manager/server/knowledge/http.go)

### 16.1 路由注册（L110-L137）

```go
func (h *Handler) Register(r chi.Router) {
    r.Get("/v1/knowledge/docs", h.listDocs)
    r.Get("/v1/knowledge/docs/{id}", h.getDoc)
    r.With(h.writeMW("knowledge:doc")).Post("/v1/knowledge/docs", h.createDoc)
    r.With(h.writeMW("knowledge:doc")).Patch("/v1/knowledge/docs/{id}", h.updateDoc)
    r.With(h.writeMW("knowledge:doc")).Patch("/v1/knowledge/docs/{id}/move", h.moveDoc)
    r.With(h.deleteMW("knowledge:doc")).Delete("/v1/knowledge/docs/{id}", h.deleteDoc)
    r.With(h.writeMW("knowledge:doc")).Post("/v1/knowledge/upload", h.uploadDoc)
    r.Get("/v1/knowledge/search", h.search)
    r.Get("/v1/knowledge/paths", h.listPaths)
    r.Get("/v1/knowledge/repos", h.listRepos)
    r.With(h.writeMW("knowledge:repo")).Post("/v1/knowledge/repos", h.createRepo)
    r.With(h.writeMW("knowledge:repo")).Post("/v1/knowledge/repos/{id}/sync", h.syncRepo)
    r.With(h.deleteMW("knowledge:repo")).Delete("/v1/knowledge/repos/{id}", h.deleteRepo)
    r.With(h.writeMW("knowledge:repo")).Post("/v1/knowledge/vault/sync", h.syncVault)
    r.Get("/v1/knowledge/ssh-identities", h.listSSHIdentities)
    r.With(h.writeMW("knowledge:repo")).Post("/v1/knowledge/ssh-identities", h.createSSHIdentity)
    r.With(h.writeMW("knowledge:repo")).Post("/v1/knowledge/ssh-identities/generate", h.generateSSHIdentity)
    r.With(h.writeMW("knowledge:repo")).Patch("/v1/knowledge/ssh-identities/{id}", h.updateSSHIdentity)
    r.With(h.deleteMW("knowledge:repo")).Delete("/v1/knowledge/ssh-identities/{id}", h.deleteSSHIdentity)
}
```

### 16.2 docDTO uint64 string 序列化（L146-L158）

```go
// docDTO uses `,string` for ID + RepoID because qdrant point IDs are
// md5-derived uint64 values larger than JS Number.MAX_SAFE_INTEGER
// (2^53). Without string serialization, browsers parse 4.5e17 IDs as
// IEEE-754 doubles, lose precision in the low digits, and subsequent
// GET /docs/{rounded-id} 404s because the real id no longer matches.
type docDTO struct {
    ID         uint64    `json:"id,string"`
    SourceType string    `json:"source_type"`
    RepoID     *uint64   `json:"repo_id,omitempty,string"`
    // ...
}
```

### 16.3 uploadDoc multipart（L296-L353）

8 MiB 上限（`maxUploadBytes = 8 << 20`），支持 `.md` / `.txt` / `.pdf` / `.docx`（pdf/docx 由 `docextract` 解析）。

### 16.4 错误映射（L602-L611）

```go
func writeErr(w http.ResponseWriter, err error) {
    switch {
    case errors.Is(err, errs.ErrNotFound):
        http.Error(w, err.Error(), http.StatusNotFound)
    case errors.Is(err, errs.ErrInvalid):
        http.Error(w, err.Error(), http.StatusBadRequest)
    default:
        http.Error(w, err.Error(), http.StatusInternalServerError)
    }
}
```

### 16.5 审计事件（L511-L518, L533-L540, L552-L558, L576-L581）

`createRepo` / `syncRepo` / `syncVault` / `deleteRepo` 都通过 `auditmw.SetAuditEvent` 记录审计事件（`ActionRepoCreate` / `ActionRepoSync` / `ActionRepoDelete`）。

### 16.6 装配条件（main.go L1748-L1753, L2342-L2343）

```go
var knowledgeHandler *managerserverknowledge.Handler
if knowledgeUC != nil {
    knowledgeHandler = managerserverknowledge.NewHandler(knowledgeUC)
    knowledgeHandler.SetAuthz(authzMW)
}
// ...
if knowledgeHandler != nil {
    knowledgeHandler.Register(protected)
}
```

embedder 不可用时 `knowledgeUC == nil`，路由不注册——避免 404 假象。

---

## 17. systemhealth：Qdrant + Embedding 探测

源文件：[internal/manager/service/systemhealth/service.go](file:///d:/claude/ongrid/internal/manager/service/systemhealth/service.go)

### 17.1 Config（L97-L112）

```go
type Config struct {
    // ...
    EmbeddingConfigured bool
    QdrantURL           string
    QdrantCollection    string
}
```

`New` 默认 `QdrantCollection = "ongrid_knowledge"`（L123-L124）。

### 17.2 checkQdrant（L238-L263）

```go
func (s *Service) checkQdrant(ctx context.Context) Check {
    return s.probe(ctx, "qdrant", "data", "Qdrant", func(ctx context.Context) (Status, string, map[string]any) {
        if strings.TrimSpace(s.cfg.QdrantURL) == "" {
            return StatusDegraded, "Qdrant URL is not configured", nil
        }
        u := strings.TrimRight(s.cfg.QdrantURL, "/") + "/collections/" + url.PathEscape(s.cfg.QdrantCollection)
        req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
        if err != nil { return StatusFailed, "Qdrant request build failed: " + err.Error(), nil }
        resp, err := s.deps.HTTP.Do(req)
        if err != nil { return StatusFailed, "Qdrant collection probe failed: " + err.Error(), nil }
        defer resp.Body.Close()
        if resp.StatusCode/100 == 2 {
            return StatusOK, "Qdrant collection is reachable", map[string]any{"collection": s.cfg.QdrantCollection}
        }
        raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
        msg := fmt.Sprintf("Qdrant collection probe returned HTTP %d", resp.StatusCode)
        if body := strings.TrimSpace(string(raw)); body != "" { msg += ": " + body }
        return StatusFailed, msg, map[string]any{"collection": s.cfg.QdrantCollection}
    })
}
```

**只探测 collection 可达性**——GET `/collections/{name}`，2xx = OK，其他 = Failed。错误体限 256 字节。

### 17.3 checkEmbedding（L379-L384）

```go
func (s *Service) checkEmbedding(ctx context.Context) Check {
    return s.probe(ctx, "embedding", "ai", "Embedding provider", func(context.Context) (Status, string, map[string]any) {
        if !s.cfg.EmbeddingConfigured {
            return StatusDegraded, "embedding provider is not configured", nil
        }
        return StatusOK, "embedding provider is configured", nil
    })
}
```

**只检查配置标志**——`EmbeddingConfigured` 由 main.go 在 `embedding.New()` 失败时设为 false（main.go L1724）。不实际探测连通性，避免健康检查触发嵌入 API 调用。

### 17.4 装配（main.go L1715-L1737）

```go
systemHealthSvc := managersvcsystemhealth.New(managersvcsystemhealth.Config{
    // ...
    EmbeddingConfigured: embErr == nil,
    QdrantURL:           qdrantURL,
    QdrantCollection:    managerbizknowledge.CollectionName,
}, managersvcsystemhealth.Dependencies{
    // ...
})
```

### 17.5 测试（service_test.go L59-L116）

`TestCheckAggregatesFailedDependency`：httptest 起 qdrant 桩，断言 path == `/collections/ongrid_knowledge`，2xx → StatusOK。

### 17.6 前端展示（Health.tsx L91, L96）

```typescript
qdrant: { zh: 'Qdrant 向量库', en: 'Qdrant vector DB' },
embedding: { zh: 'Embedding 模型', en: 'Embedding provider' },
```

`collection` 字段标签（L101）：

```typescript
collection: { zh: '集合', en: 'Collection' },
```

---

## 18. 配置与启动装配

### 18.1 环境变量（main.go L1236-L1261）

```go
embAPIKey := os.Getenv("ONGRID_EMBEDDING_API_KEY")
if embAPIKey == "" {
    embAPIKey = cfg.OpenAI.APIKey  // fallback 到 OPENAI_API_KEY
}
embBaseURL := os.Getenv("ONGRID_EMBEDDING_BASE_URL")
if embBaseURL == "" {
    embBaseURL = cfg.OpenAI.BaseURL
}
embDim := 1536
if v := os.Getenv("ONGRID_EMBEDDING_DIM"); v != "" {
    if n, err := strconv.Atoi(v); err == nil && n > 0 { embDim = n }
}
embedder, embErr := embedding.New(embedding.Config{
    Provider: os.Getenv("ONGRID_EMBEDDING_PROVIDER"),
    Model:    os.Getenv("ONGRID_EMBEDDING_MODEL"),
    BaseURL:  embBaseURL,
    APIKey:   embAPIKey,
    Dim:      embDim,
    Log:      log.With(slog.String("comp", "embedding")),
})
qdrantURL := os.Getenv("ONGRID_QDRANT_URL")
if qdrantURL == "" { qdrantURL = "http://qdrant:6333" }
```

### 18.2 环境变量总表

| 变量 | 默认 | 说明 |
|------|------|------|
| `ONGRID_QDRANT_URL` | `http://qdrant:6333` | qdrant HTTP 端点 |
| `ONGRID_EMBEDDING_PROVIDER` | `openai` | `openai` / `local` / `fastembed` / `onnx` |
| `ONGRID_EMBEDDING_MODEL` | `text-embedding-3-small`（openai）/ `bge-small-zh-v1.5`（local） | 模型 ID |
| `ONGRID_EMBEDDING_BASE_URL` | OpenAI 默认 / 空（local） | HTTP base |
| `ONGRID_EMBEDDING_API_KEY` | 空（local 不需要） | bearer token |
| `ONGRID_EMBEDDING_DIM` | 1536 / 512（local BGE-small-zh） | 维度（须与 collection 一致） |
| `ONGRID_EMBEDDING_CACHE_DIR` | `/var/lib/ongrid/embeddings` | local 模型缓存目录 |
| `ONGRID_KNOWLEDGE_REPO_DIR` | `/var/lib/ongrid/repos` | git clone 落盘根目录 |
| `ONGRID_BUILTIN_VAULT_SEED` | 空（启用） | `off` / `-` 跳过 boot vault seed |
| `ONNX_PATH` | `/usr/lib/libonnxruntime.so` | ONNX Runtime 共享库路径 |

### 18.3 knowledgeUC 构造（main.go L1262-L1332）

```go
var knowledgeUC *managerbizknowledge.Usecase
{
    qdrantClient := qdrantx.New(qdrantURL, log.With(slog.String("comp", "qdrant")))
    var maybeEmbedder embedding.Embedder
    if embErr != nil {
        log.Warn("knowledge: embedder unavailable — reads enabled, writes disabled", slog.Any("err", embErr))
    } else {
        maybeEmbedder = embedder
    }
    uc, kErr := managerbizknowledge.New(rootCtx, knowledgeRepo, qdrantClient, maybeEmbedder,
        os.Getenv("ONGRID_KNOWLEDGE_REPO_DIR"),
        log.With(slog.String("comp", "knowledge")))
    if kErr != nil {
        log.Warn("knowledge: usecase build failed", slog.Any("err", kErr))
    } else {
        knowledgeUC = uc
        toolsReg.SetKnowledgeSearcher(knowledgeUC)
        // ... vault seed ...
    }
}
```

### 18.4 Vault seed boot goroutine（main.go L1306-L1331）

```go
if seed := strings.TrimSpace(os.Getenv("ONGRID_BUILTIN_VAULT_SEED")); seed == "-" || strings.EqualFold(seed, "off") {
    log.Info("knowledge: built-in vault seed disabled via env")
} else {
    // 迁移 legacy vault repo row
    if purged, pErr := knowledgeUC.PurgeBuiltinVaultRepo(rootCtx); pErr != nil {
        log.Warn("knowledge: purge legacy vault repo", slog.Any("err", pErr))
    } else if purged {
        log.Info("knowledge: migrated built-in vault off the repos table")
    }
    go func() {
        syncCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
        defer cancel()
        if knowledgeUC.HasVaultDocs(syncCtx) {
            log.Info("knowledge: built-in vault already indexed — skipping boot sync")
            return
        }
        if n, src, sErr := knowledgeUC.SyncBuiltinVault(syncCtx); sErr != nil {
            log.Warn("knowledge: initial vault sync failed — operator can retry from UI", slog.Any("err", sErr))
        } else {
            log.Info("knowledge: initial vault sync ok", slog.Int("file_count", n), slog.String("source", src))
        }
    }()
}
```

**5 分钟超时** + **HasVaultDocs 短路**——已索引则跳过，避免 boot 重复嵌入。

---

## 19. 部署面：docker-compose + Dockerfile + .env

### 19.1 docker-compose.yml qdrant 服务（deploy/docker-compose.yml L308-L315）

```yaml
qdrant:
  image: docker.cnb.cool/ongridio/ongrid/qdrant:v1.11.3
  container_name: ongrid-qdrant
  restart: unless-stopped
  volumes:
    - qdrant_data:/qdrant/storage
  networks:
    - ongrid_net
```

### 19.2 install/docker-compose.yml 生产定义（L395-L413）

```yaml
# qdrant — vector store for the knowledge base / RAG. One container,
# HTTP API only (port 6333). Persistent volume keeps collections
# across restarts. Manager reaches it at http://qdrant:6333; no
# external port (knowledge-base writes / queries all go through the
# manager API).
qdrant:
  image: docker.cnb.cool/ongridio/ongrid/qdrant:v1.11.3
  container_name: ongrid-qdrant
  restart: unless-stopped
  volumes:
    - ${ONGRID_DATA_DIR:-/var/lib/ongrid}/qdrant:/qdrant/storage
  logging:
    driver: json-file
    options:
      max-size: 50m
      max-file: "5"
```

**无外部端口**——所有读写走 manager API。

### 19.3 manager 容器环境变量（install/docker-compose.yml L121-L139）

```yaml
# Knowledge base / RAG: qdrant vector store + embedding provider.
ONGRID_QDRANT_URL: ${ONGRID_QDRANT_URL:-http://qdrant:6333}
ONGRID_EMBEDDING_PROVIDER: ${ONGRID_EMBEDDING_PROVIDER:-openai}
ONGRID_EMBEDDING_MODEL: ${ONGRID_EMBEDDING_MODEL:-text-embedding-3-small}
ONGRID_EMBEDDING_DIM: ${ONGRID_EMBEDDING_DIM:-1536}
ONGRID_EMBEDDING_BASE_URL: ${ONGRID_EMBEDDING_BASE_URL:-}
ONGRID_EMBEDDING_API_KEY: ${ONGRID_EMBEDDING_API_KEY:-}
ONGRID_EMBEDDING_CACHE_DIR: ${ONGRID_EMBEDDING_CACHE_DIR:-/var/lib/ongrid/embeddings}
```

install 包默认 `openai`（生产更高质量），dev compose 默认 `local`（无外网即可用）。

### 19.4 dev docker-compose 默认（deploy/docker-compose.yml L91-L100）

```yaml
# Knowledge base / RAG: local BGE embedding + qdrant. This mirrors
# the install package default so the dev stack has KB search without
# an external embedding API key.
ONGRID_QDRANT_URL: ${ONGRID_QDRANT_URL:-http://qdrant:6333}
ONGRID_EMBEDDING_PROVIDER: ${ONGRID_EMBEDDING_PROVIDER:-local}
ONGRID_EMBEDDING_MODEL: ${ONGRID_EMBEDDING_MODEL:-bge-small-zh-v1.5}
ONGRID_EMBEDDING_DIM: ${ONGRID_EMBEDDING_DIM:-512}
ONGRID_EMBEDDING_API_KEY: ${ONGRID_EMBEDDING_API_KEY:-}
ONGRID_EMBEDDING_CACHE_DIR: ${ONGRID_EMBEDDING_CACHE_DIR:-/var/lib/ongrid/embeddings}
ONNX_PATH: /usr/lib/libonnxruntime.so
```

### 19.5 模型缓存 volume（install/docker-compose.yml L226-L231）

```yaml
# ADR-027 Phase-2: local ONNX embedding model cache. install.sh
# stages the bundled bge-small-zh-v1.5 here on first install;
# operator can also drop other fastembed models in by mkdir +
# cp + setting ONGRID_EMBEDDING_MODEL. Read-write because
# fastembed-go writes its own download-progress lock files.
- ${ONGRID_DATA_DIR:-/var/lib/ongrid}/embeddings:/var/lib/ongrid/embeddings
```

dev compose 用 `.cache/embedding-models`（L167）。

### 19.6 Dockerfile.ongrid ONNX Runtime 安装（L82-L113）

```dockerfile
# ONNX Runtime shared library. Pin the version so a future apt-mirror
# bump doesn't silently change ABI. v1.20.1 is the latest stable that
# fastembed-go's yalue/onnxruntime_go@v1.7.0 was tested against.
ARG ONNXRUNTIME_VERSION=1.20.1
ARG TARGETARCH
RUN set -eux; \
    apt-get update && apt-get install -y --no-install-recommends unzip; \
    arch="${TARGETARCH:-$(dpkg --print-architecture)}"; \
    case "${arch}" in \
        amd64|x86_64) ORT_ARCH=x64; NUGET_RID=linux-x64 ;; \
        arm64|aarch64) ORT_ARCH=aarch64; NUGET_RID=linux-arm64 ;; \
        *) echo "unsupported arch ${arch}" >&2; exit 1 ;; \
    esac; \
    if curl -fsSL -o /tmp/ort.tgz \
        "https://github.com/microsoft/onnxruntime/releases/download/v${ONNXRUNTIME_VERSION}/onnxruntime-linux-${ORT_ARCH}-${ONNXRUNTIME_VERSION}.tgz"; then \
        tar -xzf /tmp/ort.tgz -C /tmp; \
        install -m 0755 /tmp/onnxruntime-linux-${ORT_ARCH}-${ONNXRUNTIME_VERSION}/lib/libonnxruntime.so.${ONNXRUNTIME_VERSION} /usr/lib/; \
    else \
        curl -fsSL -o /tmp/ort.nupkg \
            "https://www.nuget.org/api/v2/package/Microsoft.ML.OnnxRuntime/${ONNXRUNTIME_VERSION}"; \
        unzip -q /tmp/ort.nupkg "runtimes/${NUGET_RID}/native/libonnxruntime.so" -d /tmp/ort-nuget; \
        install -m 0755 /tmp/ort-nuget/runtimes/${NUGET_RID}/native/libonnxruntime.so /usr/lib/libonnxruntime.so.${ONNXRUNTIME_VERSION}; \
    fi; \
    ln -sf /usr/lib/libonnxruntime.so.${ONNXRUNTIME_VERSION} /usr/lib/libonnxruntime.so; \
    ldconfig
ENV ONNX_PATH=/usr/lib/libonnxruntime.so
```

**双源 fallback**：先试 GitHub releases，失败则从 NuGet 拉 nupkg。支持 amd64 + arm64。

### 19.7 CGO 必需（Dockerfile.ongrid L8-L12）

```dockerfile
# We need CGO for the fastembed-go local embedder (yalue/onnxruntime_go
# is a thin cgo wrapper over libonnxruntime). Switched base from
# ...
# CGO_ENABLED=1 because of the local embedder (see top of stage 1).
```

### 19.8 fetch-embedding-model.sh（dist/fetch-embedding-model.sh）

```bash
MODEL=fast-bge-small-zh-v1.5
FASTEMBED_BASE=${ONGRID_FASTEMBED_BASE:-https://storage.googleapis.com/qdrant-fastembed}
TARGET="$DEST/$MODEL"

FILES=(model_optimized.onnx tokenizer_config.json special_tokens_map.json
       config.json tokenizer.json vocab.txt ort_config.json)
```

预缓存 BGE-small-zh-v1.5 到 `.cache/embedding-models/`，让 `dist/package.sh` 打包进 install tarball。注释（L7-L9）：

> The model is ~55MB download + ~97MB on disk after extraction, slow over CN networks; pinning a build step on network is brittle.

### 19.9 .env.example 配置说明（install/.env.example L90-L115）

```
#   local — in-process ONNX inference via fastembed-go. No network, no API key.
#     The model ships in the tarball and install.sh stages it under
#     /var/lib/ongrid/embeddings/ automatically. This is the default so a
#     fresh install can sync the built-in vault and run semantic search with
#     zero extra config.
#
#   openai — any OpenAI-compatible /v1/embeddings endpoint (OpenAI, GLM,
#     Qwen, DeepSeek). Higher quality / bigger dim, but needs a key + net.
#
# NB: ONGRID_EMBEDDING_DIM must match the active provider's output dim, or
# qdrant rejects upserts. Changing provider/dim on a populated collection
# requires a re-index (qdrantx auto drop+recreate kicks in only when empty).
```

**关键约束**：`ONGRID_EMBEDDING_DIM` 必须与 provider 输出维度匹配；切换 provider/dim 在已填充 collection 上需手动 re-index。

---

## 20. 前端 API：knowledge.ts

源文件：[web/src/api/knowledge.ts](file:///d:/claude/ongrid/web/src/api/knowledge.ts)

### 20.1 类型定义（L8-L46）

```typescript
export type DocSource = 'manual' | 'repo' | 'url' | 'vault' | 'upload';

export type KnowledgeDoc = {
  id: string;          // string 防 JS Number 精度丢失
  source_type: DocSource;
  repo_id?: string | null;
  url?: string;
  title: string;
  title_en?: string;
  content?: string;
  path?: string;
  tags?: string[];
  created_at: string;
  updated_at: string;
};
```

注释（L5-L7）：

> id is a string because qdrant point IDs are md5-derived uint64 values that overflow JS Number's 2^53 safe-integer range. Backend sends them as JSON strings (uint64 + `,string` tag) — see server/knowledge/http.go.

### 20.2 isBuiltinVault 单一真源（L48-L56）

```typescript
export function isBuiltinVault(repo: Pick<KnowledgeRepo, 'url' | 'is_builtin'>): boolean {
  if (repo.is_builtin) return true;
  const u = (repo.url ?? '').trim();
  return u.startsWith('builtin://') || u.includes('ongridio/vault');
}
```

注释（L49-L51）：

> Prefers the server's is_builtin flag; falls back to the builtin:// URL scheme (and the legacy ongridio/vault form) so it still works against an older manager that doesn't send the flag yet.

### 20.3 API 函数（L62-L192）

- `listDocs` / `getDoc` / `createDoc` / `updateDoc` / `deleteDoc` / `moveDoc`
- `uploadDoc`（multipart FormData，绕过 JSON request helper）
- `searchKnowledge` / `listPaths`
- `listRepos` / `createRepo` / `syncRepo` / `deleteRepo` / `syncVault`
- `listSSHIdentities` / `createSSHIdentity` / `generateSSHIdentity` / `updateSSHIdentity` / `deleteSSHIdentity`

### 20.4 i18n 本地化（L249-L403）

`KNOWLEDGE_PATH_SEGMENTS` 与 `KNOWLEDGE_TITLES` 两张表，让中文 vault 在 en-US locale 下显示英文：

```typescript
export function localizedDocTitle(input: KnowledgeDoc | string): string {
  if (typeof input === 'string') {
    const en = KNOWLEDGE_TITLES[input];
    return en ? trInline(input, en) : input;
  }
  if (input.title_en && input.title_en.trim() !== '') {
    return trInline(input.title, input.title_en);
  }
  const en = KNOWLEDGE_TITLES[input.title];
  return en ? trInline(input.title, en) : input.title;
}

export function localizedPath(path: string | null | undefined): string {
  if (!path) return path ?? '';
  return path.split('/').map(localizedPathSegment).join('/');
}
```

注释（L260-L263）说明未来 ongridio/vault auto-loader 替换后该表可退役：

> When the future `ongridio/vault` auto-loader replaces these seeds with English-source content, this table will become a thin no-op for those docs (unseen keys pass through) and can be retired.

---

## 21. 并发、错误与可观测性

### 21.1 qdrantx.Client 并发安全

- 字段全部只读（`base` / `hc` / `log`），安全用于并发调用
- `http.Client` 内部维护连接池，可被多 goroutine 共享
- 所有 IO 函数第一参 `context.Context`，可取消与超时
- 30s HTTP 超时为 `New` 默认值

### 21.2 localEmbedder 串行化

```go
type localEmbedder struct {
    // ...
    mu sync.Mutex
}

func (e *localEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
    e.mu.Lock()
    defer e.mu.Unlock()
    // ...
}
```

ONNX 推理非线程安全，mutex 串行化。注释明确：单核 CPU 是瓶颈，mutex 不让事情更慢。

### 21.3 错误体有界读取

所有 qdrant 非 2xx 路径走 `truncate(..., 256)`：

```go
raw, _ := io.ReadAll(resp.Body)
return fmt.Errorf("qdrant: ...: http %d: %s", resp.StatusCode, truncate(string(raw), 256))
```

防止 qdrant 错误页（可能含整页 HTML）膨胀日志。

### 21.4 错误传播链

- qdrantx：`fmt.Errorf("qdrant: ...: %w", err)` 包装
- knowledge biz：`fmt.Errorf("knowledge: ...: %w", err)` 二次包装
- HTTP handler：`writeErr` 映射 `errs.ErrNotFound` → 404，`errs.ErrInvalid` → 400，其他 → 500

### 21.5 错误注入路径

- `Embedder not configured`：所有写路径（CreateManualDoc / UploadDoc / UpdateManualDoc / MoveDoc / Sync / SyncBuiltinVault / Search）在 `u.embed == nil` 时返回 `errs.ErrNotWiredYet`
- 读路径（ListDocs / GetDoc / ListPaths / ListRepos）**不** gate embedder，让 fresh install 的 SPA 列表能渲染

### 21.6 可观测性

- `slog` 结构化日志，组件标签 `"comp"="qdrant"` / `"comp"="embedding"` / `"comp"="knowledge"`
- `systemhealth` 暴露 qdrant + embedding 状态给 SPA Health 页
- `auditmw.SetAuditEvent` 记录 repo CRUD 审计事件
- `recordSyncFailure` 持久化 `last_sync_error` 到 MySQL（L1380-L1384）

### 21.7 关键日志点

| 事件 | 级别 | 字段 |
|------|------|------|
| `knowledge: built-in vault synced` | Info | source / file_count |
| `knowledge: cloud vault clone failed — retrying` | Warn | attempt / err |
| `knowledge: cloud vault clone ok after retry` | Info | attempt |
| `knowledge: sync failed` | Warn | repo_id / err |
| `knowledge: repo deleted` | Warn | repo_id / url / file_count |
| `knowledge: embedder unavailable — reads enabled, writes disabled` | Warn | err |
| `embedding: local model loaded` | Info | model / dim / cache_dir |

---

## 22. 架构红线与设计要点

### 22.1 Qdrant 是唯一向量真源，无双写

> Today's schema is intentionally simple: knowledge_repos — git repo registrations; knowledge_docs content rows live as qdrant payload on a point. No double-write. — [usecase.go](file:///d:/claude/ongrid/internal/manager/biz/knowledge/usecase.go) L1-L15

MySQL 只存 repo 注册 + SSH 凭据，所有 doc 正文 + 向量在 qdrant。避免了关系表与向量库之间的双写一致性陷阱。

### 22.2 Embedder 不可用时读路径不阻塞

```go
// Build with a nil embedder when one isn't configured — the usecase
// exposes read paths (ListDocs/Repos/GetDoc/ListPaths) and gates write
// paths (CreateManualDoc/Sync/Search) on embed != nil so the SPA's
// 知识库 / 代码仓库 pages render on fresh install instead of 404'ing.
// Operator configures ONGRID_EMBEDDING_API_KEY later → writes unblock
// without restart-of-stack (only the manager needs the key on boot).
```

—— main.go L1265-L1271

fresh install 无 LLM key 时 SPA 列表仍能渲染，避免误判"RAG 坏了"。

### 22.3 Point ID 稳定哈希 + chunk 0 保持原 ID

- `md5(scope||url)` → 高 8 字节 uint64
- chunk 0 ID = 原 docID，让 operator-saved deep links 在 chunking 引入后仍可用
- 重 sync 覆盖原 point 而非重复

### 22.4 GetPoints vs Search 的 uint64 边界

> doc IDs are full uint64 (md5-derived) and qdrant's filter parser rejects values > int64. — [usecase.go](file:///d:/claude/ongrid/internal/manager/biz/knowledge/usecase.go) L205-L208

按 ID 加载 doc 必须用 `GetPoints`（point-id API 原生支持 uint64），不能用 `Search` filter。

### 22.5 DeleteByFilter 拒绝空 filter

```go
if len(mustMatch) == 0 {
    return fmt.Errorf("qdrant: DeleteByFilter requires at least one match clause (refusing to delete all)")
}
```

—— [client.go](file:///d:/claude/ongrid/internal/pkg/qdrantx/client.go) L180-L182

防误删全表。

### 22.6 EnsureCollection 维度不匹配的智能处理

- 空 collection + 维度不匹配 → DROP + recreate（安全）
- 有 points + 维度不匹配 → 拒绝 + 提示手动 curl 命令（防数据丢失）

### 22.7 path_prefixes 数组字段规避 text tokenizer

qdrant `text` tokenizer 对 CJK 太松（"网络" 会误匹配 "K8s/网络"）。改为 `path_prefixes` 数组字段 + `keyword` 索引，每个 doc 存所有累积前缀，prefix 查询 = 精确 keyword 匹配数组元素。

### 22.8 id_alias 不索引

doc ID 跨全 uint64 范围（md5 派生），qdrant filter 解析器拒绝 >int64 的值。`id_alias` 仅作 payload helper，从不作为 filter 字段——按 ID 查询走 `GetPoints`。

### 22.9 原子 temp + rename swap

- `syncAtomicReplace`：clone 到 sibling tmp → 成功后 `rm -rf <dir>` + `os.Rename(tmp, dir)`
- `materializeBuiltinVault`：embed.FS 写到 sibling tmp → 同样 rename swap
- `os.Rename` 在同一文件系统内原子，crash 后要么旧要么新，绝不 hybrid

### 22.10 Vault cloud 优先 + embedded fallback

- `fetchCloudVault` 3 次重试 + 30s per-try
- 失败 → `materializeBuiltinVault`（embed.FS 离线 38-file baseline）
- ADR-029：vault 永远来自固定 `github.com/ongridio/vault`，不可 operator 配置

### 22.11 localEmbedder 串行化 + clampForLocalEmbed

- ONNX 推理非线程安全 → mutex 串行化
- sugarme/tokenizer @ v0.2.3 有 SIGSEGV bug → `clampForLocalEmbed` 350 char 截断
- 空输入 crash tokenizer → 替换为单空格

### 22.12 WaitDelay 防 git hang

```go
cmd.WaitDelay = 10 * time.Second
```

ctx 超时 kill git 后，子进程 ssh/git-remote-https 继承输出 pipe 不释放，`CombinedOutput` 会 block >280s。WaitDelay 强制关闭继承的 pipe。

### 22.13 5 个 payload index 强制建立

```go
for _, f := range []string{"source_type", "repo_id", "path", "path_prefixes", "tags"} {
    if err := vec.EnsurePayloadIndex(ctx, CollectionName, f, "keyword"); err != nil {
        log.Warn(...)
    }
}
```

没有这些 index，qdrant 全表扫描。建立失败只 Warn 不 fail——允许 qdrant 版本不支持某些 index 时降级运行。

### 22.14 docDTO uint64 string 序列化

qdrant point ID 是 md5 派生的 uint64，可能 >2^53（JS Number.MAX_SAFE_INTEGER）。`json:"id,string"` 让 JSON 编码为字符串，避免浏览器精度丢失导致后续 GET 404。

### 22.15 knowledgeUC nil 时路由不注册

```go
if knowledgeHandler != nil {
    knowledgeHandler.Register(protected)
}
```

embedder 不可用时 `knowledgeUC == nil`，路由不注册——避免 handler 调用 nil usecase panic。

---

## 23. 附录：测试覆盖

### 23.1 qdrantx 包

| 测试文件 | 测试 | 覆盖 |
|----------|------|------|
| [filter_test.go](file:///d:/claude/ongrid/internal/pkg/qdrantx/filter_test.go) | `TestBuildFilter_Empty` | nil / 空 map → nil |
| | `TestBuildFilter_StringValue` | string → `match.value` |
| | `TestBuildFilter_StringSliceBecomesAny` | []string → `match.any` |
| | `TestBuildFilter_PrefixMatch` | PrefixMatch → `match.text` |
| | `TestBuildFilter_EmptyPrefixSkipped` | 空 PrefixMatch 跳过 |
| | `TestBuildFilter_EmptySliceSkipped` | 空 []string 跳过 |

### 23.2 knowledge biz 包

| 测试文件 | 测试 | 覆盖 |
|----------|------|------|
| [chunk_test.go](file:///d:/claude/ongrid/internal/manager/biz/knowledge/chunk_test.go) | `TestSplitForChunks_ShortDoc` | 短 doc 单 chunk |
| | `TestSplitForChunks_LongDoc` | 长 doc 多 chunk + overlap |
| | `TestSplitForChunks_CJK` | CJK rune 计数 |
| | `TestSplitForChunks_MaxChunkCap` | maxChunksPerFile 上限 |
| | `TestRepoChunkDocID` | chunk 0 ID = repoDocID，>0 distinct |
| [dedupe_test.go](file:///d:/claude/ongrid/internal/manager/biz/knowledge/dedupe_test.go) | `TestDedupeByIDAlias_PrechunkingManualDoc` | 旧 doc 无 chunk_index 不丢 |
| | `TestDedupeByIDAlias_ChunkedRepoDoc` | 多 chunk 折叠 1 行 |
| | `TestDedupeByIDAlias_HeadUpgrade` | 乱序时 head upgrade |
| | `TestDedupeByIDAlias_MixedManualAndRepo` | 混合 source |
| | `TestDedupeByIDAlias_LimitCap` | limit 截断 |
| | `TestChunkIndexFromPayload` | 多 JSON number flavor |
| [builtin_vault_test.go](file:///d:/claude/ongrid/internal/manager/biz/knowledge/builtin_vault_test.go) | `TestBuiltinVault_Embedded` | embed.FS 非空 |
| | `TestIsBuiltinVaultURL` | sentinel 匹配 |
| | `TestMaterializeBuiltinVault` | 物化 + scanRepoFiles 一致性 |
| | `TestMaterializeBuiltinVault_Idempotent` | 二次物化不报错 + 无 stale tmp |
| [upload_edit_test.go](file:///d:/claude/ongrid/internal/manager/biz/knowledge/upload_edit_test.go) | `TestUpdateManualDoc_UploadDoc` | ADR-028 upload CRUD 回归 |
| | `fakeVec` / `fakeEmbed` | 测试桩注入 |

### 23.3 HTTP handler 包

| 测试文件 | 测试 | 覆盖 |
|----------|------|------|
| [http_test.go](file:///d:/claude/ongrid/internal/manager/server/knowledge/http_test.go) | `memVec` | in-memory qdrant，JSON round-trip 模拟生产 payload 类型 |

`memVec` 注释（L24-L30）：

> memVec is a faithful-enough stand-in for *qdrantx.Client: it stores points by id and, crucially, JSON round-trips every payload on Upsert so numbers come back as float64 and arrays as []any — exactly what the real HTTP client decodes. That makes the biz dedupe/filter helpers (chunkIndexFromPayload, docIDAlias, MustMatch matching) run the same code paths they do against live qdrant.

### 23.4 systemhealth 包

| 测试文件 | 测试 | 覆盖 |
|----------|------|------|
| [service_test.go](file:///d:/claude/ongrid/internal/manager/service/systemhealth/service_test.go) | `TestCheckAggregatesFailedDependency` | qdrant httptest 桩 + path 验证 |
| | `TestCheckReportsDegradedWhenOptionalCapabilitiesMissing` | EmbeddingConfigured=false → Degraded |

### 23.5 测试策略总结

- **单元测试**：buildFilter / splitForChunks / dedupeByIDAlias / chunkIndexFromPayload / IsBuiltinVaultURL / materializeBuiltinVault 全覆盖
- **集成测试**：`memVec` 提供 in-memory qdrant，让 HTTP handler 测试不依赖真实 qdrant 容器
- **回归测试**：`TestDedupeByIDAlias_PrechunkingManualDoc` 覆盖"RAG 又没了" bug；`TestUpdateManualDoc_UploadDoc` 覆盖 ADR-028 回归
- **端到端测试**：`tests/e2e/` 下的 `workflow_test.go` 等使用真实 qdrant 容器（见 [tests/e2e/testenv/env.go](file:///d:/claude/ongrid/tests/e2e/testenv/env.go)）

---

## 文档版本

- 撰写时间：2026-07-31
- 仓库快照：撰写时主分支状态
- 覆盖范围：qdrantx 客户端 + embedding 双实现 + knowledge biz 全流程 + aiops RAG 消费 + systemhealth 探测 + 配置装配 + docker-compose 部署 + Dockerfile ONNX + 前端 API
- 跨引用：[ongrid_LLM.md](file:///d:/claude/ongrid/ongrid_LLM.md)（LLM 客户端 / eino / RAG 上层编排）、[ongrid_integration.md](file:///d:/claude/ongrid/ongrid_integration.md)（外部系统集成总览）、[ongrid_api.md](file:///d:/claude/ongrid/ongrid_api.md)（HTTP API 全集）
