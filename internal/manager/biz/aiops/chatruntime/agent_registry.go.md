# `agent_registry.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/chatruntime/agent_registry.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/chatruntime`

## 1. 概述

本文件实现 `AgentRegistry` —— agent persona 的进程内注册表。`sync.RWMutex` 保护 agents + warnings 两个切片；Load/Reload 委托 `LoadAll` 统一加载（含 plugin container 递归）；All/Warnings/ByName 返回值拷贝防外部修改；Add/Replace/Remove 支持运行时增删（user-agent 编辑路径）；AddWarnings 合并 loader 警告。Reload 原子交换切片，in-flight coordinator 持有的快照不受影响。

## 2. 包信息

- **包名**：`chatruntime`
- **所属模块**：`internal/manager/biz/aiops/chatruntime`
- **依赖方向**：被 `runtime.go`/`worker.go`/`system_prompt.go` 调用；依赖 `load_all.go`（`LoadAll`/`LoadAllConfig`）、`types.go`（`Agent`/`LoadWarning`）

## 3. 关键类型与接口

```go
type AgentRegistry struct {
    mu       sync.RWMutex
    agents   []*Agent
    warnings []LoadWarning
}
```

## 4. 关键函数与流程

### `NewAgentRegistry`
- **签名**：`func NewAgentRegistry() *AgentRegistry`
- **职责**：返回空注册表

### `Load`
- **签名**：`func (r *AgentRegistry) Load(agentsRoot string) error`
- **职责**：从 agentsRoot 加载所有 agent persona（递归 + plugin container）
- **流程**：
  1. `LoadAll(LoadAllConfig{AgentsRoot: agentsRoot})` 统一加载
  2. 失败 → `%w: chatruntime: agent registry load`
  3. 加锁 → `r.agents = append([]*Agent(nil), res.Agents...)`（copy）→ `r.warnings = append(...)` → 解锁
- **注意**：agentsRoot 不存在不算错误（fresh install 无 agent 正常）

### `Reload`
- **签名**：`func (r *AgentRegistry) Reload(agentsRoot string, extras ...string) error`
- **职责**：热重载（marketplace Install/Uninstall 用）
- **流程**：
  1. `LoadAll(LoadAllConfig{AgentsRoot, ExtraAgentsRoots: extras})` 重新加载
  2. **原子性**：先在锁外构建 newAgents/newWarnings → 加锁 → 替换 → 解锁（O(1) 交换）
- **extras 用途**：保留 image-baked 内置 agent 通过 marketplace install reload 不丢失

### `All`
- **签名**：`func (r *AgentRegistry) All() []*Agent`
- **职责**：返回所有 agent 的拷贝切片（防外部修改）
- **流程**：RLock → make + copy → RUnlock

### `Warnings`
- **签名**：`func (r *AgentRegistry) Warnings() []LoadWarning`
- **职责**：返回所有警告的拷贝切片

### `ByName`
- **签名**：`func (r *AgentRegistry) ByName(name string) (*Agent, bool)`
- **职责**：按 name 精确查找 agent（coordinator spawn 时用）
- **流程**：RLock → 遍历 → name 匹配 → 返回（指针，非拷贝）
- **注意**：返回指针，调用方不应修改；如需修改应用 Replace

### `Add`
- **签名**：`func (r *AgentRegistry) Add(ag *Agent)`
- **职责**：追加单个 agent（内置 agent 用，如 general-purpose）
- **流程**：nil → skip；Lock → append → Unlock

### `AddAll`
- **签名**：`func (r *AgentRegistry) AddAll(agents []*Agent)`
- **职责**：批量追加（LoadAll 合并 plugin container 输出用）
- **流程**：遍历 → 逐个 Add（nil 跳过）

### `Replace`
- **签名**：`func (r *AgentRegistry) Replace(ag *Agent)`
- **职责**：upsert 语义替换同名 agent（user-agent 编辑路径用）
- **流程**：nil 或 name 空 → skip；Lock → 遍历找同名 → 替换 → 未找到 → append → Unlock

### `Remove`
- **签名**：`func (r *AgentRegistry) Remove(name string) bool`
- **职责**：删除指定 name 的 agent（user-agent 删除路径用）
- **流程**：name 空 → false；Lock → 遍历 → 找到 → 切片拼接删除 → 返回 true；未找到 → false → Unlock

### `AddWarnings`
- **签名**：`func (r *AgentRegistry) AddWarnings(ws []LoadWarning)`
- **职责**：追加 loader 警告（LoadAll 的警告合并到 registry）

## 5. 依赖关系

- **内部包**：无外部包依赖
- **包内依赖**：`load_all.go`（`LoadAll`、`LoadAllConfig`）、`types.go`（`Agent`、`LoadWarning`）
- **外部库**：标准库 `sync`、`fmt`

## 6. 并发与资源管理

- **`mu` sync.RWMutex**：保护 agents + warnings 两个切片
  - 读路径（All/Warnings/ByName）：RLock
  - 写路径（Load/Reload/Add/Replace/Remove/AddWarnings）：Lock
- **原子 Reload**：先在锁外构建新切片，再加锁替换，O(1) 交换；in-flight coordinator 持有的旧切片不受影响
- **值拷贝返回**：All/Warnings 返回 copy 切片，调用方可安全持有和修改
- **ByName 返回指针**：非拷贝，性能优先；调用方不应修改

## 7. 设计模式与亮点

- **RWMutex 读写分离**：读多写少场景（coordinator 频繁 ByName，Reload 偶发），RWMutex 提升并发度
- **原子 Reload**：锁外构建 + 锁内替换，最小化锁持有时间
- **值拷贝防泄漏**：All/Warnings 返回 copy，外部修改不影响 registry 内部状态
- **upsert 语义 Replace**：Replace 同名替换，未找到则追加，简化 user-agent 编辑路径
- **extras 可变参数**：Reload 支持 extras root，保留内置 agent 不丢失
- **委托 LoadAll**：Load/Reload 都委托 LoadAll，统一 plugin container 递归逻辑
- **nil 安全**：Add/AddAll 跳过 nil agent；Replace 跳过 nil 或空 name

## 8. 注意事项

- **ByName 返回指针非拷贝**：调用方修改会污染 registry；如需修改应用 Replace
- **Reload 不保证 in-flight 一致性**：已 spawn 的 worker 持有旧 agent 定义，不受 Reload 影响（这可能是有意的，避免 worker 中途行为变化）
- **name 唯一性不强制**：Add 不检查重名，可能插入同名 agent；Replace 则替换首个同名
- **Remove 切片拼接**：`append(agents[:i], agents[i+1:]...)` 会修改原切片底层数组，但因持锁安全
- **Reload 失败不回滚**：LoadAll 失败时 registry 保持原状（因新切片在锁外构建，失败时不加锁）
- **agentsRoot 不存在不报错**：fresh install 场景正常，但可能掩盖配置错误
- **无索引**：ByName 线性扫描 O(n)；agent 数量通常 <100，无需 map 索引
