# `credinject.go` 技术实现文档

> 源文件：`internal/pkg/credinject/credinject.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/credinject`

## 1. 概述

该文件实现 skill / MCP 声明式凭证注入的核心解析逻辑（HLD-017）：将 skill manifest 中 `requires.credentials[].inject` 的模板（`{{field}}` → ENV / 文件）与 operator 绑定的具体凭证字段拼接，产出实际要应用到 skill exec 环境的 env vars 与文件。包刻意保持零业务依赖、零 import cycle，调用方传入 plain map / slice 而非领域类型，使其可独立单元测试。

## 2. 包信息

- **包名**：`credinject`
- **所属模块**：`internal/pkg/`（基础设施层，无业务依赖）
- **依赖方向**：被 skill 加载器 / MCP 客户端 wiring 调用；仅依赖标准库。

## 3. 关键类型与接口

### `FileSpec`
镜像 manifest 中的凭证文件声明（plain 类型）。

```go
type FileSpec struct {
    Path    string
    Content string
    Mode    string // 八进制字符串，默认 "0600"
}
```

### `FilePlan`
解析后待物化的文件，exec 前创建、exec 后删除。

```go
type FilePlan struct {
    Path    string
    Content string
    Mode    fs.FileMode
}
```

### `Plan`
单个 credential slot 的完整注入结果。

```go
type Plan struct {
    Env   map[string]string
    Files []FilePlan
}
```

## 4. 关键函数与流程

### `Resolve`
- **签名**：`func Resolve(envSpec map[string]string, fileSpecs []FileSpec, fields map[string]string) (Plan, []string, error)`
- **职责**：展开 env + file 模板，返回 Plan + 缺失字段名列表。
- **流程**：
  1. 构造 `missing` map 记录引用但缺失的字段。
  2. `expand` 闭包：用 `fieldRe.ReplaceAllStringFunc` 替换 `{{ field }}`，命中 `fields` 则替换为值，未命中记入 `missing` 并替换为空串。
  3. 遍历 `envSpec`：trim name，跳过空 name，写入 `plan.Env`。
  4. 遍历 `fileSpecs`：trim path，跳过空 path；解析 `Mode`（默认 `0o600`，`ParseUint(..., 8, 32)`）；解析失败返回错误 `bad file mode`。
  5. 将 `missing` 转 slice 并 `sortStrings` 排序，便于调用方稳定 warn。
- **错误处理**：仅 file mode 解析失败返回 error；缺失字段不视为错误，仅返回告知调用方。

### `sortStrings`（私有）
- **签名**：`func sortStrings(s []string)`
- **职责**：插入排序。
- **设计理由**：避免仅为一次调用引入 `sort` 包。

### `fieldRe`（包级变量）
- 正则 `\{\{\s*([a-zA-Z0-9_.-]+)\s*\}\}`，匹配 `{{ field }}` 模板，允许内部空格。

## 5. 依赖关系

- **内部包**：无。
- **外部库**：仅标准库 `fmt` / `io/fs` / `regexp` / `strconv` / `strings`。
- **被调用方**：skill loader / MCP wiring（cmd/ongrid + manager biz/skill）。

## 6. 并发与资源管理

无并发控制。`Resolve` 是纯函数式调用，无共享状态；输入与输出均由调用方持有。

## 7. 设计模式与亮点

- **零依赖设计**：刻意不依赖任何业务包或第三方库，保持可测试性与无环依赖。
- **plain types 边界**：输入输出均为 map / slice / string，避免领域类型污染，使包可独立单测。
- **缺失字段不致命**：缺失字段展开为空串并记录返回，调用方可 warn 而非 fail，符合"宽松输入、严格输出"的集成思路。
- **手写排序避免 import**：`sortStrings` 仅 5 行，省去 `sort` 包引入，体现极简依赖策略。
- **八进制 mode 字符串**：与 manifest YAML 友好（YAML 中 `0600` 易被解析为十进制 600，字符串形式更安全）。

## 8. 注意事项

- **空串替换的副作用**：缺失字段展开为空串而非报错，可能导致 skill 拿到空凭证后失败；调用方应主动检查返回的 `missing` 列表并 warn。
- **正则全局共享**：`fieldRe` 是包级 var，`regexp` 包编译后只读，并发安全。
- **文件 mode 解析严格**：仅接受合法八进制，错误立即返回；manifest 作者需注意格式。
- **不做字段值校验**：`fields` 内容原样塞入 env / file，调用方需自行做敏感字段长度 / 字符校验。
- **扩展点**：当前模板仅支持字段替换，若未来需条件逻辑或循环，需扩展为更完整的模板引擎。
