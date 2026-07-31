# `plugin_container.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/chatruntime/plugin_container.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/chatruntime`

## 1. 概述

本文件实现插件容器探测与加载：识别 `.claude-plugin/`（claude）、`openclaw.plugin.json`（openclaw）、裸 skills 目录（bare-skills）三种容器形态，递归加载容器内的 skills + agents + commands，合成 Pack 聚合结果。是 LoadAll 的插件容器子系统。设计目标：容器优先级 openclaw > claude > bare-skills > none；symlink 安全；openclaw legacy 字段保留到 UIMetadata；hooks 目录扫描产出诊断警告。

## 2. 包信息

- **包名**：`chatruntime`
- **所属模块**：`internal/manager/biz/aiops/chatruntime`
- **依赖方向**：被 load_all.go 的 walkLoadRoot 调用；调用 skill_parser.go / agent_parser.go / command_parser.go（claudecmd）

## 3. 关键类型与接口

```go
type ContainerKind string

const (
    ContainerNone      ContainerKind = "none"
    ContainerBareSkills ContainerKind = "bare-skills"
    ContainerClaude    ContainerKind = "claude"
    ContainerOpenclaw  ContainerKind = "openclaw"
)

type PluginManifest struct {
    Name, Version, Description string
    Author, Homepage, License  string
    Skills []string  // openclaw: skills[] 路径列表
    // 其他字段保留到 UIMetadata
    Raw map[string]any  // 原始 JSON
}
```

## 4. 关键函数与流程

### `DetectContainer`
- **签名**：`func DetectContainer(dirEntry os.DirEntry) ContainerKind`
- **职责**：探测一个目录是否是插件容器，返回最高优先级形态
- **流程**：
  1. 非目录 → `ContainerNone`
  2. 存在 `openclaw.plugin.json` → `ContainerOpenclaw`（最高优先级）
  3. 存在 `.claude-plugin/` 子目录 → `ContainerClaude`
  4. `hasBareSkills(dir)` → `ContainerBareSkills`
  5. 否则 `ContainerNone`
- **优先级**：openclaw > claude > bare-skills > none

### `hasBareSkills`
- **签名**：`func hasBareSkills(dir string) bool`
- **职责**：扫描目录判断是否是"裸 skills"形态（无 manifest 但含 SKILL.md / .md）
- **流程**：读 dir entries，命中 `SKILL.md` 或任意 `.md` → true

### `LoadPluginContainer`
- **签名**：`func LoadPluginContainer(path string, wantSkills bool, result *LoadResult) error`
- **职责**：递归加载插件容器内的 skills + agents + commands，合成 Pack
- **流程**：
  1. `DetectContainer` 确定容器类型
  2. 解析 manifest（claude 读 `.claude-plugin/plugin.json`，openclaw 读 `openclaw.plugin.json`）
  3. `resolveSkillSubRoots(manifest, path)` 计算实际 skills 子目录列表（openclaw manifest.skills[] 路径处理）
  4. 对每个 skill sub-root 调 `walkLoadRoot(subRoot, true, result)`
  5. 对 agents 子目录（claude 是 `.claude-plugin/agents/`，openclaw 按 manifest）调 `walkLoadRoot(subRoot, false, result)`
  6. `scanHooksDir(path)` 扫 hooks 目录产出诊断警告（非 fatal）
  7. 合成 `Pack{ID, Root, ManifestPath, Skills, Agents, Commands, Manifest}` append 到 result.Packs
  8. bare-skills 走 `synthesizeBareSkillsPack` 无 manifest 合成路径
- **安全检查**：每个 sub-root 调用前 `pathSafeUnderRoot` 安检

### `parsePluginManifest`
- **签名**：`func parsePluginManifest(path string) (*PluginManifest, error)`
- **职责**：解析 openclaw / claude 的 manifest JSON
- **流程**：`os.ReadFile` → `json.Unmarshal` 到强类型字段 + `Raw` map（保留未知字段给 UIMetadata）
- **错误处理**：文件不存在 / JSON 解析失败返回 error

### `synthesizeBareSkillsPack`
- **签名**：`func synthesizeBareSkillsPack(path string, wantSkills bool, result *LoadResult) error`
- **职责**：无 manifest 的裸 skills 目录合成 Pack
- **流程**：
  1. `bareSkillsPackID(path)` 规范化生成 PackID
  2. 调 `walkLoadRoot(path, wantSkills, result)` 加载子树
  3. 从 result 中提取本次新增的 Skills/Agents/Commands 组装 Pack
  4. Pack.Manifest = nil（标记无 manifest）

### `bareSkillsPackID`
- **签名**：`func bareSkillsPackID(path string) string`
- **职责**：为裸 skills 目录生成规范 PackID
- **流程**：`filepath.Clean` + 替换路径分隔符为 `_` + 加 `bare-` 前缀

### `scanHooksDir`
- **签名**：`func scanHooksDir(root string) []LoadWarning`
- **职责**：扫描 hooks 目录（`.claude-plugin/hooks/` 等），产出诊断警告
- **流程**：列目录，每个 hook 文件 append 一条 `Code: "hooks_diagnostic"` 的 LoadWarning
- **非 fatal**：仅诊断，不阻止加载

### `resolveSkillSubRoots`
- **签名**：`func resolveSkillSubRoots(manifest *PluginManifest, containerPath string) []string`
- **职责**：从 openclaw manifest.skills[] 计算实际 skills 子目录绝对路径
- **流程**：遍历 manifest.Skills，`filepath.Join(containerPath, s)` + `EvalSymlinks` 解析

## 5. 依赖关系

- **标准库**：`os`、`path/filepath`、`encoding/json`、`strings`
- **同包**：`load_all.go`（walkLoadRoot / pathSafeUnderRoot）、`skill_parser.go`、`agent_parser.go`、`command_parser.go`（claudecmd.ConvertCommandFile）、`types.go`（Pack / LoadResult / LoadWarning）
- **被调用方**：load_all.go 的 walkLoadRoot

## 6. 并发与资源管理

- **无并发状态**：所有函数纯同步，结果通过参数 / 返回值传递
- **文件句柄**：os.ReadFile / ReadDir 后立即释放
- **EvalSymlinks 调用**：每次 pathSafeUnderRoot 都调用，开销可接受（加载阶段一次性，非热路径）

## 7. 设计模式与亮点

- **容器优先级 openclaw > claude > bare-skills**：openclaw 是 ongrid 原生插件格式优先；claude 是 Claude Code 兼容；bare-skills 是无 manifest 兜底
- **openclaw legacy 保留到 UIMetadata**：`PluginManifest.Raw` 保留原始 JSON，UI 层可读未知字段——前向兼容
- **hasBareSkills 探测**：无 manifest 但含 .md 的目录也认作容器，避免漏加载用户随手放的 SKILL.md
- **scanHooksDir 诊断而非 fatal**：hooks 配置错误只 warn，不阻止 skills/agents 加载——降低用户排障门槛
- **resolveSkillSubRoots 路径处理**：openclaw manifest.skills[] 是相对路径，Join + EvalSymlinks 解析为绝对路径，防相对路径歧义
- **synthesizeBareSkillsPack 无 manifest 合成**：保证裸 skills 目录也有 Pack 入口，UI 能统一展示
- **pathSafeUnderRoot 安检**：每个 sub-root 加载前都过一遍，防 symlink 逃逸到容器外

## 8. 注意事项

- **优先级硬编码**：openclaw > claude > bare-skills 在 DetectContainer 中以 if-else 顺序体现，调整优先级需改源码
- **openclaw manifest.skills[] 相对路径**：必须是相对容器根的路径，绝对路径会被 Join 错位
- **.claude-plugin/ 子目录结构假设**：claude 容器假设 agents 在 `.claude-plugin/agents/`，commands 在 `.claude-plugin/commands/`——若用户自定义子目录需扩展 resolveSkillSubRoots
- **scanHooksDir 仅诊断**：不解析 hooks 内容，不阻止加载；hooks 实际执行由前端 / runtime 层负责
- **bareSkillsPackID 路径规范化**：用 `_` 替换路径分隔符，Windows 上 `\` 也会被替换，但 PackID 不保证跨平台一致（仅 UI 展示用）
- **LoadPluginContainer 递归 walkLoadRoot**：容器内子树再走 walkLoadRoot，理论上支持嵌套容器（容器中的容器），但实际未测试该场景
- **manifest.Raw 保留未知字段**：但强类型字段（Name/Version/Skills 等）解析失败会丢失到 Raw——若 openclaw schema 变更某字段类型，强类型解析失败但 Raw 仍有数据
