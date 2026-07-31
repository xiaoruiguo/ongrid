# doc.go

## 1. 概述

`doc.go` 是 metric 包的包级注释文件，仅一行 `// Package metric ...` 注释，无任何代码。它向阅读者说明 metric 子域 biz 层的职责边界：

- 异步批量 ingest 来自 edge 的 host metrics
- 按时间窗口自动选表查询（raw / 5m / 1h）
- 下采样作业（5m 与 1h）
- 保留期执行（retention enforcement）

## 2. 包信息

- 包名：`metric`
- 路径：`internal/manager/biz/metric`
- 导入路径：`github.com/ongridio/ongrid/internal/manager/biz/metric`
- 文件无导入、无声明，纯文档

## 3. 关键类型与接口

无（纯文档文件）。

## 4. 关键函数与流程

无。

## 5. 依赖关系

无。

### 包内文件布局

- `doc.go` —— 包注释（本文件）
- `repo.go` —— `Writer` / `Reader` 接口声明
- `ingester.go` —— `Ingester` 异步批量 ingest + drop-oldest + retry + dead-letter
- `downsample.go` —— `Downsampler` 5m / 1h 聚合 + Loop
- `query.go` —— `QueryUsecase` 范围查询 + 自动分辨率选择
- `retention.go` —— `Retention` 按层级 TTL 删除 + 每日 Loop

## 6. 并发与资源管理

不适用。

## 7. 设计模式与亮点

### 单一职责边界声明

包注释明确四条职责，让阅读者立刻知道 metric 子域做什么、不做什么。例如：不负责告警评估（那是 alert 包）、不负责仪表盘镜像（那是 monitor + grafana 包）。

### 存储无关接口

注释暗示 Writer/Reader 接口是存储无关的（"SQLite writer lock" 在 retention 注释中提及，但接口本身用 domain `Point` / `Bucket` 类型，无 gorm tag）。这让 biz 层不关心数据落在 SQLite 还是 ClickHouse。

## 8. 注意事项

- **本文件无代码**：纯文档，修改包职责时同步更新此注释
- **包注释是 godoc 入口**：`go doc github.com/ongridio/ongrid/internal/manager/biz/metric` 首行来自此文件
- **职责边界声明**：新增功能若超出这四条职责，应考虑是否该放在别的包
