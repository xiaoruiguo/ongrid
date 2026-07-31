# `custommetrics/spec.go` 技术实现文档

> 源文件：`internal/edgeagent/plugins/custommetrics/spec.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/plugins/custommetrics`

## 1. 概述

本文件是 custommetrics 插件的 spec 解析层：把 `PluginConfig.Spec["targets"]`（JSON 解码后的 `[]interface{}`）转成强类型的 `[]metricscommon.Target`，应用默认值、校验 URL/时长/重复 ID/重复 URL，并支持 `resource.category=database` 的资源分类（用于在 extra_labels 注入 `resource_category`/`db_type`）。所有类型转换都容忍 JSON 解码后的 `interface{}` 形态。

## 2. 包信息

- **包名**：`custommetrics`
- **所属模块**：`internal/edgeagent/plugins/custommetrics`
- **依赖方向**：被本包 `plugin.go` 的 `Configure`/`run` 调用；依赖 `metricscommon.Target` 与校验工具

## 3. 关键类型与接口

无包级导出类型。所有函数为包内私有，仅 `parseSpec` 被外部（plugin.go）调用。返回值复用 `metricscommon.Target`。

## 4. 关键函数与流程

### `parseSpec(spec) ([]metricscommon.Target, error)`
- **职责**：入口，解析 `spec["targets"]` 数组。
- **流程**：
  1. 取 `spec["targets"]`；缺失返回 `(nil, nil)`（无目标合法）
  2. 断言为 `[]interface{}`；否则报 "targets must be an array"
  3. 遍历每项，断言为 `map[string]interface{}` → `parseTarget(i, m)`
  4. 校验：`seen[t.ID]` 重复 ID 报错；`seenURLs[canonicalTargetURL(t.URL)]` 重复 URL 报错
  5. append 到 out
- **错误处理**：任一 target 解析失败立即返回错误。

### `parseTarget(i, m) (Target, error)`
- **职责**：解析单个 target。
- **流程**：
  1. `id` 必填；`target_url` 必填并通过 `metricscommon.ValidateURL`
  2. `scrape_interval`/`scrape_timeout` 走 `durationFrom`，默认 `DefaultInterval`/`DefaultTimeout`；timeout > interval 时强制 clamp 到 interval
  3. `enabled` 默认 true；`source_label` 默认 `"custom:"+id`
  4. `auth` 子 map 取 `bearer_token`/`username`/`password`
  5. `extra_labels` 取 `stringMap`
  6. `resource` 子 map 走 `targetResourceClassification`：若 category=database，向 extra_labels 注入 `resource_category`/`db_type`，并设 `Kind=dbType`
  7. `sample_limit` 默认 5000，<0 报错
  8. `label_drop` 取 stringSlice
- **错误处理**：所有校验失败用 `%w` 包装，前缀 `targets[%d].xxx`。

### `targetResourceClassification(m) (category, dbType string, err error)`
- **职责**：解析 `resource` 子对象，目前仅支持 `category=database`。
- **流程**：
  1. 缺失/nil 返回空
  2. 断言为 map；`category` 必填，经 `normalizeResourceCategory` 归一化
  3. category 必须为 "database"，其他报 "unsupported category"
  4. `type` 必填，经 `normalizeDatabaseType` 归一化（postgres/pg→postgresql，mongo→mongodb）
  5. `isSupportedDatabaseType` 校验（mysql/postgresql/redis/mongodb）

### 工具函数族
- `normalizeResourceCategory`/`normalizeDatabaseType`：trim+lower，pg/postgres→postgresql，mongo→mongodb
- `isSupportedDatabaseType`：白名单 mysql/postgresql/redis/mongodb
- `canonicalTargetURL`：解析 URL，scheme/host 转小写后重新 String，用于 URL 去重（避免 `http://X` 与 `http://x` 被当成两个 target）
- `stringFrom`/`boolFrom`/`intFrom`/`durationFrom`/`mapFrom`/`stringMap`/`stringSlice`：容忍 `interface{}` 的类型转换助手；`intFrom` 接受 float64/int/string，`durationFrom` 用 `time.ParseDuration` 并校验 >0
- `firstNonEmpty(vals ...string)`：返回首个非空

## 5. 依赖关系

- **内部包**：`internal/edgeagent/plugins/metricscommon`（Target/ValidateURL/DefaultInterval/DefaultTimeout）
- **外部库**：标准库 `fmt`/`net/url`/`strconv`/`strings`/`time`
- **被调用方**：本包 `plugin.go`

## 6. 并发与资源管理

无并发控制。所有函数纯函数式，无副作用，可被多 goroutine 安全调用（实际只在 Configure/run 中串行调用）。

## 7. 设计模式与亮点

- **JSON interface{} 容忍转换**：所有 spec 取值经 `stringFrom`/`boolFrom`/`intFrom` 等助手，兼容 JSON 解码后的 `float64`/`bool`/`string`/`[]interface{}` 形态，避免 type assertion panic。
- **URL 规范化去重**：`canonicalTargetURL` 把 scheme/host 小写后去重，避免操作员大小写差异导致同 URL 被抓两次。
- **timeout clamp**：`timeout > interval` 时强制等于 interval，避免抓取跨越下一周期导致重叠。
- **resource 分类扩展点**：通过 `resource.category` 字段为 custom target 标注数据库类型，注入 `db_type` label，让 manager 侧 PromQL 能按数据库类型聚合；currently 仅 database 一类，未来可扩展。
- **source_label 默认值**：`custom:<id>` 让多 target 在 push_prom_samples 的 source 字段天然可区分。

## 8. 注意事项

- `parseSpec` 返回 `(nil, nil)` 当 targets 缺失——这意味着"无 target"是合法配置，插件会启但所有 goroutine 都不启动，run 立即 wg.Wait 返回；manager UI 需提示操作员。
- `canonicalTargetURL` 在 URL 解析失败时回退到 trim 原值，可能让两个等价的非法 URL 被视为不同——但非法 URL 本会在 `ValidateURL` 阶段被拒。
- `intFrom` 对 string 用 `strconv.Atoi`，无法解析时返回默认值而非错误——对 sample_limit 这种数值字段可能掩盖操作员 typo（"5_000" 会被当成默认 5000）。
- `durationFrom` 错误信息前缀仅 "must be > 0" 或 ParseDuration 原始错误，调用方加 `targets[%d].scrape_interval: %w` 前缀，定位尚可。
- `stringMap` 会过滤值为非 string 的 key——若 operator 误把 `extra_labels: {flag: true}` 当 bool 传，会被静默丢弃，应考虑报错或在 manager UI 校验。
