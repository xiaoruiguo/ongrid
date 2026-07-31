# query_edges_basetool.go

## 1. 概述

本文件实现 `query_devices`（Go 标识符 `query_edges`）工具的 BaseTool 形态。镜像 `executeQueryEdges`（见 `query_edges.go`）：首选 device usecase，回退到 edge usecase。注释明示 "output bytes match for equivalent inputs"，即两路径对相同输入产生字节级一致的输出。

是任何 incident triage 链的**第一个**工具——先拿到 `device_id`，再喂给 `get_host_load` / `query_promql` / `get_edge_summary`。

## 2. 包信息

- **包名**：`tools`
- **路径**：`internal/manager/biz/aiops/tools/query_edges_basetool.go`
- **导入**：
  - `basetool`
  - `devicebiz` / `edgebiz` / `devicemodel`（同闭包路径）
  - 额外 `log/slog`
- **Class**：`read`

## 3. 关键类型与接口

### `QueryEdgesTool`

```go
type QueryEdgesTool struct {
    devices *devicebiz.Usecase  // 首选
    edges   *edgebiz.Usecase    // legacy fallback
    log     *slog.Logger
}
```

注意：BaseTool 形态直接持有 `*devicebiz.Usecase` 具体类型（而非 `deviceLister` 接口），与闭包路径 `Registry.devices` 类型一致，保证 fallback 语义等价。

`QueryEdgesArgs` / `EdgeRow` / `QueryEdgesSchema` / `QueryEdgesDescription` / `ToolNameQueryEdges` / `queryEdgesCallTimeout` 均复用 `query_edges.go` 的定义（共享）。

## 4. 关键函数与流程

### `NewQueryEdgesTool(devices, edges, log)`

`log == nil` → `slog.Default()`。两个 usecase 都可为 nil，工具降级到任何一个被 wire 的（与 `executeQueryEdges` 同语义）。

### `queryEdgesWhenToUse`（常量）

英文 LLM-facing 文案，强调：
- 用途 1：list devices / hosts（按 role/online/freshness/name/IP 过滤）
- 用途 2：作为 triage 链**第一步**，拿 `device_id` 喂下游工具
- NOT for：per-device deep dive（用 `get_edge_summary` 或 `correlate_incident`）/ log/metric/trace 内容

### `Info(_ context.Context)`

返回 `ToolInfo`：Name=`query_devices`，Class=`read`，含 WhenToUse。

### `InvokableRun(ctx, argsJSON, _ ...)`

主流程与 `executeQueryEdges` 完全镜像：

1. 校验 `devices == nil && edges == nil` → error。
2. Unmarshal，校正 limit（50 / 500）。
3. `context.WithTimeout(ctx, 10s)`。
4. **device 路径**（`devices != nil`）：
   - 构造 `ListFilter{Name, IPAddress, Limit}`。
   - Status → `*bool`，Role → bit（未知 role → error）。
   - `devices.List` → 内存过滤 freshness + NameContains（双字段 Name/Hostname）→ 截断 → Marshal。
5. **edge 路径**：
   - `edges.List(ListFilter{Status, Name, Limit})` → 同样的内存过滤 → Marshal。

Marshal 结构：`{devices: rows, count: len(rows)}`，与闭包路径一致。

## 5. 依赖关系

| 方向 | 依赖 | 说明 |
|------|------|------|
| 上游 | `basetool.BaseTool`、`ToolInfo` | 实现 BaseTool 接口 |
| 下游 | `*devicebiz.Usecase.List` | 首选 |
| 下游 | `*edgebiz.Usecase.List` | legacy fallback |
| 共享 | `query_edges.go` 中的 `QueryEdgesArgs` / `EdgeRow` / `QueryEdgesSchema` / `ToolNameQueryEdges` / `queryEdgesCallTimeout` | 避免重复定义 |

## 6. 并发与资源管理

- `context.WithTimeout(ctx, 10s)` per call。
- 无状态、无锁，并发安全（依赖 usecase 实现的线程安全）。
- Limit cap 500。

## 7. 设计模式与亮点

- **字节级一致承诺**：注释明示 "Mirror of executeQueryEdges; output bytes match for equivalent inputs"。这是有意为之，便于未来一行 type alias 切换时验证等价。
- **WhenToUse 强调"FIRST step"**：`queryEdgesWhenToUse` 把"作为 triage 第一步拿 device_id"列为用途 2，反 guard 强（NOT for deep dive / log / metric / trace），引导 LLM 在 incident 流程里**先**调它。
- **降级友好**：两个 usecase 都可 nil，工具自动走能走的那条路径——和闭包路径同样的测试 fixture 兼容性。
- **共享 schema / args / row 类型**：不在 basetool 文件重复定义，全部 import 自 `query_edges.go`，避免 schema drift。

## 8. 注意事项

- **drift 风险**：与 `query_edges.go` 是两份实现并行，任何一边改逻辑必须同步另一边，否则 "byte-for-byte" 承诺破裂。未来应抽共享 helper 或 type alias 切换。
- **直接持有具体类型**：与一些 BaseTool（如 `QueryAlertRulesTool` 用 `AlertUsecase` 接口）不同，这里直接持有 `*devicebiz.Usecase` 具体指针——这是为了和闭包路径 `Registry.devices` 类型对齐，避免接口适配层引入额外行为差。
- **WhenToUse 中英混合**：英文为主，但串了"devices / hosts"和"filter by role / online / freshness / name substring / IP address"——这是有意保留，让 LLM 在英文 prompt 和中文 user message 里都能对上。
- **未走 batch refactor（N+15）**：与 `get_edge_summary_basetool.go` / `get_incident_detail_basetool.go` 不同，这个工具本身就是 list 语义（一次返回多 device），不需要 `device_ids[]` batch 协议。
- **无 `AlertUsecase` 依赖**：纯设备查询，不碰 alert biz。
