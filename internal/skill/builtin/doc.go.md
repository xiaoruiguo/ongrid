# `doc.go` 技术实现文档

> 源文件：`internal/skill/builtin/doc.go`
> 包路径：`github.com/ongridio/ongrid/internal/skill/builtin`

## 1. 概述

`doc.go` 是 `builtin` 子包的包文档与注册聚合点：通过包级注释说明 builtin skill 集合的设计意图，并以 blank import 形式把所有子目录封装的 mutating/dangerous skill 一次性挂载到 import 链中。导入 `internal/skill/builtin` 即可让全部内置 skill 通过各自 `init()` 完成注册。

## 2. 包信息

- **包名**：`builtin`
- **所属模块**：`internal/skill/builtin`（内置 skill 实现层）
- **依赖方向**：被 `cmd/ongrid` / `cmd/ongrid-edge` blank import；本文件 blank import `internal/skill/builtin/restart_service`

## 3. 关键类型与接口

无显著类型定义。本文件仅包含包注释与一个 blank import 语句。

## 4. 关键函数与流程

本文件无函数定义。核心"流程"是 Go 的导入机制：

### Blank Import 聚合
- **导入语句**：`_ "github.com/ongridio/ongrid/internal/skill/builtin/restart_service"`
- **职责**：触发 `restart_service` 子包的 `init()`，将其 `RestartService` 注册到全局 `Registry`。
- **设计意图**：
  - 子包封装的 mutating/dangerous skill 自带辅助类型与注册 shim，不宜塞进扁平的 `builtin` 命名空间；
  - 新增 mutating/dangerous skill 时，写一个子目录 + 在本文件加一行 blank import 即可；
  - `cmd/ongrid` 与 `cmd/ongrid-edge` 只需 `_ "internal/skill/builtin"` 一行，即可传递性触发所有 skill 注册。

## 5. 依赖关系

- **内部包**：`github.com/ongridio/ongrid/internal/skill/builtin/restart_service`（blank import）
- **外部库**：无
- **被调用方**：`cmd/ongrid`、`cmd/ongrid-edge`（通过 `_ "internal/skill/builtin"` 触发整个 builtin 集合的 init）

## 6. 并发与资源管理

无并发控制。本文件仅声明 import，不涉及运行时逻辑。

## 7. 设计模式与亮点

- **包文档 + 聚合点双重职责**：`doc.go` 既承载包级设计文档（包注释详述 builtin skill 的注册机制与新增流程），又作为子包 blank import 的聚合点，符合 Go 社区惯例。
- **init() 自注册模式**：每个 skill 在自己文件中 `func init() { skill.Register(...) }`，配合 blank import 实现"导入即注册"，调用方无需手动枚举。
- **扁平 vs 子包分层**：safe 类 skill 直接放 `builtin/` 下（扁平），mutating/dangerous 类放子目录（隔离辅助类型），通过 `doc.go` 的 blank import 统一挂载。
- **传递性 import**：`cmd` 只需一行 import 即可拿到所有 builtin skill，新增 skill 时无需修改 `cmd` 代码。

## 8. 注意事项

- **新增 mutating/dangerous skill 流程**：在 `builtin/` 下建子目录 → 写 skill 文件（含 `init()` 注册）→ 在本文件 `import` 块加一行 blank import。漏掉第三步会导致 skill 不被注册。
- **blank import 顺序不影响注册**：Go 保证同一 binary 内所有 `init()` 在 `main` 之前执行，但多个 `init()` 之间无明确顺序；skill 之间不应有注册顺序依赖。
- **包注释是设计文档**：包含"为何用 skill 替代 per-capability tunnel 方法"、"权限分级设计"等核心架构决策，修改本包时应同步更新注释。
- **`builtin` 包自身无 `init()`**：所有注册由各 skill 文件或子包的 `init()` 完成；本文件只负责聚合 import。
- **safe 类 skill 无需在本文件 blank import**：它们直接在 `builtin` 包内（如 `probe_tcp.go`），导入 `builtin` 即触发其 `init()`；只有子包封装的 skill 才需要在此 blank import。
