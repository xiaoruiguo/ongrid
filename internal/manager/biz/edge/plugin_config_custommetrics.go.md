# `plugin_config_custommetrics.go` 技术实现文档

> 源文件：`internal/manager/biz/edge/plugin_config_custommetrics.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/edge`

## 1. 概述

本文件实现 custommetrics 插件 spec 的校验逻辑。`validateCustomMetricsSpec` 校验 `targets[]` 数组：每个 target 必有唯一 id + 合法 target_url（http/https + host）；URL 去重；可选 `resource` 子对象限定 category（仅 "database"）+ db_type（mysql/postgresql/redis/mongodb）。所有错误带索引 `targets[i]` 便于操作员定位。

## 2. 包信息

- **包名**：`edge`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：仅被 `plugin_config.go::Set` 调用；依赖 `pkg/errs`

## 3. 关键类型与接口

无导出类型；纯函数集。常量通过 `db_type` 字符串字面量定义（mysql/postgresql/redis/mongodb）。

## 4. 关键函数与流程

### `validateCustomMetricsSpec`
- **签名**：`func validateCustomMetricsSpec(spec map[string]interface{}) error`
- **职责**：校验 custommetrics spec 合法性
- **流程**：
  1. spec nil → nil（无 targets 视为合法）
  2. `spec["targets"]` 不存在 → nil
  3. targets 不是 array → ErrInvalid "targets must be an array"
  4. 遍历 targets：
     - 每个 target 必须是 object
     - `id` 必填且唯一（seenIDs map 去重）
     - `target_url` 必填且唯一（canonical URL 后 seenURLs 去重）
     - 调 `validateCustomMetricsTargetResource`
- **错误处理**：所有错误 `errs.ErrInvalid` + 索引 `targets[i]`；duplicate id/url 带冲突详情

### `validateCustomMetricsTargetResource`
- **签名**：`func validateCustomMetricsTargetResource(i int, target map[string]interface{}) error`
- **职责**：校验 target.resource 子对象
- **流程**：
  1. 检测 top-level `resource_type` / `db_type` → 报错指向 `resource.category/type`（防老 schema 混入）
  2. resource 不存在或 nil → nil（可选）
  3. resource 必须是 object
  4. `category` 必填；`normalizeCustomMetricsResourceCategory` 归一化（trim + lower）
  5. `customMetricsResourceCategorySupported` 校验（仅 "database"）
  6. category != "database" → nil（未来扩展点）
  7. category == "database" → `type` 必填；`normalizeCustomMetricsDBType` 归一化（pg→postgresql, mongo→mongodb）；`customMetricsDBTypeSupported` 校验（mysql/postgresql/redis/mongodb）

### `normalizeCustomMetricsResourceCategory`
- **签名**：`func normalizeCustomMetricsResourceCategory(v string) string`
- **职责**：trim + lower；空返回空；"database" 原样；其他 lower 返回

### `customMetricsResourceCategorySupported`
- **签名**：`func customMetricsResourceCategorySupported(v string) bool`
- **职责**：仅 "database" 支持

### `normalizeCustomMetricsDBType`
- **签名**：`func normalizeCustomMetricsDBType(v string) string`
- **职责**：归一化 db_type
- **流程**：trim + lower；"postgres"/"pg" → "postgresql"；"mongo" → "mongodb"；其他 lower 原样

### `customMetricsDBTypeSupported`
- **签名**：`func customMetricsDBTypeSupported(v string) bool`
- **职责**：支持 mysql/postgresql/redis/mongodb

### `canonicalCustomMetricsTargetURL`
- **签名**：`func canonicalCustomMetricsTargetURL(raw string) (string, error)`
- **职责**：归一化 URL 用于去重
- **流程**：
  1. `url.Parse(trim(raw))`
  2. scheme 必须 http/https
  3. host 必填
  4. scheme + host lower
  5. 返回 `u.String()`
- **错误处理**：parse 失败 / scheme 错 / host 缺 → error

## 5. 依赖关系

- **内部包**：`pkg/errs`
- **外部库**：`net/url`、`strings`
- **被调用方**：`plugin_config.go::Set`（custommetrics 分支）

## 6. 并发与资源管理

- **纯函数**：无共享状态；并发安全
- **无 IO**：纯内存校验

## 7. 设计模式与亮点

- **错误带索引**：所有错误含 `targets[i]` 便于操作员定位
- **id + url 双重去重**：seenIDs + seenURLs map；防同 id 或同 url 重复 target
- **URL canonical 化去重**：scheme/host lower 后比较；`http://X.com/a` 与 `http://x.com/a` 视为同 url
- **db_type 归一化**：pg/postgres → postgresql；mongo → mongodb；操作员简写兼容
- **top-level resource_type/db_type 拒绝**：强制用 `resource.category/type` 嵌套结构；防老 schema 混入
- **category 仅 "database"**：未来扩展点（如 "container"/"vm"）；当前仅 database 子校验
- **canonical URL 比较而非字面比较**：避免大小写差异导致漏检

## 8. 注意事项

- **category 仅 "database"**：其他值通过 `customMetricsResourceCategorySupported` 但不进 db_type 校验；未来扩展需更新此函数
- **db_type 4 种**：mysql/postgresql/redis/mongodb；新增需扩 `customMetricsDBTypeSupported` + `databaseMetricsDBTypeSupported`（databasemetrics 文件）
- **URL 必须 http/https**：其他 scheme（如 unix socket）拒绝
- **resource 可选**：target 可无 resource（纯 URL scrape）；有 resource 才校验 category/type
- **normalize 不报错**：归一化只 trim+lower；不合法的值由 supported 函数拒绝
- **duplicate 错误带冲突详情**：`duplicate target_url %q conflicts with target %q` 便于操作员定位冲突
