// Package origin handles fetching resources from upstream origin servers.
package origin

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

// Puller fetches resources from a list of origin servers using round-robin
// selection with per-request timeout and automatic retry on failure.
type Puller struct {
	client *http.Client
}

// New creates a Puller with a shared HTTP client.
// The client uses no timeout by itself; callers pass timeout via context.
func New() *Puller {
	return &Puller{
		client: &http.Client{
			// Do not follow redirects automatically — pass them through.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Pull fetches the resource described by req from one of the provided origins.
//
//   - origins must be non-empty.
//   - counter is a per-domain atomic counter used for round-robin; the caller
//     owns it so that the state persists across requests for the same domain.
//   - timeout is applied per individual attempt.
//   - maxRetry controls how many additional origins are tried on failure (0 = no retry).
//
// On success the caller is responsible for closing resp.Body.
func (p *Puller) Pull(
	ctx context.Context,
	origins []string,
	counter *atomic.Uint64,
	req *http.Request,
	timeout time.Duration,
	maxRetry int,
) (*http.Response, error) {
	if len(origins) == 0 {
		return nil, fmt.Errorf("origin: no origins configured")
	}

	n := len(origins)
	start := int(counter.Add(1)-1) % n

	var lastErr error
	for attempt := 0; attempt <= maxRetry; attempt++ {
		idx := (start + attempt) % n
		origin := origins[idx]

		resp, err := p.do(ctx, origin, req, timeout)
		if err == nil {
			return resp, nil
		}
		lastErr = fmt.Errorf("origin %s: %w", origin, err)
	}
	return nil, fmt.Errorf("origin: all attempts failed: %w", lastErr)
}

// do performs a single HTTP request against originBase.
func (p *Puller) do(ctx context.Context, originBase string, orig *http.Request, timeout time.Duration) (*http.Response, error) {
	tCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Build the outbound URL: origin base + original path + query.
	target := originBase + orig.URL.RequestURI()

	outReq, err := http.NewRequestWithContext(tCtx, orig.Method, target, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	// Copy safe request headers, skipping hop-by-hop headers.
	copyHeaders(outReq.Header, orig.Header)

	resp, err := p.client.Do(outReq)
	if err != nil {
		return nil, err
	}
	// Treat 5xx as retriable errors.
	if resp.StatusCode >= 500 {
		resp.Body.Close()
		return nil, fmt.Errorf("upstream returned %d", resp.StatusCode)
	}
	return resp, nil
}

// hopByHop is the set of headers that must not be forwarded.
var hopByHop = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailers":            true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
	"Host":                true,
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if hopByHop[k] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}
