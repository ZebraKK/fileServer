package cache

import (
	"container/list"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"fileServer/internal/storage"
)

// Meta holds cache metadata stored in memory alongside the LRU entry.
type Meta struct {
	Key       string
	WrittenAt time.Time
	TTL       time.Duration
	Size      int64
	Headers   http.Header
}

// ExpiresAt returns the absolute expiry time for this entry.
func (m *Meta) ExpiresAt() time.Time { return m.WrittenAt.Add(m.TTL) }

// Cache is the interface the domain handler uses; implemented by LRUCache.
type Cache interface {
	// Get returns the stored content and metadata, or an error if not found/expired.
	Get(key string) (io.ReadCloser, *Meta, error)
	// Set writes content to storage and records it in the LRU index.
	Set(key string, r io.Reader, meta *Meta) error
	// Delete removes an entry from both the LRU index and storage.
	Delete(key string) error
	// Stat returns metadata for key without reading its content.
	Stat(key string) (*Meta, error)
}

// LRUCache is a thread-safe cache backed by storage.Storage with an in-memory
// LRU index for TTL tracking and eviction.
type LRUCache struct {
	mu       sync.Mutex
	items    map[string]*list.Element // key → list element
	lru      *list.List               // front = most recently used
	maxItems int
	storage  storage.Storage
}

// NewLRUCache creates a LRUCache with the given capacity and storage backend.
func NewLRUCache(maxItems int, s storage.Storage) *LRUCache {
	return &LRUCache{
		items:    make(map[string]*list.Element),
		lru:      list.New(),
		maxItems: maxItems,
		storage:  s,
	}
}

func (c *LRUCache) Get(key string) (io.ReadCloser, *Meta, error) {
	c.mu.Lock()
	el, ok := c.items[key]
	if !ok {
		c.mu.Unlock()
		return nil, nil, fmt.Errorf("cache: miss: %s", key)
	}

	meta := el.Value.(*Meta)

	// TTL check.
	if time.Now().After(meta.ExpiresAt()) {
		c.removeLocked(el)
		c.mu.Unlock()
		return nil, nil, fmt.Errorf("cache: expired: %s", key)
	}

	// Move to front (most recently used).
	c.lru.MoveToFront(el)
	c.mu.Unlock()

	rc, _, err := c.storage.Read(key)
	if err != nil {
		// Storage inconsistency: evict from index.
		c.mu.Lock()
		if el2, ok2 := c.items[key]; ok2 {
			c.removeLocked(el2)
		}
		c.mu.Unlock()
		return nil, nil, fmt.Errorf("cache: storage read: %w", err)
	}
	return rc, meta, nil
}

func (c *LRUCache) Set(key string, r io.Reader, meta *Meta) error {
	if err := c.storage.Write(key, r, metaToStorageMeta(meta)); err != nil {
		return fmt.Errorf("cache: storage write: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Update existing entry or insert new one.
	if el, ok := c.items[key]; ok {
		el.Value = meta
		c.lru.MoveToFront(el)
	} else {
		el := c.lru.PushFront(meta)
		c.items[key] = el
		c.evictLocked()
	}
	return nil
}

func (c *LRUCache) Delete(key string) error {
	c.mu.Lock()
	if el, ok := c.items[key]; ok {
		c.removeLocked(el)
	}
	c.mu.Unlock()
	return c.storage.Delete(key)
}

func (c *LRUCache) Stat(key string) (*Meta, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, fmt.Errorf("cache: not found: %s", key)
	}
	m := el.Value.(*Meta)
	cp := *m
	return &cp, nil
}

// Len returns the current number of entries in the LRU index.
func (c *LRUCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len()
}

// ── internal helpers ──────────────────────────────────────────────────────────

// evictLocked removes LRU tail entries until the cache is within capacity.
// Must be called with c.mu held.
func (c *LRUCache) evictLocked() {
	for c.lru.Len() > c.maxItems {
		tail := c.lru.Back()
		if tail == nil {
			break
		}
		meta := tail.Value.(*Meta)
		_ = c.storage.Delete(meta.Key) // best-effort
		c.removeLocked(tail)
	}
}

// removeLocked removes el from both the LRU list and the items map.
// Must be called with c.mu held.
func (c *LRUCache) removeLocked(el *list.Element) {
	meta := el.Value.(*Meta)
	delete(c.items, meta.Key)
	c.lru.Remove(el)
}

// metaToStorageMeta converts cache.Meta to storage.Metadata for persistence.
func metaToStorageMeta(m *Meta) *storage.Metadata {
	custom := map[string]string{
		"key":        m.Key,
		"written_at": m.WrittenAt.Format(time.RFC3339Nano),
		"ttl":        m.TTL.String(),
	}
	sm := &storage.Metadata{CustomMeta: custom}
	if m.Headers != nil {
		sm.ContentType = m.Headers.Get("Content-Type")
		sm.CacheControl = m.Headers.Get("Cache-Control")
		sm.Expires = m.Headers.Get("Expires")
	}
	return sm
}

// ParseTTL determines the cache duration from an HTTP response.
// Priority: Cache-Control max-age → Expires → defaultTTL.
// Returns 0 if the response must not be cached (no-store / no-cache).
func ParseTTL(resp *http.Response, defaultTTL time.Duration) time.Duration {
	cc := resp.Header.Get("Cache-Control")
	if cc != "" {
		cc = strings.ToLower(cc)
		if strings.Contains(cc, "no-store") || strings.Contains(cc, "no-cache") {
			return 0
		}
		for _, part := range strings.Split(cc, ",") {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, "max-age=") {
				if secs, err := strconv.Atoi(strings.TrimPrefix(part, "max-age=")); err == nil && secs > 0 {
					return time.Duration(secs) * time.Second
				}
			}
		}
	}

	if exp := resp.Header.Get("Expires"); exp != "" {
		if t, err := http.ParseTime(exp); err == nil {
			d := time.Until(t)
			if d > 0 {
				return d
			}
		}
	}

	return defaultTTL
}
