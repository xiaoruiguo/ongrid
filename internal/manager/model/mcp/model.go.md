# `model.go` 技术实现文档

> 源文件：`internal/manager/model/mcp/model.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/model/mcp`

## 1. 概述

本文件定义 `Server` 实体：外部 MCP（Model Context Protocol）server 注册（HLD-018）。每行 = 一个 ongrid 作为 *client* 连接的上游 MCP server，包含 transport + endpoint（或 stdio command）、可选 credential-vault 引用 + header template 用于 auth 注入、trust/enable 开关、上次 probe 的 tools 缓存快照。设计要点：model 字段无 json tag（PascalCase 序列化，前端 remap）；敏感 auth 不存明文，仅存 credential NAME，biz/mcp 在 connect 时从 vault 解析。红线：`Trusted=false` 时 tool 调用需人工审批门（HLD-018）；stdio transport 本 phase 不支持。

## 2. 包信息

- **包名**：`mcp`
- **所属模块**：`internal/manager/model/`
- **依赖方向**：被 `manager/biz/mcp` 调用；依赖 `time`

## 3. 关键类型与接口

```go
type Server struct {
    ID uint64 `gorm:"primaryKey;autoIncrement"`

    // Name 是唯一标签 + tool 名前缀（如 "github" → "github__create_issue"）
    Name string `gorm:"size:64;not null;uniqueIndex"`

    // Transport: "http"（Streamable HTTP）或 "stdio"（subprocess，本 phase 不支持）
    Transport string `gorm:"size:16;not null;default:http"`

    // Endpoint 是 HTTP MCP URL（transport=http）
    Endpoint string `gorm:"size:512"`

    // Command + ArgsJSON 描述 stdio 子进程（transport=stdio；本 phase 可空）
    Command  string `gorm:"size:512"`
    ArgsJSON string `gorm:"type:text"` // JSON array of strings

    // Credential 是 vault NAME；空 = 无 auth 注入
    Credential string `gorm:"size:128"`

    // HeaderTemplateJSON 是 JSON map[string]string，含 {{field}} 占位符
    HeaderTemplateJSON string `gorm:"type:text"`

    // Trusted 跳过人工审批门；Enabled 切换 server in/out of live toolbag
    Trusted bool `gorm:"not null;default:false"`
    Enabled bool `gorm:"not null;default:true"`

    // ToolsCacheJSON 是 []mcpclient.Tool 快照；Status / LastError 记录 probe 结果
    ToolsCacheJSON string `gorm:"type:text"`
    Status         string `gorm:"size:16"`
    LastError      string `gorm:"size:512"`

    CreatedBy uint64    `gorm:"not null;default:0"`
    CreatedAt time.Time `gorm:"autoCreateTime"`
    UpdatedAt time.Time `gorm:"autoUpdateTime"`
}
```

## 4. 关键函数与流程

### `Server.TableName`
- **签名**：`func (Server) TableName() string`
- **职责**：固定表名 `mcp_servers`，跨未来包重命名保持 schema 名稳定

## 5. 依赖关系

- **内部包**：`secret` 包（通过 Credential 反查 vault）；`mcpclient` 包（ToolsCacheJSON 是其 Tool 切片）
- **外部库**：`time`
- **被调用方**：`manager/biz/mcp` 的 connect / probe / tool dispatcher；live toolbag 注入逻辑

## 6. 并发与资源管理

- 本文件仅定义 schema，无锁 / channel / 缓存
- 无软删（permanent audit）；如需删除由 biz 层显式 DELETE
- `Enabled` 切换不删行，仅控制 toolbag 注入

## 7. 设计模式与亮点

- **遵循 InstalledPack 约定**：model 字段无 json tag，PascalCase 序列化，前端 remap
- **Name = tool 名前缀**：避免不同 server tool 名冲突；如 "github__create_issue"
- **Transport 二选一**：http（Streamable HTTP）支持；stdio 本 phase 仅 schema 预留
- **Credential 仅存 NAME**：敏感 auth 不入明文；biz/mcp 在 connect 时从 vault 解析
- **HeaderTemplateJSON 占位符**：`{{field}}` 从 credential fields 解析；如 `{"Authorization":"Bearer {{token}}"}`
- **Trusted 跳过人工审批门**：HLD-018；`false` 时 tool 调用走 propose-confirm
- **Enabled 切换 toolbag**：不删行；离线维护时切 false
- **ToolsCacheJSON probe 快照**：上次成功 probe 的 tools；UI 显示免重 probe
- **Status / LastError probe 状态**：UI 显示 server 健康度
- **CreatedBy 审计**：注册者 user_id；0 = anonymous（仅测试）

## 8. 注意事项

- **Name 唯一**：跨所有行；与 builtin tool 名冲突时由 biz 处理
- **Transport 默认 http**：stdio 需显式写；本 phase stdio 不支持
- **Endpoint 必填（transport=http）**：transport=stdio 时可空
- **Command / ArgsJSON（transport=stdio）**：本 phase 可空；ArgsJSON 是 JSON array
- **Credential 可空**：空 = 无 auth 注入；公开 MCP server 不需
- **HeaderTemplateJSON 可空**：无 auth header 时为空
- **Trusted 默认 false**：安全默认；operator 显式 trust 才跳过审批
- **Enabled 默认 true**：注册即可用；operator 可临时禁用
- **ToolsCacheJSON 可空**：未 probe 时为空
- **Status / LastError 可空**：未 probe 时为空
- **CreatedBy 0 = anonymous**：生产路径必填 user_id
- **无软删**：permanent audit；删除由 biz 层显式 DELETE
- **stdio transport 未实现**：本 phase 仅 schema 预留；biz 层应拒绝 transport=stdio
