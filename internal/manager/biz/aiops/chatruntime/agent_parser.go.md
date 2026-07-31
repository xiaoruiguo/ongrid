# `agent_parser.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/chatruntime/agent_parser.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/chatruntime`

## 1. 概述

本文件实现 `ParseAgentMd` —— agent persona 文件（`agents/<name>.md`）解析器。与 SKILL.md 同构（YAML frontmatter + markdown body），但 body 成为 agent 的 system prompt 而非 skill prose。严格校验 3 个必填字段（name/description/when_to_use）；name 不强制 snake_case（允许 dash，如 `incident-investigator`），仅警告；未知 frontmatter key 保留到 `UnknownFields` 实现前向兼容（claude-code 新增 effort/isolation/mcp_servers/hooks 等字段不破坏加载）。

## 2. 包信息

- **包名**：`chatruntime`
- **所属模块**：`internal/manager/biz/aiops/chatruntime`
- **依赖方向**：被 `load_all.go`/`plugin_container.go` 调用；依赖 `skill_parser.go`（`splitFrontmatter`/`skillNameRe`）、`types.go`（`Agent`/`LoadWarning`）

## 3. 关键类型与接口

复用 `types.go` 的 `Agent`、`LoadWarning`。无新类型定义。

## 4. 关键函数与流程

### `ParseAgentMd`
- **签名**：`func ParseAgentMd(path string) (*Agent, []LoadWarning, error)`
- **职责**：解析 agent persona 文件
- **流程**：
  1. `os.ReadFile(path)` → 失败 `%w`
  2. `splitFrontmatter(raw)` 拆 (frontmatter, body) → 失败 `%w`
  3. `yaml.Unmarshal(frontmatter, &ag)` 解析 typed struct → 失败 `%w`
  4. `yaml.Unmarshal(frontmatter, &rawMap)` 解析 raw map → `retainUnknownAgentFields` 保留未知 key
  5. **必填字段严格校验**：
     - name 空 → error "frontmatter missing required field 'name'"
     - description 空 → error "missing 'description'"
     - when_to_use 空 → error "missing 'when_to_use'"（coordinator 无法决策是否 spawn）
  6. **name snake_case 警告**：`!skillNameRe.MatchString(ag.Name)` → warning code=`name_non_snake`（允许 dash，仅警告不重写，因 name 是 spawn key，重写会破坏引用）
  7. `ag.SystemPrompt = strings.TrimRight(string(body), "\n")`
  8. 返回 `&ag, warnings, nil`

### `retainUnknownAgentFields`
- **签名**：`func retainUnknownAgentFields(raw map[string]any) map[string]any`
- **职责**：保留 frontmatter 中未建模的 key
- **流程**：已知 key 集合（13 个：name/description/when_to_use/tools/disallowed_tools/permission_mode/max_turns/model/critical_reminder/initial_prompt/background/omit_claude_md/metadata）之外的全部保留；空 → nil
- **用途**：前向兼容 claude-code agent schema 扩展（effort/isolation/mcp_servers/hooks 等）

## 5. 依赖关系

- **内部包**：无外部包依赖
- **包内依赖**：`skill_parser.go`（`splitFrontmatter`、`skillNameRe`）、`types.go`（`Agent`、`LoadWarning`）
- **外部库**：`gopkg.in/yaml.v3`、标准库 `fmt`、`os`、`strings`

## 6. 并发与资源管理

- **纯函数**：无状态、无锁、无 IO 状态（仅读文件）
- **无 ctx 参数**：一次性解析，无取消需求

## 7. 设计模式与亮点

- **与 SKILL.md 同构**：共用 `splitFrontmatter`，frontmatter 格式一致，降低维护成本
- **必填字段严格校验**：name/description/when_to_use 缺一即 error（非 warning），保证 coordinator 决策所需信息完整
- **name 不强制重写**：与 skill 不同，agent name 允许 dash（claude-code 风格 `incident-investigator`），仅警告；因 name 是 spawn key，静默重写会破坏 LLM 引用
- **前向兼容 UnknownFields**：保留未知 frontmatter key，claude-code 新增字段不破坏 ongrid 加载
- **body 直接作 SystemPrompt**：与 skill 的 PromptBody 不同，agent body 是 system prompt，无需 H1 规范化
- **TrimRight 尾部换行**：避免 system prompt 末尾多余空行

## 8. 注意事项

- **`when_to_use` 严格必填**：coordinator 通过此字段决定 spawn 时机，缺失会导致 agent 永不被选中
- **name 不重写**：LLM 引用 agent name 必须与 frontmatter 完全一致；改名需同步更新所有引用
- **`splitFrontmatter` 共享**：与 skill_parser.go 共用，frontmatter 格式必须一致（`---` 分隔）
- **`skillNameRe` 复用**：agent name 校验复用 skill 的正则，但仅警告不强制；skill 则会自动 normalize
- **未知字段保留 raw any**：不解析未知字段的结构，仅保留原始 YAML 值
- **无 metadata 校验**：`metadata` 子树（os/requires/ongrid）不在本文件校验，由下游使用方处理
- **body 保留原始 markdown**：不做 H1 规范化（与 skill 的 `normalizeSkillBodyH1` 不同），agent system prompt 是原始内容
