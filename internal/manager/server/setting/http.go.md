# setting/http.go 技术实现文档

## 1. 概述

`http.go` 是 ongrid manager 系统设置子域（`/v1/system-settings`）的 HTTP 路由层。提供 admin 可编辑的运行时配置存储：list（任何已认证用户可读，敏感值 mask）/ put / delete / reveal（admin-only，返回明文）。共 4 个端点。

**关键设计**：sensitive key 自动检测（`*_api_key` / `*_secret` / `*_token` / `*_password`），UI 无需每次显式 opt-in。`reveal` 端点让 admin 能看到明文（eye-toggle），但不影响 list 的 mask 行为。

## 2. 包信息

- **包名**：`setting`
- **导入路径**：`github.com/ongridio/ongrid/internal/manager/server/setting`
- **路由前缀**：`/v1/system-settings`（由 `cmd/ongrid` 挂载，auth 中间件由上游注入）
- **文件定位**：HTTP 适配层（薄 handler —— auth + JSON decode + delegate to biz Service）

## 3. 关键类型与接口

### SettingService —— 窄接口

```go
type SettingService interface {
    Get(ctx context.Context, category, key string) (string, bool, error)
    Set(ctx context.Context, category, key, value string, sensitive bool) error
    List(ctx context.Context, category string) ([]bizsetting.SettingDTO, error)
    Delete(ctx context.Context, category, key string) error
}
```

由 `*biz/setting.Service` 满足。`Get` 返 (value, found, err)——found 区分"不存在"与"空值"。

### Handler

```go
type Handler struct {
    svc SettingService
}
```

### DTO

```go
type listResp struct {
    Items []bizsetting.SettingDTO `json:"items"`
    Total int                     `json:"total"`
}

type putReq struct {
    Value     string `json:"value"`
    Sensitive *bool  `json:"sensitive,omitempty"`
}
```

`Sensitive *bool` 用指针区分"未传"（自动检测）与"显式 false"。

### caller —— 本地 caller 类型

```go
type caller struct {
    UserID uint64
    Role   string
}
```

## 4. 关键函数与流程

### Register —— 路由表

```go
func (h *Handler) Register(r chi.Router)
```

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| GET | `/v1/system-settings` | any auth | 列设置（敏感值 mask），可选 `?category=` |
| PUT | `/v1/system-settings/{category}/{key}` | admin | 写设置（含审计） |
| DELETE | `/v1/system-settings/{category}/{key}` | admin | 删设置（含审计） |
| GET | `/v1/system-settings/{category}/{key}/reveal` | admin | 返明文值 |

### list

```go
func (h *Handler) list(w http.ResponseWriter, r *http.Request)
```

`callerFromRequest` 存在性检查 → `svc.List(ctx, category)` → 200 + `{items, total}`。任何已认证用户可读，敏感值在 biz 层 mask。

### put —— 写设置 + 审计

```go
func (h *Handler) put(w http.ResponseWriter, r *http.Request)
```

1. `requireAdmin`
2. 取 `category` / `key` URL 参数（空则 400）
3. `json.Decode(&putReq)`
4. **sensitive 决策**：
   - `req.Sensitive != nil` → 用显式值
   - 否则 → `isSensitiveKey(category, key)` 自动检测
5. `svc.Set(ctx, category, key, value, sensitive)`
6. **审计**：`auditmw.SetAuditEvent` 写 `ActionSettingUpdate`，payload 含 `value_hint`（sensitive 时前 4 字符 + `…`，否则截 64 字符）
7. **返更新后的 row**：`svc.List(ctx, category)` 再遍历找 key，让 UI 无需 re-list 整个 category

### reveal —— 明文读取

```go
func (h *Handler) reveal(w http.ResponseWriter, r *http.Request)
```

admin-only。`svc.Get(ctx, category, key)` → found=false 返 404 → 200 + `{value: v}`。**只返 value 字段**，不返 row（让 masked DTO 仍是 authoritative record）。

### delete

`requireAdmin` + `svc.Delete` + 审计（`ActionSettingDelete`） → 204 No Content。

### isSensitiveKey —— 自动 mask 策略

```go
func isSensitiveKey(_ string, key string) bool
```

key 后缀为 `_api_key` / `_secret` / `_token` / `_password` 之一则返 true。Operator 可通过 `req.Sensitive` 显式覆盖。

### helpers

- `callerFromRequest(r)` —— 从 tenantctx 构造 caller
- `requireAdmin(w, r)` —— caller 存在 + `Role == "admin"`
- `hasSuffix(s, suf)` —— 手写后缀匹配（避免 strings 包 import？）
- `writeJSON` / `writeErr` / `errCode` / `errSlug` —— 标准 errs 映射

## 5. 依赖关系

**外部**：
- `chi` —— URL 参数
- `net/http`、`encoding/json`、`errors`、`context`

**内部**：
- `internal/manager/biz/audit`（Event 类型）
- `internal/manager/biz/setting`（SettingService + SettingDTO）
- `internal/manager/model/audit`（Action/Resource/Status 常量）
- `internal/manager/server/middleware`（auditmw.SetAuditEvent）
- `internal/pkg/errs`
- `internal/pkg/tenantctx`

## 6. 并发与资源管理

- **无共享可变状态**：`Handler.svc` 启动时装配
- **请求级隔离**：每请求独立 ctx
- **`put` 后再 `List`**：写后读，确保返更新后的 masked row——有微小 race（其它请求改了同 category），但通常无害
- **`writeJSON` swallow encode 错误**：响应已开始无法回传

## 7. 设计模式与亮点

1. **sensitive 自动检测**：`isSensitiveKey` 按后缀自动判断，UI 无需每次显式 opt-in——降低误传明文风险
2. **`Sensitive *bool` 区分未传**：指针类型让"未传"（自动检测）与"显式 false"可区分
3. **`reveal` 独立端点**：admin 可看明文（eye-toggle），但不影响 list 的 mask 行为——双轨设计
4. **`value_hint` 审计**：sensitive 时审计 payload 含前 4 字符 + `…`，便于追溯但不泄露完整 secret
5. **put 返更新后的 row**：写后再 List 找 key，让 UI 单 cell 更新无需 re-list 整个 category
6. **`reveal` 只返 value**：不返 row，让 masked DTO 仍是 authoritative record（is_sensitive / updated_at）
7. **`hasSuffix` 手写**：避免 `strings.HasSuffix` import——但实际 `strings` 已被其它子域用，本文件可能是历史遗留
8. **审计覆盖 put + delete**：写/删都写审计，敏感配置变更可追溯
9. **list 任何已认证用户可读**：但敏感值在 biz 层 mask，确保 non-admin 看不到明文

## 8. 注意事项

1. **`reveal` 返明文**：admin 能看到完整 secret；如需更严格可移除此端点，强制 shell 访问 DB
2. **`value_hint` 前 4 字符**：可能泄露 secret 前缀；如需更安全可改为只记 length
3. **`put` 后再 List 有 race**：其它请求改了同 category 会导致返错 row；低概率但存在
4. **`isSensitiveKey` 仅按后缀**：前缀匹配（如 `password_`）或其它命名约定不会自动检测，需显式 `req.Sensitive`
5. **`hasSuffix` 手写**：与 `strings.HasSuffix` 等价，建议统一用标准库
6. **`roleAdmin` 字符串硬编码**：`c.Role != "admin"` 直接比较，避免跨 BC import
7. **`caller` 本地类型**：与 biz.Caller 平行，如 biz 层变化需同步
8. **`errSlug` 用 `invalid-argument`**：与其它子域的 `invalid` 不同（多了 `-argument`）；前端兼容需注意
9. **`delete` 返 204**：与 RESTful 约定一致；`put` 返 200 + row
10. **无 body 大小限制**：`put` 的 `json.Decode(r.Body)` 未套 `MaxBytesReader`，理论上可传超大 body；setting value 通常不大但严格防御应加上
11. **`category` / `key` URL 参数空检查**：`if category == "" || key == ""` 返 400，避免 chi 路由匹配空段
