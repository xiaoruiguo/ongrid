# `load_all.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/chatruntime/load_all.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/chatruntime`

## 1. 概述

本文件实现统一加载器 `LoadAll`：递归 walk skills root + agents root + 任意 extras roots，识别 SKILL.md / 松散 .md / 插件容器（.claude-plugin/、openclaw.plugin.json），把所有 skills + agents + packs + warnings 聚合成一个 `LoadResult`。是 SkillRegistry.Load / SkillRegistry.Reload / AgentRegistry.Load / LoadPluginContainer 的共同后端。设计目标：单一入口、symlink 安全、plugin 容器 SkipDir 防双加载、empty root 不报错。

## 2. 包信息

- **包名**：`chatruntime`
- **所属模块**：`internal/manager/biz/aiops/chatruntime`
- **依赖方向**：被 SkillRegistry / AgentRegistry / 插件容器加载路径调用；调用 plugin_container.go 的 LoadPluginContainer / DetectContainer / synthesizeBareSkillsPack、skill_parser.go 的 ParseSkillMd、agent_parser.go 的 ParseAgentMd

## 3. 关键类型与接口

```go
type LoadAllConfig struct {
    SkillsRoot        string   // 主 skills 根
    AgentsRoot        string   // 主 agents 根
    ExtraSkillsRoots  []string // 额外 skills 根（marketplace 热重载用）
    ExtraAgentsRoots  []string // 额外 agents 根
}

type LoadResult struct {
    Skills   []*Skill
    Agents   []*Agent
    Packs    []*Pack
    Warnings []LoadWarning
}
```

## 4. 关键函数与流程

### `LoadAll`
- **签名**：`func LoadAll(cfg LoadAllConfig) (*LoadResult, error)`
- **职责**：统一加载入口——递归扫所有 roots，识别 SKILL.md / 松散 .md / 插件容器，聚合结果
- **流程**：
  1. 创建空 `LoadResult`
  2. 对 `SkillsRoot` + `ExtraSkillsRoots` 调 `walkLoadRoot(root, true, result)`
  3. 对 `AgentsRoot` + `ExtraAgentsRoots` 调 `walkLoadRoot(root, false, result)`
  4. 返回 result
- **错误处理**：单个 root 不存在/无权限仅 warn 不 abort；只有顶层 cfg 全空才返回 nil result + nil err

### `walkLoadRoot`
- **签名**：`func walkLoadRoot(root string, wantSkills bool, result *LoadResult) error`
- **职责**：递归 walk 一个 root，按 `wantSkills` 区分 SKILL.md vs 松散 .md（agents）
- **流程**：
  1. `filepath.WalkDir(root, fn)`
  2. 对每个 entry：
     - 是目录 → 调 `DetectContainer(entry)` 探测插件容器；命中 → `LoadPluginContainer(path, wantSkills, result)` + `filepath.SkipDir`（**防双加载**：容器内自己的 walk 会扫子树，外层跳过）
     - 是文件 + wantSkills=true + base name == `SKILL.md` → `ParseSkillMd(path)` → append `Skill` + warnings
     - 是文件 + wantSkills=false + `.md` 后缀 + 非 SKILL.md → `ParseAgentMd(path)` → append `Agent`
     - symlink → `EvalSymlinks` 解析 + `pathSafeUnderRoot` 安检
  3. 解析错误仅 append `LoadWarning`，不 abort walk
- **安全检查**：`pathSafeUnderRoot(resolved, root)` 防 symlink 逃逸

### `pathSafeUnderRoot`
- **签名**：`func pathSafeUnderRoot(child, root string) bool`
- **职责**：判断 child 解析后是否仍在 root 之内（防 symlink 逃逸）
- **流程**：`EvalSymlinks` 双方 → `filepath.Rel(root, child)` → 不以 `..` 开头则安全
- **错误处理**：EvalSymlinks 失败返回 false（保守）

## 5. 依赖关系

- **标准库**：`os`、`path/filepath`、`strings`
- **同包**：`plugin_container.go`（DetectContainer / LoadPluginContainer / synthesizeBareSkillsPack）、`skill_parser.go`（ParseSkillMd）、`agent_parser.go`（ParseAgentMd）、`types.go`（LoadResult / LoadWarning / Skill / Agent）
- **被调用方**：SkillRegistry.Load / Reload、AgentRegistry.Load、marketplace install/uninstall 热重载

## 6. 并发与资源管理

- **无并发状态**：LoadAll 是一次性纯函数式扫描，结果通过返回值传递
- **文件句柄**：ParseSkillMd / ParseAgentMd 内部各自 `os.ReadFile` 后立即关闭，walk 不持有句柄
- **热重载原子性**：由调用方（SkillRegistry.Reload）负责——先在锁外构建新切片，再 O(1) 替换；LoadAll 本身不参与锁

## 7. 设计模式与亮点

- **单一入口**：LoadAll 同时处理 skills + agents + packs + commands + warnings，消除历史上 skills 走 SkillRegistry.Load、agents 走 AgentRegistry.Load、packs 走 LoadPluginContainer 三条独立路径的重复扫描
- **wantSkills 参数**：同一个 walk 函数通过布尔切换 SKILL.md（精确名匹配）vs 松散 .md（后缀匹配），避免代码重复
- **plugin 容器 SkipDir**：DetectContainer 命中后外层 walk 立即 SkipDir，容器内子树由 LoadPluginContainer 自己的递归 walk 接管——**防双加载**是关键不变量
- **symlink 安检**：`pathSafeUnderRoot` 用 EvalSymlinks 解析后再判 rel，防恶意/意外 symlink 逃逸到 root 之外
- **empty root 不报错**：`filepath.WalkDir` 对不存在的 root 返回 error，但 walkLoadRoot 把 "not exist" 当作空结果而非错误——fresh install 无 skills 也能 boot
- **warning 累积**：单个 SKILL.md 解析失败仅 warn，不 abort 整个 walk——"one bad SKILL.md should not take down boot"
- **ExtraRoots 变长参数**：Reload 接 variadic extras，marketplace install 时 builtin + user + newly-installed pack 三层 root 一次性扫描，避免丢失 image-baked built-in skills

## 8. 注意事项

- **walkLoadRoot 顺序敏感**：skills root 先扫、agents root 后扫；若同一文件名同时被两边匹配（理论上 SKILL.md 不会，但 .md 可能），先到先得
- **plugin 容器 SkipDir 仅对目录生效**：DetectContainer 只在 `entry.IsDir()` 时调用，文件级容器（openclaw.plugin.json 是文件但 DetectContainer 也认其父目录）需要 DetectContainer 内部处理父目录逻辑
- **symlink 安检 EvalSymlinks 失败保守**：返回 false 会导致该 entry 被跳过——若系统临时 IO 故障可能误伤，但安全优先
- **LoadResult.Packs 仅由 LoadPluginContainer 填充**：裸 SKILL.md / .md 不产生 Pack，消费者访问 Packs 时需 nil-check
- **ExtraSkillsRoots 顺序保留**：扫描顺序 = 列表顺序，去重靠调用方（Reload 不去重，重复 root 会重复加载——目前 marketplace 保证不传重复）
- **ParseAgentMd 与 ParseSkillMd 错误语义不同**：ParseSkillMd 缺 name/description 是 error；ParseAgentMd 缺 name 也是 error——但 walkLoadRoot 把两者都降级为 warning，不 abort
