# `burn_rate_sli.go` 技术实现文档

> 源文件：`internal/manager/biz/alert/burn_rate_sli.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/alert`

## 1. 概述

本文件是 burn-rate 规则 SLI 表达式与 SLO 百分比的归一化工具。它处理两件事：(1) 把用户写入的 SLI 表达式里的简单范围选择器 `[1h]` 替换为占位符 `[$window]`，使同一 SLI 可以套用到不同的 burn-rate 窗口；(2) 把 SLO 从 0~1 的小数归一化为 0~100 的百分数，统一存储与运行时的算术形式。整个文件是纯函数，无状态、无副作用，便于单测覆盖。

## 2. 包信息

- **包名**：`alert`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 `rules.go::compileMetricBurnRateRule` / `usecase.go::buildConditionsJSON` 调用；依赖 `regexp` + `strings`

## 3. 关键类型与接口

本文件不声明类型与接口，只暴露三个纯函数与一个包级正则。

```go
var burnRateSimpleRangeSelectorRE = regexp.MustCompile(`\[(\d+(?:ms|s|m|h|d|w|y))\]`)
```

正则只识别 PromQL 标准的简单范围选择器（`[5m]` / `[1h]` / `[2d]` 等），不会触碰子查询形如 `[5m:30s]` 的选择器——后者本身包含 `:`，不被 `burnRateSimpleRangeSelectorRE` 匹配。

## 4. 关键函数与流程

### `normalizeBurnRateSLIExpression`

- **签名**：`func normalizeBurnRateSLIExpression(sli string) string`
- **职责**：把 SLI 表达式归一化为含 `$window` 占位符的模板形式
- **流程**：
  1. `strings.TrimSpace` 去掉首尾空白
  2. 如果表达式已包含 `$window`，原样返回（已归一化）
  3. 否则用 `burnRateSimpleRangeSelectorRE` 把所有 `[<dur>]` 替换为 `[$window]`
- **错误处理**：无错误返回；不识别的表达式（无范围选择器且无 `$window`）原样返回，由调用方在 `compileMetricBurnRateRule` / `buildConditionsJSON` 中校验"必须含 `$window` 或范围选择器"

### `burnRateSLIUsesWindow`

- **签名**：`func burnRateSLIUsesWindow(sli string) bool`
- **职责**：判断 SLI 是否已使用 `$window` 占位符
- **流程**：直接 `strings.Contains(sli, "$window")`
- **错误处理**：无

### `normalizeBurnRateSLOPercent`

- **签名**：`func normalizeBurnRateSLOPercent(slo float64) float64`
- **职责**：把 SLO 从 0~1 小数形式归一化为 0~100 百分数形式
- **流程**：
  1. 若 `slo > 0 && slo <= 1`，返回 `slo * 100`
  2. 否则原样返回（已是百分数或越界值，越界由 `compileMetricBurnRateRule` 校验拒绝）
- **错误处理**：无；越界值（≤0 或 ≥100）由调用方校验

## 5. 依赖关系

- **内部包**：无
- **外部库**：仅标准库 `regexp` + `strings`
- **被调用方**：`rules.go`（`compileMetricBurnRateRule`）、`usecase.go`（`buildConditionsJSON` 的 `RuleKindMetricBurnRate` 分支）

## 6. 并发与资源管理

- 无状态、无锁；正则在包级 `var` 初始化，启动后只读，跨 goroutine 安全
- 所有函数无 IO、无 context，纯计算

## 7. 设计模式与亮点

- **占位符约定**：SLI 写法 `sum(rate(http_requests_total{code!~"5.."}[$window])) / sum(rate(http_requests_total[$window]))` 是 burn-rate 规则的契约，让 `windowedSLI(sli, "5m")` / `windowedSLI(sli, "1h")` 可以套用同一模板到不同窗口
- **归一化与校验分离**：本文件只做归一化，校验（必须含 `$window`、SLO 在 (0,100)）由 `rules.go::compileMetricBurnRateRule` 与 `usecase.go::buildConditionsJSON` 在编译期完成，单一职责
- **0~1 与 0~100 双兼容**：用户在 UI 里既可以输入 `0.999` 也可以输入 `99.9`，归一化后统一按 0~100 存储；运行时 `budget := 1 - slo/100` 计算 burn budget
- **简单范围选择器识别**：正则只识别 `[<dur>]`，不识别子查询 `[5m:30s]`——后者本身不会被替换为 `[$window]`，避免误改

## 8. 注意事项

- **正则不识别子查询**：`[5m:30s]` 形式的子查询选择器不会被替换为 `[$window]`，调用方需在 UI 提示用户使用 `$window` 占位符而非子查询
- **SLO 边界值不归一化**：`slo == 1` 被视为已百分化（`1%`）原样返回；`slo == 0` 也原样返回（后续被校验拒绝）。这是有意为之——避免把 `1`（即 1%）误转成 `100%`
- **无 `$window` 也无范围选择器的 SLI 会原样返回**：但 `burnRateSLIUsesWindow` 返回 false，调用方 `compileMetricBurnRateRule` 会以 `"sli must use $window or a PromQL range selector"` 拒绝
