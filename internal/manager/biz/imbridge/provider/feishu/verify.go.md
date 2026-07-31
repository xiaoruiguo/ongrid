# `verify.go` 技术实现文档

> 源文件：`internal/manager/biz/imbridge/provider/feishu/verify.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/biz/imbridge/provider/feishu`

## 1. 概述

本文件是飞书 webhook 模式的入站安全层：校验事件签名 + 解密 AES-256-CBC 加密 payload。设计要点：签名算法是 SHA-256（非 HMAC），encryptKey 作为 hash 输入的共享密钥；解密 key 是 `SHA-256(encrypt_key)`，IV 是 base64 密文前 16 字节。红线：`ErrBadSignature` 独立类型，HTTP handler 据此映射 401。

## 2. 包信息

- **包名**：`feishu`
- **所属模块**：`internal/manager/biz/imbridge/provider/feishu`
- **依赖方向**：被飞书 webhook handler 调用（webhook 模式）；仅依赖标准库 crypto

## 3. 关键类型与接口

```go
// ErrBadSignature 是 VerifyEventSignature 失败时的错误。独立类型让 HTTP handler
// 干净地映射到 401。
var ErrBadSignature = errors.New("feishu: signature mismatch")
```

## 4. 关键函数与流程

### `VerifyEventSignature`
- **签名**：`func VerifyEventSignature(timestamp, nonce, encryptKey string, body []byte, signatureHex string) error`
- **职责**：校验 X-Lark-Signature 头
- **算法**（飞书 v2 事件）：
  ```
  sig = sha256(timestamp + nonce + encryptKey + body)，hex 编码
  ```
  注释明示：是 SHA-256 而非 HMAC，encryptKey 在 hash 输入中扮演共享密钥角色
- **流程**：
  1. `sha256.New()` 依次 Write timestamp/nonce/encryptKey/body
  2. `hex.EncodeToString` 后 `EqualFold` 比对（大小写不敏感）
  3. 匹配返回 nil；不匹配返回 `ErrBadSignature`
- **错误处理**：输入畸形不 panic，统一返回 ErrBadSignature

### `DecryptEvent`
- **签名**：`func DecryptEvent(encryptKey string, encryptedBase64 string) ([]byte, error)`
- **职责**：解密飞书 AES-256-CBC 加密的事件 JSON
- **流程**：
  1. `encryptKey == ""` → error
  2. `base64.StdEncoding.DecodeString` 解码密文
  3. `len(raw) < aes.BlockSize*2` → error（ciphertext too short）
  4. `keyHash = sha256.Sum256(encryptKey)` 作为 AES key
  5. `iv = raw[:aes.BlockSize]`（前 16 字节）
  6. `ct = raw[aes.BlockSize:]`（剩余密文）
  7. `len(ct) % aes.BlockSize != 0` → error（非块对齐）
  8. `cipher.NewCBCDecrypter(block, iv).CryptBlocks(pt, ct)`
  9. `pkcs7Unpad(pt)` 去填充
- **错误处理**：base64 失败 / 密文过短 / 块不对齐 / PKCS7 填充错误均返回明确 error

### `pkcs7Unpad`
- **签名**：`func pkcs7Unpad(b []byte) ([]byte, error)`
- **职责**：去除 PKCS#7 填充
- **校验**：pad ≤ 0 / pad > BlockSize / pad > len(b) / 填充字节不等于 pad 值 → error

### `sortedKeys`
- **签名**：`func sortedKeys(m map[string]string) []string`
- **职责**：测试辅助，返回排序后的 key 列表
- **导出策略**：`var _ = sortedKeys` 仅在测试需要时使用

## 5. 依赖关系

- **内部包**：无
- **外部库**：仅标准库 `crypto/aes`、`crypto/cipher`、`crypto/sha256`、`encoding/base64`、`encoding/hex`、`sort`、`strings`
- **被调用方**：飞书 webhook handler（webhook 模式下使用；stream 模式不需要）

## 6. 并发与资源管理

- **纯函数**：无共享状态，所有函数线程安全
- **无 ctx**：纯 CPU 计算

## 7. 设计模式与亮点

- **SHA-256 非 HMAC**：注释明示飞书算法的特殊性——encryptKey 直接拼进 hash 输入而非 HMAC key
- **ErrBadSignature 独立类型**：HTTP handler 用 `errors.Is` 干净映射 401，不依赖字符串匹配
- **AES-256-CBC + PKCS7**：飞书标准加密方案；key 用 SHA-256 派生（任意长度 encryptKey → 32 字节 key）
- **IV 取密文前 16 字节**：飞书设计，无需额外传 IV
- **防御性校验**：密文长度、块对齐、PKCS7 填充字节均校验，杜绝 padding oracle

## 8. 注意事项

- **仅 webhook 模式使用**：stream 模式下 SDK 自己处理验签解密，本文件函数不被调用
- **encrypt_key 配置**：webhook 模式必填（usecase.go validate 强制）；stream 模式可选
- **PKCS7 填充校验严格**：每个填充字节都校验，防 padding oracle 攻击
- **sortedKeys 未导出**：`var _ = sortedKeys` 占位，仅测试用
- **签名大小写不敏感**：`EqualFold` 比对，兼容飞书端 hex 大小写差异
