// Package ratelimit provides a rate-limiting pipeline plugin.
// It supports two modes (domain-wide or per-client-IP) and two algorithms
// (token bucket via golang.org/x/time/rate, and a sliding window).
package ratelimit

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"fileServer/internal/observe"
	"fileServer/internal/pipeline"
)

func init() {
	pipeline.RegisterFactory("rate_limit", func(cfg map[string]any) (pipeline.Plugin, error) {
		return fromConfig(cfg)
	})
}

// Plugin enforces rate limits on incoming requests.
type Plugin struct {
	mode      string // "domain" | "ip"
	algorithm string // "token_bucket" | "sliding_window"
	ratePerSec float64
	burst      int

	mu       sync.Mutex
	limiters map[string]limiter
	lastSeen map[string]time.Time
}

type limiter interface {
	Allow() bool
}

// fromConfig parses the map[string]any config block produced by the YAML loader.
func fromConfig(cfg map[string]any) (*Plugin, error) {
	mode, _ := cfg["mode"].(string)
	if mode == "" {
		mode = "domain"
	}
	algo, _ := cfg["algorithm"].(string)
	if algo == "" {
		algo = "token_bucket"
	}

	rps := floatVal(cfg, "rate", 100)
	burst := intVal(cfg, "burst", int(rps))

	p := &Plugin{
		mode:       mode,
		algorithm:  algo,
		ratePerSec: rps,
		burst:      burst,
		limiters:   make(map[string]limiter),
		lastSeen:   make(map[string]time.Time),
	}

	// Start a background goroutine to evict stale per-IP limiters.
	if mode == "ip" {
		go p.cleanup()
	}
	return p, nil
}

func (p *Plugin) Name() string { return "rate_limit" }

func (p *Plugin) Handle(_ context.Context, _ *pipeline.Ctx, w http.ResponseWriter, r *http.Request) bool {
	key := p.key(r)

	p.mu.Lock()
	l, ok := p.limiters[key]
	if !ok {
		l = p.newLimiter()
		p.limiters[key] = l
	}
	p.lastSeen[key] = time.Now()
	allowed := l.Allow()
	domain := r.Host
	p.mu.Unlock()

	result := "pass"
	if !allowed {
		result = "reject"
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
	}
	observe.PluginTriggeredTotal.WithLabelValues(domain, "rate_limit", result).Inc()
	return allowed
}

func (p *Plugin) key(r *http.Request) string {
	if p.mode == "ip" {
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			ip = r.RemoteAddr
		}
		return ip
	}
	return r.Host
}

func (p *Plugin) newLimiter() limiter {
	if p.algorithm == "sliding_window" {
		return newSlidingWindow(p.ratePerSec)
	}
	return rate.NewLimiter(rate.Limit(p.ratePerSec), p.burst)
}

// cleanup removes limiters for IPs that have been idle for >5 minutes.
func (p *Plugin) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-5 * time.Minute)
		p.mu.Lock()
		for k, t := range p.lastSeen {
			if t.Before(cutoff) {
				delete(p.limiters, k)
				delete(p.lastSeen, k)
			}
		}
		p.mu.Unlock()
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

// Ensure fromConfig is reachable for the error return path (linter).
var _ = fmt.Sprintf
