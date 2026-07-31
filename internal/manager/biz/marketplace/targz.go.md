# targz.go

## 1. 概述

`targz.go` 实现 marketplace 包的 tarball 解压 —— 把 gzip 压缩的 tar 流展开到目标目录。它是 `usecase.fetchToStaging` 处理 `SourceTypeTarball` 时的底层工具，安全是首要关注点：

- **路径穿越防护**：拒绝绝对路径、含 `..` 的 entry，再 re-resolve 检查结果仍在 dst 内
- **symlink 限制**：symlink 目标解析后必须落在 dst 内才接受
- **per-file 大小上限**：32 MB，skill bundle 应是小包
- **非常规类型跳过**：hardlink / device / fifo 等静默跳过

## 2. 包信息

- 包名：`marketplace`
- 路径：`internal/manager/biz/marketplace`

## 3. 关键类型与接口

本文件**无导出类型**，仅暴露一个函数 `extractTarGz`。

## 4. 关键函数与流程

### extractTarGz

```go
func extractTarGz(r io.Reader, dst string) error
```

流程：

1. `gzip.NewReader(r)` 包裹，defer close
2. `tar.NewReader(gz)`
3. `filepath.Abs(dst)` 得到 `cleanDst`
4. 循环 `tr.Next()`：
   - `filepath.IsAbs(hdr.Name) || strings.Contains(hdr.Name, "..")` → skip（第一道防线）
   - `target = filepath.Join(cleanDst, hdr.Name)`
   - `filepath.Abs(target)` re-resolve → 检查 `strings.HasPrefix(clean, cleanDst+sep) || clean == cleanDst`（第二道防线，捕获 sneaky `./..` 模式）
   - 按 `hdr.Typeflag` 分发：
     - `TypeDir` → `os.MkdirAll(clean, fs(hdr.Mode, 0o755))`
     - `TypeReg` / `TypeRegA` → `os.MkdirAll(dir)` + `os.OpenFile(O_RDWR|O_CREATE|O_TRUNC)` + `io.CopyN(out, tr, 32<<20)`（32 MB cap）
     - `TypeSymlink` → 解析 linkbase + linkname，re-resolve 检查在 dst 内才 `os.Symlink`
     - 其它（hardlink / device / fifo）→ skip

5. `io.EOF` → 正常返回

### fs（辅助）

```go
func fs(mode int64, fallback os.FileMode) os.FileMode {
    if mode == 0 {
        return fallback
    }
    return os.FileMode(mode) & 0o777
}
```

tar header 的 mode 为 0 时用 fallback（dir 0o755、file 0o644），否则取 mode 的低 9 位权限。

## 5. 依赖关系

### 外部包

- `archive/tar` + `compress/gzip` —— tar.gz 解压
- `io` / `os` / `path/filepath` / `strings` —— IO 与路径处理

### 被谁调用

- `usecase.downloadAndExtractTarball` 把 HTTP 响应 body 直接喂给 `extractTarGz(resp.Body, dst)`

## 6. 并发与资源管理

- 无锁、无 goroutine，纯顺序 IO
- `defer gz.Close()` 保证 gzip reader 关闭
- `out.Close()` 在每文件结束时显式调用（错误路径也调）
- per-file 32 MB 上限防大文件耗内存

## 7. 设计模式与亮点

### 双重路径穿越防护

第一道：`IsAbs` 或含 `..` 直接 skip（早拒绝，便宜）。
第二道：`filepath.Abs` re-resolve 后 `HasPrefix` 检查（捕获 cleaner 折叠掉的 sneaky 模式）。

注释明确这是"defence in depth" —— `LoadPluginContainer` 的 `pathSafeUnderRoot` 是第三道防线。

### Symlink 限制

symlink 的 linkname 解析后必须落在 `cleanDst` 内。这防止 symlink 指向 `/etc/passwd` 等敏感路径。注释提到 `LoadPluginContainer` 的 `escapes_root` 检查是更上层防线。

### 32 MB per-file cap

```go
io.CopyN(out, tr, 32<<20)
```

注释：skill bundle 应是小包，> 32 MB 几乎必是 bug 或攻击（zip bomb）。`CopyN` 截断而非报错（返回 `io.EOF` 时正常结束），但下一文件继续。这是有意的 —— 让解压完成，让上层验证拒绝整个 pack，而非在解压中途失败留下半解压状态。

### 常规类型白名单

只处理 `TypeDir` / `TypeReg` / `TypeRegA` / `TypeSymlink`。hardlink / char device / block device / fifo 静默 skip。注释：marketplace 是 skill bundle，不是任意 archive。

### fallback 权限

`fs(mode, fallback)`：tar header mode=0 时用 fallback。某些打包工具不写 mode，fallback 保证可读可执行。

## 8. 注意事项

- **32 MB 是 per-file 不是 total**：一个含 100 个 30 MB 文件的 pack 仍能解压。total cap 由 staging dir 磁盘配额隐式约束
- **`CopyN` 截断不报错**：超过 32 MB 的文件被静默截断。若需明确拒绝，应改成检查 `CopyN` 返回的 n + 后续 `tr.Next()` 是否还在同文件 —— 当前实现接受截断
- **symlink 失败静默 skip**：linkname 指向 dst 外的 symlink 不报错，只是不创建。这可能导致 pack 加载时引用缺失文件 —— 但 `LoadPluginContainer` 的 `escapes_root` 检查会捕获
- **`TypeRegA` 兼容老 tar**：`TypeRegA` 是旧 tar 的"regular or unknown"，现代 tar 用 `TypeReg`。两者都处理保证兼容
- **不做 owner/group 还原**：tar header 的 uid/gid/uname/gname 被忽略，文件以进程 uid 创建。容器内通常 root，无安全问题
- **mtime 不还原**：文件 mtime 是创建时间。若 pack 依赖 mtime 排序需另行处理
- **gzip bomb 防护弱**：32 MB per-file 限制文件，但 gzip 流本身无解压比例上限。恶意 1 GB 压缩流可能解压成几百 GB 目录。生产部署应配 staging dir 磁盘配额
