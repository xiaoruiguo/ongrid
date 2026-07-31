# `dispatcher.go` 技术实现文档

> 源文件：`internal/edgeagent/skill/dispatcher.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/skill`

## 1. 概述

本文件是 edge 侧 `execute_skill` handler 的 dispatcher：解码 wire 请求 `{key, params}`，从共享 skill registry 查找 executor，调 `Execute(ctx, params)`，把结果（result 或 error 字符串）打包为 `{result, error}` JSON 返回。executor 错误进响应 Error 字段而非 RPC 失败——保持审计轨迹完整，让 manager 渲染错误给 operator 而非猜测传输错误 vs skill 错误。

## 2. 包信息

- **包名**：`skill`
- **所属模块**：edgeagent skill 框架接入层
- **依赖方向**：被 `biz.Agent.registerHandlers` 调用 `Dispatch`；调用 `internal/skill` 共享 registry

## 3. 关键类型与接口

无导出类型。使用匿名 struct 解码 wire 请求：

```go
var req struct {
    Key    string          `json:"key"`
    Params json.RawMessage `json:"params,omitempty"`
}
```

## 4. 关键函数与流程

### `Dispatch`
- **签名**：`func Dispatch(ctx context.Context, body []byte) ([]byte, error)`
- **职责**：execute_skill 的 body handler
- **流程**：
  1. `json.Unmarshal(body, &req)`；失败返回 `fmt.Errorf("decode execute_skill body: %w", err)`
  2. `req.Key == ""` → `marshalResp(nil, "execute_skill: key required")`
  3. `skill.Get(req.Key)` 查 registry；unknown → `marshalResp(nil, "execute_skill: unknown skill %q")`
  4. `exec.Execute(ctx, req.Params)` 执行；error → `marshalResp(nil, err.Error())`
  5. 成功 → `marshalResp(result, "")`
- **错误处理**：只有 wire 解码失败返回 RPC error；executor 错误进响应 Error 字段

### `marshalResp`
- **签名**：`func marshalResp(result json.RawMessage, errMsg string) ([]byte, error)`
- **职责**：打包 `{result, error}` 响应
- **流程**：`json.Marshal(struct{Result json.RawMessage; Error string}{...})`；marshal 自身失败返回错误（罕见）

## 5. 依赖关系

- **内部包**：`internal/skill`（共享 registry，`skill.Get` + `Executor` 接口）
- **外部库**：标准库 `context`、`encoding/json`、`fmt`
- **被调用方**：`biz.Agent.registerHandlers` 注册的 `MethodExecuteSkill` handler 闭包

## 6. 并发与资源管理

无并发控制。`Dispatch` 是无状态函数；`skill.Get` 是 registry 读操作（registry 自身负责线程安全）。`exec.Execute` 由具体 skill 实现负责并发安全。

## 7. 设计模式与亮点

- **executor 错误进响应而非 RPC error**：让 manager 区分「传输错误」（RPC fail）和「skill 执行错误」（响应 Error 字段）——保持审计轨迹完整，manager 可渲染错误给 operator
- **共享 registry**：edge 和 manager 共用 `internal/skill` 包；skill 通过 init() 注册到 registry；agent 只需 import builtin skill 包触发注册
- **`json.RawMessage` 透传 params**：Dispatch 不解析 params 结构——每个 skill executor 自己解码；支持异构 skill 参数
- **`marshalResp` 统一响应形状**：`{result, error}` 是 execute_skill 的固定 wire 契约；result 是 RawMessage（可能是任意 JSON）
- **key 必填校验**：空 key 直接返回错误响应而非查询 registry——快速失败

## 8. 注意事项

- **registry 注册时机**：skill 通过 `init()` 注册；agent 必须 import builtin skill 包（如 `internal/skill/builtin/*`）触发注册；否则 `skill.Get` 返回 unknown
- **executor 错误信息泄露**：error 字符串直接进响应——executor 应避免在 error 中包含敏感信息（如路径、密钥）
- **params 是 RawMessage**：executor 需自行 `json.Unmarshal(params, &myReq)`；空 params 时是 `nil`，executor 需容忍
- **result 是 RawMessage**：executor 返回的 `[]byte` 必须是有效 JSON；否则 `marshalResp` 的 `json.Marshal` 会失败
- **无超时**：Dispatch 不加超时；由上层 RPC 的 ctx 控制；executor 应尊重 ctx
- **无并发限制**：Dispatch 不限制同时执行的 skill；每个 RPC 在独立 goroutine，executor 自负责资源管理
- 添加新 skill：写一个 `Executor` 实现 + 在 init() 调 `skill.Register(key, executor)` + 让 agent import 该包
- 当前 dispatcher 仅服务 edge 侧；manager 侧的 execute_skill 调用通过 frontier tunnel 到达此 handler
