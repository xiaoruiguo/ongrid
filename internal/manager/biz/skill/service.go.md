# service.go 技术实现文档

## 1. 概述

`service.go` 是 `skill` 包的核心——manager 侧技能编排层。它将共享 registry（`internal/skill`）转化为面向操作员的能力：

- List/Get 提供 HTTP 友好的元数据
- Execute 通过 cloud→edge `MethodExecuteSkill` RPC 派发
- 权限闸门（Class × caller role 策略）
- 审计日志

关键约束：**manager 永远不在进程内执行技能体**（除了 `ScopeManager` 类型的技能，如 `web_search` 与 subprocess runtime）。它只派发并记录 round-trip。

## 2. 包信息

- 包名：`skill`
- 路径：`internal/manager/biz/skill/service.go`
- 导入依赖：`context` / `encoding/json` / `errors` / `fmt` / `log/slog` / `time`、`internal/pkg/errs`、`internal/pkg/tunnel`、`internal/skill`（别名 `skillcore`）

## 3. 关键类型与接口

### `Caller`

```go
type Caller struct {
    UserID uint64
    Role   string // "admin" | "user"
}
```

镜像 `service/alert.Caller` 的窄 auth 上下文，避免与 iam 包强耦合。

### `EdgeCaller`

```go
type EdgeCaller interface {
    Call(ctx context.Context, edgeID uint64, method string, body []byte) ([]byte, error)
}
```

cloud→edge RPC 的窄接口。`frontierbound.Client` 通过其 `Call` 方法满足此接口。

### `AuditSink`

```go
type AuditSink interface {
    Record(ctx context.Context, ev AuditEvent) error
}
```

nil sink 禁用审计（测试用）。具体实现 `GormAuditSink` 在 `audit.go`。

### `AuditEvent`

```go
type AuditEvent struct {
    SkillKey   string
    EdgeID     uint64
    CallerID   uint64
    CallerRole string
    Class      skillcore.Class
    Params     json.RawMessage
    Result     json.RawMessage
    Error      string
    StartedAt  time.Time
    FinishedAt time.Time
}
```

审计行的 DTO。`audit.go` 的 `GormAuditSink.Record` 将其映射到 `SkillExecution` 表。

### `Service`

```go
type Service struct {
    caller EdgeCaller
    audit  AuditSink
    log    *slog.Logger
    extra  func() []SkillSummary
}
```

- `caller`：edge RPC 客户端
- `audit`：可 nil
- `extra`：可选的额外 catalog 来源（chatruntime SKILL.md 技能，built-in + marketplace-installed），在 `main.go` 装配（HLD-017）。这些技能位于独立 registry，否则在 catalog 中不可见

### `SkillSummary` / `SkillParamDef`

DTO，含 `Key` / `Name` / `Description` / `Class` / `Scope` / `Category` / `Params` / `ResultPreview` / `Source` / `InventoryOnly`。`Source` 标记来源（`""` / `"builtin"` / `"marketplace"` / `"git"` / `"tarball"` / `"local"`），让 UI 能用不同 badge 区分。`InventoryOnly` 标记"仅列示、无自动可渲染表单"的技能——其 schema 仅存于 raw JSON Schema，UI 应指向"通过 AI chat 使用"。

## 4. 关键函数与流程

### `New` / `WithExtraSkills`

```go
func New(caller EdgeCaller, audit AuditSink, log *slog.Logger) *Service
func (s *Service) WithExtraSkills(fn func() []SkillSummary) *Service
```

`WithExtraSkills` 链式装配额外 catalog 来源。

### `List`

```go
func (s *Service) List(_ context.Context, _ Caller, category string) []SkillSummary
```

遍历 `skillcore.All()`，按 `category` 过滤，转 `SkillSummary`。随后合并 `s.extra()` 提供的 chatruntime SKILL.md 技能，用 `seen` map 去重（key 已存在则跳过）。

### `Get`

```go
func (s *Service) Get(_ context.Context, _ Caller, key string) (*SkillSummary, error)
```

仅查 `skillcore.Get(key)`，未命中返回 `errs.ErrNotFound`（HTTP 层映射 404）。注意：`Get` 不查 `extra`，因为 `extra` 提供的是 chatruntime 技能，其详情查询走另一条路径。

### `Execute` —— 核心派发流程

```go
func (s *Service) Execute(ctx context.Context, caller Caller, in ExecuteInput) (*ExecuteOutput, error)
```

流程：

1. **参数校验**：`in.Key` 必填；`skillcore.Get(key)` 未命中返回 `ErrNotFound`
2. **权限闸门**：`authorize(meta.EffectiveClass(), caller.Role)`
3. **Scope 路由**：
   - `ScopeHost`（默认）：要求 `in.EdgeID != 0`，通过 `s.caller.Call` 派发 `tunnel.MethodExecuteSkill` RPC 到 edge
   - `ScopeManager`：进程内执行 `exec.Execute(ctx, in.Params)`，用于 `web_search` 与 subprocess runtime
4. **结果解码**：RPC 成功后 `json.Unmarshal` 响应体，提取 `result` / `error`
5. **审计记录**：无论成功失败均调用 `s.audit.Record`，失败降级为 `s.log.Warn`
6. **错误传播**：RPC 失败用 `%w` 包装；技能内部错误填入 `ExecuteOutput.Error` 返回

### `authorize` —— 权限策略

```go
func authorize(class skillcore.Class, role string) error
```

PR-G1 最小策略：

- `ClassSafe`：任何认证调用者（admin/user/viewer）
- `ClassMutating`：admin/user 允许；viewer 拒绝；其他拒绝
- `ClassDangerous`：**全员拒绝**，直到 PR-G4 SOP 双签名落地

### `toSummary`

```go
func toSummary(e skillcore.Executor) SkillSummary
```

`skillcore.Executor` → `SkillSummary` 转换。关键逻辑：

- `inventoryOnly = hasRawSchema && len(m.Params) == 0`：技能通过 `inventory_bridge` 进入，schema 仅存于 raw JSON Schema，ParamSchema 为空。仍列示以暴露"agent 有何能力"，但 UI 隐藏执行按钮并指向 chat。

## 5. 依赖关系

- **`skillcore`**（`internal/skill`）：技能 registry，提供 `All()` / `Get(key)` / `Executor` / `Metadata` / `Class` / `Scope`
- **`EdgeCaller`**：cloud→edge RPC，`frontierbound.Client` 实现
- **`AuditSink`**：审计持久化，`GormAuditSink` 实现
- **`tunnel.MethodExecuteSkill`**：RPC method 名常量
- **`errs`**：`ErrInvalid` / `ErrNotFound` / `ErrForbidden`
- **`extra` 回调**：chatruntime SkillRegistry 的 SKILL.md 技能来源

## 6. 并发与资源管理

- `Service` 构造后字段不变（`caller` / `audit` / `log` / `extra` 均只读），并发安全
- 无共享可变状态、无 goroutine、无 IO 持有
- `Execute` 是同步阻塞调用，受 `ctx` 控制
- `extra` 回调在每次 `List` 时调用，要求回调自身并发安全（chatruntime SkillRegistry 内部已加锁）

## 7. 设计模式与亮点

### Scope 双路由

`ScopeHost` vs `ScopeManager` 的双路由是关键设计：

- `ScopeHost`（默认）：技能体在 edge agent 上运行，manager 仅派发。这是绝大多数技能的路径——文件操作、进程查询等必须在目标主机执行
- `ScopeManager`：技能体在 manager 进程内运行。用于 `web_search`（manager 才有外网访问）与 subprocess skill runtime。注释明确"manager never executes skill bodies in-process; it only dispatches and records"——`ScopeManager` 是这条规则的例外，且例外是刻意的

### 错误结构化返回而非抛出

技能内部错误（`exec.Execute` 失败、RPC 内部错误）填入 `ExecuteOutput.Error` 字段返回，而非作为 Go error 抛出。这让调用方（HTTP handler / AI tool registry）能拿到结构化输出，区分"RPC 失败"与"技能执行失败"。只有 RPC 本身失败才用 `%w` 包装为 Go error。

### 权限闸门的 fail-closed

`ClassDangerous` 全员拒绝，直到 PR-G4 落地 SOP 签名。这是 fail-closed 设计——宁可拒绝所有 dangerous 技能调用，也不在签名机制未就绪时放行。`authorize` 末尾的 `return errors.New("skill: unknown class")` 也是 fail-closed：未知 class 拒绝。

### `InventoryOnly` 的 UI 语义

技能通过 `inventory_bridge` 进入时，schema 可能仅存于 raw JSON Schema，无 `ParamSchema`。`toSummary` 检测 `hasRawSchema && len(m.Params) == 0` 标记 `InventoryOnly`。UI 据此隐藏执行按钮，指向 chat——因为 LLM 是预期调用方，手填空 `{}` 会在 inner BaseTool 的 required-field 校验上失败。

### `extra` 回调的延迟装配

`WithExtraSkills` 接受 `func() []SkillSummary` 而非 `[]SkillSummary`。这让 chatruntime SkillRegistry 在 `main.go` 装配时能传入一个"按需求值"的闭包，避免在 `Service.New` 时强制要求 chatruntime 已就绪——解耦了启动顺序。

### 审计的失败降级

`audit.Record` 失败时 `s.log.Warn` 而非返回 error。这是合规与可用性的折衷——审计是事后追溯，不应让一次 DB 抖动毁掉一次成功的技能调用。但 warn 日志保留了审计缺失的可追溯性。

## 8. 注意事项

- **`Get` 不查 `extra`**：`Get` 仅查 `skillcore.Get`，不查 chatruntime SKILL.md 技能。若 SPA 通过 `List` 拿到 `extra` 来源的 key 后调用 `Get`，会得到 404。SPA 需用 `List` 的结果直接渲染详情，或 chatruntime 需另提供详情端点
- **`Execute` 不支持 `extra` 技能**：`Execute` 仅查 `skillcore.Get`，chatruntime SKILL.md 技能无法通过本 `Service.Execute` 调用——它们走 chatruntime 自己的执行路径
- **`ClassDangerous` 全员拒绝**：当前任何 dangerous 技能调用都会失败。部署时需确认无 dangerous 技能被依赖，或加速 PR-G4 落地
- **`ScopeHost` 强制 `EdgeID != 0`**：`ScopeHost` 技能调用未传 `EdgeID` 会返回 `ErrInvalid`。AI tool registry 调用时需保证传入 edge 上下文
- **审计的时区**：`startedAt` / `finishedAt` 用 `time.Now().UTC()`，统一 UTC
- **`extra` 回调的并发安全**：`List` 每次调用 `s.extra()`，回调实现需并发安全（chatruntime SkillRegistry 内部已有锁）
- **`toSummary` 的 `Default any`**：`SkillParamDef.Default` 用 `any` 类型，让 UI 能渲染 numeric/string/bool 默认值——这是少数允许 `any` 的边界，符合"不可避免时就近注释"的规范
