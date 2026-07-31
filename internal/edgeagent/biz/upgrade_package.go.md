# `upgrade_package.go` 技术实现文档

> 源文件：`internal/edgeagent/biz/upgrade_package.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/biz`

## 1. 概述

本文件实现两个 handler：`MethodFetchPackage` 下载完整发布包（agent + 插件 + apply 脚本的 tarball）、校验外层 SHA256、解压到 `incoming/` staging 目录、按 `MANIFEST.txt` 逐文件再校验；`MethodApplyPackage` 仅确认 bundle 已 stage 后信号 `Run()` 退出，systemd 重启时由 `apply-pending-upgrade.sh` 真正切换。Stage 与 apply 拆分让 manager 可批量 stage 多个目标后再统一 apply。

## 2. 包信息

- **包名**：`biz`
- **所属模块**：edgeagent 升级能力实现层（bundle 模式，区别于 `upgrade.go` 的单二进制模式）
- **依赖方向**：被 `Agent.registerHandlers` 注册为 handler；调用 `tunnel`

## 3. 关键类型与接口

```go
// bundleDownloadClient 跳过 TLS 校验：私有部署 manager 前置自签 nginx 证书
// SHA256 of bundle 是信任锚 — 任何篡改/MITT/代理替换都会 hash 失败
var bundleDownloadClient = &http.Client{
    Transport: &http.Transport{
        TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
        IdleConnTimeout:       90 * time.Second,
        ResponseHeaderTimeout: 30 * time.Second,
    },
}

const bundleDirName = "incoming"  // stage 子目录名，匹配 apply 脚本读取路径
const maxBundleBytes = 1024 * 1024 * 1024  // 1GB，覆盖 otelcol-contrib ~300MB + promtail ~110MB 等
```

## 4. 关键函数与流程

### `handleFetchPackage`
- **签名**：`func (a *Agent) handleFetchPackage(ctx context.Context, req tunnel.FetchPackageRequest) (tunnel.FetchPackageResponse, error)`
- **职责**：下载 + 校验 + 解压 + 逐文件 hash 校验
- **流程**：
  1. 校验 stage dir / SHA256（64 位小写 hex）/ URL（http(s)）
  2. MkdirAll stage dir；清理 `incoming.tar.gz` 和 `incoming/` 残留
  3. `downloadAndVerify`：流式下载到 `incoming.tar.gz`，在线算 sha256，超 1GB 拒绝
  4. `extractTarGz`：解压到 `incoming/`，拒绝 symlink/hardlink/device、拒绝逃逸路径（zip-slip 防护）、累计字节超限拒绝
  5. 删除 tarball（仅作传输介质）
  6. `verifyManifest`：读 `MANIFEST.txt`，每行 `<sha> <mode> <src> <dest>` 格式，对 stage 下每个 `src` 重新算 sha256 比对
  7. 返回 `FetchPackageResponse{StagedPath, Bytes, ManifestFiles, Version}`
- **错误处理**：任何阶段失败都清理 stage / tarball；manifest 失败删整个 stage

### `handleApplyPackage`
- **签名**：`func (a *Agent) handleApplyPackage(_ context.Context, _ tunnel.ApplyPackageRequest) (tunnel.ApplyPackageResponse, error)`
- **职责**：确认 bundle 已 stage，信号 Run 退出
- **流程**：检查 `<stage>/incoming/MANIFEST.txt` 存在；非阻塞发 `upgradeRequested` 信号；返回 `{Accepted: true}`
- **错误处理**：manifest 缺失返回 "no staged bundle (run fetch_package first)"

### `downloadAndVerify`
- **签名**：`func downloadAndVerify(ctx context.Context, url, expectedSHA, out string) (int64, error)`
- **职责**：流式下载到 out 并校验 sha256
- **流程**：45 分钟超时；`http.NewRequestWithContext` + `bundleDownloadClient.Do`；`io.LimitReader(body, maxBundleBytes+1)` 检测超限；`io.MultiWriter(f, hasher)` 同步算 hash；Sync + Close 后比对 hash
- **错误处理**：任何 IO 错误删除 out；超限 / hash 不符删除 out

### `extractTarGz`
- **签名**：`func extractTarGz(src, dst string) error`
- **职责**：安全解压 tarball 到 dst
- **流程**：
  1. MkdirAll dst
  2. gzip.Reader → tar.Reader
  3. 每个 entry：只接受 `tar.TypeReg` / `tar.TypeDir`，其他（symlink/hardlink/device）拒绝
  4. `filepath.Clean` 后拒绝绝对路径和含 `..` 的路径
  5. 二次保险：`filepath.Abs(target)` 验证在 `absDst` 之下
  6. 累计 `totalBytes` 超限拒绝
- **错误处理**：每个错误都带 entry 名便于定位

### `verifyManifest`
- **签名**：`func verifyManifest(path, stage string) (int, error)`
- **职责**：读 manifest 并对 stage 下每个 src 重算 sha256 比对
- **流程**：bufio.Scanner（1MB buffer）；跳过空行和 `#` 注释；`strings.Fields` 切 4 字段；`fileSHA256(srcPath)` 比对
- **错误处理**：行字段不足 / hash 不符 / 0 文件都返回错误

### `fileSHA256`
- **签名**：`func fileSHA256(path string) (string, error)`
- **职责**：算单个文件的 sha256 hex
- **流程**：`os.Open` → `io.Copy(h, f)` → `hex.EncodeToString`

## 5. 依赖关系

- **内部包**：`internal/pkg/tunnel`
- **外部库**：`archive/tar`、`compress/gzip`、`crypto/sha256`、`crypto/tls`、`net/http`、`bufio`、`gopkg.in/yaml.v3`（间接）、标准 IO
- **被调用方**：`Agent.registerHandlers` 注册的 `MethodFetchPackage` / `MethodApplyPackage` handler 闭包

## 6. 并发与资源管理

- 单 handler 调用同步执行，无 goroutine
- 多个 `defer` 嵌套确保 Body / gz / f 都被关闭
- `context.WithTimeout(ctx, 45*time.Minute)` 长超时；`defer cancel()`
- bundleDownloadClient 是包级单例，连接池复用；operator 配置自签证书时复用同一 client

## 7. 设计模式与亮点

- **Stage / Apply 拆分**：fetch 不触发重启，apply 才信号退出；manager 可批量 stage 多个 edge 后统一 apply，最大化批次原子性
- **两层 hash 校验**：外层 tarball sha256 防 MITM/损坏；内层 MANIFEST.txt 每文件 sha256 防止单文件污染（tarball 自家 pipeline 产出仍校验，纵深防御）
- **Zip-slip 防护三重**：(1) 拒绝非 Regular/Dir 类型 entry；(2) `filepath.Clean` + 拒绝绝对路径/`..`；(3) `filepath.Abs(target)` 二次验证在 absDst 之下
- **1GB 硬上限**：`io.LimitReader(body, maxBundleBytes+1)` 检测超限；解压累计 `totalBytes` 二次校验，防止「小 tarball 但解压炸弹」
- **信任锚是 SHA256 而非 TLS**：自签证书场景下 `InsecureSkipVerify: true` 是有意为之；任何篡改都会让 hash 失败
- **ack-then-exit 模式**：apply_package 先 ack 再信号退出，让 manager 收到响应后才能开始 systemd 重启等待

## 8. 注意事项

- `bundleDownloadClient` 全局跳过 TLS 校验——artifact server 必须在 manager 已信任的网络后方；SHA256 是唯一信任锚
- 1GB 上限按当前 bundle ~460MB（otelcol-contrib + promtail + node_exporter + process_exporter + ongrid-edge）预留 2x headroom；新增大型二进制需重新评估
- MANIFEST.txt 行格式 `<sha> <mode> <src> <dest>`，字段空白分隔，**不支持路径中含空格**（自家 pipeline 产出保证）
- `extractTarGz` 拒绝 symlink——若未来 bundle 需要 symlink（如版本号软链），需放宽此约束
- 文件 mode `&0o755` 限制最大为 rwxr-xr-x；执行位保留，setuid/setgid 被屏蔽
- `handleApplyPackage` 检查 manifest 存在但不验证内容；若 fetch 后文件被外部篡改，apply 仍会触发——apply 脚本应自带二次校验
