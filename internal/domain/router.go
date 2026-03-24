// Package domain provides per-domain request handling.
package domain

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"fileServer/internal/cache"
	"fileServer/internal/config"
	"fileServer/internal/flush"
	"fileServer/internal/observe"
	"fileServer/internal/origin"
	"fileServer/internal/pipeline"

	// Side-effect imports register plugin factories.
	_ "fileServer/internal/pipeline/header"
	_ "fileServer/internal/pipeline/ratelimit"
	_ "fileServer/internal/pipeline/rewrite"
)

// Deps groups shared infrastructure injected into every domain handler.
type Deps struct {
	Cache      *cache.SingleflightCache
	FlushStore *flush.Store
	Puller     *origin.Puller
	KeyBuilder *cache.KeyBuilder
}

// DomainHandler handles all requests for one virtual-host domain.
type DomainHandler struct {
	cfg       config.DomainConfig
	pl        *pipeline.Pipeline
	deps      Deps
	rrCounter atomic.Uint64
}

func newHandler(cfg config.DomainConfig, deps Deps) (*DomainHandler, error) {
	pl, err := pipeline.Build(cfg.Plugins)
	if err != nil {
		return nil, fmt.Errorf("domain %q: build pipeline: %w", cfg.Domain, err)
	}
	return &DomainHandler{cfg: cfg, pl: pl, deps: deps}, nil
}

func (h *DomainHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := observe.FromContext(ctx)
	start := time.Now()

	// ── 1. Plugin pipeline ────────────────────────────────────────────────────
	pCtx := pipeline.NewPipelineCtx(r.URL.Path)
	if !h.pl.Execute(ctx, pCtx, w, r) {
		return // plugin short-circuited (e.g. 429)
	}

	// ── 2. Cache key ──────────────────────────────────────────────────────────
	key := h.deps.KeyBuilder.Build(h.cfg.Domain, pCtx.RewrittenPath, r, h.cfg.KeyRules)

	// ── 3. FlushRule check ─────────────────────────────────────────────────────
	// If the cached entry is older than the flush rule, evict it so that
	// GetOrFetch will call the origin fetch function.
	if meta, err := h.deps.Cache.Stat(key); err == nil {
		if rule := h.deps.FlushStore.Match(h.cfg.Domain, pCtx.RewrittenPath); rule != nil {
			if rule.CreatedAt.After(meta.WrittenAt) {
				// Lazy flush: evict from LRU index + storage before re-fetching.
				_ = h.deps.Cache.Delete(key)
				logger.Info("flush rule applied, evicting cache entry", slog.String("key", key))
			}
		}
	}

	// ── 4. Serve (cache hit or origin pull via singleflight) ──────────────────
	cacheHit := false

	// Peek at meta before GetOrFetch to detect cache hit after possible eviction.
	_, peekErr := h.deps.Cache.Stat(key)
	cacheHit = (peekErr == nil)

	rc, meta, err := h.deps.Cache.GetOrFetch(key, h.originFetch(ctx, r, key, logger))
	if err != nil {
		logger.Error("serve failed", slog.String("error", err.Error()), slog.String("key", key))
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}
	defer rc.Close()

	// ── 5. Write response ─────────────────────────────────────────────────────
	pipeline.ApplyResponseMutations(w, pCtx)
	if meta != nil && meta.Headers != nil {
		for k, vs := range meta.Headers {
			for _, v := range vs {
				w.Header().Set(k, v)
			}
		}
	}
	if cacheHit {
		w.Header().Set("X-Cache", "hit")
		observe.CacheHitsTotal.WithLabelValues(h.cfg.Domain).Inc()
	} else {
		w.Header().Set("X-Cache", "miss")
		observe.CacheMissesTotal.WithLabelValues(h.cfg.Domain).Inc()
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)

	latency := time.Since(start)
	observe.RequestDuration.WithLabelValues(h.cfg.Domain, fmt.Sprintf("%v", cacheHit)).Observe(latency.Seconds())

	logger.Info("served",
		slog.String("key", key),
		slog.Bool("cache_hit", cacheHit),
		slog.Int64("latency_ms", latency.Milliseconds()),
	)
}

// originFetch returns the fetch closure passed to SingleflightCache.GetOrFetch.
func (h *DomainHandler) originFetch(ctx context.Context, r *http.Request, key string, logger *slog.Logger) func() (io.Reader, *cache.Meta, error) {
	return func() (io.Reader, *cache.Meta, error) {
		// Use an independent context for the origin pull: the fetch may be shared
		// across multiple request goroutines via singleflight, and we must not cancel
		// the pull when any one client closes its connection.
		// Timeout is enforced by OriginTimeout via the puller, so Background is safe.
		fetchCtx, cancel := context.WithTimeout(context.Background(), h.cfg.OriginTimeout*time.Duration(h.cfg.OriginRetry+2))
		defer cancel()

		// Clone the request using the independent context.
		outReq := r.Clone(fetchCtx)
		outReq.URL.Path = r.URL.Path

		pullStart := time.Now()
		originURL := ""
		if len(h.cfg.Origins) > 0 {
			idx := int(h.rrCounter.Load()) % len(h.cfg.Origins)
			originURL = h.cfg.Origins[idx]
		}

		resp, err := h.deps.Puller.Pull(ctx, h.cfg.Origins, &h.rrCounter, outReq, h.cfg.OriginTimeout, h.cfg.OriginRetry)
		if err != nil {
			return nil, nil, fmt.Errorf("origin pull: %w", err)
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, nil, fmt.Errorf("read origin body: %w", err)
		}

		ttl := cache.ParseTTL(resp, h.cfg.DefaultTTL)
		meta := &cache.Meta{
			Key:       key,
			WrittenAt: time.Now(),
			TTL:       ttl,
			Size:      int64(len(body)),
			Headers:   resp.Header.Clone(),
		}

		observe.OriginPullsTotal.WithLabelValues(
			h.cfg.Domain, originURL, fmt.Sprintf("%d", resp.StatusCode),
		).Inc()
		observe.OriginPullDuration.WithLabelValues(h.cfg.Domain).Observe(
			time.Since(pullStart).Seconds(),
		)
		logger.Info("origin pull", slog.String("origin", originURL), slog.String("key", key))

		return bytes.NewReader(body), meta, nil
	}
}

// ── DomainRouter ──────────────────────────────────────────────────────────────

// DomainRouter dispatches HTTP requests to per-domain handlers.
// The handler map is replaced atomically on hot-reload.
type DomainRouter struct {
	handlers atomic.Pointer[map[string]*DomainHandler]
}

// New creates an empty DomainRouter. Call Update before serving requests.
func New() *DomainRouter {
	r := &DomainRouter{}
	empty := make(map[string]*DomainHandler)
	r.handlers.Store(&empty)
	return r
}

// Update atomically replaces all domain handlers.
func (r *DomainRouter) Update(cfgs []config.DomainConfig, deps Deps) error {
	m := make(map[string]*DomainHandler, len(cfgs))
	for _, cfg := range cfgs {
		h, err := newHandler(cfg, deps)
		if err != nil {
			return err
		}
		m[cfg.Domain] = h
	}
	r.handlers.Store(&m)
	return nil
}

// ServeHTTP implements http.Handler.
func (r *DomainRouter) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	host := stripPort(req.Host)
	m := *r.handlers.Load()
	h, ok := m[host]
	if !ok {
		http.NotFound(w, req)
		return
	}
	h.ServeHTTP(w, req)
}

func stripPort(host string) string {
	for i := len(host) - 1; i >= 0; i-- {
		if host[i] == ':' {
			return host[:i]
		}
	}
	return host
}
