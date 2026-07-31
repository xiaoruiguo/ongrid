# `logs/render.go` 技术实现文档

> 源文件：`internal/edgeagent/plugins/logs/render.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/plugins/logs`

## 1. 概述

本文件是 logs 插件的 promtail.yaml 渲染层。基于 `text/template` 把 `PluginConfig` 渲染成 Promtail 配置：clients 推送到 manager nginx `/loki/api/v1/push`（basic_auth 或 bearer_token）、external_labels 注入 `device_id` + 可选 K8s cluster_id/node + extra_labels；scrape_configs 支持 host 模式（journald + file_paths）与 kubernetes 模式（CRI pod 日志 tail）。默认开启 journald（systemd 主机通用、自轮转），TLS 默认 skip-verify（标准安装自签证书）。

## 2. 包信息

- **包名**：`logs`
- **所属模块**：`internal/edgeagent/plugins/logs`
- **依赖方向**：被本包 `plugin.go` 的 `New` 作为 `ConfigRender` 传入 SubprocessPlugin；调用 `internal/edgeagent/plugins.PluginConfig`

## 3. 关键类型与接口

无导出类型。核心常量 `promtailTemplate`（text/template 字符串）与 `defaultKubernetesPodLogPath`。

## 4. 关键函数与流程

### `render(cfg plugins.PluginConfig) ([]byte, error)`
- **职责**：渲染 promtail.yaml。
- **流程**：
  1. 校验：`cfg.Endpoint` 必填；`cfg.EdgeID` 必填（=0 报错"set ONGRID_EDGE_ID"）
  2. 解析 spec：
     - `mode`（host/kubernetes），kubernetes 模式要求 `cluster_id`
     - `pod_log_path` 默认 `/var/log/pods/*/*/*.log`
     - `enable_journald`：非 K8s 模式默认 true，K8s 模式强制 false
     - `tls_insecure_skip_verify` 默认 true（自签证书）
     - `journald_units`/`file_paths` 走 stringSlice
     - `extra_labels` 走 stringMap
  3. fallback：非 K8s + journald off + 无 file_paths → tail `/var/log/syslog`/`/var/log/messages`（避免零 job 静默空 Loki）
  4. K8s 模式清空 filePaths/units
  5. 构造 `data` map（含 Endpoint/AuthUser/AuthPass/EdgeID/ExtraLabels/EnableJournald/JournaldUnits/JournaldUnitsRegex/FilePaths/TLSInsecureSkipVerify/KubernetesMode/ClusterID/NodeName/PodLogPath）
  6. `template.New("promtail").Funcs({jobNameSafe})` 解析 `promtailTemplate` → execute 到 buffer
- **错误处理**：模板 parse/execute 失败用 `%w` 包装。

### 模板 `promtailTemplate`
- **结构**：
  - `server.disable: true`（无 HTTP API，仅 push）
  - `clients`：单 client，url=Endpoint，basic_auth（AuthUser 非空）或 bearer_token（AuthPass 非空），tls_config.insecure_skip_verify（可选），tenant_id=ongrid，backoff_config/batchsize/batchwait，external_labels（device_id + K8s cluster_id/node + extra_labels）
  - `scrape_configs`：
    - K8s 模式：`kubernetes-pods` job，pipeline_stages 解析 CRI 日志文件名提取 namespace/pod/container，`__path__=pod_log_path`
    - host 模式：
      - `journald` job（若 enable_journald）：journal.max_age=12h，relabel 提取 unit/identifier/level，journald_units 非空时 regex keep 过滤
      - `file-<safe>` job per file_path：`__path__`=path，labels ongrid_source/job

### `stringSlice(spec, key) []string`
- 容忍 `[]string` 与 `[]interface{}`（JSON 解码形态）。

### `stringMap(spec, key) map[string]string`
- 容忍 `map[string]string` 与 `map[string]interface{}`。

### `stringSpec(spec, key) string`
- 容忍 string/fmt.Stringer/json.Number/float/int 等多种形态，统一返回字符串。

### `joinRegex(units) string`
- **职责**：把 journald unit 名数组拼成 OR-regex（promtail relabel regex 语义）。
- **流程**：拷贝→sort→逐个 `regexEscape`→`|` join。空数组返回空串。

### `regexEscape(s) string`
- 转义 Promtail RE2 元字符（`. + * ? ( ) [ ] { } | ^ $ \`）。

### `jobNameSafe(s) string`
- 把文件路径转成 label-safe 的 job 名片段：字母数字保留，其他转 `-`，trim 首尾 `-`。

## 5. 依赖关系

- **内部包**：`internal/edgeagent/plugins`（PluginConfig）
- **外部库**：标准库 `bytes`/`encoding/json`/`fmt`/`sort`/`strconv`/`strings`/`text/template`
- **被调用方**：本包 `plugin.go`

## 6. 并发与资源管理

无并发控制。`render` 是纯函数，可被多 goroutine 安全调用（实际仅在 SubprocessPlugin.Configure 串行调用）。

## 7. 设计模式与亮点

- **text/template 模板化配置**：YAML 用 Go template 渲染，避免手写 yaml.Marshal 的结构体对齐问题；模板内 `{{- if }}` 控制空行。
- **journald 默认开启**：systemd 主机通用 + 自轮转 + 自带 unit 标签，比 rsyslog/`/var/log/syslog`（Alpine/Arch/容器无）更可靠；操作员可 `enable_journald=false` 切回 syslog tail。
- **K8s 模式 CRI 解析**：正则从文件名 `/var/log/pods/<ns>_<pod>_<uid>/<container>/*.log` 提取 namespace/pod/container 作为 label，无需额外 k8s API 调用。
- **fallback 避免 zero-job 静默**：journald off + 无 file_paths 时自动 tail syslog，避免配置空导致 Loki 无数据被误判"RAG/logs 坏了"。
- **device_id 标签注入**：external_labels 携带 `device_id=<EdgeID>`，Loki 侧按设备维度聚合；K8s 模式额外注入 cluster_id/node。
- **journald_units regex 排序**：`joinRegex` 先 sort 再 join，保证渲染输出稳定（同配置产出同 YAML，便于 diff）。
- **TLS 默认 skip-verify**：标准安装自签证书（deploy/install/upgrade.sh），操作员换真证书后设 `tls_insecure_skip_verify=false`。

## 8. 注意事项

- 模板内 `external_labels.device_id: "{{ .EdgeID }}"` 用 uint64 渲染，若 EdgeID=0 render 期已拒，不会产生 `device_id: "0"` 的脏标签。
- `jobNameSafe` 把所有非字母数字转 `-`，对长路径会生成很长的 job 名——Promtail 对 job 名长度无硬限制但 Loki 标签值有 1024 字节限制，超长路径可能触发。
- `joinRegex` 对 unit 名做 RE2 转义，但 Promtail 的 relabel regex 是 RE2 不是 Go regexp——转义字符集一致，OK。
- `stringSpec` 对 `interface{}` 形态用 type switch，未覆盖的类型返回空串——若操作员传 `cluster_id: 42`（JSON number）会被 `stringSpec` 处理为 "42"（json.Number 分支），OK；但 `cluster_id: true` 会返回空串导致 K8s 模式报"cluster_id required"。
- 模板注释说"renamed in label space May 2026"——device_id 与 EdgeID 字段名不一致是历史包袱，文档需明确二者关系。
- `enable_journald` 在 K8s 模式被强制 false（即使 spec 设 true 也忽略），render 内 `if v, ok := spec["enable_journald"]; ok && !kubernetesMode` 才读 spec——K8s 模式直接用默认 false。
- TLS skip-verify 默认 true 是安全 trade-off：标准安装自签证书必须跳过验证才能工作，但操作员若误用 HTTP（非 HTTPS）Endpoint，skip-verify 无意义但不会报错。
