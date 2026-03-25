// Package ratelimit provides a rate-limiting pipeline plugin.
// It supports two modes (domain-wide or per-client-IP) and two algorithms
// (token bucket via golang.org/x/time/rate, and a sliding window).
//
// The plugin is a singleton: one instance is registered globally and handles
// all domains. Per-domain (and per-IP) state is maintained internally via a
// sync.Map keyed by domain name.
package ratelimit

import (
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"fileServer/internal/observe"
	"fileServer/internal/pipeline"
)

func init() {
	pipeline.RegisterPlugin("rate_limit", &Plugin{})
}

// Plugin is the singleton rate-limit plugin.
// It manages per-domain limiter state internally.
type Plugin struct {
	domains sync.Map // domainKey → *perDomainState
}

// perDomainState holds the limiter map and config for one domain.
type perDomainState struct {
	mode       string
	algorithm  string
	ratePerSec float64
	burst      int

	mu          sync.Mutex
	limiters    map[string]limiter
	lastSeen    map[string]time.Time
	cleanupOnce sync.Once
}

type limiter interface {
	Allow() bool
}

func (p *Plugin) Execute(
	pCtx *pipeline.PipelineContext,
	cfg map[string]any,
	domain string,
	w http.ResponseWriter,
	r *http.Request,
) bool {
	mode := stringVal(cfg, "mode", "domain")
	algo := stringVal(cfg, "algorithm", "token_bucket")
	rps := floatVal(cfg, "rate", 100)
	burst := intVal(cfg, "burst", int(rps))

	// Load or create per-domain state. If config changed between reloads the old
	// limiters remain (acceptable; full reset requires process restart).
	stateI, _ := p.domains.LoadOrStore(domain, &perDomainState{
		mode:       mode,
		algorithm:  algo,
		ratePerSec: rps,
		burst:      burst,
		limiters:   make(map[string]limiter),
		lastSeen:   make(map[string]time.Time),
	})
	state := stateI.(*perDomainState)

	// Start GC goroutine once per domain for IP mode.
	if mode == "ip" {
		state.cleanupOnce.Do(func() { go state.cleanup() })
	}

	key := state.limiterKey(r)

	state.mu.Lock()
	l, ok := state.limiters[key]
	if !ok {
		l = state.newLimiter()
		state.limiters[key] = l
	}
	state.lastSeen[key] = time.Now()
	allowed := l.Allow()
	state.mu.Unlock()

	result := "pass"
	if !allowed {
		result = "reject"
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
	}
	observe.PluginTriggeredTotal.WithLabelValues(domain, "rate_limit", result).Inc()
	return allowed
}

func (s *perDomainState) limiterKey(r *http.Request) string {
	if s.mode == "ip" {
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			ip = r.RemoteAddr
		}
		return ip
	}
	return "domain"
}

func (s *perDomainState) newLimiter() limiter {
	if s.algorithm == "sliding_window" {
		return newSlidingWindow(s.ratePerSec)
	}
	return rate.NewLimiter(rate.Limit(s.ratePerSec), s.burst)
}

// cleanup removes limiters for IPs idle for >5 minutes.
func (s *perDomainState) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-5 * time.Minute)
		s.mu.Lock()
		for k, t := range s.lastSeen {
			if t.Before(cutoff) {
				delete(s.limiters, k)
				delete(s.lastSeen, k)
			}
		}
		s.mu.Unlock()
	}
}

// ── Sliding window implementation ─────────────────────────────────────────────

type slidingWindow struct {
	mu         sync.Mutex
	ratePerSec float64
	window     time.Duration
	timestamps []time.Time
}

func newSlidingWindow(rps float64) *slidingWindow {
	return &slidingWindow{
		ratePerSec: rps,
		window:     time.Second,
	}
}

func (sw *slidingWindow) Allow() bool {
	sw.mu.Lock()
	defer sw.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-sw.window)

	// Drop timestamps outside the window.
	var kept []time.Time
	for _, t := range sw.timestamps {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	sw.timestamps = kept

	if float64(len(sw.timestamps)) >= sw.ratePerSec {
		return false
	}
	sw.timestamps = append(sw.timestamps, now)
	return true
}

// ── config helpers ────────────────────────────────────────────────────────────

func stringVal(cfg map[string]any, key, def string) string {
	if v, ok := cfg[key].(string); ok && v != "" {
		return v
	}
	return def
}

func floatVal(cfg map[string]any, key string, def float64) float64 {
	v, ok := cfg[key]
	if !ok {
		return def
	}
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return def
}

func intVal(cfg map[string]any, key string, def int) int {
	v, ok := cfg[key]
	if !ok {
		return def
	}
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	case int64:
		return int(n)
	}
	return def
}

// Ensure Plugin satisfies the interface at compile time.
var _ pipeline.Plugin = (*Plugin)(nil)
