# `system_prompt.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/chatruntime/system_prompt.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/chatruntime`

## 1. 概述

本文件实现系统提示分层拼装：`ComposeSystemPrompt` 把 basePrompt + coordinatorToolRouting（仅 coordinator）+ agentProfile.SystemPrompt + CriticalReminder + 激活 skills 的 `[能力: name]` body 组装成最终 SystemPrompt；`buildToolCapabilityDigest` 动态生成本轮可见工具清单（origin/class 分组统计 + direct_read_tools + 截断的工具列表）；`buildAgentCatalog` 生成可派生 specialist 目录（排除 reviewer/default）。是 LLM 理解自身能力边界与路由规则的核心 prompt 工程。

## 2. 包信息

- **包名**：`chatruntime`
- **所属模块**：`internal/manager/biz/aiops/chatruntime`
- **依赖方向**：被 runtime.go / worker.go 调用；依赖 tools 包的 IsCoreToolName、basetool.BaseTool

## 3. 关键类型与接口

```go
const coordinatorToolRouting = `## 工具选型补充
- ongrid device/edge ≠ k8s node。问 ongrid 设备用 ongrid 工具；问 k8s 集群用 ` + "`mcp__k8s__*`" + `，不要猜 ` + "`kubectl`" + `。
- 复杂跨域任务可同轮并行多个 ` + "`AgentTool`" + `；简单 topN / 快照 / 列表仍直接查。错误来源连续 2 次不匹配就换路或说明缺口。`

const maxCapabilityDigestTools = 12

type capabilityDigestRow struct {
    name, origin, class, desc string
}
```

## 4. 关键函数与流程

### `ComposeSystemPrompt`
- **签名**：`func ComposeSystemPrompt(basePrompt string, activeSkills []*Skill, agentProfile *Agent) string`
- **职责**：分层拼装 SystemPrompt
- **分层顺序**：
  1. `basePrompt`（runtime 通用前导，可空）
  2. `coordinatorToolRouting`（**仅 coordinator**，`agentProfile == nil` 时注入）
  3. `agentProfile.SystemPrompt` + `<critical-reminder>...</critical-reminder>`（仅 worker persona）
  4. 每个 active skill：`[能力: <name>]` header + skill PromptBody（若 body 已以 header 开头则不重复加）
- **拼接**：`strings.Join(parts, "\n\n")`
- **注释明示**：纯字符串拼装，不在此处注入 `<system-reminder>`——per-turn 注入由 graph 层 `buildSystemReminder` 负责（防长会话注意力漂移）

### `buildToolCapabilityDigest`
- **签名**：`func buildToolCapabilityDigest(tools []basetool.BaseTool) string`
- **职责**：生成本轮可见工具的紧凑清单（动态对应静态 basePrompt）
- **流程**：
  1. 遍历 tools，`t.Info(ctx)` 获取 name/origin/class/desc
  2. origin 空 → `"builtin"`；class 空 → `"read"`
  3. `counts[origin+"/"+class]++` 统计
  4. **过滤**：`info.Origin == "" && !isDigestBuiltin(info.Name)` → 跳过（只列 builtin 核心工具 + 所有有 origin 的 MCP/installed 工具）
  5. `desc = compactOneLine(firstNonEmpty(WhenToUse, Description), 72)` 截断
  6. 按 origin 然后 name 排序
  7. 输出：
     - `## 本轮可见能力（动态）`
     - `- counts: builtin/read=5, mcp/read=3, ...`
     - `- direct_read_tools: `query_devices`, `query_edges`, ...`
     - 工具列表（最多 `maxCapabilityDigestTools=12` 行）：`- name [origin/class]: desc`
     - 超出截断：`- ... N more; use ToolSearch/select:<tool> if a schema is redacted`
- **目的**：让 LLM 知道本轮有哪些工具可用，避免幻觉不存在的工具名

### `directReadToolNames`
- **签名**：`func directReadToolNames(rows []capabilityDigestRow) []string`
- **职责**：从 rows 中筛出 `origin=builtin + class=read + IsCoreToolName` 的工具名
- **流程**：过滤 + 排序

### `buildAgentCatalog`
- **签名**：`func buildAgentCatalog(reg *AgentRegistry) string`
- **职责**：生成可派生 specialist 目录（注入 coordinator system prompt）
- **流程**：
  1. `reg.All()` 全量
  2. **排除** `reviewer`（仅 review_gate decorator 用）+ `default`（虚拟顶层 persona，递归派生自己无意义）
  3. 每行：`- ` + "`name`" + ` — desc。何时派：when_to_use 首行`
  4. 头部：`## 可用的 specialist 助理（AgentTool 的 subagent_type）`
  5. 尾部：`派活时：在 prompt 里写清 incident_id / device_id / 用户原话——worker 看不到你的上下文。`
- **目的**：让 coordinator LLM 知道 AgentTool 的合法 subagent_type 值 + 何时派哪个

### 辅助函数
- **`isDigestBuiltin(name)`**：调 `aiopstools.IsCoreToolName(name)`
- **`compactOneLine(s, max)`**：`strings.Fields` 压空白 + 截断到 max-1 + `…`
- **`firstNonEmpty(values...)`**：返回首个非空字符串

## 5. 依赖关系

- **标准库**：`context`、`fmt`、`sort`、`strings`
- **同包**：`types.go`（Skill / Agent）、`agent_registry.go`（AgentRegistry.All）
- **跨包**：`aiopstools "internal/manager/biz/aiops/tools"`（IsCoreToolName）、`basetool`（BaseTool）
- **被调用方**：runtime.go（coordinator 路径）、worker.go（worker 路径）

## 6. 并发与资源管理

- **无并发状态**：所有函数纯同步，无共享可变状态
- **`t.Info(ctx)` 调用**：每个工具一次，工具实现保证并发安全（basetool 契约）
- **`maxCapabilityDigestTools=12` 常量**：截断上限，避免工具过多撑爆 system prompt

## 7. 设计模式与亮点

- **分层拼装**：basePrompt → coordinator routing → persona → skills，每层独立可空——coordinator 与 worker 共用同一函数，靠 `agentProfile == nil` 区分
- **coordinator-only routing**：`coordinatorToolRouting` 仅注入 coordinator（worker 已被 persona toolbag 过滤，不需要路由规则）——避免 worker prompt 噪音
- **CriticalReminder 双层注入**：SystemPrompt 里 plant 一次（`<critical-reminder>`）+ graph 层每轮 re-inject（`<system-reminder>`）——防长会话注意力漂移
- **`[能力: name]` header 去重**：若 PromptBody 已以 header 开头则不重复加——ParseSkillMd 的 normalizeSkillBodyH1 保证 body 首行就是 `[能力: name]`，此处兜底
- **动态能力清单 vs 静态 basePrompt**：basePrompt 保持小而稳定；动态工具（MCP/installed）通过 buildToolCapabilityDigest 自动浮现——新增工具无需改 basePrompt
- **direct_read_tools 单独列出**：让 LLM 快速识别核心只读工具，优先用直接查询而非 AgentTool 派生——降低不必要的 sub-agent 开销
- **specialist 目录排除 reviewer/default**：reviewer 仅供 review_gate decorator 用，default 是虚拟顶层——列出会让 coordinator 误派
- **when_to_use 首行截断**：避免冗长 when_to_use 撑爆 prompt，只取首行作为"何时派"提示
- **派活提示**：尾部"worker 看不到你的上下文"——强制 coordinator 在 prompt 里写清 incident_id/device_id，避免 worker 因缺上下文而失败

## 8. 注意事项

- **`coordinatorToolRouting` 中文硬编码**：ongrid device ≠ k8s node 的区分是 ongrid 业务特定知识，跨项目不可复用
- **`maxCapabilityDigestTools=12` 硬编码**：工具数 >12 时截断并提示用 ToolSearch——若工具数远超 12 需考虑分级展示
- **`isDigestBuiltin` 仅认 IsCoreToolName**：非核心 builtin 工具（若有）不会出现在 digest 列表，但仍计入 counts
- **`buildAgentCatalog` 依赖 AgentRegistry**：reg==nil 返回空字符串，runtime 需 nil-check
- **`compactOneLine` 截断用 `…`**：单字节省略号，跨平台显示一致
- **`ComposeSystemPrompt` 不含动态 hints**：动态 hints（consecutiveFailedTool 等）由 graph 层 buildSystemReminder 注入，不在此处——避免 basePrompt 每轮变化
- **`firstNonEmpty` 优先 WhenToUse**：WhenToUse 是工具的"何时用"提示，比 Description 更 actionable——digest 优先用 WhenToUse
- **specialist 目录注入位置**：runtime.go 中是 append 到 basePrompt 后再调 ComposeSystemPrompt——catalog 在 basePrompt 层而非 skills 层
