# `doc.go` 技术实现文档

> 源文件：`internal/manager/data/metric/store/doc.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/data/metric/store`

## 1. 概述

本文件是 `metric/store` 包的 package doc，声明本包是 `manager/metric` 的 MVP Writer/Reader。Writer 用 GORM `CreateInBatches(500)` 写 raw / 5m / 1h 三层 + dead-letter 专用路径；Reader 按 time window 在 `host_metrics_raw` / `_5m` / `_1h` 间选择（由 `biz/metric` QueryUsecase 驱动，reader 本身是 plain DAO）。

## 2. 包信息

- **包名**：`store`
- **所属模块**：`internal/manager/data/metric`
- **依赖方向**：被 `internal/manager/biz/metric` 装配；依赖 `internal/manager/model/metric`、`gorm.io/gorm`。Phase 2 升级后部分流量切至 `metric/clickhouse`。

## 3. 关键类型与接口

无类型定义；仅 package doc 注释。

```go
// Package sqlite is the MVP writer/reader for manager/metric (+
//Writer uses GORM CreateInBatches(500) for raw / 5m / 1h and
// a dedicated path for dead-letter rows. Reader picks between
// host_metrics_raw / _5m / _1h by time window (driven by biz/metric
// QueryUsecase — the reader itself is a plain DAO).
package store
```

## 4. 关键函数与流程

无函数定义。

## 5. 依赖关系

- 仅声明性依赖。
- **被调用方**：通过同包 `writer.go::NewBizWriter` / `reader.go::NewBizReader` / `migrate.go::Migrate` 被外部装配调用。

## 6. 并发与资源管理

不涉及。

## 7. 设计模式与亮点

- **三层聚合 raw / 5m / 1h**：注释明示三层聚合表，Writer 各自批量写，Reader 按 time window 选择。
- **dead-letter 专用路径**：失败点单独路径存储，每点一行（非 blob），便于运维查表。
- **Reader plain DAO**：注释明示 reader 是 plain DAO，time window 选择由 biz QueryUsecase 驱动，职责分离。

## 8. 注意事项

- **注释笔误**："Package sqlite" 实际包名 `store`；"+Writer" 缺空格；历史遗留。
- **Phase 2 升级**：`metric/clickhouse` 占位符升级后，部分流量（历史数据）切至 ClickHouse，本包仍承担实时写入。
- **三层聚合 schema**：扩展指标需同步更新三层 model + Writer + Reader。
