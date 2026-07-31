# `model.go` 技术实现文档

> 源文件：`internal/iam/model/model.go`
> 包路径：`github.com/ongridio/ongrid/internal/iam/model`

## 1. 概述

本文件是 IAM BC 的持久化实体定义层，承载三张表：`users` / `orgs` / `org_memberships`。同时定义系统级角色常量（admin/user/viewer）、状态常量（active/disabled）、成员关系角色常量（org_admin/member/viewer），以及对应的合法性校验函数。包级注释清晰阐述了与 casbin 的职责分工：本包仅持有 HR 真值，casbin 是策略引擎。

## 2. 包信息

- **包名**：`model`
- **所属模块**：`internal/iam/model` —— IAM BC 的 model 层（实体定义）
- **依赖方向**：被 `internal/iam/biz/**`、`internal/iam/data/**`、`internal/iam/service`、`internal/iam/server` 共同依赖；仅依赖标准库 `time`

## 3. 关键类型与接口

```go
const (
	RoleAdmin  = "admin"
	RoleUser   = "user"
	RoleViewer = "viewer"
)

const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
)

const (
	MembershipRoleAdmin  = "org_admin"
	MembershipRoleMember = "member"
	MembershipRoleViewer = "viewer"
)

type User struct {
	ID          uint64 `gorm:"primaryKey;autoIncrement"`
	Email       string `gorm:"size:256;uniqueIndex"`
	PassHash    string `gorm:"size:512;column:pass_hash"`
	DisplayName string `gorm:"size:128;not null;default:'';column:display_name"`
	Phone       string `gorm:"size:32;not null;default:''"`
	Role         string    `gorm:"size:32;default:user;check:role IN ('admin','user','viewer')"`
	IsSuperuser  bool      `gorm:"not null;default:false;column:is_superuser"`
	Status       string    `gorm:"size:32;default:active;check:status IN ('active','disabled')"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type Org struct {
	ID          uint64 `gorm:"primaryKey;autoIncrement"`
	Name        string `gorm:"size:128;uniqueIndex;not null"`
	Description string `gorm:"size:512;not null;default:''"`
	ParentID    *uint64 `gorm:"column:parent_id;index"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type OrgMembership struct {
	ID        uint64    `gorm:"primaryKey;autoIncrement"`
	UserID    uint64    `gorm:"not null;column:user_id;uniqueIndex:idx_user_org,priority:1"`
	OrgID     uint64    `gorm:"not null;column:org_id;uniqueIndex:idx_user_org,priority:2"`
	Role      string    `gorm:"size:32;not null;check:role IN ('org_admin','member','viewer')"`
	CreatedAt time.Time
	UpdatedAt time.Time
}
```

- `User`：登录身份，PassHash 为 argon2id 编码；`IsSuperuser` 独立于 membership，代表系统管理员，casbin 不对其生效（middleware 短路）。
- `Org`：组织单元，可通过 `ParentID` 嵌套，但权限不沿层级继承。
- `OrgMembership`：用户与 org 的 N:M 关联，带 role；同一用户可在多个 org 拥有不同 role。
- 角色常量 + `IsValidRole` / `IsValidMembershipRole` / `RoleCanMutate`：集中定义角色语义。

## 4. 关键函数与流程

### `IsValidRole`
- **签名**：`func IsValidRole(r string) bool`
- **职责**：判断 r 是否为系统级角色常量（admin/user/viewer）。
- **被调用方**：`biz/user.SetRole` / `Create` / `Register` 拒绝非法角色。

### `RoleCanMutate`
- **签名**：`func RoleCanMutate(r string) bool`
- **职责**：判断角色是否可执行写操作（admin/user 可写，viewer 只读）。
- **注释要点**：viewer 是唯一返回 false 的角色。

### `IsValidMembershipRole`
- **签名**：`func IsValidMembershipRole(r string) bool`
- **职责**：判断 r 是否为成员关系角色常量（org_admin/member/viewer）。
- **被调用方**：`biz/membership.AddOrUpdate` 拒绝非法成员角色。

### `TableName` 方法
- 三个结构体均定义 `TableName() string`，显式 pin 表名（`users` / `orgs` / `org_memberships`），避免 GORM 默认复数化推断导致漂移。

## 5. 依赖关系

- **内部包**：无（仅标准库 `time`）
- **外部库**：无（GORM tag 是注释，不引入运行时依赖）
- **被调用方**：`internal/iam/biz/authz`、`biz/membership`、`biz/org`、`biz/user`、`data/membership/store`、`data/org/store`、`data/user/sqlite`、`service`、`server`

## 6. 并发与资源管理

无并发控制（纯类型定义）。

## 7. 设计模式与亮点

- **HR 真值与策略表分离**：包级注释明确「Casbin is the policy engine; this package only owns the HR-style truth」，单一真值来源原则。
- **角色常量集中化**：系统角色与成员角色常量同文件管理，配合 `IsValid*` 校验函数，避免散落字符串字面量。
- **GORM tag 表达 schema**：列名、size、默认值、唯一索引、CHECK 约束均通过 tag 声明，AutoMigrate 据此生成 DDL。
- **复合唯一索引**：`OrgMembership` 用 `uniqueIndex:idx_user_org,priority:1/2` 表达 (user_id, org_id) 联合唯一。
- **指针表达可空**：`Org.ParentID *uint64` 用指针表示 NULL，避免 zero-value 歧义。

## 8. 注意事项

- `User.Role` 与 `User.IsSuperuser` 双列并存是 legacy 兼容产物（May 2026 pivot 后单一特权层级），需通过 `EnsureSuperuser` 迁移保持同步；未来应考虑合并。
- `MembershipRoleViewer` 与 `RoleViewer` 同名「viewer」但分属不同列（系统级 vs 成员级），注释已提示不冲突；调用方需注意区分。
- `Org` 无外键约束（注释提及 sqlite 方言漂移），完整性由 biz 层 `org.Service` 保障；生产环境建议加监控检测 orphan。
- `User.PassHash` size:512 需容纳 argon2id 编码字符串，若未来切换算法需评估长度。
- CHECK 约束在 SQLite 内联于列定义，MySQL 为表级；`data/user/sqlite/migrate.go` 中有专门的 drop+recreate 处理。
- 角色枚举变更需同步：本文件常量 + `data/user/sqlite/migrate.go` 的 ADD CONSTRAINT + biz 层 `IsValidRole`。
