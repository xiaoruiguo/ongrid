# `host_files_register.go` 技术实现文档

> 源文件：`internal/manager\biz\aiops\tools\host_files_register.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools`

## 1. 概述

本文件是 `host_files` 三个 BaseTool（`find_large_files` / `du_summary` / `stat_file`）的 **注册器**：`AppendHostFilesTools(bag, caller, edges, devices, log)` 把三个 tool append 到 `ToolBag`，返回 bag 供 caller 链式调用。Wiring contract：caller 传与 `Registry.Tool` 相同的依赖三元组（caller + edges + devices），三者必须非 nil——任一为 nil 则 helper 返回 bag 不变（gating pattern，让 host_files 只在 tunnel + device junction 都 online 的部署上 wire，mirror `NewRegistry` 的 promQuery/logQuery/alertUC 检查）。log 为 nil 时 default 到 `slog.Default()`。

## 2. 包信息

- **包名**：`tools`
- **所属模块**：`internal/manager/biz/aiops/tools`
- **依赖方向**：被 `BuildBaseTools`（或未来 BaseTool registry helper）调用；依赖 `devicebiz.Usecase` / `edgebiz.Usecase` / `Caller` / `ToolBag` / `slog`。调用 `NewFindLargeFilesTool` / `NewDuSummaryTool` / `NewStatFileTool`（`host_files_basetool.go`）。

## 3. 关键类型与接口

```go
func AppendHostFilesTools(bag *ToolBag, c Caller, e *edgebiz.Usecase, d *devicebiz.Usecase, log *slog.Logger) *ToolBag
```

无新增类型。函数签名是关键：接收 `ToolBag` 指针 + 依赖三元组 + log，返回同一 `ToolBag` 指针（链式）。

## 4. 关键函数与流程

```go
func AppendHostFilesTools(bag, c, e, d, log) *ToolBag
```

流程：
1. `bag == nil` → 返回 bag（nil-safe）。
2. **Gating**：`c == nil || e == nil || d == nil` → 返回 bag 不变。注释明示 "callers can wire host_files only on deployments where the tunnel + device junction are both online"。
3. `log == nil` → `log = slog.Default()`。
4. `bag.Append(NewFindLargeFilesTool(c, e, d, log))`。
5. `bag.Append(NewDuSummaryTool(c, e, d, log))`。
6. `bag.Append(NewStatFileTool(c, e, d, log))`。
7. 返回 bag。

## 5. 依赖关系

- **ToolBag**：`Append(tool BaseTool)` 方法。本文件不定义 ToolBag，仅在 `toolbag.go` 定义。
- **Caller / edgebiz.Usecase / devicebiz.Usecase**：依赖三元组，由 caller 传入。
- **NewFindLargeFilesTool / NewDuSummaryTool / NewStatFileTool**（`host_files_basetool.go`）：构造三个 BaseTool 实例。
- **slog.Default()**：log 降级。

## 6. 并发与资源管理

- **无锁、无共享可变状态**：纯注册函数，不持有状态。`ToolBag.Append` 自身负责并发安全（若 bag 实现是线程安全的）。
- **无 goroutine**：同步构造三个 tool 并 append。
- **无资源持有**：注册完成后，tool 实例由 bag 持有。

## 7. 设计模式与亮点

- **Gating pattern**：`c == nil || e == nil || d == nil` 时返回 bag 不变。mirror `NewRegistry` 的 promQuery/logQuery/alertUC 检查——让 host_files 只在 tunnel + device junction 都 online 的部署上 wire。部署缺 tunnel（如纯 metric 监控模式）则 host_files 工具不注册，LLM 看不到。
- **链式返回**：`return bag` 让 caller 链式调用 `AppendHostFilesTools(bag, ...).AppendXxx(...)`。
- **nil-safe**：`bag == nil` 直接返回，不 panic。
- **log 降级**：`log == nil` → `slog.Default()`，与所有 BaseTool 构造函数一致。
- **单一注册点**：三个 host_files 工具集中注册，caller 一行调用即可全量 wire。若未来加第四个 host_files 工具（如 `host_tail_file`），只需在此函数加一行 `bag.Append(NewXxxTool(...))`。
- **Tier 暗示**：注释明示 "when the bag is in deferring mode, host_files lands in the 'specialty' tier (per tierByName) so its schema is redacted by default. Below threshold it stays in core alongside everything else."——bag 的 tier 机制决定 host_files schema 是否默认 redacted。

## 8. 注意事项

- **Gating 是部署级开关**：若部署缺 tunnel 或 device junction，host_files 三个工具都不注册。LLM 完全看不到这些工具。这是有意设计，避免 LLM 调用必然失败的工具。
- **`ToolBag.Append` 顺序**：三个工具按 find_large_files → du_summary → stat_file 顺序 append。若 bag 内有 tier 或 priority 排序，顺序可能影响 LLM 看到的 tool 顺序。
- **注释提到 "tierByName"**：这是 `toolbag.go` 的 tier 机制。host_files 在 deferring mode 下落 "specialty" tier，schema 默认 redacted。caller 需 awareness，若希望 host_files 始终在 core tier，需调整 bag 模式或 tierByName。
- **无单测**：本文件是纯注册逻辑，测试通过 `host_files_basetool_test.go` 覆盖各 tool 行为，注册器本身未单独测。
- **caller 必须传相同依赖三元组**：与 `Registry.Tool` 闭包路径相同的 caller + edges + devices。若 caller 传 nil edges 但非 nil devices，gating 会触发（返回 bag 不变），host_files 不注册。
- **返回值是入参 bag**：非新 bag。caller 若忽略返回值也无影响（bag 已被 mutate）。
