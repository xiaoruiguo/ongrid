# topology/http.go 技术实现文档

## 1. 概述

`http.go` 是 ongrid manager 拓扑图子域（`/v1/topology/*`）的 HTTP 路由层。提供四类资源的 CRUD：nodes（节点）/ relations（关系）/ relation-types（关系类型）/ node-types（节点类型）。读操作任何已认证用户可调，写操作 admin-only。共 18 个端点。

## 2. 包信息

- **包名**：`topology`
- **导入路径**：`github.com/ongridio/ongrid/internal/manager/server/topology`
- **路由前缀**：`/v1/topology`（由 `cmd/ongrid` 挂载，auth 中间件由上游注入）
- **文件定位**：HTTP 适配层（薄 handler —— auth + JSON decode + delegate to biz Usecase）

## 3. 关键类型与接口

### Handler

```go
type Handler struct {
    uc *biz.Usecase
}
```

直接持有 `*biz.Usecase` 具体类型。

### 常量

```go
const roleAdmin = "admin"
```

本地常量镜像 RBAC 角色名，避免跨 BC import。

### DTO

#### Node

```go
type nodeItem struct {
    ID        uint64 `json:"id"`
    Type      string `json:"type"`
    Name      string `json:"name"`
    Props     any    `json:"props,omitempty"`
    CreatedAt string `json:"created_at"`
    UpdatedAt string `json:"updated_at"`
}

type createNodeReq struct {
    Type  string `json:"type"`
    Name  string `json:"name"`
    Props any    `json:"props,omitempty"`
}

type updateNodeReq struct {
    Name  *string `json:"name,omitempty"`
    Props any     `json:"props,omitempty"`
}
```

`updateNodeReq.Name *string` 区分"未传"与"显式空"。`Props any` 透传任意 JSON。

#### Relation

```go
type relationItem struct {
    ID        uint64 `json:"id"`
    SrcID     uint64 `json:"src_id"`
    DstID     uint64 `json:"dst_id"`
    Type      string `json:"type"`
    Props     any    `json:"props,omitempty"`
    CreatedAt string `json:"created_at"`
}

type createRelationReq struct {
    SrcID uint64 `json:"src_id"`
    DstID uint64 `json:"dst_id"`
    Type  string `json:"type"`
    Props any    `json:"props,omitempty"`
}

type updateRelationReq struct {
    Props any `json:"props,omitempty"`
}
```

`updateRelationReq` 仅含 `Props`——关系的 src/dst/type 不可改。

#### RelationType / NodeType

```go
type relationTypeItem struct {
    Name              string `json:"name"`
    DisplayName       string `json:"display_name"`
    DisplayNameEN     string `json:"display_name_en,omitempty"`
    Builtin           bool   `json:"builtin"`
    PropagatesFailure bool   `json:"propagates_failure"`
    Direction         string `json:"direction"`
    SemanticsTag      string `json:"semantics_tag"`
    Description       string `json:"description"`
}

type nodeTypeItem struct {
    Name          string `json:"name"`
    DisplayName   string `json:"display_name"`
    DisplayNameEN string `json:"display_name_en,omitempty"`
    Builtin       bool   `json:"builtin"`
    Tier          int    `json:"tier"`
    Description   string `json:"description"`
}
```

`Tier` 用于 UI 分层布局；`PropagatesFailure` 标记故障传播关系。

## 4. 关键函数与流程

### Register —— 路由表

```go
func (h *Handler) Register(r chi.Router)
```

| 资源 | GET list | GET one | POST | PATCH/PUT | DELETE |
|---|---|---|---|---|---|
| nodes | any auth | any auth | admin | admin (PATCH name/props) | admin |
| relations | any auth (filter src/dst/type/src_or_dst) | any auth | admin | admin (PATCH props only) | admin |
| relation-types | any auth | any auth (by name) | admin | — | admin (by name) |
| node-types | any auth | any auth (by name) | admin | — | admin (by name) |

### requireAdmin 中间件

```go
func (h *Handler) requireAdmin(next http.Handler) http.Handler
```

`tenantctx.From` + `Role == roleAdmin`，否则 401/403。作为 chi `r.With` 中间件挂载到写端点。

### listNodes / listRelations —— 过滤 + 分页

- `listNodes`：filter by `type` / `q`（名称搜索），`limit` / `offset`
- `listRelations`：filter by `type` / `src_id` / `dst_id` / `src_or_dst_id`（双向查询），`limit` / `offset`

### createNode / updateNode

`createNode`：`encodeProps(in.Props)` 转 JSON 字符串存储 → `uc.CreateNode(ctx, type, name, propsStr)` → 201。

`updateNode`：先 `uc.GetNode` 取当前值，`in.Name != nil` 才覆盖 name，`in.Props != nil` 才覆盖 props（PATCH 语义）→ `uc.UpdateNode` → 204。

### encodeProps / decodeProps —— Props 序列化

```go
func encodeProps(v any) (string, error)  // any → JSON string
func decodeProps(s string) any           // JSON string → any, fallback raw string
```

`decodeProps` 解析失败时返原始字符串（防 bypass API 插入垃圾数据导致整体不可读）。

### helpers

- `parseID(r, key)` —— `ParseUint(chi.URLParam(key))`
- `toNodeItem` / `toRelationItem` / `toRelationTypeItem` / `toNodeTypeItem` —— model → DTO 转换，时间格式化为 `2006-01-02T15:04:05Z`
- `writeJSON` / `writeErr` / `errCode` —— 标准 errs 映射，含 `conflict` / `not-wired-yet` slug

## 5. 依赖关系

**外部**：
- `chi` —— URL 参数
- `net/http`、`encoding/json`、`strconv`、`errors`

**内部**：
- `internal/manager/biz/topology`（Usecase + NodeListFilter + RelationListFilter）
- `internal/manager/model/topology`（Node / Relation / RelationType / NodeType）
- `internal/pkg/errs`
- `internal/pkg/tenantctx`

## 6. 并发与资源管理

- **无共享可变状态**：`Handler.uc` 启动时装配
- **请求级隔离**：每请求独立 ctx
- **无 body 大小限制**：`json.Decode(r.Body)` 未套 `MaxBytesReader`——Props 可能很大，严格防御应加上
- **`writeJSON` swallow encode 错误**：响应已开始无法回传

## 7. 设计模式与亮点

1. **四类资源统一模式**：nodes/relations/relation-types/node-types 都走 list/get/create/update/delete，handler 模式一致，便于维护
2. **`requireAdmin` 作为 chi 中间件**：用 `r.With(h.requireAdmin).Post(...)` 挂载，比每个 handler 内调 `requireAdmin` 更声明式
3. **`updateNodeReq.Name *string` PATCH 语义**：指针区分"未传"与"显式空"
4. **`encodeProps` / `decodeProps` 双向转换**：DTO 用 `any` 透传任意 JSON，存储用 JSON 字符串
5. **`decodeProps` fallback raw string**：解析失败返原始字符串而非报错，防 bypass API 插入垃圾数据导致整体不可读
6. **`relationTypeItem.PropagatesFailure`**：标记故障传播关系，UI 可据此渲染影响链
7. **`nodeTypeItem.Tier`**：分层布局依据，UI 按 tier 分行渲染
8. **时间格式 `2006-01-02T15:04:05Z`**：UTC 固定格式，避免时区歧义
9. **`roleAdmin` 本地常量**：避免跨 BC import iam 包
10. **`src_or_dst_id` 双向查询**：relations 支持按 src 或 dst 任一端点查询，便于"该节点的所有关联"

## 8. 注意事项

1. **`roleAdmin` 字符串硬编码**：与其它子域同模式
2. **无 body 大小限制**：Props 可能很大（复杂拓扑节点），建议加 `MaxBytesReader`
3. **`updateRelation` 仅改 Props**：src/dst/type 不可改，需"改关系"只能删重建
4. **`relation-types` / `node-types` 用 name 作 key**：非数字 ID，URL 路径含 name
5. **`Builtin` 字段**：内置类型标记，UI 可据此禁止删除（但本层未强制，由 biz 层决定）
6. **无审计**：本文件未调 `SetAuditEvent`，拓扑变更不写审计；如需追溯需补 hook
7. **`listNodes` / `listRelations` 无 limit 默认值**：未传 limit 时 biz 层决定（可能返全部）；建议显式默认值
8. **`parseID` 不拒绝 0**：与其它子域不同，`id == 0` 不报错——可能误命中
9. **`createNode` 返 201**：与 RESTful 一致；`updateNode` 返 204 No Content
10. **`Props any` 不验证 schema**：handler 不校验 props 是否符合 node-type 定义，由 biz 层负责
11. **`DisplayNameEN` 双语**：支持中英文显示名，前端按 locale 选择
12. **`Direction` / `SemanticsTag`**：关系类型的方向性和语义标签，biz 层决定可选值
