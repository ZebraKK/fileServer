// Package admin provides the management HTTP API for cache operations.
package admin

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"fileServer/internal/cache"
	"fileServer/internal/config"
	"fileServer/internal/flush"
	"fileServer/internal/observe"
)

// Handler handles all /admin/* routes.
type Handler struct {
	sfCache    *cache.SingleflightCache
	flushStore *flush.Store
	keyBuilder *cache.KeyBuilder
	globalCfg  *config.Config
}

// New creates an admin Handler.
func New(c *cache.SingleflightCache, fs *flush.Store, kb *cache.KeyBuilder, cfg *config.Config) *Handler {
	return &Handler{sfCache: c, flushStore: fs, keyBuilder: kb, globalCfg: cfg}
}

// Register mounts admin routes onto r.
func (h *Handler) Register(r chi.Router) {
	r.Post("/admin/flush/url", h.FlushURL)
	r.Post("/admin/flush/prefix", h.FlushPrefix)
	r.Post("/admin/flush/domain", h.FlushDomain)
	r.Get("/admin/stat", h.Stat)
}

// ── /admin/flush/url ──────────────────────────────────────────────────────────

type flushURLReq struct {
	URL string `json:"url"`
}

// FlushURL handles POST /admin/flush/url.
func (h *Handler) FlushURL(w http.ResponseWriter, r *http.Request) {
	var req flushURLReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.URL == "" {
		http.Error(w, "bad request: url required", http.StatusBadRequest)
		return
	}

	parsed, err := http.NewRequest(http.MethodGet, req.URL, nil)
	if err != nil {
		http.Error(w, "bad request: invalid url", http.StatusBadRequest)
		return
	}

	domain := parsed.URL.Hostname()
	key := h.keyBuilder.Build(domain, parsed.URL.Path, parsed, h.domainKeyRules(domain))

	if err := h.sfCache.Delete(key); err != nil {
		observe.FromContext(r.Context()).Error("flush url delete", slog.String("error", err.Error()))
	}

	w.WriteHeader(http.StatusNoContent)
}

// ── /admin/flush/prefix ───────────────────────────────────────────────────────

type flushPrefixReq struct {
	Domain string `json:"domain"`
	Prefix string `json:"prefix"`
}

// FlushPrefix handles POST /admin/flush/prefix.
func (h *Handler) FlushPrefix(w http.ResponseWriter, r *http.Request) {
	var req flushPrefixReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Domain == "" || req.Prefix == "" {
		http.Error(w, "bad request: domain and prefix required", http.StatusBadRequest)
		return
	}

	if err := h.flushStore.AddRule(req.Domain, req.Prefix); err != nil {
		http.Error(w, fmt.Sprintf("failed to add flush rule: %v", err), http.StatusInternalServerError)
		return
	}

	observe.FlushRulesTotal.WithLabelValues(req.Domain).Set(float64(h.flushStore.RuleCount(req.Domain)))
	w.WriteHeader(http.StatusNoContent)
}

// ── /admin/flush/domain ───────────────────────────────────────────────────────

type flushDomainReq struct {
	Domain string `json:"domain"`
}

// FlushDomain handles POST /admin/flush/domain.
func (h *Handler) FlushDomain(w http.ResponseWriter, r *http.Request) {
	var req flushDomainReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Domain == "" {
		http.Error(w, "bad request: domain required", http.StatusBadRequest)
		return
	}

	// A prefix of "/" matches every path.
	if err := h.flushStore.AddRule(req.Domain, "/"); err != nil {
		http.Error(w, fmt.Sprintf("failed to add flush rule: %v", err), http.StatusInternalServerError)
		return
	}

	observe.FlushRulesTotal.WithLabelValues(req.Domain).Set(float64(h.flushStore.RuleCount(req.Domain)))
	w.WriteHeader(http.StatusNoContent)
}

// ── /admin/stat ───────────────────────────────────────────────────────────────

type statResponse struct {
	Hit            bool      `json:"hit"`
	WrittenAt      time.Time `json:"written_at,omitempty"`
	ExpiresAt      time.Time `json:"expires_at,omitempty"`
	TTLRemaining   int64     `json:"ttl_remaining_seconds,omitempty"`
	Size           int64     `json:"size,omitempty"`
	FlushRuleMatch bool      `json:"flush_rule_match"`
}

// Stat handles GET /admin/stat.
func (h *Handler) Stat(w http.ResponseWriter, r *http.Request) {
	rawURL := r.URL.Query().Get("url")
	if rawURL == "" {
		http.Error(w, "url query param required", http.StatusBadRequest)
		return
	}

	parsed, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		http.Error(w, "invalid url", http.StatusBadRequest)
		return
	}

	domain := parsed.URL.Hostname()
	key := h.keyBuilder.Build(domain, parsed.URL.Path, parsed, h.domainKeyRules(domain))

	resp := statResponse{}
	meta, err := h.sfCache.Stat(key)
	if err == nil {
		resp.Hit = true
		resp.WrittenAt = meta.WrittenAt
		resp.ExpiresAt = meta.ExpiresAt()
		remaining := time.Until(meta.ExpiresAt())
		if remaining > 0 {
			resp.TTLRemaining = int64(remaining.Seconds())
		}
		resp.Size = meta.Size

		if rule := h.flushStore.Match(domain, parsed.URL.Path); rule != nil {
			if rule.CreatedAt.After(meta.WrittenAt) {
				resp.FlushRuleMatch = true
				resp.Hit = false // effectively a miss due to flush rule
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// domainKeyRules returns the KeyRulesConfig for a domain, or nil for global.
func (h *Handler) domainKeyRules(domain string) *config.KeyRulesConfig {
	for i := range h.globalCfg.Domains {
		if h.globalCfg.Domains[i].Domain == domain {
			return h.globalCfg.Domains[i].KeyRules
		}
	}
	return nil
}
