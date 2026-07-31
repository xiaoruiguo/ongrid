# `service.go` 技术实现文档

> 源文件：`internal/iam/service/service.go`
> 包路径：`github.com/ongridio/ongrid/internal/iam/service`

## 1. 概述

本文件是 IAM BC 的 service 层（HTTP/gRPC handler 层），封装 `biz/user.Usecase` 与 Phase-1 新增的 org / membership / authz 三个 biz 服务。负责请求验证、错误映射、委托 biz 用例。gospec 红线：禁止 import `internal/iam/data/**`，由 go-arch-lint 强制。Phase-1 子服务可选装配，未装配时对应路由返回 503。

## 2. 包信息

- **包名**：`service`
- **所属模块**：`internal/iam/service` —— IAM BC 的 service / handler 层
- **依赖方向**：被 `internal/iam/server` 唯一调用；依赖 `internal/iam/biz/authz`、`biz/membership`、`biz/org`、`biz/user`、`data/membership/store`（仅取类型）、`iam/model`

## 3. 关键类型与接口

```go
type Service struct {
	user        *biz.Usecase
	orgs        *org.Service
	memberships *membership.Service
	authz       *authz.Enforcer
	log         *slog.Logger
}
```

- `Service`：聚合四个 biz 子服务 + logger。`user` 为必填，其余三个可后置注入（Phase-1 灰度策略）。

## 4. 关键函数与流程

### `New`
- **签名**：`func New(user *biz.Usecase, log *slog.Logger) *Service`
- **职责**：构造 Service，仅注入必填的 user usecase。
- **注释要点**：orgs/memberships/authz 后置 Set* 注入，nil 时对应 HTTP 路由返回 503 NotWiredYet。

### 后置注入：`SetOrgs` / `SetMemberships` / `SetAuthz`
- **签名**：分别接收 `*org.Service` / `*membership.Service` / `*authz.Enforcer`
- **职责**：启动后构造阶段注入 Phase-1 子服务。
- **非并发安全**：注释未提及，应在启动单线程阶段完成注入后不再变更。

### 访问器：`Orgs` / `Memberships` / `Authz` / `User`
- **签名**：返回对应指针，可能为 nil
- **职责**：暴露子服务给 HTTP 层（`server` 包）。
- **`User()` 注释要点**：直接暴露 usecase 而非为每个新方法加薄透传，避免本文件膨胀。

### 透传方法：`Register` / `Login` / `Refresh` / `GetByID` / `List` / `Delete` / `SetRole`
- **签名**：与 `biz/user.Usecase` 对应方法一致
- **职责**：薄透传至 `s.user`。
- **错误处理**：透传 biz 错误，不做额外映射。

### `MembershipsByUser`
- **签名**：`func (s *Service) MembershipsByUser(ctx, userID uint64) ([]store.MembershipWithOrg, error)`
- **职责**：返回用户所属 org 列表。
- **流程**：若 `s.memberships == nil` 返回 `nil, nil`（让 HTTP 层显示空列表而非 503）；否则调 `ListByUser`。
- **注释要点**：`/v1/me` 端点在 memberships 未装配时返回用户基本信息 + 空 memberships。

## 5. 依赖关系

- **内部包**：`internal/iam/biz/authz`、`biz/membership`、`biz/org`、`biz/user`（alias 为 `biz`）、`data/membership/store`（仅取 `MembershipWithOrg` 类型）、`iam/model`
- **外部库**：`log/slog`、标准库 `context`
- **被调用方**：`internal/iam/server.Handler`（唯一消费者）

## 6. 并发与资源管理

无显式锁；Service 本身无状态（依赖注入的子服务各自保证并发安全）。所有方法首参 `context.Context`。Set* 方法在启动期单线程调用，运行期不再变更。

## 7. 设计模式与亮点

- **gospec 分层强制**：注释明确「must never import internal/iam/data/**」，由 go-arch-lint 静态强制。
- **Phase-1 灰度装配**：后置 Set* + nil 检查，旧部署未升级 Phase-1 时仍可运行，新路由返回 503。
- **薄透传 + 直接暴露**：核心 auth 方法薄透传，新方法（profile/pass-hash）直接暴露 `User()` usecase 避免每次新增都改本文件。
- **唯一消费者**：注释声明 HTTP router 是唯一调用方，保证 service 层不被多处依赖耦合。
- **`MembershipsByUser` 优雅降级**：nil 时返回空切片而非错误，前端无感。

## 8. 注意事项

- `Set*` 方法无锁，若运行期被调用会引入 data race；需在文档/启动流程中约束「仅启动期注入」。
- `data/membership/store` 的 import 仅用于取 `MembershipWithOrg` 类型，存在 service 层直接依赖 data 层类型的轻微耦合；理论上该类型应在 biz 层定义或由 biz 层返回，但当前为最小代价方案。
- `User()` 直接暴露 usecase，绕过 service 层封装；新增 usecase 方法需评估是否需要 service 层薄透传以保持一致性。
- `MembershipsByUser` 返回 nil 时 HTTP 层 `me` handler 会显示空 memberships，需确保前端能区分「无成员关系」与「服务未装配」。
- 未装配子服务时对应路由返回 503，但路由本身仍注册；若希望完全隐藏路由需在 RegisterProtected 阶段判断。
