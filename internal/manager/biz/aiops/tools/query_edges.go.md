# query_edges.go

## 1. 概述

本文件实现 `query_devices`（Go 标识符沿用历史名 `query_edges`）工具的闭包路径，列 ongrid 管理的设备（hosts）按 role / online status / last-seen freshness / name 子串 / IP 子串过滤。

**Post-split 命名约定**（May 2026 设备/edge 拆分后）：
- LLM 看到的 wire name 是 `query_devices`
- Go 标识符保留 `query_edges` / `QueryEdgesArgs` / `EdgeRow` 以维持源码稳定性
- 返回字段用 `device_id`，提示 LLM 在生成的 PromQL/LogQL/TraceQL 里用 `device_id`（不是 `edge_id`）

`EdgeRow` 字段故意收窄，不暴露 `secret_key_hash` / `access_key_id` 等敏感字段。

## 2. 包信息

- **包名**：`tools`
- **路径**：`internal/manager/biz/aiops/tools/query_edges.go`
- **导入**：
  - `devicebiz`（`internal/manager/biz/device`）—— `ListFilter`
  - `edgebiz`（`internal/manager/biz/edge`）—— 旧路径 fallback
  - `devicemodel`（`internal/manager/model/device`）—— Role 常量 + `DecodeRoles`
- **Class**：`read`

## 3. 关键类型与接口

### `QueryEdgesArgs`

```go
type QueryEdgesArgs struct {
    Role                  string `json:"role,omitempty"`                    // server/storage/network/database
    Status                string `json:"status,omitempty"`                  // online/offline
    LastSeenWithinMinutes int    `json:"last_seen_within_minutes,omitempty"`
    NameContains          string `json:"name_contains,omitempty"`
    IPContains            string `json:"ip_contains,omitempty"`
    Limit                 int    `json:"limit,omitempty"`                   // 默认 50，cap 500
}
```

### `EdgeRow`（wire shape）

```go
type EdgeRow struct {
    ID         uint64     `json:"device_id"`
    Name       string     `json:"name"`
    Hostname   string     `json:"hostname,omitempty"`
    IPAddress  string     `json:"ip_address,omitempty"`
    Online     bool       `json:"online"`
    Roles      []string   `json:"roles"`
    LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
}
```

注意：`ID` 的 JSON key 是 `device_id`（不是 `edge_id`），与拆分后的术语一致。

### `Registry` 持有的依赖

- `r.devices`（`*devicebiz.Usecase`）—— 首选路径
- `r.edges`（`*edgebiz.Usecase`）—— legacy fallback

## 4. 关键函数与流程

### `executeQueryEdges(ctx, args) (ExecuteResult, error)`

1. 校验 `r.devices == nil && r.edges == nil` → `"device usecase not configured"`。
2. Unmarshal `QueryEdgesArgs`，校正 limit（≤0 → 50，>500 → 500）。
3. `context.WithTimeout(ctx, 10s)`。
4. **首选 device 路径**（`r.devices != nil`）：
   - 构造 `devicebiz.ListFilter{Name, IPAddress, Limit}`。
   - `Status` 映射到 `*bool`：`online→true`，`offline→false`，空串不设。
   - `Role` 映射到 `RolesAny` bit：`server/storage/network/database` 对应 `RoleBitServer` 等；未知值 → error。
   - 调 `r.devices.List(callCtx, f)`。
   - 内存过滤 `LastSeenWithinMinutes`（cutoff = `now - N min`，`LastSeenAt` 早于 cutoff 跳过）。
   - 内存过滤 `NameContains`（同时匹配 `Name` 或 `Hostname`）。
   - 截断到 `Limit`，Marshal `{devices: rows, count: len(rows)}` 返回。
5. **Legacy fallback 路径**（`r.devices == nil`）：
   - 用 `edgebiz.ListFilter{Status, Name, Limit}` 调 `r.edges.List`。
   - 同样的 `LastSeenWithinMinutes` + `NameContains` 内存过滤。
   - `EdgeRow` 只填 `ID/Name/Online(e.Status=="online")/LastSeenAt`，无 hostname/ip/roles。
   - 注意：edge 路径**无法**按 role 过滤（edge 没有角色概念）。

## 5. 依赖关系

| 方向 | 依赖 | 说明 |
|------|------|------|
| 上游 | `Registry`（闭包持有者） | `r.devices` / `r.edges` / `r.log` |
| 下游 | `devicebiz.Usecase.List` | 首选 |
| 下游 | `edgebiz.Usecase.List` | legacy fallback |
| 类型 | `devicemodel.RoleBit*` / `DecodeRoles` | 角色位运算 |

## 6. 并发与资源管理

- `context.WithTimeout(ctx, 10s)` per call。
- 无共享可变状态，纯函数式过滤 + marshal。
- `Limit` cap 500 防止大 fleet 拉爆 LLM 上下文。

## 7. 设计模式与亮点

- **Post-split 术语迁移**：Go 标识符保留 `query_edges`（避免大改），wire name 改为 `query_devices`，返回字段用 `device_id`，并在描述里明确告诉 LLM "use device_id (NOT edge_id) in any PromQL/LogQL/TraceQL"。这种"源码稳定 + 提示面迁移"的拆分降低 PR 噪音。
- **双路径 fallback**：首选 device usecase，回退到 edge usecase 兼容老 fixture。注释明示"older test fixtures"——生产路径必然走 device，edge fallback 只是测试友好。
- **`EdgeRow` 字段收窄**：故意不暴露 `secret_key_hash` / `access_key_id`，避免敏感字段泄漏到 LLM 上下文。
- **Role 用 bit 位**：`RolesAny = RoleBitServer` 等，支持"按角色位匹配"的语义（一个设备可有多个角色位）。返回时 `DecodeRoles` 解码为字符串数组。
- **NameContains 双字段匹配**：device 路径同时匹配 `Name` 和 `Hostname`，edge 路径只匹配 `Name`（edge 无 hostname 概念），这种不对称是 fallback 的代价。

## 8. 注意事项

- **角色校验不对称**：device 路径对未知 role 返回 error（fail fast），edge 路径根本不支持 role 过滤——LLM 在 fallback 场景传 role 会被静默忽略。
- **`LastSeenWithinMinutes` 在内存过滤**：`List` 已经按 DB limit 截断，再在内存做 freshness 过滤可能得到少于 `Limit` 的行（DB 已经返回了被 freshness 排除的行）。这是已知 trade-off，`devicebiz.ListFilter` 没暴露 `LastSeenSince` 字段。
- **NameContains 在 List 之后过滤**：同上，可能少于 Limit。`ListFilter.Name` 已经在 DB 端过滤，但代码里又做一次 `strings.Contains(d.Name || d.Hostname)`——这是为了 hostname 兜底，因为 `ListFilter.Name` 只匹配 name 字段。
- **`EdgeRow.Hostname` 和 `IPAddress` 用 omitempty**：legacy edge 路径返回的行不带这俩字段，wire shape 自然省略，前端 / LLM 都能容忍。
- **闭包路径与 BaseTool 路径并存**：见 `query_edges_basetool.go`，两路径 byte-for-byte 等价（注释明示 "output bytes match for equivalent inputs"），存在 drift 风险，未来应抽共享 helper 或一行 type alias 切换。
