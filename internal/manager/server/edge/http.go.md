# `http.go` 技术实现文档

> 源文件：`internal/manager/server/edge/http.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/server/edge`

## 1. 概述

本文件是 manager/edge 子域的 HTTP 路由层：暴露 `/v1/edges` CRUD、`/v1/edges/{id}/upgrade*`（单 + 批量）、`/v1/edges/{id}/processes`、`/v1/edges/{id}/plugins`、`/v1/integrations/plugin-counts` 等端点。设计要点：Post-split（May 2026）role 数据迁移到 Device，edge listing 仍 denormalise host device 的 roles + host_info 供 UI 兼容；批量操作用 bounded concurrency（`batchConcurrency=8`）+ per-id 结果信封（partial failure 是常态）。关键红线：`roleAdmin` 本地常量镜像 iam/model；升级 bundle 走 sha256 校验门；`maxBatchIDs=500` 防滥用。

## 2. 包信息

- **包名**：`edge`
- **所属模块**：`internal/manager/server/`（HTTP handler 层）
- **依赖方向**：被上层 router 装配调用 `NewHandler` + `Set*` + `Register`；依赖 `biz/edge`、`biz/device`（`Repo`）、`model/edge`、`model/device`、`pkg/errs`、`pkg/tenantctx`、`pkg/tunnel`

## 3. 关键类型与接口

```go
const roleAdmin = "admin"
const maxBatchIDs = 500
const batchConcurrency = 8

type EdgeService interface {
    Create / List / Get / Delete / RotateSecret
    UpgradeAgent(ctx, edgeID, url, sha256) (tunnel.AgentUpgradeResponse, error)
    FetchPackage(ctx, edgeID, url, sha256, version) (tunnel.FetchPackageResponse, error)
    ApplyPackage(ctx, edgeID) (tunnel.ApplyPackageResponse, error)
    GetProcessList(ctx, edgeID, topN, sortBy) (tunnel.GetProcessListResponse, error)
    PluginHealth(edgeID) []biz.PluginHealth
}

type PackageResolver interface {
    ResolveBundle(arch, version string) (url, sha256, resolvedVersion string, err error)
}

type PluginConfigService interface {
    ListForUI(ctx, edgeID) ([]biz.PluginRow, error)
    Set(ctx, edgeID, plugin, in) (*biz.PluginRow, error)
    CountByPlugin(ctx) (map[string]int64, error)
}

type AuthzMW interface {
    Require(obj, act string) func(http.Handler) http.Handler
}

type Handler struct {
    svc       EdgeService
    devices   devicebiz.Repo          // 可空：listings 省略 host_info
    pluginCfg PluginConfigService     // 可空：plugin 路由 404
    pkgRes    PackageResolver         // 可空：upgrade-package 503
    authz     AuthzMW                 // 可空：退化为 requireAdmin
}
```

DTO：`createReq/Resp`、`hostInfoDTO`、`listItem`/`getResp`（含 denormalised roles + host_info）、`upgradeReq/Resp`、`upgradePkgReq/Resp`、`batchReq`/`batchUpgradePkgReq`/`batchUpgradeAgentReq`/`batchResultItem`/`batchResp`、`pluginItemDTO`/`pluginHealthDTO`/`pluginTargetHealthDTO`、`processDTO`/`processesResp`。

## 4. 关键函数与流程

### `NewHandler` + `SetPackageResolver` / `SetAuthz`
- **职责**：构造 + 可选注入；`devices`/`pluginCfg` 可空触发降级
- **流程**：`SetAuthz` 后 `writeMW`/`deleteMW` 优先走 casbin，否则走 `requireAdmin`

### `writeMW` / `deleteMW`
- **签名**：`func (h *Handler) writeMW(obj string) func(http.Handler) http.Handler`
- **职责**：返回写/删路由的中间件；authz 已 wire 走 `authz.Require(obj, "write"/"delete")`，否则走 `requireAdmin`
- **关键**：向后兼容——legacy caller 不调 `SetAuthz` 仍得 admin-only enforcement

### `Register`
- **职责**：挂载全部路由
- **流程**：
  - `POST /v1/edges`（writeMW）、`GET /v1/edges`、`GET /v1/edges/{id}`、`DELETE /v1/edges/{id}`（deleteMW）、`POST /v1/edges/{id}/rotate-secret`（writeMW）
  - `POST /v1/edges/{id}/upgrade`（writeMW）、`POST /v1/edges/{id}/upgrade-package`（writeMW）
  - 批量：`POST /v1/edges/batch/upgrade-package`、`/batch/upgrade`（writeMW）、`/batch/delete`（deleteMW）
  - `GET /v1/edges/{id}/processes`、`GET /v1/edges/{id}/plugins`、`PUT /v1/edges/{id}/plugins/{name}`（writeMW `edge:plugin`）、`GET /v1/integrations/plugin-counts`
- **关键**：chi 静态段 `batch` 优先于 `{id}` 匹配

### `listEdges` / `getEdge`
- **职责**：列表/详情；从 device repo hydrate host facts + roles
- **流程**：
  1. tenantctx 校验
  2. listEdges：解析 filter → `h.svc.List` → `loadDevicesForEdges` 批量加载 device → 逐 edge 拼 `listItem`（含 `deviceRoles`/`deviceToHostInfo`）
  3. getEdge：parseID → `h.svc.Get` → `loadDevice` 单个加载 → 拼 `getResp`
- **关键**：`loadDevicesForEdges` 用 `GetMany` 批量加载避免 N+1

### `createEdge` / `deleteEdge` / `rotateSecret`
- **职责**：CRUD
- **流程**：createEdge 取 `t.UserID` 作 createdBy；rotateSecret 返新 secret

### `upgradeAgent`
- **职责**：`POST /v1/edges/{id}/upgrade` —— 显式 URL+sha 升级
- **流程**：parseID → decode `{url, sha256}` → 浅校验非空 → `h.svc.UpgradeAgent` → 200 `{staged_path, bytes}`
- **关键**：浅校验（presence+length），edge 侧再校验 sha 后才落盘

### `upgradePackage`
- **职责**：`POST /v1/edges/{id}/upgrade-package` —— 一键 bundle 升级
- **流程**：
  1. parseID → `h.pkgRes == nil` → 503 `ErrNotWiredYet`
  2. decode（body 可选，忽略错误）→ arch 默认 `linux-amd64` → `h.pkgRes.ResolveBundle(arch, version)` → 得 url/sha/ver
  3. `h.svc.FetchPackage`（长 RPC，下载 + 校验 + stage）
  4. `h.svc.ApplyPackage`（短 RPC，edge ACK 后 exit）
  5. apply 失败但 stage 成功 → 202 `{applied: false, apply_error: ...}`（提示可重试 apply）
  6. 全成功 → 200 `{applied: true}`
- **关键**：两步合一 HTTP 调用——「I clicked the button, it's done」

### 批量操作（`batchDelete` / `batchUpgradeAgent` / `batchUpgradePackage`）
- **职责**：fleet 级操作，per-id 结果信封
- **流程**：
  1. decode `{ids: [...]}` + 操作特定字段
  2. `normalizeBatchIDs`：空/超 500/含 0 → ErrInvalid；去重保序
  3. `runEdgeBatch`：bounded concurrency（sem channel `batchConcurrency=8`）fan-out，结果按输入顺序回填
  4. 统计 succeeded/failed → 200 `{total, succeeded, failed, results: [...]}`
- **关键**：partial failure 是常态（部分 edge 离线），永不返单 500；`batchUpgradePackage` 中 stage 成功但 apply 失败的 edge 标 `applied: false` + `apply_error`，提示可重试

### `getProcesses`
- **职责**：`GET /v1/edges/{id}/processes?top_n=&sort_by=` —— top-N 进程
- **流程**：tenantctx + parseID → topN 默认 20（上限 200）→ sortBy 默认 `mem`（仅认 cpu/mem）→ `h.svc.GetProcessList` → 翻译 `processDTO` → 200 `{items, sampled_at}`
- **关键**：注释明示 sort_by 默认 mem 匹配 Monitor 页「what's eating my memory」用例

### plugin 端点（`listPlugins` / `setPlugin` / `pluginCounts`）
- **职责**：插件配置 + 运行时健康 + 集成卡片计数
- **流程**：
  - `listPlugins`：`h.pluginCfg == nil` → 404；否则 `ListForUI` + 合并 `PluginHealth`（heartbeat 上报的实时状态，按 plugin name 索引）→ 200
  - `setPlugin`：`h.pluginCfg == nil` → 404；否则 decode `biz.SetInput` → `Set` → 200
  - `pluginCounts`：`h.pluginCfg == nil` → 空计数；否则 `CountByPlugin` → 200 `{counts: {...}}`
- **关键**：`nilIfZero` 把零时间转 nil，避免 JSON 输出 `0001-01-01T00:00:00Z`

### helpers
- `loadDevicesForEdges`：批量 `GetMany` 加载 device，避免 N+1；nil repo / 空 ids 返 nil
- `loadDevice`：单 device 加载；nil repo / nil id 返 nil
- `deviceRoles`：nil device → `[]string{}`（legacy UI 依赖 `roles: []`）
- `deviceToHostInfo`：Device → hostInfoDTO；nil → nil（omitempty）
- `normalizeBatchIDs` / `runEdgeBatch`：批量操作 helper
- `nilIfZero`：零时间 → nil 指针
- `parseID` / `writeJSON` / `writeErr` / `errCode`：标准 helper

## 5. 依赖关系

- **内部包**：
  - `biz/edge`（`CreateResult`/`ListFilter`/`PluginHealth`/`PluginRow`/`SetInput`）、`biz/device`（`Repo`）
  - `model/edge`（`Edge`）、`model/device`（`Device`、`DecodeRoles`）
  - `pkg/errs`、`pkg/tenantctx`、`pkg/tunnel`（RPC 响应类型 + `ProcessSortBy*` 常量）
- **外部库**：`github.com/go-chi/chi/v5`
- **被调用方**：上层 router 装配代码（`cmd/ongrid/main.go`）

## 6. 并发与资源管理

- **`batchConcurrency=8` 信号量**：批量操作用 `chan struct{}` 限并发，避免 hammer 全 fleet；注释明示 8 是 throughput vs manager/network load 的平衡
- **`sync.WaitGroup`**：`runEdgeBatch` 等所有 goroutine 完成
- **结果按输入顺序回填**：`results := make([]batchResultItem, len(ids))` + 按 index 写，避免排序
- **ctx 透传**：所有 service 调用透传 `r.Context()`；批量操作共用同一 ctx
- **无共享状态**：Handler 字段启动期设定后只读

## 7. 设计模式与亮点

- **Post-split denormalisation**：role 数据迁到 Device，但 edge listing 仍 embed roles + host_info 供 UI 兼容——`loadDevicesForEdges` 批量 `GetMany` 避免 N+1
- **AuthzMW 可选注入**：`SetAuthz` 后走 casbin（含 superuser 短路），否则退化为 `requireAdmin`——向后兼容
- **`writeMW`/`deleteMW` 工厂**：按 obj + act 生成中间件，声明式装配
- **两步升级合一 HTTP**：`upgradePackage` 内部 fetch_package + apply_package 串行——「I clicked the button, it's done」；apply 失败但 stage 成功返 202 + `applied: false` 提示可重试
- **批量 partial failure 信封**：永不返单 500；per-id `{ok, error, code}` + 总计 succeeded/failed
- **`maxBatchIDs=500`**：防超大 body hammer manager；`normalizeBatchIDs` 去重保序
- **`nilIfZero` 零时间**：避免 JSON `0001-01-01T00:00:00Z`，UI omitempty 干净
- **`PluginHealth` 合并**：静态 config（enabled/spec）+ 实时 heartbeat 健康按 name join，单次响应完整
- **`roleAdmin` 本地常量**：镜像 iam/model 避免 BC 跨界 import

## 8. 注意事项

- **`roleAdmin` 字面量耦合**：iam 改值需同步
- **`maxBatchIDs=500`**：超 500 报 ErrInvalid；若 fleet 超 500 需分批调用
- **`batchConcurrency=8` 写死**：升级场景平衡值；纯 delete 可调高但当前不区分
- **`upgradePackage` body 可选**：`_ = json.NewDecoder(...).Decode(&req)` 忽略错误——空 body 走默认 arch+version
- **`upgradeAgent` 浅校验**：仅 presence+length，edge 侧再校验 sha 后落盘
- **`listEdges` total 是 `len(items)`**：未调真实 count；若需总数需扩展 service
- **`pluginCfg == nil` 降级**：listPlugins/setPlugin 返 404；pluginCounts 返空计数
- **`pkgRes == nil` 降级**：upgrade-package 返 503，SPA 退化为 URL+sha modal
- **chi 静态段优先**：`/v1/edges/batch/*` 优先于 `/v1/edges/{id}` 匹配，无需手动排序
- **`getProcesses` topN 上限 200**：防超大 topN 拖垮 edge
- **`errCode` slug 表**：覆盖 `not-found`/`unauthorized`/`forbidden`/`conflict`/`invalid`/`budget-exceeded`/`edge-offline`/`not-wired-yet`/`internal`；新增 sentinel 需扩展
