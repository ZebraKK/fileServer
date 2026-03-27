# CDN 节点文件资源管理服务 — 代码结构

## 目录树

```
cdncache/
├── cmd/
│   └── server/
│       └── main.go                  # 启动入口
├── config.example.yaml              # 配置示例
├── internal/
│   ├── config/
│   │   ├── config.go                # 配置结构定义
│   │   └── loader.go                # YAML 解析 + 热加载
│   ├── server/
│   │   ├── server.go                # HTTP server 生命周期管理
│   │   └── middleware.go            # 通用 middleware
│   ├── domain/
│   │   └── router.go                # 域名路由（Host → 域名配置）
│   ├── pipeline/
│   │   ├── pipeline.go              # Plugin 接口 + 流水线执行
│   │   ├── ratelimit/
│   │   │   └── ratelimit.go         # 限流插件
│   │   ├── rewrite/
│   │   │   └── rewrite.go           # URL 改写插件
│   │   └── header/
│   │       └── header.go            # 请求头/响应头处理插件
│   ├── cache/
│   │   ├── cache.go                 # Cache 接口 + LRU+TTL 实现
│   │   ├── key.go                   # 缓存 key 构造与规范化
│   │   └── singleflight.go          # singleflight 防击穿封装
│   ├── origin/
│   │   └── puller.go                # 回源：多源轮询 + 超时重试
│   ├── storage/
│   │   ├── storage.go               # Storage 接口定义
│   │   └── metadata.go              # Metadata / FileInfo 数据结构
│   ├── flush/
│   │   └── store.go                 # FlushRule 管理：写入/持久化/匹配检查
│   ├── admin/
│   │   └── handler.go               # 管理 API handler（flush/stat）
│   └── observe/
│       ├── logger.go                # 结构化日志（slog）
│       └── metrics.go               # Prometheus metrics 注册与更新
└── integration/                     # 集成测试（见 integration/README.md）
    ├── README.md
    ├── mock/
    │   └── storage_mock.go
    └── cases/
        └── cache_test.go
```

---

## 各包职责说明

### `cmd/server/main.go`

程序入口，负责：
1. 解析命令行参数（配置文件路径）
2. 加载配置（调用 `config.Load`）
3. 初始化各组件（Storage、Cache、FlushStore、OriginPuller、PluginFactory）
4. 构建 Domain Router 和 Plugin Pipeline
5. 启动 Business HTTP Server 和 Admin HTTP Server
6. 监听 SIGTERM/SIGINT，触发优雅关闭

---

### `internal/config/`

#### `config.go` — 配置结构

```go
type Config struct {
    Server   ServerConfig
    Admin    AdminConfig
    KeyRules KeyRulesConfig   // 全局默认 key 规则
    Domains  []DomainConfig
}

type DomainConfig struct {
    Domain        string
    Origins       []string
    OriginTimeout time.Duration
    OriginRetry   int
    DefaultTTL    time.Duration
    KeyRules      *KeyRulesConfig  // nil 时继承全局
    Plugins       []PluginConfig
}

type PluginConfig struct {
    Type   string          // "rate_limit" | "url_rewrite" | "header"
    Config map[string]any  // 插件专属配置，由各插件解析
}

type KeyRulesConfig struct {
    IncludeQueryParams []string
    IncludeHeaders     []string
}
```

#### `loader.go` — 配置加载与热加载

- `Load(path string) (*Config, error)`：解析 YAML 文件
- `Watch(path string, onChange func(*Config))`：使用 `fsnotify` 监听文件变化，变化时重新 Load 并调用回调
- 热加载时，`DomainRouter` 原子替换域名配置 map（`sync.Map` 或带锁的指针）

---

### `internal/server/`

#### `server.go` — HTTP Server

- 创建两个 HTTP server：业务端口（默认 8080）和管理端口（默认 9090）
- 注册路由：
  - 业务 server：`/*` → `DomainRouter.ServeHTTP`
  - 管理 server：`/admin/*` → `AdminHandler`，`/metrics` → Prometheus handler
- `Shutdown(ctx)`：调用 `http.Server.Shutdown`，等待进行中请求完成

#### `middleware.go` — 通用 Middleware

- `RequestID`：生成并注入唯一 request ID（UUID）到 request context 和响应头
- `Recovery`：panic 恢复，返回 500
- `Logging`：在请求完成后记录结构化日志（从 context 中取 request ID 等信息）
- `Metrics`：记录请求耗时和状态码到 Prometheus

---

### `internal/domain/`

#### `router.go` — 域名路由

```go
type DomainRouter struct {
    domains sync.Map  // map[string]*DomainHandler
}

func (r *DomainRouter) ServeHTTP(w http.ResponseWriter, req *http.Request)
func (r *DomainRouter) Update(domains []DomainConfig)  // 热加载时调用
```

- 从 `req.Host` 提取域名（去掉端口）
- 查找对应的 `DomainHandler`，不存在则返回 404
- 将请求转发给 `DomainHandler`

---

### `internal/pipeline/`

#### `pipeline.go` — 插件接口与流水线

```go
type Context struct {
    Request        *http.Request
    RewrittenPath  string           // URL 改写插件写入，后续读取
    ResponseHeader http.Header      // 响应头处理插件写入，返回时应用
}

type Plugin interface {
    Name() string
    Handle(ctx context.Context, pCtx *Context, w http.ResponseWriter) (next bool)
}

type Pipeline struct {
    plugins []Plugin
}

func (p *Pipeline) Execute(ctx context.Context, pCtx *Context, w http.ResponseWriter) bool
```

#### `ratelimit/ratelimit.go` — 限流插件

- 配置项：`mode`（`domain`/`ip`）、`algorithm`（`token_bucket`/`sliding_window`）、`rate`（请求数/秒）、`burst`（令牌桶最大容量）
- 令牌桶：每个 key（domain 或 IP）一个 `rate.Limiter`（`golang.org/x/time/rate`）
- 限流器存储：`sync.Map[key → *rate.Limiter]`，配合定时清理（避免 IP 计数器无限增长）
- 触发限流：返回 `false`，写入 429 响应

#### `rewrite/rewrite.go` — URL 改写插件

- 配置项：`rules []RewriteRule`，每条规则包含 `match`（正则）和 `replace`（模板）
- 初始化时预编译所有正则（`regexp.MustCompile`）
- `Handle` 时遍历规则，第一条命中则改写 `pCtx.RewrittenPath`，停止遍历

#### `header/header.go` — Header 处理插件

- 配置项：`request []HeaderRule` 和 `response []HeaderRule`
- `HeaderRule`：`{op: "set"|"add"|"del", key: string, value: string}`
- 请求头规则在 `Handle` 时立即应用到 `req.Header`
- 响应头规则写入 `pCtx.ResponseHeader`，由缓存层/透传层在写响应前应用

---

### `internal/cache/`

#### `cache.go` — 缓存接口与实现

```go
type Cache interface {
    Get(key string) (io.ReadCloser, *CacheMeta, error)
    Set(key string, r io.Reader, meta *CacheMeta) error
    Delete(key string) error
    Stat(key string) (*CacheMeta, error)
}

type CacheMeta struct {
    WrittenAt  time.Time
    TTL        time.Duration
    Size       int64
    Headers    http.Header  // 缓存的响应头
}
```

LRU 实现：
- 内存中维护 `map[string]*list.Element` + `container/list`（doubly linked list）
- 读取时将 element 移到链表头部
- 写入时若超过容量（按 item 数或总字节数），淘汰链表尾部元素
- TTL 过期检查：`Get` 时检查 `WrittenAt + TTL < now`，过期则删除并返回 miss

#### `key.go` — 缓存 Key 构造

```go
type KeyBuilder struct {
    global KeyRulesConfig
}

func (kb *KeyBuilder) Build(domain, rewrittenPath string, req *http.Request, domainRules *KeyRulesConfig) string
```

构造逻辑：
1. 确定有效规则：domainRules 不为 nil 则用 domainRules，否则用 global
2. 从 `req.URL.Query()` 取指定参数，按参数名排序后拼接
3. 从 `req.Header` 取指定 header 值，拼接后 SHA1 hash（避免特殊字符污染 key）
4. 拼接：`domain + ":" + rewrittenPath + "?" + sortedQuery + "#" + headerHash`

#### `singleflight.go` — 防击穿

```go
type SingleflightCache struct {
    cache Cache
    group singleflight.Group
}

func (sc *SingleflightCache) GetOrFetch(key string, fetch func() (io.Reader, *CacheMeta, error)) (io.ReadCloser, *CacheMeta, error)
```

- `Get` 命中则直接返回
- Miss 时通过 `group.Do(key, fetch)` 确保并发只触发一次 `fetch`

---

### `internal/origin/`

#### `puller.go` — 回源

```go
type Puller struct {
    client *http.Client
}

func (p *Puller) Pull(ctx context.Context, origins []string, req *http.Request) (*http.Response, error)
```

- 从 `origins` 列表中轮询选择（原子计数器 % len）
- 构造回源请求：复制原始请求的 Method、部分 Header（去掉 Host），替换 URL
- 超时通过 `context.WithTimeout` 控制
- 失败时重试（切换下一个 origin），超出重试次数返回错误

---

### `internal/storage/`

#### `storage.go` — Storage 接口

```go
// Storage 是对专有本地文件管理库的抽象接口
// 实现由专有库提供，本服务不直接操作文件系统
type Storage interface {
    Read(key string) (io.ReadCloser, *Metadata, error)
    Write(key string, r io.Reader, meta *Metadata) error
    Delete(key string) error
    Exists(key string) bool
    Stat(key string) (*FileInfo, error)
    List(prefix string) ([]string, error)
}
```

#### `metadata.go` — 数据结构

```go
type Metadata struct {
    ContentType  string
    CacheControl string
    Expires      string
    CustomMeta   map[string]string  // 存储 WrittenAt、TTL 等缓存元数据
}

type FileInfo struct {
    Size    int64
    ModTime time.Time
}
```

---

### `internal/flush/`

#### `store.go` — FlushRule 管理

```go
type FlushRule struct {
    ID        string
    Domain    string
    Prefix    string    // "/" 表示整域名
    CreatedAt time.Time
}

type Store struct {
    mu      sync.RWMutex
    rules   map[string][]FlushRule  // domain → rules
    storage Storage
}

func (s *Store) AddRule(domain, prefix string) error     // 添加规则并持久化
func (s *Store) Match(domain, path string) *FlushRule    // 查找命中的最新规则
func (s *Store) Load() error                             // 启动时从 Storage 恢复
func (s *Store) Cleanup(maxAge time.Duration) error      // 清理过期规则
```

持久化：将所有 rules 序列化为 JSON，通过 `storage.Write("__flush_rules__", ...)` 写入。

---

### `internal/admin/`

#### `handler.go` — 管理 API

```
POST /admin/flush/url     → 精确 URL 刷新（删除缓存文件 + 内存索引）
POST /admin/flush/prefix  → 前缀刷新（写入 FlushRule）
POST /admin/flush/domain  → 整域名刷新（写入 FlushRule，prefix="/"）
GET  /admin/stat          → 缓存状态查询
```

---

### `internal/observe/`

#### `logger.go` — 结构化日志

- 使用 Go 1.21+ 内置 `log/slog`
- 默认输出 JSON 格式到 stdout
- 提供 `FromContext(ctx) *slog.Logger` 获取带 request_id 的 logger

#### `metrics.go` — Prometheus Metrics

- 在包 init 时注册所有 metric（Counter/Gauge/Histogram）
- 提供函数供各层调用（如 `RecordRequest(domain, status, cacheHit, latency)`）

---

## 配置文件示例

```yaml
# config.example.yaml

server:
  addr: ":8080"

admin:
  addr: ":9090"

# 全局默认缓存 key 规则
key_rules:
  include_query_params: []
  include_headers: []

domains:
  - domain: "cdn.example.com"
    origins:
      - "http://origin1.example.com"
      - "http://origin2.example.com"
    origin_timeout: "10s"
    origin_retry: 2
    default_ttl: "1h"

    # 该域名的 key 规则（覆盖全局）
    key_rules:
      include_query_params: ["version", "format"]
      include_headers: ["Accept-Language"]

    plugins:
      - type: rate_limit
        config:
          mode: "domain"          # 或 "ip"
          algorithm: "token_bucket"
          rate: 1000              # 请求/秒
          burst: 200

      - type: url_rewrite
        config:
          rules:
            - match: "^/v1/(.*)"
              replace: "/api/$1"

      - type: header
        config:
          request:
            - op: set
              key: "X-Forwarded-Host"
              value: "cdn.example.com"
          response:
            - op: set
              key: "X-Cache-Node"
              value: "node-01"
            - op: del
              key: "Server"

  - domain: "static.example.com"
    origins:
      - "http://static-origin.example.com"
    origin_timeout: "5s"
    origin_retry: 1
    default_ttl: "24h"
    plugins:
      - type: rate_limit
        config:
          mode: "ip"
          algorithm: "sliding_window"
          rate: 100
          burst: 20
```
