// Package mock provides in-memory implementations of service interfaces for testing.
package mock

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fileServer/internal/storage"
)

// entry holds a single stored object.
type entry struct {
	data []byte
	meta *storage.Metadata
}

// MemStorage is a thread-safe, in-memory Storage implementation for tests.
// It satisfies the storage.Storage interface.
type MemStorage struct {
	mu    sync.RWMutex
	store map[string]*entry

	// call counters (atomic)
	readCount   atomic.Int64
	writeCount  atomic.Int64
	deleteCount atomic.Int64
	existsCount atomic.Int64
	statCount   atomic.Int64
	listCount   atomic.Int64
}

// NewMemStorage creates an empty MemStorage.
func NewMemStorage() *MemStorage {
	return &MemStorage{
		store: make(map[string]*entry),
	}
}

// Read returns the content for key. Returns an error if key does not exist.
func (m *MemStorage) Read(key string) (io.ReadCloser, *storage.Metadata, error) {
	m.readCount.Add(1)
	m.mu.RLock()
	defer m.mu.RUnlock()

	e, ok := m.store[key]
	if !ok {
		return nil, nil, fmt.Errorf("storage: key not found: %s", key)
	}
	// Return a copy so callers cannot mutate stored data.
	buf := make([]byte, len(e.data))
	copy(buf, e.data)
	return io.NopCloser(bytes.NewReader(buf)), e.meta, nil
}

// Write stores r under key with the given metadata.
// The entire reader is consumed and buffered in memory.
func (m *MemStorage) Write(key string, r io.Reader, meta *storage.Metadata) error {
	m.writeCount.Add(1)

	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("storage: read body: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.store[key] = &entry{data: data, meta: meta}
	return nil
}

// Delete removes key. Returns nil if the key did not exist.
func (m *MemStorage) Delete(key string) error {
	m.deleteCount.Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.store, key)
	return nil
}

// Exists reports whether key is present.
func (m *MemStorage) Exists(key string) bool {
	m.existsCount.Add(1)
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.store[key]
	return ok
}

// Stat returns metadata for key without reading its content.
func (m *MemStorage) Stat(key string) (*storage.FileInfo, error) {
	m.statCount.Add(1)
	m.mu.RLock()
	defer m.mu.RUnlock()

	e, ok := m.store[key]
	if !ok {
		return nil, fmt.Errorf("storage: key not found: %s", key)
	}
	return &storage.FileInfo{
		Size:    int64(len(e.data)),
		ModTime: time.Now(),
	}, nil
}

// List returns all keys that start with prefix.
func (m *MemStorage) List(prefix string) ([]string, error) {
	m.listCount.Add(1)
	m.mu.RLock()
	defer m.mu.RUnlock()

	var keys []string
	for k := range m.store {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	return keys, nil
}

// --- Test helpers ---

// CallCount returns how many times the named method has been called.
// Valid names: "read", "write", "delete", "exists", "stat", "list".
func (m *MemStorage) CallCount(method string) int64 {
	switch method {
	case "read":
		return m.readCount.Load()
	case "write":
		return m.writeCount.Load()
	case "delete":
		return m.deleteCount.Load()
	case "exists":
		return m.existsCount.Load()
	case "stat":
		return m.statCount.Load()
	case "list":
		return m.listCount.Load()
	default:
		panic("mock.MemStorage.CallCount: unknown method: " + method)
	}
}

// Clear removes all stored data and resets call counters.
func (m *MemStorage) Clear() {
	m.mu.Lock()
	m.store = make(map[string]*entry)
	m.mu.Unlock()

	m.readCount.Store(0)
	m.writeCount.Store(0)
	m.deleteCount.Store(0)
	m.existsCount.Store(0)
	m.statCount.Store(0)
	m.listCount.Store(0)
}

// Keys returns a snapshot of all stored keys (for debugging).
func (m *MemStorage) Keys() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	keys := make([]string, 0, len(m.store))
	for k := range m.store {
		keys = append(keys, k)
	}
	return keys
}

// ReadJSON is a convenience helper for tests: reads key and JSON-decodes into v.
func (m *MemStorage) ReadJSON(key string, v any) error {
	rc, _, err := m.Read(key)
	if err != nil {
		return err
	}
	defer rc.Close()
	return json.NewDecoder(rc).Decode(v)
}
