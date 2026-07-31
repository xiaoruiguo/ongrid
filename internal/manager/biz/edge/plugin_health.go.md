# `plugin_health.go` 技术实现文档

> 源文件：`internal/manager/biz/edge/plugin_health.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/edge`

## 1. 概述

本文件实现 per-edge per-plugin 运行时健康状态的内存缓存。`PluginHealth` 是 edge 心跳上报的瞬态数据（state/last_error/restart_count/pid/started_at + per-target health），仅内存（manager 重启清空，30s 内重新填充）。核心价值：**操作员可见性** —— 把"logs 插件静默无输出"变成"logs: crashed — subprocess binary missing"。

## 2. 包信息

- **包名**：`edge`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：仅被 `Usecase`（同包 usecase.go）的 `RecordPluginHealth`/`PluginHealth` 方法使用；依赖 `time`

## 3. 关键类型与接口

```go
type PluginHealth struct {
    Name         string               `json:"name"`
    State        string               `json:"state"` // stopped|starting|running|crashed
    LastError    string               `json:"last_error,omitempty"`
    RestartCount int                  `json:"restart_count,omitempty"`
    PID          int                  `json:"pid,omitempty"`
    StartedAt    time.Time            `json:"started_at,omitempty"`
    UpdatedAt    time.Time            `json:"updated_at,omitempty"`   // edge 端更新时间
    ReportedAt   time.Time            `json:"reported_at,omitempty"`  // manager 接收时间
    Targets      []PluginTargetHealth `json:"targets,omitempty"`
}

type PluginTargetHealth struct {
    ID, Name, Kind, State, LastError string
    Samples int
    LastSuccessAt, UpdatedAt time.Time
}
```

Sentinel：`State` 取值 `stopped|starting|running|crashed`。

## 4. 关键函数与流程

### `RecordPluginHealth`
- **签名**：`func (u *Usecase) RecordPluginHealth(edgeID uint64, items []PluginHealth)`
- **职责**：存储一个 edge 的最新 plugin 健康快照（覆盖式）
- **流程**：
  1. edgeID==0 或 len(items)==0 → return（心跳无 plugin 数据**不能**抹掉之前的快照）
  2. now = `time.Now().UTC()`；遍历 items 盖 `ReportedAt = now`（manager 时钟，UI 显示"reported 4m ago"独立于 edge 时钟偏差）
  3. `phMu.Lock`
  4. `pluginHealth` map nil → lazy init
  5. `pluginHealth[edgeID] = items`
  6. defer Unlock
- **错误处理**：无 error；no-op for edgeID 0 或空 items

### `PluginHealth`
- **签名**：`func (u *Usecase) PluginHealth(edgeID uint64) []PluginHealth`
- **职责**：读取一个 edge 的最新 plugin 健康；无数据返回 nil
- **流程**：
  1. `phMu.RLock`
  2. `src := pluginHealth[edgeID]`
  3. len(src)==0 → nil
  4. **copy** out slice（防调用方修改内部状态）
  5. RUnlock
  6. 返回 out
- **错误处理**：无 error；nil for 无数据

## 5. 依赖关系

- **外部库**：`time`
- **被调用方**：`Usecase` 同包；HTTP handler（edge 详情页"插件健康"面板）
- **数据来源**：edge 心跳上报（frontierbound 解码后调 RecordPluginHealth）

## 6. 并发与资源管理

- **`phMu sync.RWMutex`**：保护 `pluginHealth map[uint64][]PluginHealth`
- **读多写少**：RecordPluginHealth 用 Lock（每次心跳一次写）；PluginHealth 用 RLock（UI 频繁读）
- **copy-on-read**：PluginHealth 返回 slice 副本（防调用方修改内部状态）
- **lazy init**：`pluginHealth` map 首次写时 init；零值 Usecase 可用
- **无上限**：edge 数有限；每 edge ~8 个 plugin；内存可控

## 7. 设计模式与亮点

- **瞬态内存 only**：注释明示"intentionally ephemeral"；manager 重启清空，30s 内心跳重新填充
- **ReportedAt 用 manager 时钟**：UI 显示"reported 4m ago"独立于 edge 时钟偏差
- **no-op for 空 items**：心跳无 plugin 数据不抹掉之前快照（防 transient 心跳清空 UI）
- **copy-on-read**：返回 slice 副本；调用方修改不影响内部状态
- **lazy init**：零值 Usecase 可用；pluginHealth map 首次写时 init
- **RWMutex 读多写少**：RecordPluginHealth Lock；PluginHealth RLock
- **per-target health**：`PluginTargetHealth` 支持 metric 子插件多 target 复用（custommetrics/databasemetrics）

## 8. 注意事项

- **瞬态 only**：manager 重启清空；UI 应处理 nil（edge 离线/预引入 agent/刚重启 manager）
- **30s 心跳填充**：edge 心跳间隔约 30s；重启后 30s 内 UI 显示 nil
- **覆盖式写**：每次心跳覆盖整个 edge 的 plugin 健康 slice；不支持增量更新
- **ReportedAt 是 manager 时钟**：与 edge 的 UpdatedAt 不同；UI 显示 staleness 用 ReportedAt
- **edgeID 0 no-op**：防误调用清空
- **空 items no-op**：防 transient 心跳清空；但 edge 真的没 plugin 也应上报空 slice（与"心跳无 plugin 数据"区分）
- **无 TTL 清理**：edge 离线后 pluginHealth 保留；UI 应配合 edge status 判断
- **State 4 值**：stopped/starting/running/crashed；UI 用语义色（crashed=red）
