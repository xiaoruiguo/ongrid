# marketplace/upload.go 技术实现文档

## 1. 概述

`upload.go` 实现 `POST /v1/marketplace/upload` 端点：从浏览器上传 skill pack 归档文件（zip / tar.gz / tgz / tar），解压到临时目录后交给与本地目录安装相同的 `svc.Install` 路径。admin-only。包含 zip-slip 防护、zip-bomb 防护、可执行位保留等安全加固。

## 2. 包信息

- **包名**：`marketplace`（与 `http.go` 同包）
- **导入路径**：`github.com/ongridio/ongrid/internal/manager/server/marketplace`
- **路由前缀**：`/v1/marketplace/upload`（在 `http.go.Register` 中挂载）
- **文件定位**：HTTP 端点 + 归档解压工具集

## 3. 关键类型与接口

本文件无类型定义，所有方法挂在 `http.go` 中定义的 `*Handler` 上。核心常量：

```go
const (
    maxUploadBytes   = 64 << 20  // 64 MiB request cap
    maxExtractedFile = 128 << 20 // per-file decompressed cap (zip-bomb guard)
)
```

- `maxUploadBytes`：HTTP 请求体总大小上限 64 MiB
- `maxExtractedFile`：单文件解压后大小上限 128 MiB（zip-bomb 防护——压缩比可超 1000x）

## 4. 关键函数与流程

### upload —— 主流程

```go
func (h *Handler) upload(w http.ResponseWriter, r *http.Request)
```

1. `requireAdmin` → 非 admin 403
2. `http.MaxBytesReader(w, r.Body, maxUploadBytes)` —— 64 MiB 上传硬上限
3. `r.ParseMultipartForm(maxUploadBytes)` —— multipart 解析（同样按 64 MiB 限）
4. `r.FormFile("file")` 取上传文件句柄 + `header.Filename`
5. **spool to tmp file**：`os.CreateTemp("", "ongrid-upload-*"+archiveExt(...))` 落临时文件，因为 `zip.OpenReader` 需 `ReaderAt` 接口，且让 zip/tar 共用一条代码路径
6. `io.Copy(tmp, file)` 把上传内容写到 tmp，`defer os.Remove(tmpPath)` 清理
7. `os.MkdirTemp("", "ongrid-pack-*")` 创建解压目标目录，`defer os.RemoveAll(dest)` 清理
8. `extractArchive(tmpPath, header.Filename, dest)` 按扩展名分发到 zip/tar.gz/tar 解压器
9. `descendSingleDir(dest)` 处理"归档包裹单一顶层目录"的常见 case
10. `h.svc.Install(ctx, caller, bizmp.Source{Type: "local", Path: root})` 走与本地目录安装相同的 biz 路径
11. 200 + `InstallResult`

### archiveExt —— 扩展名识别

```go
func archiveExt(name string) string
```

返回 `.tar.gz` / `.tgz` / `.zip` / `.tar` 之一，default 走 `filepath.Ext(name)`。用于 tmp 文件后缀，便于调试。

### extractArchive —— 分发解压

```go
func extractArchive(archivePath, filename, dest string) error
```

按文件名小写后缀分发：
- `.zip` → `extractZip`
- `.tar.gz` / `.tgz` → `gzip.NewReader` + `extractTar`
- `.tar` → `extractTar`
- 其它 → 错误"unsupported archive"

### extractZip —— zip 解压

```go
func extractZip(archivePath, dest string) error
```

`zip.OpenReader` 遍历每个 entry：
1. `safeJoin(dest, f.Name)` 防止 zip-slip（详见下文）
2. 目录：`os.MkdirAll(target, 0o755)`
3. 文件：`os.MkdirAll(filepath.Dir(target))` + `writeFile(target, rc, f.FileInfo().Mode())`

### extractTar —— tar 解压

```go
func extractTar(r io.Reader, dest string) error
```

`tar.NewReader` 遍历每个 header：
1. `safeJoin(dest, hdr.Name)` 防 zip-slip
2. `TypeDir` → `os.MkdirAll`
3. `TypeReg` → `writeFile`
4. **symlinks / devices / 其它 typeflag 静默跳过**（skill pack 是纯文件，不支持软链/设备节点）

### writeFile —— 写文件 + 权限处理

```go
func writeFile(target string, r io.Reader, mode os.FileMode) error
```

1. **可执行位保留**：`mode & 0o111 != 0` → `perm = 0o755`，否则 `0o644`——让带可执行的 skill（如 terraform 二进制）保持可运行
2. **剥离 setuid/setgid/sticky**：只用 0o755 或 0o644，不保留 archive 中的特殊权限位（防提权）
3. `os.OpenFile(target, O_CREATE|O_WRONLY|O_TRUNC, perm)` 创建
4. **`out.Chmod(perm)` 显式 chmod**：因为 `OpenFile` 受 umask 影响且不会重新 mode 已存在文件，显式 chmod 保证可执行位不被 umask 抹掉
5. `io.Copy(out, io.LimitReader(r, maxExtractedFile))` —— 单文件 128 MiB 上限（zip-bomb 防护）

### safeJoin —— zip-slip 防护

```go
func safeJoin(base, name string) (string, bool)
```

`filepath.Join(base, name)` 后用 `filepath.Rel(base, target)` 检查结果是否仍在 base 内：
- `rel == ".."` 或前缀为 `..` + 路径分隔符 → 越界，返 `("", false)`
- 其它 → 返 `(target, true)`

防止恶意 archive 中的 `../../etc/passwd` 之类条目逃逸解压目录。

### descendSingleDir —— 单顶层目录处理

```go
func descendSingleDir(dir string) string
```

如果 `dir` 下只有一个 entry 且是目录（常见的"archive 包了一个顶层文件夹"case，如 `my-skill/SKILL.md`），返回该子目录路径；否则返回 `dir` 不变。让 pack 根目录探测（`DetectContainer → LoadPluginContainer`）能正确找到 `SKILL.md`。

## 5. 依赖关系

**外部**：
- `archive/tar`、`archive/zip`、`compress/gzip` —— 归档解压
- `io`、`os`、`path/filepath`、`strings`、`errors`、`fmt`、`net/http`

**内部**：
- `internal/manager/biz/marketplace`（`bizmp.Source` / `bizmp.Caller` / `bizmp.InstallResult`）
- `internal/pkg/errs`

## 6. 并发与资源管理

- **临时文件清理**：
  - 上传 tmp：`defer os.Remove(tmpPath)` —— 函数返回即删
  - 解压目录：`defer os.RemoveAll(dest)` —— 函数返回即删
  - 即使 install 失败也清理，避免泄漏
- **`defer file.Close()`**：multipart 文件句柄释放
- **`defer tmp.Close()`**：tmp 文件句柄释放（在 `io.Copy` 之后立即 Close，避免 fd 占用）
- **无共享可变状态**：所有状态局部于请求，无 cross-request 共享
- **`LimitReader` 防 zip-bomb**：每个文件解压时套 128 MiB 上限，防压缩比炸弹耗尽磁盘

## 7. 设计模式与亮点

1. **spool to tmp file 统一 code path**：把 multipart 上传先落到 tmp 文件，让 zip（需 `ReaderAt`）和 tar 共用同一解压入口，避免 zip/tar 分两套 multipart 处理代码。
2. **zip-slip 防护**：`safeJoin` 显式校验每个 entry 的目标路径不逃逸 base 目录，防 `../../etc/passwd` 类攻击。这是处理上传归档的**必备**安全措施。
3. **zip-bomb 防护**：双层限制——上传总大小 64 MiB + 单文件解压 128 MiB，防高压缩比炸弹。
4. **可执行位保留 + 特权位剥离**：`0o111` 位检测保留可执行（让 terraform 等二进制 skill 可运行），但剥离 setuid/setgid/sticky 防提权。
5. **`out.Chmod(perm)` 显式 chmod**：`OpenFile` 受 umask 影响不会重 mode 已存在文件，显式 chmod 保证可执行位不被抹掉——这是经过 bug 才加的细节。
6. **`descendSingleDir` 自动适配常见打包习惯**：用户常用 `tar czf my-skill.tar.gz my-skill/` 这种带顶层目录的方式打包，自动 descend 让 pack 根探测无需用户手工对齐目录结构。
7. **symlinks 静默跳过**：tar 中 symlink/device 等非常规文件不处理，因为 skill pack 是纯文件——避免 symlink 攻击（指向 base 外的敏感文件）。
8. **复用 `svc.Install` 本地路径**：上传安装与本地目录安装走相同 biz 路径，避免两套 install 逻辑漂移。

## 8. 注意事项

1. **64 MiB 上传上限**：超出会 `ParseMultipartForm` 报错，返 `errs.ErrInvalid` + 400；如需支持更大 pack 需调整常量并评估内存/磁盘压力。
2. **128 MiB 单文件解压上限**：超出会被 `LimitReader` 截断，导致文件损坏（`io.Copy` 返 EOF 但内容不全）；当前未显式检测截断，可能产生静默坏文件——如需严格应比较 `Copy` 返回的 `n` 与后续读取是否 EOF。
3. **symlinks 不解压**：tar 中的 symlink 静默跳过，不报错；如 skill 依赖 symlink（如 `node_modules/.bin`）会失效，需要 pack 改用真实文件或脚本动态创建。
4. **`safeJoin` 在 Windows 上**：`filepath.Rel` 跨平台，但 zip/tar entry 名通常是 `/` 分隔，Windows 上 `filepath.Join` 会自动转换——需测试 Windows 部署（如有）。
5. **临时文件权限**：`os.CreateTemp` 默认 0o600，安全性 OK；解压目录默认 0o700（`MkdirTemp`），同样安全。
6. **`descendSingleDir` 只 descend 一层**：如果归档结构是 `top1/top2/SKILL.md`（嵌套两层），不会自动 descend 到 top2，pack 根探测会失败——用户需打包成 `my-skill/SKILL.md` 单层结构。
7. **install 失败时清理**：`defer os.RemoveAll(dest)` 保证 install 失败也清理解压目录，但 install 成功后 biz 层会把文件移到永久位置（`skills root`），此时 dest 已空，`RemoveAll` 是 no-op。
8. **admin-only 但无 casbin 细粒度**：`requireAdmin` 是基于 role 字符串"admin"的硬判定，未走 casbin RBAC；如需"特定 admin 才能安装特定 registry 的 pack"需扩 Service 接口。
