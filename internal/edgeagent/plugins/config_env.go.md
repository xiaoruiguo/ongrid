# `config_env.go` 技术实现文档

> 源文件：`internal/edgeagent/plugins/config_env.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/plugins`

## 1. 概述

`EnvConfigFetcher` 是 PR-C1 阶段的引导配置源：从环境变量读取插件配置，让插件运行时在 manager 侧 `edge_plugin_configs` 表与 tunnel RPC 落地前就能端到端验证。PR-C2 后降级为 fallback / dev-mode 路径，被 `TunnelConfigFetcher` 包装。

## 2. 包信息

- **包名**：`plugins`
- **所属模块**：`internal/edgeagent/plugins`
- **依赖方向**：被 `TunnelConfigFetcher`（作为 fallback）和 main（PR-C1 直接使用）调用；实现本包 `ConfigFetcher` 接口

## 3. 关键类型与接口

```go
type EnvConfigFetcher struct {
    knownPlugins []string // 启动期由 main 传入已注册插件名列表
}
```

实现 `ConfigFetcher.Fetch(ctx) (map[string]PluginConfig, error)`。

## 4. 关键函数与流程

### `NewEnvConfigFetcher(knownPlugins []string) *EnvConfigFetcher`
- **职责**：构造 fetcher，拷贝 knownPlugins 防外部修改。
- **流程**：`append([]string(nil), knownPlugins...)` 做防御性拷贝。

### `Fetch(_ context.Context) (map[string]PluginConfig, error)`
- **职责**：从 env 拼装每个已知插件的 PluginConfig。
- **流程**：读 `ONGRID_EDGE_ID`（envUint）→ 遍历 knownPlugins，对每个 name 构造 prefix `ONGRID_EDGE_PLUGIN_<UPPER_NAME>_`：
  - `ENABLED` → envBool
  - `ENDPOINT` → os.Getenv
  - `AUTH_USER` → firstNonEmpty(本插件 env, `ONGRID_EDGE_ACCESS_KEY`)
  - `AUTH_PASS` → firstNonEmpty(本插件 env, `ONGRID_EDGE_SECRET_KEY`)
  - `SPEC_JSON` → 若非空 `json.Unmarshal` 到 map；解析失败静默忽略（操作员看到插件 disabled / 缺 spec 即可定位 typo）
- **错误处理**：始终返回 `(snapshot, nil)`，不因 env typo 崩 supervisor。
- **ctx**：忽略（env 读取非 IO），签名仅为满足接口。

### `envBool(key) bool`
- 取 env，trim+lower，接受 `true`/`1`/`yes`/`on`。

### `firstNonEmpty(vals ...string) string`
- 返回首个非空字符串，用于 AUTH_USER/PASS 在插件级 env 缺失时回退到 edge 全局凭证。

### `envUint(key) uint64`
- 手写十进制解析，遇到非数字字符返回 0（不返回错误，与 ENV 配置缺失语义合并）。

## 5. 依赖关系

- **内部包**：无
- **外部库**：标准库 `context`/`encoding/json`/`os`/`strings`
- **被调用方**：`TunnelConfigFetcher.fallback`、main.go（PR-C1）

## 6. 并发与资源管理

无并发控制。无状态对象，`Fetch` 只读 env，可被多 goroutine 安全调用。

## 7. 设计模式与亮点

- **凭证复用回退**：插件级 AUTH_USER/PASS 缺失时回退到 edge 全局 ACCESS_KEY/SECRET_KEY，操作员只需设 ENABLED+ENDPOINT 即可让数据面复用 tunnel 凭证。
- **静默容错**：SPEC_JSON 解析失败不报错，避免 env typo 拉垮 supervisor；操作员通过"插件未启用 / 缺 spec"的故障表现定位。
- **knownPlugins 过滤**：仅返回已注册插件，避免 env 中残留的未知插件名污染 supervisor。

## 8. 注意事项

- `Fetch` 忽略 ctx，意味着即便 ctx 已取消也会读完所有 env——env 读取很快，实际无害。
- `envUint` 返回 0 既表示"未设"也表示"非法值"，二者无法区分；调用方（TunnelConfigFetcher）通过 env > tunnel 的优先级覆盖处理。
- 凭证会随 env 进程级泄露（如 `/proc/<pid>/environ`）；ongrid-edge 进程本身已被 owner 保护，但部署期需注意 env 不被 systemd unit dump 等渠道泄露。
- SPEC_JSON 必须是 JSON 对象，不能是数组或标量——`map[string]interface{}` 类型断言会失败导致 Spec 为空。
