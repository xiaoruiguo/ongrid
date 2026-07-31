# ssh_identity.go

## 1. 概述

`ssh_identity.go` 实现 knowledge 包的 SSH 身份（deploy key）管理 —— 双轨认证中的"密钥"那一半。每条 `SSHIdentity` 行存储一把私钥 + 该私钥允许认证的目标主机列表（glob 模式），供 `usecase.go` 的 `Sync()` 在 clone 时挑出合适的身份、物料化临时 keyfile、通过 `GIT_SSH_COMMAND` 喂给 git。

文件职责：
- 身份 DTO 与 CRUD（`ListSSHIdentities` / `CreateSSHIdentity` / `UpdateSSHIdentity` / `DeleteSSHIdentity`）
- 服务器端生成 ed25519 keypair（`GenerateSSHIdentity`，P2）
- 公钥指纹派生（`sshFingerprintSHA256`）
- 主机模式匹配（`pickSSHIdentityForHost`，glob via `filepath.Match`）
- SSH URL 解析（`isSSHURL` / `extractSSHHost`）

文件**不**做的事：
- 私钥静态加密（P1 明文存 MySQL，与 `system_settings` 中的 GitHub PAT 同等爆炸半径）
- 在 biz 层做 keygen（仅在 `GenerateSSHIdentity` 这个 P2 入口做；P1 由操作员粘贴已有 PEM）

## 2. 包信息

- 包名：`knowledge`
- 路径：`internal/manager/biz/knowledge`
- 导入路径：`github.com/ongridio/ongrid/internal/manager/biz/knowledge`
- 阶段标注：phase 1（首行注释 `ssh_identity.go — phase 1.`）

## 3. 关键类型与接口

### 输入 DTO

```go
// 粘贴已有 PEM 的创建表单
type CreateSSHIdentityInput struct {
    Name       string
    PrivateKey string   // PEM 编码（BEGIN OPENSSH PRIVATE KEY / BEGIN RSA PRIVATE KEY 等）
    Hosts      []string // 主机 glob 模式列表
    KnownHosts string   // 可选；可后续通过 accept-new 累积
}

// 编辑可变字段（私钥不可变，轮换 = 删除 + 创建）
type UpdateSSHIdentityInput struct {
    Name       string
    Hosts      []string
    KnownHosts string
}

// 服务器端生成 keypair 的创建表单（P2）
type GenerateSSHIdentityInput struct {
    Name       string
    Hosts      []string
    KnownHosts string
}
```

### 模型（来自 `model.SSHIdentity`）

由 `internal/manager/model/knowledge` 定义，本文件只读写它的字段：`Name`、`PrivateKey`、`PublicKey`、`Fingerprint`、`HostsJSON`、`KnownHosts`、`Passphrase`、`CreatedAt`、`UpdatedAt`。

### RepoStore 接口片段（定义在 `usecase.go`，本文件依赖）

```go
ListSSHIdentities(ctx) ([]*model.SSHIdentity, error)
GetSSHIdentity(ctx, id) (*model.SSHIdentity, error)
CreateSSHIdentity(ctx, *model.SSHIdentity) error
UpdateSSHIdentity(ctx, id, name, hostsJSON, knownHosts) error
TouchSSHIdentityUsage(ctx, id) error
DeleteSSHIdentity(ctx, id) error
```

## 4. 关键函数与流程

### CreateSSHIdentity

粘贴 PEM → 验证 → 派生公钥与指纹 → 持久化。

1. 校验 `name`（非空、≤128 字符）和 `private_key`（非空）
2. `ssh.ParsePrivateKey` 解析私钥：
   - 若返回 `*ssh.PassphraseMissingError` → 拒绝（P1 仅支持无密码 key）
   - 其它错误 → 包成 `errs.ErrInvalid`
3. 用 `signer.PublicKey()` 派生公钥，`ssh.MarshalAuthorizedKey` 生成 authorized_keys 行
4. `sshFingerprintSHA256` 生成 `SHA256:<base64-no-padding>` 指纹
5. `normalizeHosts` 归一化（trim、lowercase、去重、保序），至少 1 条
6. `json.Marshal` 编码 hosts → `HostsJSON`
7. 调 `u.repo.CreateSSHIdentity` 持久化
8. 出参前**擦除** `PrivateKey` / `Passphrase`（不让私钥离开 biz 层）

### GenerateSSHIdentity（P2）

服务器端生成 ed25519 keypair → 序列化为 OpenSSH PEM → 持久化。

1. 校验 `name` 和 `hosts`
2. `ed25519.GenerateKey(rand.Reader)` 生成密钥对（显式传 `crypto/rand.Reader`）
3. `ssh.MarshalPrivateKey(priv, "ongrid-deploy")` 序列化为现代 OpenSSH PEM（`BEGIN OPENSSH PRIVATE KEY`）
4. `pem.EncodeToMemory` 编码
5. `ssh.NewPublicKey(pub)` + `ssh.MarshalAuthorizedKey` 生成公钥行，固定注释 `ongrid-deploy`（便于 `ssh-keygen -lf` 识别来源）
6. 派生指纹、入库、出参擦除私钥

设计取舍：选 ed25519 而非 RSA —— 体积小、速度快、现代化。

### pickSSHIdentityForHost

为指定主机挑选匹配身份。匹配顺序：

1. **精确匹配**（`strings.EqualFold`）：遍历所有身份的所有 host 模式
2. **glob 匹配**（`filepath.Match`）：仅对含 `*?[` 的模式尝试，与 `~/.ssh/config` 的 `Host` 语义一致
3. 都不命中 → 返回 `(nil, nil)`，让 git 尝试容器默认 `~/.ssh`（非公开主机几乎必失败，提示操作员添加身份）

匹配是**顺序稳定**的：`ListSSHIdentities` 按 name 排序，因此两个 glob 都能匹配同一主机的身份，对该主机永远解析到同一个。

### sshFingerprintSHA256

```go
func sshFingerprintSHA256(pub ssh.PublicKey) string {
    sum := sha256.Sum256(pub.Marshal())
    return "SHA256:" + strings.TrimRight(base64.StdEncoding.EncodeToString(sum[:]), "=")
}
```

输出格式与 `ssh-keygen -lf <key>` 完全一致，UI 上展示以便操作员核对。

### normalizeHosts / parseHosts

- `normalizeHosts`：trim、lowercase、去重、保序、丢空串
- `parseHosts`：JSON 反解；**对坏行容忍**（返回空列表），让"一条坏身份不匹配任何主机"而不是"拖垮整个 sync"

### extractSSHHost / isSSHURL

```go
// ssh://user@host[:port]/path  或  user@host:path (scp-style)
func extractSSHHost(repoURL string) string
func isSSHURL(repoURL string) bool
```

`isSSHURL` 对 scp-style 要求**同时**有 `@` 和 `:`（且 `@` 后的 `:` 之后才是 path），避免把 `user@email.com` 误判成 SSH URL。

## 5. 依赖关系

### 外部包

- `golang.org/x/crypto/ssh` —— 私钥解析、公钥序列化、keypair 生成序列化
- `crypto/ed25519` + `crypto/rand` —— keypair 生成
- `crypto/sha256` + `encoding/base64` —— 指纹派生
- `encoding/pem` —— PEM 序列化
- `encoding/json` —— hosts 编解码
- `path/filepath` —— `filepath.Match` 做 glob 匹配（与 `~/.ssh/config` 同语义）

### 内部包

- `model "github.com/ongridio/ongrid/internal/manager/model/knowledge"` —— `SSHIdentity` 模型
- `github.com/ongridio/ongrid/internal/pkg/errs` —— 错误哨兵（`ErrInvalid`）

### 被谁调用

- `usecase.go` 的 `Sync` / `buildSSHEnv` 在 clone 前调 `pickSSHIdentityForHost` 选身份
- HTTP handler 调 `ListSSHIdentities` / `CreateSSHIdentity` / `UpdateSSHIdentity` / `DeleteSSHIdentity` / `GenerateSSHIdentity` 暴露 CRUD API

## 6. 并发与资源管理

- 本文件**无锁、无 goroutine**：所有方法都同步、无状态变更（除 DB 写）
- 并发安全由 `RepoStore` 实现（store 层）保证
- 私钥在内存中以明文 `string` 存在 —— 进程内存 dump 可见；GC 前不会被擦除。这是 Go 字符串不可变带来的固有约束，P1 接受

## 7. 设计模式与亮点

### "私钥不离开 biz 层"边界

所有返回 `*model.SSHIdentity` 的方法在 return 前都强制：

```go
row.PrivateKey = ""
row.Passphrase = ""
```

保证 HTTP 响应、日志、序列化都看不到私钥字节。这是纵深防御 —— 即使 handler 误把整个对象 JSON 化，私钥也已被擦除。

### 容错降级

`parseHosts` 对坏 JSON 行返回空列表而非 error。设计意图：一条坏身份只让自己不匹配，不拖垮整个 sync。配合 `pickSSHIdentityForHost` 的"两遍扫描"，单点故障被隔离。

### 顺序稳定的匹配

`ListSSHIdentities` 按 name 排序，`pickSSHIdentityForHost` 在两遍扫描中都按这个顺序遍历。结果：同一主机永远解析到同一身份，即使有多个 glob 都能匹配。可预测 = 可调试。

### 显式熵源

`ed25519.GenerateKey(rand.Reader)` 显式传 `crypto/rand.Reader` 而非依赖默认值。注释说明意图：让代码审计时熵源路径一目了然。

### 注释作为来源标记

`GenerateSSHIdentity` 生成的公钥行与 PEM 都带固定注释 `ongrid-deploy`。运维 `ssh-keygen -lf` 或翻 `~/.ssh/authorized_keys` 时能立刻看出"这把 key 是 ongrid 管理的"。

## 8. 注意事项

- **明文存储风险**：P1 私钥明文存 MySQL，任何 DB 读权限都能取走。AES 加密涉及全平台密钥管理故事，单独跟踪。操作员应使用**专用 deploy key**（最小权限），不要复用个人 key
- **不支持带密码的私钥**：`ssh.ParsePrivateKey` 遇到 `*ssh.PassphraseMissingError` 直接拒绝，错误消息引导操作员生成无密码 ed25519 deploy key
- **私钥不可变**：`UpdateSSHIdentity` 只能改 name / hosts / known_hosts；轮换 = 删除 + 创建
- **glob 语义**：`filepath.Match` 与 `~/.ssh/config` 的 `Host` 指令同语义 —— `*` 匹配任意序列、`?` 匹配单字符、`[abc]` 字符集。但 filepath.Match 在 Windows 上用路径分隔符语义，这里因为 host 都是 lowercase 后匹配且不含 `\`，实际无影响
- **scp-style URL 识别**：`isSSHURL` 要求 `@` 后必须有 `:`，否则把 `user@email.com` 误判为 SSH URL
- **HostsJSON 容错**：`parseHosts` 对坏行返回空列表而非 error —— 调试时若某身份始终不匹配，先查 `HostsJSON` 是否被外部写坏
- **TouchSSHIdentityUsage 未在本文件调用**：接口声明在 `RepoStore`，但本文件不调；应由 `usecase.go` 的 sync 路径在选中身份后调用以记录最后使用时间
