package pipeline_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"fileServer/internal/config"
	"fileServer/internal/pipeline"

	// Import plugins so their init() registers singletons.
	_ "fileServer/internal/pipeline/header"
	_ "fileServer/internal/pipeline/ratelimit"
	_ "fileServer/internal/pipeline/rewrite"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func run(p *pipeline.Pipeline, cfgs []config.PluginConfig, domain string, req *http.Request) (int, *pipeline.PipelineContext) {
	w := httptest.NewRecorder()
	pCtx, _, _ := p.Execute(cfgs, domain, w, req)
	return w.Code, pCtx
}

// ── url_rewrite ───────────────────────────────────────────────────────────────

func TestRewriteMatchAndReplace(t *testing.T) {
	p := pipeline.New()
	cfgs := []config.PluginConfig{{
		Type: "url_rewrite",
		Config: map[string]any{
			"rules": []any{
				map[string]any{"match": `^/v1/(.*)`, "replace": "/api/$1"},
			},
		},
	}}

	req := httptest.NewRequest(http.MethodGet, "/v1/users", nil)
	_, pCtx := run(p, cfgs, "cdn.example.com", req)

	if pCtx.RewrittenPath != "/api/users" {
		t.Errorf("expected /api/users, got %s", pCtx.RewrittenPath)
	}
}

func TestRewriteNoMatch(t *testing.T) {
	p := pipeline.New()
	cfgs := []config.PluginConfig{{
		Type: "url_rewrite",
		Config: map[string]any{
			"rules": []any{
				map[string]any{"match": `^/v1/(.*)`, "replace": "/api/$1"},
			},
		},
	}}

	req := httptest.NewRequest(http.MethodGet, "/other/path", nil)
	_, pCtx := run(p, cfgs, "cdn.example.com", req)

	if pCtx.RewrittenPath != "/other/path" {
		t.Errorf("expected path unchanged, got %s", pCtx.RewrittenPath)
	}
}

// ── header ────────────────────────────────────────────────────────────────────

func TestHeaderSetRequest(t *testing.T) {
	p := pipeline.New()
	cfgs := []config.PluginConfig{{
		Type: "header",
		Config: map[string]any{
			"request": []any{
				map[string]any{"op": "set", "key": "X-Custom", "value": "hello"},
			},
		},
	}}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	_, _ = run(p, cfgs, "cdn.example.com", req)

	if req.Header.Get("X-Custom") != "hello" {
		t.Errorf("expected X-Custom: hello, got %q", req.Header.Get("X-Custom"))
	}
}

func TestHeaderDelRequest(t *testing.T) {
	p := pipeline.New()
	cfgs := []config.PluginConfig{{
		Type: "header",
		Config: map[string]any{
			"request": []any{
				map[string]any{"op": "del", "key": "X-Secret"},
			},
		},
	}}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Secret", "sensitive")
	_, _ = run(p, cfgs, "cdn.example.com", req)

	if req.Header.Get("X-Secret") != "" {
		t.Errorf("expected X-Secret to be deleted")
	}
}

func TestHeaderStageResponseSet(t *testing.T) {
	p := pipeline.New()
	cfgs := []config.PluginConfig{{
		Type: "header",
		Config: map[string]any{
			"response": []any{
				map[string]any{"op": "set", "key": "X-Cache-Node", "value": "node-01"},
				map[string]any{"op": "del", "key": "Server"},
			},
		},
	}}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	_, pCtx := run(p, cfgs, "cdn.example.com", req)

	if pCtx.SetResponseHeaders.Get("X-Cache-Node") != "node-01" {
		t.Errorf("expected X-Cache-Node staged, got %q", pCtx.SetResponseHeaders.Get("X-Cache-Node"))
	}
	if len(pCtx.DeleteResponseHeaders) == 0 || pCtx.DeleteResponseHeaders[0] != "Server" {
		t.Errorf("expected Server in DeleteResponseHeaders, got %v", pCtx.DeleteResponseHeaders)
	}
}

func TestApplyResponseMutations(t *testing.T) {
	pCtx := &pipeline.PipelineContext{
		RewrittenPath:      "/",
		SetResponseHeaders: make(http.Header),
	}
	pCtx.SetResponseHeaders.Set("X-Node", "n1")
	pCtx.DeleteResponseHeaders = append(pCtx.DeleteResponseHeaders, "Server")

	w := httptest.NewRecorder()
	w.Header().Set("Server", "Apache")

	pipeline.ApplyResponseMutations(w, pCtx)

	if w.Header().Get("X-Node") != "n1" {
		t.Errorf("expected X-Node: n1")
	}
	if w.Header().Get("Server") != "" {
		t.Errorf("expected Server to be deleted")
	}
}

// ── rate_limit ────────────────────────────────────────────────────────────────

func TestRateLimitAllows(t *testing.T) {
	p := pipeline.New()
	cfgs := []config.PluginConfig{{
		Type: "rate_limit",
		Config: map[string]any{
			"mode": "domain", "algorithm": "token_bucket",
			"rate": float64(100), "burst": 100,
		},
	}}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "cdn.example.com"
	w := httptest.NewRecorder()

	_, _, ok := p.Execute(cfgs, "cdn.example.com", w, req)
	if !ok {
		t.Errorf("expected first request to be allowed, got %d", w.Code)
	}
}

func TestRateLimitBlocks(t *testing.T) {
	// Rate=1/s, burst=1 → second immediate request should be blocked.
	p := pipeline.New()
	cfgs := []config.PluginConfig{{
		Type: "rate_limit",
		Config: map[string]any{
			"mode": "domain", "algorithm": "token_bucket",
			"rate": float64(1), "burst": 1,
		},
	}}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "test-block.example.com"

	// First request: allowed.
	w1 := httptest.NewRecorder()
	p.Execute(cfgs, "test-block.example.com", w1, req)

	// Second request immediately: should be blocked.
	w2 := httptest.NewRecorder()
	_, _, ok := p.Execute(cfgs, "test-block.example.com", w2, req)
	if ok {
		t.Error("expected second request to be rate-limited")
	}
	if w2.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429, got %d", w2.Code)
	}
}

func TestSlidingWindowRateLimit(t *testing.T) {
	p := pipeline.New()
	cfgs := []config.PluginConfig{{
		Type: "rate_limit",
		Config: map[string]any{
			"mode": "domain", "algorithm": "sliding_window",
			"rate": float64(2), "burst": 2,
		},
	}}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "slide.example.com"

	blocked := 0
	for range 5 {
		w := httptest.NewRecorder()
		_, _, ok := p.Execute(cfgs, "slide.example.com", w, req)
		if !ok {
			blocked++
		}
	}
	if blocked == 0 {
		t.Error("expected at least one request to be rate-limited")
	}
}

// ── unknown plugin ────────────────────────────────────────────────────────────

// In the new design, unknown plugin types are silently skipped so the request
// continues through the pipeline. This tests that behaviour.
func TestUnknownPluginIsSkipped(t *testing.T) {
	p := pipeline.New()
	cfgs := []config.PluginConfig{{Type: "nonexistent"}}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	_, _, ok := p.Execute(cfgs, "cdn.example.com", w, req)
	if !ok {
		t.Error("expected unknown plugin to be skipped (pipeline should continue)")
	}
}
