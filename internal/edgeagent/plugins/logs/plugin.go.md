# `logs/plugin.go` 技术实现文档

> 源文件：`internal/edgeagent/plugins/logs/plugin.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/plugins/logs`

## 1. 概述

`logs` 是边缘端日志插件，包装 Promtail 子进程：ongrid-edge 把 manager 推送的 `PluginConfig` 渲染成 `promtail.yaml`，spawn promtail，让其直接 push 到 manager nginx `/loki/api/v1/push`。ongrid-edge 不接触日志字节流，仅负责配置渲染与生命周期。本文件仅是薄构造层，配置模板在 `render.go`。

## 2. 包信息

- **包名**：`logs`
- **所属模块**：`internal/edgeagent/plugins/logs`
- **依赖方向**：被 main 注册到 Supervisor；调用 `internal/edgeagent/plugins.NewSubprocess`

## 3. 关键类型与接口

无包级导出类型。返回值 `plugins.Plugin`（实际为 `*plugins.SubprocessPlugin`）。

```go
const Name = "logs"
```

## 4. 关键函数与流程

### `New(binDir, workDir, log) plugins.Plugin`
- **职责**：构造 SubprocessPlugin，绑定 promtail 二进制与渲染函数。
- **流程**：`NewSubprocess(SubprocessOpts{`
  - Name = "logs"
  - Binary = `binDir/promtail`
  - WorkDir = `workDir/logs`
  - ConfigFile = `workDir/logs/promtail.yaml`
  - ConfigRender = `render`（来自 render.go）
  - Args 闭包：`["-config.file=<configFile>", "-positions.file=<dir>/positions.yaml"]`（positions 文件与 config 同目录，recreate workdir 不丢 journald cursor）
  - Log
  - `})`
- **错误处理**：构造期不做 IO，不返回错误。

## 5. 依赖关系

- **内部包**：`internal/edgeagent/plugins`（NewSubprocess/SubprocessOpts/PluginConfig/Plugin）
- **外部库**：标准库 `log/slog`/`path/filepath`
- **外部二进制**：`promtail`（由 `binDir` 提供，通常 `/opt/ongrid-edge/bin`）
- **被调用方**：main.go

## 6. 并发与资源管理

无自身并发控制。生命周期全部委托给 `SubprocessPlugin`（其内部有 `mu`/`stoppedCh`/runLoop goroutine）。

## 7. 设计模式与亮点

- **薄构造层**：本文件仅 40 行，把"二进制名 + 配置文件路径 + 渲染函数 + args 模板"打包给 SubprocessPlugin，所有生命周期/崩溃重启/输出捕获由通用底座承担。
- **positions 文件同目录**：`-positions.file=` 与 config 同目录，确保重建 workDir 时 journald cursor 不丢（实际重建会丢，但同目录至少保证 promtail 运行期 cursor 持久）。
- **配置渲染与生命周期分离**：render.go 专注 YAML 模板，plugin.go 专注组装，单一职责。

## 8. 注意事项

- `New` 不校验 `binDir`/`workDir` 存在，实际校验在 `SubprocessPlugin.Start` 的 `os.Stat(binary)` 与 `Configure` 的 `os.MkdirAll(workDir)`。
- positions 文件路径用 `filepath.Dir(configFile)` 推导，若 configFile 是相对路径会出问题——实际 configFile 由绝对 workDir 拼成，OK。
- promtail 二进制需由 ongrid-edge 部署期打包到 `binDir`，main.go 需确保 binDir 正确传入。
- 本插件不暴露任何自定义 spec 解析，全部由 render.go 处理；若未来加 spec 校验需在本文件或独立文件补充。
