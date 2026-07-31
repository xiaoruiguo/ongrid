# `argon2.go` 技术实现文档（passwd）

> 源文件：`internal/pkg/passwd/argon2.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/passwd`

## 1. 概述

该文件实现 PHC 编码格式的 argon2id 哈希与校验，是 ongrid 任何"明文密码 → 哈希"场景的共享工具。参数针对 2023 M 系列 Mac 约 60ms/哈希调优；参数变更不使旧哈希失效——编码形式携带产生它的参数。`Verify` 用 `subtle.ConstantTimeCompare` 保证不通过时序侧信道泄露失败位置。

## 2. 包信息

- **包名**：`passwd`
- **所属模块**：`internal/pkg/`（基础设施层，无业务依赖）
- 依赖方向：被 iam BC（user password）与 manager BC（edge secret key）调用；依赖 `golang.org/x/crypto/argon2` + 标准库。

## 3. 关键类型与接口

无显著类型定义。仅有包级常量与两个顶层函数。

### 参数常量

```go
const (
    argonTime    uint32 = 1
    argonMemory  uint32 = 64 * 1024 // 64 MiB
    argonThreads uint8  = 4
    argonSaltLen uint32 = 16
    argonKeyLen  uint32 = 32
)
```

注释说明：参数针对笔记本合理；可通过 env 后续调整；变更参数不失效旧哈希（编码携带参数）。

## 4. 关键函数与流程

### `Hash`
- **签名**：`func Hash(plain string) (string, error)`
- **职责**：返回 PHC 编码的 argon2id 哈希。
- **格式**：`$argon2id$v=19$m=65536,t=1,p=4$<salt-b64>$<hash-b64>`
- **流程**：
  1. `plain == ""` → error `empty plaintext`。
  2. `crypto/rand.Read(salt)` 生成 16 字节 salt；失败 → error `read random salt`。
  3. `argon2.IDKey(plain, salt, time=1, memory=64MiB, threads=4, keyLen=32)` 计算哈希。
  4. `fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, ...)` 拼接 PHC 字符串，salt 与 hash 用 `base64.RawStdEncoding`（无 padding）。
- **错误处理**：rand 失败用 `%w` 包装。

### `Verify`
- **签名**：`func Verify(plain, encoded string) bool`
- **职责**：判断 plain 是否匹配 PHC 编码哈希。
- **流程**：
  1. `strings.Split(encoded, "$")` 切分；预期 6 段 `["", "argon2id", "v=19", "m=...,t=...,p=...", "<salt>", "<hash>"]`。
  2. `len != 6 || parts[1] != "argon2id"` → false。
  3. `Sscanf(parts[2], "v=%d", &version)` 失败 → false。
  4. `version != argon2.Version` → false。
  5. `Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &t, &p)` 失败 → false。
  6. `base64.RawStdEncoding.DecodeString(parts[4])` 解 salt；失败 → false。
  7. `base64.RawStdEncoding.DecodeString(parts[5])` 解 want hash；失败 → false。
  8. `argon2.IDKey(plain, salt, t, mem, p, len(want))` 重算 got。
  9. `subtle.ConstantTimeCompare(got, want) == 1` 返回。
- **设计理由**：任何解码 / 不匹配失败统一返回 false，**不通过时序侧信道泄露失败位置**——这是密码验证的关键安全要求。

## 5. 依赖关系

- **内部包**：无。
- **外部库**：`golang.org/x/crypto/argon2`；标准库 `crypto/rand` / `crypto/subtle` / `encoding/base64` / `errors` / `fmt` / `strings`。
- **被调用方**：iam BC（user password hash/verify）、manager BC（edge secret key hash/verify）。

## 6. 并发与资源管理

无并发控制。`Hash` 与 `Verify` 是纯函数，无共享状态。`argon2.IDKey` 自身 CPU-bound 内存密集（64 MiB），并发调用受内存约束——高并发场景需评估。

## 7. 设计模式与亮点

- **PHC 编码自描述**：哈希字符串携带 algorithm / version / params / salt / hash，未来参数升级后旧哈希仍用旧参数验证，新哈希用新参数产生，平滑过渡。
- **`ConstantTimeCompare` 防时序侧信道**：`Verify` 全程任何失败返回 false，最终比较用常量时间，避免攻击者通过响应时间推断失败位置。
- **RawStdEncoding 无 padding**：base64 不带 `=` padding，PHC 字符串更紧凑。
- **`crypto/rand` 强随机源**：salt 用 crypto/rand 而非 math/rand，保证不可预测。
- **argon2id 而非 argon2i / argon2d**：id 模式兼顾 side-channel 抗性（i）与 GPU 抗性（d），是 OWASP 推荐选择。
- **参数注释留痕**：参数选择依据与变更影响明确记录，便于未来调优决策。
- **空 plaintext 拒绝**：`Hash` 显式拒绝空密码，避免弱密码哈希入库。

## 8. 注意事项

- **64 MiB 内存占用**：每次 `Hash` / `Verify` 占用 64 MiB（argonMemory），高并发登录场景需评估内存压力。
- **60ms/哈希**：单次约 60ms，登录延迟可接受；批量导入用户场景需评估总耗时。
- **参数不可降级**：变更 argonMemory / argonTime 后，旧哈希仍用旧参数验证（PHC 携带），但新哈希用新参数；无法强制升级旧哈希。
- **salt 长度 16 字节**：符合 RFC 9106 推荐；变更需同步 `argonSaltLen` 常量。
- **`Verify` 不区分错误**：所有失败返回 false，调试困难；这是安全权衡，调用方不应依赖错误细节。
- **不限制密码长度**：超长密码会拖慢 argon2 计算，DoS 风险；上层应在 `Hash` 前限制长度。
- **PHC 解析严格**：`strings.Split` 期望严格 6 段；非 ongrid 产生的 argon2id 哈希（如其他库的 `$argon2id$v=19$m=...` 但含额外字段）会被拒绝。
- **版本检查严格**：`version != argon2.Version` 直接 false；若 argon2 库升级版本号，旧哈希会全部失效——需评估兼容性。
