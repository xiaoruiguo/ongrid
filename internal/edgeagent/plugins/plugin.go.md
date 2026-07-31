# `plugin.go` 技术实现文档

> 源文件：`internal/edgeagent/plugins/plugin.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/plugins`

## 1. 概述

本文件是 ongrid-edge 边缘端插件运行时框架的核心契约定义。声明了所有插件（metrics / logs / traces / hostmetrics / procmetrics / databasemetrics / custommetrics）必须实现的 `Plugin` 接口、配置 / 健康状态结构体、生命周期状态枚举，以及配置源抽象 `ConfigFetcher`。Supervisor 通过这套契约对插件进行统一注册、配置、启停与状态汇总。

## 2. 包信息

- **包名**：`plugins`
- **所属模块**：`internal/edgeagent/plugins`（edgeagent 域下的插件框架层）
- **依赖方向**：
  - 被调用方：`internal/edgeagent`（main / supervisor 组装）、`internal/edgeagent/plugins/{custommetrics,databasemetrics,hostmetrics,logs,metrics,procmetrics,traces}`（具体插件实现 import 本包以获得 `Plugin`/`PluginConfig`/`PluginHealth`/`SubprocessPlugin` 等类型）
  - 调用了谁：仅依赖标准库 `context`、`time`

## 3. 关键类型与接口

```go
// 插件统一接口。Supervisor 按 Configure → Start → (HealthSnapshot)* → Stop 顺序调用
type Plugin interface {
    Name() string
    Configure(cfg PluginConfig) error
    Start(ctx context.Context) error
    Stop(ctx context.Context) error
    HealthSnapshot() PluginHealth
}

// 单个插件在单台 edge 上的配置。Endpoint/Token 属于数据面，Spec 是插件特定 JSON 设置
type PluginConfig struct {
    Enabled  bool
    EdgeID   uint64
    Endpoint string
    AuthUser string
    AuthPass string
    Spec     map[string]interface{}
}

// 生命周期状态
type PluginState string // stopped / starting / running / crashed

// Supervisor 周期上报给 manager 的健康快照
type PluginHealth struct {
    Name         string
    State        PluginState
    LastError    string
    RestartCount int
    PID          int
    StartedAt    time.Time
    UpdatedAt    time.Time
    Targets      []TargetHealth // 多目标指标插件（custommetrics/databasemetrics）的源级状态
}

// 多目标指标插件（custommetrics / databasemetrics）的源级运行状态
type TargetHealth struct {
    ID, Name, Kind, State, LastError string
    Samples                          int
    LastSuccessAt, UpdatedAt         time.Time
}

// 配置源抽象：env 实现（EnvConfigFetcher）+ tunnel 实现（TunnelConfigFetcher）
type ConfigFetcher interface {
    Fetch(ctx context.Context) (map[string]PluginConfig, error)
}
```

## 4. 关键函数与流程

无独立业务函数，本文件仅作类型 / 接口声明。

## 5. 依赖关系

- **内部包**：无
- **外部库**：仅标准库 `context`、`time`
- **被调用方**：Supervisor、各具体插件包、main.go

## 6. 并发与资源管理

本文件不引入并发原语。并发安全责任由具体实现承担（如 `SubprocessPlugin.mu`、`Supervisor.mu`）。

## 7. 设计模式与亮点

- **统一契约**：进程内插件（metrics）与子进程插件（logs/traces）共用同一接口，Supervisor 视角无需区分。
- **配置 / 数据面分离**：`PluginConfig.Endpoint/Token`（数据面凭证）与 `Spec`（业务设置）解耦，manager 可独立轮换凭证。
- **多目标健康嵌套**：`PluginHealth.Targets` 支持多目标指标插件汇报源级状态，便于 manager UI 按目标维度展示。
- **ConfigFetcher 接口**：将"配置从哪来"留给实现，便于 PR-C1 env fallback 与 PR-C2 tunnel RPC 增量替换。

## 8. 注意事项

- `Plugin.Configure` 必须支持重复调用（reconcile 时 manager 推送新配置），实现需做语义对比，避免无谓重启。
- `Plugin.Start` 必须幂等（重复 Start 应为 no-op），`Stop` 必须在未运行时安全调用。
- `HealthSnapshot` 必须不阻塞，被 Supervisor 心跳路径同步调用。
- `PluginState` 取值为字符串常量，跨进程边界（JSON 上报）需保持稳定。
