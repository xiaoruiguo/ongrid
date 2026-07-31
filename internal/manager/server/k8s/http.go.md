# `http.go` 技术实现文档

> 源文件：`internal/manager/server/k8s/http.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/server/k8s`

## 1. 概述

本文件是 Kubernetes 集群管理子域的 HTTP 层：暴露 `/v1/k8s/clusters` CRUD、`/v1/k8s/clusters/{id}/{nodes,workloads,pods,events,health}` 列表、`/internal/k8s/{enroll,telemetry-config}` 内部端点。设计要点：分两组路由——protected（bearer + admin gating 写）+ internal（edge 调用，bootstrap token 鉴权）；`enroll` 用信号量限并发（64 slot）防 enroll 风暴。关键红线：list 端点 `total` 必须是 unfiltered count（Count* 时清零 Limit/Offset）；`decodeJSON` 用 `DisallowUnknownFields` + `MaxBytesReader(1MB)`；`requireAdmin` 允许 superuser 或 admin。

## 2. 包信息

- **包名**：`k8s`
- **所属模块**：`internal/manager/server/`（HTTP handler 层）
- **依赖方向**：被上层 router 装配调用 `NewHandler` + `RegisterProtected` + `RegisterInternal`；依赖 `biz/k8s`（`Usecase`、各种 Input/Filter/Result）、`model/k8s`、`pkg/errs`、`pkg/tenantctx`

## 3. 关键类型与接口

```go
const roleAdmin = "admin"
const maxListLimit = 500

type Service interface {
    CreateCluster / ListClusters / CountClusters / GetCluster
    ListNodesPage(ctx, f) ([]*model.Node, int64, error)
    GetNodeCoverage / GetNodeCoverageByClusterIDs
    UpgradeCommand(cluster *model.Cluster) string
    ListEdgeAttachments(ctx, limit, offset) ([]biz.EdgeAttachment, int64, error)
    GetClusterHealth(ctx, clusterID) (biz.ClusterHealthSummary, error)
    ListWorkloads / CountWorkloads
    ListPods / CountPods
    ListEvents / CountEvents
    RotateBootstrapToken / DeleteCluster
    Enroll(ctx, in biz.EnrollInput) (*biz.EnrollResult, error)
    RefreshTelemetryConfig(ctx, controllerEdgeID, proof) (*biz.TelemetryConfig, error)
}

type Handler struct {
    svc         Service
    enrollSlots chan struct{} // 容量 64，enroll 并发限流
}
```

DTO：`clusterDTO`（含 `NodeEdgeCoverage`、`Capabilities`、`UpgradeCommand`、`Inventory*` 字段）、`nodeDTO`/`workloadDTO`/`podDTO`/`eventDTO`、`clusterRegistrationDTO`、`clusterHealthDTO`、`edgeAttachmentDTO`、`enrollResponse`、`telemetryConfigResponse`。

## 4. 关键函数与流程

### `NewHandler`
- **签名**：`func NewHandler(s Service) *Handler`
- **职责**：构造 + 初始化 enroll 信号量（容量 64）
- **流程**：`enrollSlots: make(chan struct{}, 64)`

### `RegisterProtected` / `RegisterInternal`
- **职责**：分别挂 protected（admin/bearer）+ internal（edge）路由
- **流程**：
  - protected：`POST/GET /v1/k8s/clusters`、`GET /v1/k8s/edge-attachments`、`GET /v1/k8s/clusters/{id}`、`/health`、`/nodes`、`/workloads`、`/pods`、`/events`、`POST /rotate-token`（admin）、`DELETE`（admin）
  - internal：`POST /internal/k8s/enroll`、`POST /internal/k8s/telemetry-config`

### `createCluster`
- **职责**：`POST /v1/k8s/clusters`
- **流程**：decodeJSON → 取 `tenantctx.UserID` 作 `CreatedBy` → `h.svc.CreateCluster` → 201 + `registrationDTO`
- **Swagger**：有 `@Summary`/`@Router`/`@Success`

### `listClusters`
- **职责**：`GET /v1/k8s/clusters?status=&name=&mode=&limit=&offset=`
- **流程**：
  1. parse filter（Status/Name/Mode/Limit/Offset）
  2. `h.svc.ListClusters(ctx, filter)` → items
  3. **countFilter = filter 但 Limit=0, Offset=0** → `h.svc.CountClusters(ctx, countFilter)` → total（unfiltered count）
  4. `GetNodeCoverageByClusterIDs(ctx, clusterIDs)` 批量取 coverage
  5. 逐 cluster 拼 `clusterDTO`（含 coverage + `UpgradeCommand`）→ 200 `{items, total, limit, offset}`

### `getCluster` / `getClusterHealth`
- **职责**：单 cluster 详情 / 健康摘要
- **流程**：getCluster 调 `GetCluster` + `GetNodeCoverage` + `UpgradeCommand`；getClusterHealth 调 `GetClusterHealth` 返 `clusterHealthDTO`（DegradedWorkloads/PendingPods/CrashLoopBackOff/OOMKilled/ImagePullBackOff/NotReadyNodes 计数）

### `listNodes` / `listWorkloads` / `listPods` / `listEvents`
- **职责**：分页列表 + unfiltered total
- **通用流程**：parseClusterID → parse filter（含 `issue_only` bool）→ `List*` → **countFilter 清零 Limit/Offset** → `Count*` → 翻译 DTO → 200 `{items, total, limit, offset}`
- **关键**：每个 list 都调对应 Count* 取真实 total

### `listEdgeAttachments`
- **职责**：`GET /v1/k8s/edge-attachments` —— K8s 管理的 edge 附加关系
- **流程**：parse limit/offset → `h.svc.ListEdgeAttachments` → 翻译 `edgeAttachmentDTO` → 200

### `rotateToken` / `deleteCluster`
- **职责**：admin 写操作
- **流程**：requireAdmin → parseClusterID → `RotateBootstrapToken` / `DeleteCluster`（含 `force` query bool）；delete 返 204

### `enroll`
- **职责**：`POST /internal/k8s/enroll` —— edge 节点/controller 注册
- **流程**：
  1. **信号量限流**：`select { case h.enrollSlots <- struct{}{}: ... default: 429 + Retry-After: 1 }`
  2. defer 释放 slot
  3. `bearerToken(Authorization)` 取 bootstrap token；失败 → 401
  4. decodeJSON `enrollRequest`
  5. `h.svc.Enroll(ctx, biz.EnrollInput{BootstrapToken, ClusterID, ...})` → 200 + `enrollResponse`（含 EdgeID/AccessKey/SecretKey/CloudAddr/ManagerPublicURL/Telemetry）

### `refreshTelemetryConfig`
- **职责**：`POST /internal/k8s/telemetry-config` —— edge 刷新遥测配置
- **流程**：从 `X-Edge-Id` header 取 edgeID（0/缺失 → 401）→ decodeJSON `telemetryRefreshRequest` → `h.svc.RefreshTelemetryConfig(ctx, edgeID, proof{AccessKey, SecretKey})` → 200 + `telemetryConfigDTO`

### `requireAdmin`（中间件）
- **签名**：`func (h *Handler) requireAdmin(next http.Handler) http.Handler`
- **职责**：403 非 admin/superuser
- **流程**：`tenantctx.From` 失败 **或** `!t.IsSuperuser && t.Role != roleAdmin` → 403；否则 next
- **关键**：允许 superuser 或 admin（比纯 admin 更宽）

### DTO 翻译函数
- `clusterDTOFromModel` / `clusterDTOFromModelWithCoverage`：含 `EffectiveClusterStatus` 计算
- `clusterCapabilitiesFromModelWithCoverage`：根据 controller + inventory 状态生成 inventory/events/telemetry 三项 capability（ready/degraded/unavailable/query-ready）
- `nodeEdgeCoverageDTOFromBiz`：计算 `Missing = Total - EdgeLinked`、`Percent = EdgeLinked * 100 / Total`
- `nodeDTOFromModel` / `workloadDTOFromModel` / `podDTOFromModel` / `eventDTOFromModel`：字段对字段翻译
- `rawJSON(s, fallback)`：空字符串 → fallback（`{}` 或 `[]`）
- `telemetryConfigDTO`：处理 nil

### helpers
- `decodeJSON`：`MaxBytesReader(1MB)` + `DisallowUnknownFields` + join ErrInvalid
- `parseClusterID`：chi URLParam → uint64；0 → ErrInvalid
- `parseListLimit(raw, fallback)`：≤0 返 fallback；>maxListLimit 返 maxListLimit
- `parseListOffset`：< 0 返 0
- `parseBoolDefault`：失败返 fallback
- `bearerToken`：`Bearer ` 前缀拆分
- `writeJSON` / `writeErr` / `errCode`：标准 helper

## 5. 依赖关系

- **内部包**：`biz/k8s`（`Usecase`、`CreateClusterInput`、`ListClustersFilter`、`EnrollInput`、`EnrollResult`、`TelemetryConfig`、`NodeCoverage`、`ClusterHealthSummary`、`EffectiveClusterStatus` 等）、`model/k8s`（`Cluster`/`Node`/`Workload`/`Pod`/`Event`、`ClusterStatusOnline`）、`pkg/errs`、`pkg/tenantctx`
- **外部库**：`github.com/go-chi/chi/v5`
- **被调用方**：上层 router 装配代码

## 6. 并发与资源管理

- **`enrollSlots` 信号量**：容量 64，enroll 并发限流；超限返 429 + `Retry-After: 1`
- **`select` 非阻塞获取 slot**：`default` 分支返 429，避免请求堆积
- **defer 释放 slot**：确保 panic 时也释放
- **ctx 透传**：所有 service 调用透传 `r.Context()`
- **无其他共享状态**：Handler 仅 `svc` + `enrollSlots`

## 7. 设计模式与亮点

- **双路由组**：protected（bearer + admin）+ internal（edge，bootstrap token/X-Edge-Id 鉴权）——分离人类用户与 edge 调用路径
- **`enrollSlots` 信号量限流**：容量 64 防 enroll 风暴；非阻塞 select + 429 + Retry-After
- **list 端点 unfiltered total**：Count* 时 countFilter 清零 Limit/Offset，total 是真实总数——sidebar badge 轮询依赖
- **`GetNodeCoverageByClusterIDs` 批量**：listClusters 一次批量取所有 cluster 的 coverage，避免 N+1
- **`clusterCapabilitiesFromModelWithCoverage` 派生**：根据 controller + inventory 状态派生 inventory/events/telemetry 三项 capability，前端直接渲染
- **`EffectiveClusterStatus` 计算**：注释明示 status 不是存储字段而是计算得出（基于 LastSeenAt 等）
- **`requireAdmin` 允许 superuser**：`!t.IsSuperuser && t.Role != roleAdmin` —— superuser 短路
- **`decodeJSON` 严格解码**：`DisallowUnknownFields` + `MaxBytesReader(1MB)`——配置/注册类输入需严格
- **`rawJSON` fallback**：空 JSON 字段 → `{}` 或 `[]`，避免前端 null 处理
- **Swagger 注释**：每个端点有 `@Summary`/`@Router`/`@Success`

## 8. 注意事项

- **`roleAdmin` 本地常量**：与 iam/model 耦合；iam 改值需同步
- **`maxListLimit = 500`**：list 上限 500；超限返 500
- **`enrollSlots` 容量 64**：写死；若 enroll 峰值更高需调
- **`requireAdmin` 允许 superuser**：与其它 handler 纯 admin 不同；k8s 写操作更宽
- **`refreshTelemetryConfig` 从 header 取 edgeID**：`X-Edge-Id`，0/缺失 → 401；不依赖 bearer
- **`enroll` 用 bootstrap token**：`Authorization: Bearer <token>`，非 JWT
- **`clusterDTO.Status` 是计算字段**：`EffectiveClusterStatus(c, time.Now().UTC())`，非存储字段
- **`clusterDTO.Capabilities` 派生**：inventory/events/telemetry 三项，基于 controller + inventory 状态
- **`errCode` slug 表**：覆盖 `invalid`/`unauthorized`/`forbidden`/`not_found`/`conflict`/`internal`；注意 `not_found` 用下划线（与其它 handler 的 `not-found` 不一致）
- **`decodeJSON` 1MB 上限**：enroll/create 请求体上限；超大请求 400
- **`parseListLimit` fallback 各异**：listClusters 默认 50，listNodes/Workloads/Pods/Events 默认 100，edgeAttachments 用 maxListLimit(500)
