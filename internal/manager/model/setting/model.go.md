# `model.go` 技术实现文档

> 源文件：`internal/manager/model/setting/model.go`
> 包路径：`github.com/ongridio/ongrid/internal/manager/model/setting`

## 1. 概述

本文件定义 `Setting` 实体与系统级常量集合：`system_settings` key/value 表支撑 DB 驱动的运行时配置（admin 通过 UI 编辑无需重启 manager，如 LLM 凭据 / 模型 / base URL）。设计要点：flat / 单租户；多租户落地时 uniqueness 应扩为 (org_id, category, key)，当前 shape forward-compatible 因 Category 可按 feature area 命名空间（llm / notification / ...）。红线：`Sensitive=true` 行的 Value 永不应在 list endpoint 明文返回；server/setting/http.go 的 default-mask policy 按 `*_api_key` / `*_secret` / `*_token` / `*_password` 后缀自动识别敏感（无需 allowlist）；MySQL 拒 TEXT DEFAULT（Error 1101），故 Value 无 default 但 biz 层 Set 总写值。

## 2. 包信息

- **包名**：`setting`
- **所属模块**：`internal/manager/model/`
- **依赖方向**：被 `manager/biz/setting` 与 `internal/pkg/llm`、`internal/pkg/promauth`、Grafana API client 等读取；依赖 `time`

## 3. 关键类型与接口

```go
type Setting struct {
    ID        uint64    `gorm:"primaryKey;autoIncrement"`
    Category  string    `gorm:"size:32;not null;uniqueIndex:idx_settings_cat_key,priority:1"`
    Key       string    `gorm:"size:128;not null;uniqueIndex:idx_settings_cat_key,priority:2"`
    // MySQL 拒 TEXT DEFAULT；Go 零值已 ""，biz 层 Set 总显式写入
    Value     string    `gorm:"type:text;not null"`
    Sensitive bool      `gorm:"not null;default:false"`
    UpdatedAt time.Time `gorm:"autoUpdateTime"`
    CreatedAt time.Time `gorm:"autoCreateTime"`
}

// Category 常量
const (
    CategoryLLM       = "llm"
    CategoryProm      = "prom"
    CategoryGrafana   = "grafana"
    CategoryLoki      = "loki"
    CategoryTempo     = "tempo"
    CategoryWebSearch = "websearch"
    CategoryAgent     = "agent"
)

// CategoryAgent 下已知 key
const (
    KeyAgentWriteEnabled = "write_enabled" // 仅 "true" 启用 write tools
)

// CategoryLLM 下已知 key（节选）
const (
    KeyOpenAIAPIKey       = "openai_api_key"
    KeyOpenAIModel        = "openai_model"
    KeyOpenAIBaseURL      = "openai_base_url"
    KeyOpenAIModels       = "openai_models"
    KeyOpenAIDefaultModel = "openai_default_model"

    KeyAnthropicAPIKey       = "anthropic_api_key"
    KeyAnthropicBaseURL      = "anthropic_base_url"
    KeyAnthropicModels       = "anthropic_models"
    KeyAnthropicDefaultModel = "anthropic_default_model"

    KeyZhipuAPIKey       = "zhipu_api_key"
    KeyZhipuBaseURL      = "zhipu_base_url"
    KeyZhipuModels       = "zhipu_models"
    KeyZhipuDefaultModel = "zhipu_default_model"

    KeyGeminiAPIKey       = "gemini_api_key"
    KeyGeminiBaseURL      = "gemini_base_url"
    KeyGeminiModels       = "gemini_models"
    KeyGeminiDefaultModel = "gemini_default_model"

    KeyDeepSeekAPIKey       = "deepseek_api_key"
    KeyDeepSeekBaseURL      = "deepseek_base_url"
    KeyDeepSeekModels       = "deepseek_models"
    KeyDeepSeekDefaultModel = "deepseek_default_model"

    KeyKimiAPIKey       = "kimi_api_key"
    KeyKimiBaseURL      = "kimi_base_url"
    KeyKimiModels       = "kimi_models"
    KeyKimiDefaultModel = "kimi_default_model"

    KeyCustomAPIKey       = "custom_api_key"
    KeyCustomBaseURL      = "custom_base_url"
    KeyCustomModels       = "custom_models"
    KeyCustomDefaultModel = "custom_default_model"

    KeyLLMDefaultProvider = "default_provider"
)

// LLM Provider ID 枚举
const (
    LLMProviderOpenAI    = "openai"
    LLMProviderAnthropic = "anthropic"
    LLMProviderZhipu     = "zhipu"
    LLMProviderGemini    = "gemini"
    LLMProviderDeepSeek  = "deepseek"
    LLMProviderKimi      = "kimi"
    LLMProviderCustom    = "custom"
)

// CategoryProm 下已知 key
const (
    KeyPromQueryURL       = "query_url"
    KeyPromRemoteWriteURL = "remote_write_url"
    KeyPromBearerToken    = "bearer_token"
    KeyPromBasicUser      = "basic_user"
    KeyPromBasicPassword  = "basic_password"
    KeyPromTLSInsecure    = "tls_insecure"
    KeyPromTLSCAPEM       = "tls_ca_pem"
)

// CategoryGrafana 下已知 key
const (
    KeyGrafanaRootURL = "root_url"
    KeyGrafanaSAToken = "sa_token"
    KeyGrafanaAPIKey  = "api_key"
    KeyGrafanaOrgID   = "org_id"
)

// CategoryLoki / CategoryTempo 下已知 key
const (
    KeyLokiURL           = "url"
    KeyLokiBasicUser     = "basic_user"
    KeyLokiBasicPassword = "basic_password"
    KeyLokiTLSInsecure   = "tls_insecure"

    KeyTempoURL           = "url"
    KeyTempoBasicUser     = "basic_user"
    KeyTempoBasicPassword = "basic_password"
    KeyTempoTLSInsecure   = "tls_insecure"
)

// CategoryWebSearch 下已知 key
const (
    KeyWebSearchProvider = "provider"
    KeySearxngURL        = "searxng_url"
    KeyTavilyAPIKey      = "tavily_api_key"
    KeyBraveAPIKey       = "brave_api_key"
)

// WebSearch Provider 名
const (
    ProviderSearxng = "searxng"
    ProviderTavily  = "tavily"
    ProviderBrave   = "brave"
)

const DefaultSearxngURL = "http://searxng:8080"
```

## 4. 关键函数与流程

### `Setting.TableName`
- **签名**：`func (Setting) TableName() string`
- **职责**：固定表名 `system_settings`，避免包重命名后误创建新 schema

## 5. 依赖关系

- **内部包**：被 `internal/pkg/llm`（LLMSettingsResolver 读 LLM key）、`internal/pkg/promauth`（读 Prom key）、Grafana API client（读 Grafana key）、`manager/biz/edge` 的 PluginConfigUC（读 Loki/Tempo key）、`internal/skill/builtin/web_search.go`（读 WebSearch key）等
- **外部库**：`time`
- **被调用方**：`manager/biz/setting` 的 CRUD service；上述所有 reader

## 6. 并发与资源管理

- 本文件仅定义 schema，无锁 / channel / 缓存
- 无软删（permanent audit）；如需删除由 biz 层显式 DELETE
- `Value` 列读写并发由 biz 层保证（admin 编辑 + reader 读取）

## 7. 设计模式与亮点

- **DB 驱动运行时配置**：admin 通过 UI 编辑无需重启 manager
- **(Category, Key) 联合唯一**：跨未软删行；Category 按 feature area 命名空间
- **forward-compatible 多租户**：未来扩 (org_id, category, key) unique 不破坏当前 shape
- **Sensitive 标记**：list endpoint 不返回明文 Value；service 层负责 mask
- **default-mask policy 后缀匹配**：`*_api_key` / `*_secret` / `*_token` / `*_password` 自动识别敏感，无需 allowlist
- **MySQL TEXT DEFAULT 兼容**：Value 无 default；Go 零值 "" 已满足 NOT NULL
- **7 个 LLM Provider key 集合**：openai / anthropic / zhipu / gemini / deepseek / kimi / custom；每 provider 4 key（api_key/base_url/models/default_model）
- **legacy openai_* 三元组保留**：LLMSettingsResolver fallback 到 legacy `openai_model` 当 `openai_default_model` 为空
- **Custom = 通用 OpenAI-compatible**：Ollama / vLLM / OpenRouter / LM Studio / Together / Groq / 自托管 gateway；无默认 endpoint，base_url 必填
- **KeyLLMDefaultProvider 集群默认**：空 = 字母序首个 provider
- **CategoryProm URL 启动读**：URLs 在 startup 读（env seed → DB）；变更需重启 manager
- **CategoryProm bearer/basic 每请求读**：`internal/pkg/promauth` 每请求读，无需重启
- **CategoryGrafana SAToken vs APIKey**：SAToken 是 bootstrap 为内嵌 Grafana mint；APIKey 是 operator 粘贴的外部 Grafana bearer；语义相同（都 feed Authorization: Bearer）
- **CategoryGrafana OrgID**：mirror 浏览器 observability store 值；dashboard-fetch proxy 默认值
- **CategoryLoki/Tempo 镜像**：URL 是 OTLP HTTP push endpoint（Tempo）或 push URL（Loki）
- **CategoryWebSearch 三 provider**：searxng（自托管，零配置默认）/ tavily（1000 免费 / 月）/ brave（2000 免费 / 月）
- **DefaultSearxngURL docker-internal**：`http://searxng:8080`，docker-compose 内的服务名

## 8. 注意事项

- **(Category, Key) 唯一**：跨所有行；rename key 需先删后建
- **Value 必填**：biz 总写值；MySQL 拒 TEXT DEFAULT
- **Sensitive 标记**：list endpoint 必须 mask；server/setting/http.go 按 `*_api_key` / `*_secret` / `*_token` / `*_password` 后缀自动识别
- **legacy openai_* 保留**：现有部署迁移时存活；新部署建议用 per-provider key
- **Custom base_url 必填**：无默认 endpoint；未提供时 resolver 跳过
- **KeyLLMDefaultProvider 空**：字母序首个 provider
- **CategoryProm URL 启动读**：变更需重启 manager
- **CategoryProm bearer/basic 每请求读**：UI 编辑后立即生效
- **CategoryGrafana SAToken vs APIKey 二选一**：两者都 feed Authorization: Bearer；外部 Grafana 通常用 APIKey
- **CategoryGrafana OrgID**：dashboard-fetch proxy 默认
- **CategoryLoki/Tempo URL 空**：fallback 到 env-seeded default（ONGRID_LOG_URL / ONGRID_TEMPO_URL）
- **CategoryWebSearch Provider 空**：fallback 到 searxng（零配置基线）
- **CategoryWebSearch Tavily/Brave opt-in**：需 UI 配 KeySearxngURL 之外再配 API key
- **CategoryAgent KeyAgentWriteEnabled**：仅 "true" 启用 write tools；其他值（包括 unset）保持 read-only
- **Removed KeyGitGitHubToken**：legacy `git.github_token` 行现为 dead data；可安全 DELETE；HTTPS git auth 将经 credential.helper-backed table 返回（P3）；SSH 在 ssh_identities
