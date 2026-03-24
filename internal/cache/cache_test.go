package cache

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"fileServer/internal/config"
	"fileServer/internal/storage"
)

// ── storage stub ──────────────────────────────────────────────────────────────

type memStorage struct{ data map[string][]byte }

func newMemStorage() *memStorage { return &memStorage{data: make(map[string][]byte)} }

func (m *memStorage) Read(key string) (io.ReadCloser, *storage.Metadata, error) {
	b, ok := m.data[key]
	if !ok {
		return nil, nil, io.ErrUnexpectedEOF
	}
	return io.NopCloser(bytes.NewReader(b)), &storage.Metadata{}, nil
}
func (m *memStorage) Write(key string, r io.Reader, _ *storage.Metadata) error {
	b, _ := io.ReadAll(r)
	m.data[key] = b
	return nil
}
func (m *memStorage) Delete(key string) error    { delete(m.data, key); return nil }
func (m *memStorage) Exists(key string) bool     { _, ok := m.data[key]; return ok }
func (m *memStorage) Stat(key string) (*storage.FileInfo, error) {
	b, ok := m.data[key]
	if !ok {
		return nil, io.ErrUnexpectedEOF
	}
	return &storage.FileInfo{Size: int64(len(b))}, nil
}
func (m *memStorage) List(prefix string) ([]string, error) {
	var out []string
	for k := range m.data {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	return out, nil
}

// ── KeyBuilder tests ──────────────────────────────────────────────────────────

func TestKeyNormalisesQueryOrder(t *testing.T) {
	kb := NewKeyBuilder(config.KeyRulesConfig{IncludeQueryParams: []string{"b", "a"}})
	req1, _ := http.NewRequest("GET", "/?a=1&b=2", nil)
	req2, _ := http.NewRequest("GET", "/?b=2&a=1", nil)

	k1 := kb.Build("example.com", "/path", req1, nil)
	k2 := kb.Build("example.com", "/path", req2, nil)
	if k1 != k2 {
		t.Errorf("expected same key for different param orders:\n  %s\n  %s", k1, k2)
	}
}

func TestKeyDomainRulesOverrideGlobal(t *testing.T) {
	global := config.KeyRulesConfig{IncludeQueryParams: []string{"v"}}
	domain := &config.KeyRulesConfig{IncludeQueryParams: []string{"fmt"}}

	kb := NewKeyBuilder(global)
	req, _ := http.NewRequest("GET", "/?v=1&fmt=json", nil)

	kGlobal := kb.Build("a.com", "/x", req, nil)
	kDomain := kb.Build("a.com", "/x", req, domain)

	if kGlobal == kDomain {
		t.Error("domain rules should produce a different key from global rules")
	}
	if !strings.Contains(kDomain, "fmt=json") {
		t.Errorf("domain key should include fmt param, got: %s", kDomain)
	}
}

func TestKeyHeaderHash(t *testing.T) {
	kb := NewKeyBuilder(config.KeyRulesConfig{IncludeHeaders: []string{"Accept-Language"}})
	r1, _ := http.NewRequest("GET", "/", nil)
	r1.Header.Set("Accept-Language", "zh-CN")
	r2, _ := http.NewRequest("GET", "/", nil)
	r2.Header.Set("Accept-Language", "en-US")

	k1 := kb.Build("cdn.com", "/f", r1, nil)
	k2 := kb.Build("cdn.com", "/f", r2, nil)
	if k1 == k2 {
		t.Error("different Accept-Language should produce different keys")
	}
}

// ── LRUCache tests ────────────────────────────────────────────────────────────

func makeMeta(key string, ttl time.Duration) *Meta {
	return &Meta{Key: key, WrittenAt: time.Now(), TTL: ttl, Size: 4}
}

func TestCacheHitAndMiss(t *testing.T) {
	c := NewLRUCache(10, newMemStorage())

	if err := c.Set("k1", strings.NewReader("body"), makeMeta("k1", time.Hour)); err != nil {
		t.Fatalf("Set: %v", err)
	}

	rc, _, err := c.Get("k1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	rc.Close()

	if _, _, err := c.Get("missing"); err == nil {
		t.Error("expected miss for unknown key")
	}
}

func TestCacheTTLExpiry(t *testing.T) {
	c := NewLRUCache(10, newMemStorage())
	_ = c.Set("exp", strings.NewReader("x"), makeMeta("exp", 10*time.Millisecond))

	time.Sleep(20 * time.Millisecond)

	if _, _, err := c.Get("exp"); err == nil {
		t.Error("expected expired miss")
	}
}

func TestCacheLRUEviction(t *testing.T) {
	c := NewLRUCache(2, newMemStorage())
	_ = c.Set("a", strings.NewReader("a"), makeMeta("a", time.Hour))
	_ = c.Set("b", strings.NewReader("b"), makeMeta("b", time.Hour))

	// Access "a" to make it recently used; "b" becomes LRU tail.
	rc, _, _ := c.Get("a")
	rc.Close()

	// Insert "c": "b" should be evicted (LRU).
	_ = c.Set("c", strings.NewReader("c"), makeMeta("c", time.Hour))

	if c.Len() != 2 {
		t.Errorf("expected 2 items after eviction, got %d", c.Len())
	}
	if _, _, err := c.Get("b"); err == nil {
		t.Error("expected 'b' to be evicted")
	}
	if _, rc2, err := c.Get("a"); err != nil {
		t.Errorf("expected 'a' to still exist: %v", err)
	} else {
		_ = rc2
	}
}

func TestCacheDelete(t *testing.T) {
	c := NewLRUCache(10, newMemStorage())
	_ = c.Set("del", strings.NewReader("v"), makeMeta("del", time.Hour))
	_ = c.Delete("del")

	if _, _, err := c.Get("del"); err == nil {
		t.Error("expected miss after delete")
	}
}

// ── ParseTTL tests ────────────────────────────────────────────────────────────

func TestParseTTLMaxAge(t *testing.T) {
	resp := &http.Response{Header: http.Header{"Cache-Control": []string{"max-age=300"}}}
	if d := ParseTTL(resp, time.Hour); d != 300*time.Second {
		t.Errorf("expected 300s, got %v", d)
	}
}

func TestParseTTLNoStore(t *testing.T) {
	resp := &http.Response{Header: http.Header{"Cache-Control": []string{"no-store"}}}
	if d := ParseTTL(resp, time.Hour); d != 0 {
		t.Errorf("expected 0 for no-store, got %v", d)
	}
}

func TestParseTTLDefault(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	if d := ParseTTL(resp, time.Hour); d != time.Hour {
		t.Errorf("expected default 1h, got %v", d)
	}
}

// ── SingleflightCache tests ───────────────────────────────────────────────────

func TestSingleflightDeduplicates(t *testing.T) {
	lru := NewLRUCache(10, newMemStorage())
	sc := NewSingleflightCache(lru)

	var fetchCount int
	fetch := func() (io.Reader, *Meta, error) {
		fetchCount++
		return strings.NewReader("value"), makeMeta("sf-key", time.Hour), nil
	}

	const goroutines = 50
	results := make(chan error, goroutines)
	for range goroutines {
		go func() {
			rc, _, err := sc.GetOrFetch("sf-key", fetch)
			if err == nil {
				rc.Close()
			}
			results <- err
		}()
	}

	for range goroutines {
		if err := <-results; err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	}

	if fetchCount != 1 {
		t.Errorf("expected fetch called once, got %d", fetchCount)
	}
}
