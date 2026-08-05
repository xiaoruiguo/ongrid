# cmd/llm 测试方案

> 本文件给出 `cmd/llm/main.go` 测试客户端的完整测试方案:覆盖 ongrid 已实现的全部 LLM 相关 HTTP 接口,不新增任何服务端代码。
> 验证日期:2026-08-05。客户端版本:`llm dev`。

## 1. 测试目标

`cmd/llm` 是 ongrid LLM API 表面的端到端测试客户端,通过纯 HTTP 调用覆盖以下功能域:

| 功能域 | 命令前缀 | 接口数量 |
|---|---|---|
| 鉴权 | `login` / `self` | 2 |
| LLM 配置(provider/model/api_key CRUD + 探针) | `llm-*` | 5 |
| 模型目录 + 用量 | `models` / `usage` | 2 |
| 聊天会话管理 | `session-*` | 4 |
| 消息发送(同步 + SSE 流式) | `chat` / `chat-stream` / `chat-stop` / `messages` | 4 |
| 查询翻译(NL → LogQL/TraceQL/PromQL) | `translate` | 1 |
| Agent persona CRUD | `agent-*` | 5 |
| Skill 列表 / 详情 / 执行 | `skill-*` | 3 |
| 知识库 / RAG(文档 CRUD + 上传 + 搜索 + Git 仓库 + Vault) | `doc-*` / `kn-*` / `repo-*` / `vault-sync` | 13 |
| MCP server CRUD + 探针 | `mcp-*` | 6 |
| Approval(审批) | `approval-*` / `approve` / `reject` | 5 |
| Mutating proposal 审计 | `proposals` | 1 |
| Mention 搜索 | `mentions` | 1 |
| Alert investigation(RCA) | `investigate` / `investigation` | 2 |
| Report 生成 + 计划 | `report-*` | 2 |
| 便捷一键问答(创建会话→发消息→关闭) | `ask` / `ask-stream` | 2 |
| 端到端冒烟测试 | `smoke` | 1 |
| **合计** | **58 个命令** | **59 个 HTTP 接口** |

## 2. 环境前置

### 2.1 工具链

| 项 | 值 |
|---|---|
| 操作系统 | Linux / macOS / Windows |
| Go 工具链 | `go1.25.0`(`go.mod` 要求,系统自带 1.24.x 需 `GOTOOLCHAIN=go1.25.0` 拉取) |
| GOPROXY | `https://goproxy.cn`(默认 `proxy.golang.org` 在国内网络环境超时) |
| 工作目录 | `/mnt/d/claude/ongrid` |

### 2.2 ongrid 服务端

`cmd/llm` 只测试已实现的接口,不启动新服务。需要 ongrid 主服务运行:

```bash
# 方式 1:docker compose(推荐,完整依赖)
cd deploy
docker compose up -d mysql frontier ongrid
# 等待 ongrid 就绪
curl http://localhost:8080/healthz  # → 200 ok

# 方式 2:本机直接跑(需自行准备 MySQL + frontier broker)
ONGRID_DB_DSN='root:root@tcp(127.0.0.1:3306)/ongrid?charset=utf8mb4&parseTime=True&loc=Local' \
ONGRID_FRONTIER_ADDR=127.0.0.1:40011 \
ONGRID_ADMIN_EMAIL=admin@ongrid.local \
ONGRID_ADMIN_PASSWORD=change-me-on-first-login \
ONGRID_JWT_SECRET=test-jwt-secret-please-change \
  go run ./cmd/ongrid
```

### 2.3 LLM provider 配置

要让 `chat` / `ask` / `translate` 等真实调用 LLM 的命令通过,需要先配置至少一个 provider。两种方式:

**方式 A:通过 ongrid SPA 设置页**(推荐)

访问 `https://localhost` → 登录 → 系统设置 → LLM 集成 → 填入 provider / api_key / model → 保存。

**方式 B:通过 `cmd/llm llm-save` 命令**

```bash
# 以 OpenAI 为例
./bin/llm llm-save \
  -provider openai \
  -api-key sk-xxxx \
  -model gpt-4o \
  -models gpt-4o,gpt-4o-mini

# 或用国内 provider(智谱 GLM)
./bin/llm llm-save \
  -provider zhipu \
  -api-key '<your-zhipu-key>' \
  -model glm-4-flash \
  -models glm-4-flash,glm-4

# 或自定义 base_url(Ollama / vLLM / OpenRouter)
./bin/llm llm-save \
  -provider custom \
  -api-key dummy \
  -base-url http://localhost:11434 \
  -model glm4:9b \
  -models glm4:9b
```

> 不配 LLM 也能跑通鉴权 / 配置查询 / 知识库 / skill 列表 / approval 等不依赖 LLM 调用的接口;`chat` / `translate` / `ask` 会返回 503 `llm-not-configured`。

## 3. 构建客户端

```bash
GOPROXY=https://goproxy.cn GOTOOLCHAIN=go1.25.0 \
  go build -o bin/llm ./cmd/llm/
```

带版本注入:

```bash
VERSION=v0.10.2
GOPROXY=https://goproxy.cn GOTOOLCHAIN=go1.25.0 \
  go build -ldflags "-X main.version=$VERSION" -o bin/llm ./cmd/llm/
```

验证:

```bash
./bin/llm -v          # → llm dev
./bin/llm --help      # → 打印命令清单(59 条)
```

## 4. 全局参数

| 参数 | 默认 | 说明 |
|---|---|---|
| `-addr` | `http://localhost:8080` | ongrid 主服务地址 |
| `-email` | `admin@ongrid.local` | 登录邮箱 |
| `-pass` | `change-me-on-first-login` | 登录密码 |
| `-token` | — | 已有 bearer token 时跳过登录 |
| `-json` | `false` | 美化 JSON 输出 |

> 所有命令(除 `login` 自身)首次执行时会自动登录获取 token,后续命令复用同一 token(进程内变量)。跨进程需用 `-token` 传入。

## 5. 测试用例

### 5.1 鉴权

#### TC-AUTH-01:登录获取 token

```bash
./bin/llm login
```

**预期**:
- 退出码 0
- stdout 输出 JWT token 字符串(以 `eyJ` 开头)
- stderr 出现 `login ok email=admin@ongrid.local`

**失败场景**:
- 错误密码 → 非 0 退出,stderr 报 `login 401: ...`
- 服务未起 → `post /v1/auth/login: dial tcp ...: connect: connection refused`

#### TC-AUTH-02:获取当前用户

```bash
./bin/llm self
```

**预期**:`HTTP 200` + JSON 含 `id` / `role` 字段,role 应为 `admin`。

#### TC-AUTH-03:无效 token 显式拒绝

```bash
./bin/llm -token invalid.jwt.token self
```

**预期**:`HTTP 401` + 错误体 `{"error":"unauthorized","code":"..."}`。

---

### 5.2 LLM 配置

#### TC-LLM-01:列出 LLM 配置

```bash
./bin/llm llm-settings
```

**预期**:`HTTP 200` + `items` 数组。每个 item 含 `category=llm` / `key` / `value`(敏感字段显示为 `****`)/ `sensitive` 字段。

#### TC-LLM-02:探针测试(provider 未保存)

```bash
./bin/llm llm-test -provider openai -api-key sk-xxx -model gpt-4o -models gpt-4o
```

**预期**(有效 key):`HTTP 200` + JSON:
```json
{"valid":true,"code":"ok","provider":"openai","model":"gpt-4o","latency_ms":423,"saved":false}
```

**预期**(无效 key):`HTTP 200` + JSON:
```json
{"valid":false,"code":"authentication-failed","detail":"...","latency_ms":120}
```

> 注意:即使探测失败也是 200,通过 `valid` 字段区分。

#### TC-LLM-03:验证并保存

```bash
./bin/llm llm-save -provider openai -api-key sk-xxx -model gpt-4o -models gpt-4o,gpt-4o-mini
```

**预期**:`HTTP 200` + 每个 model 一条结果,`saved=true`。再次跑 `llm-settings` 应能看到新增的 `openai_api_key` / `openai_default_model` / `openai_models` 等行。

#### TC-LLM-04:单行写入 system-setting

```bash
./bin/llm llm-setting openai_default_model gpt-4o-mini
```

**预期**:`HTTP 200`。再次 `llm-settings` 看到 `openai_default_model` 值为 `gpt-4o-mini`(敏感=false 不 mask)。

#### TC-LLM-05:刷新 router 缓存

```bash
./bin/llm llm-invalidate
```

**预期**:`HTTP 200` + `{"status":"ok"}`。手动改 DB 或 `llm-setting` 后立即生效。

#### TC-LLM-06:非 admin 调用 llm-save 被拒

```bash
./bin/llm -email viewer@ongrid.local -pass viewer-pass llm-save -provider openai -api-key sk-x
```

**预期**:`HTTP 403` + `{"error":"forbidden"}`。

---

### 5.3 模型目录 + 用量

#### TC-CAT-01:列出可用模型

```bash
./bin/llm models
```

**预期**:`HTTP 200` + JSON:
```json
{"providers":[{"id":"openai","label":"OpenAI","models":["gpt-4o","gpt-4o-mini"],"model":"gpt-4o"}],
 "default":{"provider":"openai","model":"gpt-4o"}}
```

未配置任何 provider 时 `providers=[]`、`default=null`。

#### TC-USE-01:今日用量

```bash
./bin/llm usage
```

**预期**:`HTTP 200` + JSON:
```json
{"date":"2026-08-05","prompt_tokens":1234,"completion_tokens":567,"total_tokens":1801,"requests":3}
```

未发起过 chat 时所有 token 字段为 0。

---

### 5.4 聊天会话管理

#### TC-SES-01:创建会话

```bash
./bin/llm session-create -title test-session-1
```

**预期**:`HTTP 201` + JSON:
```json
{"id":"<uuid>","title":"test-session-1","user_id":"...","created_at":"..."}
```

记录返回的 `id`,后续用例用 `<SID>` 表示。

#### TC-SES-02:列出会话

```bash
./bin/llm session-list -limit 5
```

**预期**:`HTTP 200` + `items` 数组含上一步创建的会话,`total >= 1`。

#### TC-SES-03:重命名会话

```bash
./bin/llm session-rename <SID> renamed-session
```

**预期**:`HTTP 204`(无响应体)或 `HTTP 200`。再次 `session-list` 看到 title 变更。

#### TC-SES-04:关闭会话

```bash
./bin/llm session-close <SID>
```

**预期**:`HTTP 204`。再次 `session-list` 应不再含该会话(或显示 `closed_at` 非空,视实现而定)。

#### TC-SES-05:访问他人会话返回 404

```bash
# 用 viewer 用户登录,尝试访问 admin 创建的会话
./bin/llm -email viewer@ongrid.local -pass viewer-pass messages <SID-admin-created>
```

**预期**:`HTTP 404`(非 403,避免暴露存在性)。

---

### 5.5 消息发送

> 以下用例需 LLM 已配置(TC-LLM-03 通过)。

#### TC-MSG-01:同步发送消息

```bash
./bin/llm chat <SID> "你好,请用一句话介绍自己"
```

**预期**:`HTTP 200` + JSON:
```json
{
  "session_id":"<SID>",
  "assistant_message":{"id":"<uuid>","content":"我是一个 AI 助手...","created_at":"..."},
  "tool_calls":[],
  "usage":{"prompt_tokens":23,"completion_tokens":18,"total_tokens":41},
  "iterations":1
}
```

#### TC-MSG-02:指定 provider/model 覆盖默认

```bash
./bin/llm chat <SID> "讲个笑话" -provider zhipu -model glm-4-flash
```

**预期**:`HTTP 200`,响应正常。可对比与默认 provider 的回复风格差异。

#### TC-MSG-03:开启 web_search 工具

```bash
./bin/llm chat <SID> "今天北京天气怎么样" -web-search
```

**预期**:`HTTP 200`,`tool_calls` 数组含 `{"name":"web_search",...}`,`content` 引用搜索结果。

#### TC-MSG-04:SSE 流式发送

```bash
./bin/llm chat-stream <SID> "用三段话介绍 Kubernetes"
```

**预期**:stdout 按事件逐行打印:
```
[assistant] {"iteration":1,"message_id":"...","content":"...","pending_tool_calls":[]}
[done] {"session_id":"...","assistant_message":{...},"usage":{...},"iterations":1}
```

事件类型至少出现 `assistant` + `done`;若 agent 调用了工具,会出现 `tool_start` / `tool_end`。

#### TC-MSG-05:中断 in-flight 一轮

```bash
# 终端 1:发起一个耗时较长的流式
./bin/llm chat-stream <SID> "写一首 500 字的长诗"

# 终端 2:立即发起 stop
./bin/llm chat-stop <SID>
```

**预期**:终端 1 流式中断;终端 2 返回 `HTTP 200` + `{"stopped":true}`。

#### TC-MSG-06:历史消息回放

```bash
./bin/llm messages <SID>
```

**预期**:`HTTP 200` + `items` 数组,包含 user / assistant 多条消息,顺序与发送一致。

#### TC-MSG-07:LLM 未配置时返回 503

```bash
# 先清空 LLM 配置
./bin/llm llm-setting openai_api_key ""
./bin/llm llm-invalidate

# 再发消息
./bin/llm chat <SID> "ping"
```

**预期**:`HTTP 503` + 错误体含 `llm-not-configured` 或 `no-api-key`。

---

### 5.6 查询翻译

#### TC-TR-01:翻译为 LogQL

```bash
./bin/llm translate -dialect logql "dev-host-3 最近的 error 日志"
```

**预期**:`HTTP 200` + JSON:
```json
{"query":"{device_id=\"3\"} |~ \"(?i)error\"","explanation":"匹配 device 3 的 error 日志","dialect":"logql"}
```

#### TC-TR-02:翻译为 PromQL

```bash
./bin/llm translate -dialect promql "主机 3 的 CPU 使用率"
```

**预期**:`HTTP 200` + `dialect=promql` + `query` 含 `node_cpu_seconds_total` 等 PromQL 指标。

#### TC-TR-03:翻译为 TraceQL

```bash
./bin/llm translate -dialect traceql "查找慢于 2 秒的 span"
```

**预期**:`HTTP 200` + `dialect=traceql` + `query` 含 `{ duration > 2s }` 类表达式。

#### TC-TR-04:未配 LLM 返回 503

清空 LLM 配置后调用 → `HTTP 503`。

---

### 5.7 Agent persona

#### TC-AGT-01:列出 agents

```bash
./bin/llm agent-list
```

**预期**:`HTTP 200` + `items` 数组,至少含 `default` / `incident-investigator` 等内置 agent。

#### TC-AGT-02:获取单个 agent

```bash
./bin/llm agent-get default
```

**预期**:`HTTP 200` + 含 `name` / `system_prompt` / `tools` / `source=builtin` 字段。

#### TC-AGT-03~05:创建 / 更新 / 删除自定义 agent

```bash
# 创建
./bin/llm agent-create -name my-agent -desc "test" -prompt "You are a test agent"
# → HTTP 201

# 更新
./bin/llm agent-update my-agent -desc "updated"
# → HTTP 200

# 删除
./bin/llm agent-delete my-agent
# → HTTP 204
```

#### TC-AGT-06:删除内置 agent 被拒

```bash
./bin/llm agent-delete default
```

**预期**:`HTTP 400` 或 `409`,错误信息含 `builtin` / `cannot delete`。

---

### 5.8 Skill

#### TC-SKL-01:列出 skills

```bash
./bin/llm skill-list
```

**预期**:`HTTP 200` + `items` 含 `web_search` / `probe_dns` / `tail_file` 等内置 skill,每条含 `key` / `name` / `class` / `scope` / `params` 字段。

#### TC-SKL-02:获取单个 skill 详情

```bash
./bin/llm skill-get web_search
```

**预期**:`HTTP 200` + 含完整 `params` schema(JSON Schema draft-07)。

#### TC-SKL-03:执行 manager-scope skill

```bash
./bin/llm skill-exec web_search -params '{"q":"golang tutorial"}'
```

**预期**:`HTTP 200` + `{"result":{...}}` 或 `{"error":"..."}`,取决于 SearXNG 是否启用。

#### TC-SKL-04:执行 host-scope skill 缺 edge_id 报错

```bash
./bin/llm skill-exec tail_file -params '{"path":"/var/log/syslog"}'
```

**预期**:`HTTP 400` + 错误信息含 `edge_id required` 或 `edge_offline`。

#### TC-SKL-05:执行 host-scope skill 指定 edge_id

```bash
./bin/llm skill-exec tail_file -edge-id 3 -params '{"path":"/var/log/syslog","lines":100}'
```

**预期**:edge 在线时 `HTTP 200` + 文件末尾内容;edge 离线时 `HTTP 503` + `edge-offline`。

---

### 5.9 知识库 / RAG

#### TC-KN-01:创建文档

```bash
./bin/llm doc-create -title "测试文档1" -content "这是一篇关于 DNS 配置的文档" -path "网络/DNS" -tags "dns,network"
```

**预期**:`HTTP 201` + JSON:
```json
{"id":"<uint64-string>","source_type":"manual","title":"测试文档1","content":"...","path":"网络/DNS","tags":["dns","network"]}
```

记录返回的 `id`,后续用 `<DID>` 表示。

#### TC-KN-02:列出文档

```bash
./bin/llm doc-list -limit 5
```

**预期**:`HTTP 200` + `items` 含上一步创建的文档。

#### TC-KN-03:按 path 过滤

```bash
./bin/llm doc-list -path "网络/DNS"
```

**预期**:仅返回该 path 下的文档。

#### TC-KN-04:按 tag 过滤

```bash
./bin/llm doc-list -tag dns
```

**预期**:仅返回带 `dns` tag 的文档。

#### TC-KN-05:获取单个文档

```bash
./bin/llm doc-get <DID>
```

**预期**:`HTTP 200` + 完整文档对象(含 content 全文)。

#### TC-KN-06:更新文档

```bash
./bin/llm doc-update <DID> -title "更新后的标题" -content "更新后的内容"
```

**预期**:`HTTP 200` + `title` / `content` 字段已变更。

#### TC-KN-07:移动文档路径

```bash
./bin/llm doc-move <DID> "网络/DNS/进阶"
```

**预期**:`HTTP 200` + `path="网络/DNS/进阶"`。

#### TC-KN-08:向量搜索

```bash
./bin/llm kn-search "DNS 怎么配置" -limit 3
```

**预期**:`HTTP 200` + `items` 数组,每条含 `doc` + `score`(0~1 浮点),score 越高越相关。应能命中 TC-KN-01 创建的文档。

#### TC-KN-09:列出所有 path

```bash
./bin/llm kn-paths
```

**预期**:`HTTP 200` + `items:[{path:"网络/DNS",count:1},...]`。

#### TC-KN-10:上传文件(multipart)

先准备一个测试文件:

```bash
echo "# 测试 Markdown 文档\n\n这是上传测试。" > /tmp/test.md
```

```bash
./bin/llm doc-upload -title "上传测试" -path "测试/上传" -tags "test,upload" /tmp/test.md
```

**预期**:`HTTP 201` + `source_type="upload"` 的文档对象。

支持的文件类型:`.md` / `.txt` / `.pdf` / `.docx`,最大 8 MiB。

#### TC-KN-11:上传超大文件被拒

```bash
# 生成一个 9 MiB 文件
dd if=/dev/zero of=/tmp/big.txt bs=1M count=9

./bin/llm doc-upload /tmp/big.txt
```

**预期**:`HTTP 413` 或 `400`,错误含 `size` / `limit`。

#### TC-KN-12:删除文档

```bash
./bin/llm doc-delete <DID>
```

**预期**:`HTTP 204`。再次 `doc-get <DID>` 返回 `HTTP 404`。

#### TC-KN-13:Git 仓库 CRUD + 同步

```bash
# 创建一个 git 仓库源
./bin/llm repo-create https://github.com/example/docs.git -branch main -desc "test repo"
# → HTTP 201,记录 id 为 <RID>

# 同步(clone + 索引)
./bin/llm repo-sync <RID>
# → HTTP 200,可能耗时较长(git clone + embedding)

# 列出
./bin/llm repo-list
# → 含刚创建的 repo

# 删除
./bin/llm repo-delete <RID>
# → HTTP 204
```

> 此用例需要 ongrid 部署机器能访问外网 git;无外网环境跳过。

#### TC-KN-14:Vault 同步

```bash
./bin/llm vault-sync
```

**预期**:`HTTP 200` + `{"file_count":N,"source":"cloud|embedded","synced_at":"..."}`。

---

### 5.10 MCP server

#### TC-MCP-01:列出 MCP server

```bash
./bin/llm mcp-list
```

**预期**:`HTTP 200` + `items` 数组(初始可能为空)。

#### TC-MCP-02:创建 MCP server

```bash
./bin/llm mcp-create -name test-mcp -transport http -endpoint http://localhost:3000/sse
```

**预期**:`HTTP 201` + JSON 含 `id` 字段,记录为 `<MID>`。

#### TC-MCP-03:获取单个

```bash
./bin/llm mcp-get <MID>
```

**预期**:`HTTP 200` + 完整对象。

#### TC-MCP-04:更新

```bash
./bin/llm mcp-update <MID> -name renamed-mcp
```

**预期**:`HTTP 200` + `{"ok":true}`。

#### TC-MCP-05:测试连接(list tools)

```bash
./bin/llm mcp-test <MID>
```

**预期**(server 在线):`HTTP 200` + `{"tools":[...],"count":N}`。
**预期**(server 离线):`HTTP 502` / `503` 或 `200 + {"error":"..."}`。

#### TC-MCP-06:删除

```bash
./bin/llm mcp-delete <MID>
```

**预期**:`HTTP 204`。

#### TC-MCP-07:非 admin 调用被拒

```bash
./bin/llm -email viewer@ongrid.local -pass viewer-pass mcp-list
```

**预期**:`HTTP 403`(MCP 全部 admin-only)。

---

### 5.11 Approval

#### TC-APR-01:列出 approvals

```bash
./bin/llm approval-list
```

**预期**:`HTTP 200` + `items` 数组(初始可能为空)。

#### TC-APR-02:count

```bash
./bin/llm approval-count
```

**预期**:`HTTP 200` + `{"count":N}`。

#### TC-APR-03~05:approve / reject(需先产生 approval)

要让 LLM agent 产生 approval,需先触发一个 mutating 工具调用:

```bash
# 创建会话 + 让 agent 调用 restart_service 等危险工具
./bin/llm chat <SID> "请重启 dev-host-3 的 nginx 服务"
```

agent 触发 `approval_pending` 事件后,从 SSE 输出中拿到 `<APR_ID>`。

```bash
./bin/llm approval-get <APR_ID>
# → HTTP 200 + 完整 approval 对象

./bin/llm approve <APR_ID>
# → HTTP 200 + {"status":"approved"}

./bin/llm reject <APR_ID>
# → HTTP 200 + {"status":"rejected"}
```

---

### 5.12 Mutating proposal 审计

#### TC-PROP-01:列出 proposals

```bash
./bin/llm proposals -limit 20
```

**预期**:`HTTP 200` + `items` 数组,每条含 `tool_name` / `decision` / `arguments_json` / `created_at`。

#### TC-PROP-02:按 tool_name 过滤

```bash
./bin/llm proposals -tool restart_service
```

**预期**:仅返回该工具的 proposal。

---

### 5.13 Mention 搜索

#### TC-MEN-01:搜索 edge

```bash
./bin/llm mentions "dev" -type edge
```

**预期**:`HTTP 200` + `items:[{type:"edge",id:"3",label:"dev-host-3"}]`。

#### TC-MEN-02:无 type 过滤

```bash
./bin/llm mentions "alert"
```

**预期**:返回所有类型的 mention。

---

### 5.14 Alert investigation(RCA)

> 需 `ONGRID_INVESTIGATOR_ENABLED=true` 且存在 incident。

#### TC-INV-01:触发调查

```bash
./bin/llm investigate <incident-id>
```

**预期**:`HTTP 202 Accepted`(异步任务,立即返回)。

#### TC-INV-02:读取调查报告

```bash
# 等待 30~120 秒让 worker 跑完
sleep 60
./bin/llm investigation <incident-id>
```

**预期**:`HTTP 200` + RCA 报告(markdown 文本)。未启用 investigator 时 `HTTP 503`。

---

### 5.15 Report

#### TC-RPT-01:立即生成报告

```bash
./bin/llm report-gen -kind daily -tz Asia/Shanghai
```

**预期**:`HTTP 202`(异步)。报告生成完成后可在 SPA `/reports` 页查看。

#### TC-RPT-02:列出报告计划

```bash
./bin/llm report-schedules
```

**预期**:`HTTP 200` + `items` 数组。

---

### 5.16 便捷命令

#### TC-ASK-01:一键问答(同步)

```bash
./bin/llm ask "什么是 Kubernetes?"
```

**预期**:
- stderr:`session created id=<uuid>` → `session closed id=<uuid>`
- stdout:LLM 回复内容(纯文本)
- stderr:`usage prompt=12 completion=34 total=46 iterations=1`

#### TC-ASK-02:一键问答(流式)

```bash
./bin/llm ask-stream "讲个笑话"
```

**预期**:stdout 按事件流式打印 `[assistant] ...` `[done] ...`,结束后自动关闭会话。

#### TC-ASK-03:保留会话

```bash
./bin/llm ask "第一个问题" -keep
# 输出含 session id,后续可继续
./bin/llm chat <returned-sid> "追问第二个问题"
```

---

### 5.17 端到端冒烟测试

#### TC-SMOKE-01:全功能冒烟

```bash
./bin/llm smoke
```

**预期**:逐项打印 PASS / FAIL,最终汇总:

```
=== ongrid LLM API smoke test ===

  PASS  login
  PASS  self
  PASS  llm-settings
  PASS  models
  PASS  usage
  PASS  session-create
  PASS  session-list
  PASS  chat
  PASS  messages
  PASS  session-close
  PASS  translate
  PASS  agent-list
  PASS  skill-list
  PASS  doc-list
  PASS  kn-search
  PASS  kn-paths
  PASS  mcp-list
  PASS  approval-list
  PASS  approval-count
  PASS  proposals
  PASS  llm-invalidate

=== smoke: 21 passed, 0 failed, 8.3s ===
```

退出码 0。任一 FAIL 退出码 1,可定位失败项。

> `smoke` 不包含需要外部依赖(edge 在线 / git 仓库 / MCP server / incident)的用例,只覆盖"零依赖能跑通"的接口。

---

## 6. 端到端 happy path(完整流程)

把多个命令串起来,模拟真实用户操作流程:

```bash
#!/bin/bash
set -e
LLM=./bin/llm

# 1. 配置 LLM(首次)
$LLM llm-save -provider openai -api-key sk-xxx -model gpt-4o -models gpt-4o,gpt-4o-mini

# 2. 创建会话
SID=$($LLM -json session-create -title "e2e-flow" | grep -oP '"id":"[^"]+"' | head -1 | cut -d'"' -f4)
echo "session: $SID"

# 3. 上传知识库文档
echo "# 故障排查手册\n\nnginx 启动失败时检查端口占用。" > /tmp/doc.md
$LLM doc-upload -title "故障排查" -path "运维/故障" -tags "nginx,troubleshoot" /tmp/doc.md

# 4. 在 chat 中触发 RAG
$LLM chat $SID "nginx 启动失败怎么办?" -web-search

# 5. 流式追问
$LLM chat-stream $SID "端口 80 被占用怎么排查?"

# 6. 查历史
$LLM messages $SID

# 7. 查用量
$LLM usage

# 8. 清理
$LLM session-close $SID
```

## 7. 错误场景汇总

| 场景 | 触发方式 | 预期 |
|---|---|---|
| 服务未起 | 任何命令 | 进程退出,stderr 报 `dial tcp ...: connect: connection refused` |
| 错误密码 | `llm -pass wrong login` | 退出码非 0,`login 401` |
| 无效 token | `-token invalid xxx` | `HTTP 401` |
| 非 admin 调 admin 接口 | viewer 用户调 `llm-save` / `mcp-*` / `repo-create` | `HTTP 403` |
| 访问他人会话 | viewer 调 admin 的 `messages <sid>` | `HTTP 404`(非 403) |
| LLM 未配置 | 清空 `openai_api_key` 后调 `chat` / `translate` | `HTTP 503` |
| 上传超大文件 | `doc-upload` >8 MiB 文件 | `HTTP 413` / `400` |
| 删除内置 agent | `agent-delete default` | `HTTP 400` / `409` |
| 不存在的 session id | `messages <random-uuid>` | `HTTP 404` |
| 不存在的 doc id | `doc-get 99999` | `HTTP 404` |
| edge 离线调 host-scope skill | `skill-exec tail_file -edge-id 999` | `HTTP 503` + `edge-offline` |
| 网络断开中途 | SSE 流式中拔网线 | `scanner` 报错退出 |

## 8. 清理流程

测试产生的数据建议按以下顺序清理(避免外键约束):

```bash
LLM=./bin/llm

# 1. 关闭所有测试会话
for sid in $($LLM -json session-list -limit 100 | grep -oP '"id":"[^"]+"' | cut -d'"' -f4); do
  $LLM session-close $sid
done

# 2. 删除测试文档
for did in $($LLM -json doc-list -limit 100 -tag test | grep -oP '"id":"[^"]+"' | cut -d'"' -f4); do
  $LLM doc-delete $did
done

# 3. 删除测试 MCP server
for mid in $($LLM -json mcp-list | grep -oP '"id":"[^"]+"' | cut -d'"' -f4); do
  $LLM mcp-delete $mid
done

# 4. 删除测试 agent
$LLM agent-delete my-agent 2>/dev/null || true

# 5. 删除测试 repo
$LLM repo-delete <rid> 2>/dev/null || true
```

## 9. 测试覆盖矩阵

| 命令 | 接口 | 鉴权 | 依赖 LLM | 依赖外部 | smoke 覆盖 |
|---|---|---|---|---|---|
| `login` | POST /v1/auth/login | 无 | 否 | 否 | ✓ |
| `self` | GET /v1/self | JWT | 否 | 否 | ✓ |
| `llm-test` | POST /v1/integrations/llm/test | admin | 是* | 否 | — |
| `llm-save` | POST /v1/integrations/llm/validate-and-save | admin | 是* | 否 | — |
| `llm-invalidate` | POST /v1/integrations/llm/invalidate | admin | 否 | 否 | ✓ |
| `llm-settings` | GET /v1/system-settings?category=llm | JWT | 否 | 否 | ✓ |
| `llm-setting` | PUT /v1/system-settings/llm/{key} | admin | 否 | 否 | — |
| `models` | GET /v1/aiops/models | JWT | 否 | 否 | ✓ |
| `usage` | GET /v1/usage/today | JWT | 否 | 否 | ✓ |
| `session-create` | POST /v1/chat/sessions | JWT | 否 | 否 | ✓ |
| `session-list` | GET /v1/chat/sessions | JWT | 否 | 否 | ✓ |
| `session-rename` | PATCH /v1/chat/sessions/{id} | JWT+owner | 否 | 否 | — |
| `session-close` | DELETE /v1/chat/sessions/{id} | JWT+owner | 否 | 否 | ✓ |
| `chat` | POST /v1/chat/sessions/{id}/messages | JWT+owner | 是 | 否 | ✓ |
| `chat-stream` | POST .../messages/stream | JWT+owner | 是 | 否 | — |
| `chat-stop` | POST /v1/chat/sessions/{id}/stop | JWT+owner | 否 | 否 | — |
| `messages` | GET /v1/chat/sessions/{id}/messages | JWT+owner | 否 | 否 | ✓ |
| `translate` | POST /v1/aiops/query-translate | JWT | 是 | 否 | ✓ |
| `agent-list` | GET /v1/agents | JWT | 否 | 否 | ✓ |
| `agent-get` | GET /v1/agents/{name} | JWT | 否 | 否 | — |
| `agent-create` | POST /v1/agents/custom | JWT非viewer | 否 | 否 | — |
| `agent-update` | PATCH /v1/agents/custom/{name} | JWT非viewer | 否 | 否 | — |
| `agent-delete` | DELETE /v1/agents/custom/{name} | JWT非viewer | 否 | 否 | — |
| `skill-list` | GET /v1/skills | JWT | 否 | 否 | ✓ |
| `skill-get` | GET /v1/skills/{key} | JWT | 否 | 否 | — |
| `skill-exec` | POST /v1/skills/{key}/execute | JWT | 否 | 视 skill | — |
| `doc-list` | GET /v1/knowledge/docs | JWT | 否 | 否 | ✓ |
| `doc-get` | GET /v1/knowledge/docs/{id} | JWT | 否 | 否 | — |
| `doc-create` | POST /v1/knowledge/docs | JWT+casbin | 否 | Qdrant | — |
| `doc-update` | PATCH /v1/knowledge/docs/{id} | JWT+casbin | 否 | Qdrant | — |
| `doc-delete` | DELETE /v1/knowledge/docs/{id} | JWT+casbin | 否 | Qdrant | — |
| `doc-upload` | POST /v1/knowledge/upload | JWT+casbin | 否 | Qdrant | — |
| `doc-move` | PATCH /v1/knowledge/docs/{id}/move | JWT+casbin | 否 | Qdrant | — |
| `kn-search` | GET /v1/knowledge/search | JWT | 否 | Qdrant | ✓ |
| `kn-paths` | GET /v1/knowledge/paths | JWT | 否 | Qdrant | ✓ |
| `repo-list` | GET /v1/knowledge/repos | JWT | 否 | 否 | — |
| `repo-create` | POST /v1/knowledge/repos | JWT+casbin | 否 | git | — |
| `repo-sync` | POST /v1/knowledge/repos/{id}/sync | JWT+casbin | 否 | git+Qdrant | — |
| `repo-delete` | DELETE /v1/knowledge/repos/{id} | JWT+casbin | 否 | 否 | — |
| `vault-sync` | POST /v1/knowledge/vault/sync | JWT+casbin | 否 | Qdrant | — |
| `mcp-list` | GET /v1/mcp/servers | admin | 否 | 否 | ✓ |
| `mcp-get` | GET /v1/mcp/servers/{id} | admin | 否 | 否 | — |
| `mcp-create` | POST /v1/mcp/servers | admin | 否 | 否 | — |
| `mcp-update` | PUT /v1/mcp/servers/{id} | admin | 否 | 否 | — |
| `mcp-delete` | DELETE /v1/mcp/servers/{id} | admin | 否 | 否 | — |
| `mcp-test` | POST /v1/mcp/servers/{id}/test | admin | 否 | MCP server | — |
| `approval-list` | GET /v1/approvals | JWT | 否 | 否 | ✓ |
| `approval-count` | GET /v1/approvals/count | JWT | 否 | 否 | ✓ |
| `approval-get` | GET /v1/approvals/{id} | JWT | 否 | 否 | — |
| `approve` | POST /v1/approvals/{id}/approve | JWT | 否 | 否 | — |
| `reject` | POST /v1/approvals/{id}/reject | JWT | 否 | 否 | — |
| `proposals` | GET /v1/aiops/mutating-proposals | JWT | 否 | 否 | ✓ |
| `mentions` | GET /v1/aiops/mentions/search | JWT | 否 | 否 | — |
| `investigate` | POST /v1/alerts/incidents/{id}/investigation | JWT非viewer | 是 | incident | — |
| `investigation` | GET /v1/alerts/incidents/{id}/investigation | JWT | 否 | incident | — |
| `report-gen` | POST /v1/reports | JWT非viewer | 是 | 否 | — |
| `report-schedules` | GET /v1/report-schedules | JWT | 否 | 否 | — |
| `ask` | 组合(session+chat+close) | JWT | 是 | 否 | — |
| `ask-stream` | 组合(session+stream+close) | JWT | 是 | 否 | — |
| `smoke` | 组合(21 个零依赖接口) | JWT | 否 | 否 | — |

> `是*` 表示 llm-test/llm-save 本身调用 LLM API 做探针,但与已配置 LLM 无关。

## 10. 自动化集成

### 10.1 CI 集成(建议)

在 `.github/workflows/ci.yml` 中加 job:

```yaml
llm-api-test:
  runs-on: ubuntu-24.04
  services:
    mysql:
      image: mysql:8.0
      env:
        MYSQL_ROOT_PASSWORD: root
        MYSQL_DATABASE: ongrid
      ports: ["3306:3306"]
      options: >-
        --health-cmd="mysqladmin ping -h localhost"
        --health-interval=10s
        --health-timeout=5s
        --health-retries=5
  steps:
    - uses: actions/checkout@v4
    - uses: actions/setup-go@v5
      with:
        go-version: '1.25.x'
    - run: go build -o bin/llm ./cmd/llm/
    - run: go build -o bin/ongrid ./cmd/ongrid/
    - name: start ongrid
      run: |
        ONGRID_DB_DSN='root:root@tcp(127.0.0.1:3306)/ongrid?charset=utf8mb4&parseTime=True&loc=Local' \
        ONGRID_FRONTIER_DISABLED=true \
        ONGRID_ADMIN_EMAIL=admin@ongrid.local \
        ONGRID_ADMIN_PASSWORD=change-me-on-first-login \
        ONGRID_JWT_SECRET=ci-test-secret \
          ./bin/ongrid &
        sleep 10
    - name: smoke test
      run: ./bin/llm smoke
```

### 10.2 定期回归

把 §6 的 happy path 脚本放进 `tests/e2e/llm_client_test.sh`,定期跑(可接入 cron 或 GitHub Actions schedule)。

## 11. 已知限制

| 限制 | 说明 | 缓解 |
|---|---|---|
| 无法测试 OpenAI 兼容反向代理 | ongrid 未暴露 `/v1/chat/completions` 代理接口 | 用 `chat` / `ask` 命令代替 |
| 无法测试 token-by-token 流式 | ongrid 底层 LLM 调用始终非流式,SSE 流的是 agent 事件 | 接受现状,SSE 事件即"流" |
| 无法直接测试 embedding 接口 | ongrid 未对外暴露 HTTP `/v1/embeddings` | 通过 `doc-create` + `kn-search` 间接验证 |
| 无法直接调用 aiops 内部工具 | `query_promql` / `bash` 等只能由 LLM agent 触发 | 用 `chat` 引导 LLM 调用,从 `tool_calls` 字段观察 |
| IM 桥接(飞书 / 钉钉)无法纯 HTTP 测试 | 钉钉 / Slack 走出站长连接,飞书 webhook 需第三方回调 | 跳过,由 SPA 端集成测试覆盖 |
| MCP `stdio` transport 无法测试 | `mcp-test` 走 HTTP,stdio 需 ongrid 进程内 spawn | 仅测 `http` / `sse` transport |
| 共享报告 `/r/{token}` 无鉴权 | 不在 `cmd/llm` 命令清单 | 用 `curl` 直接验证,或后续补充命令 |

## 12. 验证清单

执行完所有测试用例后,对照下表确认:

- [ ] TC-AUTH-01~03 鉴权通过(含失败场景)
- [ ] TC-LLM-01~06 配置管理通过
- [ ] TC-CAT-01 / TC-USE-01 模型目录与用量通过
- [ ] TC-SES-01~05 会话管理通过(含 404 场景)
- [ ] TC-MSG-01~07 消息发送通过(含 SSE / 中断 / 503)
- [ ] TC-TR-01~04 查询翻译通过(三种 dialect + 503)
- [ ] TC-AGT-01~06 agent CRUD 通过(含删除内置被拒)
- [ ] TC-SKL-01~05 skill 列表/详情/执行通过
- [ ] TC-KN-01~14 知识库 CRUD + 上传 + 搜索 + 仓库 + vault 通过
- [ ] TC-MCP-01~07 MCP server CRUD + 探针通过(含 403)
- [ ] TC-APR-01~05 approval 流程通过
- [ ] TC-PROP-01~02 proposal 审计通过
- [ ] TC-MEN-01~02 mention 搜索通过
- [ ] TC-INV-01~02 alert investigation 通过(若启用)
- [ ] TC-RPT-01~02 report 通过
- [ ] TC-ASK-01~03 便捷命令通过
- [ ] TC-SMOKE-01 冒烟测试全 PASS
- [ ] §6 端到端 happy path 完整跑通
- [ ] §7 错误场景至少验证 5 项
- [ ] §8 清理流程执行,数据库无残留测试数据
