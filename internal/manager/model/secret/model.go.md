# `model.go` 技术实现文档

> 源文件：`internal/manager/model/secret/model.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/model/secret`

## 1. 概述

本文件定义 `Secret` 实体：credential vault（HLD-017）。沿用 n8n 模型——stored credential 是 NAMED、多 FIELD 实例（如 "tencent-prod" → `{secret_id, secret_key, region}`），at-rest 加密。consuming skill / 外部 MCP server 在 manifest 中声明每个 field 注入位置（env var / file via `{{field}}` 占位符）；per-skill binding 选 WHICH instance 填 slot。ongrid 永不 hard-code credential 语义——manifest 拥有注入映射，binding 拥有选择。红线：at-rest 用 `pkg/secretbox`（AES-256-GCM，key from ONGRID_SECRET_KEY）密封后才入库，DB dump 单独不能还原明文；`Data` 列永不通过 read API 返回明文。

## 2. 包信息

- **包名**：`secret`
- **所属模块**：`internal/manager/model/`
- **依赖方向**：被 `manager/biz/secret` 与 `manager/biz/mcp` / `manager/biz/marketplace`（通过 BindingsJSON）调用；依赖 `time`

## 3. 关键类型与接口

```go
type Secret struct {
    ID uint64 `gorm:"primaryKey;autoIncrement"`

    // Name 是 binding 层引用的唯一人类标签（如 "tencent-prod"、"github-bot"）
    // 不再是 env var name — manifest 映射此实例的 fields 到 env vars
    Name string `gorm:"size:128;not null;uniqueIndex"`

    // Type 是 credential 类型名（biz/secret.CredType，如 "tencentcloud" / "aws" / "custom"）
    // 驱动 create-form fields 与默认注入规则；空 = 视为 "custom"
    Type string `gorm:"size:64"`

    // Data 是 AES-GCM-sealed JSON object of credential fields（map[string]string）
    // 复用原 `value` 列避免单值时代的 schema 变更；NEVER 由 read API 返回
    Data string `gorm:"column:value;type:text;not null"`

    // Description 是可选人类备注
    Description string `gorm:"size:512"`

    CreatedAt time.Time `gorm:"autoCreateTime"`
    UpdatedAt time.Time `gorm:"autoUpdateTime"`
}
```

## 4. 关键函数与流程

### `Secret.TableName`
- **签名**：`func (Secret) TableName() string`
- **职责**：固定表名 `secrets`，跨未来包重命名保持 schema 名稳定

## 5. 依赖关系

- **内部包**：`pkg/secretbox`（AES-256-GCM 加密 / 解密）；`mcp` 包（通过 Credential 字段反查）；`marketplace` 包（通过 BindingsJSON 反查 slot → name）
- **外部库**：`time`
- **被调用方**：`manager/biz/secret` 的 CRUD service；`manager/biz/mcp` 在 connect 时解析；`manager/biz/marketplace` 在 skill exec 时解析 binding

## 6. 并发与资源管理

- 本文件仅定义 schema，无锁 / channel / 缓存
- 无软删（permanent audit）；如需删除由 biz 层显式 DELETE
- `Data` 列加密前后的并发由 biz 层保证

## 7. 设计模式与亮点

- **沿用 n8n 模型**：NAMED、多 FIELD 实例；非 env var name
- **manifest 拥有注入映射**：ongrid 不 hard-code credential 语义；env var / file via `{{field}}` 占位符由 manifest 定义
- **binding 拥有选择**：per-skill binding 选 WHICH instance 填哪个 slot
- **Data AES-GCM 加密**：`pkg/secretbox` 密封 JSON 后才入库；key from ONGRID_SECRET_KEY；DB dump 单独不能还原明文
- **复用 `value` 列名**：从单值时代升级到多 field map 时无 schema 变更
- **Name 唯一索引**：跨所有行；binding 引用稳定
- **Type 驱动 create-form**：UI 按 Type 显示字段；biz 层校验
- **Description 可选**：人类备注；不影响功能
- **NEVER 返回 Data 明文**：read API 仅返回 Name / Type / Description；Data 列永不暴露

## 8. 注意事项

- **Data 列名是 `value`**：legacy 列名；存储的是 AES-GCM-sealed JSON map
- **Name 唯一**：跨所有行；rename 需先删后建
- **Type 可空**：空 = "custom"
- **Data 必填**：写入前必须经 `pkg/secretbox` 加密
- **Description 可空**：可选人类备注
- **无软删**：permanent audit；删除由 biz 层显式 DELETE
- **NEVER 返回 Data 明文**：read API 仅返回 metadata；Data 解密仅 biz 层在 skill exec / MCP connect 时用
- **ONGRID_SECRET_KEY 管理**：key 旋转需 re-encrypt 所有 Data；本包不实现
- **Binding 引用 stability**：rename Secret 需同步更新所有 binding 的 slot → name 映射
- **MCP Credential 字段**：mcp.Server.Credential 存的是 Name 而非 ID
- **marketplace BindingsJSON**：`{slot: credential_name}` 反查此表
