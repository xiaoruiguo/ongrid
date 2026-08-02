# OnGrid 系统配置说明文档

> 本文档深入分析 OnGrid 系统的配置实现，覆盖环境变量、`.env.example` 模板、Config struct、加载机制、部署差异、敏感配置与配置优先级层级。所有引用均锚定到具体文件路径与行号，便于源码跳转。

---

## 目录

1. [配置总览](#1-配置总览)
2. [`.env.example` 三层模板](#2-env-example-三层模板)
3. [Config 顶层结构](#3-config-顶层结构)
4. [加载机制与辅助函数](#4-加载机制与辅助函数)
5. [数据库配置（DBConfig）](#5-数据库配置dbconfig)
6. [JWT 与认证配置](#6-jwt-与认证配置)
7. [Admin 引导配置](#7-admin-引导配置)
8. [OpenAI 与多 LLM Provider 配置](#8-openai-与多-llm-provider-配置)
9. [Edge Agent 配置](#9-edge-agent-配置)
10. [Frontier Client 配置](#10-frontier-client-配置)
11. [Prometheus 配置](#11-prometheus-配置)
12. [Grafana 配置](#12-grafana-配置)
13. [Notification 配置](#13-notification-配置)
14. [Alert 配置](#14-alert-配置)
15. [Logs / Traces 配置](#15-logs--traces-配置)
16. [Skills 配置](#16-skills-配置)
17. [Kubernetes Event 三件套](#17-kubernetes-event-三件套)
18. [Embedding 配置](#18-embedding-配置)
19. [Qdrant 配置](#19-qdrant-配置)
20. [Knowledge Base 配置](#20-knowledge-base-配置)
21. [Investigator 配置（HLD-011）](#21-investigator-配置hld-011)
22. [Marketplace 配置](#22-marketplace-配置)
23. [Workspace / Skills Roots 配置](#23-workspace--skills-roots-配置)
24. [Secretbox 与 ONGRID_SECRET_KEY](#24-secretbox-与-ongrid_secret_key)
25. [OTel / Audit / Flow / Pages 杂项](#25-otel--audit--flow--pages-杂项)
26. [Edge Agent 独立配置（cmd/ongrid-edge）](#26-edge-agent-独立配置cmdongrid-edge)
27. [Edge Plugin 配置（双路 fetcher）](#27-edge-plugin-配置双路-fetcher)
28. [部署差异：Dev vs Install](#28-部署差异dev-vs-install)
29. [配置优先级层级](#29-配置优先级层级)
30. [敏感配置清单](#30-敏感配置清单)
31. [架构红线与注意事项](#31-架构红线与注意事项)
32. [附录：完整 env 变量索引](#32-附录完整-env-变量索引)

---

## 1. 配置总览

### 1.1 设计哲学

OnGrid 在 MVP 阶段采用极简配置策略：**所有配置从环境变量读取 + 合理默认值**，不依赖 YAML 文件、不引入 viper 等配置框架。这降低了部署心智负担，所有配置可以通过 `.env` 文件或 docker-compose `environment:` 段注入。

源码注释明确指出这一设计决策（[config.go#L1-L9](file:///d:/claude/ongrid/internal/pkg/config/config.go#L1-L9)）：

```go
// Package config loads OnGrid manager configuration from environment variables.
//
// MVP design: plain os.Getenv + sensible defaults (no YAML/viper dep yet).
// All settings can be injected via .env file or docker-compose environment.
```

### 1.2 配置来源分层

OnGrid 配置实际有三种来源，按"生命周期"分层：

| 层 | 来源 | 何时固定 | 例子 |
|---|---|---|---|
| **启动时固定** | env → `Config` struct | 进程启动 | `ONGRID_HTTP_ADDR`、`ONGRID_DB_DSN` |
| **first-boot seed** | env → `system_settings` 表 | 首次启动写入，之后由 DB 主导 | `ONGRID_OPENAI_API_KEY`（首次写入 DB 后由 UI 管理） |
| **运行时可变** | UI → `system_settings` 表 | 实时（5s/60s 刷新） | LLM provider 配置、Prometheus Bearer/Basic |

### 1.3 涉及的关键文件

| 文件 | 作用 |
|---|---|
| [internal/pkg/config/config.go](file:///d:/claude/ongrid/internal/pkg/config/config.go) | Config struct 定义 + `Load()` 入口 + 6 个 helper |
| [.env.example](file:///d:/claude/ongrid/.env.example) | 根目录基础模板（102 行） |
| [deploy/.env.example](file:///d:/claude/ongrid/deploy/.env.example) | 本地 dev compose 模板（45 行） |
| [deploy/install/.env.example](file:///d:/claude/ongrid/deploy/install/.env.example) | 生产安装模板（206 行，最全） |
| [cmd/ongrid/main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go) | Manager 主入口，读取 config.go 之外的 env |
| [cmd/ongrid-edge/main.go](file:///d:/claude/ongrid/cmd/ongrid-edge/main.go) | Edge Agent 主入口，读取所有 `ONGRID_EDGE_*` / `ONGRID_K8S_*` |
| [internal/pkg/secretbox/secretbox.go](file:///d:/claude/ongrid/internal/pkg/secretbox/secretbox.go) | AES-256-GCM at-rest 加密 + `ONGRID_SECRET_KEY` |
| [internal/pkg/embedding/local.go](file:///d:/claude/ongrid/internal/pkg/embedding/local.go) | 本地 ONNX embedder + `ONGRID_EMBEDDING_*` |
| [internal/edgeagent/plugins/config_env.go](file:///d:/claude/ongrid/internal/edgeagent/plugins/config_env.go) | EnvConfigFetcher（edge plugin env bootstrap） |
| [internal/edgeagent/plugins/config_tunnel.go](file:///d:/claude/ongrid/internal/edgeagent/plugins/config_tunnel.go) | TunnelConfigFetcher（edge plugin 主路径 RPC + env fallback） |
| [deploy/install/install.sh](file:///d:/claude/ongrid/deploy/install/install.sh) | 生产安装脚本，自动生成 MYSQL_PASSWORD / JWT_SECRET / ADMIN_PASSWORD |

---

## 2. `.env.example` 三层模板

OnGrid 提供三个层级的 `.env.example` 模板，对应不同部署场景：

### 2.1 根目录 `.env.example`（基础模板，102 行）

文件：[.env.example](file:///d:/claude/ongrid/.env.example)

定位：源码开发场景的最小配置集，包含 DB、HTTP、Frontier、Admin、JWT、OpenAI、Prometheus、Alert、Notification、Edge 等基础配置。

关键配置项：

```env
# L22 - Frontier 地址（容器内 DNS）
ONGRID_FRONTIER_ADDR=frontier:40011

# L61 - Alert load1 阈值（0 = 禁用）
ONGRID_ALERT_LOAD1=0

# L65 - Notification 默认关闭
ONGRID_NOTIFY_ENABLED=false

# L70 - Notify 日志通道默认开（用于调试）
ONGRID_NOTIFY_LOG_ENABLED=true

# L97-102 - Edge 基础配置
ONGRID_EDGE_CLOUD_ADDR=127.0.0.1:40012
ONGRID_EDGE_ACCESS_KEY=
ONGRID_EDGE_SECRET_KEY=
```

### 2.2 `deploy/.env.example`（dev compose 模板，45 行）

文件：[deploy/.env.example](file:///d:/claude/ongrid/deploy/.env.example)

定位：本地 docker-compose 开发栈，注释明确标注"LOCAL DEV convenience stack: no TLS, trivial DB password"（[L7-L9](file:///d:/claude/ongrid/deploy/.env.example#L7-L9)）。

包含 Admin、JWT、OpenAI、K8s Event 三件套、Grafana、Embedding（默认 local `bge-small-zh-v1.5`）等关键配置。

### 2.3 `deploy/install/.env.example`（生产安装模板，206 行，最全）

文件：[deploy/install/.env.example](file:///d:/claude/ongrid/deploy/install/.env.example)

定位：生产部署的最完整模板，包含所有可用 env 变量及详细注释。关键配置：

```env
# L6
ONGRID_VERSION=v0.7.113

# L12-13 - 数据/日志目录（v0.7.45+ 使用 host bind-mount）
ONGRID_DATA_DIR=/var/lib/ongrid
ONGRID_LOG_DIR=/var/log/ongrid

# L20-24 - 端口分层（容器内 vs 主机）
ONGRID_HTTP_PORT=443
ONGRID_HTTP_REDIRECT_PORT=80
ONGRID_TUNNEL_PORT=40012
ONGRID_METRICS_PORT=9100
PROM_PORT=9090

# L30 - Docker 兼容性（旧 Docker 不支持 host-gateway）
ONGRID_HOST_GATEWAY=

# L33-34 - MySQL 密码（install.sh 自动生成）
MYSQL_PASSWORD=

# L42 - Grafana admin 密码（install.sh 自动生成）
GRAFANA_ADMIN_PASSWORD=

# L45-47 - JWT（生产 720h refresh；install.sh 生成 64-char secret）
ONGRID_JWT_REFRESH_TTL=720h
ONGRID_JWT_SECRET=

# L52-53 - Admin 密码（install.sh 生成 20-char 密码）
ONGRID_ADMIN_PASSWORD=

# L73-75 - OpenAI（注释：硬编码值会 leak back，建议留空）
ONGRID_OPENAI_API_KEY=
ONGRID_OPENAI_MODEL=
ONGRID_OPENAI_BASE_URL=

# L82-83 - Investigator（HLD-011 自动 RCA）
ONGRID_INVESTIGATOR_ENABLED=true
ONGRID_INVESTIGATOR_MAX_CONCURRENT=5

# L85-115 - Embedding provider 选择说明
# local: BGE-small-zh-v1.5 ONNX，dim=512，离线
# openai: 兼容 OpenAI/GLM/Qwen/DeepSeek，dim=1536
ONGRID_EMBEDDING_PROVIDER=local
ONGRID_EMBEDDING_DIM=512

# L118-119 - Advanced
ONGRID_HOST_GATEWAY=
ONGRID_DOCKER_COMPAT=

# L126-127 - Frontier
ONGRID_FRONTIER_ADDR=frontier:40011
ONGRID_FRONTIER_SERVICE_NAME=ongrid-manager

# L129-149 - Prom（TLS + UI 管理 Bearer/Basic）
ONGRID_PROM_ENABLED=true
ONGRID_PROM_TLS_INSECURE=false
ONGRID_PROM_TLS_CA_PATH=

# L155 - Grafana InternalURL
ONGRID_GRAFANA_INTERNAL_ROOT_URL=http://grafana:3000/grafana

# L157-172 - Alert（含 ONGRID_ALERT_EVAL_INTERVAL 注释：30s→5m 变更历史）
ONGRID_ALERT_EVAL_INTERVAL=5m

# L174-206 - Notify
ONGRID_NOTIFY_ENABLED=true
ONGRID_NOTIFY_TIMEOUT=10s
```

### 2.4 三层模板关系

```
.env.example (102 行，基础)
    ↓ 增强
deploy/.env.example (45 行，dev compose)
    ↓ 完整化
deploy/install/.env.example (206 行，生产)
```

- **基础模板**：源码开发最小集
- **dev compose**：本地一键起容器栈
- **install 模板**：生产部署完整集，由 `install.sh` 自动生成密码

---

## 3. Config 顶层结构

源码：[internal/pkg/config/config.go#L20-L62](file:///d:/claude/ongrid/internal/pkg/config/config.go#L20-L62)

```go
type Config struct {
    HTTPAddr    string        // :8080
    MetricsAddr string        // :9100
    TunnelAddr  string        // :40012
    PublicURL   string        // http://localhost:8080

    K8sEventRetention        time.Duration // 24h
    K8sEventMaxPerCluster    int           // 5000
    K8sEventCleanupInterval  time.Duration // 1h

    DB             DBConfig
    JWT            JWTConfig
    OpenAI         OpenAIConfig
    LLM            LLMConfig
    Admin          AdminConfig
    Edge           EdgeConfig
    FrontierClient FrontierClientConfig
    Prom           PromConfig
    Grafana        GrafanaConfig
    Notification   NotificationConfig
    Alert          AlertConfig
    Logs           LogsConfig
    Traces         TracesConfig
    Skills         SkillsConfig
}
```

顶层 `Config` 包含 **4 个独立字段**（HTTP/Metrics/Tunnel/PublicURL）+ **K8s Event 三件套** + **14 个子结构**。

### 3.1 14 个子结构概览

| 子结构 | 文件行号 | 作用 |
|---|---|---|
| `DBConfig` | [L267-L277](file:///d:/claude/ongrid/internal/pkg/config/config.go#L267-L277) | MySQL/SQLite 双后端 |
| `JWTConfig` | [L280-L284](file:///d:/claude/ongrid/internal/pkg/config/config.go#L280-L284) | JWT 签名密钥与 TTL |
| `OpenAIConfig` | [L287-L291](file:///d:/claude/ongrid/internal/pkg/config/config.go#L287-L291) | OpenAI 兼容 API（独立字段） |
| `LLMConfig` | [L308-L325](file:///d:/claude/ongrid/internal/pkg/config/config.go#L308-L325) | 多 provider 集群 + Default + DailyTokenLimit |
| `AdminConfig` | [L330-L333](file:///d:/claude/ongrid/internal/pkg/config/config.go#L330-L333) | Admin 引导账号 |
| `EdgeConfig` | [L336-L369](file:///d:/claude/ongrid/internal/pkg/config/config.go#L336-L369) | Edge Agent 配置 |
| `FrontierClientConfig` | [L249-L262](file:///d:/claude/ongrid/internal/pkg/config/config.go#L249-L262) | Manager → Frontier 连接 |
| `PromConfig` | [L220-L244](file:///d:/claude/ongrid/internal/pkg/config/config.go#L220-L244) | Prometheus 集成 |
| `GrafanaConfig` | [L108-L133](file:///d:/claude/ongrid/internal/pkg/config/config.go#L108-L133) | Grafana 嵌入 |
| `NotificationConfig` | [L141-L160](file:///d:/claude/ongrid/internal/pkg/config/config.go#L141-L160) | 告警通知 |
| `AlertConfig` | [L175-L210](file:///d:/claude/ongrid/internal/pkg/config/config.go#L175-L210) | 内置告警引擎 |
| `LogsConfig` | [L84-L89](file:///d:/claude/ongrid/internal/pkg/config/config.go#L84-L89) | Loki 日志查询 |
| `TracesConfig` | [L96-L102](file:///d:/claude/ongrid/internal/pkg/config/config.go#L96-L102) | Tempo 链路查询 |
| `SkillsConfig` | [L71-L78](file:///d:/claude/ongrid/internal/pkg/config/config.go#L71-L78) | Skills 外部目录 |

### 3.2 K8s Event 三件套

源码：[config.go#L380-L382](file:///d:/claude/ongrid/internal/pkg/config/config.go#L380-L382)

```go
K8sEventRetention:        getEnvDuration("ONGRID_K8S_EVENT_RETENTION", 24*time.Hour),
K8sEventMaxPerCluster:    getEnvInt("ONGRID_K8S_EVENT_MAX_PER_CLUSTER", 5000),
K8sEventCleanupInterval:  getEnvDuration("ONGRID_K8S_EVENT_CLEANUP_INTERVAL", time.Hour),
```

- `Retention=24h`：K8s 事件保留时长
- `MaxPerCluster=5000`：单集群最大事件数
- `CleanupInterval=1h`：清理循环周期

---

## 4. 加载机制与辅助函数

### 4.1 `Load()` 入口

源码：[config.go#L374-L494](file:///d:/claude/ongrid/internal/pkg/config/config.go#L374-L494)

`Load()` 是配置加载的唯一入口，依次填充所有字段。函数体纯 env 读取，无文件 IO、无 watch。所有字段的默认值在函数内联定义，便于审计。

### 4.2 六个 helper 函数

源码：[config.go#L498-L598](file:///d:/claude/ongrid/internal/pkg/config/config.go#L498-L598)

| 函数 | 行号 | 作用 |
|---|---|---|
| `getEnv` | [L498-L510](file:///d:/claude/ongrid/internal/pkg/config/config.go#L498-L510) | 字符串读取，空返回默认值 |
| `getEnvBool` | [L512-L518](file:///d:/claude/ongrid/internal/pkg/config/config.go#L512-L518) | 布尔解析（"true"/"1"/"yes" 为 true） |
| `splitProviderModels` | [L519-L540](file:///d:/claude/ongrid/internal/pkg/config/config.go#L519-L540) | 逗号/分号分隔 + 去重 |
| `getEnvCSV` | [L542-L561](file:///d:/claude/ongrid/internal/pkg/config/config.go#L542-L561) | CSV 列表解析 |
| `getEnvInt` | [L563-L572](file:///d:/claude/ongrid/internal/pkg/config/config.go#L563-L572) | 整数解析 |
| `getEnvFloat` | [L574-L583](file:///d:/claude/ongrid/internal/pkg/config/config.go#L574-L583) | 浮点解析 |
| `getEnvDuration` | [L585-L598](file:///d:/claude/ongrid/internal/pkg/config/config.go#L585-L598) | 时长解析（Go duration + 整数秒回退） |

**`getEnvDuration` 的整数秒回退**（[L594-L597](file:///d:/claude/ongrid/internal/pkg/config/config.go#L594-L597)）：

```go
// 如果不是 Go duration 格式（如 "5m"），尝试当整数秒解析
if secs, err := strconv.Atoi(s); err == nil {
    return time.Duration(secs) * time.Second
}
```

这意味着 `ONGRID_NOTIFY_TIMEOUT=10` 与 `ONGRID_NOTIFY_TIMEOUT=10s` 等价。

### 4.3 `splitProviderModels` 去重逻辑

源码：[config.go#L519-L540](file:///d:/claude/ongrid/internal/pkg/config/config.go#L519-L540)

```go
func splitProviderModels(s string) []string {
    // 同时支持逗号和分号分隔
    parts := strings.FieldsFunc(s, func(r rune) bool {
        return r == ',' || r == ';'
    })
    // 去重 + 去空白
    seen := make(map[string]struct{})
    out := []string{}
    for _, p := range parts {
        p = strings.TrimSpace(p)
        if p == "" {
            continue
        }
        if _, ok := seen[p]; ok {
            continue
        }
        seen[p] = struct{}{}
        out = append(out, p)
    }
    return out
}
```

支持 `ONGRID_OPENAI_MODELS=gpt-4o,gpt-4o-mini;gpt-4-turbo` 这种混合分隔的写法。

---

## 5. 数据库配置（DBConfig）

源码：[config.go#L267-L277](file:///d:/claude/ongrid/internal/pkg/config/config.go#L267-L277)

```go
type DBConfig struct {
    Dialect string // mysql（默认）或 sqlite
    DSN     string // MySQL DSN
    Path    string // SQLite 文件路径
}
```

### 5.1 字段读取

源码：[config.go#L386-L391](file:///d:/claude/ongrid/internal/pkg/config/config.go#L386-L391)

```go
DB: DBConfig{
    Dialect: getEnv("ONGRID_DB_DIALECT", ""),
    DSN:     getEnv("ONGRID_DB_DSN", ""),
    Path:    getEnv("ONGRID_DB_PATH", "./data/ongrid.db"),
},
```

### 5.2 Dialect 防御性回退

`Dialect` 默认空字符串，在 DB 打开逻辑中回退到 `mysql`：

```go
if cfg.DB.Dialect == "" {
    cfg.DB.Dialect = "mysql"  // 防御性回退
}
```

### 5.3 双后端语义

| Dialect | 必填字段 | 用途 |
|---|---|---|
| `mysql`（默认） | `ONGRID_DB_DSN` | 生产环境 |
| `sqlite` | `ONGRID_DB_PATH` | 单机测试 / air-gapped |

`ONGRID_DB_PATH` 默认 `./data/ongrid.db`，相对路径相对进程 CWD。

---

## 6. JWT 与认证配置

源码：[config.go#L280-L284](file:///d:/claude/ongrid/internal/pkg/config/config.go#L280-L284)

```go
type JWTConfig struct {
    Secret     string
    AccessTTL  time.Duration
    RefreshTTL time.Duration
}
```

### 6.1 字段读取

源码：[config.go#L399-L403](file:///d:/claude/ongrid/internal/pkg/config/config.go#L399-L403)

```go
JWT: JWTConfig{
    Secret:     getEnv("ONGRID_JWT_SECRET", "dev-insecure-secret-change-me"),
    AccessTTL:  getEnvDuration("ONGRID_JWT_ACCESS_TTL", 15*time.Minute),
    RefreshTTL: getEnvDuration("ONGRID_JWT_REFRESH_TTL", 168*time.Hour),
},
```

### 6.2 默认值与生产强化

| 字段 | 默认值 | 生产推荐 |
|---|---|---|
| `Secret` | `dev-insecure-secret-change-me` | install.sh 生成 64-char 随机串 |
| `AccessTTL` | `15m` | 15m（保持默认） |
| `RefreshTTL` | `168h`（7 天） | `720h`（30 天，见 install/.env.example [L45](file:///d:/claude/ongrid/deploy/install/.env.example#L45)） |

### 6.3 dev-secret fatal 检查

源码：[cmd/ongrid/main.go#L282-L286](file:///d:/claude/ongrid/cmd/ongrid/main.go#L282-L286)

```go
if cfg.JWT.Secret == "dev-insecure-secret-change-me" {
    log.Fatal("ONGRID_JWT_SECRET is using insecure default. Set ONGRID_JWT_SECRET env to a strong random value before running.")
}
```

**关键**：检测到默认 `dev-insecure-secret-change-me` 时，manager **拒绝启动**。dev 模式下也必须显式设置（哪怕是 `dev-123`）。

---

## 7. Admin 引导配置

源码：[config.go#L330-L333](file:///d:/claude/ongrid/internal/pkg/config/config.go#L330-L333)

```go
type AdminConfig struct {
    Email    string
    Password string
}
```

### 7.1 字段读取

```go
Admin: AdminConfig{
    Email:    getEnv("ONGRID_ADMIN_EMAIL", "admin@ongrid.local"),
    Password: getEnv("ONGRID_ADMIN_PASSWORD", ""),
},
```

### 7.2 Bootstrap 流程

源码：[cmd/ongrid/main.go#L291-L299](file:///d:/claude/ongrid/cmd/ongrid/main.go#L291-L299)

首次启动时，如果 `system_users` 表为空，使用 `Admin.Email` + `Admin.Password` 创建超管账号。密码为空时由 install.sh 生成 20-char 随机串（[install/.env.example#L52-L53](file:///d:/claude/ongrid/deploy/install/.env.example#L52-L53)）。

---

## 8. OpenAI 与多 LLM Provider 配置

OnGrid LLM 配置分两部分：OpenAI 独立字段 + 多 provider 集群。

### 8.1 OpenAI 独立字段

源码：[config.go#L287-L291](file:///d:/claude/ongrid/internal/pkg/config/config.go#L287-L291)

```go
type OpenAIConfig struct {
    APIKey  string
    Model   string
    BaseURL string
    Models  []string
}
```

读取：

```go
OpenAI: OpenAIConfig{
    APIKey:  getEnv("ONGRID_OPENAI_API_KEY", ""),
    Model:   getEnv("ONGRID_OPENAI_MODEL", "gpt-4o-mini"),
    BaseURL: getEnv("ONGRID_OPENAI_BASE_URL", ""),
    Models:  splitProviderModels(getEnv("ONGRID_OPENAI_MODELS", "")),
},
```

### 8.2 LLMConfig 多 provider 集群

源码：[config.go#L308-L325](file:///d:/claude/ongrid/internal/pkg/config/config.go#L308-L325)

```go
type LLMConfig struct {
    Anthropic LLMProviderConfig
    Zhipu     LLMProviderConfig
    Gemini    LLMProviderConfig
    DeepSeek  LLMProviderConfig
    Kimi      LLMProviderConfig

    Default         string
    DailyTokenLimit int
}

type LLMProviderConfig struct {
    APIKey  string
    Model   string
    BaseURL string
    Models  []string
}
```

### 8.3 空 APIKey 自动剔除

源码：[cmd/ongrid/main.go#L554-L595](file:///d:/claude/ongrid/cmd/ongrid/main.go#L554-L595)

在 MultiClient 装配时，每个 provider 用空 `APIKey` 自动从目录中剔除：

```go
providers := []ProviderSpec{}
if cfg.OpenAI.APIKey != "" {
    providers = append(providers, ProviderSpec{Name: "openai", ...})
}
if cfg.LLM.Anthropic.APIKey != "" {
    providers = append(providers, ProviderSpec{Name: "anthropic", ...})
}
// ... 其余 provider 同理
```

**设计意图**：用户只需配置实际使用的 provider，空 APIKey 的 provider 不会出现在 UI 选择列表中。

### 8.4 Default 与 DailyTokenLimit

```go
Default:         getEnv("ONGRID_LLM_DEFAULT", ""),
DailyTokenLimit: getEnvInt("ONGRID_LLM_DAILY_TOKEN_LIMIT", 0),
```

- `Default`：默认 provider 名（如 `openai`、`anthropic`）
- `DailyTokenLimit=0`：0 表示不限制

### 8.5 install/.env.example 的 leak back 警告

源码：[install/.env.example#L73-L75](file:///d:/claude/ongrid/deploy/install/.env.example#L73-L75)

```env
# ONGRID_OPENAI_API_KEY=
# ONGRID_OPENAI_MODEL=
# ONGRID_OPENAI_BASE_URL=
# 注：硬编码值会 leak back（首次启动写入 system_settings 后由 DB 主导），
# 建议留空，通过 UI 配置
```

**关键**：OpenAI 配置是 first-boot seed，首次启动后由 `system_settings` 表主导，env 修改不会生效（除非清空 DB）。

---

## 9. Edge Agent 配置

源码：[config.go#L336-L369](file:///d:/claude/ongrid/internal/pkg/config/config.go#L336-L369)

```go
type EdgeConfig struct {
    CloudAddr         string
    AccessKey         string
    SecretKey         string
    CollectorMode     string
    ScrapeConfigFile  string
    CollectorInterval time.Duration
}
```

### 9.1 字段读取

```go
Edge: EdgeConfig{
    CloudAddr:         getEnv("ONGRID_EDGE_CLOUD_ADDR", "127.0.0.1:40012"),
    AccessKey:         getEnv("ONGRID_EDGE_ACCESS_KEY", ""),
    SecretKey:         getEnv("ONGRID_EDGE_SECRET_KEY", ""),
    CollectorMode:     getEnv("ONGRID_EDGE_COLLECTOR_MODE", "off"),
    ScrapeConfigFile:  getEnv("ONGRID_EDGE_SCRAPE_CONFIG_FILE", "/etc/ongrid-edge/scrape.yaml"),
    CollectorInterval: getEnvDuration("ONGRID_EDGE_COLLECTOR_INTERVAL", 10*time.Second),
},
```

### 9.2 CollectorMode 演进

- 早期：`hostmetrics` 模式（edge agent 主动采集 host metrics）
- 当前：`off`（默认，host metrics 改用 scrape 模式，由 Prometheus 主动拉取）
- 配置文件：`ScrapeConfigFile=/etc/ongrid-edge/scrape.yaml`

### 9.3 边缘 binary 独立 env

Edge Agent 自己的 binary（`cmd/ongrid-edge/main.go`）读取独立的 `ONGRID_EDGE_*` env，与 manager 的 EdgeConfig 不同。详见 [第 26 节](#26-edge-agent-独立配置cmdongrid-edge)。

---

## 10. Frontier Client 配置

源码：[config.go#L249-L262](file:///d:/claude/ongrid/internal/pkg/config/config.go#L249-L262)

```go
type FrontierClientConfig struct {
    Addr        string
    ServiceName string
    Disabled    bool
}
```

### 10.1 字段读取

```go
FrontierClient: FrontierClientConfig{
    Addr:        getEnv("ONGRID_FRONTIER_ADDR", "frontier:40011"),
    ServiceName: getEnv("ONGRID_FRONTIER_SERVICE_NAME", "ongrid-manager"),
    Disabled:    getEnvBool("ONGRID_FRONTIER_DISABLED", false),
},
```

### 10.2 Disabled 用途

`Disabled=true` 用于 e2e 测试场景，跳过 Frontier 连接（避免依赖外部 broker）。生产环境必须为 `false`。

---

## 11. Prometheus 配置

源码：[config.go#L220-L244](file:///d:/claude/ongrid/internal/pkg/config/config.go#L220-L244)

```go
type PromConfig struct {
    Enabled      bool
    URL          string
    RemoteWriteURL string
    QueryURL     string
    TLSInsecure  bool
    TLSCAPath    string
}
```

### 11.1 字段读取

```go
Prom: PromConfig{
    Enabled:        getEnvBool("ONGRID_PROM_ENABLED", true),
    URL:            getEnv("ONGRID_PROM_URL", "http://prometheus:9090"),
    RemoteWriteURL: getEnv("ONGRID_PROM_REMOTE_WRITE_URL", ""),
    QueryURL:       getEnv("ONGRID_PROM_QUERY_URL", ""),
    TLSInsecure:    getEnvBool("ONGRID_PROM_TLS_INSECURE", false),
    TLSCAPath:      getEnv("ONGRID_PROM_TLS_CA_PATH", ""),
},
```

### 11.2 多 URL 语义

| 字段 | 用途 |
|---|---|
| `URL` | 默认 Prom 实例（query + remote_write 同址） |
| `RemoteWriteURL` | 兼容 Mimir/Cortex/Thanos 的远程写入端点 |
| `QueryURL` | 独立 query 端点（如读副本） |

### 11.3 TLS 配置

- `TLSInsecure=true`：跳过证书校验（自签证书场景）
- `TLSCAPath`：自定义 CA 证书路径

### 11.4 运行时 Bearer/Basic

**关键**：Bearer token / Basic auth 不在 env 中配置，而是通过 UI 在 `system_settings` 表中管理（5s/60s 刷新）。这是为了支持凭证轮转而不重启 manager。

详见 [install/.env.example#L129-L149](file:///d:/claude/ongrid/deploy/install/.env.example#L129-L149)。

---

## 12. Grafana 配置

源码：[config.go#L108-L133](file:///d:/claude/ongrid/internal/pkg/config/config.go#L108-L133)

```go
type GrafanaConfig struct {
    InternalRootURL   string
    BootstrapUser     string
    BootstrapPassword string
    TLSInsecure       bool
}
```

### 12.1 字段读取

```go
Grafana: GrafanaConfig{
    InternalRootURL:   getEnv("ONGRID_GRAFANA_INTERNAL_ROOT_URL", "http://grafana:3000/grafana"),
    BootstrapUser:     getEnv("ONGRID_GRAFANA_BOOTSTRAP_USER", "admin"),
    BootstrapPassword: getEnv("ONGRID_GRAFANA_BOOTSTRAP_PASSWORD", ""),
    TLSInsecure:       getEnvBool("ONGRID_GRAFANA_TLS_INSECURE", false),
},
```

### 12.2 InternalRootURL 用途

- **first-boot seed**：首次启动时通过 Grafana admin API 创建 Service Account + token
- 容器内地址：`http://grafana:3000/grafana`（Docker DNS）
- 主机浏览器地址：通过 nginx 反代

### 12.3 Bootstrap 凭证

`BootstrapUser` / `BootstrapPassword` 仅用于首次启动创建 SA，之后由 SA token 主导。`BootstrapPassword` 为空时 install.sh 自动生成（[install/.env.example#L42](file:///d:/claude/ongrid/deploy/install/.env.example#L42)）。

详细机制参考 [ongrid_grafana.md](file:///d:/claude/ongrid/ongrid_grafana.md)。

---

## 13. Notification 配置

源码：[config.go#L141-L160](file:///d:/claude/ongrid/internal/pkg/config/config.go#L141-L160)

```go
type NotificationConfig struct {
    Enabled         bool
    DefaultChannels []string
    Timeout         time.Duration
    LogEnabled      bool

    Webhook  NotifyWebhookConfig
    Slack    NotifyChannelConfig
    Feishu   NotifyChannelConfig
    DingTalk NotifyChannelConfig
}

type NotifyWebhookConfig struct {
    URL    string
    Secret string
}

type NotifyChannelConfig struct {
    WebhookURL string
    Secret     string
}
```

### 13.1 字段读取

```go
Notification: NotificationConfig{
    Enabled:         getEnvBool("ONGRID_NOTIFY_ENABLED", true),  // 默认开
    DefaultChannels: getEnvCSV("ONGRID_NOTIFY_DEFAULT_CHANNELS"),
    Timeout:         getEnvDuration("ONGRID_NOTIFY_TIMEOUT", 10*time.Second),
    LogEnabled:      getEnvBool("ONGRID_NOTIFY_LOG_ENABLED", false),
    // ... 4 个 channel
},
```

### 13.2 Enabled 默认语义

**关键差异**：
- `config.go` 默认 `true`（[L143](file:///d:/claude/ongrid/internal/pkg/config/config.go#L143)）
- `.env.example` 显式 `false`（[L65](file:///d:/claude/ongrid/.env.example#L65)）

含义：源码层默认开（配 channel 即投递），但根目录 dev 模板显式关闭，避免开发时干扰。

### 13.3 4 个 channel 类型

| Channel | 配置字段 | 用途 |
|---|---|---|
| `Webhook` | `URL` + `Secret` | 通用 webhook |
| `Slack` | `WebhookURL` + `Secret` | Slack incoming webhook |
| `Feishu` | `WebhookURL` + `Secret` | 飞书 incoming webhook |
| `DingTalk` | `WebhookURL` + `Secret` | 钉钉 incoming webhook |

---

## 14. Alert 配置

源码：[config.go#L175-L210](file:///d:/claude/ongrid/internal/pkg/config/config.go#L175-L210)

```go
type AlertConfig struct {
    Enabled            bool
    Cooldown           time.Duration
    CPUPercent         float64
    MemPercent         float64
    DiskUsedPercent    float64
    Load1              float64
    EvaluatorInterval  time.Duration
    EdgeOfflineThreshold time.Duration
    PromIngestFailLimit int
}
```

### 14.1 字段读取

```go
Alert: AlertConfig{
    Enabled:              getEnvBool("ONGRID_ALERT_ENABLED", true),
    Cooldown:             getEnvDuration("ONGRID_ALERT_COOLDOWN", 10*time.Minute),
    CPUPercent:           getEnvFloat("ONGRID_ALERT_CPU_PERCENT", 90),
    MemPercent:           getEnvFloat("ONGRID_ALERT_MEM_PERCENT", 90),
    DiskUsedPercent:      getEnvFloat("ONGRID_ALERT_DISK_USED_PERCENT", 90),
    Load1:                getEnvFloat("ONGRID_ALERT_LOAD1", 0),  // 0 = 禁用
    EvaluatorInterval:    getEnvDuration("ONGRID_ALERT_EVAL_INTERVAL", 5*time.Minute),
    EdgeOfflineThreshold: getEnvDuration("ONGRID_ALERT_EDGE_OFFLINE_THRESHOLD", 90*time.Second),
    PromIngestFailLimit:  getEnvInt("ONGRID_ALERT_PROM_INGEST_FAIL_LIMIT", 5),
},
```

### 14.2 EvaluatorInterval 变更历史

源码注释（[config.go#L191-L196](file:///d:/claude/ongrid/internal/pkg/config/config.go#L191-L196)）+ [install/.env.example#L157-L172](file:///d:/claude/ongrid/deploy/install/.env.example#L157-L172)：

```go
// EvaluatorInterval: 告警评估周期
// 历史：2026-05-31 前为 30s，导致规则堆积（3900 行/32h）
// 当前：5m（缓解规则爆炸）
// 生产建议：根据 alert 规则数量调整
```

**关键变更**：
- 2026-05-31 前：`30s`（过频，导致规则堆积 3900 行/32h）
- 2026-05-31 后：`5m`（默认，缓解规则爆炸）

### 14.3 阈值默认值

| 字段 | 默认 | 语义 |
|---|---|---|
| `CPUPercent` | 90 | CPU 使用率 % |
| `MemPercent` | 90 | 内存使用率 % |
| `DiskUsedPercent` | 90 | 磁盘使用率 % |
| `Load1` | 0 | 1 分钟负载（0 = 禁用，因不同机器核数不同） |
| `EdgeOfflineThreshold` | 90s | Edge 离线判定阈值 |
| `PromIngestFailLimit` | 5 | Prom 抓取连续失败次数阈值 |
| `Cooldown` | 10m | 同一告警冷却时间 |

---

## 15. Logs / Traces 配置

### 15.1 LogsConfig

源码：[config.go#L84-L89](file:///d:/claude/ongrid/internal/pkg/config/config.go#L84-L89)

```go
type LogsConfig struct {
    URL string
}

Logs: LogsConfig{
    URL: getEnv("ONGRID_LOGS_URL", "http://loki:3100"),
},
```

默认 `http://loki:3100`（容器内 DNS）。

### 15.2 TracesConfig

源码：[config.go#L96-L102](file:///d:/claude/ongrid/internal/pkg/config/config.go#L96-L102)

```go
type TracesConfig struct {
    URL string
}

Traces: TracesConfig{
    URL: getEnv("ONGRID_TRACES_URL", "http://tempo:3200"),
},
```

默认 `http://tempo:3200`（容器内 DNS）。

### 15.3 与 OTel 的关系

OTel endpoint 通过 `ONGRID_OTEL_ENDPOINT` 配置（[main.go#L223](file:///d:/claude/ongrid/cmd/ongrid/main.go#L223)），默认 `tempo:4318`，用于 OTLP 协议推送。Logs/Traces URL 用于查询（UI 展示）。

---

## 16. Skills 配置

源码：[config.go#L71-L78](file:///d:/claude/ongrid/internal/pkg/config/config.go#L71-L78)

```go
type SkillsConfig struct {
    ExternalDirs []string
}

Skills: SkillsConfig{
    ExternalDirs: getEnvCSV("ONGRID_SKILLS_EXTERNAL_DIRS"),
},
```

### 16.1 ExternalDirs 用途

外部 skills 目录列表（CSV），用于加载用户自定义 skills。每个目录下的 skill 会被注册到 skill registry。

### 16.2 与其他 skills roots 的关系

| env | 用途 | 默认 |
|---|---|---|
| `ONGRID_SKILLS_EXTERNAL_DIRS` | config.go 读取，用户外部目录 | 空 |
| `ONGRID_BUILTIN_SKILLS_ROOT` | main.go L1829，内置 skills | `./skills` |
| `ONGRID_BUILTIN_AGENTS_ROOT` | main.go L1830，内置 agents | `./agents` |
| `ONGRID_SKILLS_ROOT` | main.go L1831，运行时 skills | `/var/lib/ongrid/skills` |
| `ONGRID_WORKSPACE_ROOT` | main.go L1878，工作区 | `/var/lib/ongrid/workspace` |

---

## 17. Kubernetes Event 三件套

详见 [第 3.2 节](#32-k8s-event-三件套)。

| env | 默认 | 用途 |
|---|---|---|
| `ONGRID_K8S_EVENT_RETENTION` | `24h` | 事件保留时长 |
| `ONGRID_K8S_EVENT_MAX_PER_CLUSTER` | `5000` | 单集群最大事件数 |
| `ONGRID_K8S_EVENT_CLEANUP_INTERVAL` | `1h` | 清理循环周期 |

---

## 18. Embedding 配置

### 18.1 main.go 读取

源码：[cmd/ongrid/main.go#L1236-L1257](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1236-L1257)

```go
embedCfg := embedding.Config{
    Provider: getEnv("ONGRID_EMBEDDING_PROVIDER", "local"),
    APIKey:   getEnv("ONGRID_EMBEDDING_API_KEY", cfg.OpenAI.APIKey),  // fallback 到 OpenAI key
    BaseURL:  getEnv("ONGRID_EMBEDDING_BASE_URL", cfg.OpenAI.BaseURL),
    Model:    getEnv("ONGRID_EMBEDDING_MODEL", "bge-small-zh-v1.5"),
    Dim:      getEnvInt("ONGRID_EMBEDDING_DIM", 1536),  // 默认 1536（OpenAI dim）
}
```

### 18.2 双 provider 选项

| Provider | 实现 | dim | 用途 |
|---|---|---|---|
| `local`（默认） | [local.go](file:///d:/claude/ongrid/internal/pkg/embedding/local.go) ONNX | 512 | air-gapped / 离线 |
| `openai` | OpenAI 兼容 API | 1536 | 在线 / 高质量 |

### 18.3 4 个本地 model 选项

源码：[local.go#L63-L76](file:///d:/claude/ongrid/internal/pkg/embedding/local.go#L63-L76)

| Model 名 | dim | 说明 |
|---|---|---|
| `bge-small-zh-v1.5`（默认） | 512 | 中文小模型 |
| `bge-small-en-v1.5` | 384 | 英文小模型 |
| `bge-base-en-v1.5` | 768 | 英文 base |
| `all-minilm-l6-v2` | 384 | 多语言 mini |

### 18.4 CacheDir

源码：[local.go#L83-L100](file:///d:/claude/ongrid/internal/pkg/embedding/local.go#L83-L100)

```go
cacheDir := getEnv("ONGRID_EMBEDDING_CACHE_DIR", "/var/lib/ongrid/embeddings")
```

ONNX model 文件缓存目录。首次启动会从 HuggingFace 下载（约 100MB）。

### 18.5 dim 校验

源码：[local.go#L105-L110](file:///d:/claude/ongrid/internal/pkg/embedding/local.go#L105-L110)

```go
if cfg.Dim != 0 && cfg.Dim != actualDim {
    return nil, fmt.Errorf("embedding dim mismatch: configured=%d, model=%d", cfg.Dim, actualDim)
}
```

**关键**：`ONGRID_EMBEDDING_DIM` 必须与 model 实际 dim 匹配，否则启动失败。install/.env.example [L115](file:///d:/claude/ongrid/deploy/install/.env.example#L115) 显式设为 512。

### 18.6 main.go 默认 dim 与 install 模板的差异

| 来源 | 默认 dim | 原因 |
|---|---|---|
| main.go L1244 | 1536 | 兼容 OpenAI（在线场景） |
| install/.env.example L115 | 512 | 默认 local BGE-small-zh（离线场景） |

生产部署必须显式设置 `ONGRID_EMBEDDING_DIM`，避免使用 main.go 的 1536 默认值导致与 local model 不匹配。

---

## 19. Qdrant 配置

源码：[cmd/ongrid/main.go#L1258](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1258)

```go
qdrantURL := getEnv("ONGRID_QDRANT_URL", "http://qdrant:6333")
```

- 默认：`http://qdrant:6333`（容器内 DNS）
- 用途：知识库 RAG 向量检索

详细机制参考 [ongrid_qdrant.md](file:///d:/claude/ongrid/ongrid_qdrant.md)。

---

## 20. Knowledge Base 配置

### 20.1 Knowledge Repo Dir

源码：[cmd/ongrid/main.go#L1280](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1280)

```go
repoDir := getEnv("ONGRID_KNOWLEDGE_REPO_DIR", "/var/lib/ongrid/knowledge")
```

知识库文档存储目录（原始文档 + 解析后 chunk）。

### 20.2 Builtin Vault Seed

源码：[cmd/ongrid/main.go#L1306-L1331](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1306-L1331)

```go
seed := getEnv("ONGRID_BUILTIN_VAULT_SEED", "on")
if seed == "-" || strings.EqualFold(seed, "off") {
    // 跳过 boot sync
    return
}
```

- 默认：`on`（首次启动写入内置知识库 seed）
- `-` 或 `off`（大小写不敏感）：跳过 boot sync
- 用途：HLD-017 凭证保险库初始化

---

## 21. Investigator 配置（HLD-011）

源码：[cmd/ongrid/main.go#L1544-L1580](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1544-L1580)

```go
investigatorEnabled := getEnvBool("ONGRID_INVESTIGATOR_ENABLED", true)
investigatorMaxConcurrent := getEnvInt("ONGRID_INVESTIGATOR_MAX_CONCURRENT", 5)
summarizerProvider := getEnv("ONGRID_INVESTIGATOR_SUMMARIZER_PROVIDER", "")
summarizerModel := getEnv("ONGRID_INVESTIGATOR_SUMMARIZER_MODEL", "")
minSeverity := getEnv("ONGRID_INVESTIGATOR_MIN_SEVERITY", "warning")
```

### 21.1 字段语义

| env | 默认 | 用途 |
|---|---|---|
| `ONGRID_INVESTIGATOR_ENABLED` | `true` | 启用 HLD-011 自动 RCA 工作者 |
| `ONGRID_INVESTIGATOR_MAX_CONCURRENT` | `5` | 最大并发调查数 |
| `ONGRID_INVESTIGATOR_SUMMARIZER_PROVIDER` | 空 | 总结 provider（空则用 default） |
| `ONGRID_INVESTIGATOR_SUMMARIZER_MODEL` | 空 | 总结 model |
| `ONGRID_INVESTIGATOR_MIN_SEVERITY` | `warning` | 触发调查的最小告警级别 |

### 21.2 Default Locale

源码：[cmd/ongrid/main.go#L1579](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1579)

```go
defaultLocale := getEnv("ONGRID_DEFAULT_LOCALE", "en")
```

Report scheduler 默认 locale（`en` / `zh`）。

---

## 22. Marketplace 配置

源码：[cmd/ongrid/main.go#L1814-L1825](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1814-L1825)

```go
devMode := getEnv("ONGRID_MARKETPLACE_DEVMODE", "")  // 空=dev
requireSigned := getEnv("ONGRID_MARKETPLACE_REQUIRE_SIGNED_SOURCES", "ongrid-official")
pinnedPubkey := getEnv("ONGRID_MARKETPLACE_PINNED_PUBKEY", "")
```

### 22.1 字段语义

| env | 默认 | 用途 |
|---|---|---|
| `ONGRID_MARKETPLACE_DEVMODE` | 空（视为 true） | dev 模式（跳过签名校验） |
| `ONGRID_MARKETPLACE_REQUIRE_SIGNED_SOURCES` | `ongrid-official` | 要求签名的 source 列表 |
| `ONGRID_MARKETPLACE_PINNED_PUBKEY` | 空 | 固定公钥（pin） |

### 22.2 DEVMODE 默认 dev 的设计

源码：[main.go#L1814-L1815](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1814-L1815)

```go
devMode := getEnv("ONGRID_MARKETPLACE_DEVMODE", "")
// 空字符串视为 true，便于本地开发
```

**关键**：默认 dev 模式（不强制签名校验），生产部署必须显式 `ONGRID_MARKETPLACE_DEVMODE=false` + 配置 `PINNED_PUBKEY`。

---

## 23. Workspace / Skills Roots 配置

源码：[cmd/ongrid/main.go#L1829-L1831](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1829-L1831), [L1878](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1878)

```go
builtinSkillsRoot := getEnv("ONGRID_BUILTIN_SKILLS_ROOT", "./skills")
builtinAgentsRoot := getEnv("ONGRID_BUILTIN_AGENTS_ROOT", "./agents")
skillsRoot := getEnv("ONGRID_SKILLS_ROOT", "/var/lib/ongrid/skills")
workspaceRoot := getEnv("ONGRID_WORKSPACE_ROOT", "/var/lib/ongrid/workspace")
```

### 23.1 4 个目录的层次

| env | 默认 | 用途 |
|---|---|---|
| `ONGRID_BUILTIN_SKILLS_ROOT` | `./skills` | 内置 skills（随 binary 分发） |
| `ONGRID_BUILTIN_AGENTS_ROOT` | `./agents` | 内置 agents |
| `ONGRID_SKILLS_ROOT` | `/var/lib/ongrid/skills` | 运行时 skills（用户安装的） |
| `ONGRID_WORKSPACE_ROOT` | `/var/lib/ongrid/workspace` | 工作区（agent 执行 sandbox） |

### 23.2 PIP_INDEX_URL

源码：[cmd/ongrid/main.go#L1923](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1923)

```go
pipIndexURL := getEnv("ONGRID_PIP_INDEX_URL", "")
```

用于 pip 镜像加速（中国部署场景，如 `https://pypi.tuna.tsinghua.edu.cn/simple`）。

### 23.3 Pages Dir

源码：[cmd/ongrid/main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go)

```go
pagesDir := getEnv("ONGRID_PAGES_DIR", "/var/lib/ongrid/pages")
```

静态页面 / 文档目录。

---

## 24. Secretbox 与 ONGRID_SECRET_KEY

源码：[internal/pkg/secretbox/secretbox.go#L25-L75](file:///d:/claude/ongrid/internal/pkg/secretbox/secretbox.go#L25-L75)

### 24.1 全局变量

源码：[secretbox.go#L25-L29](file:///d:/claude/ongrid/internal/pkg/secretbox/secretbox.go#L25-L29)

```go
var (
    keyOnce sync.Once
    keyVal  [32]byte
    keyWeak bool
)
```

### 24.2 loadKey 派生

源码：[secretbox.go#L35-L44](file:///d:/claude/ongrid/internal/pkg/secretbox/secretbox.go#L35-L44)

```go
func loadKey() {
    keyOnce.Do(func() {
        raw := os.Getenv("ONGRID_SECRET_KEY")
        if raw == "" {
            // fallback 到不安全默认值
            raw = "ongrid-insecure-default-secret-key-set-ONGRID_SECRET_KEY"
            keyWeak = true
        }
        // sha256 派生 32 字节 key（AES-256）
        h := sha256.Sum256([]byte(raw))
        copy(keyVal[:], h[:])
    })
}
```

### 24.3 KeyIsWeak 报告

源码：[secretbox.go#L46-L51](file:///d:/claude/ongrid/internal/pkg/secretbox/secretbox.go#L46-L51)

```go
func KeyIsWeak() bool {
    loadKey()
    return keyWeak
}
```

启动时调用 `KeyIsWeak()` 检查，若为 true 则记录 warning（不 fatal，但生产环境必须设置）。

### 24.4 Encrypt wire format

源码：[secretbox.go#L56-L75](file:///d:/claude/ongrid/internal/pkg/secretbox/secretbox.go#L56-L75)

```go
func Encrypt(plaintext []byte) (string, error) {
    if len(plaintext) == 0 {
        return "", nil
    }
    loadKey()
    block, _ := aes.NewCipher(keyVal[:])
    gcm, _ := cipher.NewGCM(block)
    nonce := make([]byte, gcm.NonceSize())
    rand.Read(nonce)
    ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
    return "v1:" + base64.StdEncoding.EncodeToString(ciphertext), nil
}
```

**wire format**：`v1:` 前缀 + base64(nonce || ciphertext+tag)

- 版本前缀 `v1:` 用于未来算法升级（如换 chacha20-poly1305）
- nonce 12 字节随机
- AES-256-GCM（key 由 sha256 派生）

### 24.5 用途

`ONGRID_SECRET_KEY` 用于：
- HLD-017 凭证保险库加密（Prometheus Bearer token / Basic auth / 第三方 API key）
- DB 中敏感字段加密
- 与 `ONGRID_JWT_SECRET` 独立（JWT 用 HMAC，secretbox 用 AES-GCM）

---

## 25. OTel / Audit / Flow / Pages 杂项

### 25.1 OTel Endpoint

源码：[cmd/ongrid/main.go#L223](file:///d:/claude/ongrid/cmd/ongrid/main.go#L223)

```go
otelEndpoint := getEnv("ONGRID_OTEL_ENDPOINT", "tempo:4318")
```

OTLP 推送端点（HTTP 协议，默认 4318 端口）。

### 25.2 Audit Retention

源码：[cmd/ongrid/main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go)

```go
auditRetentionDays := getEnvInt("ONGRID_AUDIT_RETENTION_DAYS", 90)
```

审计日志保留天数（HLD-010），默认 90 天。

### 25.3 Grafana Panel Dashboard UID

源码：[cmd/ongrid/main.go#L488](file:///d:/claude/ongrid/cmd/ongrid/main.go#L488)

```go
panelDashboardUID := getEnv("ONGRID_GRAFANA_PANEL_DASHBOARD_UID", "")
```

监控面板 dashboard UID（用于嵌入到 OnGrid UI）。

### 25.4 Edge Bundle Dir

源码：[cmd/ongrid/main.go#L816](file:///d:/claude/ongrid/cmd/ongrid/main.go#L816)

```go
edgeBundleDir := getEnv("ONGRID_EDGE_BUNDLE_DIR", "/usr/share/ongrid/edge-bundles")
```

Edge Agent 二进制 bundle 目录（用于分发到边缘节点）。

### 25.5 Agent Kernel

源码：[cmd/ongrid/main.go#L1222](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1222)

```go
agentKernel := getEnv("ONGRID_AGENT_KERNEL", "react")
```

AIOps agent 内核选择：
- `react`（默认）：eino ReAct
- `graph`：opt-in 图编排

### 25.6 Flow Run Retention

源码：[internal/manager/biz/flow/usecase.go#L390-L404](file:///d:/claude/ongrid/internal/manager/biz/flow/usecase.go#L390-L404)

```go
flowRunRetentionDays := getEnvInt("ONGRID_FLOW_RUN_RETENTION_DAYS", 14)
```

Flow run 记录保留天数（默认 14 天）。

---

## 26. Edge Agent 独立配置（cmd/ongrid-edge）

Edge Agent binary 读取独立的 `ONGRID_EDGE_*` / `ONGRID_K8S_*` env，与 manager 的 EdgeConfig 不同。

### 26.1 Edge 模式与基础

| env | 默认 | 用途 |
|---|---|---|
| `ONGRID_EDGE_MODE` | 空 | edge 运行模式 |
| `ONGRID_MANAGER_PUBLIC_URL` | 空 | manager 公网 URL（edge 回连） |

### 26.2 K8s 集群注册

| env | 用途 |
|---|---|
| `ONGRID_K8S_CLUSTER_ID` | 集群 ID |
| `ONGRID_K8S_BOOTSTRAP_TOKEN` | 注册 token |
| `ONGRID_K8S_ROLE` | 节点角色 |
| `ONGRID_K8S_MODE` | k8s 模式 |
| `ONGRID_K8S_NODE_NAME` | 节点名 |
| `ONGRID_K8S_NODE_UID` | 节点 UID |
| `ONGRID_K8S_POD_NAMESPACE` | pod namespace |
| `ONGRID_K8S_POD_NAME` | pod 名 |
| `ONGRID_K8S_PROVIDER_ID` | cloud provider ID |

### 26.3 K8s Telemetry

| env | 用途 |
|---|---|
| `ONGRID_K8S_ENROLL_TLS_INSECURE` | 注册时跳过 TLS 校验 |
| `ONGRID_K8S_TELEMETRY_SECRET` | telemetry secret |
| `ONGRID_K8S_TELEMETRY_REQUIRED` | 是否强制 telemetry |
| `ONGRID_K8S_TELEMETRY_GATEWAY_ENABLED` | 启用 gateway telemetry |
| `ONGRID_K8S_TELEMETRY_CONFIG_REFRESH_INTERVAL` | 配置刷新周期 |

### 26.4 K8s Credentials

源码：[cmd/ongrid-edge/k8s_credentials.go#L45-L95](file:///d:/claude/ongrid/cmd/ongrid-edge/k8s_credentials.go#L45-L95)

| env | 用途 |
|---|---|
| `ONGRID_K8S_CREDENTIAL_FILE` | 凭证文件路径 |
| `ONGRID_K8S_CREDENTIAL_SECRET` | 凭证 secret |

### 26.5 K8s Inventory

| env | 用途 |
|---|---|
| `ONGRID_K8S_INVENTORY_INTERVAL` | 资产清单同步周期 |
| `ONGRID_K8S_INVENTORY_WATCH` | 是否 watch 实时变更 |

### 26.6 K8s Metrics（6 个）

| env | 用途 |
|---|---|
| `ONGRID_K8S_METRICS_ENABLED` | 启用 metrics |
| `ONGRID_K8S_METRICS_INTERVAL` | 采集周期 |
| `ONGRID_K8S_METRICS_KUBELET_PORT` | kubelet 端口 |
| `ONGRID_K8S_METRICS_KUBELET_SCHEME` | kubelet scheme |
| `ONGRID_K8S_METRICS_KUBELET_CA_PATH` | kubelet CA 路径 |
| `ONGRID_K8S_METRICS_APP_DISCOVERY` | 应用 metrics 自动发现 |

### 26.7 Edge Upgrade / Plugin

| env | 用途 |
|---|---|
| `ONGRID_EDGE_UPGRADE_STAGE_DIR` | 升级 staging 目录 |
| `ONGRID_EDGE_PLUGIN_BIN_DIR` | plugin 二进制目录 |
| `ONGRID_EDGE_PLUGIN_WORK_DIR` | plugin 工作目录 |
| `ONGRID_EDGE_PLUGIN_DATAPLANE_USER` | plugin dataplane 用户 |
| `ONGRID_EDGE_PLUGIN_DATAPLANE_PASS` | plugin dataplane 密码 |

### 26.8 Plugin 单实例配置

每个 plugin 有独立的 `ONGRID_EDGE_PLUGIN_<NAME>_*` env：

| env | 用途 |
|---|---|
| `ONGRID_EDGE_PLUGIN_<NAME>_ENABLED` | 启用 |
| `ONGRID_EDGE_PLUGIN_<NAME>_ENDPOINT` | endpoint |
| `ONGRID_EDGE_PLUGIN_<NAME>_AUTH_USER` | auth user |
| `ONGRID_EDGE_PLUGIN_<NAME>_AUTH_PASS` | auth pass |
| `ONGRID_EDGE_PLUGIN_<NAME>_SPEC_JSON` | spec JSON |

详见 [config_env.go](file:///d:/claude/ongrid/internal/edgeagent/plugins/config_env.go)。

---

## 27. Edge Plugin 配置（双路 fetcher）

Edge plugin 配置有两条路径：env bootstrap + tunnel RPC 主路径。

### 27.1 EnvConfigFetcher

源码：[internal/edgeagent/plugins/config_env.go](file:///d:/claude/ongrid/internal/edgeagent/plugins/config_env.go)（100 行）

读取 `ONGRID_EDGE_PLUGIN_<NAME>_*` env，fallback 到 `ONGRID_EDGE_ACCESS_KEY` / `ONGRID_EDGE_SECRET_KEY`。

### 27.2 TunnelConfigFetcher

源码：[internal/edgeagent/plugins/config_tunnel.go](file:///d:/claude/ongrid/internal/edgeagent/plugins/config_tunnel.go)（440 行）

主路径：通过 tunnel RPC 从 manager 拉取 plugin 配置。RPC 失败时 fallback 到 env。

### 27.3 Kubernetes 默认值注入

源码：[config_tunnel.go](file:///d:/claude/ongrid/internal/edgeagent/plugins/config_tunnel.go)

6 个注入函数：

| 函数 | 用途 |
|---|---|
| `withKubernetesLogsDefaults` | logs 默认值 |
| `withKubernetesTracesDefaults` | traces 默认值 |
| `withKubernetesGatewayTracesDefaults` | gateway traces 默认值 |
| `withKubernetesMetricsDefaults` | metrics 默认值 |
| `withKubernetesHostMetricsDefaults` | hostmetrics 默认值 |
| `withKubernetesProcMetricsDefaults` | procmetrics 默认值 |

这些函数在 tunnel RPC 返回的配置上叠加 K8s 场景的默认值（如 kubelet endpoint、CA 路径等）。

---

## 28. 部署差异：Dev vs Install

### 28.1 数据目录演进

- **v0.7.45 前**：使用 docker named volume
- **v0.7.45+**：host bind-mount，`ONGRID_DATA_DIR=/var/lib/ongrid`

所有 stateful 服务（MySQL / Qdrant / Embedding cache / Knowledge repo / Skills / Workspace / Pages）统一使用 host bind-mount，便于备份和迁移。

### 28.2 端口分层

| env | 容器内 | 主机 |
|---|---|---|
| `ONGRID_HTTP_ADDR` | `:8080` | - |
| `ONGRID_METRICS_ADDR` | `:9100` | - |
| `ONGRID_HTTP_PORT` | - | `443` |
| `ONGRID_HTTP_REDIRECT_PORT` | - | `80` |
| `ONGRID_TUNNEL_PORT` | - | `40012` |
| `ONGRID_METRICS_PORT` | - | `9100` |
| `PROM_PORT` | - | `9090` |

### 28.3 install.sh 自动生成

源码：[deploy/install/install.sh](file:///d:/claude/ongrid/deploy/install/install.sh)

`install.sh` 在以下 env 为空时自动生成随机值：

| env | 生成方式 |
|---|---|
| `MYSQL_PASSWORD` | 32-char random |
| `ONGRID_JWT_SECRET` | 64-char random |
| `GRAFANA_ADMIN_PASSWORD` | 20-char random |
| `ONGRID_ADMIN_PASSWORD` | 20-char random |

### 28.4 Docker 兼容性

源码：[install/.env.example#L30](file:///d:/claude/ongrid/deploy/install/.env.example#L30)

```env
ONGRID_HOST_GATEWAY=
```

旧 Docker 不支持 `host-gateway` 特殊值时，由 `install.sh` 设置为 docker bridge gateway IP。

### 28.5 Embedding 默认 provider

- **dev compose**：默认 local（BGE-small-zh-v1.5）
- **install compose**：默认 local（v0.7.113+ 统一）

### 28.6 JWT RefreshTTL

| 部署 | RefreshTTL |
|---|---|
| dev | `168h`（7 天，config.go 默认） |
| install | `720h`（30 天，install/.env.example [L45](file:///d:/claude/ongrid/deploy/install/.env.example#L45)） |

---

## 29. 配置优先级层级

OnGrid 配置有 3 层优先级，从高到低：

### 29.1 运行时可变（UI → system_settings）

最高优先级。用户通过 UI 修改的配置写入 `system_settings` 表，manager 通过 5s/60s TTL cache 感知。

**适用配置**：
- LLM provider APIKey / Model / BaseURL
- Prometheus Bearer / Basic
- Grafana SA token（首次 boot 后）
- Notification channel webhook URL

### 29.2 启动时固定（env → Config struct）

env 变量在进程启动时读取，固化到 `Config` struct，运行时不可变。

**适用配置**：
- `ONGRID_HTTP_ADDR` / `ONGRID_METRICS_ADDR` / `ONGRID_TUNNEL_ADDR`
- `ONGRID_DB_DSN` / `ONGRID_DB_DIALECT`
- `ONGRID_JWT_SECRET` / `ONGRID_JWT_ACCESS_TTL`
- `ONGRID_FRONTIER_ADDR`
- `ONGRID_LOGS_URL` / `ONGRID_TRACES_URL`
- `ONGRID_ALERT_EVAL_INTERVAL`

### 29.3 first-boot seed（env → system_settings）

env 变量在**首次启动**时写入 `system_settings` 表，之后由 DB 主导。env 修改不会生效（除非清空 DB）。

**适用配置**：
- `ONGRID_OPENAI_API_KEY` / `ONGRID_OPENAI_MODEL` / `ONGRID_OPENAI_BASE_URL`
- `ONGRID_GRAFANA_BOOTSTRAP_USER` / `ONGRID_GRAFANA_BOOTSTRAP_PASSWORD`
- `ONGRID_ADMIN_EMAIL` / `ONGRID_ADMIN_PASSWORD`

### 29.4 install/.env.example 的 leak back 警告

源码：[install/.env.example#L73-L75](file:///d:/claude/ongrid/deploy/install/.env.example#L73-L75)

```env
# 注：硬编码值会 leak back（首次启动写入 system_settings 后由 DB 主导），
# 建议留空，通过 UI 配置
```

**关键**：first-boot seed 类配置在 install 模板中建议留空，避免误导用户以为修改 env 会生效。

---

## 30. 敏感配置清单

### 30.1 高敏感（必须设置，不可入仓库）

| env | 用途 | 默认行为 |
|---|---|---|
| `ONGRID_JWT_SECRET` | JWT 签名 | 默认 `dev-insecure-secret-change-me` 时 fatal 拒绝启动 |
| `ONGRID_SECRET_KEY` | AES-256-GCM at-rest 加密 | 默认 fallback 时 `KeyIsWeak()` 报告 warning |
| `MYSQL_PASSWORD` | MySQL 密码 | install.sh 自动生成 |
| `ONGRID_OPENAI_API_KEY` | OpenAI API key | 空（first-boot seed） |
| `ONGRID_ADMIN_PASSWORD` | Admin 密码 | install.sh 自动生成 |
| `ONGRID_GRAFANA_BOOTSTRAP_PASSWORD` | Grafana admin 密码 | install.sh 自动生成 |
| `ONGRID_EDGE_ACCESS_KEY` / `ONGRID_EDGE_SECRET_KEY` | Edge 认证 | 空 |
| `ONGRID_K8S_BOOTSTRAP_TOKEN` | K8s 注册 token | 空 |
| `ONGRID_K8S_TELEMETRY_SECRET` | K8s telemetry secret | 空 |
| `ONGRID_INVESTIGATOR_SUMMARIZER_PROVIDER` / `MODEL` | Investigator LLM | 空（用 default） |

### 30.2 中敏感（生产必改，dev 可默认）

| env | 默认 | 生产推荐 |
|---|---|---|
| `ONGRID_JWT_REFRESH_TTL` | `168h` | `720h` |
| `ONGRID_ALERT_EVAL_INTERVAL` | `5m` | 根据规则数量调整 |
| `ONGRID_MARKETPLACE_DEVMODE` | 空（dev） | `false` |
| `ONGRID_PROM_TLS_INSECURE` | `false` | `false` |
| `ONGRID_GRAFANA_TLS_INSECURE` | `false` | `false` |

### 30.3 凭证轮转

- **JWT_SECRET**：轮转后所有 access/refresh token 失效，用户需重新登录
- **SECRET_KEY**：轮转后所有 secretbox 加密的字段无法解密（需重新加密）
- **MYSQL_PASSWORD**：需同步更新 MySQL 用户密码
- **OPENAI_API_KEY**：通过 UI 修改，5s/60s 刷新生效

---

## 31. 架构红线与注意事项

### 31.1 红线

1. **JWT_SECRET 必须设置**：检测到 `dev-insecure-secret-change-me` 时 fatal 拒绝启动（[main.go#L282-L286](file:///d:/claude/ongrid/cmd/ongrid/main.go#L282-L286)）
2. **SECRET_KEY 生产必须设置**：未设置时 `KeyIsWeak()` 报告 warning，生产环境必须显式配置
3. **first-boot seed 类配置不可通过 env 修改**：OpenAI / Grafana Bootstrap / Admin 等 env 仅首次启动生效，之后由 DB 主导
4. **运行时 Bearer/Basic 不入 env**：Prometheus Bearer/Basic 通过 UI 在 system_settings 中管理
5. **EMBEDDING_DIM 必须匹配 model**：local model 的 dim 是固定的，配置 dim 不匹配会启动失败
6. **MARKETPLACE_DEVMODE 默认 dev**：生产部署必须显式 `false` + 配置 `PINNED_PUBKEY`
7. **install.sh 自动生成密码**：MYSQL_PASSWORD / JWT_SECRET / ADMIN_PASSWORD / GRAFANA_ADMIN_PASSWORD 为空时自动生成
8. **端口分层**：容器内端口（`:8080`）与主机端口（`443`）分离，避免冲突
9. **Dialect 防御性回退**：`ONGRID_DB_DIALECT` 空字符串回退到 `mysql`
10. **Marketplace 默认 dev**：`ONGRID_MARKETPLACE_DEVMODE` 空字符串视为 true

### 31.2 注意事项

1. **getEnvDuration 支持整数秒**：`ONGRID_NOTIFY_TIMEOUT=10` 等价于 `10s`
2. **splitProviderModels 支持混合分隔**：`gpt-4o,gpt-4o-mini;gpt-4-turbo` 合法
3. **Load1=0 禁用**：不同机器核数不同，load1 默认禁用
4. **Notification Enabled 默认差异**：config.go 默认 `true`，.env.example 显式 `false`
5. **Edge Agent 独立 env**：`ONGRID_EDGE_*` 在 manager 和 edge binary 中语义不同
6. **TunnelConfigFetcher 主路径**：edge plugin 配置优先 RPC，env 为 fallback
7. **OTel endpoint 默认 tempo:4318**：与 TracesConfig URL（tempo:3200）不同，前者 OTLP 推送，后者查询
8. **EvaluatorInterval 变更历史**：30s → 5m（2026-05-31），避免规则堆积
9. **Audit retention 默认 90 天**：HLD-010 审计日志保留
10. **Flow run retention 默认 14 天**：Flow 编排记录保留

---

## 32. 附录：完整 env 变量索引

### 32.1 HTTP / Metrics / Tunnel / PublicURL

| env | 默认 | 文件行号 |
|---|---|---|
| `ONGRID_HTTP_ADDR` | `:8080` | [config.go#L375](file:///d:/claude/ongrid/internal/pkg/config/config.go#L375) |
| `ONGRID_METRICS_ADDR` | `:9100` | [config.go#L376](file:///d:/claude/ongrid/internal/pkg/config/config.go#L376) |
| `ONGRID_TUNNEL_ADDR` | `:40012` | [config.go#L377](file:///d:/claude/ongrid/internal/pkg/config/config.go#L377) |
| `ONGRID_PUBLIC_URL` | `http://localhost:8080` | [config.go#L378](file:///d:/claude/ongrid/internal/pkg/config/config.go#L378) |

### 32.2 K8s Event

| env | 默认 | 文件行号 |
|---|---|---|
| `ONGRID_K8S_EVENT_RETENTION` | `24h` | [config.go#L380](file:///d:/claude/ongrid/internal/pkg/config/config.go#L380) |
| `ONGRID_K8S_EVENT_MAX_PER_CLUSTER` | `5000` | [config.go#L381](file:///d:/claude/ongrid/internal/pkg/config/config.go#L381) |
| `ONGRID_K8S_EVENT_CLEANUP_INTERVAL` | `1h` | [config.go#L382](file:///d:/claude/ongrid/internal/pkg/config/config.go#L382) |

### 32.3 DB

| env | 默认 | 文件行号 |
|---|---|---|
| `ONGRID_DB_DIALECT` | 空（回退 mysql） | [config.go#L387](file:///d:/claude/ongrid/internal/pkg/config/config.go#L387) |
| `ONGRID_DB_DSN` | 空 | [config.go#L388](file:///d:/claude/ongrid/internal/pkg/config/config.go#L388) |
| `ONGRID_DB_PATH` | `./data/ongrid.db` | [config.go#L389](file:///d:/claude/ongrid/internal/pkg/config/config.go#L389) |

### 32.4 JWT

| env | 默认 | 文件行号 |
|---|---|---|
| `ONGRID_JWT_SECRET` | `dev-insecure-secret-change-me` | [config.go#L400](file:///d:/claude/ongrid/internal/pkg/config/config.go#L400) |
| `ONGRID_JWT_ACCESS_TTL` | `15m` | [config.go#L401](file:///d:/claude/ongrid/internal/pkg/config/config.go#L401) |
| `ONGRID_JWT_REFRESH_TTL` | `168h` | [config.go#L402](file:///d:/claude/ongrid/internal/pkg/config/config.go#L402) |

### 32.5 OpenAI

| env | 默认 | 文件行号 |
|---|---|---|
| `ONGRID_OPENAI_API_KEY` | 空 | [config.go#L405](file:///d:/claude/ongrid/internal/pkg/config/config.go#L405) |
| `ONGRID_OPENAI_MODEL` | `gpt-4o-mini` | [config.go#L406](file:///d:/claude/ongrid/internal/pkg/config/config.go#L406) |
| `ONGRID_OPENAI_BASE_URL` | 空 | [config.go#L407](file:///d:/claude/ongrid/internal/pkg/config/config.go#L407) |
| `ONGRID_OPENAI_MODELS` | 空（CSV） | [config.go#L408](file:///d:/claude/ongrid/internal/pkg/config/config.go#L408) |

### 32.6 LLM 多 provider

| env | 文件行号 |
|---|---|
| `ONGRID_LLM_ANTHROPIC_API_KEY` / `MODEL` / `BASE_URL` / `MODELS` | [config.go#L410-L413](file:///d:/claude/ongrid/internal/pkg/config/config.go#L410-L413) |
| `ONGRID_LLM_ZHIPU_*` | [config.go#L415-L418](file:///d:/claude/ongrid/internal/pkg/config/config.go#L415-L418) |
| `ONGRID_LLM_GEMINI_*` | [config.go#L420-L423](file:///d:/claude/ongrid/internal/pkg/config/config.go#L420-L423) |
| `ONGRID_LLM_DEEPSEEK_*` | [config.go#L425-L428](file:///d:/claude/ongrid/internal/pkg/config/config.go#L425-L428) |
| `ONGRID_LLM_KIMI_*` | [config.go#L430-L433](file:///d:/claude/ongrid/internal/pkg/config/config.go#L430-L433) |
| `ONGRID_LLM_DEFAULT` | [config.go#L435](file:///d:/claude/ongrid/internal/pkg/config/config.go#L435) |
| `ONGRID_LLM_DAILY_TOKEN_LIMIT` | [config.go#L436](file:///d:/claude/ongrid/internal/pkg/config/config.go#L436) |

### 32.7 Admin

| env | 默认 | 文件行号 |
|---|---|---|
| `ONGRID_ADMIN_EMAIL` | `admin@ongrid.local` | [config.go#L438](file:///d:/claude/ongrid/internal/pkg/config/config.go#L438) |
| `ONGRID_ADMIN_PASSWORD` | 空 | [config.go#L439](file:///d:/claude/ongrid/internal/pkg/config/config.go#L439) |

### 32.8 Edge（manager 侧）

| env | 默认 | 文件行号 |
|---|---|---|
| `ONGRID_EDGE_CLOUD_ADDR` | `127.0.0.1:40012` | [config.go#L441](file:///d:/claude/ongrid/internal/pkg/config/config.go#L441) |
| `ONGRID_EDGE_ACCESS_KEY` | 空 | [config.go#L442](file:///d:/claude/ongrid/internal/pkg/config/config.go#L442) |
| `ONGRID_EDGE_SECRET_KEY` | 空 | [config.go#L443](file:///d:/claude/ongrid/internal/pkg/config/config.go#L443) |
| `ONGRID_EDGE_COLLECTOR_MODE` | `off` | [config.go#L444](file:///d:/claude/ongrid/internal/pkg/config/config.go#L444) |
| `ONGRID_EDGE_SCRAPE_CONFIG_FILE` | `/etc/ongrid-edge/scrape.yaml` | [config.go#L445](file:///d:/claude/ongrid/internal/pkg/config/config.go#L445) |
| `ONGRID_EDGE_COLLECTOR_INTERVAL` | `10s` | [config.go#L446](file:///d:/claude/ongrid/internal/pkg/config/config.go#L446) |

### 32.9 Frontier Client

| env | 默认 | 文件行号 |
|---|---|---|
| `ONGRID_FRONTIER_ADDR` | `frontier:40011` | [config.go#L448](file:///d:/claude/ongrid/internal/pkg/config/config.go#L448) |
| `ONGRID_FRONTIER_SERVICE_NAME` | `ongrid-manager` | [config.go#L449](file:///d:/claude/ongrid/internal/pkg/config/config.go#L449) |
| `ONGRID_FRONTIER_DISABLED` | `false` | [config.go#L450](file:///d:/claude/ongrid/internal/pkg/config/config.go#L450) |

### 32.10 Prom

| env | 默认 | 文件行号 |
|---|---|---|
| `ONGRID_PROM_ENABLED` | `true` | [config.go#L452](file:///d:/claude/ongrid/internal/pkg/config/config.go#L452) |
| `ONGRID_PROM_URL` | `http://prometheus:9090` | [config.go#L453](file:///d:/claude/ongrid/internal/pkg/config/config.go#L453) |
| `ONGRID_PROM_REMOTE_WRITE_URL` | 空 | [config.go#L454](file:///d:/claude/ongrid/internal/pkg/config/config.go#L454) |
| `ONGRID_PROM_QUERY_URL` | 空 | [config.go#L455](file:///d:/claude/ongrid/internal/pkg/config/config.go#L455) |
| `ONGRID_PROM_TLS_INSECURE` | `false` | [config.go#L456](file:///d:/claude/ongrid/internal/pkg/config/config.go#L456) |
| `ONGRID_PROM_TLS_CA_PATH` | 空 | [config.go#L457](file:///d:/claude/ongrid/internal/pkg/config/config.go#L457) |

### 32.11 Grafana

| env | 默认 | 文件行号 |
|---|---|---|
| `ONGRID_GRAFANA_INTERNAL_ROOT_URL` | `http://grafana:3000/grafana` | [config.go#L459](file:///d:/claude/ongrid/internal/pkg/config/config.go#L459) |
| `ONGRID_GRAFANA_BOOTSTRAP_USER` | `admin` | [config.go#L460](file:///d:/claude/ongrid/internal/pkg/config/config.go#L460) |
| `ONGRID_GRAFANA_BOOTSTRAP_PASSWORD` | 空 | [config.go#L461](file:///d:/claude/ongrid/internal/pkg/config/config.go#L461) |
| `ONGRID_GRAFANA_TLS_INSECURE` | `false` | [config.go#L462](file:///d:/claude/ongrid/internal/pkg/config/config.go#L462) |

### 32.12 Notification

| env | 默认 | 文件行号 |
|---|---|---|
| `ONGRID_NOTIFY_ENABLED` | `true` | [config.go#L464](file:///d:/claude/ongrid/internal/pkg/config/config.go#L464) |
| `ONGRID_NOTIFY_DEFAULT_CHANNELS` | 空（CSV） | [config.go#L465](file:///d:/claude/ongrid/internal/pkg/config/config.go#L465) |
| `ONGRID_NOTIFY_TIMEOUT` | `10s` | [config.go#L466](file:///d:/claude/ongrid/internal/pkg/config/config.go#L466) |
| `ONGRID_NOTIFY_LOG_ENABLED` | `false` | [config.go#L467](file:///d:/claude/ongrid/internal/pkg/config/config.go#L467) |
| `ONGRID_NOTIFY_WEBHOOK_URL` / `SECRET` | 空 | [config.go#L468-L469](file:///d:/claude/ongrid/internal/pkg/config/config.go#L468-L469) |
| `ONGRID_NOTIFY_SLACK_WEBHOOK_URL` / `SECRET` | 空 | [config.go#L470-L471](file:///d:/claude/ongrid/internal/pkg/config/config.go#L470-L471) |
| `ONGRID_NOTIFY_FEISHU_WEBHOOK_URL` / `SECRET` | 空 | [config.go#L472-L473](file:///d:/claude/ongrid/internal/pkg/config/config.go#L472-L473) |
| `ONGRID_NOTIFY_DINGTALK_WEBHOOK_URL` / `SECRET` | 空 | [config.go#L474-L475](file:///d:/claude/ongrid/internal/pkg/config/config.go#L474-L475) |

### 32.13 Alert

| env | 默认 | 文件行号 |
|---|---|---|
| `ONGRID_ALERT_ENABLED` | `true` | [config.go#L477](file:///d:/claude/ongrid/internal/pkg/config/config.go#L477) |
| `ONGRID_ALERT_COOLDOWN` | `10m` | [config.go#L478](file:///d:/claude/ongrid/internal/pkg/config/config.go#L478) |
| `ONGRID_ALERT_CPU_PERCENT` | `90` | [config.go#L479](file:///d:/claude/ongrid/internal/pkg/config/config.go#L479) |
| `ONGRID_ALERT_MEM_PERCENT` | `90` | [config.go#L480](file:///d:/claude/ongrid/internal/pkg/config/config.go#L480) |
| `ONGRID_ALERT_DISK_USED_PERCENT` | `90` | [config.go#L481](file:///d:/claude/ongrid/internal/pkg/config/config.go#L481) |
| `ONGRID_ALERT_LOAD1` | `0` | [config.go#L482](file:///d:/claude/ongrid/internal/pkg/config/config.go#L482) |
| `ONGRID_ALERT_EVAL_INTERVAL` | `5m` | [config.go#L483](file:///d:/claude/ongrid/internal/pkg/config/config.go#L483) |
| `ONGRID_ALERT_EDGE_OFFLINE_THRESHOLD` | `90s` | [config.go#L484](file:///d:/claude/ongrid/internal/pkg/config/config.go#L484) |
| `ONGRID_ALERT_PROM_INGEST_FAIL_LIMIT` | `5` | [config.go#L485](file:///d:/claude/ongrid/internal/pkg/config/config.go#L485) |

### 32.14 Logs / Traces

| env | 默认 | 文件行号 |
|---|---|---|
| `ONGRID_LOGS_URL` | `http://loki:3100` | [config.go#L487](file:///d:/claude/ongrid/internal/pkg/config/config.go#L487) |
| `ONGRID_TRACES_URL` | `http://tempo:3200` | [config.go#L488](file:///d:/claude/ongrid/internal/pkg/config/config.go#L488) |

### 32.15 Skills

| env | 默认 | 文件行号 |
|---|---|---|
| `ONGRID_SKILLS_EXTERNAL_DIRS` | 空（CSV） | [config.go#L490](file:///d:/claude/ongrid/internal/pkg/config/config.go#L490) |

### 32.16 main.go 直接读取（config.go 之外）

| env | 默认 | 文件行号 |
|---|---|---|
| `ONGRID_OTEL_ENDPOINT` | `tempo:4318` | [main.go#L223](file:///d:/claude/ongrid/cmd/ongrid/main.go#L223) |
| `ONGRID_AUDIT_RETENTION_DAYS` | `90` | [main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go) |
| `ONGRID_GRAFANA_PANEL_DASHBOARD_UID` | 空 | [main.go#L488](file:///d:/claude/ongrid/cmd/ongrid/main.go#L488) |
| `ONGRID_EDGE_BUNDLE_DIR` | `/usr/share/ongrid/edge-bundles` | [main.go#L816](file:///d:/claude/ongrid/cmd/ongrid/main.go#L816) |
| `ONGRID_AGENT_KERNEL` | `react` | [main.go#L1222](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1222) |
| `ONGRID_EMBEDDING_PROVIDER` | `local` | [main.go#L1237](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1237) |
| `ONGRID_EMBEDDING_API_KEY` | fallback `OPENAI_API_KEY` | [main.go#L1238](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1238) |
| `ONGRID_EMBEDDING_BASE_URL` | fallback `OPENAI_BASE_URL` | [main.go#L1239](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1239) |
| `ONGRID_EMBEDDING_MODEL` | `bge-small-zh-v1.5` | [main.go#L1240](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1240) |
| `ONGRID_EMBEDDING_DIM` | `1536` | [main.go#L1241](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1241) |
| `ONGRID_EMBEDDING_CACHE_DIR` | `/var/lib/ongrid/embeddings` | [local.go#L85](file:///d:/claude/ongrid/internal/pkg/embedding/local.go#L85) |
| `ONGRID_QDRANT_URL` | `http://qdrant:6333` | [main.go#L1258](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1258) |
| `ONGRID_KNOWLEDGE_REPO_DIR` | `/var/lib/ongrid/knowledge` | [main.go#L1280](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1280) |
| `ONGRID_BUILTIN_VAULT_SEED` | `on` | [main.go#L1306](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1306) |
| `ONGRID_INVESTIGATOR_ENABLED` | `true` | [main.go#L1544](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1544) |
| `ONGRID_INVESTIGATOR_MAX_CONCURRENT` | `5` | [main.go#L1551](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1551) |
| `ONGRID_INVESTIGATOR_SUMMARIZER_PROVIDER` | 空 | [main.go#L1561](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1561) |
| `ONGRID_INVESTIGATOR_SUMMARIZER_MODEL` | 空 | [main.go#L1562](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1562) |
| `ONGRID_INVESTIGATOR_MIN_SEVERITY` | `warning` | [main.go#L1565](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1565) |
| `ONGRID_DEFAULT_LOCALE` | `en` | [main.go#L1579](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1579) |
| `ONGRID_MARKETPLACE_DEVMODE` | 空（dev） | [main.go#L1814](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1814) |
| `ONGRID_MARKETPLACE_REQUIRE_SIGNED_SOURCES` | `ongrid-official` | [main.go#L1817](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1817) |
| `ONGRID_MARKETPLACE_PINNED_PUBKEY` | 空 | [main.go#L1825](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1825) |
| `ONGRID_BUILTIN_SKILLS_ROOT` | `./skills` | [main.go#L1829](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1829) |
| `ONGRID_BUILTIN_AGENTS_ROOT` | `./agents` | [main.go#L1830](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1830) |
| `ONGRID_SKILLS_ROOT` | `/var/lib/ongrid/skills` | [main.go#L1831](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1831) |
| `ONGRID_WORKSPACE_ROOT` | `/var/lib/ongrid/workspace` | [main.go#L1878](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1878) |
| `ONGRID_PIP_INDEX_URL` | 空 | [main.go#L1923](file:///d:/claude/ongrid/cmd/ongrid/main.go#L1923) |
| `ONGRID_PAGES_DIR` | `/var/lib/ongrid/pages` | [main.go](file:///d:/claude/ongrid/cmd/ongrid/main.go) |
| `ONGRID_SECRET_KEY` | fallback 不安全值 | [secretbox.go#L37](file:///d:/claude/ongrid/internal/pkg/secretbox/secretbox.go#L37) |
| `ONGRID_FLOW_RUN_RETENTION_DAYS` | `14` | [flow/usecase.go#L390](file:///d:/claude/ongrid/internal/manager/biz/flow/usecase.go#L390) |

### 32.17 Edge Agent（cmd/ongrid-edge）

详见 [第 26 节](#26-edge-agent-独立配置cmdongrid-edge)。

---

## 33. 交叉引用

- [ongrid_architecture.md](file:///d:/claude/ongrid/ongrid_architecture.md)：架构总览
- [ongrid_LLM.md](file:///d:/claude/ongrid/ongrid_LLM.md)：LLM 客户端 / eino / RAG 上层编排
- [ongrid_rpc_singchia_geminio.md](file:///d:/claude/ongrid/ongrid_rpc_singchia_geminio.md)：RPC 隧道（Frontier 通信）
- [ongrid_integration.md](file:///d:/claude/ongrid/ongrid_integration.md)：8 个外部系统集成
- [ongrid_api.md](file:///d:/claude/ongrid/ongrid_api.md)：26 个业务域 API
- [ongrid_grafana.md](file:///d:/claude/ongrid/ongrid_grafana.md)：Grafana 嵌入集成
- [ongrid_frontier.md](file:///d:/claude/ongrid/ongrid_frontier.md)：Frontier 集成
- [ongrid_qdrant.md](file:///d:/claude/ongrid/ongrid_qdrant.md)：Qdrant 向量库
- [ongrid_errors.md](file:///d:/claude/ongrid/ongrid_errors.md)：LLM provider 错误分析

---

**文档版本**：v1.0
**生成时间**：2026-07-31
**覆盖源码版本**：v0.7.113
