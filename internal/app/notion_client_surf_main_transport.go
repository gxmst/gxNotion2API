package app

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/enetx/g"
	"github.com/enetx/surf"
)

// The main inference path used to run on Go's net/http. That sends a Chrome
// user-agent and Chrome sec-ch-ua headers over a TLS handshake and HTTP/2
// SETTINGS frame that are unmistakably Go's, which upstream notices: a denied
// request comes back as sub_type=trust-rule-denied. Routing the same requests
// through surf's utls-backed impersonation makes the wire-level fingerprint
// match the headers we already claim.
//
// Cookies stay hand-written in the `cookie` header (see cookieHeader), so these
// clients are built WITHOUT surf's .Session() jar. A jar would inject its own
// cookies alongside ours and send duplicates.

type surfMainClientCacheKey struct {
	proxy string
}

var surfMainClientCache = struct {
	mu    sync.RWMutex
	items map[surfMainClientCacheKey]*http.Client
}{
	items: map[surfMainClientCacheKey]*http.Client{},
}

// surfMainTransportEnabled reports whether the main path should impersonate.
// Absent config means enabled.
func surfMainTransportEnabled(cfg AppConfig) bool {
	if cfg.Features.UseSurfMainTransport == nil {
		return true
	}
	return *cfg.Features.UseSurfMainTransport
}

// newSurfImpersonatedClient builds a Chrome-impersonating client with no cookie
// jar, suitable for the main request path.
func newSurfImpersonatedClient(proxy string) (*http.Client, error) {
	builder := surf.NewClient().Builder().Impersonate().Chrome()
	if strings.TrimSpace(proxy) != "" {
		builder = builder.Proxy(g.String(proxy))
	}
	clientResult := builder.Build()
	if err := clientResult.Err(); err != nil {
		return nil, err
	}
	return clientResult.Unwrap().Std(), nil
}

// cachedSurfMainClient reuses one impersonating client per proxy so connections
// and the TLS session cache are shared, mirroring the native transport cache.
// The returned client's Timeout is not set; callers wrap it (see
// surfMainClientWithTimeout) because timeouts differ between streaming and
// non-streaming requests.
func cachedSurfMainClient(proxy string) (*http.Client, error) {
	key := surfMainClientCacheKey{proxy: strings.TrimSpace(proxy)}
	surfMainClientCache.mu.RLock()
	if existing := surfMainClientCache.items[key]; existing != nil {
		surfMainClientCache.mu.RUnlock()
		return existing, nil
	}
	surfMainClientCache.mu.RUnlock()

	client, err := newSurfImpersonatedClient(proxy)
	if err != nil {
		return nil, err
	}
	surfMainClientCache.mu.Lock()
	if existing := surfMainClientCache.items[key]; existing != nil {
		surfMainClientCache.mu.Unlock()
		return existing, nil
	}
	surfMainClientCache.items[key] = client
	surfMainClientCache.mu.Unlock()
	return client, nil
}

// surfMainClientWithTimeout returns a shallow client sharing the cached
// transport but carrying its own timeout. Streaming requests pass 0.
func surfMainClientWithTimeout(proxy string, timeout time.Duration) (*http.Client, error) {
	base, err := cachedSurfMainClient(proxy)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Transport:     base.Transport,
		Jar:           base.Jar,
		CheckRedirect: base.CheckRedirect,
		Timeout:       timeout,
	}, nil
}

// resolveStaticProxyForUpstream resolves the account's proxy once at client
// build time. surf bakes the proxy into the client, unlike net/http's
// per-request Proxy func, so account-scoped routing is applied here instead.
// A resolution error is reported, not swallowed: callers must fail closed on a
// broken proxy rather than silently bypassing it and exposing the real address.
func resolveStaticProxyForUpstream(resolver *ProxyResolver, accountEmail string, upstream NotionUpstream) (string, error) {
	if resolver == nil {
		return "", nil
	}
	target := upstream.CookieURL()
	if target == nil {
		return "", nil
	}
	proxyURL, _, err := resolver.ResolveProxyForRequest(accountEmail, target)
	if err != nil {
		return "", err
	}
	if proxyURL == nil {
		return "", nil
	}
	return proxyURL.String(), nil
}

// failingRoundTripper fails every request. It is installed when the account's
// upstream proxy cannot be resolved, so a broken proxy configuration fails
// closed instead of silently going direct and handing the session cookies to
// Notion from the operator's real address.
type failingRoundTripper struct{}

type unavailableSurfTransport struct{ err error }

func (t unavailableSurfTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, t.err
}

func (failingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("upstream proxy resolution failed; refusing to send requests without it")
}
