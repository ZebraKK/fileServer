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
// Puller is stateless with respect to domain configuration; all parameters
// are passed per call.
type Puller struct {
	client  *http.Client
	counter atomic.Uint64 // shared round-robin counter across all domains
}

// New creates a Puller with a shared HTTP client.
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

// Pull fetches the resource at path from one of the provided origins.
//
//   - origins must be non-empty.
//   - timeout is applied per individual attempt.
//   - retry controls how many additional origins are tried on failure (0 = no retry).
//   - path is the request URI to fetch (may include query string).
//   - header contains request headers to forward (hop-by-hop headers are filtered).
//
// On success the caller is responsible for closing resp.Body.
func (p *Puller) Pull(
	ctx context.Context,
	origins []string,
	timeout time.Duration,
	retry int,
	path string,
	header http.Header,
) (*http.Response, error) {
	if len(origins) == 0 {
		return nil, fmt.Errorf("origin: no origins configured")
	}

	n := len(origins)
	start := int(p.counter.Add(1)-1) % n

	var lastErr error
	for attempt := 0; attempt <= retry; attempt++ {
		idx := (start + attempt) % n
		resp, err := p.do(ctx, origins[idx], path, header, timeout)
		if err == nil {
			return resp, nil
		}
		lastErr = fmt.Errorf("origin %s: %w", origins[idx], err)
	}
	return nil, fmt.Errorf("origin: all attempts failed: %w", lastErr)
}

// do performs a single HTTP GET against originBase+path.
func (p *Puller) do(ctx context.Context, originBase, path string, header http.Header, timeout time.Duration) (*http.Response, error) {
	tCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	target := originBase + path

	outReq, err := http.NewRequestWithContext(tCtx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	// Copy safe request headers, skipping hop-by-hop headers.
	copyHeaders(outReq.Header, header)

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
