# `model.go` 技术实现文档

> 源文件：`internal/manager/model/webshell/model.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/model/webshell`

## 1. 概述

本文件定义 `Session` 实体：WebSSH 会话的审计行 schema。每个会话在 open 时 INSERT 一行，close 时 UPDATE 一行，含字节计数器与终止原因。设计要点：密码永不落此表；`TerminatedBy` 常量集合保证 handler 不漂移。红线：`BytesStdin` / `BytesStdout` 是双向字节计数；`ExitCode` 仅 SSH 终止时填；`TerminatedBy` 区分 user/idle/disconnect/admin_kill/ssh_auth_fail/ssh_exit/device_offline 七种终止原因。

## 2. 包信息

- **包名**：`webshell`
- **所属模块**：`internal/manager/model/`
- **依赖方向**：被 `manager/biz/webshell` 的 session manager 调用；依赖 `time`

## 3. 关键类型与接口

```go
// Termination 原因常量
const (
    TerminatedByUser          = "user"           // 浏览器关闭
    TerminatedByIdle          = "idle"           // 无输入超过 N 分钟
    TerminatedByDisconnect    = "disconnect"     // tunnel / WS 断开
    TerminatedByAdminKill     = "admin_kill"     // admin 踢出会话
    TerminatedBySSHAuthFail   = "ssh_auth_fail"  // sshd 拒绝凭据
    TerminatedBySSHExit       = "ssh_exit"       // 用户输入 exit / shell 结束
    TerminatedByDeviceOffline = "device_offline"
)

type Session struct {
    ID              string     `gorm:"primaryKey;size:64"`
    OngridUserID    uint64     `gorm:"not null;column:ongrid_user_id;index"`
    SSHUser         string     `gorm:"size:64;not null;column:ssh_user"`
    DeviceID        uint64     `gorm:"not null;column:device_id;index"`
    EdgeID          uint64     `gorm:"not null;column:edge_id"`
    ClientIP        string     `gorm:"size:64;column:client_ip"`
    StartedAt       time.Time  `gorm:"not null;column:started_at"`
    EndedAt         *time.Time `gorm:"column:ended_at"`
    BytesStdin      uint64     `gorm:"not null;default:0;column:bytes_stdin"`
    BytesStdout     uint64     `gorm:"not null;default:0;column:bytes_stdout"`
    ExitCode        int        `gorm:"not null;default:0;column:exit_code"`
    TerminatedBy    string     `gorm:"size:32;column:terminated_by"`
}
```

## 4. 关键函数与流程

### `Session.TableName`
- **签名**：`func (Session) TableName() string`
- **职责**：固定表名 `webshell_sessions`

## 5. 依赖关系

- **内部包**：`device` 包（通过 DeviceID）、`edge` 包（通过 EdgeID）
- **外部库**：`time`
- **被调用方**：`manager/biz/webshell` 的 session manager；SPA WebSSH 审计页面

## 6. 并发与资源管理

- 本文件仅定义 schema，无锁 / channel / 缓存
- 无软删（permanent audit）；如需清理另起 retention
- `ID` 是 string size:64（caller 预生成，可能是 UUID 或其他格式）

## 7. 设计模式与亮点

- **审计行 schema**：open 时 INSERT，close 时 UPDATE；不存密码
- **双向字节计数**：BytesStdin / BytesStdout 分开；便于审计用户输入 vs 输出量
- **TerminatedBy 7 种常量**：handler 不漂移；UI 按值显示中文原因
- **ExitCode 仅 SSH 终止填**：user/idle/disconnect/admin_kill 时为 0 默认值
- **OngridUserID 索引**：按用户查会话历史
- **DeviceID 索引**：按设备查会话历史
- **ClientIP 可空**：未上报时为空字符串
- **EndedAt 可空**：会话未结束时 NULL；close 时填
- **StartedAt 必填**：会话开始时刻
- **无软删**：permanent audit；retention job 可物理清理

## 8. 注意事项

- **ID 是 string size:64**：caller 预生成；可能是 UUID 或其他格式
- **OngridUserID 必填**：发起 WebSSH 的用户；0 = anonymous（仅测试）
- **SSHUser 必填**：SSH 登录用户名（如 root / ubuntu）
- **DeviceID 必填**：目标设备；通过 device 表反查
- **EdgeID 必填**：建立 SSH tunnel 的 edge
- **ClientIP 可空**：未上报时为空字符串
- **StartedAt 必填**：open 时由 biz 层填
- **EndedAt 可空**：未结束时 NULL；close 时填
- **BytesStdin / BytesStdout 默认 0**：open 时 0；close 时累计
- **ExitCode 默认 0**：仅 ssh_exit / ssh_auth_fail 时有意义
- **TerminatedBy 可空**：未结束时空字符串；close 时填常量
- **无密码字段**：密码永不落此表；ssh_auth_fail 仅记录失败事实
- **无软删**：permanent audit；retention job 物理清理
