package cache

import (
	"bytes"
	"io"

	"golang.org/x/sync/singleflight"
)

// sfResult is the shared value returned by a singleflight group.
type sfResult struct {
	data []byte
	meta *Meta
}

// SingleflightCache wraps a Cache and a singleflight group to prevent cache
// stampedes: when multiple goroutines simultaneously miss the same key, only
// one fetch is executed and the result is shared among all waiters.
type SingleflightCache struct {
	cache Cache
	group singleflight.Group
}

// NewSingleflightCache wraps c with singleflight protection.
func NewSingleflightCache(c Cache) *SingleflightCache {
	return &SingleflightCache{cache: c}
}

// GetOrFetch returns the cached value for key, fetching it with fetch() if not
// present. fetch() is called at most once per key regardless of how many
// goroutines concurrently trigger a miss for the same key.
//
// The fetch function must return the full body as an io.Reader; it is buffered
// internally so that all waiters receive independent copies.
func (sc *SingleflightCache) GetOrFetch(key string, fetch func() (io.Reader, *Meta, error)) (io.ReadCloser, *Meta, error) {
	// Fast path: cache hit.
	if rc, meta, err := sc.cache.Get(key); err == nil {
		return rc, meta, nil
	}

	// Slow path: deduplicate concurrent fetches.
	v, err, _ := sc.group.Do(key, func() (any, error) {
		// Double-check after acquiring the group lock — another goroutine may
		// have already populated the cache while we were waiting.
		if rc, meta, err := sc.cache.Get(key); err == nil {
			body, readErr := io.ReadAll(rc)
			rc.Close()
			if readErr != nil {
				return nil, readErr
			}
			return &sfResult{data: body, meta: meta}, nil
		}

		r, meta, err := fetch()
		if err != nil {
			return nil, err
		}

		// Buffer the full body so we can both write to storage and return to caller.
		body, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}

		if err := sc.cache.Set(key, bytes.NewReader(body), meta); err != nil {
			// Non-fatal: log would be nice here; for now just return the body.
			_ = err
		}

		return &sfResult{data: body, meta: meta}, nil
	})

	if err != nil {
		return nil, nil, err
	}

	res := v.(*sfResult)
	// Each waiter gets its own reader over the shared byte slice (read-only).
	return io.NopCloser(bytes.NewReader(res.data)), res.meta, nil
}

// Delete removes a key from the underlying cache.
func (sc *SingleflightCache) Delete(key string) error {
	return sc.cache.Delete(key)
}

// Stat returns metadata for key.
func (sc *SingleflightCache) Stat(key string) (*Meta, error) {
	return sc.cache.Stat(key)
}
