# `traces/render.go` 技术实现文档

> 源文件：`internal/edgeagent/plugins/traces/render.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/plugins/traces`

## 1. 概述

本文件是 traces 插件的 otelcol.yaml 渲染层。基于 `text/template` 把 `PluginConfig` 渲染成 OTel Collector 配置：receivers（OTLP gRPC+HTTP，默认 bind 127.0.0.1）→ processors（可选 memory_limiter / k8sattributes / resource/device 注入 device_id+ongrid_source+extra_attrs / 可选 resource/loki_labels）→ batch（bounded 或默认）→ exporters（otlphttp/manager 必选，可选 loki/manager 与 prometheusremotewrite/manager 或 prometheus/gateway）。支持 K8s telemetry gateway 模式（同时接收 traces/logs/metrics）与 bounded_pipelines 内存约束模式。TLS 默认 skip-verify（自签证书）。

## 2. 包信息

- **包名**：`traces`
- **所属模块**：`internal/edgeagent/plugins/traces`
- **依赖方向**：被本包 `plugin.go` 的 `New` 作为 `ConfigRender` 传入 SubprocessPlugin；调用 `internal/edgeagent/plugins.PluginConfig`

## 3. 关键类型与接口

无导出类型。核心常量 `otelcolTemplate`（text/template 字符串）。

## 4. 关键函数与流程

### `render(cfg) ([]byte, error)`
- **职责**：渲染 otelcol.yaml。
- **流程**：
  1. 校验：`cfg.Endpoint` 必填；`omit_device_id=false` 时 `cfg.EdgeID` 必填
  2. 解析 spec 各字段（默认值）：
     - `grpc_endpoint` 默认 `127.0.0.1:4317`，`http_endpoint` 默认 `127.0.0.1:4318`
     - `extra_attrs` map
     - `enable_k8sattributes`/`enable_logs`/`logs_endpoint`/`enable_metrics`/`metrics_export_endpoint`/`metrics_remote_write_endpoint`
     - `bounded_pipelines` + `memory_limit_mib`(384)/`memory_spike_limit_mib`(96)/`batch_send_size`(2048)/`batch_max_size`(4096)/`queue_size`(512)
     - `tls_insecure_skip_verify` 默认 true
     - `collector_metrics_endpoint` 默认 `127.0.0.1:8888`
  3. bounded_pipelines 校验：spike < limit；0 < send_size <= max_size <= 4096；0 < queue_size <= 4096
  4. enable_logs=true 要求 logs_endpoint；enable_metrics=true 要求 metrics_export_endpoint 或 metrics_remote_write_endpoint
  5. 构造 authHeader：AuthPass 非空时 AuthUser 非空用 Basic，否则 Bearer
  6. logsAuthHeader：默认同 authHeader；`logs_auth_override=true` 时从 logs_auth_user/pass/bearer 构造
  7. metricsAuthHeader：从 metrics_remote_write_auth_user/pass/bearer 构造
  8. 构造 data map（含上述所有字段）
  9. `template.New("otelcol").Parse(otelcolTemplate)` execute 到 buffer
- **错误处理**：校验失败 / template parse/execute 失败用 `%w` 包装。

### 模板 `otelcolTemplate`
- **结构**：
  - `receivers.otlp.protocols.grpc.endpoint` / `http.endpoint`
  - `processors`：
    - `memory_limiter`（bounded_pipelines 时）
    - `k8sattributes`（enable_k8sattributes 时，serviceAccount auth，提取 k8s.* metadata）
    - `resource/device`：upsert `device_id`（omit_device_id=false 时）+ `ongrid_source=otlp` + extra_attrs
    - `resource/loki_labels`（enable_logs 时，从 k8s.* 提取 namespace/pod/node + loki.resource.labels）
    - `batch/traces`/`batch/logs`/`batch/metrics`（bounded_pipelines 时）或 `batch`（默认）
  - `exporters`：
    - `otlphttp/manager`：traces_endpoint=Endpoint，headers=Authorization，tls skip-verify，compression=gzip，timeout=30s，sending_queue + retry_on_failure
    - `loki/manager`（enable_logs 时）：endpoint=logs_endpoint，default_labels_enabled，可选 sending_queue+retry
    - `prometheusremotewrite/manager`（enable_metrics + metrics_remote_write_endpoint 时）：endpoint，resource_to_telemetry_conversion，remote_write_queue
    - `prometheus/gateway`（enable_metrics 无 remote_write 时）：endpoint=metrics_export_endpoint
  - `extensions.health_check`：127.0.0.1:13133
  - `service`：
    - `telemetry.logs.level=info`，`telemetry.metrics.address=collector_metrics_endpoint`
    - `pipelines.traces`：receivers=[otlp] → processors=[memory_limiter?, k8sattributes?, resource/device, batch?] → exporters=[otlphttp/manager]
    - `pipelines.logs`（enable_logs 时）：+ resource/loki_labels → [loki/manager]
    - `pipelines.metrics`（enable_metrics 时）：→ [prometheusremotewrite/manager | prometheus/gateway]

### `boolSpec(spec, key) bool`
- spec[key] 为 bool 返回，否则 false。

### `stringOr(spec, key, def) string`
- spec[key] 为非空 string 返回，否则 def。

### `stringMap(spec, key) map[string]string`
- 容忍 `map[string]string` 与 `map[string]interface{}`。

### `basicAuth(user, pass) string`
- `base64.StdEncoding.EncodeToString(user + ":" + pass)`。

### `authHeaderFromValues(user, pass, bearer) string`
- bearer 非空 → `Bearer bearer`；user && pass 非空 → `Basic base64`；否则空。

### `intSpec(spec, key, fallback) int`
- 容忍 int/int64/float64，否则 fallback。

## 5. 依赖关系

- **内部包**：`internal/edgeagent/plugins`（PluginConfig）
- **外部库**：标准库 `bytes`/`encoding/base64`/`fmt`/`strings`/`text/template`
- **被调用方**：本包 `plugin.go`

## 6. 并发与资源管理

无并发控制。`render` 是纯函数，可被多 goroutine 安全调用（实际仅在 SubprocessPlugin.Configure 串行调用）。

## 7. 设计模式与亮点

- **text/template 模板化配置**：复杂 otelcol.yaml 用 Go template 渲染，`{{- if }}` 控制条件块；模板内 pipelines 数组用条件拼接 processor 列表。
- **K8s telemetry gateway 模式**：单 otelcol 同时接收 traces/logs/metrics，K8s controller 场景下作为集群级网关；`enable_k8sattributes` 用 serviceAccount auth 提取 k8s metadata。
- **bounded_pipelines 内存约束**：memory_limiter + 小 batch + 小 queue，适合资源受限 edge；默认模式用大 batch（8192）+ 长 timeout（5s）追求吞吐。
- **device_id 注入可控**：`omit_device_id=true` 用于 cluster gateway collector（device_id 在 gateway 层无意义），避免误导查询。
- **多 exporter 鉴权独立**：traces/loki/metrics 可独立配 auth_header（logs_auth_override / metrics_remote_write_auth_*），适配不同后端鉴权方式。
- **TLS 默认 skip-verify**：标准安装自签证书，操作员换真证书设 `tls_insecure_skip_verify=false`。
- **text/template map range 稳定**：Go 1.12+ text/template range map 按 key 排序，渲染输出稳定。
- **prometheusremotewrite 用 remote_write_queue 非 sending_queue**：注释说明 prometheusremotewrite exporter 有自己的 queue 实现，reject exporterhelper 的通用 sending_queue key——细节考究。

## 8. 注意事项

- 模板内 `resource/device` 的 `device_id` 用 `{{ .EdgeID }}` 渲染，EdgeID=0 时 render 期已拒（除非 omit_device_id=true），不会产生 `device_id: "0"`。
- `bounded_pipelines` 校验严格：spike < limit、send_size <= max_size <= 4096、queue_size <= 4096——防止操作员设过大值 OOM；但 4096 上限是经验值，未来 otelcol 版本可能调整。
- `enable_logs=true` 要求 `logs_endpoint`，但 logs_endpoint 默认空——操作员必须显式设；K8s 场景由 `TunnelConfigFetcher.withKubernetesGatewayTracesDefaults` 注入。
- `metrics_remote_write_endpoint` 与 `metrics_export_endpoint` 互斥：remote_write 优先；二者都未设且 enable_metrics=true 报错。
- `k8sattributes` processor 需要 otelcol 进程能访问 K8s API（serviceAccount）；部署期需确保 RBAC 配置。
- `loki/manager` 的 `default_labels_enabled`：exporter=false/instance=false 避免高基数，job=true/level=true 保留有用标签。
- `sending_queue.queue_size` 默认 1024（非 bounded 时），bounded 时 512——bounded 模式牺牲队列容量换内存。
- `authHeaderFromValues` 优先 bearer，user&&pass 次之——若同时设 bearer 和 user/pass，bearer 胜出，可能不符合操作员预期；文档需明确。
- `intSpec` 对未知类型返回 fallback 而非报错——`memory_limit_mib: "384"` 字符串会被忽略用默认 384，操作员可能困惑。
- 模板内 `{{ .Endpoint }}` 用 `strings.TrimRight(cfg.Endpoint, "/")` 去尾斜杠——otelcol 的 traces_endpoint 是完整 URL，尾斜杠会导致 `/v1/traces/` 双斜杠可能 404。
- `otelcolTemplate` 字符串很长（~255 行），维护时需注意 template 语法与 YAML 缩进；建议有专门的渲染测试覆盖各分支。
