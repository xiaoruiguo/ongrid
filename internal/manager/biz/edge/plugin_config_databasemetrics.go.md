# `plugin_config_databasemetrics.go` 技术实现文档

> 源文件：`internal/manager/biz/edge/plugin_config_databasemetrics.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/edge`

## 1. 概述

本文件实现 databasemetrics 插件的 spec 准备 + edge-side secret 文件生成。核心职责：把 UI 提交的 `sources[].credentials` 拆出密码，生成各 DB 类型专用的连接串/配置文件（MySQL `.my.cnf` / Postgres DSN / Redis URI / Mongo URI），通过 frontierbound 写到 edge 的 `/var/lib/ongrid-edge/secrets/` 目录；spec 里只留非敏感字段 + `connection.path` 指针。含端口冲突检测、TLS 归一化、collector 白名单、previous/next diff 删除旧 secret。

## 2. 包信息

- **包名**：`edge`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 `plugin_config.go::Set`（databasemetrics 分支）调用；依赖 `pkg/errs`、`pkg/tunnel`

## 3. 关键类型与接口

```go
const databaseMetricsSecretDir = "/var/lib/ongrid-edge/secrets"

var databaseMetricsSourceIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

type dbCredentials struct {
    Host, Port, Username, Password, Database, SSLMode, AuthSource string
    TLS dbTLSConfig
}

type dbTLSConfig struct {
    Enabled, SkipVerify bool
    CAFile, CertFile, KeyFile string
}

type databaseMetricsExporterFieldMaps struct {
    bools, strings, ints, lists map[string]string
}
```

Sentinel：`databaseMetricsSecretDir = "/var/lib/ongrid-edge/secrets"`；reserved ports `9102`（hostmetrics）/`9256`（procmetrics）；default listen ports per dbType（mysql:19104, postgresql:19187, redis:19121, mongodb:19216）。

## 4. 关键函数与流程

### `prepareDatabaseMetricsSpec`
- **签名**：`func (uc *PluginConfigUC) prepareDatabaseMetricsSpec(spec) (map[string]interface{}, []tunnel.WriteDatabaseMetricsSecretRequest, error)`
- **职责**：拆分 secret 请求；归一化 sources；端口冲突检测
- **流程**：
  1. spec nil → 返回空 map, nil, nil
  2. sources 不存在 → 原样返回
  3. sources 不是 array → ErrInvalid
  4. 遍历 sources：
     - `sanitizeDatabaseMetricsSource(i, source)` → (nextSource, secretReq, err)
     - secretReq 非 nil → 加入 secretReqs
     - id 唯一性（seenIDs）
     - listen_address 缺省 `defaultDatabaseMetricsListenAddress(dbType)`
     - `databaseMetricsListenPort` 解析端口
     - 端口不能撞 reserved（9102/9256）
     - 端口不能撞其他 source（seenListenPorts）
  5. secretReqs 非空但 secretWriter nil → error
  6. 返回 out spec（sources 替换为 nextSources）+ secretReqs
- **错误处理**：所有错误 ErrInvalid + 索引 `sources[i]`；端口冲突带 owner（hostmetrics/procmetrics/source_id）

### `sanitizeDatabaseMetricsSource`
- **签名**：`func sanitizeDatabaseMetricsSource(i int, source) (map[string]interface{}, *tunnel.WriteDatabaseMetricsSecretRequest, error)`
- **职责**：归一化单个 source；拆 credentials；生成 secret 请求
- **流程**：
  1. id 必填且匹配 `databaseMetricsSourceIDRE`
  2. db_type 归一化 + supported 校验
  3. 算 secretPath = `databaseMetricsSecretPath(id, dbType)`
  4. 复制 source 但跳过 `credentials`
  5. 写 `connection = {type: "managed", path: secretPath, secret_set: true}`
  6. `validateDatabaseMetricsSourceTLS`（top-level tls）
  7. `sanitizeDatabaseMetricsExporter`（exporter 子对象）
  8. credentials 处理：
     - 无 credentials 但 connection.secret_set=true → 返回 out, nil, nil（保留旧 secret）
     - 无 credentials 且无 connection.secret_set → ErrInvalid "credentials required"
     - 有 credentials → `sanitizeDatabaseMetricsCredentials` + `buildDatabaseMetricsTLSConfig`
     - 若 connection.secret_set=true 且 credentials 无 password → secretReq 带 `PreservePassword: true`（不覆盖旧密码）
     - 否则 `buildDatabaseMetricsSecret(dbType, credentials)` 生成 content → secretReq
- **错误处理**：所有错误 ErrInvalid + 索引

### `sanitizeDatabaseMetricsCredentials`
- **签名**：`func sanitizeDatabaseMetricsCredentials(dbType string, credentials) (map[string]interface{}, error)`
- **职责**：白名单字段提取；删 password；TLS 归一化；校验
- **流程**：
  1. 白名单字段提取（host/port/username/database/sslmode/auth_source/tls_*/ssl*）
  2. 字符串字段 trim + 拒绝换行
  3. 删 password
  4. `normalizeDatabaseMetricsCredentialsForTLS`（SkipVerify 时清 TLS 文件字段；postgresql 设 sslmode=require）
  5. 构造 dbCredentials + validate
  6. `validateDatabaseMetricsTLSForDBType`
  7. redis database 必须是数字索引
- **错误处理**：字段含换行 / host 空 / port 非法 / TLS 非法 → error

### `buildDatabaseMetricsSecret`
- **签名**：`func buildDatabaseMetricsSecret(dbType string, credentials) (string, error)`
- **职责**：生成各 DB 类型的连接串/配置文件内容
- **流程**：
  1. 构造 dbCredentials + `normalizeDatabaseMetricsDBCredentials`（SkipVerify 归一化）
  2. validate + TLS 校验
  3. dbType 分支：
     - mysql → `buildMySQLSecret`（.my.cnf 格式；端口缺省 3306）
     - postgresql → `buildPostgresDSN`（DSN；端口 5432；db postgres；sslmode 按 TLS 推断）
     - redis → `buildRedisURI`（rediss:// 当 TLS；端口 6379；db 0；db 必须数字）
     - mongodb → `buildMongoURI`（mongodb://；端口 27017；db admin；authSource=db）
- **错误处理**：unsupported db_type → error

### `buildMySQLSecret / buildPostgresDSN / buildRedisURI / buildMongoURI`
- **签名**：各 DB 类型专用 secret 生成器
- **职责**：生成连接配置
- **流程**：
  - MySQL：`[client]` + user/password/host/port/database/tls/ssl-ca/ssl-cert/ssl-key 行
  - Postgres：`postgresql://user:pass@host:port/db?sslmode=...&sslrootcert=...&sslcert=...&sslkey=...`
  - Redis：`redis://` 或 `rediss://`（TLS）`user:pass@host:port/db`
  - Mongo：`mongodb://user:pass@host:port/db?authSource=...&tls=true&tlsInsecure=true&tlsCAFile=...&tlsCertificateKeyFile=...`

### `sanitizeDatabaseMetricsExporter`
- **签名**：`func sanitizeDatabaseMetricsExporter(i int, dbType string, raw interface{}) (map[string]interface{}, error)`
- **职责**：校验 exporter 子对象的字段
- **流程**：
  1. nil → nil, nil
  2. 必须 object
  3. collectors 数组校验（`databaseMetricsExporterCollectors(dbType)` 白名单）
  4. mongodb 的 `collect_all` 特殊 bool 字段
  5. 其他字段按 `databaseMetricsExporterFields(dbType)` 返回的 bools/strings/ints/lists map 分派校验
  6. 字符串字段 `validateDatabaseMetricsExporterString`（config_file/extend_query_path/script 必须绝对路径）
  7. 列表字段项不能含换行
- **错误处理**：未知字段 / 类型不符 / 不支持 → ErrInvalid + 索引

### `databaseMetricsSecretDeleteRequests`
- **签名**：`func databaseMetricsSecretDeleteRequests(previousSpec, nextSpec) []tunnel.WriteDatabaseMetricsSecretRequest`
- **职责**：diff previous vs next，生成删除请求
- **流程**：
  1. `databaseMetricsManagedSecretPaths(previousSpec)` 取旧 paths
  2. `databaseMetricsManagedSecretPaths(nextSpec)` 取新 paths
  3. 旧 path 不在新 paths → 删除请求（`Delete: true`）
  4. 排序 paths 确定性

### `databaseMetricsManagedSecretPaths`
- **签名**：`func databaseMetricsManagedSecretPaths(spec) map[string]string`
- **职责**：提取 spec 中所有 managed secret 路径
- **流程**：
  1. sources 不是 array → 空 map
  2. 遍历 sources：connection.type != "managed" 跳过（用户自管）
  3. connection.path 优先；否则 `databaseMetricsSecretPath(id, dbType)`
  4. 返回 path → id map

## 5. 依赖关系

- **内部包**：`pkg/errs`、`pkg/tunnel`（WriteDatabaseMetricsSecretRequest）
- **外部库**：`net/url`、`net`、`path/filepath`、`regexp`、`sort`、`strconv`、`strings`、`time`
- **被调用方**：`plugin_config.go::Set`（databasemetrics 分支）

## 6. 并发与资源管理

- **纯函数**：无共享状态；并发安全
- **无 IO**：spec 准备纯内存；secret 写入由 `writeDatabaseMetricsSecrets` 调 frontierbound
- **15s 写超时**：`writeDatabaseMetricsSecrets` 用 `context.WithTimeout(ctx, 15*time.Second)`

## 7. 设计模式与亮点

- **密码不入 spec**：credentials.password 拆出后生成 secret 文件；spec 只留 `connection.path` 指针；DB 中不存密码
- **per-DB 类型 secret 格式**：MySQL .my.cnf / Postgres DSN / Redis URI / Mongo URI；各 exporter 原生格式
- **managed vs 自管 connection**：connection.type != "managed" 跳过 secret 写入（用户自管 path）
- **PreservePassword 模式**：connection.secret_set=true 且 credentials 无 password → 不覆盖旧密码；允许操作员改其他字段不动密码
- **端口冲突检测**：reserved ports（9102/9256）+ source 间冲突；防 exporter 端口撞 hostmetrics/procmetrics
- **TLS 归一化**：SkipVerify 时清 CA/Cert/Key 字段；postgresql 自动设 sslmode=require
- **collector 白名单**：per-dbType collector set；防操作员写不存在的 collector
- **exporter 字段白名单**：per-dbType bools/strings/ints/lists map；未知字段拒绝
- **previous/next diff**：删除不再使用的 secret 文件；防 edge 残留旧 secret
- **id 正则 `^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`**：防 path traversal；最多 64 字符
- **字段拒绝换行**：所有字符串字段 `strings.ContainsAny(v, "\r\n")` → error；防配置文件注入

## 8. 注意事项

- **secret 写入 15s 超时**：frontierbound tunnel RPC；超时返回 error 触发 plugin_config.go 回滚 row
- **id 正则防 path traversal**：但 `.` 允许；操作员应避免 `../` 模式（正则已防）
- **db_type 4 种**：mysql/postgresql/redis/mongodb；新增需扩 `databaseMetricsDBTypeSupported` + `buildDatabaseMetricsSecret` 分支 + exporter 字段 map
- **reserved ports 9102/9256**：hostmetrics/procmetrics 占用；databasemetrics exporter 不能撞
- **default listen ports**：mysql:19104, postgresql:19187, redis:19121, mongodb:19216；操作员不指定时用默认
- **TLS 字段必须是绝对路径**：`filepath.IsAbs` 校验；防相对路径歧义
- **mongodb 不支持 tls_key_file**：用 tls_cert_file 含 combined cert+key PEM
- **postgresql sslmode 自动推断**：TLS 任意字段非空 → sslmode=require；否则 disable
- **redis database 必须数字**：是 DB index 不是名字
- **mongo authSource 缺省 = database**：通常是 admin
- **secret 文件扩展名**：mysql `.my.cnf`；其他 `.dsn`
- **exporter 字段 map 4 种**：bools/strings/ints/lists；mongodb 有特殊 `collect_all` bool + `disable_direct_connect`/`disable_exporter_metrics` noBool 字段
- **collector set per-dbType**：mysql 和 mongodb 有 collector 概念；postgresql/redis 无
