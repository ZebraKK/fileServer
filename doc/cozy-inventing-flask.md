# 重构 Business Server 路由架构（参数化流程）

## Context

fileserver 的本质是"以 key 响应请求"，domain 只是计算 key 的一个维度，
不是路由分发的目的地。当前 `DomainRouter + DomainHandler` 的虚拟主机分发模式
在概念上是错的，且把 pipeline / puller 预先实例化到 per-domain handler 中，
违反了"流程通用、参数化"的设计原则。

目标：
- 单一 "/" handler，所有请求经同一流程处理
- pipeline / puller 做成通用无状态组件
- domain cfg 在整个调用链中作为参数传递
- 流程上下文（PipelineContext）显式返回 + 写入 r.Context()，两者并存

---

## 调整后的架构

```
chi.Router "/"
  └── Handler.ServeHTTP(w, r)
        ├── host = stripPort(r.Host)
        ├── cfg = domainMap.Load()[host]  → 404 if missing
        │
        ├── pCtx, r, ok = pipeline.Execute(cfg.Plugins, domain, w, r)
        │       ├── 每个 plugin: Execute(pCtx, pluginCfg, domain, r) bool
        │       ├── 有状态 plugin（rate_limit）按 domain key 自管理内部 map
        │       ├── pCtx 显式返回（调用者直接使用）
        │       └── pCtx 同时写入 r.Context()（跨层可访问）
        │       if !ok → return (已写入响应，如 429)
        │
        ├── key = keyBuilder.Build(domain, pCtx.RewrittenPath, r, cfg.KeyRules)
        │
        ├── flush check: if flushStore.Match(domain, pCtx.RewrittenPath) newer than meta.WrittenAt
        │       → cache.Delete(key)
        │
        ├── body, meta, err = cache.GetOrFetch(key, func() {
        │       return puller.Pull(context.Background(), cfg.Origins,
        │                          cfg.OriginTimeout, cfg.OriginRetry, ...)
        │   })
        │
        └── write response
              applyResponseMutations(w, pCtx.ResponseMutations)
              copy origin headers, set X-Cache, stream body
```

---

## 各组件改动

### 1. `internal/domain/router.go` → 重构为单一 `Handler`

删除 `DomainRouter` 和 `DomainHandler`，替换为：

```go
type Handler struct {
    domains    atomic.Pointer[map[string]config.DomainConfig]
    cache      *cache.SingleflightCache
    keyBuilder *cache.KeyBuilder
    flushStore *flush.Store
    puller     *origin.Puller     // 共享，Origins 作为参数传入 Pull()
    pipeline   *pipeline.Pipeline // 共享，pluginConfigs 作为参数传入 Execute()
    metrics    *observe.Metrics
    logger     *slog.Logger
}

func NewHandler(deps Deps) *Handler { ... }

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { ... }

// 热更新：原子替换 domain config map
func (h *Handler) Update(domains []config.DomainConfig) {
    m := make(map[string]config.DomainConfig, len(domains))
    for _, d := range domains { m[d.Domain] = d }
    h.domains.Store(&m)
}
```

### 2. `internal/pipeline/pipeline.go` → 参数化 + 双输出

```go
type PipelineContext struct {
    RewrittenPath     string
    ResponseMutations []HeaderMutation
    // 可扩展：auth claims, trace tags, custom metadata...
}

type ctxKey struct{}

// Execute 返回 pCtx 和更新后的 *http.Request（已写入 ctx）
func (p *Pipeline) Execute(
    pluginConfigs []config.PluginConfig,
    domain string,
    w http.ResponseWriter,
    r *http.Request,
) (*PipelineContext, *http.Request, bool) {
    pCtx := &PipelineContext{RewrittenPath: r.URL.Path}
    for _, pcfg := range pluginConfigs {
        plugin := p.registry[pcfg.Type]
        if !plugin.Execute(pCtx, pcfg.Config, domain, r) {
            return pCtx, r, false
        }
    }
    // 写入 r.Context() 供跨层访问
    r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, pCtx))
    return pCtx, r, true
}

// 辅助函数：从 context 取出（middleware/子函数使用）
func PipelineCtxFrom(ctx context.Context) (*PipelineContext, bool) {
    v, ok := ctx.Value(ctxKey{}).(*PipelineContext)
    return v, ok
}
```

Plugin interface 调整：

```go
type Plugin interface {
    Execute(pCtx *PipelineContext, cfg map[string]any, domain string, r *http.Request) bool
}
```

**rate_limit plugin**：内部维护 `sync.Map[string, *bucketState]`（key = domain 或 domain+IP），
按 domain 参数查找/创建状态，完全自治。

**url_rewrite / header plugin**：本身无状态，接收 cfg 参数直接执行，无需变化。

### 3. `internal/origin/puller.go` → Pull 参数化

```go
// 改为：origins / timeout / retry 作为参数
func (p *Puller) Pull(
    ctx context.Context,
    origins []string,
    timeout time.Duration,
    retry int,
    path string,
    header http.Header,
) (*Response, error)
```

Puller 本身只持有共享 `*http.Client`，无任何 domain 状态。

### 4. `cmd/server/main.go` → 简化 wiring

```go
// 移除 domain.Deps 中 per-domain puller/pipeline 的构建
handler := domain.NewHandler(domain.Deps{
    Cache:      sfCache,
    KeyBuilder: keyBuilder,
    FlushStore: flushStore,
    Puller:     origin.NewPuller(httpClient),
    Pipeline:   pipeline.New(factoryRegistry),
    Metrics:    metrics,
    Logger:     logger,
})
handler.Update(cfg.Domains)
// 热更新回调改为 handler.Update(newCfg.Domains)
```

### 5. `internal/server/server.go`

`bizR.Mount("/", handler)` 不变，类型从 `*domain.DomainRouter` 改为 `*domain.Handler`。

---

## 不变的部分

- `atomic.Pointer` 热更新机制（Update 原子替换 domain map）
- `KeyBuilder.Build` 签名不变（已是参数化）
- `cache.SingleflightCache` 不变
- `flush.Store` 不变
- chi middleware 链（RequestID / Recovery / RequestLogger）不变
- Admin server 不变

---

## 后续优化（本次不做）

- 按 domain 缓存 pipeline 中编译态（如 regexp 预编译）
- Puller 连接池按 origin host 分组复用
- rate_limit 内部 map 的过期清理（GC idle domain 的桶）
- `PipelineContext` 中增加更多字段（auth、trace 等）时无需改 Execute 签名

---

## 关键文件

| 文件 | 改动 |
|---|---|
| `internal/domain/router.go` | 核心重构：删 DomainRouter/DomainHandler，建 Handler |
| `internal/pipeline/pipeline.go` | Execute 参数化（pluginConfigs + domain），双输出 + ctx 注入 |
| `internal/pipeline/ratelimit/ratelimit.go` | Execute 接收 domain，内部 sync.Map 按 domain 管理状态 |
| `internal/pipeline/rewrite/rewrite.go` | Execute 接收 cfg + domain 参数 |
| `internal/pipeline/header/header.go` | Execute 接收 cfg + domain 参数 |
| `internal/origin/puller.go` | Pull 参数化（origins / timeout / retry） |
| `cmd/server/main.go` | wiring 简化，移除 per-domain deps 构建 |
| `internal/server/server.go` | 类型名更新 |

---

## Verification

1. `go build ./...` 无编译错误
2. 单域名请求：`curl -H "Host: example.com" http://localhost:8080/path` 正常缓存返回
3. 未注册域名：返回 404
4. url rewrite：请求 `/old-path` 被重写，缓存 key 使用 rewritten path
5. rate_limit：多 domain 并发请求，各 domain 限流状态互不影响
6. 热更新：SIGHUP 后新增/删除 domain 立即生效
7. `go test ./integration/...` 通过
