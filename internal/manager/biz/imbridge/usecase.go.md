# `usecase.go` 技术实现文档

> 源文件：`internal/manager/biz/imbridge/usecase.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/imbridge`

## 1. 概述

本文件是 IM 桥接器的管理用例：ImApp CRUD + 输入校验 + allowlist 解析。设计要点：Telegram/Slack 强制 stream 模式 + 非空 allowlist（bot 公开可发现，空 allowlist 会让任何人命令 agent，ADR-031）；webhook 模式强制 encrypt_key（签名/加密事件）；ParseAllowFrom 共享解析规则，单一来源。红线：Telegram user_id 必须 numeric，Slack user_id 必须 U/W 前缀，typo 在 validate 阶段就拒。

## 2. 包信息

- **包名**：`imbridge`
- **所属模块**：`internal/manager/biz/imbridge`
- **依赖方向**：被 HTTP handler 调用；依赖 `model/imbridge`、`pkg/errs`

## 3. 关键类型与接口

```go
// AdminRepo 是 UC 需要的数据层（Repo 超集 + ImApp CRUD）
type AdminRepo interface {
    Repo
    ListApps(ctx context.Context, provider string) ([]*model.ImApp, error)
    GetApp(ctx context.Context, id uint64) (*model.ImApp, error)
    CreateApp(ctx context.Context, app *model.ImApp) error
    UpdateApp(ctx context.Context, app *model.ImApp) error
    DeleteApp(ctx context.Context, id uint64) error
}

type UC struct {
    repo AdminRepo
}

// AppInput 是变更 payload
type AppInput struct {
    Provider, Mode, Name, AppID, AppSecret, VerifyToken, EncryptKey, AllowFrom string
    Enabled       bool
    DefaultLocale string // "" | "en" | "zh"
}
```

## 4. 关键函数与流程

### `NewUC`
- **签名**：`func NewUC(repo AdminRepo) *UC`

### `ParseAllowFrom`
- **签名**：`func ParseAllowFrom(raw string) []string`
- **职责**：解析 allowlist（逗号/空格/换行/分号分隔），剥离 `telegram:`/`tg:` 前缀（OpenClaw 兼容），去重保序
- **流程**：
  1. `strings.FieldsFunc` 按分隔符切
  2. TrimSpace + 剥离前缀
  3. seen map 去重，保序
- **注释**：共享解析规则，validate 和 provider poll loop 共用；非 numeric/负数 token 在调用方过滤（Telegram numeric，Slack 字母前缀）

### `isNumericID`
- 仅 `0-9`，非空

### `AppInput.validate`
- **签名**：`func (in *AppInput) validate() error`
- **职责**：校验 + 规范化输入
- **流程**：
  1. provider ∈ {feishu, dingtalk, telegram, slack}
  2. DefaultLocale 规范化：剥离 region tag（en-US → en），∈ {en, zh} 或空（auto）
  3. mode 默认 stream，∈ {stream, webhook}
  4. AppID 非空，Name 非空
  5. webhook 模式强制 EncryptKey 非空
  6. **Telegram**：仅 stream 模式；`ParseAllowFrom` 后过滤 numeric ID；空 → error（注释：bot 公开可发现，空 allowlist 让任何人命令 agent，ADR-031）
  7. **Slack**：仅 stream 模式（Socket Mode，避免公网 ingress）；`ParseAllowFrom` 后过滤 U/W 前缀 ID；空 → error（注释：workspace 默认任何成员可发消息）
- **规范化**：Telegram/Slack 的 AllowFrom 用 `strings.Join(ids, ",")` 规范化存储形式

### `UC.ListApps / GetApp`
- 透传 repo

### `UC.CreateApp`
- **签名**：`func (uc *UC) CreateApp(ctx, in AppInput) (*model.ImApp, error)`
- **流程**：
  1. validate
  2. AppSecret TrimSpace 后非空校验
  3. 构造 ImApp，CreatedAt/UpdatedAt = now.UTC()
  4. `repo.CreateApp`
- **错误处理**：`fmt.Errorf("create im_app: %w", err)`

### `UC.UpdateApp`
- **签名**：`func (uc *UC) UpdateApp(ctx, id uint64, in AppInput) (*model.ImApp, error)`
- **流程**：
  1. validate
  2. `repo.GetApp` 取当前
  3. 更新字段；**AppSecret 空 = 保持当前**（编辑表单不需重传 secret）
  4. UpdatedAt = now.UTC()
  5. `repo.UpdateApp`
- **注释**：空 AppSecret 保留旧值，避免编辑其他字段时丢失 secret

### `UC.DeleteApp`
- 透传 repo

## 5. 依赖关系

- **内部包**：`model/imbridge`（ImApp/Provider/Mode 常量）、`pkg/errs`（ErrInvalid 等）
- **外部库**：仅标准库
- **被调用方**：HTTP handler（IM app 管理 API）

## 6. 并发与资源管理

- **UC 无状态**：仅持有 repo 引用，无共享状态
- **ctx 透传**：所有 repo 调用
- **无锁**：UC 本身无并发问题，repo 自管并发

## 7. 设计模式与亮点

- **ParseAllowFrom 单一来源**：validate 和 provider poll loop 共用，规则只有一处定义
- **provider 特化校验**：Telegram numeric ID / Slack U/W 前缀在 validate 阶段过滤，UI 拿到清晰错误
- **AllowFrom 规范化存储**：`strings.Join(ids, ",")` 统一存储形式
- **空 AppSecret 保留旧值**：编辑表单不重传 secret，避免丢失
- **DefaultLocale region 剥离**：en-US → en，防 typo 静默降级
- **webhook 强制 encrypt_key**：签名/加密事件必需
- **stream 模式优先**：Telegram/Slack 仅 stream（出向调用，代理友好，避免公网 ingress）

## 8. 注意事项

- **Telegram/Slack 仅 stream**：webhook 需公网 ingress，mainland-China 不可靠
- **allowlist 强制**：ADR-031，bot 公开可发现，空 allowlist 安全风险
- **ParseAllowFrom 不过滤 ID 格式**：调用方按 provider 过滤（Telegram numeric，Slack U/W 前缀）
- **DefaultLocale 三态**：空=auto（LLM 跟随用户），en/zh=强制
- **AppSecret 编辑语义**：空=保留旧值，非空=替换
- **CreatedAt 不可变**：UpdateApp 不改 CreatedAt
- **OpenClaw 兼容**：`telegram:`/`tg:` 前缀剥离
