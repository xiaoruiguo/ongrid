# `command_parser.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/chatruntime/command_parser.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/chatruntime`

## 1. 概述

本文件实现 `ConvertCommandFile` —— 把 claude/cursor 的 `commands/<name>.md` 文件转换为 ongrid Skill（mode=keyword）。转换规则：keyword=`["/<name>", "<name>"]`（含 dash 时追加 snake_case 别名）；skill name 加 `cmd_` 前缀避免冲突；frontmatter description → Skill.Description（缺省生成默认）；body → PromptBody（加 `[能力: cmd_<name>]` header）；frontmatter `allowed-tools`（claude 特有）作为软提示追加到 body 末尾，**不强制**（ongrid 工具类与 claude 不 1:1 映射）。

## 2. 包信息

- **包名**：`chatruntime`
- **所属模块**：`internal/manager/biz/aiops/chatruntime`
- **依赖方向**：被 `plugin_container.go` 的 `walkCommandsRoot` 调用；依赖 `skill_parser.go`（`splitFrontmatter`/`skillNameRe`/`normalizeSnakeName`）、`types.go`（`Skill`/`Activation`/`LoadWarning`/`SkillMetadata`/`OngridExt`）

## 3. 关键类型与接口

```go
type commandFrontmatter struct {
    Description  string   `yaml:"description"`
    AllowedTools []string `yaml:"allowed-tools"`  // claude 特有，kebab-case
}
```

## 4. 关键函数与流程

### `ConvertCommandFile`
- **签名**：`func ConvertCommandFile(path string) (*Skill, []LoadWarning, error)`
- **职责**：claude command .md → ongrid Skill
- **流程**：
  1. 扩展名非 `.md` → error "command file must end in .md"
  2. `os.ReadFile(path)` → 失败 `%w`
  3. `splitFrontmatter(raw)` → (frontmatter, body)
  4. `yaml.Unmarshal(frontmatter, &fm)` 解析 commandFrontmatter
  5. **basename 提取**：`filepath.Base` 去扩展名 → trim；空 → error "empty basename"
  6. **keyword 列表**：
     - 总是含 `"/<base>"` 和 `"<base>"`
     - basename 含 `-.空格` → 追加 `normalizeSnakeName(base)` 别名（如 `review-pr` → `review_pr`）
  7. **skill name**：`cmd_<nameSuffix>`（nameSuffix 非 snake_case → normalize）；`cmd_` 前缀避免与常规 skill 冲突
  8. **description**：fm.Description 空 → 生成默认 `Slash-command equivalent of /<base> (claude commands import).`
  9. **PromptBody 组装**：
     - header `[能力: cmd_<name>]`
     - bodyText（原始 markdown）
     - fm.AllowedTools 非空 → 追加软提示 `> 上游 (claude) 允许使用的工具: <tools>。ongrid 不强制此清单，仅作提示。`
  10. 构造 Skill struct（Activation.Mode=keyword, Keywords=keywords；同时填 Metadata.Ongrid.Activation 保持一致）
  11. **warning**：fm.Description 空 → warning code=`command_missing_description`
  12. 返回 Skill + warnings

## 5. 依赖关系

- **内部包**：无外部包依赖
- **包内依赖**：`skill_parser.go`（`splitFrontmatter`、`skillNameRe`、`normalizeSnakeName`）、`types.go`（`Skill`、`Activation`、`LoadWarning`、`SkillMetadata`、`OngridExt`）
- **外部库**：`gopkg.in/yaml.v3`、标准库 `fmt`、`os`、`path/filepath`、`strings`

## 6. 并发与资源管理

- **纯函数**：无状态、无锁、无 IO 状态（仅读文件）
- **无 ctx 参数**：一次性解析

## 7. 设计模式与亮点

- **claude command → ongrid Skill 适配器模式**：把 claude/cursor 的 command 格式适配为 ongrid 统一的 Skill 结构
- **`cmd_` 前缀隔离**：避免 command 与常规 skill name 冲突
- **keyword 双形式**：`/<name>` 和 `<name>` 都触发，匹配用户输入习惯
- **dash 别名**：`review-pr` 追加 `review_pr` 别名，兼容两种命名风格
- **allowed-tools 软提示**：claude 的 `allowed-tools: [Bash, Edit]` 不强制（ongrid 工具类不 1:1），仅作为 LLM 提示
- **缺省 description 生成**：fm.Description 空 → 自动生成默认描述 + warning，保证 Skill.Description 非空（Skill 必填字段）
- **OngridExt.Activation 双填**：顶层 Activation 和 Metadata.Ongrid.Activation 都填，保持一致（loader 优先 OngridExt）
- **嵌套子目录扁平化**：`commands/git/commit.md` → `/commit`（由 loader walkCommandsRoot 递归，本函数仅处理单文件）

## 8. 注意事项

- **`allowed-tools` 不强制**：claude 的工具限制（Bash/Edit/...）在 ongrid 无对应映射，仅作提示；ongrid 用 Policy + ToolClass 控制权限
- **keyword 子串匹配**：Activation.Mode=keyword 用 `strings.Contains` 匹配，`/foo` 可能误匹配包含此子串的请求
- **`cmd_` 前缀硬编码**：skill name 必然以 `cmd_` 开头，LLM 看到的 skill name 是 `cmd_<base>`
- **basename 非 snake_case 时 normalize**：但 keyword 仍保留原 basename，用户输入 `/review-pr` 也能触发
- **description 缺省生成**：默认描述是英文，与 ongrid 中文优先风格略有不符；但 command 是 claude 导入，保留英文源风格
- **body 保留原始 markdown**：不做 H1 规范化（与 ParseSkillMd 不同），因 header 是 `[能力: cmd_<name>]` 而非 `[能力: <name>]`
- **无 frontmatter 也允许**：fm 字段全空时仍生成 Skill（用默认 description），降低 claude command 导入门槛
