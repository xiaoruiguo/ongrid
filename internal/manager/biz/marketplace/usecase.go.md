# usecase.go

## 1. 概述

`usecase.go` 是 marketplace 包的编排核心 —— `Usecase` 结构持有 `Repo` / `SkillRegistry` / `AgentRegistry` / `Config`，对外暴露 install / list / uninstall / setbindings / registries 等方法。

`Install` 是最复杂的方法，9 步流程：fetch 到 staging → 检测 pack 布局 → 加载 plugin container → 路径安全检查 → 唯一性检查 → 移动 staging→install → 签名验证 → 信任闸门 → 持久化 + 热重载注册表。

文件还包含大量辅助函数：`fetchToStaging`（按 source type 分发）、`buildCapabilityDeclaration`（构造 SPA 能力快照）、`copyTree`（跨 fs rename 回退）、`expandShorthandGitURL`（skills.sh 简写展开）等。

## 2. 包信息

- 包名：`marketplace`
- 路径：`internal/manager/biz/marketplace`

## 3. 关键类型与接口

### Config（启动配置）

```go
type Config struct {
    SystemSkillsRoot     string         // 集群级 pack 根（所有租户可见）
    BuiltinSkillsRoots   []string       // 镜像内置根（/skills），Reload 时传 extras 不丢内置
    BuiltinAgentsRoots   []string       // 同上，agent 用
    TenantSkillsRoot     func(uint64) string  // 每租户安装目录；nil = 单租户用 SystemSkillsRoot
    StagingDir           string         // tarball/git 落地临时目录
    AllowedSources       []string       // source allowlist，默认 ["ongrid-official", "local"]
    RequireSignedSources []string       // 强制签名的 source label，默认 ["ongrid-official"]
    SignaturePinnedKey   string         // PEM ECDSA pubkey，空 = 不 pin
    DevMode              bool           // 旁路 allowlist + 签名闸门
    HTTPClient           *http.Client   // tarball 下载用；nil = http.DefaultClient
    GitCmd               string         // git 二进制路径；空 = "git"
    Now                  func() time.Time  // 时间注入；测试用
}
```

### Usecase

```go
type Usecase struct {
    repo     Repo
    skillReg SkillRegistry
    agentReg AgentRegistry
    cfg      Config
    log      *slog.Logger
}
```

注释：并发安全由底层 repo + registries 保证（`Usecase` 本身无可变状态）。

### AllowedRegistries / RegistryEntry

```go
type AllowedRegistries struct {
    Items []RegistryEntry
}
type RegistryEntry struct {
    Name    string
    URL     string
    Allowed bool
}
```

`Registries` 方法的返回，给 SPA "可从哪安装" picker 用。今天静态，DevMode 加合成 `<dev-mode>` entry。

## 4. 关键函数与流程

### Install（9 步流程）

```go
func (uc *Usecase) Install(ctx, caller, src) (*InstallResult, error)
```

1. **权限**：`!caller.IsAdmin()` → `ErrForbidden`
2. **source allowlist**：`checkSourceAllowed`，`DevMode` 旁路
3. **fetch 到 staging**：`fetchToStaging` 返回 `(stagingPath, parentStaging, err)`。`cleanupStaging` 闭包在每条失败路径调 `os.RemoveAll(parentStaging)`
4. **检测 pack 布局**：`chatruntime.DetectContainer(stagingPath)` 接受三种形式：`.claude-plugin/plugin.json` / `openclaw.plugin.json` / bare `skills/<name>/SKILL.md`。`ContainerNone` → 拒绝
5. **加载 plugin container**：`chatruntime.LoadPluginContainer(stagingPath)`。`Pack == nil || Pack.ID == ""` → 拒绝。`escapes_root` warning → 拒绝（path-traversal symlink）
6. **路径安全**：`pack.ID` 不含 `..` / `/` / `filepath.Separator`；`installPath = filepath.Join(root, pack.ID)` 在 `root` 内（`pathHasPrefix`）
7. **唯一性**：`(tenant, pack_id)` live → `ErrConflict` 引导用 Update/Uninstall；`(tenant, sha)` live → `ErrConflict` "identical pack already installed"。注释：reinstall-after-uninstall 由 soft-delete 标记 + `idx_tenant_pack` 索引处理，`Create` 直接成功
8. **移动 staging→install**：`os.MkdirAll(root)` → `os.RemoveAll(installPath)`（清残留）→ `os.Rename(stagingPath, installPath)`。rename 失败（跨 fs）回退 `copyTree` + `RemoveAll`。`cleanupStaging` 总是调（清残留 temp dir + 下载 .tgz 碎片）。`rebaseDirs` 把 loaded skill/agent 的 `.Dir` 重映射到最终 install path
9. **签名验证 + 信任闸门**：`VerifySignature(installPath, cfg.SignaturePinnedKey)` → `sigState`。`!DevMode && requiresSigned(label) && sigState != verified` → 删 installPath + `ErrForbidden`
10. **持久化**：`buildCapabilityDeclaration` 构造能力快照 → `json.Marshal` → `repo.Create(row)`。DB 失败 best-effort 删 installPath 保持 disk+DB 一致
11. **热重载**：`reloadRegistries()` 调 `skillReg.Reload(SystemSkillsRoot, BuiltinSkillsRoots...)` + `agentReg.Reload(primary, extras...)`。失败 warn 不 return（disk+DB 已变更，操作员应看到成功）

### List

```go
func (uc *Usecase) List(ctx, caller) ([]*model.InstalledPack, error)
```

admin (`scope=0`) 看所有租户；非 admin 只看自己租户。`caller.UserID == 0` → `ErrUnauthorized`。

### Uninstall

```go
func (uc *Usecase) Uninstall(ctx, caller, packID) error
```

1. admin 校验
2. `repo.GetByPackID` → `ErrNotFound` 幂等返回 nil
3. 路径安全：`row.InstallPath` 必须在 `targetRoot` 内才 `os.RemoveAll`，否则 warn 拒绝删（防 DB 行被篡改指向 `/etc`）
4. `repo.DeleteSoft` → `ErrNotFound` 幂等
5. `reloadRegistries`

### SetBindings

```go
func (uc *Usecase) SetBindings(ctx, caller, packID, bindings) error
```

HLD-017 凭证绑定：`bindings` map slot→credential name。admin only，wholesale replace（非 merge）。trim slot 与 cred，丢空。

### BoundCredentialNamesForSkills

```go
func (uc *Usecase) BoundCredentialNamesForSkills(ctx, skillNames) []string
```

HLD-017 设计期绑定：chat runtime 每轮调，返回与活跃 skill 关联的已安装 pack 的 bound credential name 列表。流程：`repo.List(0)` 列所有 pack → 对每个 pack 解析 `CapabilitiesJSON` 检查是否含任一 skillName → 解析 `BindingsJSON` 收集 credential name。best-effort：解析失败 skip。

### fetchToStaging

按 `src.Type` 分发：

- **local**：`src.Path` 必须绝对 + 是目录 → `copyTree` 到 `stage/<basename>`
- **tarball**：`src.URL` → `downloadAndExtractTarball` 到 `stage/<basename>`；若 single top-level dir 下降一层（让 marker probe 在 depth 0 找到 manifest）
- **git**：`expandShorthandGitURL`（`owner/repo` → `https://github.com/owner/repo.git`）→ `git clone --depth=1 [--branch=Ref]` → 删 `.git` 目录
- **registry**：未实现，拒绝

### reloadRegistries

```go
func (uc *Usecase) reloadRegistries()
```

skill registry：`Reload(SystemSkillsRoot, BuiltinSkillsRoots...)`。extras 是 builtin 根，保证镜像内置 skill（host_files / restart_service / bash）在 pack 变更触发的 reload 中不丢。

agent registry：primary = `BuiltinAgentsRoots[0]`，extras = 其余 builtin agent root + SystemSkillsRoot + BuiltinSkillsRoots。注释：agent persona 是 loose `*.md`，pack 内 `agents/` 也算 persona 源，所以 SystemSkillsRoot 当 extras 传。

### buildCapabilityDeclaration

```go
func buildCapabilityDeclaration(packID, version string, res *chatruntime.LoadResult) CapabilityDeclaration
```

遍历 `res.Skills`：每个 skill 提取 `Requires` / `Ongrid.EdgeCapabilities` / tool classes。跨 skill 去重 `Summary`：`ToolClasses` / `Bins` / `ConfigKeys` 用 set 去重后 `sortedKeys`；`CredentialSlots` 按 slot key 去重保首次出现。`Scope` 默认 `"manager"`。

### expandShorthandGitURL

```go
func expandShorthandGitURL(raw string) string
```

`owner/repo` → `https://github.com/owner/repo.git`。保守模式：单 slash、owner 无 dot、不包含 `://` 或 `@`。已有 scheme 或 SCP-style `user@host:owner/repo` 直接 passthrough。

### 辅助函数

- `targetRoot(tenantID)`：返回租户安装根或系统根
- `checkSourceAllowed`：allowlist + DevMode 旁路
- `downloadAndExtractTarball`：HTTP GET → `extractTarGz`
- `rebaseDirs`：重映射 loaded skill `.Dir` 到 install path（当前 noop，注释解释 reload 会重 walk）
- `toBizWarnings`：chatruntime.LoadWarning → biz.LoadWarning
- `pathHasPrefix`：child 是否在 parent 内
- `sortedKeys`：map keys 排序
- `copyTree`：递归复制，symlink 在 copy 时 resolve（避免指向 staging 的 link 在 staging 删除后悬挂）
- `singleTopLevelDir`：tarball 单顶层目录下降
- `stagingBasename`：路径/URL 叶子净化，空/`.`/`..` 退化 "pack"
- `requiresSigned`：label 是否在 RequireSignedSources 列表

## 5. 依赖关系

### 外部包

- `net/http` —— tarball 下载
- `os` / `os/exec` / `path/filepath` / `io` / `strings` / `sort` / `encoding/json` / `time` / `log/slog` / `fmt` / `errors` / `context`

### 内部包

- `chatruntime "github.com/ongridio/ongrid/internal/manager/biz/aiops/chatruntime"` —— `DetectContainer` / `LoadPluginContainer` / `LoadResult` / `LoadWarning` / `ClassRead` 等
- `model "github.com/ongridio/ongrid/internal/manager/model/marketplace"` —— `InstalledPack` 模型
- `github.com/ongridio/ongrid/internal/pkg/errs` —— 错误哨兵

### 被谁调用

- HTTP handler（`/v1/marketplace/*`）调所有公开方法
- `chatruntime` 反向调 `BoundCredentialNamesForSkills`（结构性满足 `CredentialBinder` 接口）

## 6. 并发与资源管理

- **无锁**：`Usecase` 无可变状态；并发安全由 repo + registries 保证
- **staging 清理**：`cleanupStaging` 闭包在每条失败/成功路径调用，保证 staging root 不积累碎片
- **defer**：`extractTarGz` 内 `defer gz.Close()`；HTTP `defer resp.Body.Close()`
- **批量大小**：`buildCapabilityDeclaration` 内 set 去重，无批量 IO
- **context 传播**：所有 IO 函数首参 `ctx`

## 7. 设计模式与亮点

### 9 步 Install 的失败语义

每步失败都 `cleanupStaging()` + 返回明确错误：
- `ErrForbidden`（权限/allowlist/签名闸门）
- `ErrInvalid`（fetch/validate/path 失败）
- `ErrConflict`（pack_id 或 sha 重复）

`InstallResult` 含 `Warnings`（非阻塞问题如 `escapes_root` 之外的字段缺失）让 SPA 提示操作员。

### staging → install 的原子性

`os.Rename` 同 fs 原子；跨 fs 回退 `copyTree + RemoveAll`。`cleanupStaging` 总调，清掉 temp parent dir + 下载碎片。`rebaseDirs` 让 loaded metadata 指向最终路径。

### 路径安全纵深防御

1. `pack.ID` 不含 `..` / `/` / separator
2. `installPath = filepath.Join(root, pack.ID)` 后 `pathHasPrefix` 验证
3. `Uninstall` 删 `row.InstallPath` 前再 `pathHasPrefix` 验证（防 DB 行被篡改指向 `/etc`）
4. `extractTarGz` 双重路径穿越检查
5. `LoadPluginContainer` 的 `escapes_root` warning 阻塞安装

### 镜像内置根不丢

`reloadRegistries` 把 `BuiltinSkillsRoots` / `BuiltinAgentsRoots` 当 extras 传，保证 pack 变更触发的 reload 不会让镜像内置的 `host_files` / `restart_service` / `bash` / 默认 agent persona 消失。

### 能力快照去重

`buildCapabilityDeclaration` 跨 skill 去重 `Summary`：set 去重 + `sortedKeys` 稳定输出。`CredentialSlots` 按 slot key 去重保首次出现，让 binding 对话框每个 slot 一行。

### reinstall-after-uninstall 由索引处理

注释明确：soft-delete 标记 + `idx_tenant_pack` 索引让软删行不占 live slot，`Create` 直接成功。无需特殊 "reinstall" 路径。

### skills.sh 简写展开

`expandShorthandGitURL` 保守识别 `owner/repo` 简写：单 slash、owner 无 dot、无 scheme/`@`。已有 URL 形式 passthrough。这让 `npx skills add owner/repo` 风格的输入直接可用。

### DevMode 旁路

`DevMode=true` 旁路 allowlist + 签名闸门。dev cluster 可不签名迭代。生产部署不应开。

## 8. 注意事项

- **registry 类型未实现**：`SourceTypeRegistry` 在 `fetchToStaging` 直接拒绝，引导用 tarball/local/git。registry 代理在后续 PR
- **`rebaseDirs` 当前 noop**：注释解释 reload 会重 walk installPath 重填 `.Dir`，所以 staging→install 后不立即重映射。若未来需要 per-skill metadata 引用相对路径（如 openapi.yaml），需补全
- **签名闸门在 DevMode 旁路**：dev cluster 可装未签名 pack。生产部署不应开 DevMode
- **`reloadRegistries` 失败只 warn**：disk+DB 已变更，操作员应看到 install 成功；若 reload 失败下次 chat 需手动重启。这是有意的 UX 取舍
- **`BoundCredentialNamesForSkills` best-effort**：解析失败的 pack 行 skip，不报错。单租户下列所有 pack（`List(0)`）
- **`copyTree` 解析 symlink**：copy 时 `os.Readlink` + `os.Symlink` 重创建，避免指向 staging 的 link 在 staging 删除后悬挂。但目标若是绝对路径仍可能指向 staging —— 上层 `escapes_root` 检查兜底
- **`pack.ID` 是 pack 自报**：来自 manifest 的 `id` 字段。恶意 pack 可伪造 id 试图覆盖已安装 pack —— 唯一性检查 `(tenant, pack_id)` live 阻止，但 soft-deleted 行不复用，重装同 id 是新行
- **`Now()` 默认 UTC**：`cfg.Now` nil 时 `time.Now().UTC()`。测试可注入固定时间
- **`installPath` 删除前 `pathHasPrefix`**：`Uninstall` 删 `row.InstallPath` 前必须验证在 root 内。DB 行若被篡改指向 `/etc` 会被拒绝删 + warn
