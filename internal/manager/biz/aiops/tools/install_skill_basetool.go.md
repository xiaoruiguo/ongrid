# `install_skill_basetool.go` 技术实现文档

> 源文件：`internal/manager\biz\aiops\tools\install_skill_basetool.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/aiops/tools`

## 1. 概述

本文件实现 `install_skill` BaseTool：对话式技能安装。用户把技能源（git repo URL / .tar.gz tarball / skills.sh 链接）粘到对话里请 agent 安装；agent 提取 URL 调用本工具。**Same safety model as cloud_bash**——不直接安装，而是把 proposal 排入 human approval inbox，用户通过 inline confirmation card 批准后才真正 fetch + install。`Class="destructive"`（安装技能 = 授予任意代码执行权，技能可自带二进制被 cloud_bash 调用）。**无 marketplace / 搜索**——源完全由用户提供。`SkillInstallProposer` 是窄接口，cmd/main.go 基于 `biz/approval.Usecase` 实现，避免本包 import approval biz。

## 2. 包信息

- **包名**：`tools`
- **所属模块**：`internal/manager/biz/aiops/tools`
- **依赖方向**：被 Registry BaseTool 路径注册调用；依赖 `basetool`（`ResolveOptions` / `SessionIDFromContext`）。**不依赖** approval biz——通过 `SkillInstallProposer` 接口解耦。

## 3. 关键类型与接口

```go
// 窄 seam 到 approval inbox。cmd/main.go 基于 biz/approval.Usecase 实现。
type SkillInstallProposer interface {
    // ProposeInstall 排队 skill install 等待 human approval，返回 approval id。
    // sourceType 已解析为 "git" | "tarball"；ref 是可选 git branch/tag。
    ProposeInstall(ctx, url, sourceType, ref, sessionID string, userID uint64) (id string, err error)
}

type InstallSkillTool struct {
    proposer SkillInstallProposer
    log      *slog.Logger
}

type installSkillArgs struct {
    URL  string `json:"url"`  // 必填，原样从用户消息提取
    Ref  string `json:"ref"`  // 可选 git branch/tag/commit
    Type string `json:"type"` // 可选 git|tarball，空则自动推断
}
```

`InstallSkillSchema`：`url` 必填，description 明示 "Extract it verbatim from the user's message; do not invent or guess one"；`type` 可选，空则自动推断（`.tar.gz`/`.tgz`/`.tar` 后缀 → tarball，否则 git）。

## 4. 关键函数与流程

```go
func NewInstallSkillTool(p SkillInstallProposer, log *slog.Logger) *InstallSkillTool
func inferSourceType(url string) string  // .tar.gz/.tgz/.tar → tarball，否则 git
func (t *InstallSkillTool) Info(_ context.Context) (*basetool.ToolInfo, error)
func (t *InstallSkillTool) InvokableRun(ctx, argsJSON string, opts ...InvokeOption) (string, error)
```

**`InvokableRun` 流程**：
1. 守门 `proposer != nil`。
2. Unmarshal → `installSkillArgs`；`url := strings.TrimSpace(in.URL)`，空 → 报错 "url is required"。
3. `srcType` 校验：`"git"` / `"tarball"` 通过；其他（含空）→ `inferSourceType(url)` 推断。
4. `cfg := basetool.ResolveOptions(opts)` 取 `UserID`。
5. `proposer.ProposeInstall(ctx, url, srcType, strings.TrimSpace(in.Ref), basetool.SessionIDFromContext(ctx), cfg.UserID)` → `id`。err 上抛 "propose: %w"。
6. 构造 `out = {status: "pending_approval", approval_id: id, source_type: srcType, url, message: "..."}`。`message` 是 LLM-facing instruction（非 user-visible copy）：inline confirmation card 已渲染，回复一句短句说安装需用户确认，不要引导用户去任何页面/菜单，不要重述 URL/id/status table，不要命名具体按钮 label。
7. Marshal 返回。

**`inferSourceType`**：`strings.ToLower(TrimSpace(url))`，后缀 `.tar.gz` / `.tgz` / `.tar` → `"tarball"`，否则 `"git"`（含 `github.com/...` / `*.git` / `ssh`）。

## 5. 依赖关系

- **basetool**：`ToolInfo`（`Class="destructive"`）、`ResolveOptions(opts)` 取 `UserID`、`SessionIDFromContext(ctx)` 取 sessionID。
- **SkillInstallProposer**：窄接口，cmd/main.go 实现。接口在消费方（tools 包）定义，符合架构红线。
- 不依赖 approval biz / devicebiz / edgebiz / prom / log / trace / tunnel。

## 6. 并发与资源管理

- **无锁、无共享可变状态**：`InstallSkillTool` 仅持有不变 `proposer` 指针，多 goroutine 可并发调用。
- **无 goroutine**：单次 `ProposeInstall` 同步调用。超时由 `ctx` 控制（本工具不加独立超时，依赖上层 agent loop ctx）。
- **无资源持有**：纯排队，不持有 install 资源。

## 7. 设计模式与亮点

- **Human-in-the-loop approval**：与 `cloud_bash` 同 safety model——不直接安装，排队等 approval。`Class="destructive"` 触发 Reviewer 审批装饰器 + 工具内排队 approval inbox，双层防护。注释明示 "installing a skill = granting arbitrary code execution (a skill can ship a binary that cloud_bash then runs), so a human is always in the loop"。
- **`SkillInstallProposer` 窄接口解耦**：接口在消费方定义，cmd/main.go 基于 `biz/approval.Usecase` 实现。本包不 import approval biz，避免循环依赖与跨层调用。
- **`type` 自动推断**：`inferSourceType` 按 URL 后缀判断 git/tarball，LLM 无需显式传 `type`。tarball 仅认 `.tar.gz`/`.tgz`/`.tar`，其他都当 git（含 `github.com/...` / `*.git` / `ssh`）。
- **`message` 是 LLM-facing instruction**：不是 user-visible copy。明确告诉 LLM "inline confirmation card 已渲染，回复一句短句，不要引导用户去页面/菜单，不要重述 URL/id，不要命名按钮 label"。这是 prompt engineering 的一部分，防 LLM 产生冗余/误导回复。
- **无 marketplace**：注释明示 "There is deliberately NO marketplace catalog / search here — the source is whatever the user provided"。这是设计选择——技能源完全由用户控制，避免 LLM 主动搜索并推荐可能恶意的技能。
- **`url` 原样提取**：schema description 明示 "Extract it verbatim from the user's message; do not invent or guess one"。防 LLM 臆造 URL。
- **`Class="destructive"`**：与 `cloud_bash_basetool` 同级，触发最严审批链。

## 8. 注意事项

- **不直接安装**：本工具只排队 proposal，真正 fetch + install 由 approval executor 在用户批准后执行。LLM 调用本工具后应回复 "等待用户确认"，不要声称已安装。
- **`Class="destructive"` 触发 Reviewer**：装饰器链的 `review_gate` 会 spawn reviewer worker（SOP double-sign），工具内又排队 approval inbox。两层审批：reviewer worker + 用户 inline confirmation。
- **`proposer.ProposeInstall` 无独立超时**：本工具不加 `context.WithTimeout`，依赖上层 agent loop ctx。若 approval inbox DB 慢，可能拖到 agent loop 超时。
- **`inferSourceType` 启发式**：仅按后缀判断，`github.com/owner/repo.tar.gz` 会被判 tarball（可能错误）。但实际用户极少这样命名 git repo，启发式足够。
- **`ref` 仅 git 有效**：tarball 源传 `ref` 会被忽略（executor 应自行处理）。
- **`message` 是 LLM instruction**：LLM 应遵循 "回复一句短句" 的指示，不要把 `message` 原样转述给用户。若 LLM 不遵循，用户体验会冗余。
- **无 `ExecuteResult.DeviceID`**：BaseTool 返回纯字符串，无 device 维度。本工具与 device 无关。
- **`approval_id` 回传**：LLM 可用此 id 查询 approval 状态（若有对应工具），但当前未提供查询工具。用户通过 inline card 操作，LLM 无需查询。
- **`url` 不做合法性校验**：本工具不校验 URL 格式 / 是否可达 / 是否 HTTPS。executor 在 fetch 时才校验。LLM 应从用户消息原样提取，不要尝试 normalize。
