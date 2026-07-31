# knowledge/http.go 技术实现文档

## 1. 概述

`http.go` 是 ongrid manager 知识库子域（`/v1/knowledge/*`）的 HTTP 路由层实现，承担两类对外能力：

1. **组织知识库文档管理**：手工文档 CRUD、文件上传、关键词搜索、目录路径聚合
2. **Git 仓库集成**：注册仓库、触发同步、注销、内置平台 vault 同步、SSH 身份 CRUD

handler 层只做"参数解析 → 调 biz Service → 写响应"三件事，所有业务逻辑下沉到 `internal/manager/biz/knowledge`。

## 2. 包信息

- **包名**：`knowledge`
- **导入路径**：`github.com/ongridio/ongrid/internal/manager/server/knowledge`
- **路由前缀**：`/v1/knowledge`（由 `cmd/ongrid` 在 chi router 上挂载，鉴权中间件由上游统一注入）
- **文件定位**：HTTP 适配层（不直接落库、不直接调外部服务）

## 3. 关键类型与接口

### Service —— 窄接口

handler 仅依赖 biz 层暴露的最小方法集，由 `*biz.Usecase` 通过结构化类型（structural typing）满足：

```go
type Service interface {
    ListDocs(ctx context.Context, f biz.ListDocsFilter) ([]*model.Doc, error)
    GetDoc(ctx context.Context, id uint64) (*model.Doc, error)
    CreateManualDoc(ctx context.Context, in biz.CreateManualDocInput) (*model.Doc, error)
    UpdateManualDoc(ctx context.Context, id uint64, in biz.UpdateManualDocInput) (*model.Doc, error)
    MoveDoc(ctx context.Context, id uint64, newPath string) (*model.Doc, error)
    UploadDoc(ctx context.Context, in biz.UploadDocInput) (*model.Doc, error)
    DeleteDoc(ctx context.Context, id uint64) error
    Search(ctx context.Context, q string, opts biz.SearchOptions) ([]biz.SearchHit, error)
    ListPaths(ctx context.Context) (map[string]int, error)

    ListRepos(ctx context.Context) ([]*model.Repository, error)
    CreateRepo(ctx context.Context, in biz.CreateRepoInput) (*model.Repository, error)
    Sync(ctx context.Context, id uint64) (*model.Repository, error)
    DeleteRepo(ctx context.Context, id uint64) error
    SyncBuiltinVault(ctx context.Context) (int, string, error)

    ListSSHIdentities(ctx context.Context) ([]*model.SSHIdentity, error)
    CreateSSHIdentity(ctx context.Context, in biz.CreateSSHIdentityInput) (*model.SSHIdentity, error)
    GenerateSSHIdentity(ctx context.Context, in biz.GenerateSSHIdentityInput) (*model.SSHIdentity, error)
    UpdateSSHIdentity(ctx context.Context, id uint64, in biz.UpdateSSHIdentityInput) (*model.SSHIdentity, error)
    DeleteSSHIdentity(ctx context.Context, id uint64) error
}
```

### AuthzMW —— 可选 casbin 中间件

```go
type AuthzMW interface {
    Require(obj, act string) func(http.Handler) http.Handler
}
```

`AuthzMW` 是窄 casbin 契约，**可选注入**（`SetAuthz` 后置装配）。nil 时所有变更类路由退化到 `passthrough`（依赖上游 auth 中间件保证已登录），保证从旧版"任何已登录用户可写"平滑迁移到 casbin RBAC。

### Handler

```go
type Handler struct {
    svc   Service
    authz AuthzMW
}
```

### DTO

```go
type docDTO struct {
    ID         uint64    `json:"id,string"`
    SourceType string    `json:"source_type"`
    RepoID     *uint64   `json:"repo_id,omitempty,string"`
    URL        string    `json:"url,omitempty"`
    Title      string    `json:"title"`
    TitleEN    string    `json:"title_en,omitempty"`
    Content    string    `json:"content,omitempty"`
    Path       string    `json:"path,omitempty"`
    Tags       []string  `json:"tags,omitempty"`
    CreatedAt  time.Time `json:"created_at"`
    UpdatedAt  time.Time `json:"updated_at"`
}

type repoDTO struct {
    ID            uint64     `json:"id"`
    URL           string     `json:"url"`
    Branch        string     `json:"branch"`
    Description   string     `json:"description,omitempty"`
    LastSyncedAt  *time.Time `json:"last_synced_at,omitempty"`
    LastSyncError string     `json:"last_sync_error,omitempty"`
    FileCount     int        `json:"file_count"`
    IsBuiltin     bool       `json:"is_builtin"`
    CreatedAt     time.Time  `json:"created_at"`
    UpdatedAt     time.Time  `json:"updated_at"`
}
```

## 4. 关键函数与流程

### Register —— 路由表

```go
func (h *Handler) Register(r chi.Router)
```

注册路由（所有路由上游已被 `tenantctx` 鉴权中间件拦截）：

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| GET | `/v1/knowledge/docs` | — | 列文档（按 source/repo/path 过滤） |
| GET | `/v1/knowledge/docs/{id}` | — | 单文档（带 content） |
| POST | `/v1/knowledge/docs` | `knowledge:doc/write` | 创建手工文档 |
| PATCH | `/v1/knowledge/docs/{id}` | `knowledge:doc/write` | 修改手工文档 |
| PATCH | `/v1/knowledge/docs/{id}/move` | `knowledge:doc/write` | 拖拽改路径（ADR-029） |
| DELETE | `/v1/knowledge/docs/{id}` | `knowledge:doc/delete` | 删除手工文档 |
| POST | `/v1/knowledge/upload` | `knowledge:doc/write` | 文件上传（ADR-028） |
| GET | `/v1/knowledge/search` | — | 全库关键词搜索 |
| GET | `/v1/knowledge/paths` | — | 路径 → 文档数聚合 |
| GET | `/v1/knowledge/repos` | — | 列仓库 |
| POST | `/v1/knowledge/repos` | `knowledge:repo/write` | 注册仓库（带审计） |
| POST | `/v1/knowledge/repos/{id}/sync` | `knowledge:repo/write` | 触发同步 |
| DELETE | `/v1/knowledge/repos/{id}` | `knowledge:repo/delete` | 注销仓库（带审计） |
| POST | `/v1/knowledge/vault/sync` | `knowledge:repo/write` | 同步内置 vault |
| GET | `/v1/knowledge/ssh-identities` | — | 列 SSH 身份 |
| POST | `/v1/knowledge/ssh-identities` | `knowledge:repo/write` | 导入私钥 |
| POST | `/v1/knowledge/ssh-identities/generate` | `knowledge:repo/write` | 现场生成密钥对 |
| PATCH | `/v1/knowledge/ssh-identities/{id}` | `knowledge:repo/write` | 更新元数据 |
| DELETE | `/v1/knowledge/ssh-identities/{id}` | `knowledge:repo/delete` | 删除 |

### writeMW / deleteMW —— 条件中间件

```go
func (h *Handler) writeMW(obj string) func(http.Handler) http.Handler
func (h *Handler) deleteMW(obj string) func(http.Handler) http.Handler
```

`authz` 非空则返回 `Require(obj, "write"|"delete")`，否则返回 `passthrough`（透传），实现"装了 casbin 才卡权限，没装就走老路径"的平滑迁移。

### uploadDoc —— 文件上传

```go
const maxUploadBytes = 8 << 20  // 8 MiB

func (h *Handler) uploadDoc(w http.ResponseWriter, r *http.Request)
```

流程：
1. `ParseMultipartForm(maxUploadBytes)` 双重保险（先 multipart buffer，再 `io.LimitReader`+`ReadAll`+`>` 判断超限）
2. `docextract.Supported(name)` 白名单 `.md / .txt / .pdf / .docx`（其余 400）
3. `docextract.Extract` 抽纯文本（md/txt 直通；pdf/docx 解析；扫描/图片 PDF 无文本层会报错拒绝，OCR 不在范围）
4. 调 `svc.UploadDoc` 落库

写审计：`auditmw.SetAuditEvent` 写 `biz.CreateRepoInput` / `Sync` / `DeleteRepo` 事件（变更类仓库操作）。

### 错误响应

`writeErr` 用 `http.Error` 写纯文本，**不走** JSON envelope —— 因为前端调用错误展示是 fallback 通道，与本子域其它 `writeJSON` 路径在错误态上分离。

## 5. 依赖关系

**外部**：
- `chi` —— 路由
- `net/http` —— 标准 HTTP

**内部**：
- `internal/manager/biz/knowledge`（biz）
- `internal/manager/biz/audit`（审计事件类型）
- `internal/manager/model/knowledge`、`internal/manager/model/audit`
- `internal/manager/server/middleware`（`auditmw`）
- `internal/pkg/docextract`（文档解析）
- `internal/pkg/errs`（错误哨兵）

## 6. 并发与资源管理

- **无共享可变状态**：`Handler.svc`、`Handler.authz` 均在启动时装配，handler 内不再修改；chi handler 是并发安全的。
- **请求级隔离**：所有 ctx 来自 `r.Context()`，handler 之间无共享资源。
- **上传大小硬上限 8 MiB**：`ParseMultipartForm` + `LimitReader` 双层限制，防止内存爆炸。
- **`defer file.Close()`** 保障 multipart 句柄释放。

## 7. 设计模式与亮点

1. **窄接口 + 结构化类型满足**：`Service` 接口只列 handler 实际调用的方法，`*biz.Usecase` 通过鸭子类型满足，避免业务层反向依赖 HTTP 包。
2. **可选中间件 + 后置装配**：`SetAuthz` 在构造之后注入 casbin，`writeMW/deleteMW` 用 `passthrough` 兜底，实现权限收紧的灰度迁移。
3. **`,string` ID 序列化防精度丢失**：`docDTO.ID` 用 `json:"id,string"`，因为 qdrant point ID 是 md5 派生的 uint64（>2^53），直接 JSON number 在浏览器 IEEE-754 double 解析下低位丢失，后续 `GET /docs/{id}` 会 404。这是经过线上 bug 才加上的关键防护。
4. **`IsBuiltin` 标志位替代字符串匹配**：`repoDTO.IsBuiltin` 用 `biz.IsBuiltinVaultURL(r.URL)` 显式判定，避免前端用脆弱的 URL 子串判断（历史上一度在 `ongridio/vault → builtin://vault` 迁移时静默失效）。
5. **vault sync 独立端点**：内置 vault 不是 repo 行，所以 `/v1/knowledge/vault/sync` 而非 `/repos/{id}/sync`，语义清晰。
6. **SSH 身份归属 knowledge 路由**：所有 git 鉴权操作挂在 `/v1/knowledge/ssh-identities`，让"代码仓库"页维持单一 URL 前缀，避免运维入口分裂。
7. **双层上传限制**：`ParseMultipartForm` 已会按 maxMemory 落盘，再用 `LimitReader(+1)` 二次校验，避免 multipart 解析边界 case 导致的越界读取。

## 8. 注意事项

1. **审计只覆盖仓库类变更**：`SetAuditEvent` 仅在 `createRepo / syncRepo / deleteRepo` 上写。文档 CRUD（手工创建/上传/移动/删除）**目前不写审计**——若 P0 要求追溯所有知识库变更，需补 hook。
2. **`MoveDoc` 走 PATCH /move 而非 PATCH /docs/{id}**：路径变更与字段更新分离，避免一次 PATCH 同时改 path 和 content 的歧义。
3. **`writeErr` 不走 JSON envelope**：前端调用本子域错误展示走 fallback，与其它子域的 `{"code","message","data"}` 一致性不同；若要全局统一需评估影响。
4. **`maxUploadBytes = 8 MiB`**：md/txt 通常很小，给 8 MiB 是为 PDF/docx 留余量；超过会 400 而非截断，避免静默损坏。
5. **OCR 不支持**：扫描/图片 PDF 会被 `docextract.Extract` 拒绝；如需 OCR 需另起异步任务。
6. **`uploadDoc` 文件名 sanitize**：用 `filepath.Base(hdr.Filename)` 取 basename，防止目录穿越攻击。
7. **`authz` 注入是 optional**：未注入时所有 `write/delete` 路由降级为 `passthrough`（已登录即可），上线灰度时务必确认部署清单里 casbin 是否就位。
