# `secretbox.go` 技术实现文档

> 源文件：`internal/pkg/secretbox/secretbox.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/secretbox`

## 1. 概述

本文件实现凭据保险库的 at-rest 加密（HLD-017）：AES-256-GCM，密钥从环境变量 `ONGRID_SECRET_KEY` 经 SHA256 派生。密文 wire 格式为 `"v1:" + base64(nonce || ciphertext+tag)`，版本前缀支持未来方案轮换。镜像 n8n 的安全姿态：加密密钥存于环境（DB 之外），DB dump 单独无法还原明文。

## 2. 包信息

- **包名**：`secretbox`
- **所属模块**：`internal/pkg/`
- **依赖方向**：被凭据 vault biz / data 层调用；仅依赖标准库 `crypto/*`、`encoding/base64`、`os`、`sync`

## 3. 关键类型与接口

```go
const prefix = "v1:"

var (
    keyOnce sync.Once
    keyVal  [32]byte
    keyWeak bool // true 表示 ONGRID_SECRET_KEY 未设，使用派生 fallback
)
```

无导出类型；导出函数 `Encrypt` / `Decrypt` / `KeyIsWeak` 是公共 API。

## 4. 关键函数与流程

### `loadKey`
- **签名**：`func loadKey()`（私有）
- **职责**：单次从 `ONGRID_SECRET_KEY` 派生 32 字节 AES 密钥
- **流程**：`sync.Once.Do`：
  - trim env 值；空则 `keyWeak = true` 并用固定 fallback 字符串 `"ongrid-insecure-default-secret-key-set-ONGRID_SECRET_KEY"`
  - `keyVal = sha256.Sum256([]byte(env))`
- **错误处理**：无错误返回；fallback 时通过 `KeyIsWeak` 暴露弱密钥状态

### `KeyIsWeak`
- **签名**：`func KeyIsWeak() bool`
- **职责**：报告密钥是否为不安全的内置 fallback；main.go 据此 Warn
- **流程**：调用 `loadKey()`（幂等）后返回 `keyWeak`

### `Encrypt`
- **签名**：`func Encrypt(plaintext string) (string, error)`
- **职责**：AES-256-GCM 加密；空输入返回空字符串
- **流程**：
  1. 空明文 → 返回 `"", nil`（保持 absent 字段不加密为噪声）
  2. `loadKey()`
  3. `aes.NewCipher(keyVal[:])` → `cipher.NewGCM(block)`
  4. `io.ReadFull(rand.Reader, nonce)` 生成 nonce（GCM 标准长度 12 字节）
  5. `gcm.Seal(nonce, nonce, plaintext, nil)` — nonce 前置
  6. 返回 `"v1:" + base64.StdEncoding.EncodeToString(sealed)`
- **错误处理**：cipher 构造或 nonce 读取失败返回 error（实际不会发生：key 固定 32 字节，rand 不会失败）

### `Decrypt`
- **签名**：`func Decrypt(enc string) (string, error)`
- **职责**：反向解密；空输入返回空；无版本前缀视为 legacy 明文直通
- **流程**：
  1. 空密文 → 返回 `"", nil`
  2. 无 `v1:` 前缀 → 视为 legacy plaintext，原样返回（前向兼容）
  3. `loadKey()`
  4. `base64.StdEncoding.DecodeString` 去 prefix 部分
  5. 构造 AES + GCM
  6. 长度校验 `len(raw) < gcm.NonceSize()` → 报"too short"
  7. 拆分 nonce 与 ct；`gcm.Open(nil, nonce, ct, nil)`
- **错误处理**：
  - base64 失败：`secretbox: base64: %w`
  - 长度不足：`secretbox: ciphertext too short`
  - GCM Open 失败：`secretbox: open (wrong ONGRID_SECRET_KEY?): %w` — 提示密钥可能不匹配

## 5. 依赖关系

- **内部包**：无
- **外部库**：无（仅标准库 crypto）
- **被调用方**：凭据 vault 的 biz / data 层（encrypt before DB write, decrypt after DB read）

## 6. 并发与资源管理

- **`sync.Once`**：保证 `loadKey` 只执行一次；后续调用直接返回。`keyVal` 与 `keyWeak` 在 Once 完成后只读，安全并发读
- **`crypto/rand.Reader`**：OS 级 CSPRNG，可并发安全读
- **`aes.NewCipher` / `cipher.NewGCM`**：每次 Encrypt/Decrypt 新建，无共享状态

## 7. 设计模式与亮点

- **密钥外置**：密钥从环境变量而非 DB 读取，DB dump 单独无法还原明文（n8n 同款姿态）
- **版本前缀 `v1:`**：支持未来方案轮换（如换 chacha20或 Argon2id 派生），通过前缀分发解密路径
- **Legacy 明文直通**：Decrypt 无前缀时原样返回，让"未加密的历史行"在迁移期内仍可读
- **弱密钥自检**：`KeyIsWeak` 让启动路径 Warn 而非 fail，保持 out-of-the-box 可用
- **空输入空输出**：保持 absent 字段不加密为噪声，DB schema 不变
- **GCM nonce 前置**：密文自包含 nonce，无需额外列存储

## 8. 注意事项

- **SHA256 派生不是 KDF**：直接 SHA256(env) 作为密钥，无 salt、无迭代；若 `ONGRID_SECRET_KEY` 是低熵字符串（如短密码），抗暴力破解弱。生产建议用高熵随机字符串
- **fallback 字符串是公开的**：源码可见，未设 env 时安全性为零；`KeyIsWeak` 仅是提示，不阻止运行
- **nonce 长度 12 字节**：GCM 标准；`crypto/rand` 生成，生日碰撞概率可忽略（同密钥下 ~2^48 次才会冲突）
- **无密钥轮换 API**：当前若更换 `ONGRID_SECRET_KEY`，旧密文无法解密；需迁移工具（依次用旧 key Decrypt、新 key Encrypt）。版本前缀为此预留
- **GCM Open 失败的错误信息**：包含"wrong ONGRID_SECRET_KEY?"提示，但未区分"密钥错"与"密文损坏"，调试时需注意
- **`_ = err` 在 Init 中**：`tracing.Init` 类似的占位注释，本文件无此问题
