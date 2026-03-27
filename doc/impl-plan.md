# 开发实施计划：CDN Cache 服务

## 项目路径
`/Users/xiaowyu/xwill/fileServer`

## 已确认决策

| 决策项 | 结论 |
|--------|------|
| 入口文件 | 迁移到 `cmd/server/main.go`，根目录 main.go 删除 |
| HTTP 框架 | 换用 **chi**（移除 gin，添加 github.com/go-chi/chi/v5） |
| Storage 实现 | 基于 `os` 标准库的文件系统实现（开发用，后续替换专有库） |
| 开发节奏 | 按 Phase 逐步，每 Phase 完成后确认后再继续 |

---

## 依赖变更

```bash
# 移除
go get -u 不用，直接从 go.mod 中移除 gin 相关依赖（手动清理）

# 添加
go get github.com/go-chi/chi/v5
go get golang.org/x/time/rate
go get github.com/prometheus/client_golang/prometheus
go get github.com/prometheus/client_golang/prometheus/promhttp
go get github.com/fsnotify/fsnotify
```

---

## Phase 1：项目骨架 & 基础层

**目标**：跑通最小服务，日志 metrics 可用，可接受请求。

### 任务列表

**1-A 目录重组**
- 删除 `main.go`（根目录）
- 创建 `cmd/server/` 目录
- 创建 `internal/` 下各子目录（config, storage, observe, server, pipeline, cache, origin, flush, domain, admin）

**1-B 依赖更新**
- 更新 go.mod：移除 gin，添加 chi/prometheus/fsnotify/x-time

**1-C `internal/storage/storage.go`**
```go
type Storage interface {
    Read(key string) (io.ReadCloser, *Metadata, error)
    Write(key string, r io.Reader, meta *Metadata) error
    Delete(key string) error
    Exists(key string) bool
    Stat(key string) (*FileInfo, error)
    List(prefix string) ([]string, error)
}
```

**1-D `internal/storage/metadata.go`**
- Metadata struct（ContentType, CacheControl, Expires, CustomMeta map）
- FileInfo struct（Size, ModTime）

**1-E `internal/storage/localfs.go`** — 文件系统实现
- 以 key 的 SHA256 hex 作为文件名（避免特殊字符）
- Metadata 序列化为同名 `.meta` JSON 文件
- `List(prefix)` 通过扫描目录实现
- 配置一个 root 目录（从 config 读取）

**1-F `internal/config/config.go`** — 配置结构（见 code-structure.md）
```go
type Config struct { Server, Admin, Storage, KeyRules, Domains... }
```
Storage 配置新增：
```go
type StorageConfig struct {
    Type    string  // "localfs" | "custom"
    RootDir string  // localfs 使用
}
```

**1-G `internal/config/loader.go`** — YAML 解析
- `Load(path) (*Config, error)`
- 热加载暂缓，Phase 4 补充

**1-H `internal/observe/logger.go`**
- slog JSON handler，默认输出到 stdout
- `NewLogger()` 创建全局 logger
- `WithRequestID(ctx, id) context.Context` 注入 request_id

**1-I `internal/observe/metrics.go`**
- 注册所有 Prometheus metrics（Counter/Gauge/Histogram）
- 提供 `RecordRequest()`, `RecordCacheHit()`, `RecordOriginPull()` 等函数

**1-J `internal/server/middleware.go`**
- `RequestID` middleware（生成 UUID，注入 ctx 和响应头）
- `Recovery` middleware（panic → 500）
- `Logging` middleware（请求完成后写结构化日志）
- `Metrics` middleware（记录耗时 + 状态码到 Prometheus）

**1-K `cmd/server/main.go`** — 启动骨架
```go
func main() {
    cfg := config.Load(...)
    storage := storage.NewLocalFS(cfg.Storage.RootDir)
    r := chi.NewRouter()
    r.Use(middleware.RequestID, middleware.Recovery, middleware.Logging, middleware.Metrics)
    r.Get("/metrics", promhttp.Handler())
    r.Get("/*", func(w http.ResponseWriter, r *http.Request) {
        w.WriteHeader(200)
        w.Write([]byte("TODO: domain router"))
    })
    srv := &http.Server{Addr: cfg.Server.Addr, Handler: r}
    // SIGTERM 优雅关闭
}
```

**验收标准**：
```bash
go build ./cmd/server
./server -config config.example.yaml
curl http://localhost:8080/       # → 200 "TODO: domain router"
curl http://localhost:9090/metrics  # → Prometheus 格式输出
```

---

## Phase 2：缓存核心

**目标**：cache miss → 回源 → 写缓存 → 再次命中完整流程。

### 任务列表

**2-A `internal/origin/puller.go`**
- `type Puller struct { client *http.Client }`
- `Pull(ctx, origins []string, req *http.Request) (*http.Response, error)`
- 原子计数器 Round Robin 选 origin
- 超时通过 `context.WithTimeout`
- 失败重试（切换到下一个 origin）
- 单元测试：轮询顺序、超时行为、重试逻辑（用 httptest）

**2-B `internal/flush/store.go`**
- `FlushRule{ID, Domain, Prefix, CreatedAt}`
- `Store{mu, rules map[string][]FlushRule, storage Storage}`
- `AddRule(domain, prefix string) error` — 写内存 + 持久化
- `Match(domain, path string) *FlushRule` — 返回匹配的最新规则
- `Load() error` — 从 `__flush_rules__` 反序列化
- `Cleanup(maxAge time.Duration) error`
- 单元测试：规则匹配、持久化/恢复

**2-C `internal/cache/key.go`**
- `KeyBuilder{global KeyRulesConfig}`
- `Build(domain, rewrittenPath string, req *http.Request, domainRules *KeyRulesConfig) string`
  - 有效规则：domainRules != nil 则用之，否则 global
  - URL 参数：取 include_query_params 中的参数，名称排序后拼接
  - Header：取 include_headers 中的值，拼接后 SHA1
  - 最终：`domain:rewrittenPath?sortedQuery#headerHash`
- 单元测试：规范化、两级覆盖、边界（空参数、空 header）

**2-D `internal/cache/cache.go`**
- `Cache` 接口（Get/Set/Delete/Stat）
- `CacheMeta{WrittenAt, TTL, Size, Headers}`
- `LRUCache` 实现（container/list + map + sync.RWMutex）
  - 容量：MaxItems（配置项）
  - TTL 检查：Get 时检查，过期 → 删除 Storage 文件 + 返回 miss
  - LRU 淘汰：写入超容量时淘汰链表尾部，同时删除 Storage 文件
- TTL 决策函数 `ParseTTL(resp *http.Response, defaultTTL time.Duration) time.Duration`
- 单元测试：LRU 淘汰顺序、TTL 过期检查

**2-E `internal/cache/singleflight.go`**
- `SingleflightCache{cache Cache, group singleflight.Group}`
- `GetOrFetch(key string, fetch func() (io.Reader, *CacheMeta, error)) (io.ReadCloser, *CacheMeta, error)`
  - 先 Get，hit 则返回
  - Miss 则 `group.Do(key, fetch)`，结果写入 cache，返回给所有等待者
- 单元测试：并发 miss 只触发一次 fetch（goroutine + sync）

**验收标准**：
- `go test ./internal/cache/... ./internal/flush/... ./internal/origin/... -v -race`
- 覆盖率 ≥ 80%

---

## Phase 3：插件系统

**目标**：三个插件全部可用，可按域名配置插件链。

### 任务列表

**3-A `internal/pipeline/pipeline.go`**
```go
type PipelineContext struct {
    RewrittenPath  string
    ResponseHeader http.Header  // 响应头规则暂存
}

type Plugin interface {
    Name() string
    Handle(ctx context.Context, pCtx *PipelineContext, w http.ResponseWriter, r *http.Request) (next bool)
}

type Pipeline struct{ plugins []Plugin }
func (p *Pipeline) Execute(ctx, pCtx, w, r) bool
```
- 工厂函数 `Build(configs []PluginConfig) (*Pipeline, error)`

**3-B `internal/pipeline/ratelimit/ratelimit.go`**
- 令牌桶：`golang.org/x/time/rate.Limiter`，per-domain 或 per-IP
- 滑动窗口：手动实现（环形队列记录时间戳）
- `sync.Map[key → limiter]` + 定时清理（time.Ticker）
- 单元测试：速率、IP 隔离、mode 切换

**3-C `internal/pipeline/rewrite/rewrite.go`**
- 加载时预编译正则
- 顺序匹配，第一条命中写 `pCtx.RewrittenPath`
- 单元测试：匹配/不匹配、替换结果

**3-D `internal/pipeline/header/header.go`**
- `HeaderRule{Op, Key, Value}`（Op: set/add/del）
- 请求头：立即应用 `r.Header`
- 响应头：写入 `pCtx.ResponseHeader`，返回时由上层调用 `apply(w)`
- 单元测试：三种操作正确性

**验收标准**：
- `go test ./internal/pipeline/... -v -race`
- 各插件单元测试全通过

---

## Phase 4：服务层组装

**目标**：完整请求链路打通，热加载可用，admin API 可用。

### 任务列表

**4-A `internal/domain/router.go`**
```go
type DomainRouter struct {
    handlers atomic.Pointer[map[string]*DomainHandler]  // 原子替换
}
func (r *DomainRouter) ServeHTTP(w http.ResponseWriter, req *http.Request)
func (r *DomainRouter) Update(domains []DomainConfig, deps DomainDeps)
```
- `DomainHandler`：持有 pipeline、cache（SingleflightCache）、puller、flushStore、keyBuilder
- ServeHTTP 流程：
  1. 取域名配置，404 if not found
  2. 执行 pipeline
  3. 构造 cache key
  4. 检查 FlushRule
  5. cache.GetOrFetch（fetch = puller.Pull + cache.Set）
  6. 应用响应头规则 + 写响应

**4-B `internal/admin/handler.go`**
- chi Router 注册 4 个接口
- `POST /admin/flush/url`：从 URL 构造 key → cache.Delete + storage.Delete
- `POST /admin/flush/prefix`：flushStore.AddRule(domain, prefix)
- `POST /admin/flush/domain`：flushStore.AddRule(domain, "/")
- `GET  /admin/stat`：cache.Stat + flushStore.Match → JSON 响应

**4-C `internal/server/server.go`**
- 创建业务 server（chi Router，含 domain router）
- 创建 admin server（chi Router，含 admin handler + /metrics）
- `Start()` 并发运行两个 server
- `Shutdown(ctx)` 优雅关闭

**4-D `internal/config/loader.go` — 补充热加载**
- `Watch(path, onChange func(*Config))`：fsnotify 监听
- SIGHUP handler（`signal.Notify`）
- 热加载回调：`domainRouter.Update(newCfg.Domains, deps)`

**4-E `cmd/server/main.go` — 完整组装**
- 初始化：storage → flushStore（Load） → keyBuilder → cache → puller
- 构建 domainRouter（含 pipeline per domain）
- 启动热加载 watcher
- 启动服务，等待 SIGTERM

**验收标准**：
```bash
# 启动服务
./server -config config.example.yaml

# 触发回源
curl -H "Host: cdn.example.com" http://localhost:8080/file.txt
# 日志输出 cache_hit=false origin_pull=true

# 再次请求命中缓存
curl -H "Host: cdn.example.com" http://localhost:8080/file.txt
# 日志输出 cache_hit=true origin_pull=false

# 前缀刷新
curl -X POST http://localhost:9090/admin/flush/prefix \
  -d '{"domain":"cdn.example.com","prefix":"/"}'

# 再次请求 → 重新回源
curl -H "Host: cdn.example.com" http://localhost:8080/file.txt
# 日志输出 cache_hit=false origin_pull=true
```

---

## Phase 5：集成测试接入 & 收尾

**目标**：所有集成测试通过，项目可交付。

### 任务列表

**5-A 更新 `integration/cases/cache_test.go`**
- 替换 `buildService` stub → 使用真实 `server.New(cfg, deps)`
- 解注释各 Test 函数中的断言
- TC-06 实现（共享 LocalFS storage，重建 server）

**5-B `go test` 全通过**
```bash
go test ./... -v -count=1 -race
```

**5-C 补充 `config.example.yaml`**

**5-D 压测验收**
```bash
# 预热
curl -H "Host: cdn.example.com" http://localhost:8080/file.txt

# 全命中压测
hey -n 100000 -c 100 -H "Host: cdn.example.com" http://localhost:8080/file.txt
# 期望：P99 < 10ms，QPS > 10,000
```

---

## 文件创建/修改清单

| 文件 | 操作 | Phase |
|------|------|-------|
| `main.go` | 删除 | 1 |
| `cmd/server/main.go` | 新建 | 1 |
| `internal/storage/storage.go` | 新建 | 1 |
| `internal/storage/metadata.go` | 新建 | 1 |
| `internal/storage/localfs.go` | 新建 | 1 |
| `internal/config/config.go` | 新建 | 1 |
| `internal/config/loader.go` | 新建 | 1 |
| `internal/observe/logger.go` | 新建 | 1 |
| `internal/observe/metrics.go` | 新建 | 1 |
| `internal/server/middleware.go` | 新建 | 1 |
| `internal/origin/puller.go` | 新建 | 2 |
| `internal/flush/store.go` | 新建 | 2 |
| `internal/cache/key.go` | 新建 | 2 |
| `internal/cache/cache.go` | 新建 | 2 |
| `internal/cache/singleflight.go` | 新建 | 2 |
| `internal/pipeline/pipeline.go` | 新建 | 3 |
| `internal/pipeline/ratelimit/ratelimit.go` | 新建 | 3 |
| `internal/pipeline/rewrite/rewrite.go` | 新建 | 3 |
| `internal/pipeline/header/header.go` | 新建 | 3 |
| `internal/domain/router.go` | 新建 | 4 |
| `internal/admin/handler.go` | 新建 | 4 |
| `internal/server/server.go` | 新建 | 4 |
| `internal/config/loader.go` | 更新（热加载） | 4 |
| `cmd/server/main.go` | 更新（完整组装） | 4 |
| `integration/cases/cache_test.go` | 更新（接入真实服务） | 5 |
| `config.example.yaml` | 新建 | 5 |
