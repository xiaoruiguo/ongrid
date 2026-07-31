# usecase.go

## 1. 概述

`usecase.go` 是 secret 包的核心用例文件 —— 凭证 vault 的 biz 层（HLD-017）。职责：
- **加密**：seal field map 后存储，unseal 仅用于进程内注入路径
- **脱敏**：list/get API 暴露字段 NAMES，永不暴露 values

`Usecase` 持有 `Repo`，对外暴露 Create / Update / Delete / List / `ResolveFields` / `ResolveInjection`。`ResolveFields` 返回解密 field map（进程内注入路径，永不序列化 over API）；`ResolveInjection` 用 type 的 inject rule 算 env vars。

## 2. 包信息

- 包名：`secret`
- 路径：`internal/manager/biz/secret`
- 包注释：明确加密 + 脱敏两大职责

## 3. 关键类型与接口

### Repo 接口

```go
type Repo interface {
    Create(ctx, *model.Secret) error
    Update(ctx, id uint64, data, description string) error
    Delete(ctx, id uint64) error
    List(ctx) ([]*model.Secret, error)
    GetByName(ctx, name string) (*model.Secret, error)
}
```

### View（脱敏 shape）

```go
type View struct {
    ID          uint64
    Name        string
    Type        string
    Description string
    FieldKeys   []string  // 字段名，无 values
    CreatedAt   time.Time
    UpdatedAt   time.Time
}
```

注释：返回 API 调用方的脱敏 shape —— 只字段名，无值。

### Usecase

```go
type Usecase struct{ repo Repo }
```

## 4. 关键函数与流程

### Create

```go
func (u *Usecase) Create(ctx, name, credType, description string, fields map[string]string) (*View, error)
```

1. `name` trim 非空校验
2. `credType` 空 → `CredTypeCustom`
3. `fields = clean(fields)` —— trim keys、丢空 key/value
4. `len(fields) == 0` → error
5. `sealed, err := seal(fields)` —— `json.Marshal` + `secretbox.Encrypt`
6. `model.Secret{Name, Type: credType, Data: sealed, Description}`
7. `repo.Create`
8. `toView(s, fields)` 返回脱敏 view

### Update

```go
func (u *Usecase) Update(ctx, id uint64, description string, fields map[string]string) error
```

- `fields != nil`：`clean` + 至少一字段 + `seal`
- `fields == nil`：仅改 description，`sealed = ""`
- `repo.Update(ctx, id, sealed, description)`

### Delete

委派 `repo.Delete`。

### List

```go
func (u *Usecase) List(ctx) ([]*View, error)
```

`repo.List` → 对每行 `unseal(s.Data)`（best-effort：解密失败仍 list 行，无 keys）→ `toView`。

### ResolveFields

```go
func (u *Usecase) ResolveFields(ctx, name string) (map[string]string, error)
```

`repo.GetByName` + `unseal(s.Data)`。注释：进程内注入路径 only，永不序列化 over API。

### ResolveInjection

```go
func (u *Usecase) ResolveInjection(ctx, name string) (map[string]string, []string, error)
```

1. `repo.GetByName` + `unseal`
2. `t := LookupCredType(s.Type)`
3. `t.IsCustom() || len(t.InjectEnv) == 0`：每个 field 作为同名 env var
4. 否则：`credbinect.Resolve(t.InjectEnv, nil, fields)` 返回 `(plan, missing, err)`，返回 `plan.Env` + `missing`

注释：返回 env map + type 规则引用但凭证缺的 field names。进程内 only。Skills 声明 OWN `slot.inject` 用 `ResolveFields` + `credbinect` 直接 —— per-skill mapping 优先。

### seal / unseal（helper）

```go
func seal(fields map[string]string) (string, error) {
    b, err := json.Marshal(fields)
    // ...
    return secretbox.Encrypt(string(b))
}

func unseal(data string) (map[string]string, error) {
    plain, err := secretbox.Decrypt(data)
    // ...
    json.Unmarshal([]byte(plain), &out)
    return out, nil
}
```

### clean（helper）

trim keys、丢空 key/value。

### toView（helper）

构造 `View`，`FieldKeys` 排序。

## 5. 依赖关系

### 外部包

- `context` / `encoding/json` / `fmt` / `sort` / `strings` / `time`

### 内部包

- `model "github.com/ongridio/ongrid/internal/manager/model/secret"` —— `Secret` 模型
- `github.com/ongridio/ongrid/internal/pkg/credbinect` —— `Resolve` 注入规划
- `github.com/ongridio/ongrid/internal/pkg/errs` —— `ErrInvalid`
- `github.com/ongridio/ongrid/internal/pkg/secretbox` —— `Encrypt` / `Decrypt`

### 被谁调用

- HTTP handler（`/v1/secrets/*`）调 CRUD + List
- `mcp/usecase.go` 的 `BuildClient` 调 `ResolveFields`（SecretResolver 接口）
- chatruntime 调 `ResolveInjection`（agent exec 时注入 env）+ `ResolveFields`（per-skill slot.inject）

## 6. 并发与资源管理

- **无锁**：`Usecase` 无共享可变状态；并发安全由 repo 保证
- **无 goroutine**：所有方法同步
- **`secretbox` 加密无状态**：每次 `Encrypt` / `Decrypt` 独立，无 session

## 7. 设计模式与亮点

### 加密 + 脱敏双职责

包注释明确：加密（seal field map 后存储，unseal 仅进程内注入）+ 脱敏（list/get API 暴露字段 NAMES，永不 values）。`View` 结构体只有 `FieldKeys`，无 values。

### ResolveFields vs ResolveInjection 分离

- `ResolveFields`：返回解密 field map，调用方自己决定怎么用（如 mcp 的 `expandHeaders`、skill 的 per-slot inject）
- `ResolveInjection`：用 type 的 inject rule 算 env vars，返回 env map + missing field names

注释：Skills 声明 OWN `slot.inject` 用 `ResolveFields` + `credbinect` 直接 —— per-skill mapping 优先。未声明的 skill 用 `ResolveInjection` 走 type 规则。

### Unknown type → custom 容错

`LookupCredType` 对 unknown name 返回 custom type。`ResolveInjection` 对 custom 走 field→env 路径。这让旧数据或 type 删除后仍能工作。

### List best-effort 解密

`List` 对每行 `unseal`，失败仍 list 行（无 keys）。注释：best-effort —— 解密失败不阻断 list，让 UI 仍能看到行存在。

### Update nil fields = 仅改 description

`Update` 的 `fields == nil` 表示仅改 description，`sealed = ""`。这让 `repo.Update` 实现区分"不改 data"与"改 data"。

### clean 丢空 key/value

`clean` trim keys、丢空 key/value。防 UI 误传空字段污染存储。

### toView FieldKeys 排序

`toView` 对 `FieldKeys` 排序。让 UI 渲染顺序稳定，不受 map 迭代顺序影响。

## 8. 注意事项

- **`ResolveFields` 永不 over API**：注释明确。仅进程内注入路径。HTTP handler 不应调它返回给前端
- **`ResolveInjection` 返回 missing**：type 规则引用但凭证缺的 field names。调用方应决定是否 warn（当前 chatruntime 不阻断）
- **`seal` 用 `secretbox.Encrypt`**：加密 key 由 `secretbox` 包管理（通常 env-seeded）。key 轮换需 re-encrypt 所有 credential
- **`List` best-effort 解密**：失败行无 keys。UI 应处理 `FieldKeys == nil` 情况
- **`Update` nil fields = 仅改 description**：`fields == nil` 与 `fields = {}` 不同。前者不改 data，后者会 error（"at least one field required"）
- **`clean` 丢空 value**：`v == ""` 的 field 被丢。若需存空值字段（如可选 region），UI 不应传空串
- **`View.FieldKeys` 排序**：UI 渲染顺序稳定，但与创建顺序可能不同
- **`Repo.GetByName` 是 key lookup**：name 应有 unique 约束。重复 name 由 repo 层 surface error
- **`secretbox.Decrypt` 失败**：`unseal` 返回 error。`List` 容错仍 list 行；`ResolveFields` / `ResolveInjection` 返回 error
