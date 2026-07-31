# `json.go` 技术实现文档

> 源文件：`internal/edgeagent/biz/json.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/biz`

## 1. 概述

本文件提供两个泛型 JSON 编解码辅助函数 `jsonEncode` 和 `jsonDecode`，把 handler 调用点的「值 + 错误」打包/解包模式收敛到一行。`jsonEncode` 特别保证错误先于序列化返回，符合 `tunnel.Handler` 契约（err != nil 时通过 SetError 发送到对端）。

## 2. 包信息

- **包名**：`biz`
- **所属模块**：edgeagent 业务工具层
- **依赖方向**：被同包 `agent.go` 的 handler 闭包使用

## 3. 关键类型与接口

无类型定义。仅是泛型函数。

## 4. 关键函数与流程

### `jsonEncode`
- **签名**：`func jsonEncode[T any](v T, err error) ([]byte, error)`
- **职责**：把「(值, 错误)」二元组打包成 `([]byte, error)`
- **流程**：err != nil 直接返回 `(nil, err)`；否则 `json.Marshal(v)`
- **错误处理**：err 优先于 marshal 错误返回，避免吃掉上游错误

### `jsonDecode`
- **签名**：`func jsonDecode(body []byte, out any) error`
- **职责**：`json.Unmarshal` 的语义别名，保持调用点对称
- **流程**：直接 `json.Unmarshal(body, out)`

## 5. 依赖关系

- **内部包**：无
- **外部库**：标准库 `encoding/json`
- **被调用方**：同包 `agent.go`（registerHandlers / get_host_load / execute_skill 等 handler 闭包）

## 6. 并发与资源管理

无并发控制。函数纯无状态。

## 7. 设计模式与亮点

- **泛型「pack (value, err)」**：Go 1.18+ 泛型让 handler 调用点压缩到一行 `return jsonEncode(a.collector.GetHostLoad(ctx))`，避免每个 handler 都写 `if err != nil { return nil, err }; return json.Marshal(v)`
- **错误优先传播**：`if err != nil { return nil, err }` 保证上游 collector 错误不被 `json.Marshal` 的零值掩盖，符合 tunnel.Handler 的 SetError 语义

## 8. 注意事项

- `jsonEncode` 接收 `err error` 是显式的——若调用方忘记传 err（如直接 `jsonEncode(v, nil)`），错误会被静默吃掉；调用点应保持 `jsonEncode(fn(...))` 的「整体接收函数返回」形式
- 不处理 NaN/Inf 等 JSON 不支持的值；指标层面由 `mapper.go` 的 `appendPromSample` 负责过滤
