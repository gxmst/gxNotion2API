package app

import (
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSurfMainTransportEnabledDefaults(t *testing.T) {
	cfg := AppConfig{}
	if !surfMainTransportEnabled(cfg) {
		t.Error("absent use_surf_main_transport must default to enabled")
	}
	off := false
	cfg.Features.UseSurfMainTransport = &off
	if surfMainTransportEnabled(cfg) {
		t.Error("explicit false must disable")
	}
	on := true
	cfg.Features.UseSurfMainTransport = &on
	if !surfMainTransportEnabled(cfg) {
		t.Error("explicit true must enable")
	}
	// normalizeConfig must materialise the default so it round-trips.
	normalized := normalizeConfig(defaultConfig())
	if normalized.Features.UseSurfMainTransport == nil {
		t.Fatal("normalizeConfig left use_surf_main_transport nil")
	}
	if !*normalized.Features.UseSurfMainTransport {
		t.Error("normalizeConfig default must be enabled")
	}
}

// The impersonating client must carry no cookie jar: the main path writes the
// `cookie` header by hand, and a jar would append its own on later requests.
func TestSurfImpersonatedClientHasNoJar(t *testing.T) {
	client, err := newSurfImpersonatedClient("")
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}
	if client.Jar != nil {
		t.Error("impersonating main-path client must not carry a cookie jar")
	}
}

func TestCachedSurfMainClientReusesTransport(t *testing.T) {
	a, err := cachedSurfMainClient("")
	if err != nil {
		t.Fatalf("first build failed: %v", err)
	}
	b, err := cachedSurfMainClient("")
	if err != nil {
		t.Fatalf("second build failed: %v", err)
	}
	if a != b {
		t.Error("same proxy key must return the cached client")
	}
	withTimeout, err := surfMainClientWithTimeout("", 5*time.Second)
	if err != nil {
		t.Fatalf("timeout wrapper failed: %v", err)
	}
	if withTimeout.Timeout != 5*time.Second {
		t.Errorf("timeout = %v, want 5s", withTimeout.Timeout)
	}
	if withTimeout.Transport != a.Transport {
		t.Error("wrapper must share the cached transport")
	}
	if withTimeout == a {
		t.Error("wrapper must not mutate the cached client's timeout")
	}
}

// A surf-side transport failure must fall back to native rather than failing the
// request, and the retry must carry the same headers and body.
func TestPostJSONFallsBackToNativeOnTransportError(t *testing.T) {
	var gotBody string
	var gotSpaceID string
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)
		gotSpaceID = r.Header.Get("x-notion-space-id")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	cfg := normalizeConfig(defaultConfig())
	cfg.APIKey = "k"
	client := &NotionAIClient{
		Config:  cfg,
		Session: SessionInfo{SpaceID: "space-42", UserID: "user-1"},
		// Primary client always fails at the transport layer.
		HTTPClient: &http.Client{Transport: alwaysFailingTransport{}},
		FallbackHTTPClient: &http.Client{
			Transport: &http.Transport{TLSClientConfig: &tls.Config{}},
			Timeout:   10 * time.Second,
		},
	}

	resp, err := client.postJSONResponse(t.Context(), srv.URL, map[string]any{"hello": "world"}, "application/json")
	if err != nil {
		t.Fatalf("expected native fallback to succeed, got %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if hits != 1 {
		t.Errorf("server saw %d requests, want 1 (only the retry)", hits)
	}
	if !strings.Contains(gotBody, "world") {
		t.Errorf("retry lost the body: %q", gotBody)
	}
	if gotSpaceID != "space-42" {
		t.Errorf("retry lost headers: x-notion-space-id = %q", gotSpaceID)
	}
}

// Without a fallback client configured, a transport error must surface as-is.
func TestPostJSONWithoutFallbackReturnsError(t *testing.T) {
	cfg := normalizeConfig(defaultConfig())
	client := &NotionAIClient{
		Config:     cfg,
		Session:    SessionInfo{SpaceID: "s", UserID: "u"},
		HTTPClient: &http.Client{Transport: alwaysFailingTransport{}},
	}
	_, err := client.postJSONResponse(t.Context(), "https://example.invalid/api/v3/x", map[string]any{}, "application/json")
	if err == nil {
		t.Fatal("expected the transport error to surface")
	}
	if !strings.Contains(err.Error(), "simulated transport failure") {
		t.Errorf("unexpected error: %v", err)
	}
}

// The impersonating transport writes its ordering hints into the outgoing
// header before it fails, using field names that end in a colon. Copying those
// into the native retry makes net/http reject the request outright, so the
// fallback has to drop them.
func TestPostJSONFallbackDropsImpersonationOrderingHeaders(t *testing.T) {
	var gotKeys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for key := range r.Header {
			gotKeys = append(gotKeys, key)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	cfg := normalizeConfig(defaultConfig())
	client := &NotionAIClient{
		Config:     cfg,
		Session:    SessionInfo{SpaceID: "space-42", UserID: "user-1"},
		HTTPClient: &http.Client{Transport: orderingHeaderInjectingTransport{}},
		FallbackHTTPClient: &http.Client{
			Transport: &http.Transport{TLSClientConfig: &tls.Config{}},
			Timeout:   10 * time.Second,
		},
	}

	resp, err := client.postJSONResponse(t.Context(), srv.URL, map[string]any{"hello": "world"}, "application/json")
	if err != nil {
		t.Fatalf("native fallback rejected the retry: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	for _, key := range gotKeys {
		if strings.Contains(key, ":") {
			t.Errorf("ordering pseudo-header reached the wire: %q", key)
		}
	}
}

func TestSanitizedHeaderForNativeTransportKeepsRealHeaders(t *testing.T) {
	src := http.Header{
		"Header-Order:":       []string{"a,b"},
		"PHeader-Order:":      []string{":method,:authority"},
		"X-Notion-Space-Id":   []string{"space-42"},
		"Accept-Language":     []string{"zh-CN"},
		"Sec-Ch-Ua-Mobile":    []string{"?0"},
		"bad space":           []string{"x"},
		"weird(paren)":        []string{"x"},
		"Fine!#$%&'*+-.^_`|~": []string{"x"},
	}
	got := sanitizedHeaderForNativeTransport(src)

	for _, dropped := range []string{"Header-Order:", "PHeader-Order:", "bad space", "weird(paren)"} {
		if _, ok := got[dropped]; ok {
			t.Errorf("kept invalid field name %q", dropped)
		}
	}
	for _, kept := range []string{"X-Notion-Space-Id", "Accept-Language", "Sec-Ch-Ua-Mobile", "Fine!#$%&'*+-.^_`|~"} {
		if _, ok := got[kept]; !ok {
			t.Errorf("dropped valid field name %q", kept)
		}
	}
	if got.Get("X-Notion-Space-Id") != "space-42" {
		t.Errorf("value lost: %q", got.Get("X-Notion-Space-Id"))
	}
	// The copy must not alias the source slices.
	got["X-Notion-Space-Id"][0] = "mutated"
	if src.Get("X-Notion-Space-Id") != "space-42" {
		t.Error("sanitized header aliases the source values")
	}
}

var errSimulatedTransport = errors.New("simulated transport failure")

type alwaysFailingTransport struct{}

func (alwaysFailingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errSimulatedTransport
}

// orderingHeaderInjectingTransport mimics surf: it stamps ordering hints onto
// the request it was handed, then fails.
type orderingHeaderInjectingTransport struct{}

func (orderingHeaderInjectingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header["Header-Order:"] = []string{"accept,user-agent,cookie"}
	req.Header["PHeader-Order:"] = []string{":method,:authority,:scheme,:path"}
	return nil, errSimulatedTransport
}
