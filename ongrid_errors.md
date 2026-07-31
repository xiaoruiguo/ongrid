# OnGrid 错误分析集

> 本文档汇总 OnGrid 运行中出现的各类错误现象、根因分析与修复方案。

---

# 错误 1：Ollama 配置超时（Settings → 模型 探测失败）

> **错误现象**：以 quickstart 方式运行 ongrid，在配置 Ollama 模型时出现：
> ```
> ✗ 连接或模型响应超时 · provider did not respond before the probe deadline
> 检查网络、代理和模型负载；探测最长等待 20 秒。
> ```

---

## 1. 错误定位

### 1.1 错误码

`timeout`（`LLMProbeCodeTimeout`）

源文件：[internal/manager/biz/setting/llm_probe.go](file:///d:/claude/ongrid/internal/manager/biz/setting/llm_probe.go#L368-L392)

### 1.2 触发链路

```
浏览器 SPA（Settings → LLM 配置）
   │ POST /v1/integrations/llm/test  {provider:"custom", base_url:"http://?:11434/?", api_key:"占位", models:["..."]}
   ▼
manager HTTP handler → LLMConfigurationService.Probe
   ▼
LLMConfigProbe.probeValidated  ←  context.WithTimeout(ctx, 20s)
   ▼
llm.ProbeChatCompletion(ctx, cfg)  ←  cfg.Timeout = 20s
   ▼
openai SDK CreateChatCompletion
   ▼ POST http://<base_url>/chat/completions
   ▼ 等待响应
超时（20s 内无任何响应）  ←  根因在此之后
   ▼
context.DeadlineExceeded / net.Error.Timeout()==true
   ▼
classifyLLMProbeError → LLMProbeCodeTimeout
   ▼
"provider did not respond before the probe deadline"
```

### 1.3 触发该错误的两个判定分支

[llm_probe.go:368-392](file:///d:/claude/ongrid/internal/manager/biz/setting/llm_probe.go#L368-L392)：

```go
func classifyLLMProbeError(err error, apiKey string) (string, string) {
    if errors.Is(err, context.DeadlineExceeded) {
        return LLMProbeCodeTimeout, "provider did not respond before the probe deadline"
    }
    // ...
    var netErr net.Error
    if errors.As(err, &netErr) && netErr.Timeout() {
        return LLMProbeCodeTimeout, "provider did not respond before the probe deadline"
    }
    // ...
}
```

即：**context 超时** 或 **net.Error 且 Timeout()==true** 都会触发该错误。

### 1.4 关键约束：探测超时硬上限 20s

[llm_probe.go:48](file:///d:/claude/ongrid/internal/manager/biz/setting/llm_probe.go#L48)：

```go
const defaultLLMProbeTimeout = 20 * time.Second
```

[probe.go:35-39](file:///d:/claude/ongrid/internal/pkg/llm/probe.go#L35-L39)：

```go
if cfg.Timeout <= 0 {
    cfg.Timeout = 20 * time.Second
}
callCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
```

**注意**：这与生产 LLM 客户端的 `defaultTimeout = 120s`（[client.go:44](file:///d:/claude/ongrid/internal/pkg/llm/client.go#L44)）**不同**。探测路径专门用 20s 紧窗口，operator 期望快速反馈。Ollama 首次推理（模型冷加载）经常超过 20s。

---

## 2. 根因分析

### 2.1 根因 A：docker-compose 中 ongrid-ollama 未接入 ongrid_net（最可能）

源文件：[deploy/docker-compose.yml:365-382](file:///d:/claude/ongrid/deploy/docker-compose.yml#L365-L382)

```yaml
  ongrid-ollama:
    image: docker.io/ollama/ollama:latest
    container_name: ongrid-ollama
    environment:
      - OLLAMA_MODELS=/modelfiles
    ports:
      - 11434:11434
    volumes:
      - ollama_data:/root/.ollama
      - /mnt/d/ai/ollama_models:/modelfiles
    restart: unless-stopped
    deploy:
      resources:
        reservations:
          devices:
            - driver: nvidia
              count: all
              capabilities: [gpu]
    # ❌ 缺失 networks: - ongrid_net
```

对比其他所有服务（mysql/ongrid/frontier/nginx/prometheus/grafana/loki/tempo/qdrant）都声明了 `networks: - ongrid_net`（[docker-compose.yml:36-37, 182-183, 207-208, ...](file:///d:/claude/ongrid/deploy/docker-compose.yml#L36-L37)），**只有 `ongrid-ollama` 没有**。

**后果**：

- ongrid-ollama 只在 host 暴露 `11434`，未接入 docker bridge `ongrid_net`
- manager 容器（在 ongrid_net 内）无法通过服务名 `ongrid-ollama:11434` 解析（DNS 不通）
- manager 容器（Linux）无法通过 `host.docker.internal:11434` 访问 host（Linux 上 `host.docker.internal` 默认不工作）
- 浏览器能访问 `http://localhost:11434` 是因为 host 端口映射，但 **manager 是从容器内发起的探测请求**，与浏览器视角不同

### 2.2 根因 B：Base URL 格式问题（次可能）

#### B1. 缺 `/v1` 后缀

[client.go:298-331](file:///d:/claude/ongrid/internal/pkg/llm/client.go#L298-L331) 的 `normalizeOpenAIBaseURL` 会自动补 `/v1`：

```go
if strings.Trim(u.Path, "/") == "" {
    u.Path = "/v1"
    return u.String()
}
```

**测试覆盖**（[client_test.go:547-552](file:///d:/claude/ongrid/internal/pkg/llm/client_test.go#L547-L552)）：

| 输入 | 输出 |
|------|------|
| `http://192.168.8.5:11434` | `http://192.168.8.5:11434/v1` |
| `http://192.168.8.5:11434/` | `http://192.168.8.5:11434/v1` |
| `http://192.168.8.5:11434/v1` | `http://192.168.8.5:11434/v1`（不变） |

**但**：`validateLLMBaseURL`（[llm_probe.go:348-366](file:///d:/claude/ongrid/internal/manager/biz/setting/llm_probe.go#L348-L366)）在 normalize 之前先校验，如果用户填了带 query/fragment 的 URL 会被拒：

```go
if u.RawQuery != "" || u.Fragment != "" {
    return fmt.Errorf("base URL must not contain query or fragment")
}
```

**错误形态**：如果填 `http://ongrid-ollama:11434/v1/`（trailing slash）会被 normalize 处理掉，不会出错；但 `http://ongrid-ollama:11434?foo=bar` 会被 `invalid-base-url` 拒绝，不是 timeout。

#### B2. scheme-less 输入

[client_test.go:558](file:///d:/claude/ongrid/internal/pkg/llm/client_test.go#L558)：

```go
{"scheme-less", "192.168.8.5:11434", "192.168.8.5:11434"},
```

**注意**：scheme-less 输入 `192.168.8.5:11434` 不会被 normalize（`url.Parse` 把 `192.168.8.5` 当 scheme），SDK 会报错。但 `validateLLMBaseURL` 会先拒（`scheme must be http or https`）→ `invalid-base-url`，不是 timeout。

**所以根因 B 不是 timeout 的直接原因**，但会导致后续即使网络通了也连不上。

### 2.3 根因 C：模型冷加载超过 20s（很可能与 A 叠加）

Ollama 默认行为：

- 模型首次推理时需要从磁盘加载到 GPU/CPU 内存
- 大模型（如 qwen2.5:14b、llama3:8b）冷加载可能 30-90s
- 加载完成后的推理通常 < 5s

**探测窗口只有 20s**（[llm_probe.go:48](file:///d:/claude/ongrid/internal/manager/biz/setting/llm_probe.go#L48)），冷加载期间连接已建立但无响应，会被 `netErr.Timeout()` 判定为 timeout。

**判定方法**：观察 ongrid-ollama 容器日志：

```bash
docker logs ongrid-ollama -f
# 看是否有 "llm start" / "loading model" 之类日志
```

### 2.4 根因 D：API Key 占位符问题（不太可能但需排查）

Ollama 无需鉴权，但 ongrid UI 要求 Custom provider 必须填 API Key（[LLM.tsx:144-145](file:///d:/claude/ongrid/web/src/pages/settings/LLM.tsx#L144-L145)）：

```
无需鉴权的本地服务（如 Ollama）随便填个占位 key。
```

[llm_probe.go:220-223](file:///d:/claude/ongrid/internal/manager/biz/setting/llm_probe.go#L220-L223)：

```go
if operational && strings.TrimSpace(in.APIKey) == "" {
    result.Code = LLMProbeCodeMissingAPIKey
    return cfg, result, false
}
```

空 key 会立即返回 `missing-api-key`，不会进入网络阶段。但如果填了非空占位 key（如 `ollama`），Ollama 会忽略它，不会触发 timeout。**除非** Ollama 配置了鉴权而 key 不匹配 → 401 → `authentication-failed`，不是 timeout。

---

## 3. 排查步骤（按优先级）

### 步骤 1：确认 ongrid-ollama 是否接入 ongrid_net

```bash
docker network inspect ongrid_net --format '{{range .Containers}}{{.Name}} {{end}}'
```

**预期**：看到 `ongrid-ollama`、`ongrid`、`mysql` 等。
**实际**：很可能只看到 `ongrid`、`mysql`、`frontier` 等，**没有 `ongrid-ollama`**。

### 步骤 2：从 manager 容器内测试连通性

```bash
docker exec ongrid sh -c 'wget -qO- --timeout=5 http://ongrid-ollama:11434/api/tags 2>&1 || echo "DNS/connection failed"'
```

- **DNS 失败**（`wget: bad address`）→ 根因 A 确认
- **连接拒绝**（`Connection refused`）→ ollama 服务未启动
- **超时**（5s 无响应）→ 网络策略/防火墙
- **返回 JSON**（`{"models":[...]}`）→ 网络通，跳到步骤 3

### 步骤 3：测试 OpenAI 兼容端点

```bash
docker exec ongrid sh -c 'wget -qO- --timeout=5 http://ongrid-ollama:11434/v1/models 2>&1'
```

应返回 `{"object":"list","data":[{"id":"qwen2.5:7b","object":"model",...}]}`。

### 步骤 4：测试 Chat Completions 端点（带模型名）

```bash
docker exec ongrid sh -c 'wget -qO- --timeout=20 --post-data='\''{"model":"qwen2.5:7b","messages":[{"role":"user","content":"Reply with OK."}]}'\'' \
  -O- http://ongrid-ollama:11434/v1/chat/completions'
```

- **20s 超时** → 模型冷加载或推理慢（根因 C）
- **404** → 端点路径错（根因 B1）
- **正常返回** → 配置应能保存

### 步骤 5：观察 ongrid-ollama 启动日志

```bash
docker logs ongrid-ollama --tail 50
```

看是否有：
- `Listening on [::]:11434`（已启动）
- 模型是否已 pull：`docker exec ongrid-ollama ollama list`

---

## 4. 修复方案

### 修复 A（根因 A，最关键）：把 ongrid-ollama 加入 ongrid_net

编辑 [deploy/docker-compose.yml](file:///d:/claude/ongrid/deploy/docker-compose.yml#L365-L382)，给 `ongrid-ollama` 加 `networks`：

```yaml
  ongrid-ollama:
    image: docker.io/ollama/ollama:latest
    container_name: ongrid-ollama
    environment:
      - OLLAMA_MODELS=/modelfiles
    ports:
      - 11434:11434
    volumes:
      - ollama_data:/root/.ollama
      - /mnt/d/ai/ollama_models:/modelfiles
    restart: unless-stopped
    deploy:
      resources:
        reservations:
          devices:
            - driver: nvidia
              count: all
              capabilities: [gpu]
    networks:          # ← 新增
      - ongrid_net     # ← 新增
```

应用：

```bash
make compose-down
make compose-up
# 或：docker compose -f deploy/docker-compose.yml up -d ongrid-ollama
```

### 修复 B：正确配置 Base URL

在 ongrid UI（Settings → LLM）配置 Custom provider：

| 字段 | 值 |
|------|-----|
| Provider | `custom` |
| Base URL | `http://ongrid-ollama:11434/v1`（**容器间用服务名**，不是 `localhost`） |
| API Key | `ollama`（任意非空占位符） |
| Models | `qwen2.5:7b`（实际 `ollama list` 看到的 tag） |
| Default Model | `qwen2.5:7b` |

**关键**：

- ❌ 不要填 `http://localhost:11434`（manager 容器内的 localhost 是 manager 自己）
- ❌ 不要填 `http://127.0.0.1:11434`（同上）
- ❌ 不要填 `http://192.168.x.x:11434`（host IP 在 Linux 容器内不可达，除非额外配置）
- ✅ 填 `http://ongrid-ollama:11434/v1`（docker DNS 解析服务名）
- ✅ `/v1` 后缀可加可不加（`normalizeOpenAIBaseURL` 会自动补）

### 修复 C（根因 C）：预热模型 / 延长超时

#### 方案 C1：预热模型（推荐）

```bash
# 在 ongrid-ollama 容器内手动触发一次推理，让模型加载到内存
docker exec ongrid-ollama ollama run qwen2.5:7b "Reply with OK." --keepalive 30m
```

`--keepalive 30m` 让模型在内存保留 30 分钟，避免每次探测都冷加载。

然后再去 ongrid UI 测试连接。

#### 方案 C2：用更小的模型探测

先 pull 一个小模型用于探测：

```bash
docker exec ongrid-ollama ollama pull qwen2.5:0.5b
```

在 ongrid UI 临时用 `qwen2.5:0.5b` 探测，通过后再换大模型。0.5b 冷加载 < 5s。

#### 方案 C3：调大探测超时（不推荐，需改代码）

[llm_probe.go:48](file:///d:/claude/ongrid/internal/manager/biz/setting/llm_probe.go#L48)：

```go
const defaultLLMProbeTimeout = 20 * time.Second  // 改为 60s
```

**不推荐**：探测超时是 operator 体验设计，改大会让真故障时 operator 等更久。应该修根因 A/B/C。

### 修复 D：确认 API Key 非空

UI 上确认 API Key 字段填了任意非空字符串（如 `ollama`、`placeholder`、`sk-xxx`）。

---

## 5. 验证流程

修复后按顺序验证：

### 5.1 验证网络

```bash
docker exec ongrid sh -c 'wget -qO- --timeout=5 http://ongrid-ollama:11434/api/tags'
# 应返回 {"models":[...]}
```

### 5.2 验证模型已加载

```bash
docker exec ongrid-ollama ollama ps
# 应看到 qwen2.5:7b 在 NAME 列，SIZE 列非 0
```

### 5.3 验证 Chat Completions

```bash
docker exec ongrid sh -c 'wget -qO- --timeout=20 --post-data='\''{"model":"qwen2.5:7b","messages":[{"role":"user","content":"Reply with OK."}]}'\'' http://ongrid-ollama:11434/v1/chat/completions'
# 应返回 {"choices":[{"message":{"content":"OK"}}],...}
```

### 5.4 在 ongrid UI 测试连接

Settings → LLM → Custom provider → 填入正确配置 → 点击"测试连接"。

**预期**：`✓ 连接正常`，延迟 < 5s。

### 5.5 实际对话验证

回到 AIOps 对话页，发一条消息，确认 LLM 正常响应。

---

## 6. 预防措施

### 6.1 修复 docker-compose.yml（建议提交 PR）

[deploy/docker-compose.yml:365-382](file:///d:/claude/ongrid/deploy/docker-compose.yml#L365-L382) 的 `ongrid-ollama` 服务应加入 `networks: - ongrid_net`，与其他所有服务一致。

### 6.2 UI 提示优化建议

[LLM.tsx:144-147](file:///d:/claude/ongrid/web/src/pages/settings/LLM.tsx#L144-L147) 的 hint 已提到 Ollama 示例，但可补充：

- "Base URL 在 docker-compose 部署时填 `http://ongrid-ollama:11434/v1`（服务名），不是 `localhost`"
- "首次探测可能因模型冷加载超时，建议先用 `ollama run` 预热模型"

### 6.3 文档建议

[deploy/README.md](file:///d:/claude/ongrid/deploy/README.md) 的 Quickstart 章节可补充 Ollama 配置示例，特别是"容器间用服务名"这一易错点。

---

## 7. 相关代码索引

| 文件 | 行 | 职责 |
|------|----|------|
| [internal/manager/biz/setting/llm_probe.go](file:///d:/claude/ongrid/internal/manager/biz/setting/llm_probe.go#L48) | 48 | `defaultLLMProbeTimeout = 20s` |
| [internal/manager/biz/setting/llm_probe.go](file:///d:/claude/ongrid/internal/manager/biz/setting/llm_probe.go#L297-L331) | 297-331 | `probeValidated` 主流程 |
| [internal/manager/biz/setting/llm_probe.go](file:///d:/claude/ongrid/internal/manager/biz/setting/llm_probe.go#L368-L392) | 368-392 | `classifyLLMProbeError` 超时判定 |
| [internal/manager/biz/setting/llm_probe.go](file:///d:/claude/ongrid/internal/manager/biz/setting/llm_probe.go#L348-L366) | 348-366 | `validateLLMBaseURL` URL 校验 |
| [internal/pkg/llm/probe.go](file:///d:/claude/ongrid/internal/pkg/llm/probe.go) | 全文 | `ProbeChatCompletion` 实际探测请求 |
| [internal/pkg/llm/client.go](file:///d:/claude/ongrid/internal/pkg/llm/client.go#L44) | 44 | 生产 `defaultTimeout = 120s`（与探测不同） |
| [internal/pkg/llm/client.go](file:///d:/claude/ongrid/internal/pkg/llm/client.go#L298-L331) | 298-331 | `normalizeOpenAIBaseURL` 自动补 `/v1` |
| [internal/pkg/llm/client_test.go](file:///d:/claude/ongrid/internal/pkg/llm/client_test.go#L547-L552) | 547-552 | Ollama URL normalize 测试用例 |
| [web/src/pages/settings/LLM.tsx](file:///d:/claude/ongrid/web/src/pages/settings/LLM.tsx#L624) | 624 | 前端 `timeout` 错误文案 |
| [deploy/docker-compose.yml](file:///d:/claude/ongrid/deploy/docker-compose.yml#L365-L382) | 365-382 | `ongrid-ollama` 服务定义（**缺 networks**） |

---

## 8. 结论

**最可能根因**（按概率排序）：

1. **根因 A（最可能）**：[deploy/docker-compose.yml](file:///d:/claude/ongrid/deploy/docker-compose.yml#L365-L382) 中 `ongrid-ollama` 服务**未声明 `networks: - ongrid_net`**，导致 manager 容器无法通过服务名 DNS 解析到 ollama，探测请求 DNS 失败或连接超时。

2. **根因 C（很可能与 A 叠加）**：即使网络通，Ollama 首次推理时模型冷加载（大模型 30-90s）超过探测硬上限 20s（[llm_probe.go:48](file:///d:/claude/ongrid/internal/manager/biz/setting/llm_probe.go#L48)），导致已建立连接但无响应的 timeout。

3. **根因 B（次可能）**：Base URL 配置错误（如填了 `http://localhost:11434`，manager 容器内的 localhost 是 manager 自己）。但这通常会导致 `connection-failed` 而非 `timeout`。

**修复优先级**：

1. **先修根因 A**：给 `ongrid-ollama` 加 `networks: - ongrid_net`，重启容器
2. **再修配置 B**：Base URL 填 `http://ongrid-ollama:11434/v1`（容器间服务名）
3. **最后修根因 C**：用 `ollama run --keepalive 30m` 预热模型，或临时用 0.5b 小模型探测

**验证**：修复后从 manager 容器内 `wget http://ongrid-ollama:11434/v1/models` 应返回 JSON，UI 测试连接应在 5s 内返回 `✓`。

---

# 错误 2：会话中 LLM provider 报错（"可能是 API key / 模型名 / 网络"）

> **错误现象**：模型配置（Settings → 模型）已成功保存，但在 AIOps 会话中发送消息（如 `List my edges and show me which has the highest load.`）时返回：
> ```
> LLM provider 报错（可能是 API key / 模型名 / 网络）。请到「设置 → 模型」检查 provider 配置；详细 raw 错误请看 manager 日志。
> ```

---

## 1. 错误定位

### 1.1 错误文案来源

源文件：[internal/manager/biz/aiops/chatruntime/runtime.go:1763-1769](file:///d:/claude/ongrid/internal/manager/biz/aiops/chatruntime/runtime.go#L1763-L1769)

```go
// Other LLM provider errors (auth failure, model not found, etc.)
// land here so the user gets a hint to check provider config
// instead of a 200-char raw API error dump.
case strings.Contains(low, "llm: chat completion"),
    strings.Contains(low, "openai api"),
    strings.Contains(low, "api error"):
    return "LLM provider 报错（可能是 API key / 模型名 / 网络）。请到「设置 → 模型」检查 provider 配置；详细 raw 错误请看 manager 日志。"
```

### 1.2 触发链路

```
浏览器 SPA（AIOps 对话页）
   │ POST /v1/chat/sessions/{id}/messages  {content:"List my edges...", provider/model: ...}
   ▼
manager HTTP handler → aiopschatruntime.Runtime.Handle
   ▼
graph.BuildReActGraph(rt.cfg.ChatModel, sessionToolBag, graphCfg)
   ▼
g.Invoke(ctx, &graph.Input{...})  ← eino ReAct 图执行
   ▼
inner ReAct → ChatModel.Generate(ctx, input, opts)
   ▼
RoutingChatModel.pick(opts)  ← 解析 provider
   ▼
clientChatModel.Generate → llmClient.Chat(ctx, req)
   ▼ openai SDK POST http://<base_url>/chat/completions
   ▼
上游返回错误（401/404/500/网络错误/超时/空 choices 等）
   ▼
err 透传回 graph.Invoke
   ▼
buildGraphErrorApology(invokeErr)  ← [runtime.go:1703-1778]
   ▼
匹配 "llm: chat completion" / "openai api" / "api error" 分支
   ▼
返回 "LLM provider 报错..." 文案
   ▼
写入 fallback assistant message + emit Done
```

### 1.3 错误分类器全貌

[runtime.go:1699-1778](file:///d:/claude/ongrid/internal/manager/biz/aiops/chatruntime/runtime.go#L1699-L1778) 的 `buildGraphErrorApology` 按 error message 字符串匹配分 7 类：

| 顺序 | 匹配关键字 | 文案 | 错误类 |
|------|-----------|------|--------|
| 1 | `not found in toolsnode` / `tool`+`not found` | "这个问题需要的深度查询能力不在我（协调员）的工具范围内..." | LLM 幻觉工具名 |
| 2 | `exceeds max` / `exceeded max` / `max steps` / `maxstep` / `max iterations` | "我跑了很多轮工具调用还是没收敛..." | ReAct 超步数 |
| 3 | `context canceled` / `context deadline` | "本次请求超时或被取消..." | ctx 超时/取消 |
| 4 | `budget` | "今日 LLM 预算已用完..." | 预算门控 |
| 5 | `insufficient tool messages` / `tool_calls must be followed` / `tool messages responding` | "本会话历史里有一轮工具调用的响应没落库..." | 历史回放脏数据 |
| 6 | `429` / `too many requests` / `余额不足` / `insufficient_quota` / `insufficient balance` / `credit balance` / `rate limit` / `quota` | "LLM provider 当前不可用——可能是配额用尽..." | 配额/限流 |
| **7** | **`llm: chat completion` / `openai api` / `api error`** | **"LLM provider 报错（可能是 API key / 模型名 / 网络）..."** | **本次错误** |
| 8 | 其他 | "抱歉，处理消息时遇到错误：\n```\n<前200字>\n```..." | 未知 |

**本次错误命中第 7 类**，意味着 raw error message 含 `"llm: chat completion"` / `"openai api"` / `"api error"` 之一。

---

## 2. 根因分析

### 2.1 根因 A：探测路径与会话路径用的 LLM 客户端配置不同（最关键）

**这是导致"配置成功但会话失败"的最常见根因**。

#### 探测路径（Settings → 模型测试）

- 用 [llm.ProbeChatCompletion](file:///d:/claude/ongrid/internal/pkg/llm/probe.go#L23-L71)
- **直接用 UI 提交的临时 cfg**（api_key/base_url/model 来自请求体）
- **不读 DB settings**，不经过 resolver
- 探测成功 → `Save` 写入 DB（[llm_probe.go:139-180](file:///d:/claude/ongrid/internal/manager/biz/setting/llm_probe.go#L139-L180)）

#### 会话路径（AIOps 对话）

- 用 [llm.openaiClient.Chat](file:///d:/claude/ongrid/internal/pkg/llm/client.go#L106)（生产客户端）
- 经 [Resolver](file:///d:/claude/ongrid/internal/pkg/llm/client.go#L66-L68) 从 DB 读凭据（60s TTL 缓存）
- 经 [RoutingChatModel](file:///d:/claude/ongrid/internal/pkg/llm/eino_routing.go#L172-L184) 按 `req.Provider` 路由到 inner ChatModel
- inner ChatModel 调 `providerInjectingClient`（[main.go:3427](file:///d:/claude/ongrid/cmd/ongrid/main.go#L3427)）→ `llmClient.Chat`

**两者配置来源不同的后果**：

- 探测用的 `base_url` 可能与会话用的 `base_url` **不一致**（如探测时填 `http://ongrid-ollama:11434/v1`，但 DB 里存的 `custom_base_url` 是空或旧值）
- 探测用的 `api_key` 是临时的，会话用的 `api_key` 从 DB 读（可能为空或被 redact 逻辑截断）
- 探测用的 `model` 是 UI 填的，会话用的 `model` 从 `req.Model` + `default_model` settings 解析

### 2.2 根因 B：Resolver TTL 缓存延迟（很可能与 A 叠加）

[client.go:79-80, 131](file:///d:/claude/ongrid/internal/pkg/llm/client.go#L79-L80)：

```go
resolveTTL time.Duration  // 60s
// ...
resolved resolvedCreds
resolvedAt time.Time
```

`effectiveCreds` 用 60s TTL 缓存（[client.go:131](file:///d:/claude/ongrid/internal/pkg/llm/client.go#L131)）：

```go
func (c *openaiClient) effectiveCreds(ctx) (apiKey, model, baseURL string, error) {
    // resolver nil → 直接返回 cfg
    // 否则检查 resolvedAt + TTL；过期调 resolver.Resolve(ctx)
    // 失败 Warn + 回退 cfg；空字段回退 cfg
}
```

**后果**：admin 在 UI 改完 provider 配置后，会话路径最长 60s 内仍用旧凭据。探测路径不走缓存，所以探测立即生效。

**触发场景**：用户刚改完 provider 配置 → 立即去对话页测试 → 会话仍用旧（错误的）凭据 → 报错。

### 2.3 根因 C：RoutingChatModel 的 provider 路由失败

[eino_routing.go:172-184](file:///d:/claude/ongrid/internal/pkg/llm/eino_routing.go#L172-L184)：

```go
func (r *RoutingChatModel) pick(opts ...model.Option) (model.ChatModel, string, error) {
    po := model.GetImplSpecificOptions(&providerOpts{}, opts...)
    prov := po.provider
    if prov == "" {
        prov = r.defaultProvider
    }
    inner, ok := r.inner[prov]
    if !ok {
        return nil, prov, fmt.Errorf("%w: %q", ErrUnknownProvider, prov)
    }
    return inner, prov, nil
}
```

`req.Provider` 来自 SPA model picker。如果 SPA 传了 `provider="ollama"`（不在已知列表），`pick` 返回 `ErrUnknownProvider: "ollama"`。

**但**：[main.go:3483-3487](file:///d:/claude/ongrid/cmd/ongrid/main.go#L3483-L3487) 预注册了所有已知 provider id 的 inner：

```go
for _, id := range knownLLMProviderIDs() {
    if _, ok := innerModels[id]; !ok {
        addInner(id, "") // model supplied per-call
    }
}
```

已知 id 见 [model.go:124-130](file:///d:/claude/ongrid/internal/manager/model/setting/model.go#L124-L130)：`openai/anthropic/zhipu/gemini/deepseek/kimi/custom`。**Ollama 不是独立 provider**，必须用 `custom`。

**如果 SPA 传 `provider="ollama"`** → `ErrUnknownProvider` → 但这个错误 message 是 `"unknown provider: \"ollama\""`，不含 `"llm: chat completion"` / `"openai api"` / `"api error"`，会落到第 8 类 default 而非第 7 类。**所以根因 C 不直接命中本次文案**，但可能导致后续连锁。

### 2.4 根因 D：Ollama 返回非 OpenAI 兼容错误（很可能）

Ollama 在某些场景返回非标准错误：

- **模型未 pull**：`{"error":"model 'qwen2.5:7b' not found, try pulling it first"}` → 404
- **模型名错误**：`{"error":"model not found"}` → 404
- **OOM**：`{"error":"cuda out of memory"}` → 500
- **请求格式错**：`{"error":"invalid request"}` → 400

openai SDK 会把这些包装成 `*openai.APIError`，error message 形如：

```
llm probe: chat completion: openai api error: 404 model not found
```

或：

```
llm: chat completion: error, status code: 404, message: model not found
```

**这些 message 含 `"openai api"` 或 `"llm: chat completion"`，命中第 7 类**。

### 2.5 根因 E：会话用 model 名与探测时不同

会话路径的 model 解析（[client.go:358-366](file:///d:/claude/ongrid/internal/pkg/llm/client.go#L358-L366)）：

```go
apiKey, defaultModel, baseURL, _ := c.effectiveCreds(ctx)
// ...
model := strings.TrimSpace(req.Model)
if model == "" {
    model = defaultModel
}
if model == "" {
    model = c.cfg.Model
}
```

**如果**：
- `req.Model`（SPA picker 选的）= `qwen2.5:7b`
- 但 DB `custom_default_model` 存的是 `qwen2.5:7b-instruct`（探测时填的，但保存时被截断/拼写错）
- 或 `req.Model` 为空，`defaultModel` 为空，`c.cfg.Model` 为空

→ Ollama 收到不存在的 model 名 → 404 → 命中第 7 类。

### 2.6 根因 F：docker-compose 网络问题未修复（与错误 1 关联）

如果错误 1 的根因 A（ongrid-ollama 未接入 ongrid_net）**未修复**，但探测时用了 `http://localhost:11434`（manager 容器内不可达）或 host IP（Linux 容器内不可达）：

- **探测可能"成功"**：如果探测时 manager 容器恰好能通过某种方式（如 host network mode）访问到 ollama
- **会话失败**：会话路径用同样的 base_url，但执行环境/时机不同，可能失败

**更可能**：探测和会话用**不同的 base_url**（根因 A），探测用可达地址，会话用 DB 存的不可达地址。

---

## 3. 排查步骤（按优先级）

### 步骤 1：查看 manager 日志拿到 raw error（最关键）

```bash
docker logs ongrid --tail 100 2>&1 | grep -i "chat completion\|openai api\|api error\|llm"
```

或：

```bash
docker logs ongrid -f 2>&1 | grep -i "chatruntime\|llm"
```

**关注**：

- `chatruntime: graph invoke failed (apology emitted)` 后面的 `err=...`
- `llm: chat completion: ...` 开头的错误
- HTTP status code（401/404/500/429）
- 上游返回的 message（如 `model not found`）

**这一步能直接定位是根因 D/E 还是网络问题**。

### 步骤 2：确认 SPA 传的 provider 和 model

打开浏览器 DevTools → Network → 找到 `POST /v1/chat/sessions/{id}/messages` → 看 Request Body：

```json
{
  "content": "List my edges...",
  "provider": "custom",       // ← 应该是 custom，不是 ollama
  "model": "qwen2.5:7b",      // ← 应该与 ollama list 一致
  ...
}
```

**常见错误**：
- `provider: "ollama"` → 不存在，应改 `custom`（根因 C）
- `model: "qwen2.5:7b-instruct"` 但 ollama 里是 `qwen2.5:7b` → 404（根因 E）
- `provider: ""` + `model: ""` → 用 default，但 default 可能未配

### 步骤 3：确认 DB 存的 provider 配置

```bash
docker exec ongrid mysql -uongrid -pongrid ongrid -e \
  "SELECT category, key_name, LEFT(value, 80) FROM system_settings WHERE category='llm'"
```

**关注**：
- `llm_custom_api_key` 是否非空（应为占位符如 `ollama`）
- `llm_custom_base_url` 是否为 `http://ongrid-ollama:11434/v1`（不是 `localhost`）
- `llm_custom_default_model` 是否与 ollama list 一致
- `llm_custom_models` 是否是 JSON 数组且包含 default_model
- `llm_default_provider` 是否为 `custom`

### 步骤 4：从 manager 容器内测试会话用的 base_url

```bash
# 用 DB 里的 base_url 测试
docker exec ongrid sh -c 'wget -qO- --timeout=10 --header="Authorization: Bearer ollama" \
  --post-data='\''{"model":"qwen2.5:7b","messages":[{"role":"user","content":"Reply with OK."}]}'\'' \
  http://ongrid-ollama:11434/v1/chat/completions'
```

- **DNS 失败** → 根因 F（网络未修复）
- **404 model not found** → 根因 E（model 名错）
- **401** → 根因 D（auth 错，但 Ollama 通常不鉴权）
- **正常返回** → 跳到步骤 5

### 步骤 5：确认 ollama 模型已加载

```bash
docker exec ongrid-ollama ollama list
# 确认 qwen2.5:7b 在列表里
docker exec ongrid-ollama ollama ps
# 确认模型已加载到内存（或用 run 预热）
```

### 步骤 6：刷新 LLM 缓存（根因 B）

```bash
# 调用 invalidate API 立即刷新 resolver 缓存
# 需要带 admin JWT
curl -X POST http://localhost:8080/v1/integrations/llm/invalidate \
  -H "Authorization: Bearer <admin_jwt>"
```

或重启 manager 让缓存清空：

```bash
docker restart ongrid
```

然后再对话测试。

---

## 4. 修复方案

### 修复 A（根因 A/B，最关键）：确保会话路径用正确配置

#### A1. 确认 DB 配置正确

在 UI（Settings → 模型 → Custom provider）重新填写并**点击保存**（不只是测试）：

| 字段 | 值 |
|------|-----|
| Provider | `custom` |
| Base URL | `http://ongrid-ollama:11434/v1` |
| API Key | `ollama`（非空占位符） |
| Models | `qwen2.5:7b`（与 `ollama list` 一致） |
| Default Model | `qwen2.5:7b` |

**关键**：UI 的"测试连接"只探测不保存；必须点"保存"按钮才会写入 DB。

#### A2. 设为默认 provider

确保 `llm_default_provider = custom`（UI 上有"设为默认"选项）。

#### A3. 刷新缓存或重启

```bash
# 方案 1：调用 invalidate API（需 admin JWT）
curl -X POST http://localhost:8080/v1/integrations/llm/invalidate \
  -H "Authorization: Bearer <admin_jwt>"

# 方案 2：重启 manager（最彻底）
docker restart ongrid
```

### 修复 B（根因 C）：SPA 传正确的 provider

如果 SPA model picker 选了 "Ollama"（不存在），改为选 "Custom"。

检查 [LLM.tsx](file:///d:/claude/ongrid/web/src/pages/settings/LLM.tsx) 的 provider 列表，Ollama 应该归入 Custom provider 的 hint，不是独立选项。

### 修复 C（根因 D/E）：纠正 model 名

```bash
# 看 ollama 实际有什么模型
docker exec ongrid-ollama ollama list
```

把 UI 里的 model 名改成与 `ollama list` 输出**完全一致**（包括 tag，如 `qwen2.5:7b` 不是 `qwen2.5-7b`）。

### 修复 D（根因 F）：修复 docker-compose 网络

如果错误 1 的网络问题未修复，先按错误 1 的修复 A 给 `ongrid-ollama` 加 `networks: - ongrid_net`。

### 修复 E：预热模型

```bash
docker exec ongrid-ollama ollama run qwen2.5:7b "Reply with OK." --keepalive 30m
```

避免会话首次推理时冷加载超时（会话路径超时 120s，但 Ollama 冷加载大模型可能更久）。

---

## 5. 验证流程

### 5.1 验证 DB 配置

```bash
docker exec ongrid mysql -uongrid -pongrid ongrid -e \
  "SELECT key_name, LEFT(value, 80) FROM system_settings WHERE category='llm' AND key_name LIKE 'llm_custom_%'"
```

预期：
- `llm_custom_api_key` = 非空
- `llm_custom_base_url` = `http://ongrid-ollama:11434/v1`
- `llm_custom_default_model` = `qwen2.5:7b`
- `llm_custom_models` = `["qwen2.5:7b"]`

### 5.2 验证网络

```bash
docker exec ongrid sh -c 'wget -qO- --timeout=5 http://ongrid-ollama:11434/v1/models'
# 应返回 {"object":"list","data":[{"id":"qwen2.5:7b",...}]}
```

### 5.3 验证 Chat Completions

```bash
docker exec ongrid sh -c 'wget -qO- --timeout=30 --post-data='\''{"model":"qwen2.5:7b","messages":[{"role":"user","content":"Reply with OK."}]}'\'' http://ongrid-ollama:11434/v1/chat/completions'
# 应返回 {"choices":[{"message":{"content":"OK"}}],...}
```

### 5.4 验证 manager 日志无错误

```bash
docker logs ongrid --tail 20 2>&1 | grep -i "chatruntime\|llm"
# 不应有 "graph invoke failed" 或 "chat completion" 错误
```

### 5.5 在 AIOps 对话页测试

发一条简单消息（如 `hello`），确认 LLM 正常响应。

然后再发 `List my edges and show me which has the highest load.`，确认：
1. LLM 调用 `query_devices` 工具（[query_edges_basetool.go:57](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/query_edges_basetool.go#L57)）
2. 工具返回 device 列表
3. LLM 调用 `get_host_load` 工具（[registry_basetool.go:80](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/registry_basetool.go#L80)）拿负载
4. LLM 综合回答

---

## 6. 深入：query_devices 工具依赖链

用户问题 `List my edges and show me which has the highest load` 需要 LLM 调用 `query_devices` 工具，该工具的依赖链：

### 6.1 工具注册条件

[registry_basetool.go:132-136](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/registry_basetool.go#L132-L136)：

```go
// 6-7: query_devices + get_topology gated on edges (matches
// NewRegistry — both need the edge usecase or device usecase).
if r.edges != nil || r.devices != nil {
    out = append(out, NewQueryEdgesTool(r.devices, r.edges, r.log))
}
```

**如果 `r.edges` 和 `r.devices` 都为 nil**，`query_devices` 工具不会注册，LLM 看不到它，会幻觉一个工具名 → 命中第 1 类错误（"这个问题需要的深度查询能力不在我（协调员）的工具范围内"），不是本次错误。

**所以本次错误不是工具未注册问题**，而是 LLM 调用本身失败。

### 6.2 工具执行依赖

[query_edges_basetool.go:57-60](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/query_edges_basetool.go#L57-L60)：

```go
func (t *QueryEdgesTool) InvokableRun(ctx, argsJSON, _ ...) (string, error) {
    if t.devices == nil && t.edges == nil {
        return "", fmt.Errorf("query_devices: device usecase not configured")
    }
    // ...
}
```

**如果工具注册了但执行时返回 error**，eino ToolsNode 会把 error 当 graph-fatal（[tool_adapter.go:349-360 注释](file:///d:/claude/ongrid/internal/manager/biz/aiops/graph/tool_adapter.go#L349-L360)），但 error message 是 `query_devices: ...`，不含 `"llm: chat completion"`，会落到第 8 类 default 而非第 7 类。

**所以本次错误也不是工具执行失败**，而是 LLM Generate 调用本身失败。

### 6.3 结论：本次错误是 LLM 调用本身失败

错误命中第 7 类（`"llm: chat completion"` / `"openai api"` / `"api error"`），说明：

- **不是**工具未注册（第 1 类）
- **不是**工具执行失败（第 8 类 default）
- **不是**超步数（第 2 类）
- **不是**预算超限（第 4 类）
- **不是**历史脏数据（第 5 类）
- **不是**配额限流（第 6 类）
- **是** LLM provider 返回错误（401/404/500/网络/超时等）

**根因聚焦**：会话路径的 LLM 客户端配置（api_key/base_url/model）与探测路径不一致，或 Resolver 缓存延迟，或 Ollama 返回非 OpenAI 兼容错误。

---

## 7. 预防措施

### 7.1 UI 优化建议

- **保存按钮高亮**：测试通过后自动滚动到保存按钮，避免用户只测不存
- **配置对比**：UI 显示"当前 DB 配置" vs "本次测试配置"，让用户看到差异
- **model 名自动补全**：从 ollama `/api/tags` 拉模型列表，避免拼写错

### 7.2 后端优化建议

- **探测与生产共用 Resolver**：让 ProbeChatCompletion 也走 Resolver，确保探测的就是生产用的
- **错误日志带配置上下文**：`buildGraphErrorApology` 命中第 7 类时，log 里带上 `provider/baseURL/model`（脱敏 api_key），方便定位
- **Resolver 缓存缩短**：LLM 配置 60s TTL 太长，改 5s 与其他 system_settings 一致

### 7.3 文档建议

[deploy/README.md](file:///d:/claude/ongrid/deploy/README.md) Quickstart 补充：

- "配置完 Ollama provider 后，务必点击**保存**按钮（不只是测试连接）"
- "会话页如果报 LLM provider 错误，先重启 manager 清缓存，或调用 `/v1/integrations/llm/invalidate`"

---

## 8. 相关代码索引

| 文件 | 行 | 职责 |
|------|----|------|
| [internal/manager/biz/aiops/chatruntime/runtime.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/chatruntime/runtime.go#L1699-L1778) | 1699-1778 | `buildGraphErrorApology` 7 类错误分类 |
| [internal/manager/biz/aiops/chatruntime/runtime.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/chatruntime/runtime.go#L680-L816) | 680-816 | graph 构建与 Invoke + 错误处理 |
| [internal/manager/biz/aiops/chatruntime/runtime.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/chatruntime/runtime.go#L1763-L1769) | 1763-1769 | 本次错误文案分支 |
| [internal/pkg/llm/client.go](file:///d:/claude/ongrid/internal/pkg/llm/client.go#L106) | 106 | `openaiClient.Chat` 生产 LLM 调用 |
| [internal/pkg/llm/client.go](file:///d:/claude/ongrid/internal/pkg/llm/client.go#L66-L68) | 66-68 | `Resolver` 接口 |
| [internal/pkg/llm/client.go](file:///d:/claude/ongrid/internal/pkg/llm/client.go#L79-L80) | 79-80 | `resolveTTL = 60s` |
| [internal/pkg/llm/client.go](file:///d:/claude/ongrid/internal/pkg/llm/client.go#L128-L131) | 128-131 | `effectiveCreds` TTL 缓存 |
| [internal/pkg/llm/eino_routing.go](file:///d:/claude/ongrid/internal/pkg/llm/eino_routing.go#L172-L184) | 172-184 | `RoutingChatModel.pick` provider 路由 |
| [internal/pkg/llm/probe.go](file:///d:/claude/ongrid/internal/pkg/llm/probe.go#L23-L71) | 23-71 | `ProbeChatCompletion` 探测路径（不读 DB） |
| [internal/manager/biz/setting/llm_probe.go](file:///d:/claude/ongrid/internal/manager/biz/setting/llm_probe.go#L139-L180) | 139-180 | `Save` 写入 DB |
| [cmd/ongrid/main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go#L3418-L3528) | 3418-3528 | RoutingChatModel 装配 |
| [internal/manager/biz/aiops/tools/registry_basetool.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/registry_basetool.go#L132-L136) | 132-136 | `query_devices` 注册条件 |
| [internal/manager/biz/aiops/tools/query_edges_basetool.go](file:///d:/claude/ongrid/internal/manager/biz/aiops/tools/query_edges_basetool.go#L57-L60) | 57-60 | `query_devices` 执行入口 |

---

## 9. 结论

**最可能根因**（按概率排序）：

1. **根因 A（最可能）**：探测路径（ProbeChatCompletion）用 UI 临时配置，会话路径（openaiClient.Chat）用 DB 配置经 Resolver 解析，**两者配置不一致**。最常见场景：用户在 UI 测试通过但**未点保存**，或保存的 base_url/model 与测试时不同。

2. **根因 B（很可能与 A 叠加）**：[client.go:79-80](file:///d:/claude/ongrid/internal/pkg/llm/client.go#L79-L80) 的 Resolver TTL 缓存 60s，用户改完配置立即去对话页测试，会话仍用旧凭据。

3. **根因 D/E（次可能）**：会话用的 model 名与 Ollama 实际模型不一致（如 `qwen2.5:7b-instruct` vs `qwen2.5:7b`），Ollama 返回 404，openai SDK 包装成含 `"openai api"` 的 error，命中第 7 类。

4. **根因 F（可能）**：错误 1 的 docker-compose 网络问题未修复，会话路径的 base_url 不可达。

**修复优先级**：

1. **先看 manager 日志**（步骤 1）拿到 raw error，直接定位是 401/404/500/网络/超时
2. **确认 DB 配置正确**（步骤 3），确保保存了正确配置
3. **刷新缓存或重启 manager**（修复 A3），消除 Resolver TTL 延迟
4. **纠正 model 名**（修复 C），与 `ollama list` 完全一致
5. **修复网络**（修复 D），如果错误 1 未解决

**关键区别**：探测路径不走 Resolver/DB，会话路径走 Resolver/DB。**"配置成功"（探测通过）≠ "会话能用"（DB 配置正确且缓存刷新）**。

**验证**：修复后 manager 日志无 `chatruntime: graph invoke failed`，AIOps 对话页能正常调 `query_devices` + `get_host_load` 并回答问题。
