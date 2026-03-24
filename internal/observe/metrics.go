package observe

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// All metrics are registered once at package init via promauto.
var (
	RequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cdncache_requests_total",
		Help: "Total number of HTTP requests handled, by domain and status code.",
	}, []string{"domain", "status"})

	CacheHitsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cdncache_cache_hits_total",
		Help: "Total cache hits by domain.",
	}, []string{"domain"})

	CacheMissesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cdncache_cache_misses_total",
		Help: "Total cache misses by domain.",
	}, []string{"domain"})

	OriginPullsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cdncache_origin_pulls_total",
		Help: "Total origin pull requests, by domain, origin URL and HTTP status.",
	}, []string{"domain", "origin", "status"})

	OriginPullDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "cdncache_origin_pull_duration_seconds",
		Help:    "Origin pull latency distribution.",
		Buckets: prometheus.DefBuckets,
	}, []string{"domain"})

	RequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "cdncache_request_duration_seconds",
		Help:    "End-to-end request latency distribution.",
		Buckets: prometheus.DefBuckets,
	}, []string{"domain", "cache_hit"})

	CacheStorageBytes = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cdncache_cache_storage_bytes",
		Help: "Estimated bytes used by cached files on disk, by domain.",
	}, []string{"domain"})

	PluginTriggeredTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cdncache_plugin_triggered_total",
		Help: "Number of times a plugin was triggered, by domain, plugin name and result.",
	}, []string{"domain", "plugin", "result"})

	FlushRulesTotal = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cdncache_flush_rules_total",
		Help: "Current number of active flush rules, by domain.",
	}, []string{"domain"})
)
