# `secrets.go` 技术实现文档

> 源文件：`tests/e2e/testenv/secrets.go`
> 包路径：`github.com/ongridio/ongrid/tests/e2e/testenv`

## 1. 概述

本文件是 e2e 测试环境的 secret 管理模块，负责从 `os.Getenv` 优先、`tests/e2e/secrets.local.env`（gitignored）次之加载真实外部服务凭据，并提供 `RequireSecret`（缺失则 `t.Skip` 而非失败）、`LiveMode`（E2E_LIVE_X 套件级开关）、`RedactSecret`（日志脱敏）三个核心 helper。红线：(1) 真实 token 永不进 repo —— `secrets.local.env` 被 gitignore；(2) 缺失 secret 永远 `t.Skip` 而非 t.Fatal —— 默认 mock-only 运行不失败；(3) dotenv 按进程读一次缓存，`Reload` 供工具类测试手动刷新。

## 2. 包信息

- **包名**：`testenv`（与 env.go 同包，但 `//go:build e2e` 标签）
- **所属模块**：`tests/e2e/testenv`
- **依赖方向**：被 e2e 测试用例调用（RequireSecret / LiveMode / RedactSecret）；依赖标准库 `os` / `bufio` / `path/filepath` / `runtime` / `strings` / `sync` / `testing`

## 3. 关键类型与接口

```go
const secretsFileName = "secrets.local.env"

var (
    mu     sync.Mutex
    loaded bool
    dotenv map[string]string
)
```

无导出类型；仅导出函数 `LookupSecret` / `RequireSecret` / `LiveMode` / `RedactSecret` / `Reload`。

## 4. 关键函数与流程

### `LookupSecret(name) (string, bool)`

- **职责**：非跳过变体的 secret 查询。
- **流程**：
  1. `os.Getenv(name)` TrimSpace 非空 → 返回 (value, true)。
  2. 加锁；若 !loaded → `dotenv = loadDotenv()` + loaded=true。
  3. 查 dotenv[name]；mu.Unlock。
  4. TrimSpace；空 → ("", false)；否则返回 (value, true)。
- **错误处理**：无错误返回；缺失即 (value="", ok=false)。

### `RequireSecret(t, name) string`

- **职责**：缺失则 `t.Skip` 的 secret 查询。
- **流程**：
  1. `t.Helper()`。
  2. `LookupSecret(name)`；!ok → `t.Skipf("SKIP: %s — needs %s (set in env or %s; see secrets.example.env for the template)", t.Name(), name, displaySecretsPath())` + 返回 ""。
  3. 否则返回 value。
- **错误处理**：缺失不失败，仅 Skip + 统一提示消息（指向 secrets.example.env 模板）。

### `LiveMode(label) bool`

- **职责**：套件级 live 开关查询。
- **流程**：
  1. `isTruthy(getRaw("E2E_LIVE_ALL"))` → true。
  2. 否则 `isTruthy(getRaw("E2E_LIVE_" + strings.ToUpper(label)))`。
- **使用方式**：注释示例 `if !testenv.LiveMode("SLACK") { t.Skip(...) }` —— LiveMode 自身不 Skip，caller 决定。
- **与 RequireSecret 配合**：LiveMode=true 但 token 缺失时，RequireSecret 会 Skip（注释明示"I asked for live but the token is missing"路径）。

### `RedactSecret(s) string`

- **职责**：日志脱敏。
- **流程**：TrimSpace；len<=8 → "***"；否则 `s[:6] + "…"`（前 6 字符 + 省略号）。

### `Reload()`

- **职责**：丢弃 dotenv 缓存，下次 LookupSecret 重新读文件。
- **流程**：加锁；loaded=false；dotenv=nil。
- **使用场景**：注释明示"rarely need this; exists so 'mutate the file inside a test then re-read' works for tooling tests"。

### 内部辅助

- **`getRaw(name)`**：`v, _ := LookupSecret(name); return v`。
- **`isTruthy(v)`**：lowercase trim 后 `1/true/yes/y/on` → true。
- **`loadDotenv()`**：
  1. `secretsFilePath()` 取路径（本文件向上找 `tests/e2e/secrets.local.env`，或 cwd fallback）。
  2. 空路径 → 返回空 map。
  3. `os.Open` 失败 → 返回空 map（容忍缺失文件）。
  4. bufio.Scanner 逐行：跳空行 / `#` 注释；找 `=`；split key/value；TrimSpace；剥离首尾配对引号（dotenv 风格）；写入 map。
- **`secretsFilePath()`**：
  1. `runtime.Caller(0)` 取本文件源码路径。
  2. `filepath.Dir(filepath.Dir(src))` 得 `tests/e2e/`；Join secretsFileName。
  3. `os.Stat` 存在 → 返回。
  4. Fallback：cwd + `tests/e2e/secrets.local.env`。
  5. 都无 → 返回 ""。
- **`displaySecretsPath()`**：
  1. secretsFilePath()；空 → `"tests/e2e/" + secretsFileName`。
  2. 否则 trim 到 `tests/e2e/` 前缀（让消息读"tests/e2e/secrets.local.env"而非绝对路径）。

## 5. 依赖关系

- **内部包**：无
- **外部库**：标准库（`os` / `bufio` / `path/filepath` / `runtime` / `strings` / `sync` / `testing`）
- **被调用方**：所有需要真实外部服务凭据的 e2e 测试用例

## 6. 并发与资源管理

- **`mu`（sync.Mutex）**：保护 `loaded` + `dotenv` 缓存。
- **dotenv 按进程读一次**：首次 LookupSecret 触发 loadDotenv；后续命中缓存。
- **Reload 丢弃缓存**：测试 mutate 文件后可重新读。
- **`os.Getenv` 无锁**：Go runtime 自身线程安全。

## 7. 设计模式与亮点

- **三态查询分离**：`LookupSecret`（不 Skip）/ `RequireSecret`（Skip）/ `LiveMode`（开关查询）—— 让测试按场景选择。
- **环境变量优先于 dotenv**：`os.Getenv` 先查，让 CI 显式注入覆盖文件值；dotenv 是本地开发便利。
- **dotenv 路径自动发现**：从源码位置向上找 `tests/e2e/`，让 `go test` 从任意 cwd 运行都能找到；cwd fallback 兜底。
- **缺失即 Skip 而非 Fatal**：默认 mock-only 运行不失败；live-mode 测试按需启用。
- **统一 Skip 消息**：`RequireSecret` 的 t.Skipf 含 t.Name() + secret 名 + 文件路径 + 模板提示，便于开发者快速定位。
- **dotenv 解析容忍**：缺失文件、格式错误行都返回空 map / 跳过；不 fail。
- **剥离配对引号**：支持 `KEY="value"` 和 `KEY='value'` 两种 dotenv 风格。
- **RedactSecret 短值全屏蔽**：len<=8 全 `***`；防短 token 前缀被泄露。
- **LiveMode 不自身 Skip**：caller 决定 Skip 行为，支持"部分 live"运行（mocks + 一个真实服务）。
- **displaySecretsPath 剪裁绝对路径**：让消息读 `tests/e2e/secrets.local.env` 而非 `/abs/path/...`，跨开发环境一致。

## 8. 注意事项

- **`//go:build e2e` 标签**：与 env.go / fakes.go / http.go 同包；仅 e2e 构建包含。
- **dotenv 文件 gitignored**：`secrets.local.env` 不进 repo；`secrets.example.env` 是模板（注释提及）。
- **dotenv 解析不支持多行值**：每行一个 KEY=VALUE；不支持 `\` 续行或 `"""..."""`。
- **`Reload` 仅清缓存不重读**：下次 LookupSecret 才触发 loadDotenv。
- **`isTruthy` 接受 1/true/yes/y/on**：大小写不敏感；其他值视为 false。
- **`RedactSecret` 前 6 字符**：对长 token 暴露前 6 字符用于识别（如 `sk-abc…`）；短值全屏蔽。
- **`secretsFilePath` 向上找 2 层**：`filepath.Dir(filepath.Dir(src))` 从 `testenv/secrets.go` 得 `tests/e2e/`；假设本文件位于 `tests/e2e/testenv/`。
- **cwd fallback**：若源码路径无法定位（如二进制被移动），用 cwd + `tests/e2e/` 兜底。
- **`displaySecretsPath` 找不到时返回相对路径字面值**：`"tests/e2e/" + secretsFileName`，让消息仍有意义。
- **`getRaw` 忽略 ok**：`v, _ := LookupSecret(name)` —— LiveMode 场景下 falsy 值即可。
- **线程安全**：`mu` 保护 dotenv；`os.Getenv` 自身安全；但 `Reload` + 并发 `LookupSecret` 可能短暂竞争（Reload 持锁清缓存，LookupSecret 等待）。
- **不支持 secret 轮转**：dotenv 读一次缓存；若运行中轮转需 Reload。
