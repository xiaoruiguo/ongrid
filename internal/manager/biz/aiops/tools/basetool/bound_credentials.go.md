# `bound_credentials.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/basetool/bound_credentials.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools/basetool`

## 1. 概述

本文件实现 session 激活 skills 绑定的凭据名（HLD-017 设计期凭据绑定）的 ctx 传播。runtime 解析激活的 skills、查每个 skill 的 pack 级绑定，把结果（凭据名切片）attach 到 ctx。`cloud_bash` 在 propose 阶段读取该列表，让 queued approval 在 exec 时注入对应凭据——绑定在 install 时决定，不由 LLM 或用户在 run time 选择。

## 2. 包信息

- **包名**：`basetool`
- **所属模块**：叶子包，与 locale.go 同样的反循环依赖设计
- **依赖方向**：被 chatruntime（set）、`tools/cloud_bash`（read）调用；仅依赖标准库 `context`

## 3. 关键类型与接口

```go
type boundCredsCtxKeyT struct{}
var boundCredsCtxKey = boundCredsCtxKeyT{}
```

ctx key 使用空 struct 类型实例，避免 key 冲突。

## 4. 关键函数与流程

### `WithBoundCredentials`
- **签名**：`func WithBoundCredentials(ctx context.Context, names []string) context.Context`
- **职责**：attach 激活 skills 绑定的凭据名切片
- **流程**：`len(names) == 0` → no-op 返回原 ctx；否则 `context.WithValue` attach 切片
- **错误处理**：无错误返回

### `BoundCredentialsFromContext`
- **签名**：`func BoundCredentialsFromContext(ctx context.Context) []string`
- **职责**：取出凭据名切片，无则返回 nil
- **流程**：类型断言 `ctx.Value(key).([]string)`，失败返回 nil

## 5. 依赖关系

- **内部包**：无
- **外部库**：标准库 `context`
- **被调用方**：`chatruntime`（producer）、`tools/cloud_bash_basetool.go`（consumer）

## 6. 并发与资源管理

- 切片是 immutable 共享引用；底层切片若被 producer 后续修改会引入数据竞争，约定 producer attach 后不再修改
- 无锁、无 channel

## 7. 设计模式与亮点

- **绑定决策时机**：install time 决定而非 run time 选择，避免 LLM/用户绕过权限边界
- **空切片 no-op**：caller 可放心传空，不污染 ctx
- **叶子包反循环**：chatruntime 和 cloud_bash 都依赖 basetool，避免 chatruntime → tools 循环

## 8. 注意事项

- **切片共享风险**：producer attach 后不得修改底层切片；若需修改应先 copy
- **nil 与空等价**：函数对 nil 和空切片都视为 no-op
- **凭据名为 string**：实际凭据值由 edge 在 exec 时按名查找，ctx 不携带明文凭据
