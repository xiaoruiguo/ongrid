# `user_agent.go` 技术实现文档

> 源文件：`internal/manager/service/aiops/user_agent.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/service/aiops`

## 1. 概述

本文件是 Phase-3 用户自定义 persona（User Agent）的应用服务。每个用户创建的 agent 既持久化到 `user_agents` 表，又实时镜像到 `chatruntime.AgentRegistry`，使增删改即时生效（无需 manager 重启）。核心红线：(1) 名称形态严格（`[a-z][a-z0-9_-]{0,63}`），禁止与内置/磁盘 persona 冲突；(2) viewer 拒绝、非 owner 拒绝（IDOR fix），admin 可绕过；(3) DB 写优先，registry 镜像失败仅 Warn —— 下次重启从 DB hydrate 保证最终一致。

## 2. 包信息

- **包名**：`aiops`（与 `service.go` 同包）
- **所属模块**：`internal/manager/service/aiops`
- **依赖方向**：被 HTTP handler 调用；依赖 `biz/aiops/chatruntime`、`model/aiops`、`internal/pkg/errs`

## 3. 关键类型与接口

```go
type UserAgentRepo interface {
    List(ctx) ([]*model.UserAgent, error)
    GetByName(ctx, name string) (*model.UserAgent, error)
    Create(ctx, ua *model.UserAgent) error
    Update(ctx, name string, ua *model.UserAgent) error
    Delete(ctx, name string) error
}

type UserAgentService struct {
    repo     UserAgentRepo
    registry *chatruntime.AgentRegistry  // 可 nil（legacy kernel 时）
    log      *slog.Logger
}

type CreateUserAgentInput struct {
    UserID           uint64
    Name             string
    Description      string
    WhenToUse        string
    SystemPrompt     string
    CriticalReminder string
    AllowedTools     []string
    DisallowedTools  []string
    PermissionMode   string
    Model            string
    MaxTurns         int
}

type UpdateUserAgentInput struct { /* 同上但无 Name / UserID */ }

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var reservedSourceTags = map[string]bool{"builtin": true, "disk": true}
```

辅助：`IsInvalid(err)` 判 `errs.ErrInvalid`，仅供测试用。

## 4. 关键函数与流程

### `NewUserAgentService(repo, registry, log)`

- log nil → `slog.Default()`；registry 可 nil（legacy 模式 graph 未构建时 CRUD 仅触 DB，下次 graph 重启 hydrate）。

### `HydrateRegistry(ctx)`

- **职责**：boot 时把所有持久化的 user agent 灌进 live registry。
- **流程**：registry nil 直接返回；`repo.List` 失败 `%w`；逐行 `userAgentRowToChatruntimeAgent` 后 `registry.Replace(ag)`；Info 日志记录 count。
- **错误处理**：list 失败 fail-fast；单行转换无错误返回（纯内存映射）。

### `List(ctx)`

- 直接委托 `repo.List`。

### `Create(ctx, in)`

- **流程**：
  1. TrimSpace name；`!nameRE.MatchString` → `ErrInvalid`（提示正则）。
  2. `reservedSourceTags[name]` → `ErrInvalid`（保留名）。
  3. TrimSpace description / system_prompt，空 → `ErrInvalid`。
  4. **冲突检查 1**：`registry.ByName(name)` 命中且 `Source != "user"` → `ErrInvalid`（禁止 shadow 内置/磁盘 persona）。
  5. **冲突检查 2**：`repo.GetByName(ctx, name)` 命中 → `ErrInvalid`（同名 user agent 已存在）。
  6. `marshalStringSlice(AllowedTools / DisallowedTools)` —— 空切片返回空字符串（不是 `"[]"`）。
  7. 构造 `model.UserAgent` row（now UTC）；`repo.Create` 失败 `%w`。
  8. registry 非 nil → `registry.Add(...)`。
- **错误处理**：DB 失败直接返回；registry.Add 失败不拦截（注释明示"下次重启 hydrate 保证最终一致"——但代码实际未对 Add 做错误处理，Add 无返回值）。

### `Update(ctx, caller, name, in)`

- **授权**：`caller.IsViewer()` → `ErrForbidden`；`!nameRE.MatchString(name)` → `ErrNotFound`（隐藏存在性）；description / system_prompt 空 → `ErrInvalid`。
- **流程**：`repo.GetByName` 取 existing；非 admin 且 `existing.UserID != caller.UserID` → `ErrForbidden`（IDOR fix）。
- 构造 `updated`（保留 `existing.ID` / `existing.UserID` / `existing.CreatedAt`，刷新 `UpdatedAt`）；`repo.Update` 失败返回；registry 非 nil → `registry.Replace(updated)`。
- **错误处理**：name 形态不合法直接 `ErrNotFound` 而非 `ErrInvalid`，避免泄露存在性。

### `Delete(ctx, caller, name)`

- **授权**：同 Update。
- **流程**：`repo.GetByName`；`repo.Delete`；registry 非 nil → `registry.Remove(name)`。
- **错误处理**：磁盘 persona 无法通过此路径删除（它们是文件，重启后仍在）。

### 转换 / 辅助

- **`userAgentRowToChatruntimeAgent(row)`**：nil 安全；`unmarshalStringSlice` 解析 AllowedToolsJSON / DisallowedToolsJSON；构造 `chatruntime.Agent`，`Source: "user"`。
- **`marshalStringSlice(s)`**：空切片返回 `("", nil)`；否则 `json.Marshal`。
- **`unmarshalStringSlice(s)`**：空字符串返回 nil；解析失败返回 nil（容错）。

## 5. 依赖关系

- **内部包**：`biz/aiops/chatruntime`、`model/aiops`、`internal/pkg/errs`
- **外部库**：`log/slog`、`regexp`、`encoding/json`、`errors`、`time`、`strings`
- **被调用方**：HTTP handler（`/v1/agents/custom` 系列）；`cmd/ongrid/main.go` 在 boot 后调 `HydrateRegistry`

## 6. 并发与资源管理

- **无锁**：本 service 自身无共享可变状态；并发安全依赖 `chatruntime.AgentRegistry` 内部锁 + DB 事务隔离。
- **registry 可 nil**：legacy 模式或 graph 未构建时降级为纯 DB CRUD。
- **无 ctx 透传特殊处理**：所有 IO 函数首参 `context.Context`，遵循 gospec 规范。

## 7. 设计模式与亮点

- **DB-first + registry 镜像**：先写 DB（source of truth），再 mirror 到 registry；部分失败仅 Warn，下次重启 hydrate 保证最终一致 —— 比双写事务简单且足够可靠。
- **名称冲突双层防御**：reservedSourceTags（保留字）+ registry.ByName（运行时内置 persona）+ repo.GetByName（同名 user agent）。
- **内置 persona 不可被 user shadow**：`existing.Source != "user"` 拒绝；磁盘文件 reload 后会覆盖 user 同名 agent，提前拒绝避免未来 race 让用户惊讶。
- **IDOR fix**：Update / Delete 强制 owner 校验（注释明示"之前任意登录用户可覆盖他人 agent"）。
- **nameRE 与 skill key 形态一致**：避免冲突 path / UI grammar。
- **`Update` 的 name 形态不合法返回 `ErrNotFound` 而非 `ErrInvalid`**：隐藏存在性，与 GetSession 同思路。
- **`HydrateRegistry` 用 `Replace` 而非 `Add`**：boot 时若 registry 已有同名磁盘 persona，user 版本应该不覆盖（实际 Replace 会覆盖 —— 见注意事项）。

## 8. 注意事项

- **registry nil 时 CRUD 仅触 DB**：legacy kernel 部署下，user agent 的增删改不会即时被 chat session 感知，需重启。
- **registry.Add 无返回值**：当前实现下 mirror 失败无法捕获；依赖下次重启 hydrate。
- **`HydrateRegistry` 用 `Replace`**：若磁盘 persona 与 user agent 同名，Replace 会覆盖磁盘版本 —— 但 `Create` 已禁止同名，所以 boot 时只在"磁盘 persona 后加" race 下才触发，注释提及该 race 由磁盘 reload 时 user 版本被覆盖解决。
- **`reservedSourceTags` 仅检查 name 字面值**：实际内置 persona 的 reserved 由 registry.ByName(.Source) 兜底。
- **Name 不可改**：Update 不接受 name 参数变化；要改 name 需删后重建。
- **`MaxTurns` 为 int**：0 表示不限制（runtime 层解释）。
- **AllowedToolsJSON 空切片存为空字符串而非 `[]`**：与 `marshalStringSlice` 行为一致；`unmarshalStringSlice` 双向兼容。
- **`IsInvalid` 仅测试用**：注释明示，公共 API 不应依赖此 helper。
