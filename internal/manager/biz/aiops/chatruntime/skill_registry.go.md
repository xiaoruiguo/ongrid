# `skill_registry.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/chatruntime/skill_registry.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/chatruntime`

## 1. 概述

本文件实现 `SkillRegistry`：持有所有已加载 Skill + 非致命 warnings，提供 Load / Reload / All / Warnings / Add / AddAll / AddWarnings / Resolve。是技能系统的运行时注册中心。并发模型：`sync.RWMutex` 保护内部切片；Reload 在锁外构建新切片后 O(1) 原子替换；All/Warnings/Resolve 返回值拷贝，已返回快照不受后续 mutation 影响。Resolve 是核心查询路径：activation 过滤 + policy 工具类过滤双闸门。

## 2. 包信息

- **包名**：`chatruntime`
- **所属模块**：`internal/manager/biz/aiops/chatruntime`
- **依赖方向**：被 runtime.go / worker.go / marketplace usecase 调用；调用 load_all.go 的 LoadAll、types.go 的 Skill/Policy/Activation/ToolDecl

## 3. 关键类型与接口

```go
type SkillRegistry struct {
    mu       sync.RWMutex
    skills   []*Skill
    warnings []LoadWarning
}

func NewSkillRegistry() *SkillRegistry
```

## 4. 关键函数与流程

### `Load`
- **签名**：`func (r *SkillRegistry) Load(skillsRoot string) error`
- **职责**：从 skillsRoot 加载所有 skills + warnings
- **流程**：
  1. `LoadAll(LoadAllConfig{SkillsRoot: skillsRoot})`
  2. 锁内 `r.skills = append([]*Skill(nil), res.Skills...)`（值拷贝切片头）
  3. 锁内 `r.warnings = append([]LoadWarning(nil), res.Warnings...)`
- **错误处理**：LoadAll 失败 `%w` 包装；不存在的 root 不是错误（LoadAll 内部容忍）

### `Reload`
- **签名**：`func (r *SkillRegistry) Reload(skillsRoot string, extras ...string) error`
- **职责**：热重载——marketplace install/uninstall 后无需重启刷新 skills
- **流程**：
  1. `LoadAll(LoadAllConfig{SkillsRoot: skillsRoot, ExtraSkillsRoots: extras})`
  2. **锁外构建** `newSkills` + `newWarnings`（避免持锁期间长 IO）
  3. 锁内 `r.skills = newSkills; r.warnings = newWarnings`（O(1) 替换）
- **原子性**：注释明示"builds the new slice outside the lock, then swaps under r.mu.Lock() in O(1)"
- **空 root 清空**：skillsRoot="" 行为同 Load("")，清空 registry

### `All` / `Warnings`
- **签名**：`func (r *SkillRegistry) All() []*Skill` / `Warnings() []LoadWarning`
- **职责**：返回内部切片的**值拷贝**
- **流程**：RLock → `make + copy` → 返回新切片
- **不变量**：caller 拿到的切片不受后续 Reload 影响

### `Add` / `AddAll` / `AddWarnings`
- **签名**：`Add(sk *Skill)` / `AddAll(skills []*Skill)` / `AddWarnings(ws []LoadWarning)`
- **职责**：编程方式插入 skill / 批量插入 / 追加 warnings
- **用途**：feature-flag gated skills（无 SKILL.md 文件）、LoadAll 合并 plugin-container 输出
- **AddAll**：nil 跳过；逐个调 Add（非批量加锁，简单实现）

### `Resolve`（核心查询）
- **签名**：`func (r *SkillRegistry) Resolve(query string, policy Policy) []*Skill`
- **职责**：按 (query, policy) 双过滤返回激活且策略允许的 skills（copies，Tools 已过滤）
- **流程**：
  1. RLock → `copy(skills, r.skills)` → RUnlock（拷贝指针切片，快）
  2. queryLower = `strings.ToLower(strings.TrimSpace(query))`
  3. 遍历每个 skill：
     - `!activationMatches(sk.Activation, queryLower)` → 跳过
     - `filtered := filterToolsByPolicy(sk.Tools, policy)`
     - `len(sk.Tools) > 0 && len(filtered) == 0` → 跳过（skill 有工具但全被 policy 干掉）
     - `clone := *sk`（浅拷贝 Skill）+ `clone.Tools = filtered` → append
  4. 返回 out
- **不变量**：原 registry skills 不被 mutation；返回的 clone 是独立副本

### `activationMatches`
- **签名**：`func activationMatches(a Activation, queryLower string) bool`
- **职责**：判定 skill activation 是否匹配 query
- **流程**：
  - mode `""` / `"always"` → true
  - mode `"keyword"` → 任一 keyword（lowercased）被 queryLower contains 即 true
  - 未知 mode → **fail open**（true），注释明示"parse-time warning is the place to surface this"
- **keyword 空字符串跳过**：避免空 keyword 误匹配

### `filterToolsByPolicy`
- **签名**：`func filterToolsByPolicy(tools []ToolDecl, policy Policy) []ToolDecl`
- **职责**：按 policy 过滤工具列表
- **流程**：
  - 空 tools → nil
  - 每个 tool：`class = t.Class; class=="" → ClassRead`（**空 class 默认 read**）
  - `!policy.Allows(class)` → 丢弃
- **安全默认**：空 class 视为 read-only

### `pathHasPrefix`
- **签名**：`func pathHasPrefix(child, parent string) bool`
- **职责**：判断 child 是否在 parent 之内（含自身）
- **流程**：`filepath.Clean` 双方 → 相等 true；`filepath.Rel(parent, child)` 不以 `..` 开头则 true
- **用途**：注释明示"Mirrors internal/skill/loader.go for consistency"

## 5. 依赖关系

- **标准库**：`fmt`、`path/filepath`、`strings`、`sync`
- **同包**：`load_all.go`（LoadAll / LoadAllConfig / LoadResult）、`types.go`（Skill / LoadWarning / Policy / Activation / ToolDecl / ClassRead）
- **被调用方**：runtime.go（Resolve + ComposeSystemPrompt）、worker.go（Resolve）、marketplace usecase（Reload）、config_confirm.go

## 6. 并发与资源管理

- **`sync.RWMutex`**：保护 `skills` + `warnings` 两个切片
- **Reload 锁外构建**：避免持锁期间长 IO（LoadAll 含大量磁盘 IO），仅最终赋值在锁内 O(1)
- **All/Warnings/Resolve 值拷贝**：caller 拿到的切片独立，不受后续 Reload 影响——in-flight chat 用旧快照跑完
- **AddAll 非批量加锁**：逐个 Add，每次独立 Lock/Unlock——简单实现，性能可接受（AddAll 仅 boot 期调用）

## 7. 设计模式与亮点

- **RWMutex + 值拷贝快照**：读多写少场景的经典模式；Reload 是唯一写路径，All/Warnings/Resolve 全是读
- **锁外构建 + 锁内 O(1) 替换**：Reload 不持锁做 IO，避免阻塞 in-flight Resolve
- **Resolve 返回 copies**：浅拷贝 Skill + 替换 Tools 切片——caller 修改不影响 registry
- **activation fail open**：未知 mode 默认 true，避免解析期 warning 之外再叠加运行期静默禁用
- **filterToolsByPolicy 空 class 默认 read**：安全默认，与 Policy.Allows 空 AllowedClasses 默认 read-only 对称
- **"有工具但全被 policy 干掉则丢弃 skill"**：Resolve 流程第 3 步的不变量——避免返回空 Tools 的 skill 误导 LLM
- **Reload variadic extras**：marketplace install 时传 (builtinRoot, userRoot, newlyInstalledPack)，保证 image-baked built-in skills 不丢
- **pathHasPrefix 跨包一致性**：注释明示与 internal/skill/loader.go 镜像，保持路径判断逻辑全项目一致

## 8. 注意事项

- **`All()` 返回 `[]*Skill`**：切片本身是拷贝，但内部 `*Skill` 指针仍指向 registry 内部对象——caller 不应修改 Skill 字段（虽然 Resolve 返回的是 clone，All 不是）
- **`AddAll` 逐个加锁**：性能非最优，但仅 boot 期调用，可接受
- **`activationMatches` 未知 mode fail open**：注释建议 parse-time warning 是 surfacing 的地方，但当前 ParseSkillMd 未对未知 mode 产生 warning
- **`filterToolsByPolicy` 空 class 默认 read**：与 `Policy.Allows` 空 AllowedClasses 默认 read-only 对称，但两者独立实现，修改时需同步
- **`Reload` 空root 清空**：skillsRoot="" 会清空 registry，caller 需注意不要误传空字符串
- **`pathHasPrefix` 未被本文件使用**：注释说 mirror internal/skill/loader.go，实际是给同包其他文件（如 plugin_container.go）调用
- **`Resolve` 不缓存**：每次调用都重新过滤，热路径（每轮 chat）调用——当前 skill 数量小可接受，未来可考虑 query→skills 缓存
