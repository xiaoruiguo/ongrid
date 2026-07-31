# `migrate.go` 技术实现文档

> 源文件：`internal/manager/data/alert/store/migrate.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/alert/store`

## 1. 概述

本文件是 alert store 的 schema 入口，使用 GORM `AutoMigrate` 注册七张表，并在 AutoMigrate 之后执行一系列幂等的数据回填与 schema 重写：legacy kind 折叠为 `metric_raw`、`edge_id` 列重命名为 `device_id`、`rule_key` 重命名、`conditions_json` 形状简化、`metric_threshold` 编译为 `metric_raw`。所有 rewrite 均 idempotent，二次启动匹配空集。红线：生产 schema 演进应迁至版本化 SQL migration 文件。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/alert`
- **依赖方向**：被 `cmd/ongrid` 启动调用；依赖 `gorm.io/gorm`、`internal/manager/model/alert`。

## 3. 关键类型与接口

```go
// 无对外导出类型；内部使用匿名 struct 描述迁移行
type row struct {
    ID             uint64
    ConditionsJSON string `gorm:"column:conditions_json"`
    JoinMode       string `gorm:"column:join_mode"`  // 仅 rewriteMetricThresholdToMetricRaw 用
}
```

## 4. 关键函数与流程

### `Migrate`
- **签名**：`func Migrate(db *gorm.DB) error`
- **职责**：AutoMigrate 七张表 + 多轮幂等数据回填。
- **流程**：
  1. `AutoMigrate(Incident, Event, Silence, Rule, Channel, Delivery, InvestigationReport)`
  2. **PR-E backfill**：空 `kind` → `metric_raw`
  3. **rename**：`prom_query` → `metric_raw`
  4. **collapse**：`edge_absence` / `edge_offline` / `health_ingest` / `ingest_health` / `event_internal` → `metric_raw`，各自带预设 conditions_json（部分带 extras 如 severity=info + enabled=false）
  5. **scope split**：原 `monitoring_pipeline` 拆为 `monitoring_pipeline`（平台自观测）与 `global`（跨主机聚合）；按 kind 翻 scope_type
  6. **scope sweep**：legacy rule_key（`edge_offline` / `device_offline`）的 scope 翻为 global
  7. `renameLegacyEdgeIDColumns(db)`：edge_id → device_id
  8. **rule_key rename**：`UPDATE alert_rules SET rule_key='device_offline' WHERE rule_key='edge_offline'`；同 `alert_incidents.rule`
  9. `rewriteMetricRawToExprOnly(db)`：legacy 4-field → 1-field expr 形状
  10. `rewriteMetricThresholdToMetricRaw(db)`：metric_threshold → metric_raw
- **错误处理**：每步错误立即返回。

### `renameLegacyEdgeIDColumns`
- **签名**：`func renameLegacyEdgeIDColumns(db *gorm.DB) error`
- **职责**：把 `alert_incidents.edge_id` 与 `alert_silences.edge_id` 重命名为 `device_id`（May 2026 entity split）。`chat_tool_calls` best-effort 同步。
- **流程**（每张表）：
  1. `HasOld && !HasNew` → `RenameColumn`；失败回退 `ALTER ADD + UPDATE + DROP`
  2. `HasOld && HasNew` → `UPDATE ... SET device_id = edge_id WHERE device_id IS NULL` + `DropColumn(edge_id)`
  3. 其余 no-op
- **方言兼容**：SQLite 需 3.25+（实际 3.40+），MySQL 8.0+。

### `rewriteMetricRawToExprOnly`
- **签名**：`func rewriteMetricRawToExprOnly(db *gorm.DB) error`
- **职责**：把 legacy `{expr, operator, threshold, for_seconds}` 形状重写为 `{expr: "<expr> <op> <thr>"}` 单字段形状。PromQL 自身比较运算符成为唯一谓词来源。
- **流程**：
  1. SELECT 所有 `kind = metric_raw` 的 id + conditions_json
  2. unmarshal；非 JSON 行跳过（runtime compile 会报错给 operator）
  3. 无 `operator` key 视为新形状，跳过
  4. 拼接 `expr op threshold`，marshal 回 `{expr: ...}`
  5. UPDATE 单行
  6. 打印 rewrite 数量
- **幂等性**：二次启动无 `operator` key，全部跳过。

### `metricExprFor`
- **签名**：`func metricExprFor(metric string) (string, bool)`
- **职责**：闭集 host metric → PromQL 查询映射。镜像 biz 层 `evaluators_phaseA.metricExprFor`，避免 data → biz 循环依赖。**两表必须同步**，biz 层为 source of truth。
- **覆盖**：cpu_pct / mem_pct / disk_used_pct / disk_avail_bytes / load1 / load5 / load15 / net_rx_bps / net_tx_bps。

### `rewriteMetricThresholdToMetricRaw`
- **签名**：`func rewriteMetricThresholdToMetricRaw(db *gorm.DB) error`
- **职责**：把 `metric_threshold` 行编译为单 PromQL 表达式后改写为 `metric_raw`。镜像 biz 层 `compileMetricThresholdExpr`。
- **流程**：
  1. SELECT `kind = metric_threshold` 的 id + conditions_json + join_mode
  2. unmarshal 为 `[]struct{Metric, Operator, Threshold}`
  3. 解码失败 / 空条件 → 跳过 + 打印
  4. 每个 condition 通过 `metricExprFor` 取 PromQL，拼 `(base) op threshold`
  5. metric 不在闭集 → 跳过整行 + 打印
  6. 单条件 → 裸比较；多条件按 `join_mode`：`any` → `or`，默认 → `and on(device_id)`
  7. marshal `{expr: ...}` + UPDATE kind 与 conditions_json
- **幂等性**：WHERE `kind = metric_threshold`，二次启动空集。

### `formatThreshold` / `numberToString`
- **签名**：`func formatThreshold(v float64) string` + `func numberToString(v any) string`
- **职责**：渲染 JSON 解码的 threshold 为最短忠实字符串（90 而非 90.000000）。镜像 biz 层 `%g`。

## 5. 依赖关系

- **内部包**：`internal/manager/model/alert`（Rule 常量）
- **外部库**：`gorm.io/gorm`、`encoding/json`、`fmt`、`strings`
- **被调用方**：`cmd/ongrid` 启动序列
- **镜像同步**：`metricExprFor` 与 `internal/manager/biz/alert/evaluators_phaseA.metricExprFor` 必须同步；`rewriteMetricThresholdToMetricRaw` 与 `compileMetricThresholdExpr` 必须同步。

## 6. 并发与资源管理

- **无锁**：启动期串行执行，与请求路径隔离。
- **幂等保证**：所有 rewrite 都通过 WHERE 条件确保二次启动匹配空集。

## 7. 设计模式与亮点

- **AutoMigrate 后多轮 backfill**：AutoMigrate 加列后，立即跑数据回填使新 schema 可用，避免 v0.2.0 → v0.3.0 升级时 rule 行失效。
- **idempotent rewrites**：每轮 rewrite 都设计为二次启动匹配空集，安全反复执行。
- **闭集映射镜像 biz**：`metricExprFor` 在 data 层重复 biz 层逻辑，避免循环依赖，注释明示"两表必须同步"。
- **legacy kind 折叠**：edge_absence / health_ingest / event_internal 等特殊 kind 全部折叠为 metric_raw + PromQL，简化 evaluator 矩阵。
- **scope split**：把 monitoring_pipeline 拆为平台自观测 + global 跨主机聚合，让 channel router 过滤器可预测。
- **chat_tool_calls best-effort 同步**：edge_id → device_id 在 aiops 审计表也同步迁移，best-effort 容忍失败。

## 8. 注意事项

- **预生产阶段**：注释明示生产应迁至版本化 SQL migration 文件。
- **AutoMigrate 不删列**：废弃列需显式 migration 文件清理。
- **`metricExprFor` 双表同步**：扩展 host metric 闭集必须同步更新 data 与 biz 两处。
- **`event_internal` backfill 不保留原义**：pre-launch 直接改为 `vector(0) > 0` + severity=info + enabled=false，原 window/event_type 丢失。
- **`edge_absence` 阈值 90s 硬编码**：用户自定义阈值不保留（pre-launch 决策）。
- **`fmt.Printf` 日志**：注释提到应通过 gorm Logger / slog 输出，但当前作用域无 slog handle，暂用 Printf。
- **方言兼容**：SQLite 3.25+ / MySQL 8.0+ 才支持 RENAME COLUMN；老版本回退 ALTER ADD + UPDATE + DROP。
- **`chat_tool_calls` 迁移 best-effort**：失败不影响 alert 主流程，但可能导致 aiops 审计列不一致。
- **rule_key rename 双表**：`alert_rules.rule_key` 与 `alert_incidents.rule` 都要更新，否则历史 incident join 不到 rule。
