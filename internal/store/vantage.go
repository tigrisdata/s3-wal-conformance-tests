package store

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"
)

// Serving-region capture and per-vantage transport routing.
//
// A conformance claim is only as global as its vantages actually are, and a
// URL alone proves nothing: many providers front every hostname with one
// anycast address, so two "vantages" from one machine can silently land on
// the same regional gateway. Two mechanisms fix that:
//
//   - each vantage may route through its own proxy (typically a SOCKS
//     tunnel to a host in the target region, e.g. `ssh -N -D`), so the
//     connection genuinely originates there;
//   - when the provider exposes a response header naming the region that
//     served each request (Tigris: X-Tigris-Served-From), the transport
//     records it per operation, the store tallies it per vantage, and the
//     report upgrades to a verified multi-vantage stamp only when the
//     observed regions actually differ.

// RegionCapture receives the serving-region header value for the operation
// whose context carries it.
type RegionCapture struct {
	mu  sync.Mutex
	val string
}

func (r *RegionCapture) set(v string) {
	r.mu.Lock()
	r.val = v
	r.mu.Unlock()
}

// Value returns the captured serving region, or "" if none was observed.
func (r *RegionCapture) Value() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.val
}

type captureKey struct{}

// ContextWithRegionCapture attaches a RegionCapture to the context; the
// vantage's transport fills it with the serving-region header of the next
// response issued under this context.
func ContextWithRegionCapture(ctx context.Context) (context.Context, *RegionCapture) {
	rc := &RegionCapture{}
	return context.WithValue(ctx, captureKey{}, rc), rc
}

func captureFrom(ctx context.Context) *RegionCapture {
	rc, _ := ctx.Value(captureKey{}).(*RegionCapture)
	return rc
}

// capturingTransport reads the configured serving-region header off every
// response, filling the context's RegionCapture when one is attached.
type capturingTransport struct {
	base   http.RoundTripper
	header string
}

func (t *capturingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err == nil && resp != nil {
		if rc := captureFrom(req.Context()); rc != nil {
			rc.set(resp.Header.Get(t.header))
		}
	}
	return resp, err
}

// buildHTTPClient assembles the vantage's HTTP client: an optional
// per-vantage proxy plus the optional serving-region capture.
func buildHTTPClient(proxyURL, regionHeader string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("proxy %q: %w", proxyURL, err)
		}
		switch u.Scheme {
		case "socks5", "socks5h", "http", "https":
		default:
			return nil, fmt.Errorf("proxy %q: unsupported scheme %q (use socks5://, http:// or https://)", proxyURL, u.Scheme)
		}
		transport.Proxy = http.ProxyURL(u)
	}
	var rt http.RoundTripper = transport
	if regionHeader != "" {
		rt = &capturingTransport{base: transport, header: regionHeader}
	}
	return &http.Client{Transport: rt}, nil
}

// regionTally counts serving regions observed at one vantage.
type regionTally struct {
	mu     sync.Mutex
	counts map[string]int64
}

func (t *regionTally) add(region string) {
	if region == "" {
		return
	}
	t.mu.Lock()
	if t.counts == nil {
		t.counts = map[string]int64{}
	}
	t.counts[region]++
	t.mu.Unlock()
}

func (t *regionTally) snapshot() map[string]int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]int64, len(t.counts))
	for k, v := range t.counts {
		out[k] = v
	}
	return out
}
