# `model.go` 技术实现文档

> 源文件：`internal/manager/model/monitor/model.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/model/monitor`

## 1. 概述

本文件定义 `Panel` 实体：用户管理的 Monitor 页面面板。Operator 通过 SPA 创建 / 编辑 / 删除面板；行是 source of truth，ongrid 异步镜像到单个 Grafana dashboard 让 deep-link / "在 Grafana 中打开" 持续工作。设计要点：单向同步——ongrid 是 source of truth，Grafana 中编辑镜像 dashboard 不回拉（操作员想保留改动必须经 ongrid UI round-trip）。红线：`PanelType` 值与 SPA PromQLPanel 标识符及 Grafana panel type 1:1 映射，保证镜像 dashboard 渲染一致。

## 2. 包信息

- **包名**：`monitor`
- **所属模块**：`internal/manager/model/`
- **依赖方向**：被 `manager/biz/monitor` 与 Grafana mirror 同步 job 调用；依赖 `time`

## 3. 关键类型与接口

```go
// PanelType 枚举
const (
    PanelTypeTimeseries = "timeseries"
    PanelTypeStat       = "stat"
    PanelTypeGauge      = "gauge"
)

type Panel struct {
    ID        uint64    `gorm:"primaryKey;autoIncrement"                                json:"id"`
    Title     string    `gorm:"size:128;not null"                                       json:"title"`
    Type      string    `gorm:"size:32;not null;default:timeseries"                     json:"type"`
    PromQL    string    `gorm:"type:text;not null;column:promql"                        json:"promql"`
    Legend    string    `gorm:"size:255;not null;default:''"                            json:"legend"`
    Unit      string    `gorm:"size:32;not null;default:''"                             json:"unit"`
    Ordinal   int       `gorm:"not null;default:0;index"                                json:"ordinal"`
    LastSyncError string `gorm:"size:512;not null;default:'';column:last_sync_error"     json:"last_sync_error,omitempty"`
    LastSyncAt    *time.Time `gorm:"column:last_sync_at"                                 json:"last_sync_at,omitempty"`
    UpdatedAt time.Time `gorm:"autoUpdateTime"                                          json:"updated_at"`
    CreatedAt time.Time `gorm:"autoCreateTime"                                          json:"created_at"`
}
```

## 4. 关键函数与流程

### `Panel.TableName`
- **签名**：`func (Panel) TableName() string`
- **职责**：固定表名 `monitor_panels`，避免包重命名后误分支 schema

### `ValidPanelType`
- **签名**：`func ValidPanelType(t string) bool`
- **职责**：判断 t 是否为支持的 panel type
- **流程**：switch t 是否在 timeseries / stat / gauge 中
- **用途**：biz validator 在持久化前调用，拒绝未知 type

## 5. 依赖关系

- **内部包**：无
- **外部库**：`time`
- **被调用方**：`manager/biz/monitor` 的 CRUD service；Grafana mirror 同步 job

## 6. 并发与资源管理

- 本文件仅定义 schema，无锁 / channel / 缓存
- 无软删（Panel 删除直接物理 DELETE）
- `Ordinal` 索引支持按顺序渲染

## 7. 设计模式与亮点

- **单向同步**：ongrid 是 source of truth；Grafana 编辑不回拉
- **PanelType 1:1 映射 Grafana**：SPA PromQLPanel 用相同标识符；Grafana panel type 1:1 对应，镜像 dashboard 渲染一致
- **Ordinal 控制渲染顺序**：新建面板 `max(ordinal)+1` 落底；reorder 走 PATCH ordinal
- **LastSyncError / LastSyncAt 同步状态**：UI 显示最近一次 Grafana mirror 失败原因；空 = 成功或未尝试
- **PromQL TEXT NOT NULL**：biz 层总写值
- **Legend / Unit 可空**：默认空字符串
- **Type 默认 timeseries**：最常用类型
- **ValidPanelType biz 校验**：持久化前调，拒绝未知 type
- **json tag PascalCase 不带 omitempty（除 LastSyncError/LastSyncAt）**：UI 总需显示

## 8. 注意事项

- **Type 必须是已知 PanelType**：biz 层 ValidPanelType 校验
- **PromQL 必填**：空 PromQL 无意义
- **Ordinal 0 是合法值**：首次创建默认 0
- **LastSyncError 空表示成功或未尝试**：UI 应区分（看 LastSyncAt 是否 NULL）
- **LastSyncAt NULL**：从未同步过
- **无软删**：Panel 删除直接 DELETE；Grafana mirror 同步删除对应 panel
- **单向同步**：Grafana 中编辑镜像 dashboard 会丢失；操作员想保留必须经 ongrid UI
- **Title 必填**：UI 显示用
- **Legend / Unit 可选**：影响渲染样式
- **Ordinal 索引**：支持 ORDER BY ordinal 渲染
