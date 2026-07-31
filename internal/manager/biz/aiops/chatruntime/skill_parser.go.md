# `skill_parser.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/chatruntime/skill_parser.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/chatruntime`

## 1. 概述

本文件实现 SKILL.md 解析器：YAML frontmatter 分割、snake_case 名称校验与自动规范化、body H1 自动改写为 `[能力: <name>]`、未知字段保留（前向兼容）、`metadata.ongrid.activation` 与顶层 `activation` 冲突时前者优先。是 SkillRegistry.Load 链路的解析核心。

## 2. 包信息

- **包名**：`chatruntime`
- **所属模块**：`internal/manager/biz/aiops/chatruntime`
- **依赖方向**：被 load_all.go 的 walkLoadRoot 调用；依赖 `gopkg.in/yaml.v3`

## 3. 关键类型与接口

```go
const frontmatterDelimiter = "---"

var (
    skillNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_]*$`)       // snake_case 校验
    nonSnakeRe  = regexp.MustCompile(`[^a-z0-9_]+`)                  // 需规范化的字符
    h1Re        = regexp.MustCompile(`^#\s+.+\s*$`)                  // markdown H1
)
```

无新增类型；产出 `*Skill` + `[]LoadWarning`（types.go 定义）。

## 4. 关键函数与流程

### `ParseSkillMd`
- **签名**：`func ParseSkillMd(path string) (*Skill, []LoadWarning, error)`
- **职责**：读取 + 解析 SKILL.md，返回 Skill + 非致命警告
- **流程**：
  1. `os.ReadFile(path)`
  2. `splitFrontmatter(raw)` 分割 frontmatter / body
  3. `yaml.Unmarshal(frontmatter, &sk)` 强类型解析
  4. `yaml.Unmarshal(frontmatter, &rawMap)` 再解析为 map
  5. `sk.UnknownFields = retainUnknownSkillFields(rawMap)` 保留未知 key
  6. **必填校验**：`name` 或 `description` 空 → error（不降级为 warning）
  7. **snake_case 校验**：`!skillNameRe.MatchString(sk.Name)` → `normalizeSnakeName` + warning `code: "name_normalized"`
  8. `sk.PromptBody = normalizeSkillBodyH1(body, sk.Name)` body H1 改写
  9. **activation 冲突调和**：`metadata.ongrid.activation` 非空 → 覆盖顶层 `activation`；若顶层也非空且不同 → warning `code: "activation_conflict"`
  10. 返回 `&sk, warnings, nil`
- **错误处理**：read 失败 / frontmatter malformed / 必填缺失 → error；其他降级 warning

### `splitFrontmatter`
- **签名**：`func splitFrontmatter(raw []byte) (frontmatter, body []byte, err error)`
- **职责**：分割 YAML frontmatter 与 markdown body
- **流程**：
  1. bufio.Scanner 逐行扫，跳过前导空行
  2. 首个非空行 != `---` → 无 frontmatter，返回 `(nil, raw, nil)`
  3. 首行 == `---` → 收集后续行直到下一个 `---`
  4. 未闭合 → error
  5. 返回 `(fmBytes, bodyBytes, nil)`
- **buffer**：`scanner.Buffer(64KB, 1MB)` 防大 frontmatter 截断

### `normalizeSkillBodyH1`
- **签名**：`func normalizeSkillBodyH1(body []byte, name string) []byte`
- **职责**：把 body 首个 H1 改写为 `[能力: <name>]`
- **流程**：
  1. 扫首行，`h1Re.MatchString` 判断是否 H1
  2. 命中 → 替换为 `[能力: <name>]` + 剩余 body
  3. 未命中 → 在 body 头部插入 `[能力: <name>]\n\n`
- **目的**：保证 ComposeSystemPrompt 拼装时每个 skill 块有统一 header

### `normalizeSnakeName`
- **签名**：`func normalizeSnakeName(name string) string`
- **职责**：把非 snake_case 名字规范化为 snake_case
- **流程**：`strings.ToLower` → `nonSnakeRe.ReplaceAllString(_, "_")` → 去首尾下划线
- **示例**：`My-Skill.name` → `my_skill_name`

### `retainUnknownSkillFields`
- **签名**：`func retainUnknownSkillFields(rawMap map[string]any) map[string]any`
- **职责**：保留 frontmatter 中 8 个已知 key 之外的所有字段
- **已知 key**：`name`、`description`、`activation`、`tools`、`metadata`、`requires`、`version`、`author`（具体列表见源码）
- **前向兼容**：上游 schema 加字段不会让 ongrid 加载失败

## 5. 依赖关系

- **标准库**：`bufio`、`bytes`、`fmt`、`os`、`regexp`、`strings`
- **外部库**：`gopkg.in/yaml.v3`
- **同包**：`types.go`（Skill / LoadWarning / Activation）
- **被调用方**：load_all.go 的 walkLoadRoot

## 6. 并发与资源管理

- **无并发状态**：所有函数纯同步，无共享可变状态
- **正则常量**：`skillNameRe` / `nonSnakeRe` / `h1Re` 是包级 `var`，`regexp.MustCompile` 一次性编译，只读共享
- **scanner buffer**：每次 splitFrontmatter 新建 64KB-1MB buffer，函数退出即回收

## 7. 设计模式与亮点

- **双解析保留未知字段**：先 `yaml.Unmarshal` 到强类型 Skill，再 `yaml.Unmarshal` 到 `map[string]any`，差集保留到 `UnknownFields`——前向兼容的教科书实现
- **snake_case 自动规范化**：非 conforming 名字不报错，自动规范化 + warning，降低用户排障门槛
- **H1 自动改写**：body 首个 H1 强制改为 `[能力: <name>]`，保证 ComposeSystemPrompt 拼装时每个 skill 块 header 统一
- **activation 冲突 ongrid 优先**：`metadata.ongrid.activation` 与顶层 `activation` 冲突时前者胜——ongrid 私有命名空间优先于通用 schema
- **必填字段 error 而非 warning**：name/description 缺失是 error，注释明示"a SKILL.md without a description is unusable"
- **frontmatter malformed 是 error**：未闭合的 `---` 是 error，避免把 frontmatter 内容误当 body
- **scanner buffer 1MB 上限**：防恶意/意外超大 frontmatter 撑爆内存

## 8. 注意事项

- **`splitFrontmatter` 行级扫描**：用 bufio.Scanner 逐行扫，不解析整个 markdown——性能好但假设 frontmatter 内不会有 `---` 行（实际 YAML 中 `---` 是文档分隔符，SKILL.md frontmatter 不会嵌套）
- **`normalizeSkillBodyH1` 只改首个 H1**：body 中后续 H1 不动；若作者用 H1 做小节标题会保留
- **`retainUnknownSkillFields` 已知 key 列表硬编码**：新增已知字段需同步更新此函数的排除列表，否则会被误保留到 UnknownFields
- **`normalizeSnakeName` 不防首字符非字母数字**：`skillNameRe` 要求首字符 `[a-z0-9]`，但 normalizeSnakeName 不强制——规范化后可能仍不 conform（如 `_my_skill`），此时 ParseSkillMd 不会再校验一次（注释假设规范化后必 conform，但极端输入可能违反）
- **`activation_conflict` warning 不阻止覆盖**：ongrid activation 覆盖顶层后仍继续加载，仅 warn 提示作者
- **YAML 解析两次**：强类型 + map 两次 Unmarshal 有性能开销，但 SKILL.md 加载是 boot 期一次性，可接受
- **`h1Re` 匹配 `^#\s+.+\s*$`**：要求 H1 后必须有内容，`#` 单独一行不算 H1
