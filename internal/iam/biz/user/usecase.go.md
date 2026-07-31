# `usecase.go` 技术实现文档

> 源文件：`internal/iam/biz/user/usecase.go`
> 包路径：`github.com/ongridio/ongrid/internal/iam/biz/user`

## 1. 概述

本文件是 IAM BC 用户子域的 biz 门面，承载全部认证流程与用户管理操作。核心职责：注册（argon2id 哈希）、登录（签发 access/refresh JWT 对）、刷新令牌、首启管理员 seed、以及管理端的列表 / 删除 / 改角色 / 改状态 / 改密码 / 改 profile 等。包级红线：永不在公开 API 返回 `PassHash`。

## 2. 包信息

- **包名**：`user`
- **所属模块**：`internal/iam/biz/user` —— IAM BC biz 层用户子域核心用例
- **依赖方向**：被 `internal/iam/service` 调用；依赖 `Repo`（实现于 data 层）与 `internal/pkg/auth.Signer`

## 3. 关键类型与接口

```go
type Usecase struct {
	repo   Repo
	signer *auth.Signer
	log    *slog.Logger
}

type TokenPair struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int64 // seconds until access token expiry
	Role         string
	UserID       uint64
}

type CreateInput struct {
	Email       string
	Password    string
	DisplayName string
	Phone       string
	Role        string // admin | user
}
```

- `Usecase`：biz 门面，持有 repo、JWT signer、logger。
- `TokenPair`：Login / Refresh 的返回结构，含双 token 与过期秒数。
- `CreateInput`：管理端「邀请用户」表单 payload。

## 4. 关键函数与流程

### `NewUsecase`
- **签名**：`func NewUsecase(repo Repo, signer *auth.Signer, log *slog.Logger) *Usecase`
- **职责**：依赖注入构造 Usecase。log 可为 nil。

### `Register`
- **签名**：`func (u *Usecase) Register(ctx, email, password, role string) (*model.User, error)`
- **职责**：注册新用户。
- **流程**：TrimSpace + ToLower email → 校验非空 → 校验 role（默认 RoleUser）→ `GetByEmail` 检查冲突（NotFound 视为可注册）→ `hashPassword` → 组装 User → `repo.Create` → 清空 PassHash 返回。
- **错误处理**：邮箱已注册返回 `ErrConflict`；其他包装错误。
- **注释要点**：Role enforcement（仅 admin 可注册）是 HTTP 层职责，biz 政策中立。

### `Login`
- **签名**：`func (u *Usecase) Login(ctx, email, password string) (*TokenPair, error)`
- **职责**：校验凭证并签发 token 对。
- **流程**：email 规范化 → `GetByEmail`（NotFound 映射为 `ErrUnauthorized` 防止枚举）→ 校验 `Status == active` → `verifyPassword` → `issuePair`。
- **错误处理**：用户不存在、状态非 active、密码不匹配均返回 `ErrUnauthorized`，避免泄露用户存在性。

### `Refresh`
- **签名**：`func (u *Usecase) Refresh(ctx, refreshToken string) (*TokenPair, error)`
- **职责**：校验 refresh token 并签发新对。
- **流程**：`signer.Verify` 解析 claims → `GetByID` → 校验 active → `issuePair`。
- **注释要点**：MVP 无 rotation / revocation list，仅靠签名有效期。
- **错误处理**：Verify 失败 / 用户不存在 / 状态非 active 均 `ErrUnauthorized`。

### `BootstrapAdmin`
- **签名**：`func (u *Usecase) BootstrapAdmin(ctx, email, password string) error`
- **职责**：首启管理员幂等 seed。
- **流程**：email/password 空则静默返回 → `Count` → >0 跳过并记录 info → 否则 hashPassword → 组装 User（DisplayName="admin"、Role=admin、IsSuperuser=true、Status=active）→ Create → 记录 info。

### `GetByID` / `List`
- **职责**：读取用户；返回前清空 PassHash。

### `Delete`
- **职责**：透传 `repo.Delete`。

### `SetRole`
- **签名**：`func (u *Usecase) SetRole(ctx, id uint64, role string) error`
- **职责**：更新 role 并同步 legacy `is_superuser` 列。
- **流程**：`IsValidRole` 校验 → `UpdateRole` → best-effort `UpdateSuperuser(role == admin)`。
- **错误处理**：Superuser 同步失败被 `_ =` 忽略（注释说明：drift 不破坏新流程，casbin middleware 仍读 is_superuser 故尽量对齐）。
- **注释要点**：May 2026 单一特权层级 pivot 后，admin ⇔ superuser 由本方法保持同步。

### `UpdateProfile` / `SetStatus` / `ResetPassword`
- **职责**：分别更新 display_name+phone、active/disabled、password hash。
- **流程**：均做输入校验（长度上限）后透传 repo。
- **错误处理**：超长 / 非法状态返回 `ErrInvalid`。

### `Create`
- **签名**：`func (u *Usecase) Create(ctx, in CreateInput) (*model.User, error)`
- **职责**：管理端创建用户（含完整 profile）。
- **流程**：email 规范化 + 非空校验 → role 默认化与校验 → email 冲突检查 → hashPassword → DisplayName 兜底为 email local-part → 组装 User（IsSuperuser = role==admin）→ Create → 清空 PassHash。
- **注释要点**：DisplayName 兜底分支覆盖绕过 SPA 的旧调用方，避免侧栏展示完整 email。

### `EnsureSuperuser`
- **签名**：`func (u *Usecase) EnsureSuperuser(ctx) error`
- **职责**：启动迁移——将 legacy admin 同步为 is_superuser=true，并为空 DisplayName 回填 email local-part。
- **流程**：`List` → 遍历：admin 且非 superuser 则 `UpdateSuperuser(true)` + 记录 info；空 DisplayName 则派生 local-part 并 `UpdateProfile`。
- **错误处理**：UpdateProfile 失败记录 warn 并 continue，不中断迁移。

### `issuePair`
- **签名**：`func (u *Usecase) issuePair(user *model.User) (*TokenPair, error)`
- **职责**：组装 claims 并签发 access/refresh。
- **流程**：构造 `auth.Claims`（UserID/Email/Role/IsSuperuser + Subject=ID）→ `signer.SignAccess` → `signer.SignRefresh` → 组装 TokenPair（ExpiresIn = AccessTTL/秒）。
- **错误处理**：任一签名失败包装返回。

## 5. 依赖关系

- **内部包**：`internal/iam/model`、`internal/pkg/auth`（Signer）、`internal/pkg/errs`、同包 `hashPassword`/`verifyPassword`
- **外部库**：`github.com/golang-jwt/jwt/v5`、`log/slog`、标准库
- **被调用方**：`internal/iam/service.Service`（持有 `*biz.Usecase`）

## 6. 并发与资源管理

无显式锁；Usecase 本身为无状态共享对象，依赖 repo 与 signer 的内部并发安全。所有方法首参 `context.Context`。

## 7. 设计模式与亮点

- **PassHash 永不出流**：Register/Create/GetByID/List 均在返回前显式 `user.PassHash = ""`，纵深防御。
- **登录防枚举**：用户不存在与密码错误统一返回 `ErrUnauthorized`。
- **单一特权层级 pivot**：May 2026 后 admin 与 superuser 合一，`SetRole` / `Create` 双写保持 legacy 列同步，`EnsureSuperuser` 启动迁移。
- **DisplayName 兜底**：表单缺省时取 email local-part，避免 UI 显示完整 email。
- **BootstrapAdmin 幂等**：基于 `Count > 0` 判断，保证仅首启生效。

## 8. 注意事项

- `SetRole` 中 `UpdateSuperuser` 失败被忽略，极端情况下 legacy 列漂移；casbin middleware 仍读该列，需监控告警。
- Refresh 无 revocation list，token 泄露在有效期内无法撤销；后续需引入黑名单或短 TTL + 长 refresh。
- `EnsureSuperuser` 的 DisplayName 回填对单条失败容忍（continue），可能留下少量未回填行，需审计日志核对。
- `issuePair` 中 `IsSuperuser` 直接来自 DB 列，若该列被旁路修改将影响 JWT claim；建议未来改为派生自 Role。
- `BootstrapAdmin` 在 `email/password` 为空时静默返回 nil，可能掩盖配置缺失；调用方需自行校验启动参数。
