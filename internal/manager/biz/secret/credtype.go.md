# credtype.go

## 1. 概述

`credtype.go` 实现 secret 包的可复用凭证类型层（HLD-017，n8n 的 ICredentialType 类比）。一个 type 声明：
- (a) 该类型凭证持有哪些字段
- (b) 这些字段如何注入到 skill/MCP exec 环境

注入规则在 TYPE 上定义一次复用 —— 关键是这让**未声明**的 skills.sh skill 也能收凭证：操作员 attach typed credential，type 的 inject rule 适用，即使 skill 本身不声明。

内置类型在 `init()` 注册（custom / tencentcloud / aws / alicloud / github）；集合可扩展（pack 可 ship 更多）。特殊 `"custom"` 类型无固定字段与 inject rule —— 每个字段成为同名 env var（"我只需设一些 env var"的逃生舱）。

## 2. 包信息

- 包名：`secret`
- 路径：`internal/manager/biz/secret`
- 文件注释：明确 HLD-017 + n8n ICredentialType analog + 未声明 skill 也能收凭证的设计目标

## 3. 关键类型与接口

### CredField

```go
type CredField struct {
    Key    string `json:"key"`
    Label  string `json:"label"`
    Secret bool   `json:"secret"`  // UI mask（passwords/keys）；false for region 等
}
```

### CredType

```go
type CredType struct {
    Name      string            `json:"name"`        // machine name（Secret.Type 引用）
    Label     string            `json:"label"`       // display name
    Fields    []CredField       `json:"fields"`      // create dialog form schema
    InjectEnv map[string]string `json:"inject_env,omitempty"`  // ENV_VAR -> "{{field}}" template；空 for custom
    Builtin   bool              `json:"builtin"`
}
```

### CredTypeCustom

```go
const CredTypeCustom = "custom"
```

untyped 逃生舱。

### 包私有 registry

```go
var credTypes = map[string]*CredType{}
func registerCredType(t *CredType) { credTypes[t.Name] = t }
```

## 4. 关键函数与流程

### IsCustom

```go
func (t *CredType) IsCustom() bool { return t.Name == CredTypeCustom }
```

### LookupCredType

```go
func LookupCredType(name string) *CredType
```

返回 type 或 nil。注释：unknown/empty name 解析为 "custom" type，让 untyped credential 仍 inject（field→env）。

### AllCredTypes

```go
func AllCredTypes() []*CredType
```

返回所有注册 type（给 create-credential UI）。

### init() 注册内置类型

```go
func init() {
    registerCredType(&CredType{Name: CredTypeCustom, Label: "自定义 (Custom)", Builtin: true})
    registerCredType(&CredType{
        Name: "tencentcloud", Label: "腾讯云 (Tencent Cloud)", Builtin: true,
        Fields: []CredField{
            {Key: "secret_id", Label: "SecretId", Secret: true},
            {Key: "secret_key", Label: "SecretKey", Secret: true},
            {Key: "region", Label: "Region (可选)"},
        },
        InjectEnv: map[string]string{
            "TENCENTCLOUD_SECRET_ID":  "{{secret_id}}",
            "TENCENTCLOUD_SECRET_KEY": "{{secret_key}}",
            "TENCENTCLOUD_REGION":     "{{region}}",
        },
    })
    // aws / alicloud / github 同理
}
```

每个 type 的 `InjectEnv` map ENV_VAR → `"{{field}}"` template。`ResolveInjection` 用 `credbinect.Resolve` 展开。

## 5. 依赖关系

### 外部包

无（纯数据类型 + map registry）。

### 被谁调用

- `usecase.go` 的 `ResolveInjection` 调 `LookupCredType(s.Type)` 取 type，再用 `credbinect.Resolve(t.InjectEnv, nil, fields)` 展开
- HTTP handler 调 `AllCredTypes` 给 create-credential UI 渲染类型选择

## 6. 并发与资源管理

- **`credTypes` map 写仅在 `init()`**：启动时注册内置类型，之后只读。并发安全（init 完成后无写）
- **`init()` 仅做注册**：符合 gospec 红线"init() 仅允许做注册，禁止做 IO 或可能 panic"
- 无锁、无 goroutine

## 7. 设计模式与亮点

### 注入规则在 TYPE 上定义一次

注释：inject rule 在 type 上定义一次复用。关键设计 —— 让未声明的 skills.sh skill 也能收凭证。操作员 attach typed credential，type 的 inject rule 适用，即使 skill 本身不声明 `requires.credentials`。

### Custom 逃生舱

`"custom"` 类型无固定字段与 inject rule —— 每个字段成为同名 env var。注释："I just need to set some env vars"的逃生舱。这让无 type 适配的凭证仍可注入。

### Unknown → custom 容错

`LookupCredType` 对 unknown/empty name 返回 custom type 而非 nil。注释：让 untyped credential 仍 inject。这让旧数据或 type 删除后仍能工作。

### Secret 字段标记

`CredField.Secret = true` 标记 UI mask（passwords/keys）；false for region 等。这是 UI 渲染提示，不影响存储（所有字段都 `seal` 加密）。

### Builtin 标记

`CredType.Builtin = true` 标记 ongrid 自带（vs pack-supplied）。UI 可区分显示。当前所有内置 type 都 Builtin=true。

### InjectEnv template 语法

`"{{field}}"` template。`credbinect.Resolve` 展开。与 mcp 包的 `expandHeaders` 同款语法，保持一致。

### init() 注册模式

`init()` 注册内置 type，符合 gospec 红线"init() 仅做注册"。pack 未来 ship 更多 type 时可调 `registerCredType`（但需在 init 后，可能需同步保护）。

## 8. 注意事项

- **`credTypes` 是包私有 map**：外部只能通过 `LookupCredType` / `AllCredTypes` 读。直接改 map 不安全
- **`init()` 注册顺序**：custom 先注册，其它后注册。若同名 type 重复注册，后注册覆盖 —— pack 慎用
- **`LookupCredType` unknown → custom**：调试时若 type 行为异常，先查 `Secret.Type` 是否拼写正确。unknown 静默回退 custom 可能掩盖 bug
- **`InjectEnv` template 语法 `{{field}}`**：与 mcp 包 `expandHeaders` 一致。`credbinect.Resolve` 实现
- **`Custom` type 无 `Fields`**：操作员自定义。`ResolveInjection` 对 custom 走 field→env 路径
- **`CredField.Secret` 仅 UI 提示**：不影响存储加密。所有字段都 `seal` 加密
- **pack 添加 type 未实现**：注释提到"pack 可 ship 更多"，但当前无 pack 注册 type 的机制。未来需扩展
- **类型名是稳定 wire shape**：`"tencentcloud"` / `"aws"` 等是 DB 持久化值。改值需 migration
