// Package flush manages cache invalidation rules.
//
// Directory-level cache flushes use a lazy approach: instead of scanning and
// deleting every matching file immediately (which can be expensive for large
// caches), a FlushRule is written and persisted. On each subsequent request
// the cache layer checks whether the matched entry was written *before* the
// rule was created; if so, the entry is treated as a miss and re-fetched from
// origin.
package flush

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"fileServer/internal/storage"
)

const persistKey = "__flush_rules__"

// Rule describes a single cache invalidation rule.
type Rule struct {
	ID        string    `json:"id"`
	Domain    string    `json:"domain"`
	Prefix    string    `json:"prefix"` // "/" means the entire domain
	CreatedAt time.Time `json:"created_at"`
}

// Store holds all active flush rules in memory and mirrors them to storage.
type Store struct {
	mu      sync.RWMutex
	rules   map[string][]Rule // domain → rules (newest last)
	storage storage.Storage
}

// New creates an empty Store backed by the given storage.
func New(s storage.Storage) *Store {
	return &Store{
		rules:   make(map[string][]Rule),
		storage: s,
	}
}

// Load restores rules from the persistent storage key.
// Must be called once at service startup before serving requests.
func (st *Store) Load() error {
	rc, _, err := st.storage.Read(persistKey)
	if err != nil {
		// No rules stored yet — not an error.
		return nil
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return fmt.Errorf("flush: read rules: %w", err)
	}

	var all []Rule
	if err := json.Unmarshal(data, &all); err != nil {
		return fmt.Errorf("flush: decode rules: %w", err)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	for _, r := range all {
		st.rules[r.Domain] = append(st.rules[r.Domain], r)
	}
	return nil
}

// AddRule records a new flush rule for domain+prefix and persists all rules.
func (st *Store) AddRule(domain, prefix string) error {
	r := Rule{
		ID:        fmt.Sprintf("%d", time.Now().UnixNano()),
		Domain:    domain,
		Prefix:    prefix,
		CreatedAt: time.Now(),
	}

	st.mu.Lock()
	st.rules[domain] = append(st.rules[domain], r)
	all := st.allRulesLocked()
	st.mu.Unlock()

	return st.persist(all)
}

// Match returns the most-recently-created rule that covers domain+path,
// or nil if no rule matches.
func (st *Store) Match(domain, path string) *Rule {
	st.mu.RLock()
	defer st.mu.RUnlock()

	rules := st.rules[domain]
	var latest *Rule
	for i := range rules {
		r := &rules[i]
		if strings.HasPrefix(path, r.Prefix) {
			if latest == nil || r.CreatedAt.After(latest.CreatedAt) {
				latest = r
			}
		}
	}
	return latest
}

// Cleanup removes rules older than maxAge and persists the remaining set.
func (st *Store) Cleanup(maxAge time.Duration) error {
	cutoff := time.Now().Add(-maxAge)

	st.mu.Lock()
	for domain, rules := range st.rules {
		var kept []Rule
		for _, r := range rules {
			if r.CreatedAt.After(cutoff) {
				kept = append(kept, r)
			}
		}
		if len(kept) == 0 {
			delete(st.rules, domain)
		} else {
			st.rules[domain] = kept
		}
	}
	all := st.allRulesLocked()
	st.mu.Unlock()

	return st.persist(all)
}

// RuleCount returns the total number of active rules across all domains.
func (st *Store) RuleCount(domain string) int {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return len(st.rules[domain])
}

// ── internal helpers ──────────────────────────────────────────────────────────

func (st *Store) allRulesLocked() []Rule {
	var all []Rule
	for _, rs := range st.rules {
		all = append(all, rs...)
	}
	return all
}

func (st *Store) persist(rules []Rule) error {
	data, err := json.Marshal(rules)
	if err != nil {
		return fmt.Errorf("flush: encode rules: %w", err)
	}
	if err := st.storage.Write(persistKey, bytes.NewReader(data), &storage.Metadata{
		ContentType: "application/json",
	}); err != nil {
		return fmt.Errorf("flush: persist rules: %w", err)
	}
	return nil
}
