# service.go 技术实现文档

## 1. 概述

`service.go` 是 `setting` 包的核心——manager 侧 `system_settings` 键值存储的应用服务。它位于 HTTP handler / 内部调用方（如 LLM resolver）与 data repo 之间，提供两项关键增值：

1. **进程内缓存**：让热路径调用方（如 `pkg/llm` 的每次 Chat）不必每次 round-trip DB
2. **敏感字段脱敏**：list 端点自动 mask 敏感值；cleartext 仅在显式 `Get` 时返回（供 LLM resolver 使用）

## 2. 包信息

- 包名：`setting`
- 路径：`internal/manager/biz/setting/service.go`
- 包注释明确：当前是 flat / single-tenant。多租户落地后 cache key 需加 `org_id` 前缀
- 导入依赖：`context` / `errors` / `fmt` / `log/slog` / `strings` / `sync`、`model/setting`、`internal/pkg/errs`

## 3. 关键类型与接口

### `Repo`（持久化契约）

```go
type Repo interface {
    Get(ctx context.Context, category, key string) (*model.Setting, error)
    Set(ctx context.Context, category, key, value string, sensitive bool) (*model.Setting, error)
    SetBatch(ctx context.Context, settings []model.Setting) error
    List(ctx context.Context, category string) ([]*model.Setting, error)
    Delete(ctx context.Context, category, key string) error
}
```

具体实现位于 `data/setting/store`。

### `SettingDTO`（wire shape）

```go
type SettingDTO struct {
    Category  string `json:"category"`
    Key       string `json:"key"`
    Value     string `json:"value"`
    Sensitive bool   `json:"sensitive"`
    UpdatedAt string `json:"updated_at"`
}
```

`Value` 在序列化前已 mask。`UpdatedAt` 统一为 UTC ISO 8601。

### `Service`

```go
type Service struct {
    repo Repo
    log  *slog.Logger
    mu    sync.RWMutex
    cache map[string]string // key = "category|key"
}
```

并发安全。`system_settings` 表预期生命周期 < 100 行，因此 map-of-map + RWMutex 比 `sync.Map` 在典型工作负载下更优。

## 4. 关键函数与流程

### `Get` —— `(value, found, error)` 三元组

```go
func (s *Service) Get(ctx context.Context, category, key string) (string, bool, error)
```

关键语义：

- `found=false` 表示 DB 中无此行
- **存在的空值行仍返回 `found=true`**——调用方可借此用空值作为显式覆盖（如空 LLM API key = 显式禁用 env 配置的 provider）
- 缓存命中短路 DB 调用
- `errs.ErrNotFound` 折叠为 `found=false`，不当作错误

### `Set` —— upsert + 失效缓存

```go
func (s *Service) Set(ctx context.Context, category, key, value string, sensitive bool) error
```

写库后 `delete(s.cache, ck)` 失效缓存条目。下一次 `Get` 重新从 DB 加载，跨 goroutine 拾取新值。日志记录 `category` / `key` / `sensitive`（不记 value）。

### `SetBatch` —— 原子批量 upsert

```go
func (s *Service) SetBatch(ctx context.Context, settings []model.Setting) error
```

事务提交后才失效这批 cache 条目。这保证了一个经过验证的 LLM provider tuple 不会因某个字段写失败而变成"半持久化混合态"——要么全写、要么全不写。失败时用 `%w` 包装错误。

### `SetIfAbsent` —— 启动期 seed

仅当行不存在时写入。用于启动时 seed env 派生值而不覆盖管理员之前的编辑。空值直接 skip。

### `List` —— 脱敏返回

```go
func (s *Service) List(ctx context.Context, category string) ([]SettingDTO, error)
```

通过 `maskValue(r.Value, r.Sensitive)` 应用 mask 策略：

- 非 sensitive → 原值
- 空 sensitive 值 → 空串
- ≤8 字符 → 全 `*`
- >8 字符 → `首4 + "***" + 末4`

### `Delete` + 缓存失效

```go
func (s *Service) Delete(ctx context.Context, category, key string) error
```

### `InvalidateAll` —— 全量清缓存

当前未使用，但导出作为 escape hatch——管理员手动改 DB 后若发现 stale 值可调用。

### `maskValue`（私有）

脱敏策略实现。短值全遮、长值保首末各 4 字符。

## 5. 依赖关系

- **`Repo` 接口**：唯一持久化依赖，具体实现在 data 层
- **`model.Setting`**：领域类型，含 `Category` / `Key` / `Value` / `Sensitive` / `UpdatedAt`
- **`errs`**：`ErrInvalid` / `ErrNotFound`
- **被依赖方**：
  - `LLMSettingsResolver` / `LLMConfigProbe`（LLM 配置）
  - `PromResolver`（Prometheus auth + URL）
  - `LokiResolver` / `TempoResolver` / `WebSearchResolver`
  - `agent.go` 的 `AgentWriteEnabled` typed accessor
  - HTTP handler 层

## 6. 并发与资源管理

### RWMutex 策略

- `Get`：先 `RLock` 查 cache；命中即返回。未命中走 DB，写回 cache 时升级为 `Lock`
- `Set` / `SetBatch` / `Delete`：`Lock` 失效对应 cache 条目
- `InvalidateAll`：`Lock` 重建整个 map

### 事务边界

`SetBatch` 的关键：先 `repo.SetBatch`（事务），提交后再失效 cache。这避免了"cache 已失效但事务回滚"的窗口期——若事务失败，cache 仍持有旧值，下次 `Get` 命中旧值是正确的。

### 无 goroutine

`Service` 不启动任何 goroutine，所有操作同步。缓存 TTL 由调用方（如 round-tripper 的 5s TTL）在更上层管理。

## 7. 设计模式与亮点

### found vs error 显式区分

`Get` 返回 `(value, found, error)` 三元组，明确区分"行不存在"与"DB 出错"。这是为 LLM resolver 等调用方设计的——它们需要用"存在的空值行"作为"显式禁用"信号，而单纯的 `("", nil)` 无法区分"空值"与"行缺失"。

### 短路缓存 + 读时回填

经典的 cache-aside 模式。读未命中时回填，使后续读命中。写时失效（write-through invalidation）而非写时更新——避免并发写的 cache 竞态。

### SetBatch 的原子性契约

注释明确："`only after the repository transaction commits`"。这是为 LLM provider tuple 设计的——`api_key` / `base_url` / `default_model` / `models` 必须四元组一致持久化，否则可能出现"有 key 无 model"的中间态。

### mask 策略的"短值全遮"分支

`len(v) <= 8` 时全遮，避免 `ab***ef` 这种"几乎露出全部"的脱敏。8 字符是经验阈值——典型 API key 远长于此。

### 启动 seed 的 SetIfAbsent

`SetIfAbsent` 让 env 派生值在首次启动时写入 DB，但**不覆盖**管理员后续编辑。这是 env-seeded 部署的关键——env 变更后不会"反向覆盖"管理员在 UI 上的修改。

### 日志不记 value

`Set` 的日志只记 `category` / `key` / `sensitive`，绝不记 `value`。即使 `sensitive=false`，也避免把任何配置值写入日志——defense in depth。

## 8. 注意事项

- **多租户待落地**：cache key 当前是 `(category, key)`，多租户后需加 `org_id` 前缀
- **`InvalidateAll` 未使用**：保留为 escape hatch，若管理员反馈 stale 值，可考虑暴露为 admin API
- **`maskValue` 不抗暴力**：8 字符以下全遮，但 9 字符的 token 会暴露首末 4——对极短 token 仍可能泄露大部分内容；当前未遇到这种场景
- **`SetBatch` 假设 repo 实现真事务**：若 repo 的 `SetBatch` 不是原子事务，本服务的"原子性契约"会失效——data 层实现必须保证
- **cache 无 TTL**：cache 条目无过期时间，仅靠写时失效。若 DB 被外部直接修改（绕过本服务），cache 不会自动更新——这是 `InvalidateAll` 存在的原因
