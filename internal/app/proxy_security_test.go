package app

import (
	"net/url"
	"strings"
	"testing"
)

func configWithInvalidCredentialedProxy() AppConfig {
	cfg := normalizeConfig(defaultConfig())
	cfg.ProxyMode = proxyModeHTTP
	cfg.ProxyURL = "invalid://user:password@proxy.example:8080"
	return cfg
}

func TestProxyResolutionErrorRedactsCredentials(t *testing.T) {
	cfg := configWithInvalidCredentialedProxy()
	target, err := url.Parse("https://www.notion.so/api/v3/test")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = NewProxyResolver(cfg).ResolveProxyForRequest("user@example.com", target)
	if err == nil {
		t.Fatal("expected invalid proxy configuration to fail")
	}
	message := err.Error()
	for _, secret := range []string{"user", "password", "proxy.example"} {
		if strings.Contains(message, secret) {
			t.Fatalf("proxy error leaked %q: %s", secret, message)
		}
	}
}

func TestSurfMainTransportFailsClosedOnInvalidProxy(t *testing.T) {
	client := newNotionAIClient(SessionInfo{}, configWithInvalidCredentialedProxy(), "user@example.com")
	if _, ok := client.HTTPClient.Transport.(failingRoundTripper); !ok {
		t.Fatalf("transport = %T, want failingRoundTripper", client.HTTPClient.Transport)
	}
}

func TestBrowserTransportFailsClosedOnInvalidProxy(t *testing.T) {
	client := newNotionAIClient(SessionInfo{}, configWithInvalidCredentialedProxy(), "user@example.com")
	_, err := buildBrowserTransportRequest(client, map[string]any{})
	if err == nil {
		t.Fatal("expected invalid proxy configuration to fail")
	}
}

func TestLoginTransportFailsClosedOnInvalidProxy(t *testing.T) {
	cfg := configWithInvalidCredentialedProxy()
	session := &loginHTTPSession{
		ProxyResolver: NewProxyResolver(cfg),
		AccountEmail:  "user@example.com",
	}
	_, err := buildLoginTransportRequest(session, "GET", "https://www.notion.so/login", nil, nil)
	if err == nil {
		t.Fatal("expected invalid proxy configuration to fail")
	}
}
