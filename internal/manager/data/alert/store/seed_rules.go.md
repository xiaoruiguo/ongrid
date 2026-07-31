# `seed_rules.go` 技术实现文档

> 源文件：`internal/manager/data/alert/store/seed_rules.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/alert/store`

## 1. 概述

本文件实现 `SeedBuiltinRules`，每次启动把 canonical built-in alert rule 集合 seed 到 DB。所有 built-in 都是 `metric_raw` rule（friendly `metric_threshold` 形式仅 UI 入口，save 时编译为 metric_raw）。设计要点：`UpsertBuiltinRule` no-op 已存在行，admin UI 编辑被保留；threshold ≤ 0 的 rule 跳过（env 禁用）；rule_key `edge_offline` → `device_offline` 重命名（May 2026 entity split）。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/alert`
- **依赖方向**：被 `cmd/ongrid` 启动调用；依赖 `internal/manager/model/alert`、`internal/pkg/config`、`internal/pkg/notify`。

## 3. 关键类型与接口

```go
// 内部辅助类型
type metricSeed struct {
    Key, Name, Severity string
    ExprFmt             string  // 含比较运算符，Threshold 用 %g 插值
    Threshold           float64 // ≤ 0 表示跳过此 seed
}
```

## 4. 关键函数与流程

### `SeedBuiltinRules`
- **签名**：`func SeedBuiltinRules(ctx, repo *Repo, cfg config.AlertConfig) error`
- **职责**：seed 全部 built-in rule，幂等。
- **流程**：依次调用 8 个子 seeder，任一失败立即返回：
  1. `seedHostMetricRules`（cpu/mem/disk/load1）
  2. `seedEdgeOfflineRule`（device_offline）
  3. `seedScrapeDownRule`
  4. `seedPromIngestFailRule`
  5. `seedDiskFullWarningRule`
  6. `seedCPUHighDefaultRule`（默认 disabled）
  7. `seedSwapHighRule`（默认 disabled）
  8. `seedFDExhaustionRule`（默认 disabled）

### `SeedHostRulesFromConfig`
- **签名**：`func SeedHostRulesFromConfig(ctx, repo *Repo, cfg config.AlertConfig) error`
- **职责**：legacy entry-point，委托给 `SeedBuiltinRules`，保留 v0.2.0 cmd/ongrid 兼容性。

### `seedHostMetricRules`
- **职责**：seed 4 个 host metric rule（cpu_high / mem_high / disk_high / load1_high）。
- **流程**：遍历 candidates，`Threshold <= 0` 跳过；`fmt.Sprintf(ExprFmt, Threshold)` 拼 PromQL；marshal `{expr: ...}`；`UpsertBuiltinRule`。
- **PromQL 表达式**：与 `metricExprFor`（migrate.go）+ biz 层 `compileMetricThresholdExpr` 一致。

### `seedEdgeOfflineRule`
- **职责**：seed `device_offline` rule（May 2026 前为 `edge_offline`）。
- **流程**：`cfg.EdgeOfflineThreshold.Seconds() <= 0` → 默认 90s；PromQL `device_last_seen_seconds_ago > <thr>`。
- **关键**：rule_key 改名，migrate.go 中 UPDATE 把历史行重命名同步。

### `seedScrapeDownRule`
- PromQL `up == 0`，scope = monitoring_pipeline，severity = warning。

### `seedPromIngestFailRule`
- **职责**：seed prom 写入失败 rule，替代删除的 `health_ingest` kind。
- **流程**：`cfg.PromIngestFailLimit <= 0` → 默认 5；PromQL `increase(prom_write_total{result="fail"}[5m]) >= <limit>`。

### `seedDiskFullWarningRule`
- PromQL `disk_used_pct > 85`，scope = host，severity = warning。

### `seedCPUHighDefaultRule`
- PromQL `cpu > 90`，**默认 disabled**（避免与 cpu_high 双触发）。

### `seedSwapHighRule`
- PromQL `swap_used_pct > 0.5`，**默认 disabled**。

### `seedFDExhaustionRule`
- PromQL `fd_allocated / fd_maximum > 0.85`，**默认 disabled**，severity = critical。

## 5. 依赖关系

- **内部包**：`internal/manager/model/alert`、`internal/pkg/config`、`internal/pkg/notify`（Severity 常量）
- **外部库**：`encoding/json`、`fmt`
- **被调用方**：`cmd/ongrid` 启动序列
- **依赖方法**：`repo.UpsertBuiltinRule`

## 6. 并发与资源管理

- **无锁**：启动期串行执行。
- **幂等**：`UpsertBuiltinRule` no-op 已存在行，admin 编辑被保留。

## 7. 设计模式与亮点

- **全部 metric_raw**：Post-Phase-3-final 后 built-in 直接 seed metric_raw，friendly form 仅 UI 入口。
- **threshold ≤ 0 跳过**：env 禁用 rule 的统一机制，absent 行让 evaluator 无事可做。
- **默认 disabled rule**：cpu_high_default / swap_high / fd_exhaustion 默认 disabled，避免双触发或噪音，operator 按需启用。
- **rule_key 重命名同步**：`edge_offline` → `device_offline`，migrate.go 中 UPDATE 把历史行 + incident 同步重命名。
- **PromQL 与闭集映射一致**：seed 的 PromQL 与 `metricExprFor`、biz `compileMetricThresholdExpr` 保持同形状。
- **legacy entry-point 保留**：`SeedHostRulesFromConfig` 委托新 seeder，保持 v0.2.0 调用兼容。

## 8. 注意事项

- **admin 编辑被保留**：`UpsertBuiltinRule` no-op 已存在行；operator 修改 threshold 后 boot 不覆盖。
- **rule_key 不可随意改**：`edge_offline` → `device_offline` 是 May 2026 entity split 的一部分，migrate.go 必须同步 UPDATE 历史行。
- **threshold ≤ 0 跳过**：env 配置 threshold = 0 表示禁用；rule 行不存在，evaluator 无事可做。
- **默认 disabled rule**：扩展 built-in 时需决定默认 enabled/disabled，避免与现有 rule 双触发。
- **PromQL 必须与闭集映射一致**：seed PromQL 改动需同步 `metricExprFor`（migrate.go）与 biz `compileMetricThresholdExpr`，否则 UI 编译形状与 seed 形状不一致。
- **severity 用 notify 包常量**：`notify.SeverityWarning` / `notify.SeverityCritical`，避免硬编码字符串。
