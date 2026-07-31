# `regex.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/alertdraft/regex.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/alertdraft`

## 1. 概述

本文件集中定义 alertdraft 包所有**正则表达式常量**。正则按职责分两类：(1) PromQL 解析（metric 名/尾部比较/range selector/token/文件系统 metric selector/metric source 身份标签 matcher）；(2) LogQL 解析（行过滤器/label 前缀过滤器/交替 label 前缀过滤器）；(3) 简单 host metric 谓词解析（join 连接符 + 单谓词 part）。集中定义便于维护和复用，避免分散在多个文件中重复编译。

## 2. 包信息

- **包名**：`alertdraft`
- **所属模块**：`internal/manager/biz/aiops/alertdraft`
- **依赖方向**：被 `promql.go`、`metric_raw.go`、`spec_normalize.go`、`request_hints.go`、`defaults.go` 引用

## 3. 关键类型与接口

无类型定义。所有常量为 `*regexp.Regexp` 类型。

## 4. 关键正则与用途

### PromQL 解析类

```go
// 简单 host metric 谓词的 and/or 连接符（如 "cpu > 80 and mem > 90"）
var simpleHostMetricPredicateJoinRE = regexp.MustCompile(`(?i)\s+(and|or)\s+`)

// 单个简单谓词 part：(metric op threshold)，可选括号
var simpleHostMetricPredicatePartRE = regexp.MustCompile(
    `^\(?\s*([a-zA-Z_][a-zA-Z0-9_\-\s]*)\s*(==|!=|>=|<=|>|<)\s*([+-]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][+-]?\d+)?)\s*\)?$`,
)

// PromQL metric 名合法性（[a-zA-Z_:][a-zA-Z0-9_:]*）
var promMetricNameRE = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)

// node_filesystem metric selector 提取（用于 metric_forecast disk available percent 重写）
var nodeFilesystemMetricSelectorRE = regexp.MustCompile(`node_filesystem_(?:avail|free|size)_bytes\s*(\{[^}]*\})?`)

// PromQL 尾部比较谓词（>=/<=/==/!=/>/< + 数字）
var promTrailingComparisonRE = regexp.MustCompile(
    `(?s)\s*(>=|<=|==|!=|>|<)\s*([+-]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][+-]?\d+)?)\s*$`,
)

// PromQL 简单 range selector（[5m]/[1h]/[2d] 等）
var promSimpleRangeSelectorRE = regexp.MustCompile(`\[(\d+(?:ms|s|m|h|d|w|y))\]`)

// PromQL token 提取（metric 名/函数名/关键字）
var promTokenRE = regexp.MustCompile(`[a-zA-Z_:][a-zA-Z0-9_:]*`)

// metric source 身份标签 matcher（ongrid_source/device_id/job/instance/service = "value"）
var promMetricSourceIdentityMatcherRE = regexp.MustCompile(
    `(?i)\b(ongrid_source|device_id|job|instance|service)\s*(=|!=|=~|!~)\s*"([^"]+)"`,
)
```

### LogQL 解析类

```go
// 行过滤器表达式（|=~/|=/|~/!=/!~ + 内容）
var logLineFilterExprRE = regexp.MustCompile(`^\s*(\|=~\||\|=|\|~|!=|!~)\s*(.+?)\s*$`)

// label 前缀过滤器（detected_level/device_id/filename/identifier/level/ongrid_source/priority/service_name/severity/unit）
// 形如 "level=error ..." 或 "device_id:host-123 ..."
var logLabelPrefixFilterRE = regexp.MustCompile(
    `(?i)^\s*(?:\(\?i\))?\s*(detected_level|device_id|filename|identifier|level|ongrid_source|priority|service_name|severity|unit)\s*(=|!=|=~|!~)\s*"?([A-Za-z0-9_:/-]+(?:\.[A-Za-z0-9_:/-]+)*)"?\s*(?:\.\*)?(.*)$`,
)

// 交替 label 前缀过滤器（(level|severity)=error ...）
var logLabelAlternationPrefixFilterRE = regexp.MustCompile(
    `(?i)^\s*(?:\(\?i\))?\s*\(([^)]+)\)\s*\[=:\]\s*"?([A-Za-z0-9_:/-]+(?:\.[A-Za-z0-9_:/-]+)*)"?\s*(?:\.\*)?(.*)$`,
)
```

## 5. 依赖关系

- **外部库**：`regexp`（标准库）
- **无内部包依赖**：纯常量定义

## 6. 并发与资源管理

- **包级 var**：所有正则在包初始化时编译一次，`*regexp.Regexp` 是并发安全的（`regexp.MustCompile` 返回的对象可被多个 goroutine 并发使用）
- **无锁**：正则对象本身线程安全

## 7. 设计模式与亮点

- **集中定义**：所有正则放一个文件，便于审查和维护
- **命名约定**：`...RE` 后缀统一标识正则常量
- **大小写不敏感**：`(?i)` 前缀用于 label/key 匹配场景（log filter/identity matcher）
- **多模式分组**：`(?:...)` 非捕获分组，避免不必要的捕获
- **数值格式完备**：`[+-]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][+-]?\d+)?` 覆盖整数/小数/科学计数法/前导小数点
- **range selector 单位全覆盖**：`ms|s|m|h|d|w|y` 覆盖 PromQL 所有时间单位
- **身份标签白名单**：`ongrid_source|device_id|job|instance|service` 明确列出 ongrid 平台身份标签
- **log label 白名单**：`detected_level|device_id|filename|identifier|level|ongrid_source|priority|service_name|severity|unit` 是 ongrid 已知 log selector label

## 8. 注意事项

- **`simpleHostMetricPredicatePartRE` 允许 metric 名含空格**：`[a-zA-Z0-9_\-\s]*` 含 `\s`，会被 `canonicalAlertMetric` trim 处理
- **`promMetricNameRE` 不限制长度**：PromQL metric 名无长度限制
- **`nodeFilesystemMetricSelectorRE` 不处理嵌套 `{}`**：`[^}]*` 假设 selector 内无嵌套 `}`
- **`promTrailingComparisonRE` `(?s)` 开启 dotall**：`.` 匹配换行，但本正则无 `.`，`(?s)` 实际无影响（可能是防御性编写）
- **`logLabelPrefixFilterRE` 值格式限制**：`[A-Za-z0-9_:/-]+` 不支持中文/Unicode 值
- **`logLabelAlternationPrefixFilterRE` `[=:]` 操作符**：仅支持 `=` 和 `:`，不含 `!=`/`=~`/`!~`
- **正则编译失败 panic**：`regexp.MustCompile` 编译失败会 panic；本文件正则均为字面量，编译期安全
- **正则性能**：高频调用场景下，正则有开销；但 alertdraft 是草案编译路径，非热路径
