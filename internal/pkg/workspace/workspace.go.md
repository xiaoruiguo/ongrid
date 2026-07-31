# `workspace.go` 技术实现文档

> 源文件：`internal/pkg/workspace/workspace.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/workspace`

## 1. 概述

本文件实现 agent 工作目录管理（HLD-019 Agent Workspace）：在持久化 root 下解析 agent 工作目录。设计刻意 skill-agnostic — skill 是无状态能力，其文件/产物属于 agent 工作上下文。两类目录：session scratch（`sessions/<session-id>/`，单会话持久，跨 turn 共享，可归档回收）与 named project（`projects/<name>/`，显式创建长期，P2 未实现）。skill 在 agent workspace 内运行（cwd = session dir）而非自创 `/var/lib/ongrid/iac`。

## 2. 包信息

- **包名**：`workspace`
- **所属模块**：`internal/pkg/`
- **依赖方向**：被 agent 执行器 / skill runner 调用；仅依赖标准库 `os`、`path/filepath`、`fmt`

## 3. 关键类型与接口

```go
type Manager struct {
    root string // 空 = 禁用 workspace
}
```

无导出接口；导出函数 `New` / `Root` / `Session` 是公共 API。

## 4. 关键函数与流程

### `New`
- **签名**：`func New(root string) *Manager`
- **职责**：构造 Manager；空 root 禁用 workspace
- **流程**：返回 `&Manager{root: root}`

### `Manager.Root`
- **签名**：`func (m *Manager) Root() string`
- **职责**：返回配置的 root（"" = 禁用）

### `Manager.Session`
- **签名**：`func (m *Manager) Session(sessionID string) (string, error)`
- **职责**：返回会话持久工作目录，按需创建
- **流程**：
  1. `sanitizeID(sessionID)` 清洗为文件系统安全 slug
  2. root 空 或 id 空 → 返回 `"", nil`（caller 回退临时目录）
  3. `dir = filepath.Join(root, "sessions", id)`
  4. `os.MkdirAll(dir, 0o750)` — 注意 0o750 权限
  5. 返回 dir
- **错误处理**：mkdir 失败用 `%w` 包装 `"workspace: mkdir session %q: %w"`

### `sanitizeID`
- **签名**：`func sanitizeID(s string) string`
- **职责**：仅保留 `[A-Za-z0-9._-]`，防止 session id 通过路径分隔符或 `..` 逃逸 workspace root
- **流程**：遍历 rune，合法字符 append；结果为 `""` / `"."` / `".."` 返回 `""`
- **错误处理**：无错误返回，非法输入返回空字符串

## 5. 依赖关系

- **内部包**：无
- **外部库**：无
- **被调用方**：agent 执行器 / skill runner（提供 cwd 给 skill）

## 6. 并发与资源管理

- **`Manager` 字段只读**：root 在构造时设定，安全并发读
- **`os.MkdirAll` 幂等**：并发调用同一 session 目录安全（MkdirAll 检查存在性）
- **无锁**：Manager 无可变状态；Session 是纯函数式（除 mkdir 副作用）

## 7. 设计模式与亮点

- **skill-agnostic 设计**：注释明示"skills are stateless capabilities, while their files/products belong to the agent's working context"。workspace 是 agent 概念，skill 接收 cwd 而非自管目录
- **禁用降级**：root 空 时 Session 返回 `"", nil`，caller 回退临时目录，保持今天的行为。让 workspace 是 opt-in 而非强制
- **`sanitizeID` 防路径逃逸**：白名单字符 + 拒绝 `.` / `..`，session id 永远无法通过 `..` 逃逸 root
- **0o750 权限**：owner+group rwx，其他无权限；适度保护 session 内容
- **session 跨 turn 持久**：注释明示"persistent across turns within the session (so a tool can write a file in one command and read it back in the next)"，让 skill 状态跨 turn 共享
- **named project 预留**：注释提到 `projects/<name>/` 是 P2 未实现；当前仅 session scratch

## 8. 注意事项

- **`sanitizeID` 改变 id**：若 session id 含非法字符（如 `/`、空格、中文），sanitize 后可能与其他 id 碰撞；caller 应使用文件系统安全的 id 生成器
- **0o750 权限**：若 agent 进程以非 root 运行且 session 目录需其他 uid 访问需调整
- **无 session 回收 API**：注释提到"reclaimable when the session is archived"，但本文件未实现归档/清理；需 caller 自管生命周期
- **named project 未实现**：当前仅 session；P2 实现 `projects/<name>/` 时需扩展 API
- **root 空 = 禁用**：caller 必须检查返回的 dir 是否为空，否则 mkdir 空路径会失败
- **并发 mkdir 同一 session**：`os.MkdirAll` 幂等安全，但若 caller 期望"首次创建"信号需自行加锁
- **不处理 symlink**：sanitizeID 防止 id 逃逸，但 root 内的 symlink 可能让文件写到 root 外；若需严格隔离需 caller 自行检查
