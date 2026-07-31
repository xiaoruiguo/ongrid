# `traces/plugin.go` 技术实现文档

> 源文件：`internal/edgeagent/plugins/traces/plugin.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/plugins/traces`

## 1. 概述

`traces` 是边缘端链路追踪插件，包装 OpenTelemetry Collector（otelcol-contrib）子进程：ongrid-edge 把 manager 推送的 `PluginConfig` 渲染成 `otelcol.yaml`，spawn otelcol-contrib，让其接收本地应用的 OTLP gRPC/HTTP，直接 push 到 manager nginx `/v1/traces`。ongrid-edge 不接触 trace 字节流。本文件仅是薄构造层，配置模板在 `render.go`。命名遵循 OTel signal 复数约定，匹配 OTLP endpoint `/v1/traces`。

## 2. 包信息

- **包名**：`traces`
- **所属模块**：`internal/edgeagent/plugins/traces`
- **依赖方向**：被 main 注册到 Supervisor；调用 `internal/edgeagent/plugins.NewSubprocess`

## 3. 关键类型与接口

无包级导出类型。返回值 `plugins.Plugin`（实际为 `*plugins.SubprocessPlugin`）。

```go
const Name = "traces"
```

## 4. 关键函数与流程

### `New(binDir, workDir, log) plugins.Plugin`
- **职责**：构造 SubprocessPlugin，绑定 otelcol-contrib 二进制与渲染函数。
- **流程**：`NewSubprocess(SubprocessOpts{`
  - Name = "traces"
  - Binary = `binDir/otelcol-contrib`
  - WorkDir = `workDir/traces`
  - ConfigFile = `workDir/traces/otelcol.yaml`
  - ConfigRender = `render`（来自 render.go）
  - Args 闭包：`["--config=<configFile>"]`（otelcol-contrib 用 `--config=...`，也支持重复 flag 分层配置，本插件单文件足够）
  - Log
  - `})`
- **错误处理**：构造期不做 IO，不返回错误。

## 5. 依赖关系

- **内部包**：`internal/edgeagent/plugins`（NewSubprocess/SubprocessOpts/PluginConfig/Plugin）
- **外部库**：标准库 `log/slog`/`path/filepath`
- **外部二进制**：`otelcol-contrib`（由 `binDir` 提供，通常 `/usr/local/lib/ongrid-edge`）
- **被调用方**：main.go

## 6. 并发与资源管理

无自身并发控制。生命周期全部委托给 `SubprocessPlugin`。

## 7. 设计模式与亮点

- **薄构造层**：与 logs/plugin.go 同构，仅 40 行，把"二进制名 + 配置文件路径 + 渲染函数 + args 模板"打包给 SubprocessPlugin。
- **OTel 命名约定**：插件名 `traces`（复数）匹配 OTLP endpoint `/v1/traces` 与 OTel signal 命名规范。
- **配置渲染与生命周期分离**：render.go 专注 otelcol.yaml 模板（含 K8s gateway 模式），plugin.go 专注组装。

## 8. 注意事项

- `New` 不校验 binDir/workDir 存在，实际校验在 SubprocessPlugin.Start 的 `os.Stat(binary)` 与 Configure 的 `os.MkdirAll(workDir)`。
- otelcol-contrib 二进制需由 ongrid-edge 部署期打包到 `binDir`，main.go 需确保 binDir 正确传入。
- `--config=<configFile>` 用 `=` 形式，otelcol-contrib 也支持 `--config <file>` 空格形式，但 `=` 形式更明确。
- 本插件不暴露任何自定义 spec 解析，全部由 render.go 处理；render.go 含 K8s gateway 模式（enable_logs/enable_metrics/enable_k8sattributes）等复杂分支。
- otelcol-contrib 启动后会绑定 4317（gRPC）/4318（HTTP）接收端口，部署期需确保端口未被占用；render.go 中默认 bind 127.0.0.1 而非 0.0.0.0，避免公网暴露。
- otelcol-contrib 自带 retry/queue，SubprocessPlugin 的崩溃重启与之独立——otelcol 退出会被 SubprocessPlugin 视为崩溃重启。
