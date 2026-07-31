# `sandbox.go` 技术实现文档

> 源文件：`internal/edgeagent/host_files/sandbox.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/host_files`

## 1. 概述

本文件定义 `SandboxConfig`：host_files 三个 handler 的路径 / 二进制 allow-list 闸门。路径策略是「denylist 驱动 + 可选 allowlist 叠加」：`DeniedReadPaths`（虚拟 FS + 敏感文件）始终生效；`AllowedReadPaths` 非空时额外要求路径在某前缀下。还提供 `ResolveBinary` 让 handler 通过短名获取启动期解析的绝对路径。真实内核级隔离（seccomp / cgroups / chroot）超出范围，wasmtime/MCP 是长期路径。

## 2. 包信息

- **包名**：`host_files`
- **所属模块**：edgeagent 文件系统能力层（沙箱子模块）
- **依赖方向**：被同包 `handlers.go::Register` / `ValidatePath` / `ResolveBinary` 调用；被 `bash/handlers.go` 借用作为 `PathValidator`

## 3. 关键类型与接口

```go
// SandboxConfig 定义 host_files 可访问范围
type SandboxConfig struct {
    DeniedReadPaths []string          // 拒绝前缀（虚拟 FS + 敏感文件）
    AllowedReadPaths []string         // 可选 allowlist；空 = 仅 denylist
    AllowedBinaries map[string]string // 短名 → 绝对路径
}

// DefaultDeniedReadPaths 是保守基线
var DefaultDeniedReadPaths = []string{
    "/proc", "/sys", "/dev", "/run",       // 虚拟 FS（du/find 永不结束）
    "/etc/shadow", "/etc/gshadow", "/etc/sudoers", "/etc/sudoers.d",  // 密码 / sudo
    "/root/.ssh", "/root/.gnupg",           // root 私钥 / GPG
}
```

## 4. 关键函数与流程

### `DefaultSandboxConfig`
- **签名**：`func DefaultSandboxConfig() *SandboxConfig`
- **职责**：返回匹配 `SKILL.md edge_capabilities` 的默认配置
- **流程**：复制 `DefaultDeniedReadPaths`；`AllowedReadPaths` 留空（denylist alone）；`discoverBinaries(["find","du","df","stat","ls"])` 解析二进制
- **错误处理**：缺失二进制不在 map 中，`ResolveBinary` 报错

### `Validate`
- **签名**：`func (s *SandboxConfig) Validate() error`
- **职责**：启动期不变量校验
- **流程**：nil config 报错；`find` + `du` 必须在 AllowedBinaries 中（stat 是 Go 原生不需要）
- **错误处理**：缺失必需二进制报错；main.go 决定是否致命（通常 log + 继续不带 host_files 能力启动）

### `ValidatePath`
- **签名**：`func (s *SandboxConfig) ValidatePath(path string) error`
- **职责**：路径校验（denylist + 可选 allowlist + symlink 解析）
- **流程**：
  1. nil config / 空路径 / 非绝对路径 报错
  2. `filepath.Clean(path)`
  3. `filepath.EvalSymlinks(clean)` 尝试解析（不存在 / 失败时用 cleaned）
  4. **denylist 检查**：`matchesDenied(clean, resolved)`——clean 或 resolved 任一匹配前缀即拒绝
  5. **per-user敏感目录**：`matchesPerUserSensitive(resolved)`——catch `/home/<user>/.ssh` / `.gnupg`
  6. **可选 allowlist**：`AllowedReadPaths` 非空时 `lexicalInAllowList(clean) || resolvedInAllowList(resolved)` 必须命中一个
- **错误处理**：每步失败带详细 Reason（含匹配的 denied prefix）

### `matchesDenied`
- **签名**：`func (s *SandboxConfig) matchesDenied(clean, resolved string) (bool, string)`
- **职责**：检查 clean 或 resolved 是否在某 denied prefix 之下
- **流程**：遍历 DeniedReadPaths；对每个 prefix `filepath.Clean`；检查 `candidate == d` 或 `HasPrefix(candidate, d+sep)`

### `matchesPerUserSensitive`
- **签名**：`func matchesPerUserSensitive(p string) (string, bool)`
- **职责**：catch `/home/<user>/.ssh` 和 `.gnupg`（root 由静态 DeniedReadPaths 处理）
- **流程**：`HasPrefix("/home/")` → Split 路径 → parts[3] 是 `.ssh` / `.gnupg` 时拒绝

### `lexicalInAllowList` / `resolvedInAllowList`
- **签名**：`func (s *SandboxConfig) lexicalInAllowList(p string) bool` / `func (s *SandboxConfig) resolvedInAllowList(p string) bool`
- **职责**：路径在某 allowlist 前缀下（trailing separator 防止 `/var-evil` 匹配 `/var`）
- **流程**：`resolvedInAllowList` 对每个 allowlist entry 也 `EvalSymlinks`（macOS `/var` → `/private/var`）

### `ResolveBinary`
- **签名**：`func (s *SandboxConfig) ResolveBinary(name string) (string, error)`
- **职责**：从 allow-list 返回绝对路径
- **流程**：map O(1) 查找；缺失报错「not in allow-list」

### `discoverBinaries`
- **签名**：`func discoverBinaries(names []string) map[string]string`
- **职责**：解析每个二进制到绝对路径
- **流程**：`exec.LookPath(name)` 失败时遍历 `/usr/bin /bin /usr/local/bin /sbin /usr/sbin`；缺失不在 map 中

## 5. 依赖关系

- **内部包**：无
- **外部库**：标准库 `errors`、`fmt`、`os/exec`、`path/filepath`、`strings`
- **被调用方**：同包 `handlers.go`；外部 `bash/handlers.go`（作为 `cmdpolicy.PathValidator`）

## 6. 并发与资源管理

无并发控制。`SandboxConfig` 构造后视为不可变（多 handler goroutine 共享读）。`AllowedBinaries` map 在 `discoverBinaries` 构造期填充后不再修改。

## 7. 设计模式与亮点

- **denylist 驱动而非 allowlist**：默认拒绝虚拟 FS + 敏感文件，允许其他一切——支持 `du /` / `find /usr` 等合法诊断场景（之前 allowlist-only 太保守）
- **双路径校验（lexical + resolved）**：cleaned path 走 lexical 检查；EvalSymlinks 成功的走 resolved 检查——catch 对抗性 symlink（`/tmp/escape -> /etc/shadow`）+ 容忍 OS canonical 差异（macOS `/var` → `/private/var`）
- **per-user敏感目录 glob**：静态 `DeniedReadPaths` 只 catch `/root/.ssh`；`matchesPerUserSensitive` 通过路径解析 catch 任意 `/home/<user>/.ssh`
- **`AllowedReadPaths` 是叠加而非替代**：denylist 始终生效；allowlist 非空时是额外约束——operator 可在 denylist 之上收紧
- **二进制 allow-list O(1) 查找**：启动期 `discoverBinaries` 解析一次，运行时 map 查找；防御「启动后二进制被替换」（exec.Command 在 run 时校验存在性）
- **复用作为 PathValidator**：`cmdpolicy` 通过 `PathValidator` 接口借用 `ValidatePath`——单一真相源，bash 和 host_files 看到同一组路径规则
- **`Validate` 不要求 stat**：只要求 `find` + `du` 已解析（stat 是 Go 原生无二进制需求）——简化启动校验
- **trailing separator 防 prefix 误匹配**：`HasPrefix(p, a+string(filepath.Separator))` 而非 `HasPrefix(p, a)`——避免 `/var-evil` 匹配 `/var`

## 8. 注意事项

- `EvalSymlinks` 失败（路径不存在）时降级到 lexical 检查——`find_large_files` 可能扫一个尚未创建的路径，让 find 子进程而非 validator 报「不存在」
- macOS `/var` → `/private/var` 的 canonical 差异由 `resolvedInAllowList` 的 EvalSymlinks 处理；若 operator 在 allowlist 写 `/var`，resolved 检查仍能匹配 `/private/var/...`
- `matchesPerUserSensitive` 只检查 `parts[3]`——`/home/user/subdir/.ssh` 不会被拒绝（只有第一级子目录的 `.ssh` 被拒）；这是预期行为（用户可能在子目录放测试用 .ssh）
- `discoverBinaries` 的 fallback 链是 Linux + 通用 Unix 路径——Windows 上某些二进制可能找不到；但 host_files 设计是 Unix 能力
- `DefaultSandboxConfig` 不包含 `ls` 在 RequiredBinaries——`Validate` 只校验 `find` + `du`；`ls` 是 future 用途
- `ValidatePath` 不校验路径存在性——让 find/du/stat 子进程报「not found」；validator 只关心安全边界
- `AllowedReadPaths` 为空时 denylist 是唯一约束——operator 想收紧时显式设 allowlist
- 被 `cmdpolicy` 借用意味着 bash skill 看到同一组路径规则——bash 调用 `find /etc/shadow` 也会被拒（不仅 host_files 拒）
- `DefaultDeniedReadPaths` 与 `SKILL.md` 的 `edge_capabilities` block 需保持同步；单测 `TestDefaultSandbox_MatchesSkillManifest` 是漂移提醒
