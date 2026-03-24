package flush

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"fileServer/internal/storage"
)

// ── in-memory Storage stub ────────────────────────────────────────────────────

type memStore struct {
	data map[string][]byte
}

func newMemStore() *memStore { return &memStore{data: make(map[string][]byte)} }

func (m *memStore) Read(key string) (io.ReadCloser, *storage.Metadata, error) {
	b, ok := m.data[key]
	if !ok {
		return nil, nil, io.ErrUnexpectedEOF
	}
	return io.NopCloser(bytes.NewReader(b)), &storage.Metadata{}, nil
}

func (m *memStore) Write(key string, r io.Reader, _ *storage.Metadata) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.data[key] = b
	return nil
}

func (m *memStore) Delete(key string) error    { delete(m.data, key); return nil }
func (m *memStore) Exists(key string) bool     { _, ok := m.data[key]; return ok }
func (m *memStore) List(prefix string) ([]string, error) {
	var out []string
	for k := range m.data {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	return out, nil
}
func (m *memStore) Stat(key string) (*storage.FileInfo, error) {
	b, ok := m.data[key]
	if !ok {
		return nil, io.ErrUnexpectedEOF
	}
	return &storage.FileInfo{Size: int64(len(b))}, nil
}

// ── tests ─────────────────────────────────────────────────────────────────────

func TestAddAndMatch(t *testing.T) {
	st := New(newMemStore())

	if err := st.AddRule("cdn.example.com", "/static/"); err != nil {
		t.Fatalf("AddRule: %v", err)
	}

	r := st.Match("cdn.example.com", "/static/app.js")
	if r == nil {
		t.Fatal("expected rule match, got nil")
	}
	if r.Prefix != "/static/" {
		t.Errorf("unexpected prefix: %s", r.Prefix)
	}

	// Different domain must not match.
	if st.Match("other.example.com", "/static/app.js") != nil {
		t.Error("expected no match for different domain")
	}

	// Path outside prefix must not match.
	if st.Match("cdn.example.com", "/api/data") != nil {
		t.Error("expected no match for path outside prefix")
	}
}

func TestLatestRuleWins(t *testing.T) {
	st := New(newMemStore())
	_ = st.AddRule("cdn.example.com", "/a/")
	time.Sleep(2 * time.Millisecond)
	_ = st.AddRule("cdn.example.com", "/")

	r := st.Match("cdn.example.com", "/a/b.js")
	if r == nil || r.Prefix != "/" {
		t.Errorf("expected latest rule '/', got %v", r)
	}
}

func TestPersistAndLoad(t *testing.T) {
	mem := newMemStore()

	st1 := New(mem)
	if err := st1.AddRule("cdn.example.com", "/img/"); err != nil {
		t.Fatalf("AddRule: %v", err)
	}

	// Simulate restart: create new Store backed by same storage.
	st2 := New(mem)
	if err := st2.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	r := st2.Match("cdn.example.com", "/img/logo.png")
	if r == nil {
		t.Fatal("rule not restored after load")
	}
}

func TestCleanup(t *testing.T) {
	st := New(newMemStore())
	_ = st.AddRule("cdn.example.com", "/old/")

	// Cleanup with zero maxAge removes everything.
	if err := st.Cleanup(0); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if st.Match("cdn.example.com", "/old/file.js") != nil {
		t.Error("expected rule to be cleaned up")
	}
	if st.RuleCount("cdn.example.com") != 0 {
		t.Errorf("expected 0 rules after cleanup, got %d", st.RuleCount("cdn.example.com"))
	}
}

func TestDomainFlush(t *testing.T) {
	st := New(newMemStore())
	_ = st.AddRule("cdn.example.com", "/")

	for _, path := range []string{"/", "/a", "/b/c/d.js"} {
		if st.Match("cdn.example.com", path) == nil {
			t.Errorf("expected match for path %s with domain flush", path)
		}
	}
}
