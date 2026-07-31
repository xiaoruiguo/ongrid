# `service.go` 技术实现文档

> 源文件：`internal/manager/service/systemupgrade/service.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/service/systemupgrade`

## 1. 概述

本文件是系统升级检查服务，查询上游 ongrid release 元数据，比对当前版本，并为 Web UI 生成操作员可执行的升级命令。核心设计：(1) 多源 fallback —— 默认两个 URL（ongrid.cloud/dl/latest.json + GitHub releases/latest），任一可用即返回；(2) 支持双响应形态（JSON 对象 / 纯文本首字段），适配不同 release metadata 服务；(3) semver 比对仅支持 `vX.Y.Z` 三段数字，不支持 pre-release / build metadata，不可比时 `ComparisonSupported=false` 让 UI 显示"无法比较"而非误报。

## 2. 包信息

- **包名**：`systemupgrade`
- **所属模块**：`internal/manager/service/systemupgrade`
- **依赖方向**：被 HTTP handler 调用；仅依赖标准库（`net/http` / `encoding/json` / `strconv` / `strings` / `time` / `io` / `context` / `fmt` / `errors`）

## 3. 关键类型与接口

```go
const (
    defaultReleaseMetadataURL = "https://ongrid.cloud/dl/latest.json"
    defaultGitHubReleaseURL   = "https://api.github.com/repos/ongridio/ongrid/releases/latest"
    defaultDownloadBase       = "https://ongrid.cloud/dl"
    maxReleaseMetadataBytes   = 1 << 20  // 1 MiB
)

type Config struct {
    CurrentVersion  string
    ReleaseAPIURL   string   // 单 URL（向后兼容）
    ReleaseAPIURLs  []string  // 多 URL（优先）
    DownloadBase    string
    Timeout         time.Duration
}

type Service struct {
    cfg  Config
    http *http.Client
}

type Info struct {
    CurrentVersion      string
    LatestVersion       string
    UpdateAvailable     bool
    ComparisonSupported bool
    ReleaseURL          string
    PublishedAt         *time.Time
    CheckedAt           time.Time
    Commands            []UpgradeCommand
}

type UpgradeCommand struct{ ID, Label, Arch, Command string }

// 内部类型
type releasePayload struct {
    TagName, Version, LatestVersion, HTMLURL, ReleaseURL, PublishedAt, DownloadBase string
}

type releaseInfo struct {
    latestVersion string
    releaseURL    string
    publishedAt   *time.Time
    downloadBase  string
}
```

## 4. 关键函数与流程

### `New(cfg, client)`

- `normalizeReleaseURLs(cfg)` 规范化 URL 列表（ReleaseAPIURLs 优先，否则 [ReleaseAPIURL]，否则默认双源）。
- DownloadBase 空 → defaultDownloadBase；Timeout<=0 → 5s；client nil → `&http.Client{Timeout: cfg.Timeout}`。

### `Check(ctx) (*Info, error)`

- **职责**：遍历 ReleaseAPIURLs，首个成功 fetch 即返回；全失败合并 error。
- **流程**：
  1. 遍历 `s.cfg.ReleaseAPIURLs`。
  2. `fetchLatest(ctx, sourceURL)` 成功 → `buildInfo(rel)` 返回。
  3. 失败 → 收集 `fmt.Errorf("%s: %w", sourceURL, err)`。
  4. 全失败 → `fmt.Errorf("fetch latest release: %w", errors.Join(fetchErrs...))`。

### `buildInfo(rel) *Info`

- **流程**：
  1. TrimSpace latest。
  2. `compareVersions(CurrentVersion, latest)` 返回 (cmp, comparable)。
  3. `updateAvailable = comparable && cmp < 0`。
  4. downloadBase 空 → cfg.DownloadBase。
  5. 构造 Info{Commands: buildCommands(latest, downloadBase)}。

### `fetchLatest(ctx, sourceURL) (*releaseInfo, error)`

- **流程**：
  1. `context.WithTimeout(ctx, cfg.Timeout)`。
  2. `http.NewRequestWithContext(GET, sourceURL)`；Header Accept `application/json, text/plain;q=0.9, */*;q=0.8`；User-Agent `ongrid/<version>`。
  3. `s.http.Do(req)`；`io.LimitReader(resp.Body, maxReleaseMetadataBytes)` 读（防 OOM）。
  4. status 非 2xx → `fmt.Errorf("HTTP %d: %s", code, body-or-status)`。
  5. `parseReleaseMetadata(raw)`。

### `parseReleaseMetadata(raw) (*releaseInfo, error)`

- **职责**：双形态解析 —— JSON 对象或纯文本首字段。
- **流程**：
  1. TrimSpace；空 → error。
  2. `strings.HasPrefix(body, "{")` → JSON 路径：unmarshal `releasePayload`；`firstNonEmpty(TagName, LatestVersion, Version)` 取版本；空 → error；`parseOptionalTime(PublishedAt)`；构造 releaseInfo。
  3. 否则纯文本路径：`strings.Fields(body)`；首字段作 latestVersion。
- **错误处理**：JSON decode err → `%w "decode latest release"`；version 空 → error；publishedAt 非法 RFC3339 → error。

### `buildCommands(version, downloadBase) []UpgradeCommand`

- 生成三个命令：linux-amd64 / linux-arm64 / auto（uname 自动检测）。
- 命令模板：`curl -fL -O <base>/ongrid-<version>-linux-<arch>.tar.xz || wget ...` → `tar xf` → `cd` → `sudo ./upgrade.sh`。

### `compareVersions(current, latest) (int, bool)`

- `parseVersion` 各自解析 `[3]int`；任一失败 → `(0, false)`；逐位比较返回 (-1/0/1, true)。

### `parseVersion(v) ([3]int, bool)`

- TrimSpace + TrimPrefix "v"；截断 `+-` 后的 pre-release/build metadata。
- `strings.Split(s, ".")` 必须 3 段；每段 `strconv.Atoi` 且 >=0；否则 false。

### 辅助

- **`firstNonEmpty(values...)`**：TrimSpace 后取首个非空。
- **`parseOptionalTime(value)`**：空 → nil；RFC3339 parse 失败 → error。
- **`normalizeReleaseURLs(cfg)`**：ReleaseAPIURLs 优先；否则 [ReleaseAPIURL]；否则默认双源；TrimSpace 去空。

## 5. 依赖关系

- **内部包**：无（仅标准库）
- **外部库**：`net/http`、`encoding/json`、`strconv`、`strings`、`time`、`io`、`context`、`fmt`、`errors`
- **被调用方**：HTTP handler（/v1/system/upgrade/info 等）

## 6. 并发与资源管理

- **无共享可变状态**：Service 字段在 New 后只读。
- **每请求独立 timeout ctx**：`fetchLatest` 用 `context.WithTimeout(ctx, cfg.Timeout)`；defer cancel。
- **HTTP client 共享**：Timeout=cfg.Timeout（5s 默认）。
- **响应体 LimitReader 1MiB**：防恶意/异常大响应 OOM。

## 7. 设计模式与亮点

- **多源 fallback**：默认两个 URL（ongrid.cloud + GitHub），任一可用即返回；`errors.Join` 合并所有失败错误便于诊断。
- **双响应形态解析**：JSON 对象走结构化字段（TagName / Version / LatestVersion 三选一）；纯文本走首字段；适配不同 release metadata 服务。
- **semver 严格三段**：仅 `vX.Y.Z` 数字；pre-release / build metadata 截断后比较；不可比返回 `ComparisonSupported=false` 让 UI 显示"无法比较"而非误报。
- **User-Agent 自报版本**：`ongrid/<current_version>` 让上游服务统计。
- **LimitReader 1MiB**：防异常响应 OOM。
- **buildCommands 三架构**：amd64 / arm64 / auto（uname 检测）；auto 用 shell case 语句映射 `x86_64|amd64` → amd64，`aarch64|arm64` → arm64。
- **curl || wget 双备**：命令模板同时支持 curl 和 wget，兼容不同 Linux 发行版。
- **downloadBase 可配**：默认 ongrid.cloud/dl，可被 release metadata 覆盖。

## 8. 注意事项

- **不支持 pre-release / build metadata**：`v1.2.3-rc1` 与 `v1.2.3` 被视为相等（截断后比较）；UI 会显示无更新，需用户自行判断。
- **Timeout 默认 5s**：网络慢时需调大；fetchLatest 用 cfg.Timeout 覆盖 caller ctx。
- **GitHub API 限流**：未认证 GitHub releases API 限流 60/h；ongrid.cloud 主源优先可缓解。
- **maxReleaseMetadataBytes 1MiB**：超限部分被截断，可能导致 JSON decode 失败；正常 release metadata 远小于此。
- **parseOptionalTime 仅 RFC3339**：其他格式（如 ISO8601 无时区）会失败 → 整个 fetchLatest 失败。
- **buildCommands 命令含 `sudo`**：要求操作员有 sudo 权限；UI 应提示。
- **commands 是字符串列表**：UI 直接展示让操作员复制粘贴；非自动化执行。
- **ReleaseAPIURL vs ReleaseAPIURLs**：单 URL 字段向后兼容；多 URL 优先；同时存在时 ReleaseAPIURLs 胜出。
- **Check 无 ctx 透传到 fetch**：实际有透传（fetchLatest 接受 ctx）；注释正确。
- **CompareVersions 的 cmp==0 也算 comparable**：updateAvailable 仅 cmp<0 时 true；相同版本不报更新。
