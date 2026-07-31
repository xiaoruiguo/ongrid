# `databasemetrics/spec.go` 技术实现文档

> 源文件：`internal/edgeagent/plugins/databasemetrics/spec.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/plugins/databasemetrics`

## 1. 概述

本文件是 databasemetrics 插件的 spec 解析与 exporter 命令构造层。把 `PluginConfig.Spec["sources"]` 解析为强类型 `[]sourceSpec`，校验 ID/端口冲突/DB 类型/listen 地址；为每个 db_type 生成对应 exporter 二进制路径、CLI args、env（含 secret）；维护各 exporter 的 collector / flag 白名单字典（mysql/postgres/redis/mongodb），拒绝未白名单的字段防止操作员误传无效参数。还生成对应的 `metricscommon.Target` 用于抓取。

## 2. 包信息

- **包名**：`databasemetrics`
- **所属模块**：`internal/edgeagent/plugins/databasemetrics`
- **依赖方向**：被本包 `plugin.go` 的 Configure/run 调用；依赖 `metricscommon.Target`/`DefaultInterval`/`DefaultTimeout`

## 3. 关键类型与接口

```go
type connectionSpec struct{ Type, Path string } // 仅 type=managed

type tlsSpec struct {
    Enabled, SkipVerify bool
    CAFile, CertFile, KeyFile string
}

type exporterSpec struct {
    Collectors    []string
    CollectorsSet bool
    Bools         map[string]bool
    Strings       map[string]string
    Ints          map[string]int
    Lists         map[string][]string
}

type sourceSpec struct {
    ID, DBType, Name, ListenAddress string
    Enabled                         bool
    Connection                      connectionSpec
    TLS                             tlsSpec
    Exporter                        exporterSpec
    Interval, Timeout               time.Duration
    SourceLabel                     string
    ExtraLabels                     map[string]string
    SampleLimit                     int
    LabelDrop                       []string
}
```

## 4. 关键函数与流程

### `parseSpec(spec) ([]sourceSpec, error)`
- **职责**：解析 `spec["sources"]`。
- **流程**：取 sources 数组 → 遍历 `parseSource` → 校验 `seen[ID]` 重复 → `listenPort(source.ListenAddress)` 提取端口 → 校验 `reservedListenPorts`（9102 hostmetrics/9256 procmetrics 保留）与 `seenListenPorts`（同批次内端口冲突）。
- **错误处理**：任一失败返回带 `sources[%d].xxx` 前缀的错误。

### `parseSource(i, m) (sourceSpec, error)`
- **职责**：解析单 source。
- **流程**：
  1. `id` 必填；`db_type` 必填且通过 `isSupportedDBType`（mysql/postgresql/redis/mongodb）
  2. `listen_address` 缺省走 `defaultListenAddress(dbType)`，经 `validateListenAddress` 校验 host+port
  3. `connection.type` 缺省 "managed"，非 managed 报错；`connection.path` 必填（指向 secret 文件）
  4. `scrape_interval`/`scrape_timeout` 走 `durationFrom`，默认 `metricscommon.Default*`；timeout > interval 时 clamp
  5. `source_label` 默认 `"db:"+id`；`sample_limit` 默认 5000，<0 报错
  6. `label_drop` 缺省走 `defaultLabelDrop(dbType)`（mysql/pg 删 query/statement，mongo 删 collection/query）
  7. `exporter` 走 `exporterFrom`
  8. `extra_labels` 经 `withDBLabels` 注入 `db_type`/`service` 默认
  9. `tls` 走 `tlsFrom`
- **错误处理**：所有校验失败带 `sources[%d].xxx` 前缀。

### `(s sourceSpec) command(binDir, secretPath, secret) (binary, args, env, err)`
- **职责**：根据 db_type 生成 exporter 命令。
- **流程**：
  - mysql：`mysqld_exporter --web.listen-address=... --config.my-cnf=<secretPath>` + TLS skip verify flag + `mysqlExporterArgs`
  - postgresql：`postgres_exporter --web.listen-address=...` + `postgresExporterArgs`，env `DATA_SOURCE_NAME=<secret>`
  - redis：`redis_exporter --web.listen-address=... [-skip-tls-verification] [-tls-ca-cert-file=...] ...` + `redisExporterArgs`，env `REDIS_ADDR=<secret>`
  - mongodb：`mongodb_exporter --web.listen-address=...` + `mongodbExporterArgs`，env `MONGODB_URI=<secret>`
- secret 文件路径 vs secret 内容：mysql 用 `--config.my-cnf=path` 直接传文件；其他用 env 传 URI 字符串。

### exporter args 构造函数族
- `mysqlExporterArgs`/`postgresExporterArgs`/`redisExporterArgs`/`mongodbExporterArgs`：从 `exporterSpec.Collectors/Bools/Strings/Ints/Lists` 按 db_type 对应的 flag 字典生成 CLI args。
- 通用助手：
  - `appendBoolFlags`：true 时加 `--flag`
  - `appendNoableBoolFlags`：true 加 `--flag`，false 加 `--no-flag`
  - `appendStdBoolFlags`：true 加 `--flag`，false 加 `--flag=false`
  - `appendNoBoolFlags`：true 加 `--no-flag`
  - `appendStringFlags`/`appendIntFlags`：加 `--flag=value`
  - `appendListFlags`：加 `--flag=a,b,c`
  - `appendRepeatListFlags`：每项加 `--flag=item`
- `sortedKeys`：按 key 排序遍历 flag 字典，保证 args 顺序稳定（测试可重现）。

### `exporterFrom(i, dbType, m) (exporterSpec, error)`
- **职责**：解析 `m["exporter"]` 子对象。
- **流程**：
  1. 缺失返回空 spec
  2. `collectors` 走 `stringSliceFrom` + `databaseCollectorFlags(dbType)` 白名单校验
  3. mongodb 专属 `collect_all` bool
  4. 遍历 exporter map 其余 key，按 `allowedExporterFields(dbType)` 分发到 bools/strings/ints/lists 字段；非白名单 key 报错
  5. 若只有 collectors 且未显式设置，返回空 spec（用 exporter 默认）

### `(s sourceSpec) scrapeTarget() metricscommon.Target`
- **职责**：生成抓取 target，URL=`http://<ListenAddress>/metrics`。

### 字典定义
- `mysqlCollectorFlags`：~30 个 mysql collector 名（auto_increment.columns 等）
- `mysqlBoolExporterFlags`/`mysqlStringExporterFlags`/`mysqlIntExporterFlags`/`mysqlRepeatListExporterFlags`
- `postgresBoolExporterFlags`/`postgresStringExporterFlags`/`postgresIntExporterFlags`/`postgresListExporterFlags`
- `redisBoolExporterFlags`/`redisStringExporterFlags`/`redisIntExporterFlags`
- `mongoDBBoolExporterFlags`/`mongoDBNoBoolExporterFlags`/`mongoDBStringExporterFlags`/`mongoDBIntExporterFlags`/`mongoDBListExporterFlags`/`mongoDBCollectorFlags`
- `reservedListenPorts`：`{9102: hostmetrics, 9256: procmetrics}`

### 辅助函数
- `defaultListenAddress(dbType)`：mysql 19104/pg 19187/redis 19121/mongo 19216
- `defaultLabelDrop(dbType)`：mysql/pg `[query, statement]`，mongo `[collection, query]`
- `defaultMongoDBCollectors`：`[diagnosticdata, replicasetstatus, fcv]`
- `validateListenAddress`/`listenPort`：net.SplitHostPort + 端口范围校验
- `withDBLabels`：注入 `db_type`/`service` 默认 label
- `tlsFrom`：从 spec 解析 TLS 配置
- 类型转换助手 `stringFrom`/`boolFrom`/`intFrom`/`boolValue`/`stringValue`/`intValue`/`durationFrom`/`mapFrom`/`stringMap`/`stringSlice`/`stringSliceFrom`/`firstNonEmpty`：容忍 JSON interface{} 形态

## 5. 依赖关系

- **内部包**：`internal/edgeagent/plugins/metricscommon`（Target/DefaultInterval/DefaultTimeout）
- **外部库**：标准库 `fmt`/`net`/`path/filepath`/`sort`/`strconv`/`strings`/`time`
- **被调用方**：本包 `plugin.go`

## 6. 并发与资源管理

无并发控制。纯函数式解析，无副作用。

## 7. 设计模式与亮点

- **flag 白名单字典**：每个 db_type 的 collector/bool/string/int/list flag 都有显式 map，拒绝未白名单字段——既防止操作员误传无效参数，又使 spec 校验在 Configure 期而非运行时暴露错误。
- **端口冲突检测**：`reservedListenPorts` 防止 databasemetrics 占用 hostmetrics/procmetrics 端口；`seenListenPorts` 防止同批次内多 source 端口冲突。
- **label_drop 默认值**：mysql/pg 删 `query`/`statement`（高基数 SQL 文本），mongo 删 `collection`/`query`——控制 cardinality，避免 Prometheus 标签爆炸。
- **args 顺序稳定**：`sortedKeys` 遍历 flag 字典，使生成的 CLI args 顺序确定，便于测试断言与 diff。
- **collectors 显式 vs 默认**：mongodb 的 `collect_all` 优先于 collectors 列表；collectors 未显式设置时用 `defaultMongoDBCollectors`，避免空 collectors 导致 exporter 不采集。
- **secret 接收方式适配**：mysql 用文件路径（`--config.my-cnf`），其他用 env URI——匹配各 exporter 的设计。

## 8. 注意事项

- `connection.type` 当前仅支持 "managed"，未来若支持 "inline"（直接传 secret 字符串）需扩展；当前强制走 secret 文件是为凭证不进 PluginConfig.Spec。
- `defaultListenAddress` 的端口选择避开了 9100（hostmetrics 默认在 manager 容器占用）、9102（hostmetrics edge）、9256（procmetrics），但若操作员自定义 listen_address 与其他插件冲突，Configure 期 `reservedListenPorts` 仅检查 9102/9256，不检查 hostmetrics/procmetrics 实际配置的端口——可能漏检。
- `mongodbExporterArgs` 中 `collect_all=true` 时跳过 `mongodbCollectorArgs`，但仍会 append `--collect-all` 之后的 bool/string/int/list flags——若操作员同时设 collect_all 和 collectors，collectors 被忽略，行为可能不直观。
- `stringSliceFrom` 接受 `[]interface{}` 和 `[]string` 两种形态，但 `stringSlice`（调用 `stringSliceFrom`）在类型不匹配时返回 nil 而非报错——可能掩盖操作员传错类型（如传 `collectors: "foo"` 字符串而非数组）。
- flag 字典是包级 var，不可变（无显式保护但 Go 包级 var 默认不可写）——若未来支持运行时扩展需加锁。
- `mergeStringMaps` 在 `allowedExporterFields` 中用于 mongodb 合并 bool + no-bool 字典，但函数本身定义在本文件未被外部使用，可考虑提到 metricscommon 复用。
