# `config.go` 技术实现文档

> 源文件：`internal/pkg/config/config.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/config`

## 1. 概述

该文件是 ongrid 运行时配置的统一入口，从环境变量加载所有配置项并应用合理默认值。MVP 阶段刻意不引入 YAML / viper 依赖，仅用 `os.Getenv` + 默认值。`Config` 结构涵盖 HTTP / metrics / DB / JWT / LLM / Admin / Edge / Frontier / Prom / Grafana / Notification / Alert / Logs / Traces / Skills 等子系统，被 `cmd/ongrid` 与 `cmd/ongrid-edge` 共享——两个二进制各取所需字段。

## 2. 包信息

- **包名**：`config`
- **所属模块**：`internal/pkg/`（基础设施层）
- **依赖方向**：被 `cmd/ongrid` / `cmd/ongrid-edge` 启动时调用；几乎被所有 BC 的 wiring 代码引用；仅依赖标准库。

## 3. 关键类型与接口

### `Config`
顶层聚合结构，包含 14 个子配置块（HTTPAddr / MetricsAddr / TunnelAddr / PublicURL / K8s 事件三件套 / DB / JWT / OpenAI / LLM / Admin / Edge / FrontierClient / Prom / Grafana / Notification / Alert / Logs / Traces / Skills）。

### `DBConfig`
后端选择：`Dialect`（`mysql` 默认 / `sqlite`）+ `DSN`（MySQL）+ `Path`（SQLite）。

### `JWTConfig`
`Secret` / `AccessTTL` / `RefreshTTL`，供 iam 使用。

### `OpenAIConfig` 与 `LLMConfig`
OpenAI 单独顶层字段（历史原因），其余多 provider（Anthropic / Zhipu / Gemini / DeepSeek / Kimi）聚合到 `LLMConfig`。`LLMProviderConfig` 含 `Models []string` 闭集供 SPA 选择器使用。

### `PromConfig`
`Enabled` 门控 + `URL` / `RemoteWriteURL` / `QueryURL` / `TLSInsecure` / `TLSCAPath`。`Enabled=false` 时 push handler 静默丢弃、query_promql AI 工具不注册。

### `GrafanaConfig`
首次启动种子值（`InternalRootURL` / `BootstrapUser` / `BootstrapPassword` / `TLSInsecure`）；运行时值由 system_settings 管理。

### `NotificationConfig`
`Enabled` / `DefaultChannels` / `Timeout` + 四个 `NotifyWebhookConfig`（Webhook / Slack / Feishu / DingTalk）。

### `AlertConfig`
主机监控告警评估：`Enabled` / `Cooldown` / `CPUPercent` / `MemPercent` / `DiskUsedPercent` / `Load1` / `EvaluatorInterval`（默认 5m，曾因 30s 太 noisy 调整）/ `EdgeOfflineThreshold` / `PromIngestFailLimit`。

### `FrontierClientConfig`
manager 侧 service-end SDK 拨号 frontier broker：`Addr` / `ServiceName` / `Disabled`（e2e 用）。

### `EdgeConfig`
ongrid-edge 专用：`CloudAddr` / `AccessKey` / `SecretKey` / `CollectorMode`（off / auto / embedded / scrape）/ `ScrapeConfigFile` / `CollectorInterval`。

### `LogsConfig` / `TracesConfig`
Loki / Tempo 查询代理 URL，空则页面 503 + SPA 显示禁用态。

### `SkillsConfig`
manager 侧 subprocess skill loader 的外部目录白名单 `ExternalDirs []string`。

### `NotifyWebhookConfig` / `LLMProviderConfig` / `AdminConfig`
辅助子结构。

## 4. 关键函数与流程

### `Load`
- **签名**：`func Load() (*Config, error)`
- **职责**：从环境变量加载全部配置，应用默认值。
- **流程**：依次填充 14 个子配置块；每个字段调用 `getEnv` / `getEnvBool` / `getEnvInt` / `getEnvFloat` / `getEnvDuration` / `getEnvCSV` 之一。
- **错误处理**：MVP 永不返回非 nil error，签名留 error 是为未来字段校验预留。

### env helper 家族
- `getEnv(key, def)`：空串或缺失返回 def。
- `getEnvBool`：用 `strconv.ParseBool`；非法值返回 def。
- `getEnvInt` / `getEnvFloat`：解析失败返回 def。
- `getEnvDuration`：先 `time.ParseDuration`，失败再尝试整秒；都失败返回 def。
- `getEnvCSV(key, def)`：按 `,` / `;` 切分并 trim，空输入返回 def。
- `splitProviderModels(raw)`：解析 LLM 模型列表，**去重** + trim，空返回 nil。

## 5. 依赖关系

- **内部包**：无（独立工具包）。
- **外部库**：仅标准库 `os` / `strconv` / `strings` / `time`。
- **被调用方**：`cmd/ongrid` / `cmd/ongrid-edge` 启动入口；几乎所有 BC wiring 代码。

## 6. 并发与资源管理

无并发控制。`Load` 一次性构造 `*Config`，调用方通常在启动时单次调用，构造后字段只读使用。

## 7. 设计模式与亮点

- **零依赖策略**：刻意不引入 viper / envconfig，减少二进制体积与依赖面。
- **defensive defaults**：所有 helper 对非法值统一返回 def 而非报错，启动鲁棒性高（小写错 env 不会让进程崩）。
- **字段文档内联**：每个字段注释标明 `env: XXX; default: YYY`，等同于一份环境变量清单，便于运维查阅。
- **模型列表去重**：`splitProviderModels` 用 `seen` map 去重，避免重复条目污染 SPA 选择器。
- **provider gate by APIKey**：LLM provider 以 APIKey 是否非空作为是否暴露的开关，无需额外 enabled 字段。
- **历史决策留痕**：注释中记录了诸如 "log channel 2026-05 移除" / "EvaluatorInterval 30s → 5m 调整原因" 等决策上下文，便于未来维护者理解决策。

## 8. 注意事项

- **JWT secret 默认值**：`ONGRID_JWT_SECRET` 默认 `"dev-insecure-secret-change-me"`，生产必须覆盖。
- **Admin bootstrap**：`AdminConfig` 仅 cloud 二进制使用，空字段则不引导；secret 进环境变量需配合 secret manager。
- **`getEnv` 把空串当缺失**：`v != ""` 才算 set，这意味着无法通过 env 显式设空串（如关闭某 URL）；这种场景需要换成 `LookupEnv` 判定。
- **默认值散落**：默认值写在 `Load` 内联，未集中表；新增字段时易遗漏文档同步。
- **多 provider 模型列表硬编码**：`splitProviderModels` 默认值写死在 `Load` 中，模型迭代需同步改代码 + 重新发布。
- **错误返回形同虚设**：当前 `Load` 永不返回 error，调用方 `if err != nil { fatal }` 实际是死代码；引入真实校验时需全面回归。
