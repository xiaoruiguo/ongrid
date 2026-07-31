# `scrapecfg.go` 技术实现文档

> 源文件：`internal/edgeagent/collector/scrapecfg.go`
> 包路径：`github.com/ongridio/ongrid/internal/edgeagent/collector`

## 1. 概述

本文件定义 scrape 模式的 YAML 配置 schema 与加载器：`ScrapeConfig` 是 `/etc/ongrid-edge/scrape.yaml` 的解析结果，包含多个 `ScrapeTarget`（name/url/role/interval/timeout/bearer_token/tls/static_labels）。`LoadScrapeConfig` 读文件 + 解析 + 应用默认值 + 校验。

## 2. 包信息

- **包名**：`collector`
- **所属模块**：edgeagent metric 采集层（scrape 配置子模块）
- **依赖方向**：被 `cmd/ongrid-edge` 主程序调用加载；被同包 `scrape.go::NewScraper` 消费

## 3. 关键类型与接口

```go
// ScrapeConfig 是 /etc/ongrid-edge/scrape.yaml 的解析结果
type ScrapeConfig struct {
    Targets []ScrapeTarget `yaml:"targets"`
}

const (
    ScrapeRoleHost      = "host"       // 替代 embedded 快路径
    ScrapeRoleComponent = "component"  // 仅贡献 rich-path samples
)

// ScrapeTarget 描述一个 /metrics URL
type ScrapeTarget struct {
    Name            string            `yaml:"name"`              // operator-chosen id; Source = "scrape:<Name>"
    URL             string            `yaml:"url"`               // 绝对 /metrics URL
    Role            string            `yaml:"role,omitempty"`    // host / component；默认 component
    Interval        time.Duration     `yaml:"interval"`          // 默认 30s
    Timeout         time.Duration     `yaml:"timeout"`           // 默认 10s
    BearerTokenFile string            `yaml:"bearer_token_file,omitempty"`  // Authorization: Bearer <file content>
    TLSInsecure     bool              `yaml:"tls_insecure,omitempty"`       // 跳过 TLS 校验
    StaticLabels    map[string]string `yaml:"static_labels,omitempty"`      // 合并到每个 sample；producer labels 优先
}
```

## 4. 关键函数与流程

### `LoadScrapeConfig`
- **签名**：`func LoadScrapeConfig(path string) (*ScrapeConfig, error)`
- **职责**：读 + 解析 + 默认值 + 校验
- **流程**：
  1. `os.ReadFile(path)` 失败返回 `fmt.Errorf("read scrape config %q: %w", path, err)`
  2. `yaml.Unmarshal` 到 `ScrapeConfig`
  3. `len(Targets)==0` 报错「no targets」
  4. 遍历每个 target：
     - `Name == ""` 报错「target #N missing name」
     - `URL == ""` 报错「target "name" missing url」
     - `Role` TrimSpace；空时默认 `ScrapeRoleComponent`
     - `Role` 必须是 `host` / `component`，否则报错
     - `Interval <= 0` 默认 30s
     - `Timeout <= 0` 默认 10s
- **错误处理**：所有错误带 path + target 名便于定位

## 5. 依赖关系

- **内部包**：无
- **外部库**：`gopkg.in/yaml.v3`、标准库 `fmt`、`os`、`strings`、`time`
- **被调用方**：`cmd/ongrid-edge` 主程序

## 6. 并发与资源管理

无并发控制。`ScrapeConfig` 是不可变数据结构，构造后只读；多个 Scraper goroutine 并发读 `Targets` 安全。

## 7. 设计模式与亮点

- **YAML schema 简洁**：仅 `targets` 顶层字段，每 target 字段都有合理默认；operator 只需写最小配置
- **role 区分**：`host` 替代 embedded 快路径（dashboard/alert 用）；`component` 仅 rich path samples——让 operator 灵活组合多目标
- **StaticLabels 合并语义**：scrape 时合并到每个 sample，但 producer labels 优先（在 `mapper.go::mergedLabels` 实现）；防止 static_labels 覆盖真实 metric labels
- **BearerTokenFile 而非 inline token**：token 不进配置文件，避免泄露；从文件读支持轮换
- **TLSInsecure 显式 opt-in**：默认 false（安全）；operator 显式设 true 才跳过校验
- **默认值在加载时应用**：而非在 Scraper 运行时——让 `Targets` 字段在构造后就是完整可用的，避免运行时检查
- **校验失败带上下文**：错误消息含 path + target 名 + 字段，便于 operator 定位配置错误

## 8. 注意事项

- `Role` 校验是字符串相等——未来新增 role（如 `network-device`）需在此处加 case
- `Interval` / `Timeout` 是 `time.Duration`，YAML 中需用 `30s` / `10s` 等字符串；operator 误写 `30` 会被解析为 30 纳秒
- `BearerTokenFile` 路径是 edge 本地路径——容器化部署时需挂载 token 文件
- `StaticLabels` 是 `map[string]string`——YAML 中写 `static_labels: {env: prod, region: us-east}`；key/value 都必须是字符串
- `LoadScrapeConfig` 不校验 URL 可达性——只在 scrape 时发现；operator 应在部署后监控 scrape 日志
- 同名 target 会被 `s.snapshot[t.Name]` 覆盖——`LoadScrapeConfig` 不校验 name 唯一性；operator 需自行保证
- `TLSInsecure: true` 跳过整条证书链校验——仅用于自签 kubelet 等场景；生产应配正式证书
