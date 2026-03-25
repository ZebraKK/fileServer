// Package domain provides unified request handling across all virtual-host domains.
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

	// Side-effect imports register plugin singletons.
	_ "fileServer/internal/pipeline/header"
	_ "fileServer/internal/pipeline/ratelimit"
	_ "fileServer/internal/pipeline/rewrite"
)

// Deps groups shared infrastructure injected into Handler.
type Deps struct {
	Cache      *cache.SingleflightCache
	FlushStore *flush.Store
	Puller     *origin.Puller
	KeyBuilder *cache.KeyBuilder
	Pipeline   *pipeline.Pipeline
	Logger     *slog.Logger
}

// Handler handles all requests using a single unified pipeline.
// Domain configuration is looked up per-request from an atomically-replaced map.
type Handler struct {
	domains    atomic.Pointer[map[string]config.DomainConfig]
	cache      *cache.SingleflightCache
	keyBuilder *cache.KeyBuilder
	flushStore *flush.Store
	puller     *origin.Puller
	pipeline   *pipeline.Pipeline
	logger     *slog.Logger
}

// NewHandler creates a Handler from the provided dependencies.
// Call Update before serving requests.
func NewHandler(deps Deps) *Handler {
	h := &Handler{
		cache:      deps.Cache,
		keyBuilder: deps.KeyBuilder,
		flushStore: deps.FlushStore,
		puller:     deps.Puller,
		pipeline:   deps.Pipeline,
		logger:     deps.Logger,
	}
	empty := make(map[string]config.DomainConfig)
	h.domains.Store(&empty)
	return h
}

// Update atomically replaces the domain configuration map.
// Safe to call concurrently with ServeHTTP (hot-reload).
func (h *Handler) Update(domains []config.DomainConfig) {
	m := make(map[string]config.DomainConfig, len(domains))
	for _, d := range domains {
		m[d.Domain] = d
	}
	h.domains.Store(&m)
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// ── 0. Domain lookup ──────────────────────────────────────────────────────
	host := stripPort(r.Host)
	m := *h.domains.Load()
	cfg, ok := m[host]
	if !ok {
		http.NotFound(w, r)
		return
	}

	logger := observe.FromContext(r.Context())
	start := time.Now()

	// ── 1. Plugin pipeline ────────────────────────────────────────────────────
	pCtx, r, ok := h.pipeline.Execute(cfg.Plugins, cfg.Domain, w, r)
	if !ok {
		return // plugin short-circuited (e.g. 429); response already written
	}

	// ── 2. Cache key ──────────────────────────────────────────────────────────
	key := h.keyBuilder.Build(cfg.Domain, pCtx.RewrittenPath, r, cfg.KeyRules)

	// ── 3. FlushRule check ────────────────────────────────────────────────────
	if meta, err := h.cache.Stat(key); err == nil {
		if rule := h.flushStore.Match(cfg.Domain, pCtx.RewrittenPath); rule != nil {
			if rule.CreatedAt.After(meta.WrittenAt) {
				_ = h.cache.Delete(key)
				logger.Info("flush rule applied, evicting cache entry", slog.String("key", key))
			}
		}
	}

	// ── 4. Serve (cache hit or origin pull via singleflight) ──────────────────
	_, peekErr := h.cache.Stat(key)
	cacheHit := (peekErr == nil)

	rc, meta, err := h.cache.GetOrFetch(key, h.originFetch(cfg, pCtx, r, key))
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
		observe.CacheHitsTotal.WithLabelValues(cfg.Domain).Inc()
	} else {
		w.Header().Set("X-Cache", "miss")
		observe.CacheMissesTotal.WithLabelValues(cfg.Domain).Inc()
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)

	latency := time.Since(start)
	observe.RequestDuration.WithLabelValues(cfg.Domain, fmt.Sprintf("%v", cacheHit)).Observe(latency.Seconds())
	logger.Info("served",
		slog.String("key", key),
		slog.Bool("cache_hit", cacheHit),
		slog.Int64("latency_ms", latency.Milliseconds()),
	)
}

// originFetch returns the fetch closure for SingleflightCache.GetOrFetch.
func (h *Handler) originFetch(
	cfg config.DomainConfig,
	pCtx *pipeline.PipelineContext,
	r *http.Request,
	key string,
) func() (io.Reader, *cache.Meta, error) {
	// Capture path + query now so the closure doesn't hold a *http.Request reference.
	path := pCtx.RewrittenPath
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	header := r.Header.Clone()

	return func() (io.Reader, *cache.Meta, error) {
		// Use an independent context so that a singleflight-shared pull is not
		// cancelled when any one client closes its connection.
		fetchCtx, cancel := context.WithTimeout(
			context.Background(),
			cfg.OriginTimeout*time.Duration(cfg.OriginRetry+2),
		)
		defer cancel()

		pullStart := time.Now()
		resp, err := h.puller.Pull(fetchCtx, cfg.Origins, cfg.OriginTimeout, cfg.OriginRetry, path, header)
		if err != nil {
			return nil, nil, fmt.Errorf("origin pull: %w", err)
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, nil, fmt.Errorf("read origin body: %w", err)
		}

		ttl := cache.ParseTTL(resp, cfg.DefaultTTL)
		meta := &cache.Meta{
			Key:       key,
			WrittenAt: time.Now(),
			TTL:       ttl,
			Size:      int64(len(body)),
			Headers:   resp.Header.Clone(),
		}

		observe.OriginPullsTotal.WithLabelValues(
			cfg.Domain, "", fmt.Sprintf("%d", resp.StatusCode),
		).Inc()
		observe.OriginPullDuration.WithLabelValues(cfg.Domain).Observe(
			time.Since(pullStart).Seconds(),
		)
		h.logger.Info("origin pull", slog.String("key", key))

		return bytes.NewReader(body), meta, nil
	}
}

func stripPort(host string) string {
	for i := len(host) - 1; i >= 0; i-- {
		if host[i] == ':' {
			return host[:i]
		}
	}
	return host
}
