# `redact.go` 技术实现文档（k8sredact）

> 源文件：`internal/pkg/k8sredact/redact.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/k8sredact`

## 1. 概述

该文件实现 Kubernetes 资源（labels / annotations / 文本字段）的敏感信息脱敏，避免凭据明文进入日志 / metrics / 数据库。提供三个入口：`Text` 脱敏文本中的内联凭据、`StringMap` 脱敏 labels / annotations 副本、`IsSensitiveKey` 判断字段名是否常见携带凭据。

## 2. 包信息

- **包名**：`k8sredact`
- **所属模块**：`internal/pkg/`（基础设施层）
- **依赖方向**：被 manager k8s event / 资源同步代码调用；仅依赖标准库。

## 3. 关键类型与接口

无显著类型定义（仅顶层函数与两个包级正则）。

### 包级正则

```go
var sensitiveTextPattern = regexp.MustCompile(`(?i)(authorization|token|password|passwd|secret|api[_-]?key|client[_-]?secret)\s*[:=]\s*([^\s,;]+)`)
var credentialURLPattern = regexp.MustCompile(`(?i)(https?://[^:/\s]+:)[^@/\s]+@`)
```

- `sensitiveTextPattern`：匹配 `authorization: Bearer xxx` / `password=secret` 等内联 key-value 形态。
- `credentialURLPattern`：匹配 `https://user:pass@host` 形态的 URL 凭据。

## 4. 关键函数与流程

### `Text`
- **签名**：`func Text(value string) string`
- **职责**：脱敏文本中的内联凭据，保留上下文。
- **流程**：
  1. `sensitiveTextPattern.ReplaceAllString(value, "$1=[REDACTED]")`——把 `authorization: Bearer xxx` 替换为 `authorization=[REDACTED]`。
  2. `credentialURLPattern.ReplaceAllString(value, "$1[REDACTED]@")`——把 `https://user:pass@host` 替换为 `https://user:[REDACTED]@host`。
- **设计理由**：保留 key 名与 URL 结构便于运维识别字段含义，仅替换值。

### `StringMap`
- **签名**：`func StringMap(values map[string]string) map[string]string`
- **职责**：返回脱敏后的 labels / annotations 副本。
- **流程**：
  1. 空 map 原样返回（避免分配）。
  2. 新建 out map，遍历：
     - `IsSensitiveKey(key)` 为真 → value 替换为 `"[REDACTED]"`。
     - 否则 → `Text(value)` 脱敏内联凭据。
- **设计理由**：keys 通常暗示字段语义（如 `authorization-token`），整值脱敏；普通 key 仅脱敏 value 中的内联凭据。

### `IsSensitiveKey`
- **签名**：`func IsSensitiveKey(key string) bool`
- **职责**：判断字段名是否常见携带凭据。
- **流程**：
  1. `strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(key))`——归一化：小写 + 去除 `-` 与 `_`。
  2. 遍历 marker 列表（`authorization` / `token` / `password` / `passwd` / `secret` / `apikey` / `credential` / `privatekey`），任一 `strings.Contains` 命中 → true。
- **设计理由**：归一化处理 `authorization-token` / `authorization_token` / `AuthorizationToken` 等变体统一命中。

## 5. 依赖关系

- **内部包**：无。
- **外部库**：标准库 `regexp` / `strings`。
- **被调用方**：manager k8s event 同步、资源 labels / annotations 入库 / 入日志前。

## 6. 并发与资源管理

无并发控制。包级正则在 `regexp.MustCompile` 后只读，并发安全；`Text` / `StringMap` / `IsSensitiveKey` 均为纯函数，无共享状态。`StringMap` 总是新建 map 副本，不修改输入。

## 7. 设计模式与亮点

- **双层脱敏**：key 级别整值脱敏 + value 级别内联凭据脱敏，覆盖 labels / annotations 两种泄漏形态。
- **归一化匹配**：`IsSensitiveKey` 用 `NewReplacer` 去除 `-` / `_` + `ToLower`，统一处理命名变体，减少漏检。
- **保留上下文**：脱敏保留 key 名与 URL 结构，运维仍能识别字段含义，便于诊断。
- **正则预编译**：两个正则用包级 `MustCompile` 一次编译，避免每次调用重新编译的开销。
- **空 map 短路**：`StringMap` 对空输入原样返回，避免无谓分配。
- **marker 列表可扩展**：新增敏感字段类型只需在 `IsSensitiveKey` 的 slice 加一项。

## 8. 注意事项

- **正则非穷尽**：仅覆盖常见模式；非标准字段名（如自定义 token 字段）可能漏检，需扩展 marker 列表。
- **`Text` 误检风险**：正则可能匹配非凭据文本（如文档中讨论 password 的注释），调用方需权衡脱敏强度 vs 可读性。
- **不处理 base64 / 加密 value**：仅做明文模式匹配，base64 编码的 secret 不会被识别。
- **`StringMap` 不保留顺序**：map 遍历无序，输出 map 也无序；若调用方依赖顺序需另行排序。
- **`[REDACTED]` 占位固定**：调用方无法自定义占位符；如需不同标记需修改源码。
- **性能**：`Text` 两次正则替换 + `IsSensitiveKey` Replacer + 多次 Contains，对每个 label 都执行；大规模 labels 同步场景需评估开销。
- **不处理 nested 结构**：仅处理 `map[string]string`；嵌套 annotations（如 JSON 字符串值中的凭据）需调用方先展开。
