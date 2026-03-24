// Package cache implements the in-memory LRU+TTL cache index and cache key
// construction. Actual file content is stored via the storage.Storage interface.
package cache

import (
	"crypto/sha1"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"fileServer/internal/config"
)

// KeyBuilder constructs normalised cache keys from request attributes.
// A domain-level KeyRulesConfig fully replaces (not merges) the global one.
type KeyBuilder struct {
	global config.KeyRulesConfig
}

// NewKeyBuilder creates a KeyBuilder with the given global rules.
func NewKeyBuilder(global config.KeyRulesConfig) *KeyBuilder {
	return &KeyBuilder{global: global}
}

// Build returns the cache key for a request.
//
//	key = "<domain>:<rewrittenPath>?<sortedQuery>#<headerHash>"
//
// If domainRules is nil, the global KeyRulesConfig is used.
func (kb *KeyBuilder) Build(domain, rewrittenPath string, req *http.Request, domainRules *config.KeyRulesConfig) string {
	rules := kb.global
	if domainRules != nil {
		rules = *domainRules
	}

	// Normalised query string: only include configured param names, sorted.
	queryPart := buildQuery(req, rules.IncludeQueryParams)

	// Header hash: stable hash of configured header values.
	headerPart := buildHeaderHash(req, rules.IncludeHeaders)

	return fmt.Sprintf("%s:%s?%s#%s", domain, rewrittenPath, queryPart, headerPart)
}

// buildQuery returns a normalised query string containing only the listed
// parameter names, sorted alphabetically to avoid key duplication.
func buildQuery(req *http.Request, params []string) string {
	if len(params) == 0 {
		return ""
	}
	q := req.URL.Query()
	sorted := make([]string, 0, len(params))
	for _, name := range params {
		if vals := q[name]; len(vals) > 0 {
			sorted = append(sorted, name+"="+vals[0])
		}
	}
	sort.Strings(sorted)
	return strings.Join(sorted, "&")
}

// buildHeaderHash returns a short hex string derived from the listed header values.
// An empty slice produces an empty string (no "#" suffix confusion).
func buildHeaderHash(req *http.Request, headers []string) string {
	if len(headers) == 0 {
		return ""
	}
	var parts []string
	for _, name := range headers {
		if v := req.Header.Get(name); v != "" {
			parts = append(parts, name+"="+v)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	sort.Strings(parts)
	h := sha1.Sum([]byte(strings.Join(parts, ";")))
	return fmt.Sprintf("%x", h[:8]) // 8 bytes = 16 hex chars is enough
}
