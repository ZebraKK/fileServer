# CDN 节点文件资源管理服务 — 设计文档

> 基于需求文档 `cdncache` 编写，Go 实现，单节点部署，模块化架构。

---

## 第一章：功能设计

### 1.1 多域名管理

服务使用 **Domain Registry** 维护所有域名配置，每个域名拥有独立的配置对象：

```yaml
# 域名配置结构（概念）
domain: "cdn.example.com"
origins:
  - "http://origin1.example.com"
  - "http://origin2.example.com"
origin_timeout: 10s
origin_retry: 2
default_ttl: 3600s
plugins:               # 该域名的插件链（按顺序执行）
  - type: rate_limit
    config: { ... }
  - type: url_rewrite
    config: { ... }
key_rules:             # 缓存 key 附加参数规则（覆盖全局默认）
  include_headers: ["Accept-Language"]
  include_query_params: ["version", "format"]
```

**热加载机制**：
- 监听配置文件变化（`fsnotify`）或 SIGHUP 信号
- 重新解析配置后，原子替换 Domain Registry 中的配置指针
- 正在处理中的请求继续使用旧配置直到完成，不受影响

---

### 1.2 HTTP 请求处理流水线（插件化）

#### Plugin 接口

```go
type Plugin interface {
    Name() string
    // 返回 false 表示短路，请求不再继续
    Handle(ctx context.Context, w http.ResponseWriter, r *http.Request) bool
}
```

#### 执行流程

```
请求进入
  │
  ├─ 依次执行插件链中每个插件
  │    如果任一插件返回 false（如限流），立即终止，不进入缓存/回源逻辑
  │
  └─ 所有插件通过后 → 进入缓存层
```

#### 内置插件：限流

- **按域名**：该域名所有请求共享一个令牌桶，超出速率返回 429
- **按客户端 IP**：每个 IP 独立令牌桶，使用 sync.Map 存储（配合定期清理过期条目）
- 支持两种算法（配置选择）：
  - **令牌桶（Token Bucket）**：允许突发流量，适合一般限流
  - **滑动窗口（Sliding Window）**：平滑限流，适合严格控制

#### 内置插件：URL 改写

- 配置若干条改写规则，每条规则包含：正则表达式 + 替换模板
- 规则按配置顺序匹配，第一条命中后停止（可配置是否继续匹配）
- 正则表达式在配置加载时预编译并缓存（`regexp.MustCompile`）
- 改写后的路径替换 `r.URL.Path`，后续缓存 key 构造使用改写后的路径

```yaml
rules:
  - match: "^/v1/(.*)"
    replace: "/api/$1"
  - match: "^/legacy/(.*)"
    replace: "/archive/$1"
```

#### 内置插件：请求头/响应头处理

支持三种操作类型，分别作用于请求头和响应头：

| 操作 | 说明 |
|------|------|
| `set` | 设置指定 header（存在则覆盖） |
| `add` | 追加 header（允许同名多值） |
| `del` | 删除指定 header |

响应头处理在缓存命中和回源两种路径上均生效（写入前/返回前处理）。

---

### 1.3 缓存

#### 缓存 Key 构造算法

```
key = domain + ":" + rewritten_path + "?" + normalized_query + "#" + header_hash
```

各部分说明：
- `domain`：请求的 Host header 值（已去端口）
- `rewritten_path`：URL 改写插件处理后的路径
- `normalized_query`：将配置中指定的 URL 参数**名称排序后**拼接（未指定参数忽略）
- `header_hash`：将配置中指定的请求 header 值拼接后做 hash（如 `sha1(Accept-Language=zh-CN)`）

**两级 key 参数配置**：

```yaml
# 全局默认（global）
key_rules:
  include_query_params: []
  include_headers: []

# 域名配置（domain）可覆盖全局
key_rules:
  include_query_params: ["version"]
  include_headers: ["Accept-Language"]
```

域名配置存在时完全覆盖全局配置（非合并）。

#### LRU + TTL 双淘汰

- **内存索引**：维护一个 LRU 链表，记录所有缓存 key 及其元数据（写入时间、TTL、大小）
- **磁盘存储**：实际文件内容通过 Storage 接口读写
- **TTL 检查**：请求时检查内存索引中的写入时间 + TTL，过期则视为 miss
- **LRU 淘汰**：内存索引达到容量上限时，淘汰最久未使用的条目（同时删除磁盘文件）
- **内存索引持久化**：服务启动时从磁盘重建索引（扫描 Storage 中的 Stat 信息）

#### 缓存击穿防护（singleflight）

```
并发 N 个请求同时 miss 同一 key：
  ├─ 第 1 个请求触发回源，注册 singleflight group
  ├─ 第 2~N 个请求检测到 group 存在，等待
  └─ 回源完成后，所有等待请求同时获得结果
```

使用 `golang.org/x/sync/singleflight`，以 cache key 为 group key。

#### TTL 决策优先级

1. 响应 `Cache-Control: max-age=N` → 使用 N 秒
2. 响应 `Expires` header → 计算剩余秒数
3. 以上均无 → 使用域名 `default_ttl` 配置
4. 若响应 `Cache-Control: no-store` 或 `no-cache` → 不缓存，直接透传

---

### 1.4 回源（Origin Pull）

#### 多源轮询（Round Robin）

每个域名配置的 origin 列表按 Round Robin 策略轮流使用。轮询状态存储在域名配置对象中（原子计数器），线程安全。

#### 回源流程

```
1. 从域名配置中取下一个 origin 地址
2. 构造回源请求（复制原始请求的 Method、Headers，替换 Host 和 Path）
3. 发起 HTTP 请求（使用配置的 timeout）
4. 失败时重试（使用配置的 retry 次数，切换到下一个 origin）
5. 成功后：
   a. 根据响应决定是否缓存（检查 Cache-Control）
   b. 写入 Storage
   c. 更新内存索引
   d. 返回响应给客户端
```

---

### 1.5 缓存管理 API

所有管理接口统一在 `/admin` 路由前缀下，建议配置独立监听端口（与业务端口分离）。

#### 刷新接口

| 接口 | 方法 | 说明 |
|------|------|------|
| `POST /admin/flush/url` | `{"url": "https://cdn.example.com/path/file.js"}` | 精确 URL 刷新：删除对应缓存文件 + 清除内存索引 |
| `POST /admin/flush/prefix` | `{"domain": "cdn.example.com", "prefix": "/static/"}` | 前缀刷新：写入 FlushRule（懒加载） |
| `POST /admin/flush/domain` | `{"domain": "cdn.example.com"}` | 整域名刷新：写入 FlushRule（prefix = "/"） |

#### FlushRule 懒加载机制

前缀/目录刷新不立即扫描删除文件，而是写入一条 FlushRule：

```go
type FlushRule struct {
    Domain    string
    Prefix    string
    CreatedAt time.Time
}
```

**规则持久化**：将所有 FlushRule 序列化（JSON）后，通过 Storage 接口以专有 key（如 `__flush_rules__`）写入磁盘。服务启动时读取并恢复到内存。

**请求时匹配**：
```
缓存命中后，检查该 key 对应的 domain+path 是否匹配任一 FlushRule：
  如果匹配到规则，且 rule.CreatedAt > cache.WrittenAt
  → 视为 miss，重新回源
```

匹配算法：遍历该 domain 下的所有规则，检查 path 是否以 rule.Prefix 开头。规则数量通常极少（O(n) 遍历足够），无需复杂数据结构。

**规则清理**：可配置 FlushRule 的最大保留时长（如 7 天），定期清理过期规则。

#### 状态查询接口

```
GET /admin/stat?url=https://cdn.example.com/path/file.js

响应：
{
  "hit": true,
  "written_at": "2026-03-22T10:00:00Z",
  "expires_at": "2026-03-22T11:00:00Z",
  "ttl_remaining": 3456,
  "size": 102400,
  "flush_rule_match": false
}
```

---

### 1.6 可观测性

#### 结构化日志（slog）

每次请求记录以下字段：

| 字段 | 类型 | 说明 |
|------|------|------|
| `time` | string | ISO8601 时间戳 |
| `domain` | string | 请求域名 |
| `method` | string | HTTP 方法 |
| `path` | string | 原始路径 |
| `rewritten_path` | string | 改写后路径（如有） |
| `status` | int | 响应状态码 |
| `cache_hit` | bool | 是否命中缓存 |
| `origin_pull` | bool | 是否触发回源 |
| `latency_ms` | int | 响应耗时（毫秒） |
| `bytes` | int | 响应体大小 |
| `client_ip` | string | 客户端 IP |
| `request_id` | string | 唯一请求 ID |

#### Prometheus Metrics

| Metric | 类型 | 标签 | 说明 |
|--------|------|------|------|
| `cdncache_requests_total` | Counter | `domain`, `status` | 请求总数 |
| `cdncache_cache_hits_total` | Counter | `domain` | 缓存命中数 |
| `cdncache_cache_misses_total` | Counter | `domain` | 缓存未命中数 |
| `cdncache_origin_pulls_total` | Counter | `domain`, `origin`, `status` | 回源次数 |
| `cdncache_origin_pull_duration_seconds` | Histogram | `domain` | 回源耗时分布 |
| `cdncache_request_duration_seconds` | Histogram | `domain`, `cache_hit` | 请求耗时分布 |
| `cdncache_cache_storage_bytes` | Gauge | `domain` | 缓存占用磁盘空间 |
| `cdncache_plugin_triggered_total` | Counter | `domain`, `plugin`, `result` | 插件触发次数 |
| `cdncache_flush_rules_total` | Gauge | `domain` | 当前有效刷新规则数 |

Metrics 通过 `/metrics` 路径暴露，供 Prometheus 拉取。

---

## 第二章：服务架构

### 2.1 组件关系

```
┌─────────────────────────────────────────────────────────────┐
│                        CDN Node Service                      │
│                                                             │
│   ┌──────────────┐    ┌─────────────────────────────────┐  │
│   │  Admin API   │    │         Business HTTP Server     │  │
│   │  /admin/*    │    │                                 │  │
│   │  /metrics    │    │  ┌──────────────────────────┐  │  │
│   └──────┬───────┘    │  │     Domain Router        │  │  │
│          │            │  │  (Host header → config)  │  │  │
│          │            │  └───────────┬──────────────┘  │  │
│          │            │              │                   │  │
│          │            │  ┌───────────▼──────────────┐  │  │
│          │            │  │    Plugin Pipeline       │  │  │
│          │            │  │  RateLimit → Rewrite →  │  │  │
│          │            │  │  HeaderProcessor         │  │  │
│          │            │  └───────────┬──────────────┘  │  │
│          │            │              │                   │  │
│          │            │  ┌───────────▼──────────────┐  │  │
│          │            │  │      Cache Layer         │  │  │
│          │            │  │  (LRU+TTL + FlushRule)  │  │  │
│          │            │  └──────┬────────┬──────────┘  │  │
│          │            │    HIT  │   MISS │              │  │
│          │            │         │        │              │  │
│          │            │         │  ┌─────▼────────┐   │  │
│          │            │         │  │  singleflight│   │  │
│          │            │         │  └─────┬────────┘   │  │
│          │            │         │        │             │  │
│          │            │         │  ┌─────▼────────┐   │  │
│          │            │         │  │ Origin Puller│   │  │
│          │            │         │  │ (RoundRobin) │   │  │
│          │            │         │  └─────┬────────┘   │  │
│          │            └─────────│────────│─────────────┘  │
│          │                      │        │                  │
│          └──────────────────────┼────────┤                  │
│                                 │        │                  │
│          ┌──────────────────────▼────────▼──────────────┐  │
│          │              Storage Interface                │  │
│          │         (专有库实现 Read/Write/Delete/...)    │  │
│          └───────────────────────────────────────────────┘  │
│                                                             │
│          ┌───────────────────────────────────────────────┐  │
│          │         Config Manager (YAML + hot reload)    │  │
│          └───────────────────────────────────────────────┘  │
│                                                             │
│          ┌───────────────────────────────────────────────┐  │
│          │         Observability (slog + Prometheus)     │  │
│          └───────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────┘
```

### 2.2 请求数据流

```
HTTP Request (Host: cdn.example.com, GET /static/app.js?v=2)
  │
  ▼
[1] Domain Router
    → 查找 "cdn.example.com" 配置，未找到则 404
  │
  ▼
[2] Plugin Pipeline
    → RateLimit: 检查域名/IP 配额，超出返回 429
    → URLRewrite: /static/(.*) → /assets/$1，路径变为 /assets/app.js
    → HeaderProcessor: 注入 X-Cache-Node: node-01
  │
  ▼
[3] Cache Key 构造
    → key = "cdn.example.com:/assets/app.js?v=2"
    （假设配置了 include_query_params: ["v"]）
  │
  ▼
[4] Cache Lookup
    ├─ HIT（且未命中 FlushRule）
    │    → 直接从 Storage 读取文件内容
    │    → 应用响应头处理规则
    │    → 返回 200，记录 cache_hit=true 日志
    │
    └─ MISS / FlushRule 触发 / TTL 过期
         │
         ▼
[5] singleflight
    → 以 cache key 为 group，确保并发只有一个回源请求
         │
         ▼
[6] Origin Puller
    → Round Robin 选择 origin
    → 发送 GET http://origin1.example.com/assets/app.js?v=2
    → 超时/失败时重试，切换 origin
         │
         ▼
[7] 缓存写入（如果响应允许缓存）
    → 写入 Storage（Write）
    → 更新内存 LRU 索引
    → 返回响应，记录 origin_pull=true 日志
```

---

## 第三章：横向扩展方案对比

### 方案 A：各节点独立运行

每个 CDN 节点独立运行一个服务实例，节点间无通信，各自维护缓存和配置。

```
[负载均衡]
    │
    ├─ Node 1: cdncache（独立缓存）
    ├─ Node 2: cdncache（独立缓存）
    └─ Node 3: cdncache（独立缓存）
         ↑
    配置更新由外部工具逐节点推送（rsync/Ansible/配置管理系统）
```

| 维度 | 评估 |
|------|------|
| 部署复杂度 | ✅ 低：无额外依赖 |
| 运维复杂度 | ✅ 低：单进程，故障隔离好 |
| 配置一致性 | ⚠️ 弱：配置更新有时间窗口，节点间可能不一致 |
| 缓存效率 | ⚠️ 同一资源可能被多个节点各自回源 |
| Flush 管理 | ⚠️ 需要对每个节点分别调用 flush 接口 |
| 故障影响 | ✅ 节点故障不影响其他节点 |
| 水平扩容 | ✅ 无状态，直接加节点 |

**适合场景**：节点数较少（<20），对缓存一致性要求不极高，运维团队小。

---

### 方案 B：引入全局中心协调服务

新增一个中心服务（Control Plane），负责配置下发和缓存操作广播。

```
[控制平面 Control Plane]
    ├─ 配置存储与下发
    ├─ Flush 广播
    └─ 状态聚合（可选）
         │
    ┌────┴──────────────────────┐
    │                           │
[Node 1: cdncache]     [Node 2: cdncache]
    └─ 从 Control Plane 拉取/订阅配置
    └─ 接收 Flush 广播指令
```

| 维度 | 评估 |
|------|------|
| 部署复杂度 | ❌ 高：需要额外部署控制平面服务 |
| 运维复杂度 | ❌ 较高：控制平面本身需要高可用 |
| 配置一致性 | ✅ 强：中心统一下发，节点秒级同步 |
| 缓存效率 | ⚠️ 节点间缓存仍不共享，但可通过协调减少回源 |
| Flush 管理 | ✅ 一次调用，控制平面广播到所有节点 |
| 故障影响 | ❌ 控制平面故障时节点降级为只读（使用旧配置） |
| 水平扩容 | ✅ 节点仍可自由扩容 |

**适合场景**：节点数多（>20），需要强一致配置，有独立运维团队，flush 操作频繁。

---

### 推荐方案

**当前阶段采用方案 A**，但在以下位置预留扩展接口：

1. **配置加载**：抽象 `ConfigSource` 接口（本地文件 / 远程 HTTP / 消息队列），当前实现本地文件版本
2. **Flush 通知**：抽象 `FlushNotifier` 接口（本地处理 / 广播到集群），当前实现本地版本

未来切换到方案 B 时，只需替换这两个接口的实现，核心业务逻辑无需修改。

---

## 第四章：测试系统设计

### 4.1 单元测试

各包独立测试，使用 mock 隔离外部依赖：

| 包 | 测试重点 |
|----|---------|
| `cache/key.go` | key 构造规范化、两级配置覆盖逻辑 |
| `cache/cache.go` | LRU 淘汰、TTL 过期、FlushRule 命中判断 |
| `pipeline/ratelimit` | 令牌桶速率、per-IP 计数器独立性 |
| `pipeline/rewrite` | 正则匹配、替换结果、无匹配透传 |
| `pipeline/header` | set/add/del 操作正确性 |
| `origin/puller.go` | 多源轮询顺序、超时/重试逻辑 |
| `flush/store.go` | 规则写入/读取、持久化/恢复、过期清理 |
| `config/loader.go` | YAML 解析、热加载触发 |

**测试覆盖率目标**：核心包（cache、flush、pipeline）≥ 80%。

### 4.2 集成测试

见 `integration/` 目录。使用 `httptest.NewServer` 启动完整服务，mock Storage 接口（内存实现），包含 mini origin server（也用 httptest）。

核心测试场景（详见 `integration/README.md`）：
- cache miss → 回源 → 缓存写入 → 再次请求命中
- 并发 miss：singleflight 保证只有一次回源
- TTL 过期后重新回源
- FlushRule 懒加载：刷新后旧缓存被忽略，新请求触发回源
- FlushRule 持久化：模拟重启后规则仍有效
- 限流插件：超过配额返回 429
- URL 改写：改写后路径命中缓存
- 多域名隔离：A 域名的缓存不影响 B 域名

### 4.3 压测方案

**工具**：`hey` 或 `wrk`

**测试场景**：

| 场景 | 目的 | 配置 |
|------|------|------|
| 全命中 | 测试缓存读取吞吐量 | 预热后压测固定 URL |
| 高 miss 率 | 测试回源并发能力 | 随机 URL，缓存 TTL 极短 |
| 并发 miss（同一资源） | 验证 singleflight 防击穿效果 | 并发请求同一未缓存 URL |
| 混合流量 | 模拟真实场景 | 80% 命中 + 20% miss |
| 限流压测 | 验证限流插件在高并发下的正确性 | 超出配置速率的并发请求 |

**压测指标**：
- 全命中场景：P99 延迟 < 10ms，QPS > 10,000
- 回源场景：QPS 受 origin 限制，关注 singleflight 合并比例
- 内存/CPU 占用在压测期间稳定（无泄漏）
