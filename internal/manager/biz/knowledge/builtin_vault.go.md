# `builtin_vault.go` 技术实现文档

> 源文件：`internal/manager/biz/knowledge/builtin_vault.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/knowledge`

## 1. 概述

本文件管理平台内置知识库（vault）：用 `embed.FS` 把 markdown 嵌入二进制，air-gapped/mainland-China 主机（github 不可达）也能开箱即用。设计要点：原子 temp + rename swap 物料化（崩溃后要么旧要么新，无混合态）；`builtin://vault` 哨兵 URL 标记 legacy repo 行供迁移清理。红线：嵌入文件不绕过 qdrant——向量仍需 embedder + qdrant，仅去除获取原始 docs 的网络依赖。

## 2. 包信息

- **包名**：`knowledge`
- **所属模块**：`internal/manager/biz/knowledge`
- **依赖方向**：被同包 `usecase.go` 的 `Sync`/`SyncBuiltinVault` 调用；依赖标准库 `embed`/`io/fs`/`os`/`path/filepath`

## 3. 关键类型与接口

```go
//go:embed all:builtin_vault
var builtinVaultFS embed.FS

const builtinVaultRoot = "builtin_vault"

// BuiltinVaultURL 是内置 vault 的哨兵 URL（非真实 git remote）
// 标记 legacy repo 行，PurgeBuiltinVaultRepo 据此迁移
const BuiltinVaultURL = "builtin://vault"

// BuiltinVaultGitURL 是平台 vault 的固定云端源（ADR-029）
// SyncBuiltinVault 先尝试运行时 clone 此 repo，嵌入快照是离线回退
// URL 刻意不可配置——vault 永远来自此 repo，clone 不进 Repos 表
const (
    BuiltinVaultGitURL = "https://github.com/ongridio/vault.git"
    BuiltinVaultBranch = "main"
)
```

## 4. 关键函数与流程

### `IsBuiltinVaultURL`
- **签名**：`func IsBuiltinVaultURL(url string) bool`
- **职责**：判断 url 是否是内置 vault 哨兵
- **流程**：`EqualFold(TrimSpace(url), BuiltinVaultURL)`

### `Usecase.materializeBuiltinVault`
- **签名**：`func (u *Usecase) materializeBuiltinVault(dir string) error`
- **职责**：把嵌入 vault 物料化到 dir，原子 temp + rename swap
- **流程**：
  1. `newCloneTmpDir(dir)` 创建 sibling tmp dir（同 FS，保证 rename 原子）
  2. `writeEmbeddedVault(tmp)` 写入嵌入文件
  3. `os.RemoveAll(dir)` 删除旧目录
  4. `os.Rename(tmp, dir)` 原子替换
- **错误处理**：每步失败清理 tmp；RemoveAll/Rename 失败带 `%w`
- **注释**：原子契约同 syncAtomicReplace——崩溃后要么旧要么新，无混合态

### `writeEmbeddedVault`
- **签名**：`func writeEmbeddedVault(destRoot string) error`
- **职责**：遍历嵌入 FS，剥离 `builtin_vault` 前缀后复制到 destRoot
- **流程**：
  1. `fs.WalkDir(builtinVaultFS, builtinVaultRoot, ...)`
  2. `filepath.Rel(builtinVaultRoot, p)` 剥离前缀（`builtin_vault/concepts/x.md` → `concepts/x.md`）
  3. 目录 → `os.MkdirAll(target, 0o755)`
  4. 文件 → `builtinVaultFS.ReadFile(p)` + `os.WriteFile(target, data, 0o644)`
- **注释**：剥离前缀后匹配 git clone 上游 repo 的目录结构（scanRepoFiles 期望）

## 5. 依赖关系

- **内部包**：无
- **外部库**：仅标准库 `embed`/`io/fs`
- **被调用方**：`usecase.go` 的 `Sync`（IsBuiltinVaultURL 分支）+ `SyncBuiltinVault`（嵌入回退）

## 6. 并发与资源管理

- **embed.FS 只读**：编译时嵌入，无运行时状态
- **原子 swap**：temp dir 在同 FS，rename 原子
- **无锁**：物料化是 Sync 调用，usecase.go 的 enrollmentLocks 不覆盖此路径（但 Sync 路径无并发需求）

## 7. 设计模式与亮点

- **embed.FS 嵌入**：air-gapped/mainland-China 主机开箱即用，注释明示旧 git clone 方案在 github 不可达时启动空库
- **原子 temp + rename swap**：崩溃后要么旧要么新，无混合态；与 syncAtomicReplace 同契约
- **`builtin://vault` 哨兵**：标记 legacy repo 行，PurgeBuiltinVaultRepo 迁移清理
- **BuiltinVaultGitURL 不可配置**：vault 永远来自固定 repo，clone 不进 Repos 表
- **前缀剥离**：物料化后目录结构匹配 git clone，scanRepoFiles 无感
- **云端优先 + 嵌入回退**：SyncBuiltinVault 先尝试 clone 云端，失败回退嵌入

## 8. 注意事项

- **`//go:embed all:builtin_vault`**：`all:` 前缀包含点开头的文件
- **重新 vendor**：上游 ongridio/vault 变化后用 `scripts/sync-builtin-vault.sh` 更新嵌入
- **嵌入不绕过 qdrant**：仍需 embedder + qdrant，仅去除获取原始 docs 的网络依赖
- **BuiltinVaultGitURL 固定**：不可配置，防 operator 误指向错误 repo
- **原子 swap 同 FS**：tmp dir 是 dir 的 sibling，保证 rename 原子
- **legacy 迁移**：PurgeBuiltinVaultRepo 用 IsBuiltinVaultURL 识别旧 repo 行
