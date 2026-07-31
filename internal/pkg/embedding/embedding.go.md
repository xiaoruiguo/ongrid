# `embedding.go` 技术实现文档

> 源文件：`internal/pkg/embedding/embedding.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/embedding`

## 1. 概述

该文件实现 embedding 模型的 provider 抽象与 OpenAI 兼容 HTTP 实现。`Embedder` 接口让 knowledge service 在任何遵循 OpenAI `/v1/embeddings` 形状的 vendor（OpenAI / Azure OpenAI / GLM 智谱 / Qwen 通义 / DeepSeek）之间无感切换。Phase-1 仅交付 OpenAI 兼容 HTTP 客户端；同一接口下 `local.go` 提供 ONNX 本地推理实现作为离线 fallback。

## 2. 包信息

- **包名**：`embedding`
- **所属模块**：`internal/pkg/`（基础设施层）
- **依赖方向**：被 manager knowledge BC 调用；依赖 `internal/pkg/zhipuauth`（智谱 JWT 签名）+ 标准库。

## 3. 关键类型与接口

### `Embedder`
窄接口，knowledge service 唯一消费点。

```go
type Embedder interface {
    Dim() int
    Embed(ctx context.Context, texts []string) ([][]float32, error)
}
```

`Embed` 返回每个输入文本一个 float32 向量，**顺序与输入对齐**——实现可批量但契约仅要求顺序保持。

### `Config`
env 驱动的输入聚合。

```go
type Config struct {
    Provider string
    Model    string
    BaseURL  string
    APIKey   string
    Dim      int
    Log      *slog.Logger
}
```

### `openAIEmbedder`
OpenAI 兼容 HTTP 实现，字段：`base` / `model` / `apiKey` / `dim` / `hc` / `log`。

## 4. 关键函数与流程

### `New`
- **签名**：`func New(cfg Config) (Embedder, error)`
- **职责**：根据 `cfg.Provider` 分发。
- **流程**：`provider` trim + lower；空 → `"openai"`。
  - `"openai"` → `newOpenAI(cfg)`。
  - `"local"` / `"fastembed"` / `"onnx"` → `newLocal(cfg)`（见 `local.go`）。
  - 其他 → error `unknown provider`。

### `newOpenAI`
- **签名**：`func newOpenAI(cfg Config) (*openAIEmbedder, error)`
- **流程**：
  1. base trim + 去 trailing `/`；空 → `defaultOpenAIBase = "https://api.openai.com"`。
  2. model trim；空 → `defaultModel = "text-embedding-3-small"`。
  3. dim <= 0 → `defaultDim = 1536`。
  4. apiKey trim；空 → error `api_key required`。
  5. log nil → `slog.Default()`。
  6. 构造 `&openAIEmbedder{hc: &http.Client{Timeout: 30s}}`。

### `openAIEmbedder.Embed`
- **签名**：`func (e *openAIEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error)`
- **流程**：
  1. 空输入 → 返回 nil, nil。
  2. `json.Marshal(embedReq{Model, Input: texts})`。
  3. `embedURL(e.base)` 智能拼接路径：base 已含 `/v<digit+>` 后缀（如智谱 `.../v4`）则只追加 `/embeddings`，否则追加 `/v1/embeddings`。
  4. `http.NewRequestWithContext(ctx, POST, url, body)`。
  5. **智谱特殊处理**：`zhipuauth.LooksLikeZhipuURL(base) && LooksLikeZhipuKey(apiKey)` 为真时，用 `zhipuauth.SignJWT(apiKey, time.Hour)` 签出 JWT 作为 Bearer；否则直接用 raw apiKey。智谱 v4 端点拒绝 raw `<id>.<secret>` 形式 Bearer，需 JWT。
  6. `hc.Do(req)`；`defer resp.Body.Close()`。
  7. `io.ReadAll` 全 body；非 2xx → error 含 status + 截断 256 字符 body。
  8. `json.Unmarshal` 到 `embedResp`；`er.Error != nil` → error `provider err`。
  9. 校验 `len(er.Data) == len(texts)`；不一致 → error。
  10. 遍历 data：校验 `Index` 范围 + 向量维度匹配 `e.dim`；按 `Index` 放入 `out` slice。
  11. 返回 out。
- **错误处理**：每步 `%w` 包装并加 `embedding:` 前缀。

### `embedURL`
- **签名**：`func embedURL(base string) string`
- **职责**：根据 base 是否以 `/v<digit+>` 结尾决定追加 `/embeddings` 还是 `/v1/embeddings`。
- **设计理由**：省去运维记忆每个 vendor 的正确 URL 尾巴。

### `hasVersionSuffix`
- **签名**：`func hasVersionSuffix(base string) bool`
- **职责**：判断 base 是否以 `/v<digit+>` 结尾（如 `/v1` / `/v4`）。

### `truncate`
- **签名**：`func truncate(s string, n int) string`
- **职责**：截断字符串，超长加 `…`。

## 5. 依赖关系

- **内部包**：`github.com/ongridio/ongrid/internal/pkg/zhipuauth`。
- **外部库**：标准库 `bytes` / `context` / `encoding/json` / `errors` / `fmt` / `io` / `log/slog` / `net/http` / `strings` / `time`。
- **被调用方**：manager knowledge BC（文档 chunk → embed → qdrant upsert / 检索）。

## 6. 并发与资源管理

无显式锁。`openAIEmbedder` 字段构造后不变，`Embed` 每次创建独立 `http.Request`，`http.Client` 自身并发安全。多 goroutine 并发 Embed 由 HTTP 连接池承载。

## 7. 设计模式与亮点

- **接口隔离 BC**：`Embedder` 在消费方（embedding 包）定义，knowledge service 不直接依赖具体 provider 实现，便于测试 mock。
- **provider 抽象**：`New` switch 分发，新增 provider 只需加 case + 实现接口。
- **智能 URL 拼接**：`embedURL` 自动识别 `/v<digit+>` 后缀，兼容 OpenAI 标准与智谱 v4 等非标准路径，降低配置门槛。
- **智谱 JWT 自动签名**：检测智谱 URL + key 形态自动签 JWT，对调用方透明，避免 401 调试痛点。
- **顺序保持契约**：`Embed` 明确要求输出顺序与输入对齐，调用方按 index 取结果（如 `out[d.Index] = d.Embedding`）。
- **维度校验**：返回向量维度与 `e.dim` 不符立即报错，避免错误维度数据进 qdrant 后才发现。
- **错误 body 截断**：错误信息含截断的响应 body，便于诊断但避免日志爆炸。

## 8. 注意事项

- **30s HTTP 超时**：大批量 embed（接近 2048 上限）可能超时；调用方应自行分批。
- **无重试**：单次 HTTP 失败即返回错误，调用方需自实现重试 / 退避策略。
- **智谱 JWT 每次签名**：每次 Embed 都重签 JWT，TTL=1h；高频调用下签名开销可忽略（HMAC），但若未来扩展到多 provider 类似需求可考虑缓存。
- **维度默认 1536**：与 `text-embedding-3-small` 匹配；切换 model 必须同步调整 `Dim`，否则维度校验失败。
- **APIKey 明文持有**：`openAIEmbedder.apiKey` 字段明文，进程内存 dump 可见；gospec 红线"密钥禁止进日志"已遵守（不记录），但内存层面无额外保护。
- **provider 扩展点**：新增非 OpenAI 兼容 vendor 需要新建独立 embedder 类型，不能复用 `openAIEmbedder`。
