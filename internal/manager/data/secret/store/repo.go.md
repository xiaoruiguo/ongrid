# secret/store/repo.go

## 1. 概述

本文件实现凭证保险库（`secrets` 表）的 GORM-backed 持久化。**Repo 是「哑存储」**——它**永远看不到明文**：到达 data 层时 `Data` 字段已经是 sealed blob，加解密在 `biz/secret` 层完成。覆盖能力：创建、按字段更新、删除、列表、按 name 取（注入路径用）。

设计要点：
- **明文隔离**：包注释明确「biz/secret does encrypt/decrypt」；`List` 返回的 `Data` 是 sealed blob，biz 层在 API 响应前 redact。
- **name 唯一约束**：`Create` 把唯一索引冲突翻译为 `errs.ErrConflict`，让调用方知道重名。
- **空 data 跳过更新**：`Update` 的 `data == ""` 时不写 `value` 列，支持「只改描述」场景。
- **跨方言 dup 检测**：`isDup` 同时用 `errors.Is(gorm.ErrDuplicatedKey)` 与字符串匹配，覆盖 MySQL/SQLite/Postgres。

## 2. 包信息

- **包名**：`store`（包注释明确「GORM-backed persistence for the credential vault」）
- **所属模块**：`internal/manager/data/secret/store`
- **依赖方向**：`controlplane → biz/secret → data/secret/store → model/secret + pkg/errs`；接口在消费方 biz 层定义（编译期断言未在本文件显式写出，但 Repo 由 wire 注入 biz）。

## 3. 关键类型与接口

```go
// Repo 是 GORM-backed 凭证存储；并发安全。
type Repo struct{ db *gorm.DB }

func NewRepo(db *gorm.DB) *Repo

// 跨方言 duplicate key 检测（局部 helper）
func isDup(err error) bool
```

`model.Secret` 结构体字段（隐含）：`id uint64`、`name string`、`value string`（sealed blob）、`description string`、`created_at`、`updated_at`。

## 4. 关键函数与流程

### `NewRepo(db *gorm.DB) *Repo`

构造器，直接包装 `*gorm.DB`。

### `Create(ctx, s *model.Secret) error`

- **职责**：插入新凭证；name 必须唯一。
- **流程**：
  1. `s == nil || strings.TrimSpace(s.Name) == ""` → `errs.ErrInvalid` 包装「name required」。
  2. `r.db.WithContext(ctx).Create(s)`。
  3. 错误检测：`isDup(err)` → `errs.ErrConflict` 包装「credential %q already exists」；否则透传。
- **设计意图**：name 是凭证的业务标识（如 `db-prod-password`），重名会让注入路径歧义，故用唯一索引 + 冲突翻译。
- **安全约束**：`s.Value` 必须是 sealed blob，data 层不校验也不解密。

### `Update(ctx, id uint64, data, description string) error`

- **职责**：按 id 更新 `value`（sealed blob）和/或 `description`。
- **流程**：
  1. 构造 `fields map[string]any{"description": description}`。
  2. `data != ""` → `fields["value"] = data`（空 data 跳过，支持「只改描述」）。
  3. `Model(&Secret{}).Where("id = ?", id).Updates(fields)`。
  4. `RowsAffected == 0` → `errs.ErrNotFound`。
- **设计权衡**：用 `map[string]any` + `Updates` 而非 `Save`，避免覆盖 `name` / `created_at` 等不可变字段。
- **注意**：`description` 即使为空也会被写入（map 形式会写零值），调用方若想「只改 value 不动 description」需先 Get 原描述。

### `Delete(ctx, id uint64) error`

- **职责**：物理删除凭证。
- **流程**：`Where("id = ?", id).Delete(&model.Secret{})`。
- **错误处理**：
  - `res.Error` 透传。
  - `RowsAffected == 0` → `errs.ErrNotFound`。
- **注意**：物理删除；如需审计追溯需在 biz 层先归档 sealed blob。

### `List(ctx) ([]*model.Secret, error)`

- **职责**：返回所有凭证，按 name 升序。
- **流程**：`Order("name ASC").Find(&out)`。
- **安全语义**：返回的 `Data` 是 sealed blob；**biz 层在 API 响应前必须 redact**（注释明确）。
- **不分页**：凭证数量通常很少（几十到几百），全量返回可接受。

### `GetByName(ctx, name string) (*model.Secret, error)`

- **职责**：按 name 取单行（注入路径用）。
- **流程**：`Where("name = ?", name).First(&s)`；`gorm.ErrRecordNotFound` → `errs.ErrNotFound`。
- **使用场景**：biz 层在注入凭证到下游服务时，按绑定 name 取 sealed blob，然后解密传给下游。
- **安全约束**：返回的 `Data` 是 sealed blob，data 层不解密。

### `isDup(err error) bool`

- **职责**：跨方言检测唯一索引冲突。
- **实现**：
  1. `errors.Is(err, gorm.ErrDuplicatedKey)`——GORM 1.25+ 的标准 sentinel（方言无关）。
  2. fallback 字符串匹配：`"UNIQUE"` / `"Duplicate"` / `"duplicate"`——覆盖旧版 GORM 或 SQLite 的原始错误。
- **设计权衡**：先用类型化检测，再用字符串兜底，兼顾稳健性与兼容性。

## 5. 依赖关系

- **内部包**：
  - `github.com/ongridio/ongrid/internal/manager/model/secret`——`Secret` 结构体。
  - `github.com/ongridio/ongrid/internal/pkg/errs`——`ErrInvalid`、`ErrConflict`、`ErrNotFound` 标准错误。
- **外部库**：`gorm.io/gorm`（含 `ErrDuplicatedKey`、`ErrRecordNotFound` sentinel）。
- **标准库**：`context`、`errors`、`fmt`、`strings`。
- **被调用方**：`biz/secret` 的凭证管理 usecase；注入路径（如 edge plugin config 注入凭证）也会调 `GetByName`。

## 6. 并发与资源管理

- **无共享状态**：`Repo` 仅持有不可变 `*gorm.DB`，可并发调用。
- **每次新 session**：`r.db.WithContext(ctx)` 隔离事务状态。
- **ctx 透传**：所有方法首参 `context.Context`，符合 gospec 红线。
- **无锁、无 channel、无缓存**。
- **无资源释放**：GORM 连接池管理。
- **并发 Create 同名**：依赖唯一索引兜底，第二个 Create 会收到 `ErrConflict`。

## 7. 设计模式与亮点

- **明文隔离边界**：包注释 + 方法注释反复强调「data 层不接触明文」，是安全设计的核心边界；任何加解密逻辑都应在 biz 层。
- **name 业务标识 + 唯一索引**：name 作为凭证的业务标识（注入路径按 name 取），唯一索引保证注入无歧义。
- **空 data 跳过更新**：`Update` 的 `data == ""` 跳过 `value` 列，支持「只改描述」场景，避免误清空 sealed blob。
- **map 形式 Updates**：避免 `Save` 覆盖不可变字段（name、created_at），是更新部分列的安全模式。
- **跨方言 dup 检测双保险**：`errors.Is(gorm.ErrDuplicatedKey)` + 字符串匹配，兼顾新旧 GORM 版本与多方言。
- **三态删除**：`Delete` 用 `RowsAffected == 0` → `ErrNotFound`，与项目其他 repo 一致。
- **List 安全语义注释**：明确「biz redacts before any API response」，防止后续开发者误把 sealed blob 当明文返回。

## 8. 注意事项

- **明文绝不进 data 层**：任何在 data 层加解密的代码都是安全漏洞；如需在 data 层做密钥轮换，应改为 biz 层读 sealed → 解密 → 重新加密 → 调 Update。
- **`List` 返回 sealed blob**：biz 层必须 redact 后再返回 API；前端永远不应看到 sealed blob。
- **`description` 空字符串会被写入**：`Update` 用 map 形式 Updates，`description=""` 会清空描述列；若想「只改 value 不动 description」需先 Get 原描述再传。
- **物理删除**：`Delete` 是硬删除；如需审计追溯需在 biz 层先归档 sealed blob 到审计表。
- **`isDup` 字符串匹配**：依赖数据库错误消息文本，跨版本/跨语言环境可能失效；优先依赖 `gorm.ErrDuplicatedKey`，字符串匹配是 fallback。
- **name 大小写敏感**：`GetByName` 用 `WHERE name = ?`，MySQL 默认大小写不敏感（取决于 collation），SQLite 大小写敏感；如需统一行为应在 model 层加 `LOWER(name)` 索引或 biz 层 normalize。
- **不分页**：`List` 全量返回；凭证数量增长后需加分页（目前可接受）。
- **密钥轮换**：本 repo 不支持批量 re-encrypt；轮换主密钥需 biz 层遍历所有凭证解密重加密。
- **审计**：data 层不记录凭证访问日志；如需审计「谁取了哪个凭证」应在 biz 层打审计日志。
- **跨方言**：所有查询方言无关；`isDup` 覆盖 MySQL/SQLite/Postgres。
