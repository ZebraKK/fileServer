# Dev Log — CDN Cache Service

## 项目信息
- 模块名：`fileServer`
- 路径：`/Users/xiaowyu/xwill/fileServer`
- 参考文档：cdncache.md / design.md / code-structure.md / impl-plan.md

---

## Phase 1：项目骨架 & 基础层

**目标**：跑通最小服务，日志 metrics 可用，可接受请求。
**开始时间**：2026-03-22

---

### 1-A 目录重组

- [x] 删除根目录 `main.go`
- [x] 创建 `cmd/server/`
- [x] 创建 `internal/` 各子目录：storage, config, observe, server, pipeline/{ratelimit,rewrite,header}, cache, origin, flush, domain, admin

---

### 1-B 依赖更新

- [x] 移除 gin（go mod edit -droprequire）
- [x] 添加新依赖：chi/v5, fsnotify, prometheus/client_golang, x/time/rate
- [!] go mod tidy 因无源文件清空了 go.mod，依赖将在源文件写完后统一 `go get` + `tidy` 恢复

---

### 1-C~E storage 包

- [x] `internal/storage/metadata.go` — Metadata, FileInfo 结构
- [x] `internal/storage/storage.go` — Storage 接口（6 个方法）
- [x] `internal/storage/localfs.go` — 文件系统实现（SHA256 路径 + .meta JSON sidecar）
  - 注意：List() 依赖 meta 中 CustomMeta["key"] 字段，写入时需由 cache 层填充

---

### 1-F~G config 包

- [x] `internal/config/config.go` — Config/ServerConfig/AdminConfig/StorageConfig/CacheConfig/KeyRulesConfig/DomainConfig/PluginConfig，带 Defaults()
- [x] `internal/config/loader.go` — Load(path) YAML 解析（热加载 Phase 4 补充）
  - 使用 goccy/go-yaml（已在 go.mod）

---

### 1-H~I observe 包

- [x] `internal/observe/logger.go` — slog JSON logger，WithLogger/FromContext 注入 ctx
- [x] `internal/observe/metrics.go` — promauto 注册所有 Prometheus metrics（8 个）

---

### 1-J server/middleware.go

- [x] `internal/server/middleware.go` — RequestLogger（slog + Prometheus）、Recovery（panic→500）
  - 复用 chi middleware.GetReqID 获取 request ID（由 chi/middleware.RequestID 注入）
  - responseWriter 包装捕获 status code 和 bytes

---

### 1-K cmd/server/main.go

- [x] `cmd/server/main.go` — 启动骨架：读配置 → 初始化 LocalFS → chi router → 优雅关闭
- [x] `config.yaml` — 最小测试配置

### Phase 1 验收结果 ✅

```
go build ./cmd/server  → BUILD OK（无编译错误）
curl :8080/            → 200 "CDN cache service — Phase 1 skeleton"
curl :9090/healthz     → 200 "ok"
curl :9090/metrics     → cdncache_requests_total{...} 1
```

JSON 日志格式正确，request_id 注入正常，Prometheus metrics 注册并可拉取。

---

## Phase 2：缓存核心

**目标**：cache miss → 回源 → 写缓存 → 再次命中完整流程。
**开始时间**：2026-03-22

---

### 2-A origin/puller.go

- [x] `internal/origin/puller.go` — Round Robin + 超时 + 重试 + hop-by-hop header 过滤
- [x] 单元测试 4 项（轮询顺序、重试、超时、空 origin）全通过

### 2-B flush/store.go

- [x] `internal/flush/store.go` — FlushRule Add/Match/Load/Cleanup + JSON 持久化到 `__flush_rules__`
- [x] 单元测试 5 项（匹配、最新规则优先、持久化恢复、清理、整域名刷新）全通过

### 2-C~E cache 包

- [x] `internal/cache/key.go` — KeyBuilder：两级配置、URL 参数排序规范化、header SHA1 hash
- [x] `internal/cache/cache.go` — LRUCache（container/list + map + RWMutex）+ ParseTTL
- [x] `internal/cache/singleflight.go` — SingleflightCache：body 缓冲解决 reader 共享问题
- [x] 单元测试 11 项全通过（key 规范化、LRU 淘汰、TTL 过期、ParseTTL、singleflight 并发去重）
- [!] **问题**：`golang.org/x/sync` 版本缺少 go.sum 条目 → `go get golang.org/x/sync/singleflight` 解决

---

## Phase 3：插件系统

**目标**：三个插件全部可用，可按域名配置插件链。
**开始时间**：2026-03-23

---

### 3-A pipeline/pipeline.go

- [x] `internal/pipeline/pipeline.go` — Plugin 接口 + Pipeline.Execute + RegisterFactory/Build 工厂 + NewPipelineCtx + ApplyResponseMutations
- [!] **设计修正**：Ctx 最初只有 `ExtraResponseHeaders`，无法存储 `del` op（del 应用到空 map 无效）
  → 拆为 `SetResponseHeaders http.Header` + `DeleteResponseHeaders []string`，语义清晰

### 3-B ratelimit

- [x] `internal/pipeline/ratelimit/ratelimit.go` — 令牌桶（x/time/rate）+ 滑动窗口（环形时间戳）
  - per-domain 和 per-IP 两种 mode
  - 后台 goroutine 定期清理过期 IP 限流器（防内存泄漏）

### 3-C rewrite

- [x] `internal/pipeline/rewrite/rewrite.go` — 正则改写，配置时预编译，首条命中即停

### 3-D header

- [x] `internal/pipeline/header/header.go` — set/add/del 三种操作，请求头立即应用，响应头暂存 Ctx

### Phase 3 测试结果 ✅

```
go test ./internal/pipeline/... -v -race
10/10 PASS（rewrite×2, header×4, ratelimit×3, 未知插件错误×1）
注：macOS ld warning 为系统链接器兼容告警，非代码问题
```

---

## Phase 4：服务层组装

**目标**：完整请求链路打通，热加载，admin API 可用。
**开始时间**：2026-03-23

---

### 4-A domain/router.go

- [x] `internal/domain/router.go` — DomainHandler（pipeline → FlushRule 检查 → singleflight → origin pull）+ DomainRouter（atomic.Pointer 原子替换）
- [!] **Bug & 修复**：originFetch 复用了请求的 context，client 关闭连接导致 context cancel，origin pull 被中断
  → 修复：fetch 内用 `context.WithTimeout(context.Background(), ...)` 独立 context，不绑定请求生命周期

### 4-B admin/handler.go

- [x] `internal/admin/handler.go` — flush/url, flush/prefix, flush/domain, stat 四个接口

### 4-C server/server.go

- [x] `internal/server/server.go` — 业务 server（chi + middleware）+ admin server 双 server 管理

### 4-D config/loader.go 热加载

- [x] `internal/config/loader.go` — 补充 Watch()：fsnotify 文件监听 + SIGHUP 信号处理

### 4-E cmd/server/main.go 完整组装

- [x] `cmd/server/main.go` — 全量组装：storage → flush → cache → puller → router → admin → server → hot-reload watcher → 优雅关闭

### Phase 4 验收结果 ✅

```
1. 第一次请求 → X-Cache: miss  ✅
2. 第二次请求 → X-Cache: hit   ✅（latency 0ms）
3. 未知域名   → HTTP 404       ✅
4. flush prefix → HTTP 204     ✅
5. flush 后请求 → X-Cache: miss（重新回源） ✅
6. /admin/stat  → JSON含 hit/ttl/size ✅
7. /metrics     → cache_hits=1, cache_misses=2, origin_pulls=2 ✅

JSON 结构化日志、request_id、Prometheus metrics 全部正常
```

---

## Phase 5：集成测试接入 & 收尾

**目标**：integration/ 测试全部使用真实服务，config.example.yaml 补全。
**开始时间**：2026-03-23

---

### 5-A 集成测试接入

- [x] `integration/mock/storage_mock.go` — 修复：将 `mock.Metadata`/`mock.FileInfo` 替换为 `storage.Metadata`/`storage.FileInfo`（直接导入 internal 包），使 `*MemStorage` 满足 `storage.Storage` 接口
- [!] **编译问题**：原 mock 自定义了独立 Metadata/FileInfo 结构，与接口签名不匹配 → 直接改用 `storage` 包类型解决
- [x] `integration/cases/cache_test.go` — 完整重写：
  - 移除包级 `import_note := ""` 语句（非法 Go 语法）
  - 移除不存在的 `extractBizHandler`/`extractAdminHandler` 调用
  - 修复 `newHarness`：改为直接调用 `buildTestHandlers(cfg, router, adminHandler)` 组装测试 handler，不再依赖 `server.New`
  - 移除 `"fileServer/internal/server"` import（测试无需真实 server）
  - 添加 `noopLogger()` 工具函数
  - 补全 6 个测试用例（TC-01~TC-06）

### 5-A 测试结果 ✅

```
go test ./integration/cases/... -v -count=1 -race
TC-01 CacheMiss      PASS — X-Cache: miss，origin 调用 1 次
TC-02 CacheHit       PASS — X-Cache: hit，origin 调用 1 次（无重复回源）
TC-03 UnknownDomain  PASS — 404，origin 0 次调用
TC-04 FlushPrefix    PASS — 刷新后 X-Cache: miss，origin 调用 2 次
TC-05 Singleflight   PASS — 10 并发只触发 1 次 origin pull
TC-06 FlushURL       PASS — /a.txt 被单独驱逐，/b.txt 仍命中
```

### 5-B 全量回归 ✅

```
go test ./... -count=1 -race
integration/cases ✅  cache ✅  flush ✅  origin ✅  pipeline ✅
（ld: warning 为 macOS 系统链接器兼容性告警，非代码问题）
```

### 5-C config.example.yaml

- [x] 创建 `config.example.yaml`：含三个示例域名配置（rate limit、rewrite+header、IP 限速），覆盖所有插件和配置项

---

## Phase 5 验收结果 ✅

所有集成测试全通过，`go test ./... -race` 零失败，`config.example.yaml` 补全。项目可交付。

---

## Phase 6：架构重构 — 参数化流程（cozy-inventing-flask）

**目标**：删除 `DomainRouter + DomainHandler` 虚拟主机分发模式，替换为单一 `Handler`；pipeline / puller 改为通用无状态组件，domain cfg 在调用链中作为参数传递。
**开始时间**：2026-03-25

**核心变更清单**：
| 文件 | 改动 |
|---|---|
| `internal/pipeline/pipeline.go` | Ctx→PipelineContext；Plugin interface 改为 Execute(pCtx, cfg, domain, w, r)；Pipeline 持有 registry；Execute 返回 (pCtx, r, bool) + ctx 注入；移除 Build |
| `internal/pipeline/ratelimit/ratelimit.go` | 改为 singleton，per-domain 状态通过 sync.Map 自管理 |
| `internal/pipeline/rewrite/rewrite.go` | 改为 singleton，每次 Execute 从 cfg 参数解析规则 |
| `internal/pipeline/header/header.go` | 改为 singleton，每次 Execute 从 cfg 参数解析操作 |
| `internal/origin/puller.go` | Pull 参数化（path + header 替代 *http.Request，移除 counter 参数，内部维护 RR 计数器） |
| `internal/domain/router.go` | 删 DomainRouter/DomainHandler，建 Handler（持有 domains atomic.Pointer + 所有 deps） |
| `cmd/server/main.go` | wiring 简化：domain.NewHandler + handler.Update(cfg.Domains) + pipeline.New() |
| `internal/server/server.go` | 类型名 *DomainRouter → *Handler |
| `integration/cases/cache_test.go` | 适配新 domain.Handler API |

---

### 6-A pipeline/pipeline.go 重构

- [x] `Ctx` 重命名为 `PipelineContext`（导出字段不变：`RewrittenPath`、`SetResponseHeaders`、`DeleteResponseHeaders`）
- [x] 新增 `ctxKey` + `PipelineCtxFrom(ctx)` 辅助函数，供跨层访问 pCtx
- [x] `Plugin` 接口改为 `Execute(pCtx, cfg, domain, w, r) bool`，接收 `cfg map[string]any` 和 `domain string` 参数，移除 `Name()` 方法
- [x] `Pipeline` 结构从持有 `[]Plugin` 改为持有 `registry map[string]Plugin`（singleton 注册表）
- [x] 新增全局 `registry` + `RegisterPlugin(typeName, p)` 替代 `RegisterFactory`
- [x] `New() *Pipeline` 创建绑定全局注册表的 Pipeline
- [x] `Execute(pluginConfigs, domain, w, r) (*PipelineContext, *http.Request, bool)` — 返回 pCtx + 注入 pCtx 到 ctx 的新 r + bool
- [x] 移除 `Build`、`NewPipelineCtx` 函数
- [x] 保留 `ApplyResponseMutations`，参数更新为 `*PipelineContext`
- [!] **设计决定**：未知插件类型静默跳过（而非返回错误），符合"运行时参数化"语义；`TestUnknownPluginReturnsError` 测试更新为 `TestUnknownPluginIsSkipped`

### 6-B 插件包重构（singleton 模式）

- [x] `ratelimit`：改为 singleton，`init()` 注册 `&Plugin{}`；`Plugin` 持有 `sync.Map[domain → *perDomainState]`；`Execute` 从 cfg 解析 mode/algo/rate/burst，按 domain 查找/创建 per-domain 状态，IP 模式下 `cleanupOnce` 保证 GC goroutine 仅启动一次
- [x] `rewrite`：改为 stateless singleton，`Execute` 从 cfg 每次解析 rules 并编译正则（后续优化点：按 domain 缓存编译态）
- [x] `header`：改为 stateless singleton，`Execute` 从 cfg 每次解析 request/response ops
- [!] **问题**：`ratelimit` 原来的 `fromConfig` / `floatVal` / `intVal` 本地函数仍保留，新增 `stringVal` 辅助；`fmt.Sprintf` 占位符已清理

### 6-C origin/puller.go 参数化

- [x] 移除 `counter *atomic.Uint64` 参数，改在 `Puller` 结构体内持有共享 `counter atomic.Uint64`（跨所有 domain 的轮询计数器，不完全 per-domain RR 但可接受）
- [x] 移除 `*http.Request` 参数，改接收 `path string` + `header http.Header`
- [x] `originFetch` 调用者显式传入 `pCtx.RewrittenPath`（含 query string），修复了旧代码中未使用 rewritten path 的 bug
- [x] 更新 `puller_test.go` 适配新签名（移除 counter、req 参数）

### 6-D domain/router.go 核心重构

- [x] 删除 `DomainRouter`、`DomainHandler`、`newHandler`
- [x] 新建 `Handler` 结构体：`domains atomic.Pointer[map[string]config.DomainConfig]` + 所有 shared deps
- [x] `NewHandler(deps Deps) *Handler` — 初始化 handler，存入空 domain map
- [x] `Update(domains []config.DomainConfig)` — 原子替换 domain config map（热更新）
- [x] `ServeHTTP` 流程：域名 lookup → pipeline.Execute → key build → flush check → cache.GetOrFetch → write response
- [x] `originFetch` 预先捕获 `path`（含 query）和 `header.Clone()`，闭包不持有 `*http.Request` 引用

### 6-E wiring 更新

- [x] `cmd/server/main.go`：使用 `domain.NewHandler(deps)` + `handler.Update(cfg.Domains)` + `pipeline.New()`；热更新回调简化为 `handler.Update(newCfg.Domains)`
- [x] `internal/server/server.go`：`*domain.DomainRouter` → `*domain.Handler`
- [x] `integration/cases/cache_test.go`：适配新 `domain.Handler` API，补充 `pipeline.New()` 依赖

---

## Phase 6 验收结果 ✅

```
go build ./...          → BUILD OK（无编译错误）
go test ./... -race     → 全部通过（ld warning 为 macOS 系统链接器兼容告警，非代码问题）

integration/cases ✅（TC-01~TC-06 全通过）
cache ✅  flush ✅  origin ✅  pipeline ✅
```

架构验证：
- 单一 "/" handler，所有请求经同一流程处理 ✅
- pipeline / puller 通用无状态，domain cfg 全程作为参数传递 ✅
- rate_limit 内部 sync.Map 按 domain 自管理状态 ✅
- pCtx 显式返回 + 写入 r.Context()，两者并存 ✅
- 热更新：handler.Update 原子替换 domain map，无锁 ✅

