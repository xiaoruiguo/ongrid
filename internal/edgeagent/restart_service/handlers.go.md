# `handlers.go` 技术实现文档

> 源文件：`internal/edgeagent/restart_service/handlers.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/restart_service`

## 1. 概述

本文件实现 `MethodRestartService` edge handler：ongrid 第一个 mutating skill（double-sign / mutating class）。manager 侧 BaseTool 在 reviewer worker 批准后才 dispatch 到此；edge 侧职责双重：(1) defense-in-depth allow-list 重新校验（不信任云端 wire body）；(2) PR-7 mock systemctl shell-out（返回 `Mocked=true` 让审计行显式标明姿态）。真实 `systemctl restart <unit>` 在后续 PR 实现。

## 2. 包信息

- **包名**：`restart_service`
- **所属模块**：edgeagent mutating 能力层
- **依赖方向**：被 `cmd/ongrid-edge` 调用 `Register`；调用 `tunnel`

## 3. 关键类型与接口

```go
// SandboxConfig 是 edge 侧 allow-list
type SandboxConfig struct {
    AllowedUnits []string  // systemd short names（无 .service 后缀）
    Mocked bool            // true = 假装成功；false = 错误「real systemctl not implemented」
    once    sync.Once
    allowed map[string]struct{}  // lazy 填充 by ensure()
}
```

## 4. 关键函数与流程

### `DefaultSandboxConfig`
- **签名**：`func DefaultSandboxConfig() *SandboxConfig`
- **职责**：返回生产默认配置
- **流程**：AllowedUnits = `[nginx, redis, prometheus, loki, tempo, grafana, mysql, ongrid]`；Mocked=true（PR-7 stub 姿态）
- **错误处理**：无错误返回

### `ensure`
- **签名**：`func (s *SandboxConfig) ensure()`
- **职责**：lazy 预计算 allowlist set
- **流程**：`sync.Once.Do` 中遍历 AllowedUnits，小写 + TrimSpace 后加入 map
- **错误处理**：无错误

### `Validate`
- **签名**：`func (s *SandboxConfig) Validate() error`
- **职责**：启动期不变量校验
- **流程**：nil config 报错；`len(AllowedUnits)==0` 报错（防止 operator 误配空 allowlist 等于「允许一切」）
- **错误处理**：返回错误让 Register 传播到 main

### `Allows`
- **签名**：`func (s *SandboxConfig) Allows(unit string) bool`
- **职责**：检查 unit 是否在 allowlist
- **流程**：`ensure()` 预计算 → `canonicalUnit(unit)` 规范化 → map 查找
- **错误处理**：nil config 返回 false；canonical 空返回 false

### `canonicalUnit`
- **签名**：`func canonicalUnit(unit string) string`
- **职责**：规范化 unit 名
- **流程**：`ToLower(TrimSpace(unit))` → `TrimSuffix(u, ".service")` → 含 `/\\ \t` 返回空
- **错误处理**：规范化后空返回空

### `Register`
- **签名**：`func Register(client tunnel.Client, log *slog.Logger) error`
- **职责**：装配 SandboxConfig 并注册 handler
- **流程**：log nil → default；`DefaultSandboxConfig()` → `Validate()` → log Info → `client.RegisterHandler(MethodRestartService, makeRestartHandler(sb, log))`
- **错误处理**：Validate 失败返回错误

### `makeRestartHandler`
- **签名**：`func makeRestartHandler(sb *SandboxConfig, log *slog.Logger) tunnel.Handler`
- **职责**：返回 per-call 闭包
- **流程**：
  1. 解码 `RestartServiceRequest`；失败返回错误
  2. `canonicalUnit(req.Service)` 规范化；空返回错误
  3. `sb.Allows(canonical)` allowlist 重新校验；不在返回错误带完整 allowlist
  4. log Info「restart_service invoked」带 service / reason / mocked
  5. `context.WithTimeout(ctx, restartHandlerTimeout=10s)` + defer cancel
  6. PR-7 stub：`Mocked=false` → 错误「real systemctl shell-out not implemented; set sandbox Mocked=true」；`Mocked=true` → 返回 `RestartServiceResponse{Service, Restarted:true, Mocked:true, StartedAt, EndedAt}`
  7. ctx 已取消 → 错误
- **错误处理**：每步失败返回带上下文的错误

## 5. 依赖关系

- **内部包**：`internal/pkg/tunnel`
- **外部库**：标准库 `context`、`encoding/json`、`errors`、`fmt`、`log/slog`、`strings`、`sync`、`time`
- **被调用方**：`cmd/ongrid-edge` 主程序调 `Register`

## 6. 并发与资源管理

- **`sync.Once`**：`ensure()` 用 Once lazy 预计算 allowlist set；首次 `Allows` 调用时填充
- **per-call `context.WithTimeout`**：10s 上限；`defer cancel()`
- **handler 闭包无状态**：sb 是共享只读（Once 填充后不再修改）；多 RPC 并发调用安全

## 7. 设计模式与亮点

- **defense-in-depth allow-list 重新校验**：manager BaseTool 已校验，但 edge 不信任云端 wire body——重新校验 unit 名；防止被攻破的 manager / 直接 edge poke 绕过
- **canonicalUnit 三重规范化**：TrimSpace + ToLower + TrimSuffix(".service")——容忍 `Nginx` / `nginx.service` / ` nginx ` 等变体
- **`canonicalUnit` 拒绝路径分隔符**：含 `/\\ \t` 返回空——防止 `../etc/passwd` 风格注入
- **Mocked 标志位**：PR-7 stub 显式返回 `Mocked=true`；审计行明确姿态；operator 翻 Mocked=false 期待真实 systemctl 时，handler 错误「not implemented」——防止半实现配置意外触发
- **空 allowlist 校验**：`Validate` 拒绝空 AllowedUnits——防止 operator 误配等于「允许一切」
- **unit 列表与 SKILL.md 同步**：DefaultSandboxConfig 的 8 个 unit 镜像 SKILL.md `[能力: restart_service]` 块
- **错误带完整 allowlist**：拒绝时返回 `(unit, allowed_units)`——让 LLM / operator 知道允许范围

## 8. 注意事项

- **真实 systemctl 未实现**：`Mocked=false` 时永远报错；operator 误翻 Mocked 不会意外重启服务
- **PR-7 stub 不 shell out**：让 reviewer-flow E2E 可在无 systemd 的 dev box / CI 运行；真实 systemctl 需 root + dbus + 真实失败服务——超出 SOP gating PR 范围
- **`restartHandlerTimeout=10s`**：mock 微秒级完成；真实 systemctl 需评估（某些服务重启可能超 10s）
- **allowlist 是单租户 dev box 设定**：8 个 unit 是小爆炸半径，可恢复；多租户 / 生产应通过 `SandboxConfig` 直接构造收紧
- **无审计日志写入持久存储**：仅 `log.Info`；审计轨迹在 manager 侧（reviewer approval + BaseTool dispatch）
- **unit 名规范化不验证存在性**：`nginx` 在 allowlist 但主机未安装 nginx 时，stub 仍返回成功；真实 systemctl 应在 shell out 失败时返回 ExitCode + stderr
- **`AllowedUnits` 是 `[]string` 而非 `map`**：`ensure()` lazy 转换；多次 `Allows` 调用共用同一 map
- **未来真实 systemctl 实现路径**：(1) `exec.CommandContext("systemctl", "restart", canonical)`；(2) 捕获 ExitError 返回 ExitCode + stderr；(3) 保留 Mocked 标志用于 CI / 无 systemd 环境
- **`RestartServiceResponse.Mocked=true` 是 wire 契约**：manager / UI 应根据此字段渲染「mocked」徽章；未来真实实现时改为 false
