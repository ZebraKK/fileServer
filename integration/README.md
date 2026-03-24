# 集成测试设计文档

## 测试目标

验证 CDN 节点服务各功能在真实 HTTP 请求链路下的端到端行为，包括：
- 缓存命中与未命中的完整流程
- singleflight 防击穿效果
- FlushRule 懒加载与持久化
- 插件链的执行效果
- 多域名隔离

## 测试环境

### 依赖组件

| 组件 | 实现方式 | 说明 |
|------|---------|------|
| Storage | `mock.MemStorage`（内存） | 不依赖真实文件系统 |
| Origin Server | `httptest.NewServer` | 模拟源站，可控制响应内容和延迟 |
| CDN Service | `httptest.NewServer` | 启动完整服务（含 Router、Pipeline、Cache） |

### 测试隔离

每个 `Test*` 函数创建独立的 MemStorage 和 httptest server 实例，测试间无状态共享。

---

## Mock 设计

### `mock.MemStorage`

实现 `storage.Storage` 接口，数据存储在内存 map 中，提供以下额外能力：
- `CallCount(method string) int`：统计各方法调用次数（验证回源次数）
- `Clear()`：清空所有数据

详见 `mock/storage_mock.go`。

### Mini Origin Server

使用 `httptest.NewServer` 创建，支持：
- 配置固定响应内容和状态码
- 记录被调用次数（验证 singleflight 效果）
- 模拟响应延迟（测试超时行为）
- 模拟临时失败（测试重试逻辑）

---

## 测试项清单

### TC-01：Cache Miss → 回源 → 缓存写入 → 再次命中

**目的**：验证基础缓存读写流程

**步骤**：
1. 请求 `GET /file.txt`（cache miss）
2. 验证：响应 200，`X-Cache: miss`，origin server 被调用 1 次
3. 再次请求同一 URL
4. 验证：响应 200，`X-Cache: hit`，origin server 调用总次数仍为 1

---

### TC-02：并发 Miss → singleflight 只触发一次回源

**目的**：验证缓存击穿防护

**步骤**：
1. origin server 添加 100ms 延迟
2. 并发发送 50 个请求（同一 URL，均 miss）
3. 等待所有请求完成
4. 验证：origin server 只被调用 1 次（singleflight 合并）
5. 验证：所有 50 个响应均返回 200 且内容一致

---

### TC-03：TTL 过期后重新回源

**目的**：验证 TTL 淘汰机制

**步骤**：
1. 域名配置 `default_ttl = 100ms`
2. 请求 URL，触发回源并缓存
3. 等待 150ms（TTL 过期）
4. 再次请求同一 URL
5. 验证：origin server 被调用 2 次（第二次视为 miss）

---

### TC-04：精确 URL 刷新

**目的**：验证管理 API 的精确刷新

**步骤**：
1. 缓存某 URL（完成一次回源）
2. 调用 `POST /admin/flush/url` 刷新该 URL
3. 再次请求该 URL
4. 验证：origin server 被调用 2 次（刷新后重新回源）

---

### TC-05：目录前缀刷新（懒加载）

**目的**：验证 FlushRule 懒加载机制

**步骤**：
1. 缓存若干 URL（如 `/static/a.js`、`/static/b.css`）
2. 调用 `POST /admin/flush/prefix`，prefix = `/static/`
3. 请求 `/static/a.js`
4. 验证：命中 FlushRule，触发重新回源（即使缓存文件仍存在）
5. 验证：FlushRule 写入 Storage（检查 `__flush_rules__` key 存在）

---

### TC-06：FlushRule 持久化（模拟重启）

**目的**：验证 FlushRule 在服务重启后依然有效

**步骤**：
1. 缓存某 URL
2. 调用前缀刷新，写入 FlushRule（规则持久化到 MemStorage）
3. 用同一个 MemStorage 实例重新初始化服务（模拟重启，FlushRule 从 Storage 恢复）
4. 请求被刷新的 URL
5. 验证：FlushRule 规则生效，触发回源（而非返回旧缓存）

---

### TC-07：限流插件触发

**目的**：验证限流插件在超出速率时返回 429

**步骤**：
1. 配置域名限流：10 req/s，mode=domain
2. 快速连续发送 20 个请求
3. 验证：部分请求返回 429，origin server 调用次数 < 20

---

### TC-08：URL 改写后缓存命中

**目的**：验证改写后路径作为缓存 key

**步骤**：
1. 配置改写规则：`^/v1/(.*)` → `/api/$1`
2. 请求 `/v1/users`（改写为 `/api/users`，cache miss）
3. 验证：origin server 收到的路径是 `/api/users`
4. 再次请求 `/v1/users`
5. 验证：命中缓存（origin server 未被再次调用）

---

### TC-09：多域名隔离

**目的**：验证不同域名的缓存互相独立

**步骤**：
1. 配置两个域名：`a.example.com` 和 `b.example.com`，各有独立 origin
2. 通过 `a.example.com` 缓存 `/file.txt`
3. 通过 `b.example.com` 请求 `/file.txt`
4. 验证：`b.example.com` 触发回源（缓存 key 包含 domain，隔离正确）

---

### TC-10：响应头处理插件

**目的**：验证响应头的 set/del 操作

**步骤**：
1. 配置响应头规则：`set X-Cache-Node node-01`，`del Server`
2. 请求任意 URL（cache miss，触发回源，origin 返回 `Server: Apache`）
3. 验证：响应头包含 `X-Cache-Node: node-01`，不包含 `Server`

---

## 测试报告格式

运行 `go test ./integration/cases/... -v` 查看详细输出。

CI 中使用 `-count=1 -race` 参数：
- `-count=1`：禁用测试缓存，每次都实际运行
- `-race`：开启竞态检测（singleflight、限流等并发场景必须通过）
