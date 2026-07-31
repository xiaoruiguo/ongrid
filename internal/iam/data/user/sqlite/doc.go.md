# `doc.go` 技术实现文档

> 源文件：`internal/iam/data/user/sqlite/doc.go`
> 包路径：`github.com/ongridio/ongrid/internal/iam/data/user/sqlite`

## 1. 概述

本文件是 IAM BC 用户 sqlite 持久化子包的包级文档文件，仅含 package 注释。声明本包使用 GORM/SQLite 实现 `internal/iam/biz/user.Repo` 接口，是 post-pivot 后 IAM 唯一的持久化后端。

## 2. 包信息

- **包名**：`sqlite`
- **所属模块**：`internal/iam/data/user/sqlite` —— data 层用户持久化（GORM/SQLite）
- **依赖方向**：实现 `internal/iam/biz/user.Repo`；被 `cmd/ongrid` 装配

## 3. 关键类型与接口

无显著类型定义。

## 4. 关键函数与流程

无函数定义。

## 5. 依赖关系

- **内部包**：无 import
- **外部库**：无
- **被调用方**：包级文档，不被调用

## 6. 并发与资源管理

无并发控制。

## 7. 设计模式与亮点

- **后端单一化声明**：注释明确「Post-pivot this is the only persistence backend for iam」，避免维护者误以为存在多后端切换。
- **GORM + SQLite 组合**：MVP 阶段选择轻量嵌入式方案，降低部署复杂度。

## 8. 注意事项

- 包名虽为 `sqlite`，但 GORM 实际方言无关；若未来引入 MySQL 后端，本包内代码大部分可直接复用，仅需调整连接配置。
- 注释描述的「only backend」是 post-pivot 现状，未来若多后端需拆分。
