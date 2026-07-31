# `code_source_basetool.go` 技术实现文档

> 源文件：`internal/manager/biz/aiops/tools/code_source_basetool.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools`

## 1. 概述

本文件实现 HLD-012 Phase 1 的三个 read-only BaseTool：`list_repo_sources`（列仓库目录）、`read_source`（读源文件窗口）、`grep_source`（regex 搜源码）。让 Agent 把告警/日志的 file:line / 函数 / 错误串关联到实际代码。thin wrapper——所有路径安全、大小限制、repo 解析都在 knowledge biz（CodeBrowser）。

## 2. 包信息

- **包名**：`tools`
- **所属模块**：`internal/manager/biz/aiops/tools`
- **依赖方向**：被 Registry 注册和 agent loop 调用；依赖 `basetool`、`knowledgebiz`

## 3. 关键类型与接口

```go
type CodeBrowser interface {
    ListRepoSources(ctx, ref, subpath string) (*knowledgebiz.RepoSourceListing, error)
    ReadSource(ctx, ref, path string, startLine, endLine int) (*knowledgebiz.SourceFile, error)
    GrepSource(ctx, ref, pattern, pathGlob string, max int) (*knowledgebiz.GrepResult, error)
}

const (
    ToolNameListRepoSources = "list_repo_sources"
    ToolNameReadSource      = "read_source"
    ToolNameGrepSource      = "grep_source"
)

type ListRepoSourcesTool struct { svc CodeBrowser; log *slog.Logger }
type ReadSourceTool      struct { svc CodeBrowser; log *slog.Logger }
type GrepSourceTool      struct { svc CodeBrowser; log *slog.Logger }
```

共享 WhenToUse `codeSourceWhenToUse`：运维/故障分析时把代码线索关联到源码做逻辑探查；典型流程 grep_source → read_source → 顺着调用链迭代追；前提是仓库已注册并 sync。

## 4. 关键函数与流程

### 三个工具的 `Info`
- 都返回 `Class: "read"`；共享 `codeSourceWhenToUse`
- 各自 `Parameters` 为对应 schema（listRepoSourcesSchema / readSourceSchema / grepSourceSchema）

### `InvokableRun`（三者模式相同）
- **流程**：
  1. svc nil → error "code browser not configured"
  2. unmarshal args；err → wrap "bad args"
  3. 调对应 svc 方法（ListRepoSources / ReadSource / GrepSource）
  4. err → wrap tool name
  5. `marshalToolJSON(toolName, res)` marshal 返回

### `marshalToolJSON`
- **签名**：`func marshalToolJSON(toolName string, v any) (string, error)`
- **职责**：共享的 "encode result envelope" helper
- **流程**：json.Marshal；err → wrap "marshal response"

## 5. 依赖关系

- **内部包**：`basetool`、`knowledgebiz`（CodeBrowser 接口、RepoSourceListing/SourceFile/GrepResult 类型）
- **外部库**：标准库 `context`、`encoding/json`、`fmt`、`log/slog`
- **被调用方**：Registry 注册；`*knowledge.Usecase` 实现 CodeBrowser

## 6. 并发与资源管理

- 三个 Tool struct immutable，多 goroutine 共享安全
- 无锁、无 channel；svc 内部自管并发

## 7. 设计模式与亮点

- **thin wrapper 模式**：tool 只做 schema 定义 + args 解析 + 转发，业务逻辑在 knowledge biz
- **CodeBrowser 窄接口**：在消费方定义（tools 包），避免 tools → knowledgebiz 内部细节耦合
- **共享 WhenToUse**：三个工具用同一份 routing hint，引导 LLM 走 grep → read → 迭代追调用链的流程
- **marshalToolJSON 复用**：三个工具共用 marshal helper
- **repo ref 灵活**：repo 参数接受 URL / 唯一子串 / 数字 id，CodeBrowser 内部解析

## 8. 注意事项

- **前提：仓库已注册并 sync**：目标仓库必须在"代码仓库"注册并 Repos-sync 过
- **read_source 行号**：1-indexed，start_line=0/省略 = 全文件；end_line=0/省略 = EOF 或 start 周围合理窗口
- **binary 文件拒绝**：ReadSource 拒绝 binary；大文件 cap
- **grep max_results**：默认 50，max 200
- **path_glob 可选**：`*.go` 或 `internal/manager/` 缩小搜索范围
- **区别于 host_bash**：这里读控制面仓库的源码（代码事实），host_bash 读在线主机文件
