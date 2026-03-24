package origin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestPullRoundRobin(t *testing.T) {
	var calls [3]atomic.Int32
	servers := make([]*httptest.Server, 3)
	origins := make([]string, 3)
	for i := range servers {
		i := i
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls[i].Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		defer servers[i].Close()
		origins[i] = servers[i].URL
	}

	p := New()
	req, _ := http.NewRequest(http.MethodGet, "/test", nil)
	var counter atomic.Uint64

	for range 6 {
		resp, err := p.Pull(context.Background(), origins, &counter, req, 5*time.Second, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		resp.Body.Close()
	}

	for i, c := range calls {
		if c.Load() != 2 {
			t.Errorf("server %d: expected 2 calls, got %d", i, c.Load())
		}
	}
}

func TestPullRetryOnFailure(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer good.Close()

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()

	p := New()
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	var counter atomic.Uint64

	// bad is first in list; should retry and succeed on good.
	resp, err := p.Pull(context.Background(), []string{bad.URL, good.URL}, &counter, req, 5*time.Second, 1)
	if err != nil {
		t.Fatalf("expected success after retry, got: %v", err)
	}
	resp.Body.Close()
}

func TestPullTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer slow.Close()

	p := New()
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	var counter atomic.Uint64

	_, err := p.Pull(context.Background(), []string{slow.URL}, &counter, req, 50*time.Millisecond, 0)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

func TestPullNoOrigins(t *testing.T) {
	p := New()
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	var counter atomic.Uint64
	_, err := p.Pull(context.Background(), nil, &counter, req, time.Second, 0)
	if err == nil {
		t.Fatal("expected error for empty origins")
	}
}
