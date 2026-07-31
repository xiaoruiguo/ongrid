# agent.go

## 1. 概述

`agent.go` 是 setting 包的 typed accessor —— `CategoryAgent` 行为开关的薄包装。镜像 telemetry/websearch readers：在 generic key/value `Service` 上烘焙默认值，让调用方不必重复 policy。

唯一方法 `AgentWriteEnabled` 报告 chat agent 是否可用 write/mutating tools。默认 DISABLED（fail-safe）：缺行或读错误都解析为 false，开箱即用 agent 是 read-only，admin 必须显式 opt-in。只有字面量 `"true"` enable。

## 2. 包信息

- 包名：`setting`
- 路径：`internal/manager/biz/setting`

## 3. 关键类型与接口

无导出类型。文件只给 `Service` 加一个方法 `AgentWriteEnabled`。

## 4. 关键函数与流程

### AgentWriteEnabled

```go
func (s *Service) AgentWriteEnabled(ctx context.Context) bool {
    v, found, err := s.Get(ctx, model.CategoryAgent, model.KeyAgentWriteEnabled)
    if err != nil || !found {
        return false
    }
    return v == "true"
}
```

逻辑：
1. `s.Get(ctx, CategoryAgent, KeyAgentWriteEnabled)` 取值
2. `err != nil || !found` → return false（fail-safe）
3. `v == "true"` → return true（仅字面量 "true" enable）

注释：gate 现在也 unlock `host_bash` 的 unrestricted（cmdpolicy-bypass）mode —— permissive default 会 ship full root command channel by default。只有字面 "true" enable。

## 5. 依赖关系

### 外部包

- `context`

### 内部包

- `model "github.com/ongridio/ongrid/internal/manager/model/setting"` —— `CategoryAgent` / `KeyAgentWriteEnabled`
- 同包：`Service`（在 `service.go` 定义）

### 被谁调用

- chatruntime 调 `AgentWriteEnabled` 决定是否允许 write/mutating tools
- `host_bash` 工具调它决定是否 unlock unrestricted mode

## 6. 并发与资源管理

不适用（单方法，委派 `Service.Get`，无共享状态）。

## 7. 设计模式与亮点

### Fail-safe 默认 DISABLED

注释：缺行或读错误 → false。开箱即用 agent 是 read-only，admin 必须显式 opt-in。这是安全设计 —— permissive default 会 ship full root command channel。

### 仅字面量 "true" enable

`v == "true"` 严格匹配。`"True"` / `"1"` / `"yes"` 都不 enable。防 UI 误传非布尔值意外 enable。

### 与 host_bash unrestricted 联动

注释：gate 现在也 unlock `host_bash` 的 unrestricted（cmdpolicy-bypass）mode。这让 `AgentWriteEnabled` 成为两个安全机制的单一开关 —— write tools + host_bash unrestricted。

### Typed accessor 模式

镜像 telemetry/websearch readers：在 generic key/value Service 上烘焙默认值，让调用方不必重复 policy。这是 setting 包的通用模式 —— 每个 typed accessor 文件负责一类 category。

### err || !found 都 false

`err != nil || !found` 都 return false。读错误不 panic、不返回 error，直接 false。这是 fail-safe 的具体实现 —— 任何异常都倾向安全侧。

## 8. 注意事项

- **默认 DISABLED 是安全关键**：改默认值会 ship root command channel。任何改动需安全 review
- **仅 `"true"` enable**：`"True"` / `"1"` / `"yes"` 不 enable。UI 应传字面 "true"
- **err 不返回**：`Get` 失败静默 false。调用方无法区分"未配置"与"读错误" —— 都视为 disabled
- **与 host_bash 联动**：unlock unrestricted mode 也靠此 gate。改此方法影响两个安全机制
- **`CategoryAgent` / `KeyAgentWriteEnabled` 是 model 常量**：改值需 migration
- **无 setter**：本文件只有 getter。setter 通过 generic `Service.Set` 或 admin UI
- **`AgentWriteEnabled` 是 bool 返回**：不返回 error。调用方无法知道读失败 —— 设计如此
