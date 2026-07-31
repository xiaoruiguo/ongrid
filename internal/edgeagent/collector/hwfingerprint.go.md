# `hwfingerprint.go` 技术实现文档

> 源文件：`internal/edgeagent/collector/hwfingerprint.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/collector`

## 1. 概述

本文件实现 clone-resistant 硬件指纹：从物理 NIC MAC + CPU 型号 + 磁盘序列号派生 SHA256，解决克隆 Linux VM 的 SMBIOS `product_uuid` 重复问题（issue #96）。还提供 `primaryIPv4()` 用于 register_edge 上报主 IPv4 地址。从 liaison-cloud edge agent 移植（pkg/utils/fingerprint.go）。

## 2. 包信息

- **包名**：`collector`
- **所属模块**：edgeagent metric 采集层（主机身份子模块）
- **依赖方向**：被同包 `embedded.go` / `scrape.go` 的 `HostInfo` 调用；调用 `gopsutil`、`net`

## 3. 关键类型与接口

无导出类型。内部使用 `ni` / `di` 匿名 struct 排序。

## 4. 关键函数与流程

### `hardwareFingerprint`
- **签名**：`func hardwareFingerprint() string`
- **职责**：派生 clone-resistant 硬件指纹
- **流程**：
  1. `physicalMACs()` 取最多前 2 个物理 NIC MAC（按接口名排序）
  2. 空切片返回 ""（caller fallback 到 HostID）
  3. `fmt.Sprintf("%s|%s|%s", macs, cpuSignature, diskSignature)` 拼接
  4. `sha256.Sum256` + `hex.EncodeToString`
- **错误处理**：任一组件失败返回空字符串，hash 仍可计算（MAC 主导身份）

### `physicalMACs`
- **签名**：`func physicalMACs() []string`
- **职责**：取物理 NIC MAC（最多 2 个，按名排序）
- **流程**：`net.Interfaces()` → 过滤 `isPhysicalNIC` → 排序 → 截断 2 → 提取 MAC 字符串
- **错误处理**：无物理 NIC 返回 nil

### `isPhysicalNIC`
- **签名**：`func isPhysicalNIC(iface *net.Interface) bool`
- **职责**：判断接口是否像真实物理 NIC
- **流程**：拒绝 loopback / point-to-point / MAC-less；拒绝名字匹配虚拟 NIC 关键词（docker/veth/cni/flannel/virbr/br-/tun/tap/vmnet/vboxnet/vmware/hyper-v/vbox/wsl/vpn/utun/bridge/awdl/anpi/isatap/teredo/kube）
- **错误处理**：无错误返回；不匹配返回 false

### `cpuSignature`
- **签名**：`func cpuSignature() string`
- **职责**：去重 + 排序的 CPU 型号身份
- **流程**：`cpu.Info()` → 每颗 CPU `ModelName+VendorID+Family` 拼接 → 去重 → 排序 → 逗号连接
- **错误处理**：失败返回 ""

### `diskSignature`
- **签名**：`func diskSignature() string`
- **职责**：取最多前 2 个物理磁盘序列号（按设备名排序）
- **流程**：`disk.IOCounters()` → 过滤空序列号 + 名字含 "virtual"/"loop" → 排序 → 截断 2 → 逗号连接
- **错误处理**：失败返回 ""

### `primaryIPv4`
- **签名**：`func primaryIPv4() string`
- **职责**：返回首选非 loopback IPv4
- **流程**：
  1. 第一遍：物理 NIC 上的 IPv4（同 `isPhysicalNIC` 过滤）
  2. 第二遍：任意非 loopback 接口（cloud-only VM fallback）
  3. 都失败返回 ""
- **错误处理**：每个接口 Addrs 失败 continue

## 5. 依赖关系

- **内部包**：无
- **外部库**：`github.com/shirou/gopsutil/v3/cpu`、`github.com/shirou/gopsutil/v3/disk`、标准库 `crypto/sha256`、`encoding/hex`、`fmt`、`net`、`sort`、`strings`
- **被调用方**：同包 `embedded.go::HostInfo`、`scrape.go::HostInfo`

## 6. 并发与资源管理

无并发控制。函数无状态、纯计算 + 系统调用。`net.Interfaces()` 和 gopsutil 调用各自线程安全。

## 7. 设计模式与亮点

- **Hypervisor 分配 vs SMBIOS 复制**：克隆 VM 时 SMBIOS `product_uuid` 被复制（导致 HostID 重复），但 hypervisor 给每个克隆分配新 NIC MAC——所以 MAC-keyed 指纹能区分
- **多组件拼接**：`MAC|CPU|disk` 三组件——MAC 主导身份，CPU/disk 作为冗余信号；任一缺失不影响 hash 稳定性
- **排序保证稳定**：所有组件按名 / 型号排序，跨重启指纹不变
- **截断到 2 个**：避免多 NIC 主机指纹过长；前 2 个已足够区分克隆
- **虚拟 NIC 关键词列表**：覆盖 Linux 前缀（docker/veth/cni/...）+ 跨平台 vendor 名（vmnet/vboxnet/vmware/hyper-v/...）+ 协议名（tun/tap/vpn/isatap/teredo）
- **`primaryIPv4` 两遍扫描**：物理 NIC 优先（与 fingerprint 同过滤），fallback 任意非 loopback（cloud VM 主 NIC 可能不匹配启发式）
- **disk 名字过滤 "virtual"/"loop"**：排除虚拟磁盘 / loop device；物理磁盘序列号才是稳定身份

## 8. 注意事项

- 指纹稳定性依赖硬件不变——更换 NIC / 磁盘会让指纹变化，cloud 侧会创建新 device row；这是预期行为
- 容器化部署（如 K8s pod）通常无物理 NIC MAC，`physicalMACs()` 返回 nil → fingerprint 返回 "" → register_edge 用 HostID fallback；K8s 部署应配置 `Kubernetes.NodeName/NodeUID` 让 `applyKubernetesHostIdentity` 覆盖指纹
- `isPhysicalNIC` 关键词列表是启发式——新型虚拟化技术（如 AWS ena、Azure accelerated networking）可能不被识别；operator 可通过 K8s 身份注入绕过
- `disk.IOCounters()` 在某些 Linux 内核上不返回序列号（依赖 udev）；序列号缺失时 diskSignature 返回 ""，fingerprint 仍可计算（MAC 主导）
- `primaryIPv4` 在 NAT 后的主机上返回内网 IP——这是预期，cloud 侧应通过隧道源 IP 而非该字段判断真实公网 IP
- 指纹不包含主机名 / 内核版本等易变字段——保证跨重启稳定
