# `batch_helper.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/batch_helper.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools`

## 1. 概述

本文件实现 N+15 batch-first BaseTool 重构的共享并发原语：`runBatch` 泛型函数（semaphore + waitgroup + 有序结果切片）和 `validateBatchIDs` 校验。6 个 per-id BaseTool（get_host_load / get_process_list / get_edge_summary / get_incident_detail / correlate_incident / bash）共用此 primitive，避免 6 份发散拷贝。hard ceiling 4 并发，匹配 edge host_files 的并发上限。

## 2. 包信息

- **包名**：`tools`
- **所属模块**：`internal/manager/biz/aiops/tools`
- **依赖方向**：被 batch-flavored BaseTool 调用；依赖标准库

## 3. 关键类型与接口

无导出类型。常量：

```go
const batchMaxIDs = 16       // 服务端硬上限，schema maxItems=16 LLM 端守
const batchConcurrency = 4   // 并发 fan-out 上限，匹配 edge host_files
```

## 4. 关键函数与流程

### `runBatch`
- **签名**：`func runBatch[ID any, R any](ctx context.Context, ids []ID, fn func(ctx context.Context, id ID) R) []R`
- **职责**：并发 fan-out fn 到每个 id，按输入顺序返回结果
- **流程**：
  1. `results := make([]R, len(ids))`；len 0 → 返回空切片
  2. `sem := make(chan struct{}, batchConcurrency)` 信号量
  3. for i, id := range ids：`wg.Add(1)`；`i, id := i, id`（捕获循环变量）；`sem <- struct{}{}` 阻塞等槽；goroutine：defer Done + defer `<-sem`；`results[i] = fn(ctx, id)`
  4. `wg.Wait()` 等所有完成
  5. 返回 results
- **错误处理**：fn 无 error 返回——partial failure 编码进 R（典型为 `R.Error string` 字段）；helper 自身无 error return（fan-out 不能"全局失败"）
- **ctx 处理**：不传播 ctx.Done() 快速 shutdown；fn 已在 caller 的 derived ctx 下跑，per-tool timeout 主导每个 child 的时长；wg join 无条件保证切片总被写满

### `validateBatchIDs`
- **签名**：`func validateBatchIDs[T any](idLabel string, ids []T) error`
- **职责**：服务端 belt-and-braces 校验 1..batchMaxIDs
- **流程**：len 0 → `fmt.Errorf("%s: must contain at least 1 element")`；len > batchMaxIDs → `fmt.Errorf("%s: too many (%d > max %d)")`
- **说明**：schema 的 minItems/maxItems 已覆盖 LLM 端；此函数防 schema validator 被绕过（测试 harness、手构 argsJSON）

## 5. 依赖关系

- **内部包**：无
- **外部库**：标准库 `context`、`fmt`、`sync`
- **被调用方**：`bash_basetool.go`、`host_load_basetool.go`、`host_processes_basetool.go`、`get_edge_summary_basetool.go`、`get_incident_detail_basetool.go`、`correlate_incident_basetool.go` 等 batch tool

## 6. 并发与资源管理

- `sem chan struct{}` 容量 4：硬限并发 goroutine 数
- `sync.WaitGroup`：等所有 goroutine 完成
- `results` 切片每个 index 由唯一 goroutine 写入，无竞争
- ctx 透传给 fn；helper 不主动 cancel
- goroutine 内 `defer` 顺序：先 Done 再 release sem（sem release 在 Done 后避免 sem 释放早于 goroutine 退出）

## 7. 设计模式与亮点

- **泛型 type-safe 端到端**：`ID` 和 `R` 独立类型参数；fn 直接返回 tool 特定的 ResultEntry，无需 `any` 装箱或反射
- **顺序保留**：`results[i]` 对应 `ids[i]`，与完成顺序无关——caller 可直接遍历计算 success/error count
- **partial failure in-band**：fn 不返回 error，partial failure 写入 R 的 Error 字段，保证切片总是 full-length
- **4 并发上限**：匹配 edge host_files 的并发，超过 4 会在典型 edge 服务能力外排队，浪费 manager goroutine
- **belt-and-braces 校验**：服务端守 schema 约束的二次防线

## 8. 注意事项

- **fn 必须 self-contained**：catch 自己的 error 写入 R.Error，不能 propagate
- **不快速 shutdown**：ctx cancel 不立即终止未完成 fn；fn 自己应尊重 ctx
- **goroutine 捕获循环变量**：`i, id := i, id` 在 Go <1.22 必需；Go 1.22+ 可省略但保留兼容
- **batchMaxIDs=16 与 hostFilesMaxBatchPaths 镜像**：让 LLM 看统一 ceiling
- **batchConcurrency 可调**：未来若 batch tool 上游更快可调整此常量
