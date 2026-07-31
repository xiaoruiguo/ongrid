# `types.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/chatruntime/types.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/chatruntime`

## 1. 概述

本文件定义 chatruntime 包的全部核心类型：技能（Skill）、智能体（Agent）、加载结果（LoadResult/LoadWarning）、策略（Policy）、工具声明（ToolDecl）、凭证需求（CredentialRequirement/Inject/File）、技能元数据（SkillMetadata）、来源溯源（Provenance）、Ongrid 扩展（OngridExt）。当前阶段是 PR-2 scaffolding：仅类型 + 加载/解析逻辑，无 graph/LLM 接线——这些类型被 SkillRegistry / AgentRegistry / ComposeSystemPrompt / worker.go / LoadAll 共享，是整个 chatruntime 契约层的"通用语"。

## 2. 包信息

- **包名**：`chatruntime`
- **所属模块**：`internal/manager/biz/aiops/chatruntime`
- **依赖方向**：被同包的 skill_parser / skill_registry / agent_parser / agent_registry / load_all / plugin_container / system_prompt / runtime / worker 大量引用；不依赖任何外部业务包

## 3. 关键类型与接口

```go
type ToolClass string  // "read" | "write" | "execute" | "*"

const (
    ClassRead   ToolClass = "read"
    ClassWrite  ToolClass = "write"
    ClassExec   ToolClass = "execute"
    ClassAll    ToolClass = "*"
)

type Activation struct {
    Mode     string   // "always" | "keyword" | ""
    Keywords []string
}

type Policy struct {
    AllowedClasses []string  // 空 = 默认 read-only
}

type ToolDecl struct {
    Name, Class, Description string
    Parameters json.RawMessage
    WhenToUse string
}

type Skill struct {
    Name, Description, PromptBody string
    Activation Activation
    Tools []ToolDecl
    Metadata SkillMetadata
    UnknownFields map[string]any  // 前向兼容
    Dir, Source string  // Provenance
}

type Agent struct {
    Name, Description, SystemPrompt string
    WhenToUse string
    Source, Dir string
    Tools []string  // 白名单
    DisallowedTools []string  // 黑名单（black wins）
    PermissionMode string  // "" | "default" | "plan" | "acceptEdits"
    MaxTurns int  // 0 = 继承全局
    Model string
    CriticalReminder string
    InitialPrompt string
    Background bool
    OmitClaudeMd bool
    Metadata map[string]any
}

type Pack struct {
    ID, Root, ManifestPath string
    Skills []*Skill
    Agents []*Agent
    Commands []*claudecmd.Command
    Manifest *PluginManifest
}

type LoadResult struct {
    Skills []*Skill
    Agents []*Agent
    Packs []*Pack
    Warnings []LoadWarning
}

type LoadWarning struct {
    Path, Code, Reason string
}

type Provenance struct {
    Source string  // "disk" | "user" | "builtin" | plugin container
    Dir string
}

type SkillMetadata struct {
    Ongrid OngridExt
    // 其他未知 key 进入 Skill.UnknownFields
}

type OngridExt struct {
    Activation Activation  // metadata.ongrid.activation（冲突时优先）
    Visibility string
}

type CredentialRequirement struct {
    Name, Description string
    Required bool
    Inject []CredentialInject
    Files []CredentialFile
}

type CredentialInject struct {
    Env, Value string
}

type CredentialFile struct {
    Path, Content string
    Mode int
}

type Requires struct {
    Credentials []CredentialRequirement
}
```

`Policy.Allows(class ToolClass) bool`：空 `AllowedClasses` 默认只允许 `read`（安全降级）；含 `"*"` 全允许；否则按精确匹配。

## 4. 关键函数与流程

### `Policy.Allows`
- **签名**：`func (p Policy) Allows(class ToolClass) bool`
- **职责**：判定某个 ToolClass 是否被策略允许
- **流程**：
  1. `len(AllowedClasses) == 0` → 仅 `class == ClassRead` 返回 true（**空策略 = read-only**，安全默认）
  2. 遍历 AllowedClasses，命中 `"*"` 立即 true
  3. 命中 `string(class)` 返回 true
  4. 否则 false

### 常量与默认值
- **ToolClass 常量**：`ClassRead="read"`、`ClassWrite="write"`、`ClassExec="execute"`、`ClassAll="*"`
- **Agent 字段语义**：
  - `Tools` 白名单为空 = 继承全部
  - `DisallowedTools` 黑名单优先级高于白名单（black wins）
  - `PermissionMode` 仿 Claude Code（`default`/`plan`/`acceptEdits`）
  - `MaxTurns=0` 继承全局 `graph.Config.MaxIterations`
  - `CriticalReminder` 由 graph 层每轮重新注入 `<system-reminder>`
  - `Background` 控制是否 fire-and-forget 派生 worker
  - `OmitClaudeMd` 控制是否注入 CLAUDE.md（未来扩展）

## 5. 依赖关系

- **标准库**：`encoding/json`
- **同包**：`claudecmd`（Pack.Commands 字段引用 `*claudecmd.Command`）
- **被同包所有文件引用**：是 chatruntime 的"通用语"

## 6. 并发与资源管理

- 本文件全部为类型定义与一个纯函数 `Policy.Allows`，**无并发状态**
- `Skill.UnknownFields` / `Agent.Metadata` map 由解析路径写入、运行路径只读——线程安全前提是加载阶段完成后不再 mutation（SkillRegistry.Reload 整体替换切片保证这一点）

## 7. 设计模式与亮点

- **前向兼容**：`Skill.UnknownFields` 保留 YAML frontmatter 中所有非已知 key，注释明示"explicitly requires this so upstream schema additions don't break ongrid loading"——上游 schema 加字段不会让 ongrid 加载失败
- **冲突优先级**：`metadata.ongrid.activation` 与顶层 `activation` 冲突时前者胜（在 skill_parser.go 中实现，types.go 只是字段承载）
- **空策略 = read-only**：`Policy.Allows` 对空 `AllowedClasses` 默认只放行 read，安全默认优于"全放行"
- **black wins**：`Agent.DisallowedTools` 优先级高于 `Tools` 白名单——禁止项不可被白名单覆盖
- **Provenance 双字段**：`Source` + `Dir` 分开，`Source` 区分加载来源（disk/user/builtin/plugin），`Dir` 是文件系统路径——audit 与重载用 Dir，统计用 Source
- **Pack 聚合 Skills+Agents+Commands**：一个插件容器一次性聚合三类资源，LoadResult 同时持有扁平 Skills/Agents 列表与 Pack 分组——消费者可按需选择视图
- **CredentialRequirement 多形态**：`Inject`（env 变量）+ `Files`（写入文件）双轨，覆盖不同 SDK 的凭证加载习惯

## 8. 注意事项

- **PR-2 scaffolding only**：注释明示当前阶段无 graph/LLM 接线，只是类型与加载逻辑
- **`Agent.Tools` 空切片 vs nil**：空切片语义为"无白名单 = 继承全部"，nil 同义；`DisallowedTools` 同理
- **`MaxTurns=0` 不等于"禁止"**：0 是"继承全局"的哨兵值，全局默认 30
- **`PermissionMode` 当前未深度使用**：仿 Claude Code 设计，目前主要靠 `Tools`/`DisallowedTools` 控制；`PermissionMode` 留给未来 plan/acceptEdits 模式
- **`OngridExt` 仅 Activation 字段被使用**：`Visibility` 字段已声明但当前未消费，预留多租户/角色场景
- **`SkillMetadata` 不保留未知 key**：与 `Skill.UnknownFields` 不同，metadata 子结构仅强类型化已知字段；未知 metadata 子 key 会丢失（设计权衡：metadata 是 ongrid 私有命名空间，不容错）
- **`Pack.Manifest` 可为 nil**：`synthesizeBareSkillsPack` 合成的 Pack 没有 manifest，消费者需 nil-check
