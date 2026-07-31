# `analyze_database_status.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/analyze_database_status.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools`

## 1. 概述

本文件实现两个 BaseTool：`analyze_database_status`（深度分析 MySQL/PostgreSQL/Redis/MongoDB 健康与性能）和 `list_database_sources`（列出配置的数据库指标源，不查 Prometheus）。`analyze_database_status` 作为数据库类问题的首选工具，先 discover metric names 再按 db_type 跑预定义 PromQL 检查；返回 capability matrix + findings + 健康状态聚合。关键红线：lookback 300..86400s 钳位；source 列表截断 32 条；metric name 列表上限 128/64。

## 2. 包信息

- **包名**：`tools`
- **所属模块**：`internal/manager/biz/aiops/tools`
- **依赖方向**：被 Registry 注册和 agent loop 调用；依赖 `basetool`、`devicebiz`、`edgebiz`、`edgemodel`、`promquery`

## 3. 关键类型与接口

```go
const (
    ToolNameAnalyzeDatabaseStatus = "analyze_database_status"
    ToolNameListDatabaseSources   = "list_database_sources"
    maxDatabaseSourceMetricNames     = 128
    maxDatabaseCapabilityMetricNames = 64
    maxDatabaseUnmappedMetricNames   = 64
)

type AnalyzeDatabaseStatusArgs struct {
    DeviceIDs            []uint64
    DBTypes              []string
    SourceIDs            []string
    LookbackSeconds      int
    IncludeCustomMetrics *bool
    IncludeDisabled      bool
}

type ListDatabaseSourcesArgs struct { /* subset */ }

type DatabaseStatusResponse struct {
    Status, GeneratedAt, LookbackSeconds, Count, Truncated
    ByDBType, ByPlugin map[string]int
    Sources []DatabaseStatusSource
    Errors  []string
}

type DatabaseStatusSource struct {
    DeviceID, EdgeID, SourceID, DBType, Plugin, HealthState, Status string
    SampleCount5m float64
    Capabilities  []DatabaseCapability
    MetricNames, UnmappedNames []string
    Metrics       map[string]float64
    Findings      []DatabaseStatusFinding
}

type DatabaseCapability struct {
    Name, Status string
    Metrics, MissingMetrics []string
    Message string
}

type DatabaseStatusFinding struct {
    Severity, Code, Title, Threshold, PromQL, Message string
    Value float64
}

type PluginConfigLister interface {
    ListForUI(ctx context.Context, edgeID uint64) ([]edgebiz.PluginRow, error)
}

type AnalyzeDatabaseStatusTool struct {
    promQuery     PromQuerier
    edges         *edgebiz.Usecase
    devices       *devicebiz.Usecase
    pluginConfigs PluginConfigLister
    log           *slog.Logger
}

type databaseStatusRunner struct { /* same deps */ }
type databaseSourceInventoryRunner struct { /* subset */ }
```

## 4. 关键函数与流程

### `NewAnalyzeDatabaseStatusTool / NewListDatabaseSourcesTool`
- 构造工具；log nil → slog.Default()

### `Info`
- `AnalyzeDatabaseStatusTool.Info` 返回 Class="read"；`ListDatabaseSourcesTool.Info` 同样

### `InvokableRun`
- 两者都把 args 转给 runner.run；runner 持有 deps；返回 JSON 字符串

### `databaseSourceInventoryRunner.run`
- **流程**：edges/plugins nil → error；unmarshal args；`context.WithTimeout(ctx, 15s)`；调 statusRunner.resolveCandidates + discoverSources；构 DatabaseSourcesResponse（按 device/dbtype/sourceid 排序）；截断 256 条；marshal 返回

### `databaseStatusRunner.run`
- **流程**：
  1. promQuery/edges/plugins nil → error
  2. unmarshal args；LookbackSeconds <=0 → 3600；<300 → 300；>86400 → 86400
  3. `context.WithTimeout(ctx, queryPromqlCallTimeout)`
  4. resolveCandidates + discoverSources；sources >32 截断 + Truncated=true
  5. 对每个 source 调 analyzeSource
  6. 聚合 status = aggregateDatabaseStatus(rows)
  7. marshal 返回

### `resolveCandidates`
- **流程**：deviceIDs 非空 → 逐个 resolveCandidate（device_id >0，去重）；空 → devices.List 500 → LookupEdgeForDevice；devices nil → edges.List 500

### `discoverSources`
- **流程**：对每个 candidate 调 pluginConfigs.ListForUI；按 PluginName 分发：databasemetrics → discoverDatabaseMetricsSources；custommetrics（includeCustom 时）→ discoverCustomMetricSources；按 DeviceID/DBType/SourceID 排序

### `analyzeSource`
- **核心分析函数**：
  1. 构 DatabaseStatusSource skeleton；attachHealth（edge PluginHealth）
  2. !Enabled → Finding source_disabled + status unknown + 返回
  3. selector = `device_id=.., ongrid_source=..`
  4. activeSeriesExpr 查 sum(count by __name__)；SampleCount5m <=0 → critical finding "no_recent_samples" + 返回
  5. discoverMetricNames → metricNames sorted + limit 128
  6. databaseCapabilities(dbType, discovered)
  7. unmappedDatabaseMetricNames limit 64
  8. analyzeDatabaseObjectInventory（按 db_type 跑 schema/table/key count 查询）
  9. 按 db_type 分发 analyzeMySQL / analyzePostgreSQL / analyzeRedis / analyzeMongoDB
  10. status = statusFromFindings；unknown + SampleCount5m>0 → "ok"

### `analyzeMySQL / PostgreSQL / Redis / MongoDB`
- 各自调 checkUp + 大量 checkThreshold（连接使用率、慢查询、死锁、cache hit、内存、复制延迟等预定义检查）
- Redis 额外有 checkRedisMemory / checkRedisMemoryFragmentation / checkRedisKeyspaceHitRatio
- MongoDB 用 checkFirstThreshold / checkFirstUp 兼容新旧 exporter metric 名

### `checkThreshold`
- **流程**：required metrics 缺 → skip；queryScalar 查 expr；err → prom_query_error finding；non-finite → error；命中 CritAbove/CritBelow/WarnAbove/WarnBelow 阈值 → 对应 severity finding

### `databaseCapabilities / evaluateDatabaseCapability`
- **流程**：按 dbType 取 capability specs（mysql 56 项、postgresql 40 项、redis 45 项、mongodb 50 项）；All/Any/Prefixes 匹配；status = available/partial/unavailable

### `discoverMetricNames`
- **流程**：`count by (__name__) ({selector})` → instantValues → 取 `__name__` 标签

### `queryScalar`
- **流程**：promQuery.Query → instantValues；NaN/Inf → error；返回首个值

### `instantValues / parsePromValue`
- 解析 Prom instant query JSON；支持 vector 和 scalar

### `aggregateDatabaseStatus / statusFromFindings / maxStatus / statusRank / normalizeStatus`
- status 排序：critical(4) > warning(3) > unknown(2) > ok(1)；info 视为 ok

## 5. 依赖关系

- **内部包**：`basetool`、`devicebiz`、`edgebiz`、`edgemodel`、`internal/pkg/promquery`
- **外部库**：标准库 `context`、`encoding/json`、`fmt`、`log/slog`、`math`、`sort`、`strconv`、`strings`、`time`
- **被调用方**：Registry.executeAnalyzeDatabaseStatus / executeListDatabaseSources；agent loop

## 6. 并发与资源管理

- 工具实例 immutable（除 log），多 goroutine 共享安全
- `context.WithTimeout` 限定 Prom 查询时长
- 无锁、无共享可变状态

## 7. 设计模式与亮点

- **首选工具定位**：数据库类问题优先此工具，避免直接 query_promql 走过窄路径
- **capability matrix**：每个 dbType 列出 exporter 应暴露的指标族，available/partial/unavailable 三态，让 LLM 知道哪些问题可答/不可答
- **object_inventory 检查**：直接从指标回答 "MySQL schema 数量 / Redis key 数量 / MongoDB collection 数量"，缺失时 Finding 提示
- **预定义 PromQL 检查**：每个 dbType 几十条 threshold check（连接压力、慢查询、死锁、cache hit、内存、复制延迟等），LLM 无需自写 PromQL
- **checkFirstThreshold / checkFirstUp**：MongoDB 新旧 exporter metric 名兼容（mongodb_ss_ vs mongodb_）
- **selector 隔离**：`device_id=.., ongrid_source=..` 双 label 隔离同 device 多 source
- **status 聚合**：从 findings 取最高 severity 作为 source status；从 sources 取最高作为整体 status
- **截断保护**：sources 32、metric names 128、capability metrics 64、unmapped 64，防 response token 爆炸

## 8. 注意事项

- **LookbackSeconds 钳位**：<=0 → 3600；<300 → 300；>86400 → 86400
- **source 截断 32**：narrow by device_ids/db_types/source_ids 缩小范围
- **list 工具截断 256**：list_database_sources 用更大上限 256
- **include_disabled 默认 false**：禁用 source 不参与分析
- **include_custommetrics 默认 true**：custommetrics 的 database category target 默认包含
- **db_type 归一化**：postgres/pg → postgresql；mongo → mongodb
- **prom_query_error finding**：Prom 查询失败不 fail 整个 tool，写入 finding 让 LLM 知道
- **NaN/Inf 检测**：queryScalar 显式拒绝 non-finite 值
- **dry-run list**：list_database_sources 不查 Prom，仅列配置；分析健康用 analyze_database_status
