# `model.go` 技术实现文档

> 源文件：`internal/manager/model/knowledge/model.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/model/knowledge`

## 1. 概述

本文件是用户面向知识库 + git repo 集成的 data model。schema 故意简单：`knowledge_repos`（git repo 注册）+ `ssh_identities`（SSH 私钥 + 允许主机）落 MySQL；`knowledge_docs` 文档本体存 qdrant（向量搜索），本文件的 `Doc` 是 biz 层 in-memory 形状。设计要点：Phase-2 将加 embeddings（独立 knowledge_chunks 表 + 向量列），保持本文件为 source of truth 让加向量层是 additive 不 rewrite。红线：`PrivateKey` / `Passphrase` 在 data 层 AES 加密后才入库，永不日志；`Doc.ID` 是 qdrant point id（非 MySQL 自增）。

## 2. 包信息

- **包名**：`knowledge`
- **所属模块**：`internal/manager/model/`
- **依赖方向**：被 `manager/biz/knowledge` 与 `manager/data/knowledge/store` 调用；依赖 `time`

## 3. 关键类型与接口

```go
// SourceType 常量
const (
    SourceManual = "manual" // 用户 /v1/knowledge POST 粘贴
    SourceRepo   = "repo"   // 从 git repo 文件自动导入
    SourceURL    = "url"    // 未来 Phase-2：URL 抓取
    SourceVault  = "vault"  // 平台内置 vault，非 knowledge_repos 行
    SourceUpload = "upload" // 组织上传文件（ADR-028），org-owned
)

type Repository struct {
    ID            uint64    `gorm:"primaryKey;autoIncrement"`
    URL           string    `gorm:"size:512;not null;uniqueIndex:idx_repo_url"`
    Branch        string    `gorm:"size:128;not null;default:main"`
    Description   string    `gorm:"size:512"`
    LastSyncedAt  *time.Time `gorm:"column:last_synced_at"`
    LastSyncError string    `gorm:"type:text;column:last_sync_error"`
    FileCount     int       `gorm:"column:file_count"`
    CreatedAt     time.Time
    UpdatedAt     time.Time
}

type SSHIdentity struct {
    ID           uint64    `gorm:"primaryKey;autoIncrement"`
    Name         string    `gorm:"size:128;not null;uniqueIndex:uk_ssh_name"`
    PrivateKey   string    `gorm:"type:text;not null;column:private_key"` // data 层 AES 加密
    PublicKey    string    `gorm:"type:text;not null;column:public_key"`
    Fingerprint  string    `gorm:"size:128;not null"` // SHA256:xxx from PublicKey
    Passphrase   string    `gorm:"type:text;column:passphrase"` // nullable；MVP 拒绝非空
    HostsJSON    string    `gorm:"type:text;not null;column:hosts"` // JSON array of host glob
    KnownHosts   string    `gorm:"type:text;not null;column:known_hosts"` // MySQL TEXT 无 DEFAULT
    LastUsedAt   *time.Time `gorm:"column:last_used_at"`
    CreatedAt    time.Time
    UpdatedAt    time.Time
}

// Doc 是 in-memory 形状，qdrant point id 为 ID
type Doc struct {
    ID         uint64
    SourceType string
    RepoID     *uint64
    URL        string
    Title      string     // 原语言
    TitleEN    string     // 可选英文 overlay
    Content    string
    Path       string     // "/"-分隔；空 = root
    Tags       []string
    CreatedAt  time.Time
    UpdatedAt  time.Time
}

type ListDocsFilter struct {
    SourceType string
    RepoID     *uint64
    Path       string
    PathPrefix string
    Tag        string
    Limit      int
}

type SearchOptions struct {
    Path       string
    PathPrefix string
    Tags       []string // any-match
    Limit      int
}

type SearchHit struct {
    Doc   *Doc
    Score float64
}
```

## 4. 关键函数与流程

### `Repository.TableName`
- **签名**：`func (Repository) TableName() string`
- **职责**：固定表名 `knowledge_repos`

### `SSHIdentity.TableName`
- **签名**：`func (SSHIdentity) TableName() string`
- **职责**：固定表名 `ssh_identities`

## 5. 依赖关系

- **内部包**：无（Doc 是 in-memory，不与 qdrant 客户端耦合）
- **外部库**：`time`
- **被调用方**：`manager/biz/knowledge` 的 service / Search；`manager/data/knowledge/store` 的 CRUD；`manager/biz/git` 的 SSH 解析

## 6. 并发与资源管理

- 本文件仅定义 schema，无锁 / channel / 缓存
- `Repository` / `SSHIdentity` 无软删（永久审计）
- `Doc` 是 in-memory 形状不落 DB

## 7. 设计模式与亮点

- **schema 故意简单**：仅 knowledge_repos + ssh_identities 落 MySQL；doc 本体在 qdrant
- **Phase-2 加向量层 additive**：保持本文件 source of truth；新增 knowledge_chunks 表不 rewrite
- **5 个 SourceType 区分来源**：manual / repo / url / vault / upload；vault 不在 knowledge_repos 表中
- **Vault vs Upload 区分**：vault 是平台内置（同步进 qdrant 但不在用户 repos 列表）；upload 是 org-owned（完整 CRUD）
- **Repository.LastSyncError**：同步失败原因；UI 显示帮助调试
- **SSHIdentity.HostsJSON glob**：支持 `git.acme.*` 通配；clone 时按 host 匹配
- **SSHIdentity 自包含**：不 FK 到独立 keys 表；"少量 key per deployment" 用 flat row 最简
- **PrivateKey / Passphrase AES 加密**：data 层 `pkg/secretbox` 加密后才入库；永不日志
- **Fingerprint SHA256:xxx**：从 PublicKey 派生；UI 显示帮助识别
- **KnownHosts NOT NULL 无 default**：MySQL 禁 TEXT DEFAULT；biz 总写值
- **Title + TitleEN 双语**：中文 vault 不强制翻译；英文用户读 TitleEN overlay；空 = fallback Title
- **Path 作为 folder tree**：`/`-分隔 breadcrumb；既是 SPA 树视图又是 LLM filter
- **Tags 多标签**：与 Path 正交；qdrant payload 索引
- **SearchOptions payload 过滤前置**：Path / PathPrefix / Tags 在 cosine 之前过滤，提升精度
- **Doc.ID 是 qdrant point id**：非 MySQL 自增；biz 层生成

## 8. 注意事项

- **Doc 不落 MySQL**：仅 Repository / SSHIdentity 落 DB；doc 本体在 qdrant payload
- **Passphrase MVP 拒绝非空**：当前仅支持无 passphrase 的 key；未来扩展
- **Repository.URL 唯一**：跨未软删行；同 URL 重复注册需先删除
- **SSHIdentity.Name 唯一**：跨所有行
- **HostsJSON 必填**：biz 层应至少写 `[]`
- **KnownHosts 必填**：写入前应 populate；空表示无限制（不安全）
- **LastSyncedAt 可空**：未同步时 NULL
- **FileCount**：sync 后更新；UI 显示 repo 健康度
- **ListDocsFilter.Path vs PathPrefix**：Path 精确匹配；PathPrefix 匹配 breadcrumb root（如 "网络/" 匹配 "网络/DNS"）
- **SearchOptions.Tags any-match**：doc 包含任一 tag 即通过
- **SearchHit.Score**：cosine 相似度；0-1
- **Phase-2 向量层 additive**：未来加 knowledge_chunks 表带向量列；本文件不变
