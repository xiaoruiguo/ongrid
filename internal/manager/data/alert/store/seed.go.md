# `seed.go` 技术实现文档

> 源文件：`internal/manager/data/alert/store/seed.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/alert/store`

## 1. 概述

本文件实现 `SeedChannelsFromConfig`，每次启动把 env 配置中的 notification channel（webhook / slack / feishu / dingtalk）同步到 DB。设计要点：disabled channel 不删而是 upsert enabled=false，保留历史 delivery 的 channel_id FK；factory default 不 seed 空占位符，避免 fresh install 出现四条 disabled+empty 行产生 no-op "notification_sent" 噪音；每 boot 软删 legacy "log" 类型 channel。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/alert`
- **依赖方向**：被 `cmd/ongrid` 启动调用；依赖 `internal/manager/model/alert`、`internal/pkg/config`、`gorm.io/gorm`（间接）。

## 3. 关键类型与接口

```go
// 内部辅助类型，不导出
type seedCandidate struct {
    Name    string
    Type    string
    Enabled bool
    URL     string
    Secret  string
}
```

## 4. 关键函数与流程

### `SeedChannelsFromConfig`
- **签名**：`func SeedChannelsFromConfig(ctx, repo *Repo, cfg config.NotificationConfig) error`
- **职责**：每 boot 把 env 配置的 channel 同步到 DB，幂等。
- **流程**：
  1. `repo == nil` → 返回 nil（防御性）
  2. 构造 4 个 candidate：webhook / slack / feishu / dingtalk，分别从 cfg 取 Name / Type / Enabled / URL / Secret
  3. 遍历 candidate：
     - `Name == ""` → 跳过
     - `!Enabled && URL 为空` → 跳过（factory default 不 seed 空占位符）
     - 构造 `model.Channel`，`ConfigJSON` 由 `encodeChannelConfig` 生成
     - `repo.UpsertChannelByName` 插入或更新
  4. `repo.PurgeLegacyLogChannels` 软删 legacy "log" 类型 channel
- **错误处理**：每步错误用 `%w` 包装上抛。

### `encodeChannelConfig`
- **签名**：`func encodeChannelConfig(url, secret string) string`
- **职责**：把 url + secret 编码为 JSON 字符串。secret 不直接存值，而是存 `{"secret_set": "true"}` 标记，operator 调试时单列可查。空 url + 空 secret → `"{}"`。
- **流程**：构造 map → `json.Marshal`，失败回退 `"{}"`。

## 5. 依赖关系

- **内部包**：`internal/manager/model/alert`、`internal/pkg/config`
- **外部库**：`encoding/json`、`fmt`、`strings`
- **被调用方**：`cmd/ongrid` 启动序列
- **依赖方法**：`repo.UpsertChannelByName`、`repo.PurgeLegacyLogChannels`

## 6. 并发与资源管理

- **无锁**：启动期串行执行。
- **幂等**：每 boot 重写 config_json + enabled；新 name 插入，cfg 中不存在的 name 不动（config-managed 与未来 UI-managed 行共存）。

## 7. 设计模式与亮点

- **factory default 不 seed 空占位符**：注释明示避免 fresh install 出现四条 disabled+empty 行，operator 从 UI 按需添加。
- **secret 不存值存标记**：`{"secret_set": "true"}` 让 operator 单列调试，避免 secret 明文落 DB 列。
- **disabled 不删**：upsert enabled=false 保留历史 delivery 的 channel_id FK。
- **legacy log channel 软删**：pinned rule 回退全局默认 channel 集，软删保留 delivery 审计。
- **idempotent**：每 boot 安全反复执行。

## 8. 注意事项

- **secret 不存明文**：`encodeChannelConfig` 仅存 `secret_set: true` 标记；真实 secret 由 notify sender 在运行时从 cfg 或 secret store 取。
- **config-managed 与 UI-managed 共存**：cfg 中不存在的 name 不动，未来 PR-D UI-managed 行可共存。
- **UpsertChannelByName 行为**：见 `repo.go` 文档；存在则 Updates 三列（channel_type / enabled / config_json）。
- **PurgeLegacyLogChannels 幂等**：每 boot 软删，二次启动匹配空集。
