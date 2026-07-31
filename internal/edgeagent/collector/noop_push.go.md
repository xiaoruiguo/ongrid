# `noop_push.go` 技术实现文档

> 源文件：`internal/edgeagent/collector/noop_push.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/collector`

## 1. 概述

本文件实现 `NoopPushCollector`：抑制周期性 push 路径（`CollectAll` 返回 nil），但保留按需 RPC（`HostInfo` / `GetHostLoad` / `GetProcessList`）委托给内嵌 `EmbeddedCollector`。当 `hostmetrics` 插件（node_exporter 子进程）运行时，manager 直接 scrape host metrics，embedded push 会产出重复 `node_*` series（`ongrid_source=embedded` 标签），污染 Monitor 面板——本包装消除该噪声。

## 2. 包信息

- **包名**：`collector`
- **所属模块**：edgeagent metric 采集层（noop-push 模式）
- **依赖方向**：被 `cmd/ongrid-edge` 在 plugin 模式下构造；调用 `tunnel`

## 3. 关键类型与接口

```go
// NoopPushCollector 包装 EmbeddedCollector，仅 stub CollectAll
type NoopPushCollector struct {
    emb *EmbeddedCollector  // nil 时所有 RPC 返回空响应（测试模式）
}
```

## 4. 关键函数与流程

### `NewNoopPush`
- **签名**：`func NewNoopPush(emb *EmbeddedCollector) *NoopPushCollector`
- **职责**：构造 noop-push 包装器
- **流程**：直接返回结构体字面量
- **错误处理**：无错误返回

### `CollectAll`
- **签名**：`func (n *NoopPushCollector) CollectAll(_ context.Context) ([]CollectorOutput, error)`
- **职责**：返回空切片抑制周期 push
- **流程**：直接 `return nil, nil`
- **错误处理**：永远返回 nil

### `HostInfo`
- **签名**：`func (n *NoopPushCollector) HostInfo(ctx context.Context) (tunnel.HostInfo, error)`
- **职责**：委托给 emb
- **流程**：emb 为 nil 返回空 HostInfo；否则 `n.emb.HostInfo(ctx)`
- **错误处理**：透传 emb 错误

### `GetHostLoad`
- **签名**：`func (n *NoopPushCollector) GetHostLoad(ctx context.Context) (tunnel.GetHostLoadResponse, error)`
- **职责**：委托给 emb
- **流程**：emb 为 nil 返回空响应；否则 `n.emb.GetHostLoad(ctx)`
- **错误处理**：透传 emb 错误

### `GetProcessList`
- **签名**：`func (n *NoopPushCollector) GetProcessList(ctx context.Context, topN int, sortBy string) (tunnel.GetProcessListResponse, error)`
- **职责**：委托给 emb
- **流程**：emb 为 nil 返回空响应；否则 `n.emb.GetProcessList(ctx, topN, sortBy)`
- **错误处理**：透传 emb 错误

## 5. 依赖关系

- **内部包**：`internal/pkg/tunnel`
- **外部库**：标准库 `context`
- **被调用方**：`cmd/ongrid-edge` 主程序（plugin 模式下用 `NewNoopPush(NewEmbedded(log))` 替代直接用 `EmbeddedCollector`）

## 6. 并发与资源管理

无并发控制。`NoopPushCollector` 是无状态包装器，所有方法委托给 `emb`。`EmbeddedCollector` 自身通过 `sync.Mutex` 保证 snapshot 串行化。

## 7. 设计模式与亮点

- **装饰器模式**：`NoopPushCollector` 包装 `EmbeddedCollector`，仅覆盖 `CollectAll` 行为；其他方法透传——经典装饰器，无需重新实现 gopsutil 调用
- **抑制而非禁用**：保留按需 RPC（get_host_load / get_process_list / host_info）让 AIOps 工具和 EdgeDetail「current load」卡片继续工作；这些 RPC 每次取新鲜 gopsutil 样本，不产生重复 Prom series
- **nil-safe**：`emb == nil` 时所有 RPC 返回空响应——可用作测试桩
- **解决具体问题**：`ongrid_source=embedded` 标签的 `node_*` series 在 Monitor 面板产生额外 legend 行，纯噪声；本包装从源头消除

## 8. 注意事项

- 使用 `NoopPushCollector` 时，周期 push_prom_samples 完全不发——cloud 侧的 Prom series 仅来自 manager 直接 scrape hostmetrics 插件；若插件未运行，host metrics 完全缺失
- `HostInfo` 仍走 emb——register_edge 仍能正常上报主机身份
- `GetHostLoad` / `GetProcessList` 每次取新鲜样本——比缓存 push 数据更新鲜，但每次调用有 gopsutil 开销；高频调用应考虑缓存
- 测试模式（`emb == nil`）所有 RPC 返回空响应——可用于单元测试桩
- 若未来 hostmetrics 插件被废弃，应直接切回 `EmbeddedCollector`；本包装器是过渡方案
