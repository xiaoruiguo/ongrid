# usecase.go

## 1. 概述

`usecase.go` 是 knowledge 包的核心用例文件，承载所有面向 HTTP 的业务方法。它把关系型数据（`knowledge_repos` 表，存仓库注册元数据）与向量库（qdrant `ongrid_knowledge` collection，存文档正文 + 向量）这两条数据通路粘合起来，对外暴露：

- **Doc CRUD**：手工文档（`SourceManual`）、上传文件（`SourceUpload`）、仓库同步来的文档（`SourceRepo`）、内置 vault 文档（`SourceVault`）
- **Repo CRUD**：git 仓库注册表的增删查
- **Sync**：克隆/拉取仓库 → 扫描 → 切块 → 嵌入 → 替换 qdrant 点集
- **SyncBuiltinVault**：刷新内置平台 vault（云优先 + 嵌入式快照回退）
- **Search / ListDocs / ListPaths**：向量搜索 + 滚动列表 + 文件夹树聚合

文件刻意把"关系元数据"和"正文向量"分到两个存储：MySQL 只存小关系行；正文只活在 qdrant，不做双写。这样 sync 失败不会污染关系表，且 qdrant 的向量负载天然就是正文副本。

## 2. 包信息

- 包名：`knowledge`
- 路径：`internal/manager/biz/knowledge`
- 导入路径：`github.com/ongridio/ongrid/internal/manager/biz/knowledge`
- 包注释：明确两条流程（手工 docs / repo sync）+ 存储分工（关系表 vs qdrant）

## 3. 关键类型与接口

### RepoStore 接口（关系型窄面）

```go
type RepoStore interface {
    ListRepos(ctx) ([]*model.Repository, error)
    GetRepo(ctx, id) (*model.Repository, error)
    GetRepoByURL(ctx, url) (*model.Repository, error)
    CreateRepo(ctx, *model.Repository) error
    UpdateRepoSync(ctx, id, fileCount, syncErr) error
    DeleteRepo(ctx, id) error

    // SSH 身份（与 repo 同库，方便事务一致）
    ListSSHIdentities(ctx) ([]*model.SSHIdentity, error)
    GetSSHIdentity(ctx, id) (*model.SSHIdentity, error)
    CreateSSHIdentity(ctx, *model.SSHIdentity) error
    UpdateSSHIdentity(ctx, id, name, hostsJSON, knownHosts) error
    TouchSSHIdentityUsage(ctx, id) error
    DeleteSSHIdentity(ctx, id) error
}
```

### QdrantClient 接口（向量库窄面）

```go
type QdrantClient interface {
    EnsureCollection(ctx, name, dim) error
    EnsurePayloadIndex(ctx, collection, field, schema) error
    Upsert(ctx, collection, points) error
    DeleteByFilter(ctx, collection, mustMatch) error
    DeleteByID(ctx, collection, id) error
    GetPoints(ctx, collection, ids) ([]SearchHit, error)
    Search(ctx, collection, vector, opts) ([]SearchHit, error)
    Scroll(ctx, collection, opts) (*ScrollResult, error)
}
```

### Usecase 结构

```go
type Usecase struct {
    repo     RepoStore
    vec      QdrantClient
    embed    embedding.Embedder
    cloneDir string                       // 默认 /var/lib/ongrid/repos
    log      *slog.Logger
    onRepoDelete func(ctx, url string)    // 删 repo 后的钩子（用于持久化 vault_seed_optout 哨兵）
}
```

### 输入 DTO

- `CreateManualDocInput` / `UpdateManualDocInput` / `UploadDocInput`
- `CreateRepoInput`

### 重导出（让外部调用方留在外部 biz 别名）

```go
type (
    ListDocsFilter = model.ListDocsFilter
    SearchOptions  = model.SearchOptions
    SearchHit      = model.SearchHit
)
```

### 常量

```go
const CollectionName = "ongrid_knowledge"  // 唯一的 qdrant collection
```

## 4. 关键函数与流程

### New（启动构造）

启动时同步调用 `EnsureCollection`，让读路径在全新安装（没配 LLM key）下也能渲染列表。维度选择：
1. 有 embedder → `embed.Dim()`
2. 无 embedder → 读 `ONGRID_EMBEDDING_DIM` env
3. 都没有 → 默认 1536

随后对 `source_type` / `repo_id` / `path` / `path_prefixes` / `tags` 建 keyword 索引（幂等）。注释解释了为何不用 qdrant 文本分词器：中文 "网络" 会被宽松分词误匹配 "K8s/网络"。`id_alias` 故意不建索引 —— doc ID 是 md5 派生的全 uint64，qdrant 过滤器拒绝 > int64 的值。

### CreateManualDoc

校验 → 嵌入 → upsert 单点。`title` 截断到 256；ID 由 `manualDocID(title)`（md5 派生）决定，因此同一 title 重写覆盖。

### UploadDoc + ingestUpload

`UploadDoc` 接受已解码 UTF-8 文本，`ingestUpload` 是共享的核心：
1. **先 sweep**：`DeleteByFilter(source_type=upload, url=<filename>)` 清掉旧版本的所有 chunk（处理文件变短留下高序号 chunk 的情况）
2. `splitForChunks` 切块
3. 每批 32 个文本 → `embed.Embed` → `uploadChunkPoint` 生成点 → `Upsert`
4. 第 0 块的 body 前加 `title + "\n\n"` 让向量吸收"这是什么文档"的信号
5. 返回 head chunk id 作为逻辑 doc id

### UpdateManualDoc

按 `existing.SourceType` 分支：
- `SourceManual` → 直接改字段后 `upsertDoc`
- `SourceUpload` → 复用 `ingestUpload` 重新切块嵌入（url 不可改，作为稳定身份；CreatedAt 保留）
- 其它（vault/repo）→ 拒绝，引导操作员重新同步源

### DeleteDoc

按 `SourceType` 分支：
- `SourceManual` → `DeleteByID`（单点）
- `SourceUpload` → `DeleteByFilter(source_type=upload, url=<filename>)`（多 chunk）
- 其它 → 拒绝，引导操作员取消注册整个源

### ListDocs + dedupeByIDAlias

历史坑：原本 qdrant 层 `MustMatch chunk_index=0`，但 manual doc 的 upsert 路径曾忘记设 `chunk_index`，导致老数据对严格过滤器不可见（操作员的反馈是"RAG 又没了"）。

新方案：scroll 时不带 `chunk_index` 过滤，Go 端按 `id_alias` 去重，**优先保留 head chunk**（`chunk_index==0` 或字段缺失）。`scanLimit = limit * 8`（capped 10000）补偿非 head chunk 占用的扫描配额，让 `limit` 仍代表逻辑文档数。

`dedupeByIDAlias` 的精妙点：达到 `limit` 后**继续扫描**剩余点，因为后续可能出现某个已收录 doc 的 head chunk，需要升级对应槽位。

### ListPaths

文件夹树聚合：scroll 全部点 → 去重 → 按 `path` 计数；若 doc 无 `path`（repo 文档从来不显式设 path），从 `URL` 的目录部分派生（`reference/external/dns/foo.md` → 计入 `reference/external/dns`）。这让 SPA 树视图对 repo 文档不再空白。

### Search

query 嵌入 → qdrant cosine top-K（带 server-side 过滤）。`overFetch = limit * 5`（capped 200）应对长文档多 chunk 占据头部命中。Go 端按 `parent_url`（chunked 文档）或 `url`（单 chunk）去重，保留最高分。

### Sync（仓库同步主流程）

1. 校验 embedder 已配置
2. 取 repo 行 + 准备 `repoDir(id)`
3. **builtin vault 分支**：`IsBuiltinVaultURL(repo.URL)` → `materializeBuiltinVault(dir)`，本地物料化嵌入式 vault，不走 git/网络。这是 air-gapped / 国内主机（github 慢）的关键
4. **git 分支**：
   - `buildGitAuthEnv` 构造 git 环境（**绝不**把 token 拼进 URL —— 会从 argv/stderr 泄漏）。GitHub PAT 走 `GIT_ASKPASS` 脚本，token 只进 spawn 的环境变量，脚本本身 `printenv` 另一个变量；SSH URL 走 `buildSSHEnv`，物料化临时 keyfile
   - `purgeStaleCloneTmps(dir)` 清理上次崩溃留下的 `.tmp-clone-*` 兄弟目录
   - **fast path**（健康 `.git` 存在）：`fetch + reset --hard FETCHHEAD` 原地更新；任何失败 fall through 到 slow path
   - **slow / repair path**：`clone --depth=1` 到随机 tmp 目录 → 成功后 `os.Rename(tmp, dir)` 原子替换。同一文件系统内 rename 原子，崩溃要么留旧目录要么留新目录，绝不混合
5. `scanRepoFiles(dir)` 扫描 `.md/.txt/.rst/.yaml/.yml/.toml/.json`
6. `DeleteByFilter(source_type=repo, repo_id=id)` 先清旧点集（宁愿显示"0 indexed + last_sync_error"也不要新旧混在一起）
7. 切块（`splitForChunks`）→ 每文件 1+ chunk，第 0 块加 title 前缀
8. 批量嵌入（每批 32）→ `repoChunkPoint` 生成点 → `Upsert`
9. `UpdateRepoSync(id, file_count, "")` 记录成功（file_count 是去重后的文件数，不是 chunk 扇出数）

### DeleteRepo

删除前**快照 URL**：forensic 日志能记录被删的是什么（操作员抱怨过"知识静默丢失无法追踪"）；`onRepoDelete` 钩子能持久化 `vault_seed_optout` 哨兵 scoped 到该 URL。

顺序：qdrant 删点 → repo 删行 → 磁盘删目录 → 日志 → 钩子。任一步失败只 warn 不阻断后续。

### SyncBuiltinVault

刷新内置 vault（`source_type="vault"`），**不是** `knowledge_repos` 行（vault 是平台内容，不出现在 Repos 列表）。

ADR-029 cloud-first：
1. `fetchCloudVault` 尝试克隆公开 github vault
2. 失败 → 回退到嵌入式快照（`materializeBuiltinVault`）
3. 失败永不冒泡为按钮错误 —— 总是至少给出 38 文件基线

返回 `(fileCount, source, err)`，`source` 是 `"cloud"` 或 `"embedded"`，让 UI 告诉操作员这次刷新是否真的连上了云。

### 辅助函数

- `normalizePath` / `pathPrefixes` / `normalizeTags`：路径与标签归一化
- `manualDocID` / `uploadChunkDocID` / `repoChunkPoint` / `vaultChunkPoint`：点 ID 与 payload 构造（实现在同包其它文件）
- `splitForChunks` / `truncateForEmbedding`：切块与嵌入前截断
- `buildGitAuthEnv` / `buildSSHEnv` / `syncFastPath` / `syncAtomicReplace` / `runGitWithRetry` / `isTransientGitErr` / `purgeStaleCloneTmps` / `annotateGitError`：git 同步与错误处理（实现在同包其它文件）
- `payloadToDoc` / `docIDAlias` / `chunkIndexFromPayload` / `ptrU64`：payload 与 model 互转

## 5. 依赖关系

### 外部包

- `crypto/md5` + `encoding/binary` —— doc ID 派生
- `encoding/json` —— payload 字段类型断言
- `io/fs` / `os` / `os/exec` / `path/filepath` —— 文件系统、git 执行
- `log/slog` —— 结构化日志
- `strconv` / `strings` / `time` —— 工具

### 内部包

- `model "github.com/ongridio/ongrid/internal/manager/model/knowledge"` —— 模型（Doc / Repository / SSHIdentity / 各种 Source* 常量）
- `github.com/ongridio/ongrid/internal/pkg/embedding` —— Embedder 接口
- `github.com/ongridio/ongrid/internal/pkg/errs` —— 错误哨兵
- `github.com/ongridio/ongrid/internal/pkg/qdrantx` —— qdrant 客户端与 Point/ScrollOpts 等类型

### 被谁调用

- HTTP handler（`/api/v1/knowledge/*`）调所有公开方法
- `main.go` 调 `New` + `WithRepoDeleteHook` + `EnsureRepoSeed` + `SyncBuiltinVault`（boot 时）

## 6. 并发与资源管理

- **无锁**：`Usecase` 本身无共享可变状态；所有并发安全由 `RepoStore` 与 `QdrantClient` 实现保证
- **无 goroutine**：所有方法同步；sync 是长操作但调用方（HTTP handler）按需起 context 超时
- **defer cleanup**：`buildGitAuthEnv` 返回 `askpassCleanup`，`Sync` 用 `defer askpassCleanup()` 确保临时 askpass 脚本被清理
- **批量上限**：嵌入批 32、scroll 上限 10000、search over-fetch 上限 200，防止大仓库压爆内存或 LLM 上下文
- **context 传播**：所有 IO 函数首参 `ctx`，符合 gospec 红线

## 7. 设计模式与亮点

### 双存储分工

MySQL 存关系元数据（小、强一致、可事务），qdrant 存正文+向量（大、最终一致、search target）。**无双写**：sync 失败不会污染关系表；qdrant 的 payload 天然含正文副本。

### 原子 temp + rename swap

sync slow path 的核心：clone 到随机 tmp 目录 → 成功后 `os.Rename(tmp, dir)`。同一 FS 内 rename 原子，崩溃语义清晰（要么旧要么新，绝不混合）。fast path 失败 fall through 到 slow path，最坏只是浪费带宽。

### Chunk 与去重协同

- 切块让长文档的尾部内容也能被语义搜索命中
- `parent_url` + `chunk_index` payload 让 listing 能折叠成"一文件一条目"
- `dedupeByIDAlias` 在 Go 端做去重，绕过 qdrant 过滤器对老数据（缺 `chunk_index` 字段）的兼容问题
- `ListPaths` 用 URL 目录派生 path，让 repo 文档也进文件夹树

### 鉴权不进 argv

`buildGitAuthEnv` 注释强调：token 永远不拼进 URL（会从 `ps` / `/proc/<pid>/cmdline` / 捕获的 stderr 泄漏）。GitHub PAT 走 `GIT_ASKPASS` 脚本 + 独立 env var；SSH 走临时 keyfile + `GIT_SSH_COMMAND`。

### 删除前快照 URL

`DeleteRepo` 在删行前先读出 URL。两个用途：
1. forensic 日志记录被删的是什么（操作员曾抱怨"知识静默丢失无法追踪"）
2. `onRepoDelete` 钩子能持久化 scoped 到该 URL 的 `vault_seed_optout` 哨兵

### 内联注释解释"为什么"

通篇注释解释设计意图而非行为：为什么不用 qdrant 文本分词器、为什么 `id_alias` 不建索引、为什么 fast path 失败要 fall through、为什么 sync 错误存原始 git 输出而非预本地化字符串（SPA 按 locale 分类展示）。这些注释是未来维护者的护身符。

### Cloud-first 容错

`SyncBuiltinVault` 总是先尝试云克隆，失败静默回退到嵌入式快照。操作员永远拿到至少基线内容，按钮永远不会因网络红。

## 8. 注意事项

- **embedder 缺失时的契约**：`New` 接受 `embed == nil`（让列表端点在全新安装下渲染），但 `CreateManualDoc` / `UploadDoc` / `Sync` / `Search` / `MoveDoc` 都会返回 `errs.ErrNotWiredYet`。这是刻意的渐进式启动设计
- **chunk_index 历史包袱**：老 manual doc 可能缺 `chunk_index` 字段。`dedupeByIDAlias` 与 `ListPaths` 都做了兼容（缺字段视为 head chunk）。不要再加回 qdrant 层的 `chunk_index=0` 严格过滤
- **sync 错误存原始 git 输出**：`recordSyncFailure` 存的是 git 的英文 stderr，不是预本地化字符串。SPA 端 `gitErrorHint` 按 locale 分类展示。改这里会让英文模式显示中文
- **builtin vault 不是 repo 行**：`SyncBuiltinVault` 不碰 `knowledge_repos` 表；vault 不出现在 Repos 列表。删除 repo 不会影响 vault
- **GitHub PAT 路径将被移除**：注释提到 HTTPS git auth 的 P3 走 `credential.helper`，P1+P2 的 SSH identities 覆盖现实场景。GitHub-only PAT 代码路径在 P3 落地后会被删
- **Fast path 失败必须 fall through**：`syncFastPath` 返回 false 时调 `syncAtomicReplace`。不要把 fast path 失败直接返回错误 —— 那会丢失 slow path 的修复能力
- **DeleteByFilter 先于 Upsert**：sync 流程先清旧点再写新点。中间窗口搜索会返回空。若改成"先写后清"会临时出现新旧混合
- **`onRepoDelete` 钩子可能为 nil**：测试不注入时调用前要判空（代码已判）
- **scanLimit 计算**：`limit * 8` capped 10000。若未来 collection 涨到几万点，这个 cap 会让 `ListDocs` 的 limit 失真 —— 需要重新评估
