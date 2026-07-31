# `handlers.go` 技术实现文档

> 源文件：`internal/edgeagent/service/handlers.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/service`

## 1. 概述

本文件是 edgeagent 的独立 handler 注册助手包。`RegisterWithCollector` 在 tunnel client 上安装 `get_host_load` 和 `get_process_list` 两个 cloud→edge handler，由传入的 `biz.Collector` 支持；`Register` 是 Phase 1 兼容包装（传 nil collector → 注册 stub handler 返回零值）。保留用于测试和绕过完整 Agent 的 caller。

## 2. 包信息

- **包名**：`service`
- **所属模块**：edgeagent handler 注册辅助层
- **依赖方向**：被 `cmd/ongrid-edge` 或测试代码调用；调用 `biz.Collector`、`tunnel`

## 3. 关键类型与接口

无类型定义。仅函数。

## 4. 关键函数与流程

### `Register`
- **签名**：`func Register(client tunnel.Client, log *slog.Logger)`
- **职责**：Phase 1 兼容入口，注册 stub handler
- **流程**：直接调 `RegisterWithCollector(client, nil, log)`
- **错误处理**：无错误返回

### `RegisterWithCollector`
- **签名**：`func RegisterWithCollector(client tunnel.Client, collector biz.Collector, log *slog.Logger)`
- **职责**：注册 handler，collector 为 nil 时降级 stub
- **流程**：
  1. log 为 nil → `slog.Default()`
  2. collector 为 nil → `registerStubs(client, log)` 返回
  3. 注册 `MethodGetHostLoad` handler：调 `collector.GetHostLoad(ctx)` → `json.Marshal(v)` 返回
  4. 注册 `MethodGetProcessList` handler：解码 body → 默认 TopN=20 / SortBy=CPU → 调 `collector.GetProcessList` → `json.Marshal(v)`
- **错误处理**：handler 闭包内 collector 错误直接返回；JSON 解码错误返回

### `registerStubs`
- **签名**：`func registerStubs(client tunnel.Client, log *slog.Logger)`
- **职责**：安装零值响应 handler
- **流程**：
  1. `MethodGetHostLoad` → log Debug + `json.Marshal(tunnel.GetHostLoadResponse{})`
  2. `MethodGetProcessList` → log Debug + `json.Marshal(tunnel.GetProcessListResponse{Processes: []tunnel.ProcessInfo{}})`
- **错误处理**：stub 永不返回错误

## 5. 依赖关系

- **内部包**：`internal/edgeagent/biz`（用 `Collector` 接口）、`internal/pkg/tunnel`
- **外部库**：标准库 `context`、`encoding/json`、`log/slog`
- **被调用方**：`cmd/ongrid-edge` 主程序（或测试代码）

## 6. 并发与资源管理

无并发控制。handler 闭包无状态，每次调用独立。`collector` 是接口，实现自身负责线程安全。

## 7. 设计模式与亮点

- **stub 降级**：collector 为 nil 时降级 stub——让 dev box 无真实 collector 也能启动 agent（mid-rollout 场景）
- **handler 默认值**：TopN=0 → 20、SortBy="" → CPU——让调用方不必传参
- **保留向后兼容**：`Register` 是 Phase 1 入口，新代码用 `RegisterWithCollector`；避免破坏老 caller
- **包注释明确职责边界**：service 是「唯一 speaks tunnel body wire format」的 edgeagent 包——但 `biz.Agent.registerHandlers` 也注册 handler（包括 get_host_load / get_process_list），两者职责重叠；service 包更像是「不依赖完整 Agent 的轻量注册路径」

## 8. 注意事项

- **职责与 `biz.Agent.registerHandlers` 重叠**：`biz.Agent` 也注册 `get_host_load` / `get_process_list` handler；两者同时调用会让后注册的覆盖前者。`cmd/ongrid-edge` 应只调用其一——推荐用 `biz.Agent`，service 包保留为兼容 / 测试
- stub 返回零值 `GetHostLoadResponse{}`——调用方需容忍全零字段（CPUPct=0 等）
- stub 返回 `Processes: []tunnel.ProcessInfo{}`（空切片而非 nil）——让调用方 `len(resp.Processes)==0` 判断空
- handler 闭包捕获 `collector` / `log` 变量——多次调用 `RegisterWithCollector` 会覆盖前次注册的 handler（tunnel 层 idempotent）
- `GetProcessListRequest` body 为空时跳过解码，仍用默认 TopN/SortBy——支持无参调用
- 若 collector 在运行时被替换（如热重载），handler 仍持有旧引用——需重新调 `RegisterWithCollector` 才能生效
