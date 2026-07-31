# `loader.go` 技术实现文档

> 源文件：`internal/skill/loader.go`
> 包路径：`github.com/ongridio/ongrid/internal/skill`

## 1. 概述

`loader.go` 是 ongrid 外部 skill pack 的磁盘加载器：扫描受信任的 allowlist 目录树，逐个解析 `skill.json` 清单文件，构建 `SubprocessSkill` 实例并注册到全局 `Registry`。它把 skills.sh / openclaw 风格的"可执行文件 + JSON 清单"打包格式接入框架，让外部二进制（无需 Go/Python/Node 运行时）也能作为 LLM 工具参与调度。

## 2. 包信息

- **包名**：`skill`
- **所属模块**：`internal/skill`（L2 设备能力框架层）
- **依赖方向**：被 `cmd/ongrid` / `cmd/ongrid-edge` 在启动阶段调用 `LoadDirs`；调用 `SubprocessSkill`（同包）、`Register` / `Get`（同包 `registry.go`）

## 3. 关键类型与接口

```go
// 外部 skill pack 的磁盘清单格式，与可执行文件同目录存放
type SkillManifest struct {
    Name          string          // skill key（lower_snake），必填
    Description   string          // 给人 + LLM 的描述，必填
    Schema        json.RawMessage // 原始 JSON Schema，可选
    Entry         string          // 可执行文件路径（相对/绝对），必填
    EnvAllow      []string        // 转发给子进程的环境变量名白名单
    TimeoutSeconds int            // 子进程超时；0 = 默认 30s
    Class         string          // safe/mutating/dangerous，空 = safe
    Category      string          // UI 分组标签，空 = "external"
}

// 调用方传入 NewLoader/LoadDirs 的配置
type LoaderConfig struct {
    Dirs   []string                 // allowlist 目录列表
    Logger func(format string, args ...any) // 审计日志回调，可为 nil
}
```

## 4. 关键函数与流程

### `LoadDirs`
- **签名**：`func LoadDirs(cfg LoaderConfig) (int, error)`
- **职责**：遍历 `cfg.Dirs`，对每个绝对路径且存在的目录调用 `loadOneDir`，累加注册数。
- **流程**：跳过空串/非绝对路径；`os.Stat` 失败时按"目录不存在"软跳过、其他错误也仅记录；目录合法则下沉 `loadOneDir`。
- **错误处理**：返回 (累计注册数, 首个致命错误)；单目录失败不阻塞其他目录；Logger 为 nil 时静默。

### `loadOneDir`
- **签名**：`func loadOneDir(root string, logf func(string, ...any)) (int, error)`
- **职责**：递归遍历单个 allowlist 根，发现 `skill.json` 即解析、构建、注册。
- **流程**：`filepath.Walk` → 命中 `skill.json` → `parseManifest` → `buildSubprocessSkill` → 重复 key 跳过 → `defer/recover` 包裹 `Register`。
- **错误处理**：单清单解析/构建失败仅记录日志、跳过；Walk 自身的错误向上返回。`defer/recover` 防御未来 `Register` 收紧导致 boot 崩溃；重复 key 软跳过避免热重载抖动。

### `parseManifest`
- **签名**：`func parseManifest(path string) (*SkillManifest, error)`
- **职责**：读取并 JSON 解码清单文件。
- **错误处理**：读文件、解码分别用 `%w` 包装为 `read:` / `decode:` 前缀错误。

### `buildSubprocessSkill`
- **签名**：`func buildSubprocessSkill(m *SkillManifest, manifestPath, allowRoot string) (*SubprocessSkill, error)`
- **职责**：把清单转换为 `SubprocessSkill`，并做安全/语义校验。
- **流程**：
  1. 校验 `Name`（非空 + `validKey`）、`Description`、`Entry` 必填；
  2. 解析 `Entry`：相对路径按清单目录拼接为绝对路径；
  3. `filepath.EvalSymlinks` 规范化 Entry 与 allowRoot，`pathHasPrefix` 验证 Entry 落在 allowRoot 内（防 `..` 与软链接逃逸）；
  4. `Class` 合法性校验，空降级为 `ClassSafe`；`Category` 空降级为 `"external"`；`TimeoutSeconds<=0` 降级为 `DefaultSubprocessTimeout`；
  5. 构造 `SubprocessSkill`（强制 `Scope=ScopeManager`），再调用 `Metadata().Validate()` 兜底校验。
- **错误处理**：每一步失败均返回带上下文的 `error`，由调用方决定是否记录跳过。

### `pathHasPrefix`
- **签名**：`func pathHasPrefix(child, parent string) bool`
- **职责**：判断 child 是否位于 parent 内（含相等）。先 Clean，再通过 `filepath.Rel` 判断是否以 `..` 开头。

## 5. 依赖关系

- **内部包**：无（仅同包内 `SubprocessSkill`、`Register`/`Get`、`Metadata`/`Validate`、`Class`/`Scope` 常量、`validKey`、`DefaultSubprocessTimeout`）
- **外部库**：仅 Go 标准库（`encoding/json`、`errors`、`fmt`、`os`、`path/filepath`、`strings`、`time`）
- **被调用方**：`cmd/ongrid` / `cmd/ongrid-edge` 启动逻辑（注入 allowlist 目录后批量装载外部 skill）

## 6. 并发与资源管理

无显式并发控制。`LoadDirs` 在进程启动阶段单线程执行；并发安全由下游 `Registry` 的 `sync.RWMutex` 保证。`filepath.Walk` 为同步遍历，无 goroutine。

## 7. 设计模式与亮点

- **Allowlist + EvalSymlinks 双层防逃逸**：通过 `pathHasPrefix` 与 `filepath.EvalSymlinks` 双重校验，杜绝清单通过 `..` 或软链接指向 allowlist 外的二进制（如 `/bin/sh`）。
- **Fail-soft 装载策略**：单清单/单目录失败不阻塞整体 boot，配合"重复 key 软跳过"支持热重载场景。
- **defer/recover 防御 Register panic**：即便作者侧未来收紧 `Register` 校验，也不会拖垮整个进程启动。
- **零依赖**：仅用标准库，便于在 edge agent 极简运行时中复用。

## 8. 注意事项

- **Entry 必须绝对路径 + 落在 allowRoot**：作者若手写 `SubprocessSkill` 绕过 Loader，仅由 `SubprocessSkill.Execute` 做绝对路径校验，allowlist 边界不再生效。
- **EnvAllow 默认空 = 子进程无任何环境变量**（连 `PATH` 都没有），manifest 需显式 opt-in。
- **`Logger` 为 nil 时静默**：生产建议注入结构化 logger，否则装载审计不可见。
- **`filepath.Walk` 已被官方标记推荐替换为 `filepath.WalkDir`**：当前实现可继续工作，但 Walk 会额外分配 `os.FileInfo`，未来若目录规模大可考虑切换。
- **`buildSubprocessSkill` 强制 `Scope=ScopeManager`**：外部子进程 skill 永远不会跑在 edge 上，这是有意为之的安全约束。
