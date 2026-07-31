# `search.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/mentions/search.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/mentions`

## 1. 概述

本文件实现 SPA chat 输入框的 @-mention 搜索 biz facade：用户输入 `@<term>`，popover 调 `Searcher.Search` 查 devices / incidents / rules / log files（filename label values），返回 `Item` 列表供渲染；`Searcher.Resolve` 把结构化 `Mention` 引用水合为 markdown bullet 供 agent 内联到 system prompt。设计：read-only（不改平台状态）、只返回最小字段（id+label+subtitle）、nil-safe deps（无 Loki 仍可用 device/incident/rule）、单源失败不 blank 整个 popover、稳定类型顺序（device→incident→rule→file）。

## 2. 包信息

- **包名**：`mentions`
- **所属模块**：`internal/manager/biz/aiops/mentions`
- **依赖方向**：被 chat HTTP handler 调用（Search）+ chatruntime.MentionResolver 调用（Resolve）；依赖 device.Usecase / alert.Usecase / logquery.Client

## 3. 关键类型与接口

```go
type Type string
const (
    TypeDevice   Type = "device"
    TypeIncident Type = "incident"
    TypeRule     Type = "rule"
    TypeFile     Type = "file"
)

type Item struct {
    Type     Type   `json:"type"`
    ID       string `json:"id"`
    Label    string `json:"label"`
    Subtitle string `json:"subtitle,omitempty"`
}

type Mention struct {
    Type  Type   `json:"type"`
    ID    string `json:"id"`
    Label string `json:"label"`
}

type Searcher struct {
    devices   *device.Usecase
    alerts    *alert.Usecase
    logClient logQuerier
}

type logQuerier interface {
    LabelValues(ctx, name string, start, end time.Time) ([]string, error)
}

type Query struct {
    Term  string
    Filter Type  // 空 = 所有类型
    Limit int    // 0 → 10，上限 50
}

type LogQuerierAdapter = logquery.Client  // 类型别名，wiring 站点清晰
```

## 4. 关键函数与流程

### `New`
- **签名**：`func New(devices *device.Usecase, alerts *alert.Usecase, logClient logQuerier) *Searcher`
- **职责**：构造 Searcher，三个 dep 任一可 nil（对应类型返回空结果）

### `Search`
- **签名**：`func (s *Searcher) Search(ctx, q Query) ([]Item, error)`
- **职责**：派发 term 到各子搜索，稳定类型顺序返回
- **流程**：
  1. nil receiver → nil
  2. limit 规范化：0→10，>50→50
  3. term = `strings.TrimSpace(q.Term)`
  4. `run(t, fn)` helper：Filter 非空且 != t → skip；fn 失败或空 → skip（**单源失败不 blank popover**）
  5. 按顺序 run：TypeDevice → TypeIncident → TypeRule → TypeFile
  6. 返回 out（Filter 空时最多 4*limit，每类型 limit）
- **稳定顺序**：device → incident → rule → file，popover 渲染可预测

### `searchDevices`（内部）
- **流程**：
  1. devices nil → nil
  2. term 全数字 → `devices.Get(id)` 精确匹配（operator 常贴 device id）
  3. term 非空 → `List(Name: term)` + `List(Hostname: term)` 双查 union，capped limit
  4. term 空 → `List(Limit: limit)` 最近 N
- **去重**：`containsItem` 防 id match + name match 重复

### `searchIncidents`（内部）
- **流程**：
  1. alerts nil → nil
  2. term 全数字 → `GetIncident(id)`
  3. `ListIncidents(RuleKey: term, Limit)`——**MVP 限制**：IncidentFilter 仅 exact RuleKey，substring 需用户完整粘贴
  4. term 空 → 最近 incidents 补足 limit
- **去重**：containsItem

### `searchRules`（内部）
- **流程**：
  1. alerts nil → nil
  2. `ListRules("")` 全量拉取（无 server-side filter）
  3. 客户端 `strings.Contains` 过滤 RuleKey / Name（lowercased）
  4. capped limit

### `searchFiles`（内部）
- **流程**：
  1. logClient nil → nil
  2. `to = now; from = to - 24h`（最近 24h 足够）
  3. `logClient.LabelValues(ctx, "filename", from, to)` 拉 filename label values
  4. 客户端 `strings.Contains` 过滤
  5. 每个 value → `Item{Type: TypeFile, ID: v, Label: v, Subtitle: "log file"}`

### `Resolve`
- **签名**：`func (s *Searcher) Resolve(ctx, mentions []Mention) []string`
- **职责**：把 Mention 引用水合为 markdown bullet（agent 内联到 system prompt）
- **流程**：每个 mention 调 `resolveOne`，非空 bullet 收集到 out
- **失败降级**：删除的 device 不应 kill agent run——resolveOne 失败返回空字符串

### `resolveOne`（内部）
- **职责**：单个 Mention 水合
- **分支**：
  - TypeDevice：`devices.Get(id)` → `"- device #N name (online/offline, hostname=X, ip=Y)"`
  - TypeIncident：`alerts.GetIncident(id)` → `"- incident #N rule=X severity=Y status=Z title=Q"`
  - TypeRule：`ListRules` + 遍历匹配 id/rule_key → `"- rule key (name) severity=X enabled=Y"`
  - TypeFile：无水合，filename 自描述 → `"- log file X"`
  - fallback：`"- type label (id)"`
- **失败 break**：任一步骤失败跳出 switch，走 fallback 或空字符串

### 辅助函数
- `deviceToItem` / `incidentToItem` / `ruleToItem`：model → Item 转换，含 Subtitle 格式化
- `displayDeviceName`：Name > Hostname > "device-N" 优先级
- `containsItem`：Type+ID 双键去重
- `isAllDigits`：纯数字判定（用于 id 精确匹配）

## 5. 依赖关系

- **标准库**：`context`、`fmt`、`strings`、`time`
- **内部包**：`biz/alert`（Usecase / ListFilter / IncidentFilter）、`biz/device`（Usecase / ListFilter）、`manager/model/alert`（Incident / Rule）、`manager/model/device`（Device / DecodeRoles）、`pkg/logquery`（Client）
- **被调用方**：chat HTTP handler（Search）、chatruntime.MentionResolver（Resolve）

## 6. 并发与资源管理

- **无并发状态**：Searcher 是纯 facade，无内部可变状态
- **Search 串行**：四个子搜索顺序执行，非并发——popover 响应时间 = 四子搜索之和，当前规模可接受
- **Resolve 串行**：mention 逐个水合，非并发
- **ctx 透传**：所有子搜索接收 ctx，支持 caller 超时取消

## 7. 设计模式与亮点

- **read-only**：包注释明示"never mutates platform state"——纯查询
- **最小字段**：只返回 id+label+subtitle，agent 深度水合自行负责
- **nil-safe deps**：devices/alerts/logClient 任一 nil 对应类型返回空——无 Loki 部署仍可用
- **单源失败不 blank**：`run` helper 静默 skip 失败子搜索——单 bad source 不影响其他
- **稳定类型顺序**：device → incident → rule → file，popover 渲染可预测
- **id 精确匹配优先**：term 全数字时先 Get 精确匹配（operator 常贴 id），再 substring 补充
- **Limit 双约束**：0→10 默认，>50→50 上限，防 popover 卡顿
- **Filter 空 = 4*limit**：每类型 limit，总计最多 4*limit——"top N from each grouped"
- **Resolve 失败降级**：删除的 device 返回空字符串，agent run 不受影响
- **resolveOne fallback**：Type 未匹配或水合失败走 `"- type label (id)"` generic 格式
- **TypeFile 无水合**：filename 自描述，无需额外查询——省一次 logquery 调用
- **searchFiles 24h 窗口**：注释明示"most operators want the file they're actively writing to"——quiet file 走 free-text
- **LogQuerierAdapter 类型别名**：`type LogQuerierAdapter = logquery.Client`，wiring 站点清晰，无适配开销
- **多租户注释**：包注释明示 v1 单租户，tenancy 落地时加 owner 参数——wire shape 稳定

## 8. 注意事项

- **searchRules 全量拉取**：`ListRules("")` 无 server-side filter，客户端 Contains 过滤——规则数大时性能差
- **searchIncidents MVP 限制**：IncidentFilter 仅 exact RuleKey，substring 需完整粘贴——popover 用户通常 copy/paste
- **searchFiles 仅 filename label**：其他 label（job/instance）未暴露，未来扩展需改 Type 集合
- **Search 串行**：四子搜索顺序执行，高延迟场景 popover 可能慢——可改并发但当前规模够用
- **Resolve TypeRule 性能**：ListRules 全量 + 遍历匹配，规则多时慢
- **`isAllDigits` 简单实现**：仅 0-9，不含负号/小数点——device/incident id 是 uint64 正整数，足够
- **`displayDeviceName` 优先级**：Name > Hostname > "device-N"，三者为空时 fallback 兜底
- **Subtitle 格式硬编码**：`"online · role1,role2 · ip"` 格式固定，i18n 需改源码
- **无租户过滤**：v1 单租户，多租户落地需加 owner 参数并传播到 repo 调用
- **`LogQuerierAdapter` 类型别名**：`logquery.Client` 方法集已与 `logQuerier` 接口结构匹配，别名仅 wiring 清晰用
- **`Resolve` 返回 markdown bullet**：格式 `- xxx`，agent 内联到 system prompt 前导块——格式改动需同步 chatruntime.Handle 的 mentionsRendered 拼装
