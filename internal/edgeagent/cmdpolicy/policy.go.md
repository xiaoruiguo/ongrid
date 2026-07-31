# `policy.go` 技术实现文档

> 源文件：`internal/edgeagent/cmdpolicy/policy.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/cmdpolicy`

## 1. 概述

本文件定义 v1 只读策略基线：`DefaultReadOnly` 返回 curated 的二进制 + 类别 + matcher 列表；`LoadFromYAML` 在基线之上合并 operator 覆盖；`Policy.Decide` / `decideSegment` 是策略决策核心（不做 path / network 校验，由 Sandbox 层叠加）。

## 2. 包信息

- **包名**：`cmdpolicy`
- **所属模块**：edgeagent 命令策略层
- **依赖方向**：被同包 `sandbox.go` 调用；调用 `parse.go`、`types.go`

## 3. 关键类型与接口

无新类型定义（类型在 `types.go`）。本文件主要定义 `DefaultReadOnly`、`LoadFromYAML`、`Decide` 等函数。

```go
const (
    defaultStdoutCap = 64 * 1024
    defaultStderrCap = 16 * 1024
    defaultTimeout   = 30 * time.Second
    defaultMaxArgs   = 32
)
```

## 4. 关键函数与流程

### `DefaultReadOnly`
- **签名**：`func DefaultReadOnly() *Policy`
- **职责**：构造生产默认只读策略
- **流程**：初始化 Policy（caps/timeout/maxargs + path/network allowlist），按类别注册二进制：
  - **ClassReadFS (16)**：cat/head/tail/tac/less/ls/find/du/stat/readlink/file/tree/wc/grep/egrep/fgrep + awk（拒绝 `system(` `| sh` 等）/ sed（拒绝 `-i` `--in-place`）
  - **ClassReadSystem (17)**：ps/top/uptime/free/df/iostat/vmstat/mpstat/pidstat/lsof/ss/netstat/dmesg/who/w/uname/id/groups + hostname（拒绝 `-b -s --set`）/ date（拒绝 `-s --set`）/ journalctl（拒绝 `--rotate --vacuum-* --flush --sync` 等）
  - **ClassMixed (7)**：iptables/ip6tables/tc/systemctl/ip/mount/crontab — 每个 ReadOnlyMatchers / WriteMatchers 划分读写子命令
  - **高级网络探针**：ovs-vsctl/ovs-ofctl/ovs-dpctl/ovs-appctl/nft/conntrack/ipset/ethtool/bpftool — 全部 Mixed 类别
  - **ClassNetwork (7)**：nc（仅 `-z`，拒 `-e -c -l`）/ curl（仅 `--head -I`，拒 `-o -O --output -T`）/ wget（仅 `--spider`，拒 `-O -o`）/ dig / host / nslookup / ping（拒 `-f`）/ traceroute
  - **ClassDenied (~25)**：shells（bash/sh/zsh/dash/ash）、scripting（python/perl/ruby/node/lua/tcl）、destructive fs（rm/rmdir/mv/cp/dd/mkfs/truncate/shred）、permission（chmod/chown/chgrp/setfacl）、system mutators（shutdown/reboot/halt/poweroff/kexec）、auth（useradd/userdel/usermod/groupadd/groupdel/passwd/chpasswd）、iptables-restore/ip6tables-restore
  - 解析每个非 Denied 二进制的 AbsPath（`exec.LookPath` + `/usr/sbin` 等回退链）
- **错误处理**：AbsPath 解析失败保留 ""，`Decide` 会报「not installed」

### `LoadFromYAML`
- **签名**：`func LoadFromYAML(path string, base *Policy) (*Policy, error)`
- **职责**：在 base 之上合并 YAML 覆盖
- **合并语义**：
  - `binaries[*]`：同 name **完全替换** base 中条目；新 name 追加
  - `network_host_allowlist` / `path_allowlist`：set 时替换，absent 时保留 base
  - caps / timeout / max_argv_length：> 0 时替换，0/absent 保留
- **流程**：`os.ReadFile` → `yaml.Unmarshal` 到 `yamlPolicy` → `clonePolicy(base)` → 遍历 binaries 调 `addBin` 覆盖 → 应用 network/path/caps 覆盖
- **错误处理**：读文件 / 解析 / 空 name / 非法 class 都返回带 path 的错误

### `Policy.Decide`
- **签名**：`func (p *Policy) Decide(cmd string) Decision`
- **职责**：策略层决策（不含 path/network 校验）
- **流程**：
  1. `SplitPipes` 切管道分段
  2. 每段 `len(seg) > MaxArgs` 拒绝
  3. `decideSegment(seg)` 单段决策
  4. 任一段拒绝立即返回；全部通过返回 `{Allow: true, Segments}`
- **错误处理**：SplitPipes 错误包装为 `Decision{Allow: false, Reason}`

### `decideSegment`
- **签名**：`func (p *Policy) decideSegment(argv []string) Decision`
- **职责**：单段分类决策
- **流程**：
  1. argv[0] 取 basename，查 `bins[bin]`；nil 报「binary not in policy」
  2. `ClassDenied` 直接拒绝
  3. `AbsPath==""` 报「not installed」
  4. DeniedArgs 子串匹配（catches `system(` 在 awk 程序中、`-i` 在 sed 中）
  5. 按类别：
     - `ClassReadFS` / `ClassReadSystem`：直接 ALLOW
     - `ClassMixed`：mount 特殊启发（≥2 绝对路径 = 写）；kill 特殊启发（无 `-l`/`-L` = 写）；WriteMatcher 命中 → REJECT；ReadOnlyMatcher 命中 → ALLOW；默认 REJECT
     - `ClassNetwork`：WriteMatcher 命中 → REJECT；否则 ALLOW（host allowlist 由 Sandbox 校验）
- **错误处理**：未知 class 报错

### matcher 辅助函数
- `matcherListMatches` / `matcherHits` / `hasAnyFlag` / `hasSubcmdPath` / `countAbsolutePaths`：匹配器实现细节
- `addBin`：注册或覆盖二进制策略（later call wins，支持 DefaultReadOnly 中的 loop + override 模式）
- `Lookup`：按 basename 查策略
- `Bins`：返回所有二进制名
- `discoverBin`：`exec.LookPath` + `/usr/bin /bin /usr/sbin /sbin /usr/local/bin` 回退
- `clonePolicy`：深拷贝 Policy 防止 YAML 覆盖 mutate 调用方的 base

## 5. 依赖关系

- **内部包**：无外部 import；只用同包 `types.go` 的类型 + `parse.go` 的 SplitPipes
- **外部库**：`gopkg.in/yaml.v3`、标准库 `fmt`、`os`、`os/exec`、`path/filepath`、`strings`、`time`
- **被调用方**：同包 `sandbox.go::Sandbox.Decide` / `Sandbox.Exec`；`bash/handlers.go::Register`

## 6. 并发与资源管理

无并发控制。`Policy` 构造后视为不可变（多个 Sandbox 共享读）。`bins` 是 map 但构造期填充后不再修改；`LoadFromYAML` 通过 `clonePolicy` 深拷贝隔离。

## 7. 设计模式与亮点

- **类别 + matcher 双层模型**：BinaryClass 是粗粒度分类（read-fs / read-system / mixed / network / denied）；ArgMatcher 是细粒度 argv 匹配（AnyFlag / Subcmd / SubcmdPath 三种子规则）。同一二进制可有多组 ReadOnlyMatchers / WriteMatchers
- **WriteMatcher 优先 + 默认 REJECT**：Mixed 类别默认拒绝，必须命中 ReadOnlyMatcher 才允许；安全侧偏向
- **catch-all matcher**：空 matcher 匹配任意 argv，用于 mount 的「bare `mount` 默认读」、ovs-appctl 的「默认读」等回退
- **特殊启发式编码在 Decide**：mount 的「≥2 绝对路径 = 写」、kill 的「无 -l/-L = 写」无法用 matcher 表达，硬编码在 `decideSegment`；保持 matcher 数据驱动的同时支持这些特殊场景
- **`netns exec` 显式拒绝**：`ip netns exec <ns> <cmd>` 会重新进入 cmdpolicy 边界用任意 argv，必须拒绝；同时 `-n <ns>` 全局选项不拒绝（仅命名空间读）
- **operator 覆盖完全替换语义**：YAML 中同 name 二进制完全替换而非字段级合并；v1 不提供「移除」动词，operator 可传 `base=空` 重建；保持兼容同时支持未来加 remove verb

## 8. 注意事项

- DeniedArgs 是**子串匹配**而非精确匹配——`system(` 能 catch awk 程序中的调用，但也会误杀包含 `system(` 字面的合法路径参数；需谨慎添加
- `iptables-restore` / `ip6tables-restore` 在 Denied 列表中，但 `iptables-restore` 二进制 basename 是 `iptables-restore`——Lookup 时取 `filepath.Base`，正确匹配
- `discoverBin` 的回退链是 Linux 路径；macOS / Windows 上某些二进制可能找不到（如 `journalctl`），AbsPath 为空时 Decide 报「not installed」
- `DefaultReadOnly` 中 `find` 先注册简单条目再注册带 DeniedArgs 的版本——`addBin` 的 later-call-wins 语义让这种「loop + override」模式工作
- 操作员 YAML 中 `class` 必须是有效值（read-fs/read-system/mixed/network/denied）；非法值在 LoadFromYAML 时报错
- `clonePolicy(nil)` 会用默认 caps 而非零值——避免调用方传 nil base 时遇到零值 caps 导致输出无上限
