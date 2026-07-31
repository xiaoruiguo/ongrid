# `fakes.go` 技术实现文档

> 源文件：`tests/e2e/testenv/fakes.go`
> 包路径：`github.com/ongridio/ongrid/tests/e2e/testenv`

## 1. 概述

本文件实现 e2e 测试用的 4 个 fake 外部服务：FakeLLM（OpenAI/Anthropic 兼容 chat completions）、FakeSlack（incoming webhook 捕获）、FakeTelegram（Bot API getUpdates/sendMessage/editMessageText）、FakeProm（Prometheus query/query_range）。核心设计：每个 fake 用 `httptest.Server` 实现，无真实业务逻辑，返回固定 canned 响应让测试断言"got AN answer"而非"got THIS answer"；通过 mutex 保护 captures 的并发读写。红线：fake 不实现真实 LLM/Prom 语义，仅满足 manager 客户端的 wire format 契约。

## 2. 包信息

- **包名**：`testenv`（与 env.go 同包）
- **所属模块**：`tests/e2e/testenv`
- **依赖方向**：被 env.go 构造、被 e2e 测试用例访问；依赖标准库 `net/http` / `net/http/httptest` / `encoding/json` / `sync` / `time` / `strconv` / `strings`

## 3. 关键类型与接口

```go
type FakeLLM struct {
    server    *httptest.Server
    mu        sync.Mutex
    reply     string
    calls     int
    gotModels []string  // 每次请求的 model 参数，按序
}

type FakeSlack struct {
    server   *httptest.Server
    mu       sync.Mutex
    captures []SlackCapture
}

type SlackCapture struct {
    Path, RawQuery string
    Headers        http.Header
    Body           map[string]any
}

type FakeTelegram struct {
    server  *httptest.Server
    mu      sync.Mutex
    updates []map[string]any  // 入站 FIFO 队列
    sent    []map[string]any  // 出站 sendMessage
    edited  []map[string]any  // 出站 editMessageText
    nextID  int
}

type FakeProm struct {
    server  *httptest.Server
    mu      sync.Mutex
    series  map[string][][2]any       // query_range: query → samples
    instant map[string][]InstantEntry // query: query → entries
}

type InstantEntry struct {
    Labels map[string]string
    Value  float64
}
```

## 4. 关键函数与流程

### FakeLLM

- **`NewFakeLLM()`**：reply 默认 "PONG — fake LLM canned reply..."；mux 注册 `/v1/chat/completions`（openaiChat）+ `/v1/messages`（anthropicMessages）；`httptest.NewServer(mux)`。
- **`URL()` / `Close()`**：返回 server.URL / 关闭。
- **`SetLLMReply(s)`**：加锁替换 reply（测试可覆盖 canned 响应做断言）。
- **`CallCount()`**：加锁返回 calls。
- **`ModelsRequested()`**：加锁拷贝 gotModels 切片返回（防外部修改）。
- **`openaiChat(w, r)`**：decode `{model}`；加锁 calls++ + append gotModels + 取 reply；构造 OpenAI chat completion 响应（choices[0].message.content=reply，usage prompt_tokens=42/completion_tokens=8/total=50）；Content-Type: application/json；encode 写回。
- **`anthropicMessages(w, r)`**：decode `{model}`；同上计数 + 取 reply；构造 Anthropic message 响应（content=[{type:text, text:reply}]，usage input_tokens=42/output_tokens=8，stop_reason=end_turn）。

### FakeSlack

- **`NewFakeSlack()`**：`httptest.NewServer(handler)`。
- **`URL()`**：host 部分。
- **`WebhookURL()`**：返回完整 Slack 形态 webhook URL（`<URL>/services/T0FAKE/B0FAKE/abcdef`）。
- **`Captures()`**：加锁拷贝 captures 切片返回。
- **`handle(w, r)`**：decode body 为 map；加锁 append SlackCapture{Path, RawQuery, Headers: cloneHeader(r.Header), Body}；返回 200 + "ok"（真实 Slack 行为）。

### FakeTelegram

- **`NewFakeTelegram()`**：nextID=100；`httptest.NewServer(handler)`。
- **`URL()` / `Close()`**。
- **`PushUpdate(text, fromID, chatID)`**：chatID==0 默认=fromID（DM）；加锁 nextID++ + append update（message_id / from{id,first_name:"TestUser"} / chat{id,type:"private"} / text）。
- **`SentMessages()`**：加锁拷贝 sent 返回。
- **`handle(w, r)`**：
  - 解析 path `/bot<TOKEN>/<method>`；不匹配 404。
  - `getUpdates`：加锁取 out=updates + 清空；返回 `{ok:true, result:out}`。
  - `sendMessage`：decode body；加锁 append sent + nextID++；返回 `{ok:true, result:{message_id}}`。
  - `editMessageText`：decode body；加锁 append edited；返回 `{ok:true, result:true}`。
  - default：`{ok:true, result:{}}`。

### FakeProm

- **`NewFakeProm()`**：series / instant 各自 map；mux 注册 `/api/v1/query_range` + `/api/v1/query`。
- **`URL()` / `Close()`**。
- **`SetSeries(query, samples)`**：加锁写 series[query]。
- **`SetInstant(query, entries)`**：加锁写 instant[query]。
- **`queryRange(w, r)`**：`readQueryParam(r)` 取 query；加锁取 samples；构造 Prom matrix 响应（resultType=matrix，result=[{metric:{}, values:samples}] 或空）；writeOK。
- **`queryInstant(w, r)`**：`readQueryParam(r)`；加锁取 entries；逐条转 Prom vector 格式（metric=Labels，value=[ts, FormatFloat]）；writeOK。
- **`readQueryParam(r)`**：先查 URL query 的 `query`；空则 `r.ParseForm()` 取 form body 的 `query`（Prom 客户端用 POST application/x-www-form-urlencoded）。

### 辅助

- **`writeOK(w, body)`**：Content-Type: application/json；encode + write。
- **`cloneHeader(h)`**：深拷贝 http.Header（防外部修改）。

## 5. 依赖关系

- **内部包**：无
- **外部库**：标准库（`net/http` / `net/http/httptest` / `encoding/json` / `sync` / `time` / `strconv` / `strings`）
- **被调用方**：env.go 构造；e2e 测试用例通过 `env.FakeLLM()` 等访问

## 6. 并发与资源管理

- **每个 fake 一个 `sync.Mutex`**：保护 captures / calls / gotModels / updates / sent / edited / series / instant。
- **拷贝返回防外部修改**：`ModelsRequested()` / `Captures()` / `SentMessages()` 都加锁 + copy 后返回。
- **httptest.Server 自带并发安全**：多测试 goroutine 可并发访问同一 fake。
- **Close 幂等**：httptest.Server.Close() 可多次调用（第二次 no-op）。

## 7. 设计模式与亮点

- **canned 响应 + 可覆盖**：默认固定 reply；`SetLLMReply` / `SetSeries` / `SetInstant` 让测试按需定制；满足"got AN answer"而非"got THIS answer"的测试哲学。
- **双 path OpenAI/Anthropic**：FakeLLM 同时服务 `/v1/chat/completions`（OpenAI 形态）和 `/v1/messages`（Anthropic 形态）；注释明示"manager router only picks the right one"。
- **SlackCapture 保留 RawQuery**：注释解释 DingTalk 在 URL query 签名（?timestamp=…&sign=…），Slack/Feishu 在 JSON body 签名；保留 RawQuery 让 DingTalk 测试可断言。
- **FakeTelegram 双向**：PushUpdate 模拟入站用户消息（getUpdates long-poll 取出）；SentMessages 捕获出站 sendMessage；让 bridge 测试像真实 Telegram 流量。
- **FakeProm 双 endpoint 不同 resultType**：query 返回 vector（alert evaluator 用）；query_range 返回 matrix（grafana-shaped panel 用）；注释明示"predicate baked into PromQL" —— SetInstant 的 entry 存在即表示 rule fires。
- **readQueryParam 双形态**：GET query string 或 POST form body；注释明示"real client sends POST application/x-www-form-urlencoded"。
- **cloneHeader 深拷贝**：防测试修改 captures 中的 header 影响后续请求。
- **nextID 自增**：FakeTelegram 从 100 开始，模拟真实 Telegram update_id / message_id 单调递增。

## 8. 注意事项

- **`//go:build e2e` 标签**：与 env.go 同包，仅 e2e 构建包含。
- **FakeLLM 不实现 streaming**：仅同步 completion；不返回 `stream:true` 响应。
- **FakeLLM usage 固定**：prompt_tokens=42 / completion_tokens=8 / total=50；Anthropic input=42 / output=8；测试若断言 token 数需知此固定值。
- **FakeSlack WebhookURL 路径固定**：`/services/T0FAKE/B0FAKE/abcdef`；测试若需多 channel 需自行构造不同 path。
- **FakeTelegram PushUpdate 的 fromID**：必须与 allow_from 配置匹配；chatID==0 默认 DM。
- **FakeProm SetSeries samples 是 `[2]any`**：Prom 形态 `[unix_ts, "value-string"]`；测试构造时需注意 value 是字符串。
- **FakeProm query 未命中返回空成功**：matrix 空 result / vector 空 result；不是错误。
- **FakeProm queryInstant 的 ts**：用 `time.Now().Unix()`；测试若需固定 ts 需自行调整。
- **所有 fake 的 Close 可重复调用**：httptest.Server 安全。
- **fake 不模拟网络故障**：若测试需断言重试逻辑，需自行 wrap fake server 注入错误。
