# `parse.go` 技术实现文档

> 源文件：`internal/edgeagent/cmdpolicy/parse.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/cmdpolicy`

## 1. 概述

本文件实现 cmdpolicy 自有的极简 POSIX shell tokenizer：仅支持简单命令 + 可选管道 + 双/单引号 + `\` 转义；任何其他 shell 特性（重定向、`&&`、`||`、`&`、`;`、命令替换 `$()`/反引号、子 shell `()`、进程替换 `<()`）一律拒绝。解析结果直接喂给 `exec.Command`，从不调用真实 shell。

## 2. 包信息

- **包名**：`cmdpolicy`
- **所属模块**：edgeagent 命令策略 + sandbox 执行层
- **依赖方向**：被同包 `policy.go` 的 `Decide` 调用；调用 `errors`、`fmt`、`strings`

## 3. 关键类型与接口

无类型定义。返回值是 `[][]string`（管道分段 + 每段的 argv）。

## 4. 关键函数与流程

### `SplitPipes`
- **签名**：`func SplitPipes(cmd string) ([][]string, error)`
- **职责**：把命令字符串切成「管道分段 + argv 数组」
- **流程**：
  1. TrimSpace；空串报错
  2. 调 `tokenize` 获取 tokens + breaks（breaks[i]=true 表示该 token 是 `|` 分隔符）
  3. 遍历 tokens，遇 `|` 收尾当前 segment 并开新 segment；普通 token 追加到当前 segment
  4. 空 segment / 空 trailing segment / 零 segment 都报错
- **错误处理**：tokenize 内部对禁止字符直接返回错误；分段阶段对空段返回错误

### `ParseSegment`
- **签名**：`func ParseSegment(seg string) ([]string, error)`
- **职责**：把单段命令字符串 tokenize 成 argv（不允许出现 `|`）
- **流程**：tokenize 后检查 breaks 中无 true，否则报错「pipe encountered in single segment」
- **错误处理**：空 segment / 含 pipe / 零 token 报错

### `tokenize`
- **签名**：`func tokenize(in string) ([]string, []bool, error)`
- **职责**：核心扫描器，返回平行数组 tokens（字符串）+ breaks（是否为 `|` 分隔符）
- **流程**：逐 rune 扫描：
  - 空白：`flush()` 当前 token
  - `|`：`||` 拒绝；单 `|` flush 后追加分隔符 token
  - `&` / `;` / `>` / `<` / 反引号 / `(` / `)`：一律拒绝
  - `$`：`$(` / `${` 拒绝；`$VAR` 当字面 token 接受（因为从不调用 shell，不会展开）
  - `\`：取下一 rune 字面值；末尾反斜杠拒绝
  - `"` / `'`：调 `readQuoted`
  - 默认：写入 buf
  - `inToken` 标志位让空引号 `""` 也能成为有效 token
- **错误处理**：任何禁止字符或未终止引号返回错误

### `readQuoted`
- **签名**：`func readQuoted(runes []rune, start int, term rune) (string, int, error)`
- **职责**：读取引号内容直到未转义的终止符
- **流程**：
  - 双引号内：`\"` `\\` `\$` `` \` `` 按 POSIX 转义；其他 `\` 字面保留；拒绝 `$(` / `${` / 反引号
  - 单引号内：所有字符字面直到下一个单引号（POSIX 单引号规则）
  - 未找到终止符返回「unterminated quoted string」错误

## 5. 依赖关系

- **内部包**：无
- **外部库**：标准库 `errors`、`fmt`、`strings`
- **被调用方**：同包 `policy.go::Policy.Decide` → `SplitPipes`

## 6. 并发与资源管理

无并发控制。函数无状态、纯字符串处理。`strings.Builder` 在函数栈上分配。

## 7. 设计模式与亮点

- **手写 ~120 行 tokenizer**：刻意不引入 `google/shlex` 或完整 shell parser，减少依赖与攻击面
- **平行数组返回 (tokens, breaks)**：用 breaks 标识 `|` 分隔符而非引入 `Token` 结构体，简化数据流
- **`inToken` 标志位**：让空引号 `""` 也能成为 argv 元素（否则空 builder 会被误判为「无 token」）
- **拒绝所有写入/分支特性**：重定向、`;`、`&&`、`||`、`&`、`$()`、反引号、`()` 全部拒绝——cmdpolicy 不希望 LLM 误以为可以用任何 shell 高级特性
- **`$VAR` 字面通过**：因为从不调用 shell，`$VAR` 不会被展开；只有 `$(` / `${` 表明「意图展开」才拒绝

## 8. 注意事项

- 单引号内 `\` 不被识别为转义（POSIX 严格规则）；这与 bash 行为一致但与某些脚本习惯不同
- 双引号内仅 `\"` `\\` `\$` `` \` `` 被转义，其他 `\X` 字面保留为 `\X`
- 不支持多行命令（输入中的 `\n` 视为空白 flush）；如需多行请用 `;` 分隔——但 `;` 被拒绝，所以多命令必须通过多次调用
- tokenizer 不做 globbing（`*` `?` `[`）——这些字符会作为字面 token 传给 `exec.Command`，下游 binary 自行决定是否展开
- 不支持 heredoc / herestring，刻意为之：LLM 不应通过 heredoc 绕过 cmdpolicy
