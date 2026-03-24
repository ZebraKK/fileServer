// Package config defines all configuration structures for the CDN cache service.
// Configurations are loaded from a YAML file and support hot-reload (Phase 4).
package config

import "time"

// Config is the top-level configuration structure.
type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Admin    AdminConfig    `yaml:"admin"`
	Storage  StorageConfig  `yaml:"storage"`
	Cache    CacheConfig    `yaml:"cache"`
	KeyRules KeyRulesConfig `yaml:"key_rules"`
	Domains  []DomainConfig `yaml:"domains"`
}

// ServerConfig holds the business HTTP server settings.
type ServerConfig struct {
	Addr string `yaml:"addr"` // default ":8080"
}

// AdminConfig holds the admin/metrics HTTP server settings.
type AdminConfig struct {
	Addr string `yaml:"addr"` // default ":9090"
}

// StorageConfig describes the storage backend to use.
type StorageConfig struct {
	// Type selects the backend: "localfs" (default) or "custom".
	Type string `yaml:"type"`
	// RootDir is the filesystem root for the "localfs" backend.
	RootDir string `yaml:"root_dir"`
}

// CacheConfig controls in-memory LRU cache behaviour.
type CacheConfig struct {
	// MaxItems is the maximum number of entries held in the LRU index.
	// When exceeded the least-recently-used entry is evicted. Default: 10000.
	MaxItems int `yaml:"max_items"`
	// FlushRuleMaxAge is how long flush rules are retained. Default: 168h (7d).
	FlushRuleMaxAge time.Duration `yaml:"flush_rule_max_age"`
}

// KeyRulesConfig controls which request attributes are folded into the cache key.
// A domain-level KeyRulesConfig completely replaces (not merges) the global one.
type KeyRulesConfig struct {
	// IncludeQueryParams lists URL parameter names to include in the key.
	// Parameters are sorted before hashing to avoid key duplication.
	IncludeQueryParams []string `yaml:"include_query_params"`
	// IncludeHeaders lists request header names to include in the key.
	IncludeHeaders []string `yaml:"include_headers"`
}

// DomainConfig holds per-domain settings.
type DomainConfig struct {
	// Domain is the virtual host name (without port).
	Domain string `yaml:"domain"`

	// Origins is the list of upstream origin URLs for this domain.
	// Requests are distributed round-robin; on failure the next origin is tried.
	Origins []string `yaml:"origins"`

	// OriginTimeout is the per-request timeout when fetching from an origin.
	OriginTimeout time.Duration `yaml:"origin_timeout"`

	// OriginRetry is the maximum number of retry attempts (switching origins).
	OriginRetry int `yaml:"origin_retry"`

	// DefaultTTL is the cache duration used when the origin response carries
	// no Cache-Control or Expires header.
	DefaultTTL time.Duration `yaml:"default_ttl"`

	// KeyRules overrides the global KeyRules for this domain.
	// If nil the global KeyRules apply.
	KeyRules *KeyRulesConfig `yaml:"key_rules,omitempty"`

	// Plugins lists the plugin chain for this domain in execution order.
	Plugins []PluginConfig `yaml:"plugins"`
}

// PluginConfig identifies a plugin and carries its opaque configuration map.
type PluginConfig struct {
	// Type is the plugin name: "rate_limit", "url_rewrite", or "header".
	Type string `yaml:"type"`
	// Config is the plugin-specific configuration, decoded by the plugin itself.
	Config map[string]any `yaml:"config"`
}

// Defaults fills in zero-value fields with sensible defaults.
func (c *Config) Defaults() {
	if c.Server.Addr == "" {
		c.Server.Addr = ":8080"
	}
	if c.Admin.Addr == "" {
		c.Admin.Addr = ":9090"
	}
	if c.Storage.Type == "" {
		c.Storage.Type = "localfs"
	}
	if c.Storage.RootDir == "" {
		c.Storage.RootDir = "./cache_data"
	}
	if c.Cache.MaxItems <= 0 {
		c.Cache.MaxItems = 10_000
	}
	if c.Cache.FlushRuleMaxAge <= 0 {
		c.Cache.FlushRuleMaxAge = 7 * 24 * time.Hour
	}
	for i := range c.Domains {
		d := &c.Domains[i]
		if d.OriginTimeout <= 0 {
			d.OriginTimeout = 10 * time.Second
		}
		if d.OriginRetry < 0 {
			d.OriginRetry = 0
		}
		if d.DefaultTTL <= 0 {
			d.DefaultTTL = time.Hour
		}
	}
}
