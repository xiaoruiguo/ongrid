# signature.go

## 1. 概述

`signature.go` 实现 marketplace 包的签名验证 —— pack 来源完整性与真实性的信任锚。算法是 ECDSA P-256 + SHA-256，签名文件 `signature.json` 与 pack 内容一起分发；缺失即"unsigned"（不一定是失败），存在但验证失败即"failed"。

文件暴露唯一入口 `VerifySignature(packPath, expectedKey)`，被 `usecase.go` 的 `Install` 在 staging→install 移动后调用。`expectedKey` 是 registry pinned PEM 公钥，非空时签名 manifest 的 `pub_key` 必须 DER-equal 匹配。

## 2. 包信息

- 包名：`marketplace`
- 路径：`internal/manager/biz/marketplace`

## 3. 关键类型与接口

### SignatureState（别名 + 常量）

```go
type SignatureState = string

const (
    SigStateVerified SignatureState = "verified"
    SigStateUnsigned SignatureState = "unsigned"
    SigStateFailed   SignatureState = "failed"
)
```

镜像 `model` 包常量，让 biz 层调用方不必跨包 reach。值与 `model.SigState*` 完全一致。

### signatureManifest（wire shape）

```go
const signatureManifestName = "signature.json"

type signatureManifest struct {
    Sig    string `json:"sig"`     // base64(ASN.1 ECDSA 签名)
    PubKey string `json:"pub_key"` // base64(PEM "PUBLIC KEY" PKIX block)
}
```

两个字段都 base64 编码：`Sig` 是 `ecdsa.SignASN1` 的原始字节；`PubKey` 是 base64 包裹的 PEM 块（这样 JSON 保持单行/可粘贴，无需转义换行）。

## 4. 关键函数与流程

### VerifySignature

入口。返回契约：
- `("verified", nil)` —— manifest 存在，hash + 签名有效（pin 匹配若设置）
- `("unsigned", nil)` —— pack 根无 `signature.json`
- `("failed", non-nil err)` —— manifest 存在但任一环节失效

流程：

1. `os.ReadFile(signature.json)`：
   - `iofs.ErrNotExist` → 返回 `unsigned`（不是失败）
   - 其它错误（如权限拒绝）→ 返回 `failed`。注释解释：不当 "unsigned" 处理会因部署错误静默降级信任
2. `json.Unmarshal` → 校验 `Sig` / `PubKey` 非空
3. base64 解码 `Sig` 与 `PubKey`
4. `parseECDSAPublicKey(pubPEM)` 解析公钥，返回 `*ecdsa.PublicKey` + raw DER
5. 若 `expectedKey` 非空：解析 pin key → `bytesEqual(pubDER, expectedDER)` constant-time 比较。不等 → `failed`
6. `computePackHash(packPath)` 重算 pack 的规范 SHA-256
7. `ecdsa.VerifyASN1(pubKey, hash[:], sigBytes)` → 不通过 `failed`，通过 `verified`

### parseECDSAPublicKey

```go
func parseECDSAPublicKey(pemBytes []byte) (*ecdsa.PublicKey, []byte, error)
```

PEM "PUBLIC KEY" (PKIX) → `*ecdsa.PublicKey` + raw DER。非 PEM / 非 ECDSA / 畸形输入均 error。返回的 DER 用于 pin 比较的 constant-time 检查。

### computePackHash

规范 pack hash：SHA-256 over 可签名文件的确定性拼接。

- 可签名 = packPath 下所有 `*.md` / `*.json` 常规文件，**排除** `signature.json` 自身（manifest 不签自己）
- 路径用 forward slash（`filepath.ToSlash`），保证 Linux 签名在 macOS/Windows 验证一致
- 文件先 `filepath.WalkDir` 收集，再按相对路径升序排序，再 concat
- 每文件写入：`relpath + NUL + data + NUL` 作为 domain separator。注释说明：NUL 防止重命名攻击 —— 否则交换两个等名 slot 的内容仍能 hash 匹配

### bytesEqual

```go
func bytesEqual(a, b []byte) bool {
    if len(a) != len(b) {
        return false
    }
    return subtle.ConstantTimeCompare(a, b) == 1
}
```

用 `crypto/subtle.ConstantTimeCompare` 做 constant-time 比较，防 timing side-channel。长度不等先短路（`ConstantTimeCompare` 本身要求等长）。

## 5. 依赖关系

### 外部包

- `crypto/ecdsa` —— ECDSA P-256 verify
- `crypto/sha256` —— pack hash
- `crypto/subtle` —— constant-time 比较
- `crypto/x509` + `encoding/pem` —— PKIX 公钥解析
- `encoding/base64` / `encoding/json` —— wire shape 解码
- `io/fs` / `os` / `path/filepath` / `sort` / `strings` —— 文件遍历

### 被谁调用

- `usecase.go` 的 `Install` 在文件落盘 install path 后调 `VerifySignature(installPath, cfg.SignaturePinnedKey)`
- 信任闸门：`RequireSignedSources` 列表中的 source label 若 state != verified 则拒绝安装（`DevMode` 旁路）

## 6. 并发与资源管理

- 无锁、无 goroutine，纯函数式
- 全部 IO 同步，调用方（`Install`）已在 staging→install 后顺序调用

## 7. 设计模式与亮点

### "缺失"与"失败"严格区分

`iofs.ErrNotExist` → `unsigned`，其它读错误 → `failed`。注释解释意图：权限错误若当 unsigned 处理，会让部署错误（如挂载问题）静默降级信任。

### 跨平台 hash 一致性

`filepath.ToSlash` 强制 forward slash，Linux 签名在 Windows 验证一致。这对 registry 跨平台分发是硬要求。

### NUL domain separator

每文件 `relpath + NUL + data + NUL`。注释给出攻击场景：无 separator 时，交换两个等名 slot 内容能 hash 匹配。NUL 在合法路径与文本中几乎不出现，是安全的分隔符。

### 排除 signature.json 自身

manifest 不签自己的签名 —— 否则签名会包含自己的内容，而签名本身在签名时还未生成，构成鸡生蛋悖论。注释明确这一点。

### Constant-time pin 比较

pinned key 的 DER 比较 用 `subtle.ConstantTimeCompare`，防 timing 攻击推断 pin key 字节。长度不等先短路（无法 constant-time 比较不同长度）。

### ECDSA P-256 选择

注释虽未明说，但 P-256 是 NIST 标准曲线，与现代 git signing、JWT 等生态兼容；比 RSA 签名短、验证快。ASN.1 形式（`VerifyASN1`）是 Go `crypto/ecdsa` 的原生输出。

## 8. 注意事项

- **unsigned 不是失败**：`RequireSignedSources` 列表外的 source（如 local / git）允许 unsigned。只有 `ongrid-official` 这类 registry source 强制 verified
- **expectedKey 空跳过 pin**：`SignaturePinnedKey` env 未设时，任何 well-formed ECDSA key 都能通过签名验证（但仍需签名匹配内容）。生产部署应设 `ONGRID_MARKETPLACE_PINNED_PUBKEY`
- **签名算法固定 ECDSA**：解析路径只接受 ECDSA 公钥（`pub.(*ecdsa.PublicKey)` 类型断言），RSA/Ed25519 签名会被拒绝。若未来要支持多算法，需扩展 `parseECDSAPublicKey`
- **可签名文件仅 .md/.json**：其它扩展名（.yaml/.txt/二进制）不参与 hash。若 pack 含可执行脚本，签名不覆盖 —— 信任模型假设 chatruntime.LoadPluginContainer 的 escapes_root 检查兜底
- **symlink 跳过**：`filepath.WalkDir` 的 `info.Mode().IsRegular()` 过滤掉 symlink。pack 应是 plain tree，symlink 由加载阶段的 `escapes_root` 检查处理
- **`signatureManifestName` 是 pack 根固定文件名**：不在子目录。改文件名要同步改 `computePackHash` 的排除逻辑
- **DevMode 旁路**：`usecase.go` 的信任闸门在 `DevMode=true` 时跳过 `RequireSignedSources` 检查，dev cluster 可不签名迭代
