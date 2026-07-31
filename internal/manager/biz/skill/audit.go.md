# audit.go 技术实现文档

## 1. 概述

`audit.go` 实现技能执行的审计日志持久化。每次 `Service.Execute` 调用都会写一行 `SkillExecution` 记录到 `skill_executions` 表，用于合规审计与回放。当前处于 PR-G1 scope——仅记录基础字段（skill_key / edge_id / caller / params / result / error / 时间戳），PR-G4 落地 SOP 签名后会扩展 `signed_by` / `signature` / `sop_id` 等签名材料。

## 2. 包信息

- 包名：`skill`
- 路径：`internal/manager/biz/skill/audit.go`
- 导入依赖：
  - 标准库：`context` / `encoding/json` / `time`
  - 第三方：`gorm.io/gorm`
  - 内部包：`github.com/ongridio/ongrid/internal/skill`（别名 `skillcore`）

## 3. 关键类型与接口

### `SkillExecution`（GORM 模型）

```go
type SkillExecution struct {
    ID         uint64          `gorm:"column:id;primaryKey;autoIncrement"`
    SkillKey   string          `gorm:"column:skill_key;size:128;not null;index:idx_skill_executions_key"`
    EdgeID     uint64          `gorm:"column:edge_id;not null;index:idx_skill_executions_edge"`
    CallerID   uint64          `gorm:"column:caller_id;not null"`
    CallerRole string          `gorm:"column:caller_role;size:16;not null"`
    Class      skillcore.Class `gorm:"column:class;size:16;not null"`
    ParamsJSON string          `gorm:"column:params_json;type:text;not null"`
    ResultJSON string          `gorm:"column:result_json;type:text"`
    Error      string          `gorm:"column:error;type:text"`
    StartedAt  time.Time       `gorm:"column:started_at;not null;index:idx_skill_executions_started"`
    FinishedAt time.Time       `gorm:"column:finished_at;not null"`
    CreatedAt  time.Time       `gorm:"column:created_at;autoCreateTime"`
}
```

表名固定为 `skill_executions`（`TableName()` 方法）。

索引设计：

- `idx_skill_executions_key` on `skill_key`：按技能查询历史
- `idx_skill_executions_edge` on `edge_id`：按设备查询执行历史
- `idx_skill_executions_started` on `started_at`：按时间范围查询

### `GormAuditSink`

```go
type GormAuditSink struct{ db *gorm.DB }
```

实现 `AuditSink` 接口（声明于 `service.go`）。`service.go` 的 `Service.audit` 字段为 `AuditSink` 类型，nil sink 用于测试时禁用审计。

## 4. 关键函数与流程

### `NewGormAuditSink`

```go
func NewGormAuditSink(db *gorm.DB) *GormAuditSink { return &GormAuditSink{db: db} }
```

### `Migrate`

```go
func Migrate(db *gorm.DB) error {
    return db.AutoMigrate(&SkillExecution{})
}
```

由 `cmd/ongrid main` 在启动时与其他 manager BC 迁移一并调用。GORM AutoMigrate 创建/补齐表结构，但不删除列。

### `Record`

```go
func (g *GormAuditSink) Record(ctx context.Context, ev AuditEvent) error {
    row := SkillExecution{
        SkillKey:   ev.SkillKey,
        EdgeID:     ev.EdgeID,
        CallerID:   ev.CallerID,
        CallerRole: ev.CallerRole,
        Class:      ev.Class,
        ParamsJSON: jsonOrEmpty(ev.Params),
        ResultJSON: jsonOrEmpty(ev.Result),
        Error:      ev.Error,
        StartedAt:  ev.StartedAt,
        FinishedAt: ev.FinishedAt,
    }
    return g.db.WithContext(ctx).Create(&row).Error
}
```

`AuditEvent` → `SkillExecution` 的字段映射。`db.WithContext(ctx)` 传递 context 以支持取消/超时。`Create` 失败时返回 error 给调用方。

### `jsonOrEmpty`

```go
func jsonOrEmpty(b json.RawMessage) string {
    if len(b) == 0 { return "" }
    return string(b)
}
```

`json.RawMessage` 为 nil/空时返回空串，避免 `string(nil)` 的 `""` 与 `string([]byte{})` 的 `""` 在 TEXT 列中产生歧义。

## 5. 依赖关系

- **`gorm.DB`**：唯一持久化依赖
- **`skillcore.Class`**：技能分类枚举（safe / mutating / dangerous）
- **`AuditEvent`**（来自 `service.go`）：DTO，承载调用方上下文（CallerID / CallerRole）、执行上下文（SkillKey / EdgeID / Class / Params）、结果（Result / Error / 时间戳）
- **被依赖方**：`Service.Execute` 调用 `audit.Record`，失败时降级为 warn 日志

## 6. 并发与资源管理

- `GormAuditSink` 构造后只读，无共享可变状态
- `gorm.DB` 自身是并发安全的连接池
- `Record` 是同步阻塞调用——`Service.Execute` 在返回前等待审计写入完成。这是刻意的：异步写入虽能降低延迟，但若进程在异步队列堆积时崩溃，会丢失审计记录，违背"合规审计必须可追溯"的语义
- `db.WithContext(ctx)` 让审计写入受调用方 context 约束——若 HTTP 请求被取消，审计写入也会被取消

## 7. 设计模式与亮点

### 审计与执行的强绑定

`Service.Execute` 在调用 `audit.Record` 后才返回，保证"成功执行的技能必有审计行"。若审计失败，`Execute` 不会失败——而是降级为 `s.log.Warn`。这种"审计失败不阻塞业务成功"是合规与可用性的折衷：审计是事后追溯，不应让一次 DB 抖动毁掉一次成功的技能调用。

### AuditSink 接口的可测试性

`AuditSink` 是接口而非具体类型，`Service.New` 接受 `nil` sink 用于测试。这让单元测试无需 DB 即可验证 `Execute` 的核心逻辑。

### GORM AutoMigrate 的渐进式 schema

`Migrate` 用 AutoMigrate，新增列时自动补齐，但**不删除列**。这为 PR-G4 添加 `signed_by` / `signature` / `sop_id` 提供了平滑路径——只需在 `SkillExecution` 结构上加字段并重新启动，AutoMigrate 会补列。

### 索引覆盖典型查询

三个索引（`skill_key` / `edge_id` / `started_at`）覆盖了"按技能/按设备/按时间"三种典型审计查询场景，无需复合索引——单列索引足以支撑等值查询 + 排序。

### `json.RawMessage` → string 的简洁序列化

`ParamsJSON` / `ResultJSON` 存原始 JSON 文本而非结构化字段。这让审计表对技能 schema 变化免疫——技能的 params/result 结构演化不会要求审计表迁移。

## 8. 注意事项

- **PR-G4 的扩展点**：注释明确"PR-G4 will add signature material (signed_by, signature, sop_id) when SOPs land"。新增字段时需同步更新 `AuditEvent` → `SkillExecution` 的字段映射，并考虑已存在行的默认值
- **审计失败的降级**：`Service.Execute` 将 `Record` 失败降级为 warn log。这意味着审计表可能有"缺失行"——合规审计时若发现某次执行无记录，需查 manager 日志确认是否为审计写入失败
- **`db.WithContext(ctx)` 的取消语义**：若 HTTP 请求取消导致 ctx 取消，`Record` 可能写入失败。这是可接受的——技能本身可能也被取消，审计缺失与执行取消一致
- **TEXT 列的存储成本**：`ParamsJSON` / `ResultJSON` 用 TEXT 类型。大 payload（如 web_search 返回 1MB 结果）会让单行变大——若成为瓶颈，考虑截断或单独的 result blob 表
- **无 `UpdatedBy` 字段**：审计行一旦写入不可变，符合审计语义。但若需"管理员手动修正错误记录"的能力，当前 schema 不支持
- **时区**：`StartedAt` / `FinishedAt` 由 `Service.Execute` 用 `time.Now().UTC()` 写入，统一 UTC；查询时由前端转本地时区
