# `repo.go` 技术实现文档

> 源文件：`internal/manager/biz/edge/repo.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/edge`

## 1. 概述

本文件定义 `Repo` interface —— manager/edge 持久化契约 + `ListFilter` 过滤器。实现位于 `internal/manager/data/edge/store`。Post-pivot 后无 org_id 参数。覆盖 edge 行的 CRUD + AccessKey 查找 + SecretHash 更新 + status/last_seen 翻转 + DeviceID 关联（convenience pointer）+ AgentVersion 记录。

## 2. 包信息

- **包名**：`edge`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 `Usecase`（同包）/ `AccessKeyAuthenticator`（authn.go）调用；依赖 `model/edge`

## 3. 关键类型与接口

```go
type ListFilter struct {
    Status    string   // 精确匹配 ""/online/offline
    Name      string   // 子串 LIKE %name%
    CreatedBy *uint64  // 非 nil 时限定创建者
    Limit, Offset int
}

type Repo interface {
    Create(ctx, e *model.Edge) error
    GetByID(ctx, id uint64) (*model.Edge, error)
    GetByAccessKey(ctx, accessKey string) (*model.Edge, error)
    GetByName(ctx, name string) (*model.Edge, error)
    List(ctx, f ListFilter) ([]*model.Edge, error)
    UpdateSecretHash(ctx, id uint64, hash string) error
    UpdateStatus(ctx, id uint64, status string, lastSeen time.Time) error
    UpdateName(ctx, id uint64, name string) error
    SetDeviceID(ctx, edgeID, deviceID uint64) error
    ClearDeviceID(ctx, edgeID uint64) error
    SetAgentVersion(ctx, id uint64, version string) error
    Delete(ctx, id uint64) error  // 软删
    Count(ctx) (int64, error)
}
```

## 4. 关键函数与流程

### `Create`
- **签名**：`Create(ctx, e *model.Edge) error`
- **职责**：插入新 edge 行；e.ID 由实现回填

### `GetByID / GetByAccessKey / GetByName`
- **签名**：单行查找
- **职责**：GetByID 按 PK；GetByAccessKey 按 AccessKeyID（authn 路径用）；GetByName 按 display name（aiops tool registry 解析 edge_name → ID 用）
- **错误处理**：未找到 → ErrNotFound（由实现定义）

### `List`
- **签名**：`List(ctx, f ListFilter) ([]*model.Edge, error)`
- **职责**：按 f 过滤；Status 精确；Name 子串；CreatedBy 精确；Limit/Offset 分页

### `UpdateSecretHash`
- **签名**：`UpdateSecretHash(ctx, id uint64, hash string) error`
- **职责**：替换 argon2id hash（RotateSecret 用）；旧 secret 立即失效（运行中 tunnel session 不受影响，geminio 仅握手校验）

### `UpdateStatus`
- **签名**：`UpdateStatus(ctx, id uint64, status string, lastSeen time.Time) error`
- **职责**：翻转 status（online/offline）+ 更新 last_seen_at；authn 握手成功后异步调用；HandleHeartbeat / HandleOffline 也用

### `UpdateName`
- **签名**：`UpdateName(ctx, id uint64, name string) error`
- **职责**：覆盖 display name；HandleRegister 用首次 hostname 回填空 name

### `SetDeviceID / ClearDeviceID`
- **签名**：edge→device convenience pointer
- **职责**：
  - SetDeviceID：edge 行的 DeviceID 字段指向 host Device；junction 真源在 edge_devices 表，此字段是 convenience
  - ClearDeviceID：清 convenience pointer；非 host edge role（如 Kubernetes controller）自愈旧误注册时用

### `SetAgentVersion`
- **签名**：`SetAgentVersion(ctx, id uint64, version string) error`
- **职责**：记录 agent 自报二进制版本（semver-ish，如 "0.7.43"）；register_edge 时值变化才写

### `Delete`
- **签名**：`Delete(ctx, id uint64) error`
- **职责**：软删 edge 行

### `Count`
- **签名**：`Count(ctx) (int64, error)`
- **职责**：总非软删 edge 数

## 5. 依赖关系

- **内部包**：`model/edge`
- **被调用方**：`Usecase`（CRUD + register/heartbeat/offline 流程）、`AccessKeyAuthenticator`（GetByAccessKey + UpdateStatus）
- **实现方**：`internal/manager/data/edge/store`

## 6. 并发与资源管理

- **纯接口**：无共享状态；并发安全由实现负责
- **ctx 透传**：所有方法第一参 context

## 7. 设计模式与亮点

- **DeviceID 是 convenience pointer**：junction 真源在 edge_devices 表；此字段避免每次查询都 JOIN；SetDeviceID/ClearDeviceID 保持同步
- **GetByAccessKey 是 authn 专用**：与 GetByID 分离；authn 路径清晰
- **GetByName 支撑 aiops tool**：aiops tool registry 解析 edge_name → ID；与 GetByID 分离
- **UpdateName 支撑首次注册回填**：HandleRegister 在 admin 留空 name 时用 hostname 回填
- **SetAgentVersion 仅值变化才写**：调用方（HandleRegister）负责比对；避免无意义 UPDATE
- **ListFilter 简单**：Status 精确 / Name 子串 / CreatedBy 精确；无复杂组合
- **软删**：Delete 软删；Count/List 排除软删行

## 8. 注意事项

- **DeviceID 非 junction 真源**：junction 在 `edge_devices` 表（`EdgeDeviceRepo`）；SetDeviceID 是 convenience 同步
- **GetByAccessKey 失败不区分**：authn 路径所有失败折叠 ErrUnauthorized（authn.go）
- **UpdateSecretHash 旧 session 不踢**：geminio 仅握手校验；运行中 session 继续有效；runbook 应指导操作员 kick edge
- **SetAgentVersion 空值不写**：调用方 HandleRegister 负责过滤空值，避免 blank 最后已知好版本
- **软删不清 junction**：Delete 仅软删 edge 行；junction 清理由 edge biz Delete 流程负责
- **ListFilter.CreatedBy nil 表示不限**：非 nil 时精确匹配
- **Count 排除软删**：与 List 一致
