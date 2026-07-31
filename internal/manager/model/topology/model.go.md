# `model.go` 技术实现文档

> 源文件：`internal/manager/model/topology/model.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/model/topology`

## 1. 概述

本文件是业务拓扑层的 schema：typed property graph 模型。每个业务实体（device / service / cluster / app / rack）由一个 `Node` 行代表；边在 `Relation`；边语义（src 故障是否传到 dst？哪个方向？）在 `RelationType`。6 个内置 relation type 作为 seed data 不能删除；operator 可注册新 type 但必须声明三个 AIOps-relevant 字段（Propagates / Direction / SemanticsTag），让 reasoning 层按语义而非名字处理 relation。设计要点：实体表（device / service / cluster / app）通过自身 `node_id UNIQUE FK→node.id` 反向链；`Relation` 仅引用 `Node.ID`，所以两 device / 两 service / device 与 service 间的关系在 graph 层都一样。红线：`NodeType.Tier` 故意无 `default:` tag——GORM 与 Go 零值组合会 silently 把合法 `tier:0`（top-tier app）翻为 default 99；`KnownSemanticsTags` 是闭集，新增 bucket 需 ADR（reasoning 层 key 在这些字符串上）。

## 2. 包信息

- **包名**：`topology`
- **所属模块**：`internal/manager/model/`
- **依赖方向**：被 `manager/biz/topology` 与 AIOps reasoning layer 调用；依赖 `gorm.io/gorm`、`gorm.io/plugin/soft_delete`、`time`

## 3. 关键类型与接口

```go
type NodeKind string

const (
    NodeTypeDevice  NodeKind = "device"
    NodeTypeService NodeKind = "service"
    NodeTypeCluster NodeKind = "cluster"
    NodeTypeApp     NodeKind = "app"
    NodeTypeRack    NodeKind = "rack"
)

type Node struct {
    ID        uint64 `gorm:"primaryKey;autoIncrement"`
    Type      string `gorm:"size:32;not null;index:idx_nodes_type_name,priority:1"`
    Name      string `gorm:"size:255;not null;index:idx_nodes_type_name,priority:2"`
    PropsJSON string `gorm:"type:text;not null;column:props_jsonb"`
    CreatedAt time.Time
    UpdatedAt time.Time
    DeletedAt gorm.DeletedAt `gorm:"index"`
}

type Relation struct {
    ID           uint64 `gorm:"primaryKey;autoIncrement"`
    SrcID        uint64 `gorm:"not null;column:src_id;uniqueIndex:idx_relations_src_dst_type,priority:1;index:idx_relations_src_type"`
    DstID        uint64 `gorm:"not null;column:dst_id;uniqueIndex:idx_relations_src_dst_type,priority:2;index:idx_relations_dst_type"`
    Type         string `gorm:"size:64;not null;uniqueIndex:idx_relations_src_dst_type,priority:3"`
    PropsJSON    string `gorm:"type:text;not null;column:props_jsonb"`
    CreatedAt    time.Time
    UpdatedAt    time.Time
    DeletedAt    *time.Time            `gorm:"index;column:deleted_at"`
    DeleteMarker soft_delete.DeletedAt `gorm:"column:delete_marker;not null;default:0;softDelete:milli,DeletedAtField:DeletedAt;uniqueIndex:idx_relations_src_dst_type,priority:4"`
}

type Direction string

const (
    DirectionSrcToDst       Direction = "src_to_dst"  // routes_to: src 故障传到 dst
    DirectionDstToSrc       Direction = "dst_to_src"  // depends_on: dst 死则 src 受影响
    DirectionBidirectional  Direction = "bidirectional" // replicates_to: 任一侧失数据污染另侧
)

type SemanticsTag string

const (
    SemanticsHardDep     SemanticsTag = "hard_dep"
    SemanticsRuntimeDep  SemanticsTag = "runtime_dep"
    SemanticsAggregation SemanticsTag = "aggregation"
    SemanticsRedundancy  SemanticsTag = "redundancy"
    SemanticsObservation SemanticsTag = "observation"
    SemanticsTraffic     SemanticsTag = "traffic"
    SemanticsAnnotation  SemanticsTag = "annotation"
)

var KnownSemanticsTags = []SemanticsTag{ /* 7 个 */ }
var KnownDirections = []Direction{ /* 3 个 */ }

type NodeType struct {
    Name        string `gorm:"size:32;primaryKey;column:name"`
    DisplayName string `gorm:"size:128;not null"`
    DisplayNameEN string `gorm:"size:128;not null;default:'';column:display_name_en"`
    Builtin       bool   `gorm:"not null;default:false"`
    Tier        int    `gorm:"not null"` // 故意无 default:
    Description string `gorm:"type:text;not null"`
    CreatedAt   time.Time
    UpdatedAt   time.Time
}

type RelationType struct {
    Name        string `gorm:"size:64;primaryKey;column:name"`
    DisplayName string `gorm:"size:128;not null;default:''"`
    DisplayNameEN     string `gorm:"size:128;not null;default:'';column:display_name_en"`
    Builtin           bool   `gorm:"not null;default:false"`
    PropagatesFailure bool   `gorm:"not null;default:false;column:propagates_failure"`
    Direction         string `gorm:"size:16;not null;column:direction"`
    SemanticsTag      string `gorm:"size:32;not null;column:semantics_tag"`
    Description       string `gorm:"type:text;not null"`
    CreatedAt         time.Time
    UpdatedAt         time.Time
}

// 内置 relation type name
const (
    RelMemberOf     = "member_of"
    RelDependsOn    = "depends_on"
    RelDeployedOn   = "deployed_on"
    RelReplicatesTo = "replicates_to"
    RelMonitors     = "monitors"
    RelRoutesTo     = "routes_to"
)
```

## 4. 关键函数与流程

### `Node.TableName / Relation.TableName / NodeType.TableName / RelationType.TableName`
- 固定表名分别为 `nodes` / `relations` / `node_types` / `relation_types`

### `BuiltinNodeTypes`
- **签名**：`func BuiltinNodeTypes() []NodeType`
- **职责**：返回 5 个内置 NodeType 的 seed set
- **覆盖**：app（tier 0）/ service（tier 1）/ cluster（tier 2）/ device（tier 3）/ rack（tier 4）
- **用途**：migrator 在每次 boot 时 upsert；DisplayNameEN 让中文 seed 在 en-US locale 也可读

### `BuiltinRelationTypes`
- **签名**：`func BuiltinRelationTypes() []RelationType`
- **职责**：返回 6 个内置 RelationType 的 seed set
- **覆盖**：member_of / depends_on / deployed_on / replicates_to / monitors / routes_to
- **关键决策**：
  - `depends_on` PropagatesFailure=true, Direction=dst_to_src, SemanticsTag=hard_dep（核心影响面计算）
  - `deployed_on` PropagatesFailure=true, Direction=dst_to_src, SemanticsTag=runtime_dep
  - `routes_to` PropagatesFailure=true, Direction=src_to_dst, SemanticsTag=traffic
  - `member_of` 不传故障，aggregation
  - `replicates_to` bidirectional，redundancy
  - `monitors` src_to_dst，observation

### `IsValidDirection`
- **签名**：`func IsValidDirection(s string) bool`
- **职责**：判断是否为已知 Direction

### `IsValidSemanticsTag`
- **签名**：`func IsValidSemanticsTag(s string) bool`
- **职责**：判断是否为 canonical semantics bucket；custom RelationType.SemanticsTag 必须通过

## 5. 依赖关系

- **内部包**：`device` 包（通过 DeviceID/NodeID 反向链）、`k8s` 包（通过 NodeID）、其他实体表（service / cluster / app / rack）
- **外部库**：`gorm.io/gorm`、`gorm.io/plugin/soft_delete`、`time`
- **被调用方**：`manager/biz/topology`；AIOps reasoning layer（按 SemanticsTag 调度）；migrator（upsert BuiltinNodeTypes / BuiltinRelationTypes）

## 6. 并发与资源管理

- 本文件仅定义 schema，无锁 / channel / 缓存
- `Node.DeletedAt` 软删除（gorm.DeletedAt）
- `Relation.DeleteMarker` 加入 unique index 让软删后同 (src, dst, type) 可重建

## 7. 设计模式与亮点

- **typed property graph**：每实体一 Node 行；Relation 仅引用 Node.ID，跨类型关系统一
- **NodeKind 5 个内置**：device / service / cluster / app / rack；custom kind 允许（plain string，无 enum）
- **NodeType operator-visible catalogue**：5 builtin row 作为 seed；DisplayNameEN 让中文 seed 在 en-US locale 可读
- **Tier 故意无 default:**：GORM 与 Go 零值组合会 silently 把合法 `tier:0`（top-tier app）翻为 default 99；Usecase 上游 normalize 非法输入
- **RelationType 6 个内置**：member_of / depends_on / deployed_on / replicates_to / monitors / routes_to
- **PropagatesFailure + Direction + SemanticsTag 三字段**：AIOps reasoning 必须声明；按 SemanticsTag 调度而非 Name，让 custom type 参与 dependency 分析无需代码变更
- **Direction 三态**：src_to_dst（routes_to）/ dst_to_src（depends_on，src=X 依赖 dst=Y，dst 死则 src 受影响）/ bidirectional（replicates_to）
- **KnownSemanticsTags 闭集**：7 个 bucket；新增需 ADR（reasoning 层 key 在这些字符串上）
- **(src, dst, type) 唯一**：同 pair 可有多 relation 仅当 type 不同（service 可同时 depends_on + monitors 另一 service）
- **BuiltinNodeTypes / BuiltinRelationTypes seed**：migrator boot 时 upsert；`DoUpdates` clause refresh display_name / tier / description，doc 微调不需独立迁移
- **Builtin=true 不可编辑**：UI hide editor for builtin row；operator editing 是 no-op
- **PropsJSON free-form bag**：owner_team / region / cost_center 等不值得独立列的字段
- **DisplayNameEN 双语 overlay**：与 knowledge.Doc.TitleEN 同 pattern；空 = fallback DisplayName

## 8. 注意事项

- **Node.Type 是 string 非 enum**：custom kind 允许；NodeType.Name 是 PK
- **NodeType.Tier 无 default**：Usecase 必须显式 normalize；0 是合法 top-tier
- **RelationType.Name 是 PK**：跨所有行；rename 需先删后建
- **Builtin=true 不可编辑 / 删除**：migrator 每次 boot upsert；UI hide editor
- **(src, dst, type) 唯一**：DeleteMarker 加入 unique 让软删后可重建
- **KnownSemanticsTags 闭集**：custom type 必须从中选；新增 bucket 需 ADR
- **KnownDirections 闭集**：custom type 必须从中选
- **Direction 语义**：
  - `depends_on`: src 依赖 dst，dst 故障影响 src（dst_to_src）
  - `routes_to`: src 流量打 dst，src 故障导致 dst 不可达（src_to_dst）
  - `replicates_to`: 双向，任一侧失数据污染另侧
- **PropsJSON 必填**：biz 总写至少 "{}"
- **Description 必填**：UI 显示帮助识别
- **DisplayNameEN 可空**：空 = fallback DisplayName
- **Node 软删用 gorm.DeletedAt**：无 DeleteMarker
- **Relation 软删用 DeleteMarker**：参与 (src, dst, type) unique 约束
