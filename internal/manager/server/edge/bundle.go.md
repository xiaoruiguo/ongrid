# `bundle.go` 技术实现文档

> 源文件：`internal/manager/server/edge/bundle.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/server/edge`

## 1. 概述

本文件实现 edge 升级 bundle 的本地文件解析器 + 静态 URL 构造器。manager 镜像在 `/usr/share/ongrid/edge-bundles/edge-bundle-<arch>-<version>.tar.gz`（+ `.sha256` 伴生文件）烘焙了 bundle，本解析器按 (arch, version) 定位文件、读 sha256、拼出可被 edge 拉取的公网 URL。设计要点：bundle 文件由 nginx 从同一路径静态分发，sha256 是唯一校验门；空 version 回退到 manager 自身版本。关键红线：arch 走白名单（`linux-amd64`/`linux-arm64`）；publicURL 必须配置否则报错。

## 2. 包信息

- **包名**：`edge`（与 `http.go` 同包）
- **所属模块**：`internal/manager/server/edge`
- **依赖方向**：被 `cmd/ongrid/main.go` 装配时构造，注入到 `Handler.SetPackageResolver`；依赖 `pkg/errs`

## 3. 关键类型与接口

```go
// 实现 edge.PackageResolver 接口（在 http.go 定义）
type FileBundleResolver struct {
    dir            string // 烘焙目录，通常 /usr/share/ongrid/edge-bundles
    managerVersion string // manager 自身版本，空 version 时回退用
    publicURL      string // manager 外部可达 origin，无 trailing slash
}
```

## 4. 关键函数与流程

### `NewFileBundleResolver`
- **签名**：`func NewFileBundleResolver(dir, managerVersion, publicURL string) *FileBundleResolver`
- **职责**：构造 resolver；TrimRight 去掉 dir / publicURL 末尾 `/`
- **流程**：直接赋值

### `ResolveBundle`
- **签名**：`func (r *FileBundleResolver) ResolveBundle(arch, version string) (url, sha256, resolvedVersion string, err error)`
- **职责**：定位 bundle 文件 + 读 sha + 拼 URL
- **流程**：
  1. `r == nil` → error「bundle resolver not wired」
  2. `!knownArch(arch)` → ErrInvalid「unsupported arch」
  3. version 空 → 回退 `r.managerVersion`；仍空 → error「manager version unknown」
  4. `name = edge-bundle-<arch>-<version>.tar.gz`
  5. `os.Stat(tarball)` 不存在 → error「bundle missing: <name> (this manager image may have been built without build-edge-bundle)」
  6. `os.ReadFile(tarball + ".sha256")` 失败 → error「bundle sha file missing」
  7. `sha = TrimSpace`；`len(sha) < 64` → error「malformed」；取前 64 字符
  8. `r.publicURL == ""` → error「publicURL not configured」
  9. 返回 `{publicURL}/edge/{name}`、sha、version、nil
- **错误处理**：每步失败返 descriptive error；注释明示 bundle bytes 由 nginx 静态分发，sha256 是校验门

### `knownArch`
- **签名**：`func knownArch(a string) bool`
- **职责**：arch 白名单校验
- **流程**：仅 `linux-amd64` / `linux-arm64` 返 true

## 5. 依赖关系

- **内部包**：`pkg/errs`（`ErrInvalid`）
- **外部库**：标准库 `os`、`path/filepath`、`strings`
- **被调用方**：`cmd/ongrid/main.go` 装配时构造，通过 `Handler.SetPackageResolver` 注入

## 6. 并发与资源管理

- **无共享状态**：resolver 字段启动期设定后只读
- **无缓存**：每次 ResolveBundle 都 `os.Stat` + `os.ReadFile`（文件小，sha256 仅 64 字节）
- **无锁**：只读字段 + 只读文件系统

## 7. 设计模式与亮点

- **文件名约定即协议**：`edge-bundle-<arch>-<version>.tar.gz` + `.sha256` 伴生——简单可靠，无需 DB 或 manifest
- **空 version 回退 manager 版本**：「upgrade to current」语义——edge 升级到与 manager 同版本是典型场景
- **sha256 是唯一校验门**：注释明示「Anonymous fetch; sha256 is the gate」——bundle bytes 走 nginx 静态分发无需鉴权，sha 防篡改
- **descriptive error**：每步失败都带具体提示（bundle missing / sha missing / malformed / publicURL 未配），方便运维排查
- **TrimRight 去 trailing slash**：避免拼出 `//edge/` 双斜杠 URL
- **sha 取前 64 字符**：容错 `.sha256` 文件含换行或额外字符

## 8. 注意事项

- **arch 白名单写死**：仅 `linux-amd64`/`linux-arm64`；新增 arch 需改 `knownArch`
- **`managerVersion` 来自构建期注入**：若构建未注入版本会返「manager version unknown」
- **`publicURL` 必须配置**：未配置时 ResolveBundle 报错；部署时需设 manager 外部可达 origin
- **bundle 文件由 Dockerfile 烘焙**：注释明示路径 `/usr/share/ongrid/edge-bundles/`；若镜像未跑 `build-edge-bundle` 会报 missing
- **无缓存**：高频调用时每次读文件；当前调用频率低（升级时），无需缓存
- **sha 文件容错**：`len(sha) < 64` 报 malformed；≥64 取前 64 字符，容忍尾部换行
- **`r == nil` 检查**：允许 nil receiver 调用返 descriptive error，避免 panic
