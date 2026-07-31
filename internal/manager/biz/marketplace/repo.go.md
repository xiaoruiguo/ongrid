# repo.go

## 1. 概述

`repo.go` 定义 marketplace 包的持久化接口与对外契约 DTO。它不包含业务逻辑，只声明：
- `Repo` 接口：`usecase` 依赖的窄持久化面（具体实现在 `data/marketplace/store`）
- `SkillRegistry` / `AgentRegistry` 接口：`usecase` 用来热重载的注册表面
- 一组 wire-shape DTO：`CapabilityDeclaration` / `SkillCapabilityRecord` / `RequiresRecord` / `CredentialSlotRecord` / `CapabilitySummary` / `InstallResult` / `LoadWarning`

这些 DTO 是 SPA 安装确认对话框的渲染目标，每个字段都有对应的 UI 元素。

## 2. 包信息

- 包名：`marketplace`
- 路径：`internal/manager/biz/marketplace`
- 导入路径：`github.com/ongridio/ongrid/internal/manager/biz/marketplace`

## 3. 关键类型与接口

### Repo 接口

```go
type Repo interface {
    Create(ctx, *model.InstalledPack) error
    GetByPackID(ctx, tenantID, packID) (*model.InstalledPack, error)
    GetByManifestSHA(ctx, tenantID, sha) (*model.InstalledPack, error)
    List(ctx, tenantID) ([]*model.InstalledPack, error)
    DeleteSoft(ctx, tenantID, packID) error
    SetBindings(ctx, tenantID, packID, bindingsJSON) error
}
```

`tenantID=0` 是"所有租户"约定（admin 视图）。

### SkillRegistry / AgentRegistry 接口

```go
type SkillRegistry interface {
    Reload(skillsRoot string, extras ...string) error
}
type AgentRegistry interface {
    Reload(agentsRoot string, extras ...string) error
}
```

注释强调 `Reload` 必须原子：构建新切片后在写锁下 swap，飞行中的 chat 已调 `Resolve()` 的看到稳定快照。`*chatruntime.SkillRegistry` / `*chatruntime.AgentRegistry` 结构性满足这两个接口。

### CapabilityDeclaration（用户审批的能力快照）

JSON wire shape，存入 `installed_skills.capabilities_json`，也是 SPA 安装确认对话框的渲染源：

```go
type CapabilityDeclaration struct {
    PackID      string
    Version     string
    Skills      []SkillCapabilityRecord
    AgentCount  int                  // agent persona 数量（agent 无能力声明，只展示计数）
    Summary     CapabilitySummary    // 跨 skill 的去重并集
}
```

### SkillCapabilityRecord / RequiresRecord / CredentialSlotRecord

```go
type SkillCapabilityRecord struct {
    Name             string
    Scope            string                 // "manager" | "edge"
    EdgeCapabilities []map[string]any
    Requires         RequiresRecord
    ToolClasses      []string
}

type RequiresRecord struct {
    Bins        []string
    Config      []string
    Credentials []CredentialSlotRecord
}

type CredentialSlotRecord struct {
    Slot   string
    Label  string
    Fields []string
    // 注意：inject template 故意丢弃 —— 操作员只需要 slot key + label + 字段名
    // 来选凭证；注入在 exec 时由 bound credential 的 TYPE 解析
}
```

### CapabilitySummary

```go
type CapabilitySummary struct {
    ToolClasses      []string
    Bins             []string
    ConfigKeys       []string
    CredentialSlots  []CredentialSlotRecord  // 跨 skill 去重的 slot 集合
}
```

是 `SkillCapabilityRecord` 集合的去重并集，渲染成对话框的单 bullet 列表（"• 网络访问: ..." / "• 可执行二进制: etcdctl"）。

### InstallResult / LoadWarning

```go
type InstallResult struct {
    Pack         *model.InstalledPack
    Capabilities CapabilityDeclaration
    Warnings     []LoadWarning
}

type LoadWarning struct {
    Path   string
    Reason string
    Code   string
}
```

`LoadWarning` 镜像 `chatruntime.LoadWarning`，避免 chatruntime import 泄漏到 biz 边界外。JSON shape 一致。

## 4. 关键函数与流程

本文件**无函数实现**，纯类型/接口声明。

## 5. 依赖关系

### 内部包

- `model "github.com/ongridio/ongrid/internal/manager/model/marketplace"` —— `InstalledPack` 模型

### 被谁实现

- `Repo`：`data/marketplace/store` 包
- `SkillRegistry` / `AgentRegistry`：`biz/aiops/chatruntime` 包

### 被谁调用

- `usecase.go` 的 `Usecase` 结构持有 `Repo` / `SkillRegistry` / `AgentRegistry`
- HTTP handler 通过 `InstallResult` 把安装结果序列化给 SPA
- SPA 安装确认对话框消费 `CapabilityDeclaration` / `CapabilitySummary`

## 6. 并发与资源管理

不适用（纯声明）。

## 7. 设计模式与亮点

### 窄接口 + 结构性满足

`SkillRegistry` / `AgentRegistry` 是窄接口（仅一个 `Reload` 方法），`chatruntime.SkillRegistry` 不需要 `impl` 声明就结构性满足。这符合 gospec "接口在消费方定义"的红线。

### DTO 与 chatruntime 解耦

`LoadWarning` / `CapabilityDeclaration` 都是 `chatruntime` 类型的镜像/重组，刻意不直接 re-export chatruntime 类型。这样 HTTP wire shape 不会因为 chatruntime 内部重构而破坏，且 chatruntime import 不穿越 biz 边界。

### 字段命名对应 UI 元素

注释明确每个字段对应 SPA 对话框的哪个渲染目标（`Summary.ToolClasses` → "• 网络访问 / 文件读取"bullet；`Summary.Bins` → "• 可执行二进制: etcdctl"；`CredentialSlots` → binding 对话框每个 slot 一行）。这是产品驱动的 DTO 设计。

### inject template 故意丢弃

`CredentialSlotRecord` 注释解释：inject template 在 exec 时由 bound credential 的 TYPE 解析，操作员在 binding UI 只需要 slot key + label + 字段名。把 template 也带出来会让 UI 复杂化且无价值。

## 8. 注意事项

- **`Reload` 必须原子**：实现方若不原子，飞行中的 chat 会看到半切换状态。新增实现必须遵守
- **`tenantID=0` 是 admin 全租户约定**：`List(ctx, 0)` 返回所有租户的行。非 admin 调用方应传自己的 tenantID
- **`DeleteSoft` 是软删**：`idx_tenant_pack` 索引让软删行不再占用 live (tenant_id, pack_id) slot，因此 reinstall-after-uninstall 直接 `Create` 即可
- **`SetBindings` 整体替换**：bindings map 是 wholesale replace，不是 merge
- **`LoadWarning.Code` 是程序化分类键**：`usecase.go` 用 `"escapes_root"` code 阻塞安装，新增 code 要同步检查 usecase 的拒绝逻辑
- **`CapabilityDeclaration.Summary` 由 usecase 构造**：不是从 pack manifest 直接读，是 `buildCapabilityDeclaration` 跨 skill 去重并集的结果
