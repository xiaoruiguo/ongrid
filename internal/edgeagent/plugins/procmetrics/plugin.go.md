# `procmetrics/plugin.go` 技术实现文档

> 源文件：`internal/edgeagent/plugins/procmetrics/plugin.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/plugins/procmetrics`

## 1. 概述

`procmetrics` 是边缘端进程级指标插件，包装 `process_exporter`（ncabatoff/process-exporter）子进程在端口 :9256（避开 hostmetrics 的 9102）暴露按 comm / cmdline regex 分组的 per-process 指标。spec 中 `process_names` 数组原样 marshal 进 YAML（manager UI 控制schema），默认 catch-all 按 comm 分组。本文件含构造、YAML 渲染、CLI args 构造。

## 2. 包信息

- **包名**：`procmetrics`
- **所属模块**：`internal/edgeagent/plugins/procmetrics`
- **依赖方向**：被 main 注册到 Supervisor；调用 `internal/edgeagent/plugins.NewSubprocess`；外部依赖 `gopkg.in/yaml.v3`

## 3. 关键类型与接口

无导出类型。返回 `plugins.Plugin`（实际 `*plugins.SubprocessPlugin`）。

```go
const (
    Name                  = "procmetrics"
    DefaultListenAddress  = ":9256"
)

var defaultProcessNames = []map[string]interface{}{
    {"name": "{{.Comm}}", "cmdline": []string{".+"}},
}
```

## 4. 关键函数与流程

### `New(binDir, workDir, log) plugins.Plugin`
- **职责**：构造 SubprocessPlugin。
- **流程**：`NewSubprocess(SubprocessOpts{`
  - Name = "procmetrics"
  - Binary = `binDir/process_exporter`
  - WorkDir = `workDir/procmetrics`
  - ConfigFile = `workDir/procmetrics/process-exporter.yaml`
  - ConfigRender = `render`
  - Args = `buildArgs`
  - Log
  - `})`

### `render(cfg) ([]byte, error)`
- **职责**：渲染 process-exporter.yaml。
- **流程**：
  1. processNames 默认为 `defaultProcessNames`（catch-all by comm）
  2. 若 `cfg.Spec["process_names"]` 是 `[]interface{}` 且非空，逐项断言为 `map[string]interface{}` 转成 `[]map[string]interface{}`
  3. 若转换后非空，覆盖 processNames
  4. `yaml.Marshal(processNames)` → `indentYAML(pnYAML, "  ")` 缩进 2 空格
  5. `template.New("procmetrics").Parse(configTmpl)` execute `{"ProcessNamesYAML": indented}` 到 buffer
- **错误处理**：yaml.Marshal / template parse / execute 失败用 `%w` 包装。

### `buildArgs(cfg, configFile) []string`
- **职责**：构造 process_exporter CLI args。
- **流程**：
  1. listen 默认 `:9256`；spec.listen_address 非空覆盖
  2. procfs 默认空（process-exporter 用 /proc）；spec.procfs 非空覆盖
  3. args = `["-web.listen-address=<listen>", "-config.path=<configFile>"]`
  4. procfs 非空 append `"-procfs=<procfs>"`
- **错误处理**：无显式错误，spec 类型断言失败用默认值。

### `indentYAML(in, prefix) string`
- **职责**：把 YAML 输出每行加 prefix 缩进。
- **流程**：按 `\n` split，跳过空行，每行加 prefix + 换行。
- **用途**：让 `process_names` 列表嵌套在模板的顶层 key 下。

### 模板 `configTmpl`
```yaml
# Rendered by ongrid-edge procmetrics plugin.
# DO NOT EDIT — regenerated from manager-pushed PluginConfig on every reconcile.

process_names:
{{ .ProcessNamesYAML }}
```

## 5. 依赖关系

- **内部包**：`internal/edgeagent/plugins`（NewSubprocess/SubprocessOpts/PluginConfig/Plugin）
- **外部库**：`gopkg.in/yaml.v3`（Marshal）、标准库 `bytes`/`fmt`/`log/slog`/`path/filepath`/`text/template`
- **外部二进制**：`process_exporter`（由 `binDir` 提供）
- **被调用方**：main.go

## 6. 并发与资源管理

无自身并发控制。生命周期全部委托给 `SubprocessPlugin`。`render` 是纯函数，可被多 goroutine 安全调用（实际仅在 SubprocessPlugin.Configure 串行调用）。

## 7. 设计模式与亮点

- **process_names 透传**：spec 中的 `process_names` 数组不经 schema 校验直接 marshal 进 YAML，agent 不需知道 process-exporter 的配置 schema——manager UI 控制schema 与校验，agent 只搬运。
- **默认 catch-all by comm**：`defaultProcessNames` 按 `{{.Comm}}` 分组所有进程，匹配 install-edge.sh 的 fallback，确保 agent 在 manager 推 spec 前就产生有用 series。
- **YAML 缩进技巧**：`yaml.Marshal` 列表项无前置缩进，`indentYAML` 加 2 空格让列表项嵌套在 `process_names:` key 下，避免手写多行 template range。
- **procfs 可配**：K8s 场景下 hostPath `/proc` 可能 mount 到别处（如 `/host/proc`），`-procfs` flag 适配。
- **端口选择**：9256 避开 9100（manager 容器）/9102（hostmetrics），与 `reservedListenPorts` 字典一致。

## 8. 注意事项

- `render` 对 `cfg.Spec["process_names"]` 的类型断言是 `[]interface{}`——JSON 解码形态；若 manager 传 `[]map[string]interface{}`（非标准 JSON）会断言失败用默认值。当前 PluginConfig.Spec 从 JSON 来，OK。
- `indentYAML` 跳过空行——若 yaml.Marshal 输出中间有空行（如多文档分隔符 `---`）会被去掉，可能改变语义；process-exporter 单文档无此问题。
- `buildArgs` 对 spec 类型断言失败静默用默认值——`listen_address: 9256`（数字非字符串）会被忽略用默认 `:9256`，操作员可能困惑。
- `defaultProcessNames` 用 `{{.Comm}}` 模板字符串——process-exporter 的 Go template 语法，本插件的 `text/template` 渲染不会展开它（因为是 YAML 字符串值非 template 指令），OK。
- 本插件无 spec 校验——`process_names` 数组结构错误（如缺 cmdline）会让 process-exporter 启动失败，SubprocessPlugin 会崩溃重启循环；manager UI 应做校验。
- `New` 不校验 binDir/workDir 存在，实际校验在 SubprocessPlugin.Start/Configure。
- process_exporter 的 `-children` flag（是否跟踪子进程）未暴露——默认 true，若操作员需关闭需通过 extra_args（但本插件未支持 extra_args），可考虑扩展。
