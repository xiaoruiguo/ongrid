# `local.go` 技术实现文档

> 源文件：`internal/pkg/embedding/local.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/embedding`

## 1. 概述

该文件实现 embedding 的离线 ONNX 推理后端，基于 `fastembed-go`（BAAI BGE 模型 + ONNX Runtime）。触发条件为 `ONGRID_EMBEDDING_PROVIDER=local` / `fastembed` / `onnx`。无网络、无 API key、无外部依赖（运行时），适用于空气隔离 / 受监管部署（ADR-027）——这些场景此前 RAG 完全不可用。文件包含对 `fastembed-go` v1.0.0 / `sugarme/tokenizer` v0.2.3 上游 bug 的多项防御性处理。

## 2. 包信息

- **包名**：`embedding`
- **所属模块**：`internal/pkg/`（基础设施层）
- **依赖方向**：被 `embedding.New` 在 provider 为 `local` 时调用；依赖 `github.com/anush008/fastembed-go` + 标准库。

## 3. 关键类型与接口

### `localEmbedder`
实现 `Embedder` 接口，包装 `*fastembed.FlagEmbedding`。

```go
type localEmbedder struct {
    model *fastembed.FlagEmbedding
    dim   int
    name  string
    mu    sync.Mutex  // 串行化 Embed 调用
    log   *slog.Logger
}
```

ONNX 推理 CPU-bound 且 fastembed 包装层非线程安全，`mu` 串行化并发 Embed，避免 tokenizer 崩溃。

### `modelDims`（包级变量）
`fastembed.EmbeddingModel` → 输出维度的映射表。预先静态记录而非运行时发现，便于 `EnsureCollection` 在首次 Embed 之前正确尺寸 qdrant collection。

## 4. 关键函数与流程

### `resolveModel`
- **签名**：`func resolveModel(name string) (fastembed.EmbeddingModel, error)`
- **职责**：把 operator 友好字符串映射到 fastembed 常量。
- **默认**：空 / `"bge-small-zh-v1.5"` / `"zh"` / `"default"` → `BGESmallZH`（中英混合最佳，30MB 量化 ONNX，dim=512）。
- **支持**：`bge-small-en-v1.5` / `bge-base-en-v1.5` / `all-minilm-l6-v2`。
- **错误处理**：未知 → error 含建议。

### `newLocal`
- **签名**：`func newLocal(cfg Config) (*localEmbedder, error)`
- **流程**：
  1. `resolveModel(cfg.Model)`。
  2. cacheDir 从 `ONGRID_EMBEDDING_CACHE_DIR` 取，默认 `/var/lib/ongrid/embeddings`。
  3. `os.MkdirAll(cacheDir, 0o755)`。
  4. log nil → `slog.Default()`。
  5. `modelDims[model]` 取 dim；未知 → error。
  6. `cfg.Dim > 0 && cfg.Dim != dim` → error（配置与模型维度不符，fail early 防 qdrant 尺寸错配）。
  7. `fastembed.NewFlagEmbedding(&InitOptions{Model, CacheDir, ShowDownloadProgress: false})`。
     - **MaxLength 故意留 0**：`sugarme/tokenizer` v0.2.3 在 `TruncateEncodings` 中存在 nil-pair NPE（单文本时 `pair` 为 nil），fastembed 强制 MaxLength=512 会触发该路径。设 0 禁用库的截断，改由 `clampForLocalEmbed` 自行限长。
  8. 初始化失败：根据错误内容附加 hint——
     - 含 `onnxruntime` / `ONNX_PATH` → 提示 `apt install libonnxruntime-dev`。
     - 含 `download` / `no such host` → 提示预下载模型到 cacheDir。
- **错误处理**：`%w` 包装 + hint 后缀。

### `localEmbedder.Dim`
返回 `e.dim`。

### `localEmbedder.Embed`
- **签名**：`func (e *localEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error)`
- **流程**：
  1. 空输入 → nil, nil。
  2. `e.mu.Lock()` 串行化。
  3. `select <-ctx.Done()` 快速失败：fastembed Embed 无 ctx hook，无法中途取消，仅在入队前检查。
  4. **预处理**：每个文本 `clampForLocalEmbed`——
     - `TrimSpace` 后空 → 替换为单空格（避免 tokenizer 在某些路径崩溃，保持 index 对齐）。
     - rune 数 > `maxLocalEmbedChars=350` → 截断到 350。
  5. `e.model.Embed(safe, 8)`：批量大小 8。
  6. 校验返回向量数 == 输入数；每个向量维度 == `e.dim`。
- **错误处理**：`%w` 包装。

### `clampForLocalEmbed`
- **签名**：`func clampForLocalEmbed(s string) string`
- **职责**：限长 + 空串保护。
- **`maxLocalEmbedChars=350` 的依据**：BGE-small-zh tokenizer 下 1 中文 ≈ 1 token，1 英文 word ≈ 1-2 token（5 字符/词），350 字符覆盖两种语言均 < 512 token，绕开 `TruncateEncodings` NPE。
- **代价**：长文档仅前 350 字符参与 embedding；全文仍存 qdrant payload 供 LLM 检索后阅读。长文档场景建议用 `openai` provider。

## 5. 依赖关系

- **内部包**：无。
- **外部库**：`github.com/anush008/fastembed-go`；标准库 `context` / `errors` / `fmt` / `log/slog` / `os` / `strings` / `sync`。
- **系统依赖**：`libonnxruntime` 共享库（deb: `libonnxruntime-dev`）。
- **被调用方**：`embedding.New` 在 provider=local 时调用。

## 6. 并发与资源管理

**`sync.Mutex` 串行化所有 Embed 调用**——这是核心并发控制。原因：fastembed-go 的 ONNX 推理与 tokenizer 均非线程安全。单核 CPU 是吞吐瓶颈，串行化不降低性能。`context` 仅在入队前检查取消，无法中断进行中的 ONNX 推理。

## 7. 设计模式与亮点

- **接口对齐**：`localEmbedder` 与 `openAIEmbedder` 实现同一 `Embedder` 接口，调用方零改动切换。
- **预静态 dim 映射**：`modelDims` 静态表让 `EnsureCollection` 在 Embed 前正确尺寸 qdrant，避免运行时才发现维度不匹配。
- **防御性预处理**：`clampForLocalEmbed` 针对上游 tokenizer bug 三重防御——空串替换单空格、rune 截断、字符数硬上限。
- **失败 hint**：初始化失败根据错误内容附加可操作 hint（装 onnxruntime / 预下载模型），降低运维支持成本。
- **batch=8**：小批量平衡吞吐与故障半径——单个文本触发 bug 时仅影响 8 条而非全部。
- **dim 配置校验**：`cfg.Dim` 显式设置时与模型 dim 不符立即失败，防 qdrant 尺寸错配。

## 8. 注意事项

- **上游 bug 绑定**：当前对 `sugarme/tokenizer` v0.2.3 NPE 的绕开依赖 `maxLocalEmbedChars=350`；fastembed-go 升级后需重新评估是否可放宽。
- **长文档质量降级**：350 字符上限意味着长文档仅前段参与 embedding，召回质量下降；运营若有长文档应建议用 `openai` provider。
- **CPU 单核瓶颈**：mutex 串行化下，高并发检索 / 同步 upsert 会排队；吞吐 ≈ 单核 ONNX 速度，需容量评估。
- **首次启动需下载模型**：cacheDir 为空且网络可达时 fastembed 自动下载；空气隔离部署必须预下载（`fetch-embedding-model.sh`）。
- **libonnxruntime 系统依赖**：镜像需包含该 .so；Debian 系 `apt install libonnxruntime-dev`，其他发行版需手工安装。
- **模型切换需重建 qdrant collection**：dim 不同（如 BGE-small-zh=512 vs BGE-base-en=768）需重新尺寸 collection。
- **ctx 无法中断推理**：取消的请求仍会完成 ONNX 推理后才返回；高 QPS 下可能积压。
