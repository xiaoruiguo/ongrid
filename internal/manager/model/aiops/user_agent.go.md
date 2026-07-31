# `user_agent.go` 技术实现文档

> 源文件：`internal/manager/model/aiops/user_agent.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/model/aiops`

## 1. 概述

本文件定义 `UserAgent` 实体：用户通过 UI 创建的 persona（Agent-Assistant 计划 Phase 3）。镜像 `chatruntime.Agent` 足够字段（name / description / when_to_use / system_prompt / 工具白/黑名单 JSON），让 registry 启动时 + 每次 CRUD 后能从该行 hydrate 出 Agent 而无需回盘读取。Phase 3 单租户上线，UserID 为创建者；SaaS 落地后同列复用为 org_id。

## 2. 包信息

- **包名**：`aiops`（与 model.go 同包）
- **所属模块**：`internal/manager/model/`
- **依赖方向**：被 `manager/biz/aiops` 下的 user-agent service / registry 调用；依赖 `time`

## 3. 关键类型与接口

```go
type UserAgent struct {
    ID                  uint64    `gorm:"primaryKey;autoIncrement"`
    UserID              uint64    `gorm:"not null;default:0;index"`
    Name                string    `gorm:"size:128;not null;uniqueIndex:idx_user_agent_name"`
    Description         string    `gorm:"size:512;not null"`
    WhenToUse           string    `gorm:"type:text;column:when_to_use"`
    SystemPrompt        string    `gorm:"type:text;column:system_prompt"`
    CriticalReminder    string    `gorm:"type:text;column:critical_reminder"`
    AllowedToolsJSON    string    `gorm:"type:text;column:allowed_tools_json"`
    DisallowedToolsJSON string    `gorm:"type:text;column:disallowed_tools_json"`
    PermissionMode      string    `gorm:"size:32;column:permission_mode"`
    Model               string    `gorm:"size:128;column:model"`
    MaxTurns            int       `gorm:"column:max_turns"`
    CreatedAt           time.Time
    UpdatedAt           time.Time
}
```

## 4. 关键函数与流程

### `UserAgent.TableName`
- **签名**：`func (UserAgent) TableName() string`
- **职责**：固定表名 `user_agents`

## 5. 依赖关系

- **内部包**：无
- **外部库**：`time`（仅标准库，不引入 uuid/gorm，因主键用 autoIncrement）
- **被调用方**：`manager/biz/aiops` 下 user-agent service 与 registry

## 6. 并发与资源管理

- 本文件仅定义 schema，无锁 / channel / 缓存
- 主键 autoIncrement，由 DB 分配

## 7. 设计模式与亮点

- **镜像 chatruntime.Agent**：让 registry 在 boot + 每次 CRUD 后 hydrate Agent 而无需读盘 markdown 文件
- **Name 唯一索引**：`idx_user_agent_name` 防重名；与系统 agent 共用一个命名空间
- **AllowedToolsJSON / DisallowedToolsJSON 双向白/黑名单**：JSON 序列化避免 schema 演进时迁移；registry 读取后重建 tool bag
- **CriticalReminder 字段**：在 system_prompt 之外附加高优先级提醒，LLM 调用前注入
- **PermissionMode 显式列**：未来 dual-sign / 自动 approve / read-only 三档控制
- **Model 字段**：让用户为 persona 指定模型（覆盖系统默认路由）
- **MaxTurns 防失控**：persona-level 上限，避免无界 agent loop
- **Phase 3 单租户立场**：所有 authed caller 共享可见性；显式注释说明"避免 tenant_bind plumbing"，待 SaaS 再补

## 8. 注意事项

- **UserID 0 = anonymous**：测试路径；生产必填 creator user_id
- **Name 唯一约束**：跨用户全局唯一；与 builtin agent name 冲突时由 biz 层处理
- **JSON 字段无 schema 校验**：biz 层负责 decode + 校验 tool 名称合法
- **PermissionMode 空值语义**：当前等价 read-only；新增模式需同步 registry 解释逻辑
- **MaxTurns = 0**：表示用系统默认；负数非法，biz 层应拒绝
- **跨用户可见性**：Phase 3 不强制隔离；SaaS 落地需补 tenant_bind
- **复用为 org_id**：列名 UserID 在 SaaS 阶段语义升级为 org_id，无 schema 变更
