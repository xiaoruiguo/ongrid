# source.go

## 1. 概述

`source.go` 定义 marketplace 包的安装源值类型 —— 描述 pack 应从哪里拉取。它是 install API 请求体的 wire shape，也定义了 `Caller`（请求方身份），并对外暴露 `SourceLabel` / `SourceURL` 两个稳定诊断值（持久化到 `installed_skills.source` / `installed_skills.source_url`）。

文件包注释也在这里，描述了整个 marketplace 包的文件布局。

## 2. 包信息

- 包名：`marketplace`
- 路径：`internal/manager/biz/marketplace`
- 包注释：`Package marketplace is the manager-side application service for the skill marketplace install / list / uninstall workflow.`

## 3. 关键类型与接口

### SourceType

```go
type SourceType string

const (
    SourceTypeLocal    SourceType = "local"     // 已在 manager 主机的目录（admin scp 上来）
    SourceTypeTarball  SourceType = "tarball"   // curl + tar -xz 远程 .tgz
    SourceTypeGit      SourceType = "git"       // git clone --depth=1 (--branch=Ref)
    SourceTypeRegistry SourceType = "registry"  // 通过 registry 代理解析
)
```

字符串值既是 wire shape 又持久化到 DB。`registry` v1 只识别 `ongrid-official`，registry 代理实现在后续 PR；当前 manager 拒绝此类型除非调用方通过 Registry+URL 路由直接给 tarball URL（实际是带标签的 tarball install）。

### Source（wire shape）

```go
type Source struct {
    Type     SourceType
    Path     string  // local 用，绝对路径
    URL      string  // tarball / git 用
    Ref      string  // git 用，空 = HEAD
    Registry string  // registry 用，须在 AllowedSources 或 DevMode
    PackID   string  // registry 用，slug
    Version  string  // registry 用，semver
}
```

扁平结构（无嵌套 oneOf），与 openapi 风格一致。`Type` 决定哪个字段被读，其余忽略。

### Caller

```go
type Caller struct {
    UserID   uint64
    TenantID uint64
    Role     string
}

func (c Caller) IsAdmin() bool { return c.Role == "admin" }
```

HTTP handler 从 `tenantctx` 构造；biz 层用它做租户 scope + audit（`installed_by`）。镜像 `server/setting.caller` 形状但不导入它。

## 4. 关键函数与流程

### Source.SourceLabel

```go
func (s Source) SourceLabel() string {
    if s.Type == SourceTypeRegistry && s.Registry != "" {
        return s.Registry
    }
    return string(s.Type)
}
```

持久化到 `installed_skills.source`。registry install 用 registry 名（如 `ongrid-official`），其它用 source 类型字面量。稳定 wire shape。

### Source.SourceURL

```go
func (s Source) SourceURL() string {
    switch s.Type {
    case SourceTypeLocal:    return s.Path
    case SourceTypeTarball, SourceTypeGit: return s.URL
    case SourceTypeRegistry: return s.Registry + ":" + s.PackID + "@" + s.Version
    }
    return ""
}
```

持久化到 `installed_skills.source_url`，best-effort 人类可读的来源。registry 用 `name:packID@version` 形式。

## 5. 依赖关系

无外部/内部包依赖（纯值类型定义）。

### 被谁调用

- HTTP handler 解码 install 请求体为 `Source`，构造 `Caller`，调 `Usecase.Install`
- `usecase.go` 调 `src.SourceLabel()` / `src.SourceURL()` 持久化诊断字段
- `signature.go` 间接通过 `usecase` 的信任闸门使用 `src.SourceLabel()` 判断是否需签名

## 6. 并发与资源管理

不适用（纯值类型，无共享状态）。

## 7. 设计模式与亮点

### 扁平 wire shape 而非 oneOf

注释明确：`Source` 是扁平结构而非嵌套 oneOf，与 `internal/manager/server/integration` 的 openapi 风格一致。简化了客户端序列化与 Go 解码。

### SourceLabel / SourceURL 分离

`SourceLabel` 是稳定分类键（用于 `RequireSignedSources` 匹配、SPA 过滤），`SourceURL` 是人类可读诊断。两者分离让"标签"与"详情"各司其职。

### Caller 镜像 setting.caller

不直接 import `server/setting.caller` 而是镜像其形状。避免 biz 层反向依赖 server 层，符合 gospec 分层红线。

### IsAdmin 显式方法

`IsAdmin()` 而非导出 `Role` 比较让权限判断集中在类型方法，未来加 superadmin 等角色只需改一处。

## 8. 注意事项

- **SourceType 字符串值是稳定 wire shape**：不能随意改，会破坏已持久化的 `installed_skills.source` 行与前端解析
- **SourceTypeRegistry 当前半实现**：v1 只识别 `ongrid-official`，且需要调用方给 URL 才能走 tarball 路径。真正的 registry 代理在后续 PR
- **Source.Path 必须绝对路径**：`usecase.fetchToStaging` 对 local 类型强制 `filepath.IsAbs`，相对路径会被拒绝
- **Source.Ref 空 = HEAD**：git 类型空 ref 由 `usecase` 默认 HEAD，不是默认分支名
- **Caller.Role == "admin" 是硬编码**：`IsAdmin()` 写死 role 字符串。若引入 RBAC 框架需同步更新
- **Source 字段在 Type 不匹配时被忽略**：如 `Type=local` 时 URL 字段不读。客户端不应依赖此行为溢出存储
