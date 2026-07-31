# skill/http.go 技术实现文档

## 1. 概述

`http.go` 是 ongrid manager skill 框架的 HTTP 路由层。skill 是可在 edge 上执行的能力单元（如运维脚本、检查命令）。提供 3 个端点：list（按 category 过滤）/ get（单 skill 元数据）/ execute（在指定 edge 上执行）。共 3 个端点，任何已认证用户可调。

## 2. 包信息

- **包名**：`skill`
- **导入路径**：`github.com/ongridio/ongrid/internal/manager/server/skill`
- **路由前缀**：`/v1/skills`（由 `cmd/ongrid` 挂载，JWT auth 中间件由上游注入）
- **文件定位**：HTTP 适配层（薄 handler —— auth + JSON decode + delegate to biz Service）

## 3. 关键类型与接口

### Service —— 窄接口

```go
type Service interface {
    List(ctx context.Context, caller svc.Caller, category string) []svc.SkillSummary
    Get(ctx context.Context, caller svc.Caller, key string) (*svc.SkillSummary, error)
    Execute(ctx context.Context, caller svc.Caller, in svc.ExecuteInput) (*svc.ExecuteOutput, error)
}
```

由 `*biz/skill.Service` 满足。`List` 返 slice（非 error）——空列表即无 skill。

### Handler

```go
type Handler struct {
    svc Service
}
```

### DTO

```go
type listResp struct {
    Items []svc.SkillSummary `json:"items"`
    Total int                `json:"total"`
}

type executeReq struct {
    EdgeID uint64          `json:"edge_id"`
    Params json.RawMessage `json:"params,omitempty"`
}
```

**关键**：`executeReq.EdgeID` 是 uint64（非指针），`edge_id == 0` 由 service 层按 scope 决定是否报错——`ScopeManager` skill（web_search / subprocess packs）跳过检查，`ScopeHost` skill 仍 400。

`Params json.RawMessage` 透传原始 JSON，handler 不感知 skill 参数 schema。

## 4. 关键函数与流程

### Register —— 路由表

```go
func (h *Handler) Register(r chi.Router)
```

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| GET | `/v1/skills` | any auth | 列所有 skill，可选 `?category=` |
| GET | `/v1/skills/{key}` | any auth | 单 skill 元数据 |
| POST | `/v1/skills/{key}/execute` | any auth | 在 edge 上执行 skill |

### list

```go
func (h *Handler) list(w http.ResponseWriter, r *http.Request)
```

`callerFromRequest` → `svc.List(ctx, caller, category)` → 200 + `{items, total}`。

### get

```go
func (h *Handler) get(w http.ResponseWriter, r *http.Request)
```

`callerFromRequest` + `chi.URLParam("key")` → `svc.Get(ctx, caller, key)` → 200 + `SkillSummary`。

### execute

```go
func (h *Handler) execute(w http.ResponseWriter, r *http.Request)
```

1. `callerFromRequest`
2. `chi.URLParam("key")`，空则 400
3. `json.Decode(&executeReq)`
4. `svc.Execute(ctx, caller, ExecuteInput{Key, EdgeID, Params})`
5. 200 + `ExecuteOutput`

**关键**：`edge_id == 0` 不在 handler 校验，下沉到 service 层按 scope 决定。

### helpers

- `callerFromRequest(r)` —— 从 `tenantctx` 构造 `svc.Caller{UserID, Role}`
- `writeJSON` / `writeErr` / `errCode` —— 标准 errs 映射
- `parseUint64(s)` —— 测试用 helper，导出但当前未用（预留 by-id 路由）

## 5. 依赖关系

**外部**：
- `chi` —— URL 参数
- `net/http`、`encoding/json`、`strconv`、`errors`、`fmt`、`context`

**内部**：
- `internal/manager/biz/skill`（Service + Caller + SkillSummary + ExecuteInput/Output）
- `internal/pkg/errs`
- `internal/pkg/tenantctx`

## 6. 并发与资源管理

- **无共享可变状态**：`Handler.svc` 启动时装配
- **请求级隔离**：每请求独立 ctx
- **无 body 大小限制**：`execute` 的 `json.Decode(r.Body)` 未套 `MaxBytesReader`——params 可能很大（如 subprocess 命令），但严格防御应加上
- **`writeJSON` swallow encode 错误**：响应已开始无法回传

## 7. 设计模式与亮点

1. **`List` 返 slice 非 error**：空列表即无 skill，简化调用方（无需处理 error + empty 双 case）
2. **`ExecuteInput.Params json.RawMessage`**：透传原始 JSON，handler 不感知 skill 参数 schema——支持任意 skill 类型
3. **`edge_id == 0` 下沉 service 校验**：scope-dependent（ScopeManager 跳过，ScopeHost 报错），handler 不重复 scope 逻辑
4. **`parseUint64` 导出但未用**：预留 by-id 路由，避免未来 import 抖动
5. **`callerFromRequest` 从 tenantctx 构造**：与其它子域同模式
6. **任何已认证用户可调**：skill 执行不卡 admin，RBAC 在 service 层按 caller 决定可用 skill 集
7. **`errCode` 标准 slug**：与 monitor/marketplace 同模式

## 8. 注意事项

1. **`execute` 无 body 限制**：params 可能很大，但恶意大 body 会耗内存；建议加 `MaxBytesReader`
2. **`edge_id == 0` 不在 handler 报错**：前端传 0 时 service 层按 scope 决定；如需统一可在 handler 加 scope 检查
3. **`Params json.RawMessage` 不验证**：handler 不校验 params 是否符合 skill 定义，由 service 层负责
4. **`List` 无分页**：返所有 skill，skill 数量大时需评估
5. **`parseUint64` 导出但未用**：lint 可能报警 dead code；如确定不用可删除
6. **无审计**：`execute` 未调 `SetAuditEvent`，skill 执行不写审计；如需追溯需补 hook（执行类操作通常建议审计）
7. **任何已认证用户可执行**：包括 viewer；如需限制写操作需在 service 层按 caller role 过滤
8. **`key` 是字符串**：skill 用 key（如 `web_search`）而非数字 ID，前端按 key 调用
9. **`ExecuteOutput` schema 由 biz 层决定**：handler 透传，前端按 skill 类型解析
10. **`callerFromRequest` 失败时 caller 零值**：返 `svc.Caller{}, false`，调用方必须检查 ok
