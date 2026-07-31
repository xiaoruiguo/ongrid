# `inventory_bridge.go` 技术实现文档

> 源文件：`internal/manager\biz\aiops\tools\inventory_bridge.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools`

## 1. 概述

本文件是 `skill_bridge.go` 的反向桥：**aiops BaseTool bag → skill registry**，让 `/skills` 页面看到每个 cloud-side 工具作为 inventoried capability（带 audit + class gate）。背景：manager-side LLM 工具集有两个并行 population——(1) ScopeHost skills（host_probe_http 等）declarative metadata 自动路由；(2) Hand-written BaseTools（correlate_incident 等）JSON-schema'd manager-side 执行，**不在 /skills 页面**。Operator 无法知道 AI agent 有哪些 cloud-side 能力。本桥修复：每个 BaseTool 生成匹配的 skill 注册（`Scope=ScopeManager`），opt-in `RawSchemaProvider` 保留 hand-written JSON Schema verbatim（不做 ParamSchema down-conversion）。**Bridge direction**：遍历 BaseTool bag（production surface fed to LLM），名字已存在为 skill 的（由 `skill_bridge` 从 skill 侧带入）跳过避免 double-counting。

## 2. 包信息

- **包名**：`tools`
- **所属模块**：`internal/manager/biz/aiops/tools`
- **依赖方向**：被 `Registry.RegisterBaseToolsAsSkills` 调用（cmd/main.go 在 `BuildBaseTools` + `AppendHostFilesTools` 之后调）；依赖 `aiopstoolsbase`（`basetool` 包，`BaseTool` / `ToolInfo`）、`skillcore`（`internal/skill`，`Metadata` / `Class` / `Scope` / `Register` / `Get`）。**反向**于 `skill_bridge.go`（skill registry → aiops Tool registry）。

## 3. 关键类型与接口

```go
// 把 aiops BaseTool 包装成 skill.Executor。JSONSchema() 实现
// skill.RawSchemaProvider，保留 BaseTool 的 hand-crafted schema。
type baseToolSkillExecutor struct {
    meta   skillcore.Metadata
    schema json.RawMessage
    tool   aiopstoolsbase.BaseTool
}

func (b *baseToolSkillExecutor) Metadata() skillcore.Metadata
func (b *baseToolSkillExecutor) JSONSchema() json.RawMessage
func (b *baseToolSkillExecutor) Execute(ctx, params json.RawMessage) (json.RawMessage, error)
```

辅助函数：`classifyToolClass(info) skillcore.Class`、`classifyToolCategory(name) string`、`nonEmptyOrFallback(s, fallback)`、`isLowerSnake(s) bool`。

## 4. 关键函数与流程

```go
func (r *Registry) RegisterBaseToolsAsSkills(bag *ToolBag, log *slog.Logger)
func (b *baseToolSkillExecutor) Execute(ctx, params) (json.RawMessage, error)
func classifyToolClass(info *aiopstoolsbase.ToolInfo) skillcore.Class
func classifyToolCategory(name string) string
func isLowerSnake(s string) bool
```

**`RegisterBaseToolsAsSkills` 流程**：
1. `bag == nil` → 返回；`log == nil` → `slog.Default()`。
2. 遍历 `bag.AllTools()`：
   - `t.Info(ctx)` 取 metadata；err / nil / 空 Name → skip。
   - `skillcore.Get(info.Name)` 已存在 → skip（避免 double-counting，skill_bridge 带入的跳过）。
   - `!isLowerSnake(info.Name)` → warn log + skip（防 `skillcore.Register` panic）。
   - 构造 `desc`：`info.Description` +（若 `WhenToUse` 非空）`"\n\nWhen to use:\n" + WhenToUse`。
   - 构造 `meta = skillcore.Metadata{Key, Name, Description: nonEmptyOrFallback(desc, name), Class: classifyToolClass(info), Scope: ScopeManager, Category: classifyToolCategory(name), ResultPreview: ""}`。
   - `meta.Validate()` 失败 → warn + skip（robustness > strict invariants）。
   - `skillcore.Register(&baseToolSkillExecutor{meta, info.Parameters, t})`。
   - `registered++`。
3. log Info "BaseTools registered as skills" with registered/skipped counts。

**`baseToolSkillExecutor.Execute` 流程**：
1. `args := string(params)`，空则 `"{}"`。
2. `b.tool.InvokableRun(ctx, args)` → `out string, err`。err 上抛。
3. `json.Valid([]byte(out))` → 直接 `json.RawMessage(out)`。
4. 否则 wrap `{"raw": out}`（defensive belt，防 /skills page renderer choke）。

**`classifyToolClass`**：`info.Class == "write"` → `ClassMutating`；其他（含 "read" / "destructive" / 空）→ `ClassSafe`。注释明示 "AgentTool / SendMessage / TaskStop are read-shaped (no edge mutation) and stay Safe. Conservative default: Safe."（注意：`destructive` 也被归 Safe，保守默认）。

**`classifyToolCategory`**：按 name 字符串嗅探分组到 "shell" / "system" / "filesystem" / "diagnostic" / "telemetry" / "agent" / "process" / "other"，unknown → "agent"。

**`isLowerSnake`**：仅 `[a-z0-9_]` 合法，防 `skillcore.Register` panic。

## 5. 依赖关系

- **aiopstoolsbase**（`basetool` 包）：`BaseTool` 接口（`Info` / `InvokableRun`）、`ToolInfo`。
- **skillcore**（`internal/skill`）：`Metadata` / `Class`（`ClassSafe` / `ClassMutating`）/ `Scope`（`ScopeManager`）/ `Register(executor)` / `Get(key) (_, exists)`。`RawSchemaProvider` 接口（`JSONSchema() json.RawMessage`）让 bridge 保留 hand-crafted schema。
- **ToolBag**：`AllTools() []BaseTool`。
- 不依赖 alertbiz / devicebiz / edgebiz / prom / log / trace / tunnel。

## 6. 并发与资源管理

- **无锁**：`RegisterBaseToolsAsSkills` 是 boot-time 一次性注册，不持有运行时可变状态。`skillcore.Register` 自身负责并发安全（若 skill registry 是线程安全的）。
- **无 goroutine**：同步遍历 + 注册。
- **`baseToolSkillExecutor.Execute` 无状态**：每次调用都代理到底层 `b.tool.InvokableRun`，工具自身负责并发安全。

## 7. 设计模式与亮点

- **反向桥（inverse of skill_bridge）**：`skill_bridge` 是 skill registry → aiops Tool registry（LLM 看到 skills 为 function-calling tools）；`inventory_bridge` 是 aiops BaseTool bag → skill registry（/skills 页面看到每个 cloud-side 工具）。两桥方向相反，互补。
- **`RawSchemaProvider` opt-in**：BaseTool 的 hand-crafted JSON Schema（含 `device_ids[]` / nested objects 等 declarative form 无法表达的形状）原样保留，不做 ParamSchema down-conversion。这是 inventory 功能的关键——保留 LLM-facing schema verbatim。
- **skip 避免双重注册**：`skillcore.Get(info.Name)` 已存在则跳过。skill_bridge 从 skill 侧带入的工具（如 ScopeHost skills）不会被重复注册。
- **robustness > strict invariants**：bad metadata（空 Info / non-snake key / Validate 失败）skip with warning 而非 panic。inventory 功能不应因单个工具 metadata 问题拖垮整个 boot。
- **`Class` 保守映射**：`Class="write"` → `ClassMutating`；其他（含 `destructive`）→ `ClassSafe`。注释明示 conservative default——`destructive` 也归 Safe 是因为 inventory 不需要审批语义，只需粗分类。
- **`Category` 字符串嗅探**：`classifyToolCategory` 按 name 分组到 8 个 bucket，unknown → "agent"。让 /skills 页面按 category 分组展示。
- **`Description` + `WhenToUse` 拼接**：skill metadata 的 Description 包含 BaseTool 的 WhenToUse，让 /skills 页面看到完整路由提示。
- **`Execute` defensive belt**：`json.Valid` 校验 BaseTool 返回，非 JSON 则 wrap `{"raw": out}`。防 BaseTool 返回非 JSON 字符串时 /skills page renderer choke。

## 8. 注意事项

- **调用时机**：必须在 `BuildBaseTools` + `AppendHostFilesTools` 之后调用，确保 bag 持有完整 production tool set。注释明示 "Call this AFTER BuildBaseTools + AppendHostFilesTools"。
- **idempotent**：`skillcore.Get` 已存在则 skip，可重复调用。但若 BaseTool 集合变化（新增/移除），本桥不会自动同步——需重新调用 `RegisterBaseToolsAsSkills`。
- **`classifyToolClass` 保守**：`destructive` 归 `ClassSafe` 是有意——inventory 不需要审批语义。若 /skills 页面需展示 destructive 风险，需扩展 `classifyToolClass` 区分 "destructive"。
- **`classifyToolCategory` 字符串嗅探**：硬编码 name → category 映射，新增工具需手动加 case。unknown 落 "agent" bucket。
- **`isLowerSnake` 防 panic**：`skillcore.Register` 对非法 key 会 panic，本函数 pre-check 防 boot 崩溃。若 BaseTool name 含 `-` 或大写，会被 skip。
- **`baseToolSkillExecutor.Execute` 代理 `InvokableRun`**：BaseTool 必须能无 `opts` 调用（`Execute` 不传 opts）。若 BaseTool 依赖 `InvokeOption`（如 `tenant_bind` / `review_gate` 装饰器的 ctx value），从 /skills 页面直接调用会缺这些 ctx value——可能导致行为异常。**本桥主要用于 inventory 展示，不期望用户从 /skills 页面直接执行 BaseTool**。
- **`ResultPreview` 留空**：BaseTool 结果形状异构，无法统一 preview。/skills 页面不展示 result preview。
- **依赖 `skillcore`**：跨 `internal/manager/biz/aiops/tools` → `internal/skill`。这是允许的（skill 是 shared 基础设施），但需注意不引入反向依赖。
