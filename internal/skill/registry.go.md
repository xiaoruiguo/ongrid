# `registry.go` 技术实现文档

> 源文件：`internal/skill/registry.go`
> 包路径：`github.com/ongridio/ongrid/internal/skill`

## 1. 概述

`registry.go` 是 ongrid skill 框架的进程级目录服务：维护 `Key -> Executor` 全局映射，提供注册、查询、按 Class 过滤等 API。skill 包通过 `init()` 调用 `Register` 完成自举；manager 拉取元数据生成 LLM 工具/HTTP 路由；edge 调度器按 Key 查找 Executor 派发 `execute_skill` RPC。

## 2. 包信息

- **包名**：`skill`
- **所属模块**：`internal/skill`（框架核心层）
- **依赖方向**：被所有 builtin skill 子包 `init()` 调用 `Register`；被 `cmd/*`、`internal/manager/*`、`internal/edgeagent/*` 读取 `Get/All/AllByClass`

## 3. 关键类型与接口

```go
// 进程级单例，包级变量 globalRegistry 在加载时初始化
var globalRegistry = &Registry{
    skills: map[string]Executor{},
}

type Registry struct {
    mu     sync.RWMutex
    skills map[string]Executor
}

// 预定义错误：skill key 未找到时返回
var ErrNotFound = errors.New("skill: not found")
```

`Registry` 同时对外暴露包级便捷函数 `Register/Get/All/AllByClass` 与实例方法，前者代理 `globalRegistry`，后者供测试构造独立实例。

## 4. 关键函数与流程

### 包级便捷函数
- `Register(e Executor) Metadata` → 转发 `globalRegistry.Register`
- `Get(key string) (Executor, bool)` → 转发 `globalRegistry.Get`
- `All() []Executor` → 转发 `globalRegistry.All`
- `AllByClass(classes ...Class) []Executor` → 转发 `globalRegistry.AllByClass`

### `(*Registry) Register`
- **签名**：`func (r *Registry) Register(e Executor) Metadata`
- **职责**：校验并注册一个 Executor。
- **流程**：
  1. nil Executor → `panic`；
  2. `e.Metadata()` + `Validate()` 校验元数据，失败 `panic`（作者期错误，应炸响）；
  3. 加写锁；重复 Key → `panic`；否则写入 map。
- **错误处理**：所有失败均 `panic` 而非返回 error——这些是 init() 阶段作者期错误，应让 binary 启动失败暴露问题，而不是运行时 nil 派发。

### `(*Registry) Get`
- **签名**：`func (r *Registry) Get(key string) (Executor, bool)`
- **职责**：按 Key 查找 Executor。读锁保护；返回 `(nil, false)` 时由调度器转换为 404 风格错误。

### `(*Registry) All`
- **签名**：`func (r *Registry) All() []Executor`
- **职责**：返回全部已注册 skill，按 Key 字典序排序。用于 LLM 工具注册表、`/skills` HTTP 列表、UI 下拉。
- **流程**：读锁 → 拷贝到新 slice → `sort.Slice` 按 `Metadata().Key` 排序。

### `(*Registry) AllByClass`
- **签名**：`func (r *Registry) AllByClass(classes ...Class) []Executor`
- **职责**：按 Class 过滤（如 manager 只让 LLM 自动调用 `ClassSafe`，`ClassMutating`/`ClassDangerous` 走审批流）。
- **流程**：将 classes 转为 set；读锁遍历，按 `Metadata().EffectiveClass()` 命中过滤；按 Key 排序。

### `NewRegistryForTest`
- **签名**：`func NewRegistryForTest() *Registry`
- **职责**：构造全新独立 Registry，用于单测隔离（验证重复 key / 非法 metadata 等场景不污染全局）。

## 5. 依赖关系

- **内部包**：仅依赖同包 `Executor`、`Metadata`、`Class`（types.go 定义）
- **外部库**：`errors`、`fmt`、`sort`、`sync`
- **被调用方**：所有 `builtin/*` 子包 `init()`；`cmd/ongrid`、`cmd/ongrid-edge` 启动；`internal/manager` 元数据列举；`internal/edgeagent` 调度派发

## 6. 并发与资源管理

- **`sync.RWMutex`** 保护 `skills` map：注册阶段（init）写锁，运行阶段并发读锁。
- 文档注释明确：注册发生在 init() 单线程阶段，读发生在运行期高并发阶段，RWMutex 兼顾二者。
- `All`/`AllByClass` 在锁内完成 slice 拷贝与排序，返回值是独立副本，调用方遍历无需再加锁。

## 7. 设计模式与亮点

- **Panic-on-Author-Error 策略**：注册失败不返回 error 而是 panic，让作者期错误在 boot 时即崩溃暴露，避免运行时 nil 派发这种隐蔽 bug。
- **包级单例 + 实例方法双 API**：生产用包级便捷函数操作 `globalRegistry`，测试用 `NewRegistryForTest()` 构造隔离实例，避免污染全局状态。
- **EffectiveClass 过滤**：调用方传入 `Class` 列表，过滤时统一走 `EffectiveClass()`（处理零值降级），保证未显式设置 Class 的旧 skill 也能被正确归类。
- **稳定排序输出**：所有列举都按 Key 字典序排序，保证 LLM 工具注册表、HTTP 响应、UI 列表跨次启动一致。

## 8. 注意事项

- **`Register` 在 init() 之外调用需谨慎**：运行期注册会与并发读竞争写锁；当前设计假设注册只在 boot 阶段完成。
- **panic 不可恢复的语义**：`Register` 的 panic 设计基于"作者期错误"假设，若运行期动态注册（如外部 skill pack 热加载）需要更柔和的策略，应另行封装（参见 `loader.go` 的 `defer/recover` 包裹）。
- **`All`/`AllByClass` 每次都分配新 slice + 排序**：在 skill 数量大且调用频繁的场景（如每请求都列举）有少量开销，可考虑缓存；当前 skill 数量级（<100）下无需优化。
- **`ErrNotFound` 当前未被本文件使用**：调度方应自行根据 `Get` 返回的 `bool` 转换为业务错误；保留 `ErrNotFound` 便于 `errors.Is` 链式判断。
- **依赖 `Metadata()` 多次调用**：`All`/`AllByClass` 排序与过滤都调用 `Metadata()`，实现方应保证该方法轻量（最好返回值类型或常量指针）。
