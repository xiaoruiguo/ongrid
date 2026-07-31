# `zhipuauth.go` 技术实现文档

> 源文件：`internal/pkg/zhipuauth/zhipuauth.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/zhipuauth`

## 1. 概述

本文件实现 open.bigmodel.cn（智谱）要求的 JWT 签名 Bearer 认证。智谱 API key 形如 `<id>.<secret>`，部分端点接受原始 key 作为 Bearer，但 v4 `/api/paas/v4/*` 系列拒绝原始 key（401 "令牌已过期或验证不正确"），要求用 secret 半部分签名的 JWT。本包提供 `SignJWT` 生成符合智谱约定的 JWT，以及 `LooksLikeZhipuKey` / `LooksLikeZhipuURL` 两个探测函数让调用方决定何时启用 JWT 路径。

## 2. 包信息

- **包名**：`zhipuauth`
- **所属模块**：`internal/pkg/`
- **依赖方向**：被 `internal/pkg/llm` 的 `zhipuJWTTransport` 调用；仅依赖标准库 `crypto/hmac`、`crypto/sha256`、`encoding/base64`、`encoding/json`、`strings`、`time`

## 3. 关键类型与接口

无导出类型；导出函数 `LooksLikeZhipuKey` / `LooksLikeZhipuURL` / `SignJWT` 是公共 API。

## 4. 关键函数与流程

### `LooksLikeZhipuKey`
- **签名**：`func LooksLikeZhipuKey(key string) bool`
- **职责**：探测 key 是否为 `<id>.<secret>` 形式
- **流程**：`strings.IndexByte(key, '.')`；返回 `idx > 0 && idx < len(key)-1`（前后都有内容）
- **错误处理**：无错误返回

### `LooksLikeZhipuURL`
- **签名**：`func LooksLikeZhipuURL(base string) bool`
- **职责**：探测 base 是否为智谱端点（含 "bigmodel"）
- **流程**：`strings.Contains(strings.ToLower(base), "bigmodel")` — 同时捕获 canonical `open.bigmodel.cn` 与转发代理

### `SignJWT`
- **签名**：`func SignJWT(key string, ttl time.Duration) (string, error)`
- **职责**：生成智谱约定的 JWT
- **流程**：
  1. `strings.IndexByte(key, '.')` 找分隔点；无分隔或边界非法 → 报错 `"zhipuauth: key must be <id>.<secret>"`
  2. 拆分 `id = key[:idx]`、`secret = key[idx+1:]`
  3. `nowMs = time.Now().UnixMilli()`、`expMs = time.Now().Add(ttl).UnixMilli()`
  4. marshal header `{"alg":"HS256","sign_type":"SIGN"}`
  5. marshal payload `{"api_key":id,"exp":expMs,"timestamp":nowMs}`
  6. `headerB64 = base64URLNoPad(header)`、`payloadB64 = base64URLNoPad(payload)`
  7. `signingInput = headerB64 + "." + payloadB64`
  8. `mac = hmac.New(sha256.New, []byte(secret))`，Write signingInput，`sigB64 = base64URLNoPad(mac.Sum(nil))`
  9. 返回 `signingInput + "." + sigB64`
- **错误处理**：key 格式错误返回 error；json marshal 失败返回 error（实际不会发生 — 输入是简单 map）

### `base64URLNoPad`
- **签名**：`func base64URLNoPad(b []byte) string`
- **职责**：JWT 标准的 base64url 无 padding 编码
- **流程**：`base64.URLEncoding.EncodeToString(b)` 后 `strings.TrimRight(..., "=")`

## 5. 依赖关系

- **内部包**：无
- **外部库**：无
- **被调用方**：`internal/pkg/llm/client.go` 的 `zhipuJWTTransport.RoundTrip`（每次请求重新签 JWT）

## 6. 并发与资源管理

无并发控制。所有函数都是无状态纯函数（`SignJWT` 用 `time.Now` 但无共享状态）。`crypto/hmac.New` 每次新建，可并发调用。

## 7. 设计模式与亮点

- **JWT 手写而非引入 jwt 库**：智谱 JWT 形态特殊（`sign_type: SIGN`、`api_key` 而非 `sub`、毫秒时间戳），标准 jwt 库需大量自定义 claims；手写 ~30 行更清晰
- **`<id>.<secret>` 内联解析**：key 自带 id 与 secret，无需额外配置；`SignJWT` 单参数即完成
- **毫秒时间戳**：注释明示"timestamps are milliseconds-since-epoch"，与智谱约定一致（标准 JWT 是秒）
- **`sign_type: SIGN` 自定义 header**：智谱要求的非标准字段，区分 SIGN 与其他签名模式
- **base64url 无 padding**：JWT 标准；`TrimRight "="` 移除 padding
- **探测函数解耦**：`LooksLikeZhipuKey` / `LooksLikeZhipuURL` 让调用方（如 `llm.sdkFor`）按需启用 JWT transport，而非硬编码 host
- **`LooksLikeZhipuURL` 容忍代理**：匹配 "bigmodel" 子串而非精确 host，覆盖转发代理场景

## 8. 注意事项

- **TTL 上限**：注释明示"Zhipu enforces <= 30 days；1h is plenty for a typical batch of API calls"；caller 不应设过长 TTL
- **secret 是 HMAC key**：注释明示"The id half is the API key id (a hex-ish blob) and the secret half is the HMAC key"；caller 不应混淆
- **每次请求重签**：`zhipuJWTTransport` 在 RoundTrip 中调用 `SignJWT(t.apiKey, time.Hour)`，不缓存 token；对高频调用可能浪费，但 1h TTL 内重复签名开销可忽略
- **`LooksLikeZhipuKey` 仅检查 `.` 位置**：不验证 id/secret 内容；任何 `xxx.yyy` 都返回 true，可能误判非智谱 key
- **`LooksLikeZhipuURL` 仅检查 "bigmodel"**：不验证 URL 合法性；可能误判含 "bigmodel" 的非智谱 URL
- **无 token 验证 API**：本包不提供 verify 路径；client 侧只需签名，验证在 server 侧
- **time.Now() 时区无关**：UnixMilli 是 UTC 毫秒，不受本地时区影响；但若系统时钟漂移可能导致 exp 异常
- **hmac 写入忽略 error**：`_, _ = mac.Write(...)` — hmac.Write 永不失败，符合 lint 例外
