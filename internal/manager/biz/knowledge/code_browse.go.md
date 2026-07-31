# `code_browse.go` 技术实现文档

> 源文件：`internal/manager/biz/knowledge/code_browse.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/knowledge`

## 1. 概述

本文件提供只读访问 git clone 的代码浏览能力（HLD-012）：ListRepoSources / ReadSource / GrepSource。设计要点：纯 git plumbing（`ls-tree`/`cat-file`/`grep`）工作于 bare/no-checkout clone；path-traversal 防护（`safeRepoPath`/`cleanRepoRel`）；二进制文件跳过；结果大小/数量上限保护 LLM context。红线：read-only + sandboxed 到单 repo clone dir，绝不越界。

## 2. 包信息

- **包名**：`knowledge`
- **所属模块**：`internal/manager/biz/knowledge`
- **依赖方向**：被 agent tool 调用；依赖 `model/knowledge`、`pkg/errs`、标准库 `os/exec`

## 3. 关键类型与接口

```go
const (
    maxSourceFileBytes = 512 << 10  // 512 KiB 单文件上限
    maxGrepHits        = 200        // grep 结果上限
    maxListEntries     = 500        // listing 上限
    codeGrepTimeout    = 30 * time.Second
)

type RepoSourceEntry struct {
    Path  string `json:"path"`
    IsDir bool   `json:"is_dir"`
    Size  int64  `json:"size,omitempty"`
}

type RepoSourceListing struct {
    Repo      string            `json:"repo"`
    RepoID    uint64            `json:"repo_id"`
    Subpath   string            `json:"subpath"`
    Entries   []RepoSourceEntry `json:"entries"`
    Truncated bool              `json:"truncated,omitempty"`
}

type SourceFile struct {
    Repo                         string `json:"repo"`
    RepoID                       uint64 `json:"repo_id"`
    Path                         string `json:"path"`
    StartLine, EndLine           int
    Content                      string `json:"content"`
    Truncated                    bool   `json:"truncated,omitempty"`
}

type GrepHit struct {
    Path string `json:"path"`
    Line int    `json:"line"`
    Text string `json:"text"`
}

type GrepResult struct {
    Repo      string    `json:"repo"`
    RepoID    uint64    `json:"repo_id"`
    Pattern   string    `json:"pattern"`
    Hits      []GrepHit `json:"hits"`
    Truncated bool      `json:"truncated,omitempty"`
}
```

## 4. 关键函数与流程

### `Usecase.resolveRepoClone`
- **签名**：`func (u *Usecase) resolveRepoClone(ctx, ref string) (*model.Repository, string, error)`
- **职责**：把 ref（numeric id / exact URL / 唯一大小写不敏感子串）映射到 repo 行 + clone dir
- **流程**：
  1. `repo.ListRepos`；空 → ErrNotFound（提示去代码仓库页添加）
  2. ref 是数字 → 按 ID 匹配
  3. exact URL 匹配（EqualFold）
  4. 唯一子串匹配（ToLower + Contains）；0 匹配 → ErrNotFound；多匹配 → ErrInvalid（提示更具体）
  5. `repoDir(matched.ID)`；`os.Stat` 非 dir → ErrInvalid（提示先 sync）
- **错误处理**：每类匹配失败有明确提示

### `safeRepoPath`
- **签名**：`func safeRepoPath(root, rel string) (string, error)`
- **职责**：join rel 到 root 并保证不越界
- **流程**：`filepath.Clean("/" + rel)` 折叠 `../`；`filepath.Join(root, clean)`；`filepath.Abs` 比较前缀
- **错误处理**：越界 → ErrInvalid "path escapes repo"

### `cleanRepoRel`
- **签名**：`func cleanRepoRel(p string) (string, error)`
- **职责**：规范化 repo-relative path 供 git pathspec
- **流程**：`ToSlash` + 去前导 `/` + 拒绝 `..` 段

### `looksBinary`
- **签名**：`func looksBinary(b []byte) bool`
- **职责**：前 8000 字节含 NUL 即判二进制（git 自己的 -I 启发式）

### `Usecase.ListRepoSources`
- **签名**：`func (u *Usecase) ListRepoSources(ctx, ref, subpath string) (*RepoSourceListing, error)`
- **职责**：列 HEAD tree 一层（like `ls`），dirs first 然后 files，alpha 排序
- **流程**：
  1. `resolveRepoClone`
  2. `git ls-tree -l HEAD [subpath/]`（subpath 空 = repo root）
  3. 解析 `<mode> <type> <sha> <size>\t<path>`；tree=dir，blob=file
  4. cap `maxListEntries`，超了 Truncated=true
  5. subpath 非空且 0 entries → ErrNotFound
  6. 排序：dirs first，alpha
- **错误处理**：git 失败 `%w`；subpath 不存在 ErrNotFound

### `Usecase.ReadSource`
- **签名**：`func (u *Usecase) ReadSource(ctx, ref, path string, startLine, endLine int) (*SourceFile, error)`
- **职责**：读文件文本，支持行窗口
- **流程**：
  1. `resolveRepoClone` + `cleanRepoRel`
  2. `git cat-file blob HEAD:<rel>`（纯 plumbing，bare clone 可用）
  3. 超 maxSourceFileBytes 截断 + Truncated=true
  4. `looksBinary` → ErrInvalid（text-only）
  5. startLine<=0 → 全文；否则按行窗口切片
- **错误处理**：`not a blob` → ErrInvalid（提示用 list）；not found → ErrNotFound；start_line 过 EOF → ErrInvalid

### `Usecase.GrepSource`
- **签名**：`func (u *Usecase) GrepSource(ctx, ref, pattern, pathGlob string, max int) (*GrepResult, error)`
- **职责**：`git grep` HEAD tree，basic-regex，pathGlob 可选收窄
- **流程**：
  1. pattern 空 → ErrInvalid
  2. `resolveRepoClone`；max clamp 到 maxGrepHits
  3. `git grep -n -I --no-color -e <pattern> HEAD [-- <pathGlob>]`
  4. exit code 1（无匹配）→ 空 result（非错误）
  5. 解析 `HEAD:path:line:text`，剥 `HEAD:` 前缀
  6. cap max，超了 Truncated=true
- **错误处理**：pathGlob `safeRepoPath` 校验防越界

## 5. 依赖关系

- **内部包**：`model/knowledge`（Repository）、`pkg/errs`
- **外部库**：仅标准库 `os/exec`
- **被调用方**：agent tool（list_repo_sources / read_source / grep_source）

## 6. 并发与资源管理

- **ctx 透传 + 超时**：所有 git 命令 `context.WithTimeout(ctx, codeGrepTimeout=30s)`
- **`GIT_TERMINAL_PROMPT=0`**：防 git 挂起等 stdin
- **无锁**：读 git 仓库无状态，多 goroutine 并发读安全
- **exec.CommandContext**：ctx 超时 kill 进程

## 7. 设计模式与亮点

- **纯 git plumbing**：`ls-tree`/`cat-file blob`/`grep HEAD` 工作于 bare/no-checkout clone，无需 working tree
- **path-traversal 双重防护**：`safeRepoPath`（FS 层）+ `cleanRepoRel`（git pathspec 层）
- **二进制跳过**：`looksBinary` NUL 检测，防 LLM context 污染
- **结果上限**：maxListEntries=500 / maxGrepHits=200 / maxSourceFileBytes=512KiB
- **ref 多态解析**：numeric id / exact URL / 唯一子串，多匹配报错提示更具体
- **exit code 1 非错误**：git grep 无匹配返回 1，正确处理
- **行窗口**：startLine/endLine 支持 LLM 按需读片段

## 8. 注意事项

- **maxSourceFileBytes=512KiB**：单文件上限，bounds RAM + LLM context
- **maxGrepHits=200 / maxListEntries=500**：防大 repo 吹爆 LLM context
- **codeGrepTimeout=30s**：大 tree grep 上限
- **GIT_TERMINAL_PROMPT=0**：防挂起
- **bare clone 友好**：纯 plumbing，不依赖 working tree
- **path-traversal 双防护**：FS 层 + git pathspec 层
- **二进制拒绝**：text-only，防 LLM 污染
- **exit code 1 处理**：grep 无匹配非错误
