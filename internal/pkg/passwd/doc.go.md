# `doc.go` 技术实现文档（passwd）

> 源文件：`internal/pkg/passwd/doc.go`
> 包路径：`github.com/ongridio/ongrid/internal/pkg/passwd`

## 1. 概述

该文件是 `passwd` 包的文档声明文件，无任何代码实现，仅以 Go 包注释说明包的定位与历史：提供共享的 argon2id `Hash` / `Verify` 工具，服务任何 BC 的"明文 → PHC 编码哈希"需求。包最初位于 `internal/iam/biz/user/hash.go`，因 manager BC 也需相同方案 hash edge SecretKey 且 arch-lint 禁止 manager → iam 导入，被提升到共享 `internal/pkg/passwd`。

## 2. 包信息

- **包名**：`passwd`
- **所属模块**：`internal/pkg/`（基础设施层，无业务依赖）
- 依赖方向：纯文档，无依赖。

## 3. 关键类型与接口

无显著类型定义（文件仅含包注释，无任何声明）。

## 4. 关键函数与流程

无关键函数。文档明确了：
- **PHC 编码格式**：`$argon2id$v=19$m=65536,t=1,p=4$<salt-b64>$<hash-b64>`
- **包的历史**：从 `internal/iam/biz/user/hash.go` 提升到 `internal/pkg/passwd`。
- **提升原因**：manager BC 需相同方案 hash edge SecretKey；arch-lint 禁止 manager → iam 导入。
- **兼容性**：iam wrapper 保留旧 unexported 名以避免 iam 测试大面积重构。

## 5. 依赖关系

- **内部包**：无。
- **外部库**：无。
- **被调用方**：无（仅作为包文档被 `go doc` / IDE 读取）。

## 6. 并发与资源管理

无并发控制（无代码）。

## 7. 设计模式与亮点

- **共享工具提升**：把多 BC 共用的 hash 逻辑从某一 BC 提升到 `internal/pkg/`，符合 gospec "utils/、lerrors/ 不依赖任何业务包" 红线与 BC 隔离原则。
- **arch-lint 强制约束**：注释提到 arch-lint 禁止 manager → iam 导入，这是 monorepo 多 BC 防耦合的具体机制。
- **wrapper 兼容**：iam 侧保留旧 unexported 名（如 `hashPassword` / `verifyPassword`）作为 `passwd.Hash` / `passwd.Verify` 的 thin wrapper，避免大面积重构既有 iam 测试，平滑迁移。
- **PHC 格式标准化**：采用业界标准 PHC 字符串格式，跨库兼容，未来切换 hash 库可保持现有哈希可用。
- **argon2id 选择留痕**：注释明确格式，便于运维识别哈希类型与参数。

## 8. 注意事项

- **文档与实现可能漂移**：注释中 PHC 格式需与 `argon2.go` 的 `Hash` 输出同步；参数变更需同步更新。
- **iam wrapper 路径未明示**：注释提到 iam wrapper 保留旧名但未指明具体文件路径；新开发者需自行搜索 iam BC。
- **不暴露其他 hash 算法**：包仅提供 argon2id，不提供 bcrypt / scrypt / PBKDF2；若未来需支持多算法（如验证遗留 bcrypt 哈希）需扩展。
- **PHC 格式严格**：`Verify` 仅识别 ongrid 产生的 argon2id PHC；从其他系统迁移的密码哈希需先重新 hash。
- **无密码强度校验**：包仅做 hash/verify，不做密码强度评估；上层应在 `Hash` 前校验长度 / 复杂度。
- **argon2id 参数固定**：参数在 `argon2.go` 常量中写死，不可通过 env 调整；注释提到"adjust via env later"，是未来扩展点。
