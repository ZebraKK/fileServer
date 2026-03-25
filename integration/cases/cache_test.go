// Package cases contains integration tests for the CDN cache service.
// Each test creates an isolated service instance (real cache + real pipeline)
// using httptest servers for the origin, and an in-memory Storage mock.
//
// Run with:
//
//	go test ./integration/cases/... -v -count=1 -race
package cases_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fileServer/integration/mock"
	"fileServer/internal/admin"
	"fileServer/internal/cache"
	"fileServer/internal/config"
	"fileServer/internal/domain"
	"fileServer/internal/flush"
	"fileServer/internal/origin"
	"fileServer/internal/pipeline"

	// Register plugin singletons.
	_ "fileServer/internal/pipeline/header"
	_ "fileServer/internal/pipeline/ratelimit"
	_ "fileServer/internal/pipeline/rewrite"
)

// ── test harness ──────────────────────────────────────────────────────────────

type harness struct {
	bizSrv   *httptest.Server
	adminSrv *httptest.Server
	storage  *mock.MemStorage
}

type originServer struct {
	srv       *httptest.Server
	callCount atomic.Int64
	mu        sync.Mutex
	body      string
	status    int
	delay     time.Duration
}

func newOrigin(body string, status int) *originServer {
	o := &originServer{body: body, status: status}
	o.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		delay, body, status := o.delay, o.body, o.status
		o.mu.Unlock()
		o.callCount.Add(1)
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Cache-Control", "max-age=3600")
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
	return o
}

func (o *originServer) URL() string              { return o.srv.URL }
func (o *originServer) Calls() int64             { return o.callCount.Load() }
func (o *originServer) SetDelay(d time.Duration) { o.mu.Lock(); o.delay = d; o.mu.Unlock() }
func (o *originServer) Close()                   { o.srv.Close() }

// newHarness builds a complete isolated service for one test.
func newHarness(t *testing.T, domainName string, originURL string, extraPlugins ...config.PluginConfig) *harness {
	t.Helper()
	mem := mock.NewMemStorage()

	cfg := &config.Config{
		Server:  config.ServerConfig{Addr: "unused"},
		Admin:   config.AdminConfig{Addr: "unused"},
		Storage: config.StorageConfig{Type: "localfs"},
		Cache:   config.CacheConfig{MaxItems: 100, FlushRuleMaxAge: 7 * 24 * time.Hour},
		KeyRules: config.KeyRulesConfig{},
		Domains: []config.DomainConfig{{
			Domain:        domainName,
			Origins:       []string{originURL},
			OriginTimeout: 5 * time.Second,
			OriginRetry:   0,
			DefaultTTL:    time.Hour,
			Plugins:       extraPlugins,
		}},
	}

	flushStore := flush.New(mem)
	lru := cache.NewLRUCache(cfg.Cache.MaxItems, mem)
	sfCache := cache.NewSingleflightCache(lru)
	kb := cache.NewKeyBuilder(cfg.KeyRules)

	handler := domain.NewHandler(domain.Deps{
		Cache:      sfCache,
		FlushStore: flushStore,
		Puller:     origin.New(),
		KeyBuilder: kb,
		Pipeline:   pipeline.New(),
		Logger:     noopLogger(),
	})
	handler.Update(cfg.Domains)

	adminHandler := admin.New(sfCache, flushStore, kb, cfg)

	biz, adm := buildTestHandlers(cfg, handler, adminHandler)
	bizSrv := httptest.NewServer(biz)
	adminSrv := httptest.NewServer(adm)

	t.Cleanup(func() {
		bizSrv.Close()
		adminSrv.Close()
	})

	return &harness{
		bizSrv:   bizSrv,
		adminSrv: adminSrv,
		storage:  mem,
	}
}

// get issues a GET to the business server with Host set to domainName.
func (h *harness) get(t *testing.T, domainName, path string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, h.bizSrv.URL+path, nil)
	req.Host = domainName
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

func (h *harness) postAdmin(t *testing.T, path string, body any) *http.Response {
	t.Helper()
	data, _ := json.Marshal(body)
	resp, err := http.Post(h.adminSrv.URL+path, "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// ── server wiring helpers ─────────────────────────────────────────────────────

func buildTestHandlers(_ *config.Config, handler *domain.Handler, adminHandler *admin.Handler) (biz, adm http.Handler) {
	bizMux := http.NewServeMux()
	bizMux.Handle("/", handler)

	admMux := http.NewServeMux()
	admMux.Handle("/metrics", http.NotFoundHandler()) // no Prometheus in tests
	admMux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	admMux.HandleFunc("/admin/flush/url", postOnly(adminHandler.FlushURL))
	admMux.HandleFunc("/admin/flush/prefix", postOnly(adminHandler.FlushPrefix))
	admMux.HandleFunc("/admin/flush/domain", postOnly(adminHandler.FlushDomain))
	admMux.HandleFunc("/admin/stat", adminHandler.Stat)

	return bizMux, admMux
}

func postOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h(w, r)
	}
}

func noopLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ── test cases ────────────────────────────────────────────────────────────────

// TC-01: First request → cache miss, origin is called once.
func TestTC01_CacheMiss(t *testing.T) {
	o := newOrigin("hello-world", http.StatusOK)
	defer o.Close()
	h := newHarness(t, "cdn.example.com", o.URL())

	resp := h.get(t, "cdn.example.com", "/file.txt")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Cache"); got != "miss" {
		t.Fatalf("want X-Cache: miss, got %q", got)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "hello-world") {
		t.Fatalf("want body to contain %q, got %q", "hello-world", body)
	}
	if calls := o.Calls(); calls != 1 {
		t.Fatalf("want 1 origin call, got %d", calls)
	}
}

// TC-02: Second request → cache hit, origin not called again.
func TestTC02_CacheHit(t *testing.T) {
	o := newOrigin("cached-content", http.StatusOK)
	defer o.Close()
	h := newHarness(t, "cdn.example.com", o.URL())

	// First request populates the cache.
	resp1 := h.get(t, "cdn.example.com", "/file.txt")
	_ = readBody(t, resp1)

	// Second request should come from cache.
	resp2 := h.get(t, "cdn.example.com", "/file.txt")
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp2.StatusCode)
	}
	if got := resp2.Header.Get("X-Cache"); got != "hit" {
		t.Fatalf("want X-Cache: hit, got %q", got)
	}
	body := readBody(t, resp2)
	if !strings.Contains(body, "cached-content") {
		t.Fatalf("want cached body, got %q", body)
	}
	if calls := o.Calls(); calls != 1 {
		t.Fatalf("want exactly 1 origin call, got %d", calls)
	}
}

// TC-03: Request for unknown domain → 404.
func TestTC03_UnknownDomain(t *testing.T) {
	o := newOrigin("hello", http.StatusOK)
	defer o.Close()
	h := newHarness(t, "cdn.example.com", o.URL())

	resp := h.get(t, "unknown.example.com", "/file.txt")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
	if o.Calls() != 0 {
		t.Fatalf("origin must not be called for unknown domain, got %d calls", o.Calls())
	}
}

// TC-04: Flush prefix → subsequent request bypasses cache.
func TestTC04_FlushPrefix(t *testing.T) {
	o := newOrigin("v1", http.StatusOK)
	defer o.Close()
	domain := "cdn.example.com"
	h := newHarness(t, domain, o.URL())

	// Populate cache.
	resp1 := h.get(t, domain, "/file.txt")
	_ = readBody(t, resp1)
	if resp1.Header.Get("X-Cache") != "miss" {
		t.Fatal("first request should be a miss")
	}

	// Flush the entire domain prefix.
	flushResp := h.postAdmin(t, "/admin/flush/prefix", map[string]string{
		"domain": domain,
		"prefix": "/",
	})
	flushResp.Body.Close()
	if flushResp.StatusCode != http.StatusNoContent {
		t.Fatalf("flush/prefix: want 204, got %d", flushResp.StatusCode)
	}

	// Next request should trigger a fresh origin pull.
	resp2 := h.get(t, domain, "/file.txt")
	_ = readBody(t, resp2)
	if resp2.Header.Get("X-Cache") != "miss" {
		t.Fatalf("want X-Cache: miss after flush, got %q", resp2.Header.Get("X-Cache"))
	}
	if calls := o.Calls(); calls != 2 {
		t.Fatalf("want 2 origin calls after flush, got %d", calls)
	}
}

// TC-05: Concurrent cache-miss requests → singleflight collapses into one origin pull.
func TestTC05_Singleflight(t *testing.T) {
	o := newOrigin("data", http.StatusOK)
	o.SetDelay(60 * time.Millisecond) // ensure overlap
	defer o.Close()
	h := newHarness(t, "cdn.example.com", o.URL())

	const concurrency = 10
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := h.get(t, "cdn.example.com", "/shared.txt")
			if resp.StatusCode != http.StatusOK {
				t.Errorf("want 200, got %d", resp.StatusCode)
			}
			resp.Body.Close()
		}()
	}
	wg.Wait()

	// Singleflight must collapse all concurrent misses into one origin pull.
	if calls := o.Calls(); calls != 1 {
		t.Fatalf("want exactly 1 origin call (singleflight), got %d", calls)
	}
}

// TC-06: Flush URL → specific URL is evicted; other URLs remain cached.
func TestTC06_FlushURL(t *testing.T) {
	o := newOrigin("body", http.StatusOK)
	defer o.Close()
	domain := "cdn.example.com"
	h := newHarness(t, domain, o.URL())

	// Cache two paths.
	r1 := h.get(t, domain, "/a.txt")
	_ = readBody(t, r1)
	r2 := h.get(t, domain, "/b.txt")
	_ = readBody(t, r2)

	// Both should now be cached.
	ra := h.get(t, domain, "/a.txt")
	if ra.Header.Get("X-Cache") != "hit" {
		t.Fatal("/a.txt should be cached")
	}
	_ = readBody(t, ra)
	rb := h.get(t, domain, "/b.txt")
	if rb.Header.Get("X-Cache") != "hit" {
		t.Fatal("/b.txt should be cached")
	}
	_ = readBody(t, rb)

	// Flush only /a.txt via the URL endpoint.
	targetURL := "http://" + domain + "/a.txt"
	flushResp := h.postAdmin(t, "/admin/flush/url", map[string]string{"url": targetURL})
	flushResp.Body.Close()
	if flushResp.StatusCode != http.StatusNoContent {
		t.Fatalf("flush/url: want 204, got %d", flushResp.StatusCode)
	}

	// /a.txt must be a miss; /b.txt must still be a hit.
	ra2 := h.get(t, domain, "/a.txt")
	if ra2.Header.Get("X-Cache") != "miss" {
		t.Fatalf("/a.txt should be a miss after flush, got %q", ra2.Header.Get("X-Cache"))
	}
	_ = readBody(t, ra2)

	rb2 := h.get(t, domain, "/b.txt")
	if rb2.Header.Get("X-Cache") != "hit" {
		t.Fatalf("/b.txt should still be cached, got %q", rb2.Header.Get("X-Cache"))
	}
	_ = readBody(t, rb2)
}

// Ensure context is usable in test helpers that accept it.
var _ = context.Background
