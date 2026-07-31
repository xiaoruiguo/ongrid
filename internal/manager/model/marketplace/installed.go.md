# `installed.go` 技术实现文档

> 源文件：`internal/manager/model/marketplace/installed.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/model/marketplace`

## 1. 概述

本文件定义 `InstalledPack` 实体：installed_skills 锁定表，每行对应一个已安装 pack（skill bundle / claude plugin / openclaw plugin）。`manifest_sha256` 是 disk-content lock，install 时判重 + boot 时验证（mismatch → mark broken 不加载）。设计要点：单租户 MVP，`tenant_id` 列为未来多租户预留（今日全写 0 或 1）；`CapabilitiesJSON` 存用户审批的能力声明（merged edge_capabilities + requires + tool classes）；`BindingsJSON` 是 credential slot → vault credential name 的映射（HLD-017）。红线：`InstallPath` 必须 sit under `cfg.TenantSkillsRoot / cfg.SystemSkillsRoot`，Uninstall `rm -rf` 前做安全检查防越界删除。

## 2. 包信息

- **包名**：`marketplace`
- **所属模块**：`internal/manager/model/`
- **依赖方向**：被 `manager/biz/marketplace` 的 install / uninstall / boot-time verifier 调用；依赖 `gorm.io/plugin/soft_delete`、`time`

## 3. 关键类型与接口

```go
type InstalledPack struct {
    ID uint64 `gorm:"primaryKey;autoIncrement"`

    TenantID uint64 `gorm:"not null;default:0;index;uniqueIndex:idx_tenant_pack,priority:1"`
    PackID string `gorm:"size:128;not null;uniqueIndex:idx_tenant_pack,priority:2"`
    DisplayName string `gorm:"size:255"`
    Version string `gorm:"size:64"`

    // Source 安装路径族：ongrid-builtin / local / git / tarball / <registry-name>
    Source string `gorm:"size:64"`
    SourceURL string `gorm:"size:512"`
    InstallPath string `gorm:"size:512"` // 绝对路径

    ManifestSHA256 string `gorm:"size:64"` // hex of manifest content

    // SignatureState: verified / unsigned / failed
    SignatureState string `gorm:"size:32"`

    CapabilitiesJSON string `gorm:"type:text"` // 用户审批的能力声明
    BindingsJSON     string `gorm:"type:text"` // {slot: credential_name}

    InstalledBy uint64 `gorm:"not null;default:0"`
    InstalledAt time.Time `gorm:"autoCreateTime"`
    UpdatedAt   time.Time `gorm:"autoUpdateTime"`

    DeletedAt    *time.Time            `gorm:"column:deleted_at;index"`
    DeleteMarker soft_delete.DeletedAt `gorm:"column:delete_marker;not null;default:0;softDelete:milli,DeletedAtField:DeletedAt;uniqueIndex:idx_tenant_pack,priority:3"`
}

// SignatureState 常量
const (
    SigStateVerified = "verified"
    SigStateUnsigned = "unsigned"
    SigStateFailed   = "failed"
)
```

## 4. 关键函数与流程

### `InstalledPack.TableName`
- **签名**：`func (InstalledPack) TableName() string`
- **职责**：固定表名 `installed_skills`，避免未来包重命名误创建新 schema

## 5. 依赖关系

- **内部包**：`secret` 包（通过 BindingsJSON 反查 vault credential）
- **外部库**：`gorm.io/plugin/soft_delete`、`time`
- **被调用方**：`manager/biz/marketplace` 的 install / uninstall / boot-time verifier；skill executor（按 BindingsJSON 解析 credential slot）

## 6. 并发与资源管理

- 本文件仅定义 schema，无锁 / channel / 缓存
- `DeleteMarker` 加入 unique index 让软删后可重建同 (tenant, pack)

## 7. 设计模式与亮点

- **ManifestSHA256 双用途**：install 时判重（同 hash 已装 → 拒绝"用 Update"）；boot 时验证（mismatch → mark broken 不加载）
- **TenantID 多租户预留**：单租户 MVP 全写 0/1；unique index 让 (tenant, pack) 联合唯一
- **Source 五种安装路径**：ongrid-builtin（manager binary 内）/ local（host 路径）/ git（clone --depth=1）/ tarball（curl + tar）/ registry（代理）
- **SourceURL 诊断**：local 是传入路径；git/tarball 是 URL；registry 是 `<registry>:<pack_id>@<version>`
- **InstallPath 绝对路径**：Uninstall `rm -rf` 此路径；biz 层做 safety check 必须在 cfg.TenantSkillsRoot / cfg.SystemSkillsRoot 下
- **SignatureState 三态**：verified（cosign 验证，未实现）/ unsigned（默认）/ failed（签名验证失败）
- **CapabilitiesJSON 用户审批**：merged edge_capabilities + requires + tool classes from every skill in pack
- **BindingsJSON slot → credential name**：HLD-017；operator 选"哪个 credential 填此 slot"
- **InstalledBy 审计**：执行 install 的 user_id
- **DeleteMarker 在 unique**：软删后同 (tenant, pack) 可重装；不破坏 active-row 唯一性
- **SignatureState 未来扩展**：当前 unsigned 默认；cosign 验证落地后 verified 才有意义

## 8. 注意事项

- **(tenant, pack) 联合唯一**：reinstall 必须先 Uninstall；DeleteMarker 让软删后可重建
- **ManifestSHA256 必填**：install 时计算；boot 时验证
- **InstallPath 安全检查**：biz 层 Uninstall 前必须验证在 cfg root 下，防越界删除
- **SourceURL 诊断用**：不参与执行；仅显示给 operator
- **SignatureState 当前 unsigned**：cosign 未实现；未来扩展
- **CapabilitiesJSON 可空**：未审批能力时为空字符串
- **BindingsJSON 可空**：pack 无 requires.credentials 时为空字符串
- **InstalledBy 0 = anonymous**：测试路径；生产必填 user_id
- **TenantID 0 / 1**：单租户 MVP；多租户落地后扩 unique 索引
- **InstalledAt / UpdatedAt**：autoCreateTime / autoUpdateTime
- **DisplayName / Version 可空**：pack manifest 未声明时为 ""
