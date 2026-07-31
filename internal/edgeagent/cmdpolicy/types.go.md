# `types.go` 技术实现文档

> 源文件：`internal/edgeagent/cmdpolicy/types.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/cmdpolicy`

## 1. 概述

本文件是 cmdpolicy 包的类型定义中心 + 包级文档注释。声明 `BinaryClass` 类别枚举、`ArgMatcher` 匹配器、`BinaryPolicy` 单二进制策略、`Policy` 全局策略、`Decision` 决策结果等核心类型。包注释阐述五大设计支柱：类别分类、curated 默认策略、operator YAML 覆盖、PathValidator 复用、bounded argv（无 shell 元字符）。

## 2. 包信息

- **包名**：`cmdpolicy`
- **所属模块**：edgeagent 命令策略层
- **依赖方向**：被同包 `policy.go` / `sandbox.go` 使用

## 3. 关键类型与接口

```go
// BinaryClass 是二进制行为类别
type BinaryClass string

const (
    ClassReadFS     BinaryClass = "read-fs"     // 文件系统只读：ls/cat/find/awk/sed 等
    ClassReadSystem BinaryClass = "read-system" // 系统状态只读：ps/df/free/ss/lsof 等
    ClassMixed      BinaryClass = "mixed"        // 依赖参数：iptables -L 读 / iptables -A 写
    ClassNetwork    BinaryClass = "network"     // 出站网络：nc/curl/dig/ping
    ClassDenied     BinaryClass = "denied"       // 无条件拒绝：shells/scripting/destructive
)

// ArgMatcher 描述对 argv 的单条匹配规则
// 三种子规则任一命中即视为 matcher 命中；三者皆空 = catch-all
type ArgMatcher struct {
    AnyFlag    []string  // argv 中任一位置出现这些 token 之一
    Subcmd     string    // argv[1] 等于此 token
    SubcmdPath []string  // argv[1..len] 等于此 slice
}

// BinaryPolicy 是单二进制的策略
type BinaryPolicy struct {
    Bin     string      // basename，如 "iptables"
    AbsPath string      // 解析的绝对路径；"" 表示未安装
    Class   BinaryClass
    ReadOnlyMatchers []ArgMatcher  // 仅 Mixed/Network 类别生效
    WriteMatchers    []ArgMatcher
    DeniedArgs []string  // 子串匹配的全局黑名单（find -delete / sed -i / awk system(）
}

// Policy 是完整策略集
type Policy struct {
    bins                map[string]*BinaryPolicy
    NetworkHostAllowlist []string         // CIDR 或 hostname suffix；空 = 拒绝所有出站
    StdoutCap           int               // 默认 64KB
    StderrCap           int               // 默认 16KB
    Timeout             time.Duration     // 默认 30s
    MaxArgs             int               // 默认 32
    PathAllowlist       []string          // 绝对路径前缀；空 = 不做 path 校验
}

// Decision 是 Policy.Decide / Sandbox.Decide 的结果
type Decision struct {
    Allow    bool
    Reason   string       // 拒绝时的人类可读原因，适合 surface 给 LLM
    Segments [][]string   // 解析后的管道分段；解析失败时为 nil
}
```

## 4. 关键函数与流程

无函数定义。仅是类型声明 + 包注释。

## 5. 依赖关系

- **内部包**：无
- **外部库**：`time`（用于 `Timeout` 字段类型）
- **被调用方**：同包 `policy.go`、`sandbox.go`；外部 `bash/handlers.go`

## 6. 并发与资源管理

无并发控制。类型本身是数据结构，无行为。

## 7. 设计模式与亮点

- **类别枚举 + 字符串类型**：`BinaryClass` 是 string 而非 int，便于 YAML 序列化和日志可读性；`validClass` 在 LoadFromYAML 中校验
- **catch-all matcher**：`ArgMatcher` 三子规则皆空时匹配任意 argv——支持 mount 的「bare `mount` 默认读」、ovs-appctl 的「默认读」回退
- **子串匹配的 DeniedArgs**：能 catch `system(` 嵌入在 awk 程序字符串中，也能 catch `-delete` / `-i` 等参数；比精确匹配灵活但有误杀风险
- **Decision 携带 Segments**：即使拒绝也返回解析后的管道分段，让 LLM 看到它「问的是什么」，而非仅 "no"——便于模型自我修正
- **AbsPath 在构造期解析**：`DefaultReadOnly` / `LoadFromYAML` 时调 `discoverBin`，避免每次 Exec 都 LookPath；缺失二进制 AbsPath 为空，Decide 报「not installed」
- **bins 是私有 map**：通过 `Lookup` / `Bins` 暴露只读访问；防止外部直接 mutate

## 8. 注意事项

- `Policy.bins` 是私有字段，外部不能直接遍历；`Bins()` 返回 keys 但**不保证顺序**（Go map 迭代无序）；调用方需自行排序
- `Decision.Segments` 在 SplitPipes 失败时为 nil；调用方需检查 Allow 字段而非 Segments 是否为空
- `ArgMatcher.AnyFlag` 是「精确等于」匹配（`a == f`），不是前缀匹配；`-L` 不会匹配 `--list`，需在 matcher 中分别列出
- `DeniedArgs` 是子串匹配（`strings.Contains`），`-i` 会误匹配 `--include=/foo` 中的 `-i`；需谨慎添加短 token
- `PathAllowlist` 为空时 Sandbox.Decide 跳过 path 校验（NOT recommended）；DefaultReadOnly 提供 curated 列表
- `NetworkHostAllowlist` 为空时拒绝所有出站——这是默认安全侧偏向；operator 必须显式配置才能让 LLM 访问内部服务
- 包注释提到「未来 skills（plugin custom shells, guarded mutating bash）可组合不同 Policy + 同一 Sandbox runner」——Sandbox 与 Policy 解耦支持这种扩展
