# knowledge/ssh_identity.go 技术实现文档

## 1. 概述

`ssh_identity.go` 实现 `/v1/knowledge/ssh-identities` 下的 SSH 身份 CRUD 端点，归属 knowledge 子域。SSH 身份用于"代码仓库"页同步私有 git repo 时的鉴权——保存私钥/known_hosts，前端仅看到公钥+指纹。共 5 个端点：list / create（导入私钥）/ generate（现场生成密钥对）/ update / delete。

## 2. 包信息

- **包名**：`knowledge`（与 `http.go` 同包）
- **导入路径**：`github.com/ongridio/ongrid/internal/manager/server/knowledge`
- **路由前缀**：`/v1/knowledge/ssh-identities`（在 `http.go.Register` 中挂载）
- **文件定位**：HTTP 适配层（薄包装，调 `h.svc.*SSHIdentity`）

## 3. 关键类型与接口

### sshIdentityDTO —— 公共视图

```go
type sshIdentityDTO struct {
    ID          uint64     `json:"id"`
    Name        string     `json:"name"`
    PublicKey   string     `json:"public_key"`
    Fingerprint string     `json:"fingerprint"`
    Hosts       []string   `json:"hosts"`
    KnownHosts  string     `json:"known_hosts,omitempty"`
    LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
    CreatedAt   time.Time  `json:"created_at"`
    UpdatedAt   time.Time  `json:"updated_at"`
}
```

**关键约束**：`PrivateKey` 与 `passphrase` **永不序列化**——即便管理员也只能看到公钥+指纹。所有响应路径只走 `toSSHIdentityDTO`。

### 请求 DTO

```go
type createSSHIdentityReq struct {
    Name       string   `json:"name"`
    PrivateKey string   `json:"private_key"`
    Hosts      []string `json:"hosts"`
    KnownHosts string   `json:"known_hosts,omitempty"`
}

type updateSSHIdentityReq struct {
    Name       string   `json:"name"`
    Hosts      []string `json:"hosts"`
    KnownHosts string   `json:"known_hosts"`
}

type generateSSHIdentityReq struct {
    Name       string   `json:"name"`
    Hosts      []string `json:"hosts"`
    KnownHosts string   `json:"known_hosts,omitempty"`
}
```

**`updateSSHIdentityReq` 不含 `PrivateKey`**：私钥不可改，要换只能新建或重新 generate。

### Handler

复用 `http.go` 中定义的 `Handler`，本文件只挂方法。

## 4. 关键函数与流程

### listSSHIdentities

```go
func (h *Handler) listSSHIdentities(w http.ResponseWriter, r *http.Request)
```

调 `svc.ListSSHIdentities`，转 DTO 数组，返回 `{items, total}`。

### createSSHIdentity —— 导入已有私钥

```go
func (h *Handler) createSSHIdentity(w http.ResponseWriter, r *http.Request)
```

接收用户提供的 `private_key` 字符串，调 `svc.CreateSSHIdentity`，返回 201 + DTO。

### generateSSHIdentity —— 服务端生成密钥对

```go
func (h *Handler) generateSSHIdentity(w http.ResponseWriter, r *http.Request)
```

**核心产品意图**：调 `svc.GenerateSSHIdentity` 现场生成密钥对，返 201 + DTO（含 `public_key`）。前端立即把公钥放进可复制块，操作者直接粘贴到 GitHub/GitLab 的 Deploy Keys，省一次往返。

### updateSSHIdentity / deleteSSHIdentity

按 `id` URL 参数定位，调对应 svc 方法。删除返 `204 No Content`。

### parseUintParam

```go
func parseUintParam(r *http.Request, key string) (uint64, error)
```

`chi.URLParam` + `strconv.ParseUint`，失败时 `errors.Join(errs.ErrInvalid, err)` 包装成 400。

### 编译期 guard

```go
var _ = context.TODO
```

文件中 helper 不直接用 `context`，但保留 import 给后续编辑留稳定编译面（避免增删函数时 import 抖动）。

## 5. 依赖关系

**外部**：
- `chi` —— URL 参数
- `net/http`、`encoding/json`、`strconv`、`time`

**内部**：
- `internal/manager/biz/knowledge`（biz 输入/输出类型）
- `internal/manager/model/knowledge`（model.SSHIdentity）
- `internal/pkg/errs`（错误哨兵）

## 6. 并发与资源管理

- **无共享可变状态**：本文件所有方法都是 `*Handler` 上的纯函数式 handler，没有自己的字段。
- **请求级隔离**：每个请求独立 ctx、独立 DTO，无 cross-request 状态。
- **无 goroutine 启动**：所有调用同步阻塞，biz 层负责异步任务。

## 7. 设计模式与亮点

1. **私钥永不外泄**：`sshIdentityDTO` 不暴露 `private_key`/`passphrase` 字段，从类型层面保证序列化路径不会泄露。即使误把 model.SSHIdentity 直接 marshal 也不会发生（因为只走 `toSSHIdentityDTO`）。
2. **update 不允许改私钥**：`updateSSHIdentityReq` 没有 `PrivateKey` 字段，强制换密钥只能新建/重新生成——避免"改一半"的不一致状态。
3. **generate 返 201 突出 public_key**：现场生成模式让前端立即拿到公钥粘贴到 git host，产品流程上消除"先生成再查询"的一次往返。
4. **归属 knowledge 路由**：SSH 身份挂在 `/v1/knowledge/ssh-identities` 而非独立 `/v1/ssh-identities`，让"代码仓库"页维持单一 URL 前缀，运维入口不分裂。
5. **`var _ = context.TODO` 编译期 guard**：防御性 import 保留，避免未来增删函数时的 import 抖动。

## 8. 注意事项

1. **私钥安全**：私钥在 biz 层加密存储，本层只接收不返回。若日志/审计需记录 SSH 身份相关事件，禁止打印 `req.PrivateKey`。
2. **`generateSSHIdentity` 的密钥强度**：本文件不约束，由 biz 层决定（通常 ed25519 或 4096-bit RSA）。
3. **`KnownHosts` 明文返回**：known_hosts 不属于机密，但其中可能含内部主机名，跨租户共享前需评估。
4. **无 casbin 细粒度授权**：所有变更走 `http.go` 的 `writeMW("knowledge:repo")` / `deleteMW("knowledge:repo")`，即"能改 repo 就能改 SSH 身份"，未单独细分；如有更细粒度需求需扩 `AuthzMW`。
5. **`Hosts` 用 `[]string` JSON 数组**：model 层存 `HostsJSON` 字符串，DTO 层在 `toSSHIdentityDTO` 中 `json.Unmarshal` 转数组；解析失败时静默返空数组（`_ = json.Unmarshal`），不阻断响应——这是有意的"宽容解码"。
6. **`id` 参数解析**：`parseUintParam` 失败返 `errs.ErrInvalid`（400），不返 404——区分"格式错"和"不存在"。
