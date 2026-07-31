# `databasemetrics/secrets.go` 技术实现文档

> 源文件：`internal/edgeagent/plugins/databasemetrics/secrets.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/plugins/databasemetrics`

## 1. 概述

本文件实现 manager→edge 的数据库凭证一次性下发机制。`RegisterSecretHandler` 在 tunnel client 上注册 `MethodWriteDatabaseMetricsSecret` 处理器，manager 保存 source 时通过该 RPC 把凭证写入 edge 本地受管 secret 文件（`/var/lib/ongrid-edge/secrets/...`）。安全要点：路径强校验必须在允许目录内、拒绝符号链接、文件权限 0600、原子写入（temp + rename）+ 备份回滚、批量写入按 plan-stage-commit 三阶段事务执行。

同时提供凭证内容生成：根据 db_type 与 credentials map 构造各 exporter 期望的凭证格式（mysql 的 `[client]` ini、postgres/redis/mongo 的 URI）。

## 2. 包信息

- **包名**：`databasemetrics`
- **所属模块**：`internal/edgeagent/plugins/databasemetrics`
- **依赖方向**：被 main 调用 `RegisterSecretHandler` 注册 RPC handler；调用 `internal/pkg/tunnel`（Client/Method/Request/Response 类型）

## 3. 关键类型与接口

```go
const (
    secretBaseDir    = "/var/lib/ongrid-edge/secrets"
    maxSecretContent = 16 << 10 // 16KiB
)

type edgeDBCredentials struct {
    Host, Port, Username, Password, Database, SSLMode, AuthSource string
    TLS edgeDBTLSConfig
}

type edgeDBTLSConfig struct {
    Enabled, SkipVerify bool
    CAFile, CertFile, KeyFile string
}

// 写入事务中间态
type managedSecretWritePlan struct{ path, content string; delete bool }
type stagedManagedSecretWrite struct {
    path, tmpPath, backupPath string
    hadOriginal, installed, deleteOnly bool
}
```

## 4. 关键函数与流程

### `RegisterSecretHandler(client, log)`
- **职责**：在 tunnel client 上注册 handler。
- **流程**：handler 内 `decodeWriteDatabaseMetricsSecretsRequest(body)` → `writeManagedSecrets(ctx, reqs)` → 逐个 log Info（written/deleted）→ 返回 `{OK: true}`。
- **错误处理**：解码/写入失败返回错误，tunnel 层转 RPC error。

### `decodeWriteDatabaseMetricsSecretsRequest(body) ([]Request, error)`
- **职责**：兼容批量（`{secrets: [...]}`）与单条（`{path, content, ...}`）两种 wire 形态。
- **流程**：先尝试批量 → 若 `secrets` 非空返回；否则尝试单条 → 单条全空且不 preserve 报 "secrets required"。

### `writeManagedSecrets(ctx, reqs) error`
- 转调 `writeManagedSecretsInBase(ctx, secretBaseDir, reqs)`。

### `writeManagedSecretsInBase(ctx, baseDir, reqs) error`
- **职责**：三阶段事务写入。
- **流程**：
  1. `planManagedSecretWrites`：校验每个 req 的路径、内容/删除语义、preserve_password 处理；产出 plan 列表
  2. `stageManagedSecretWrites`：为每个 plan 创建 temp 文件写入内容（删除 plan 跳过），任一失败 `cleanupStagedManagedSecrets` 后返回
  3. `commitManagedSecretWrites`：逐个备份原文件→rename temp→目标；任一失败 `rollbackManagedSecretWrites` 回滚已提交项；全部成功后 `cleanupCommittedManagedSecretBackups`
- **错误处理**：每步失败都 join 多个错误（`errors.Join`），保留原始错误信息。

### `planManagedSecretWrites(ctx, baseDir, reqs) ([]plan, error)`
- **职责**：校验 + 生成写入计划。
- **流程**：遍历 reqs：
  1. ctx.Err() 检查
  2. `validateManagedSecretTarget(baseDir, req.Path)`：`cleanManagedSecretPath` 校验在 baseDir 内 + `os.Lstat` 拒绝符号链接
  3. 同批次内 path 重复检测（`seenPaths`）
  4. delete 请求：禁止带 content/preserve_password
  5. 非 delete：content 空 + preserve_password → `buildManagedSecretPreservingPasswordInBase` 从现有文件提取 password 重建 content；content 仍空报错；content > 16KiB 报错
- **错误处理**：返回错误带 "write database metrics secret:" 前缀。

### `buildManagedSecretPreservingPasswordInBase(baseDir, req) (string, error)`
- **职责**：preserve_password 模式下从现有 secret 文件提取 password，与新 credentials 合并重建。
- **流程**：校验 db_type → cleanPath → 读现有文件 → `extractExistingDatabasePassword(dbType, current)` → credentials 若缺 password 则填入 → `buildEdgeDatabaseMetricsSecret(dbType, credentials)`。

### `extractExistingDatabasePassword(dbType, content) (string, error)`
- **职责**：从现有 secret 内容提取 password。
- **流程**：mysql 解析 `password=` 行；其他类型解析 URI 的 `u.User.Password()`。

### `buildEdgeDatabaseMetricsSecret(dbType, credentials) (string, error)`
- **职责**：根据 db_type 构造 exporter 凭证字符串。
- **流程**：
  1. 从 credentials map 提取 host/port/username/password/database/sslmode/auth_source/tls_* 字段（`edgeMapString`/`edgeMapBool`）
  2. `normalizeEdgeDBCredentials`：SkipVerify 时强制 TLS.Enabled、清空 CA/Cert/Key、postgres 设 sslmode=require
  3. host 必填；port 校验 1..65535；mongodb 禁止 tls_key_file（要求合并 PEM）
  4. 按 db_type 分支：
     - mysql → `buildEdgeMySQLSecret`（`[client]` ini 格式，含 TLS 字段）
     - postgresql → `buildEdgePostgresDSN`（postgresql:// URI，含 sslmode/sslrootcert/sslcert/sslkey）
     - redis → `buildEdgeRedisURI`（redis/rediss:// URI）
     - mongodb → `buildEdgeMongoURI`（mongodb:// URI，含 authSource/tls/tlsCAFile/tlsCertificateKeyFile）

### `stageManagedSecretWrite(plan) (staged, error)`
- **职责**：为单个 plan 创建 temp 文件并写入内容。
- **流程**：delete plan 直接返回 deleteOnly=true → `os.MkdirAll(dir, 0o700)` → `os.CreateTemp(dir, ".ongrid-secret-*")` → 写 content + 补换行 → `Chmod(0o600)` → `Sync` → `Close`。失败用闭包 `fail` 清理 temp 并 join 错误。

### `commitManagedSecretWrites(ctx, staged) error`
- **职责**：原子提交所有 staged 写入。
- **流程**：遍历 staged：
  1. ctx.Err() 检查 → 失败则 `rollbackManagedSecretWrites` 回滚
  2. deleteOnly：若目标存在，拒绝符号链接，`reserveManagedSecretBackupPath` 创建 backup 占位 → `os.Rename(path, backupPath)` 记录 hadOriginal
  3. 非 delete：若目标存在，同上备份；`os.Rename(tmpPath, path)` 安装；标记 installed + 清空 tmpPath
  4. 全部成功 `cleanupCommittedManagedSecretBackups`
- **错误处理**：每步失败 join 原始错误 + rollback 错误。

### `rollbackManagedSecretWrites(staged) error`
- **职责**：回滚已提交的写入。
- **流程**：逆序遍历：installed 项删除已安装文件 → hadOriginal 项 `Rename(backupPath, path)` 恢复 → tmpPath 非空项删除 temp。所有错误 join 返回。

### 路径校验
- `cleanManagedSecretPath(baseDir, path)`：必须绝对路径；`filepath.Rel(baseDir, path)` 结果不能是 `.`/`..`/以 `..` 开头/绝对路径——即 path 必须严格在 baseDir 之下。
- `validateManagedSecretTarget`：`cleanManagedSecretPath` + `os.Lstat` 拒绝符号链接。

### 辅助函数
- `reserveManagedSecretBackupPath(dir)`：CreateTemp 创建占位 → Close → Remove → 返回路径（用于后续 Rename 覆盖）
- `removeManagedSecretFile(path, action)`：Remove，IsNotExist 视为成功
- `cleanupStagedManagedSecrets`/`cleanupCommittedManagedSecretBackups`：best-effort 清理 temp/backup
- `edgeDatabaseMetricsDBTypeSupported`：白名单 mysql/postgresql/redis/mongodb
- `edgeMapString`/`edgeMapStringDefault`/`edgeMapBool`/`edgeFirstNonEmptyString`：credentials map 提取助手

## 5. 依赖关系

- **内部包**：`internal/pkg/tunnel`（Client/MethodWriteDatabaseMetricsSecret/WriteDatabaseMetricsSecretRequest/WriteDatabaseMetricsSecretsRequest/WriteDatabaseMetricsSecretResponse/Session）
- **外部库**：标准库 `context`/`encoding/json`/`errors`/`fmt`/`log/slog`/`net`/`net/url`/`os`/`path/filepath`/`strconv`/`strings`
- **被调用方**：main.go（注册 handler）

## 6. 并发与资源管理

- handler 由 tunnel client 在收到 RPC 时调度，可能并发调用——目前无显式锁，依赖文件系统原子操作（temp + rename）保证安全。
- `commitManagedSecretWrites` 内 ctx.Err() 检查确保 ctx 取消时及时回滚，避免半提交状态。
- temp 文件用 `os.CreateTemp` 保证名称唯一；backup 路径同样用 CreateTemp 占位后 Remove 再 Rename，避免覆盖已有文件。
- 文件权限：temp `0o600`、目录 `0o700`、原文件 backup 后由 rename 替换（权限继承自 temp）。

## 7. 设计模式与亮点

- **三阶段事务（plan-stage-commit）**：plan 期纯校验不触盘；stage 期创建 temp 文件不碰原文件；commit 期原子 rename。任一阶段失败不影响后续阶段，commit 失败完整回滚。
- **路径强校验**：`cleanManagedSecretPath` 用 `filepath.Rel` 确保 path 严格在 baseDir 之下，防止 `../` 逃逸；`os.Lstat` 拒绝符号链接，防止 RPC 被滥用为任意文件写入原语。
- **权限强约束**：temp 文件 `0o600`、目录 `0o700`、读取时（plugin.go `readSecretFile`）拒绝 group/other 可读，多层防御凭证泄露。
- **preserve_password 模式**：manager 推送部分字段更新时，edge 从现有 secret 提取 password 合并，避免 manager 持久化明文密码。
- **批量原子性**：同批次多个 secret 要么全部提交要么全部回滚，避免部分源更新部分失败的不一致状态。
- **db_type 适配**：mysql 用 ini 文件、其他用 URI，匹配各 exporter 的凭证接收习惯；TLS 字段按 db_type 差异化处理（postgres sslmode、redis rediss://、mongo tls/tlsCAFile）。

## 8. 注意事项

- `secretBaseDir = "/var/lib/ongrid-edge/secrets"` 硬编码，部署时需确保该目录存在且 owner 为 ongrid-edge；`stageManagedSecretWrite` 会 `MkdirAll(dir, 0o700)` 但顶层 baseDir 不会自动创建。
- `buildEdgeDatabaseMetricsSecret` 中 mongodb 禁止 `tls_key_file`，要求合并 cert+key PEM 走 `tls_cert_file`——这是 mongodb_exporter 的限制，操作员需在 manager UI 提示。
- `extractExistingDatabasePassword` 对非 mysql 类型解析 URI，若现有 secret 不是 URI 格式（如被手工篡改）会报 "invalid URI"——建议 manager UI 显示当前 secret 格式避免误操作。
- `commitManagedSecretWrites` 在 ctx 取消时回滚已提交项，但若 backup rename 失败，原文件已被移走，会留下 backup 文件——`rollbackManagedSecretWrites` 会尝试再次 rename 恢复，仍失败则 join 错误返回，运维需介入。
- handler 无并发锁，若 manager 同时下发多个 batch RPC 操作同一 path，可能产生竞态（temp 文件名不同但最终 rename 串行化，最后写入者胜）——manager 侧应串行化对同一 source 的凭证更新。
- `maxSecretContent = 16KiB` 对常规 DB 凭证足够，但若未来扩展到带 TLS 证书内嵌的 secret 可能不够，需调整。
- `RegisterSecretHandler` 的 handler 闭包捕获 `log`，若 log 为 nil 兜底为 default——避免 nil panic。
