# `http_node.go` 技术实现文档

> 源文件：`internal/manager/biz/flow/http_node.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/flow`

## 1. 概述

本文件实现 HTTP Request 节点（NodeHTTP）。让 flow 调用外部 HTTP API：method/url/headers/body 都接受 `{{...}}` 模板（engine Execute 前解析）。输出 status/body（JSON 可解析则 parsed，否则 raw text）/headers —— 可被下游引用。**4xx/5xx 是真实响应不是 transport error**：surface 在 normal port，让 flow 用 condition 分支 `{{nodes.http.output.status}} >= 400` 而非死胡同。

## 2. 包信息

- **包名**：`flow`
- **所属模块**：`internal/manager/biz/`
- **依赖方向**：被 `noderegistry.go::registerBuiltins` 注册为 NodeHTTP executor；依赖 `bytes`、`encoding/json`、`net/http`、`time`

## 3. 关键类型与接口

```go
const httpNodeMaxBody = 1 << 20  // 1 MiB
```

无导出类型；`execHTTP` 是 `ExecuteFunc` 实现。

## 4. 关键函数与流程

### `execHTTP`
- **签名**：`func execHTTP(ctx, _ Executors, cfg map[string]any, _ *RunContext) (NodeResult, error)`
- **职责**：执行 HTTP 请求；返回 status/body/headers
- **流程**：
  1. method = `toStr(cfg["method"])` Upper + Trim；空 → GET
  2. url = Trim `toStr(cfg["url"])`；空 → error "url is empty"
  3. body 处理：
     - string → 非空 → `strings.NewReader`
     - 其他 → `json.Marshal` → `bytes.NewReader`
  4. timeout 解析：
     - float64 → `time.Duration(tv) * time.Second`（tv>0）
     - string → `time.ParseDuration(tv + "s")`
     - 默认 30s；上限 120s
  5. `context.WithTimeout(ctx, timeout)`
  6. `http.NewRequestWithContext(hctx, method, url, body)`
  7. headers 遍历 `cfg["headers"]` map → `req.Header.Set`
  8. body 非 nil 且无 Content-Type → 设 `application/json`
  9. `http.DefaultClient.Do(req)`；err → error "http node: %s %s: %w"（transport error）
  10. `io.LimitReader(resp.Body, httpNodeMaxBody)` + `io.ReadAll`
  11. `json.Unmarshal(raw, &parsed)` 失败 → parsed = string(raw)（非 JSON 保 text）
  12. headers map：遍历 resp.Header → `headers[k] = resp.Header.Get(k)`
  13. 返回 `NodeResult{Output: {status, body, headers}, Port: PortNext}`
- **关键设计**：4xx/5xx 走 normal port（不报错）；transport error 走 error port
- **错误处理**：url 空 / NewRequest 失败 / Do 失败 → error；4xx/5xx 不报错

### `toStr`
- **签名**：`func toStr(v any) string`
- **职责**：any → string
- **流程**：nil → ""；string → 自身；其他 → `fmt.Sprint(t)`

## 5. 依赖关系

- **外部库**：`bytes`、`context`、`encoding/json`、`fmt`、`io`、`net/http`、`strings`、`time`
- **被调用方**：`noderegistry.go::registerBuiltins`（注册为 NodeHTTP executor）

## 6. 并发与资源管理

- **`http.DefaultClient`**：共享；无连接池配置（用默认）
- **`context.WithTimeout`**：per-request deadline；defer cancel
- **`io.LimitReader` 1 MiB**：防 giant response blow up run
- **`defer resp.Body.Close()`**：防连接泄漏

## 7. 设计模式与亮点

- **4xx/5xx 是真实响应**：注释明示"surface it on the normal port with the status, so the flow can branch on it via a condition ({{nodes.http.output.status}} >= 400) rather than dead-ending"
- **transport error vs HTTP error 区分**：网络/超时 → NodeResult error（走 error port）；4xx/5xx → normal port（可 condition 分支）
- **body 1 MiB 上限**：防 giant payload blow up run；1 MiB 足够 automation step
- **timeout 30s 默认 120s 上限**：防长时间挂起；操作员可 30-120s 间调
- **JSON 可解析则 parsed**：下游可直接 `{{nodes.http.output.body.field}}`；非 JSON 保 string
- **headers map 扁平化**：多值 header 取第一个（`resp.Header.Get`）；下游 `{{nodes.http.output.headers.Content-Type}}`
- **Content-Type 默认 application/json**：body 非 nil 且未设 → 自动补；常见 JSON API 场景友好
- **body 支持 string 或 object**：string 原样；object json.Marshal

## 8. 注意事项

- **1 MiB body 上限**：超出截断；操作员应预期大 response 可能不完整
- **timeout 30s 默认 120s 上限**：操作员设 >120s 会被截到 120s
- **4xx/5xx 不报错**：flow 需用 condition `{{nodes.http.output.status}} >= 400` 分支处理
- **transport error 报错**：网络/超时/DNS 失败 → NodeResult error；走 error port
- **headers 多值取第一个**：`resp.Header.Get` 返回第一个值；多值 header 信息丢失
- **http.DefaultClient 无定制**：无 retry / 无连接池调优；复杂场景需改
- **body string 原样**：不 marshal；操作员可传纯文本 body
- **body object json.Marshal**：自动 JSON 编码；Content-Type 自动补 application/json
- **timeout string 解析**：操作员可传 "30" 或 "30s"；`time.ParseDuration(tv + "s")` 兼容纯数字
