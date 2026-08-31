package app

import (
	"net/http"
	"testing"
)

// The admin login lockout (5 failures / 15 minutes) is the only brute-force
// defence on the admin password. It is keyed on the client identity, so if that
// identity comes from an attacker-controlled header the lockout can be bypassed
// by sending a fresh value on every attempt.

func requestFrom(remoteAddr string, headers map[string]string) *http.Request {
	r := &http.Request{RemoteAddr: remoteAddr, Header: http.Header{}}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestAdminClientIPIgnoresForwardedHeadersFromUntrustedPeer(t *testing.T) {
	// No trusted proxies configured: a directly-exposed listener must never
	// believe a header, or every guess lands under a different lockout key.
	r := requestFrom("203.0.113.9:44321", map[string]string{
		"X-Forwarded-For": "1.2.3.4",
		"X-Real-IP":       "5.6.7.8",
	})
	if got := adminClientIP(r, nil); got != "203.0.113.9" {
		t.Errorf("adminClientIP = %q, want the peer address 203.0.113.9", got)
	}
}

func TestAdminClientIPRotatingForgedHeaderCannotEvadeLockout(t *testing.T) {
	// Same attacker, a new forged header each time: all attempts must collapse
	// onto one lockout key.
	seen := map[string]struct{}{}
	for _, forged := range []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4", "5.5.5.5"} {
		r := requestFrom("198.51.100.7:1234", map[string]string{"X-Forwarded-For": forged})
		seen[adminClientIP(r, nil)] = struct{}{}
	}
	if len(seen) != 1 {
		t.Errorf("forged headers produced %d distinct lockout keys, want 1: %v", len(seen), seen)
	}
}

func TestAdminClientIPHonoursTrustedProxy(t *testing.T) {
	// Behind a reverse proxy on the same host, the real client IP is the point
	// of the header -- otherwise every remote user shares the proxy's key and
	// one attacker locks everyone out.
	r := requestFrom("127.0.0.1:5555", map[string]string{"X-Forwarded-For": "203.0.113.44"})
	if got := adminClientIP(r, []string{"127.0.0.1"}); got != "203.0.113.44" {
		t.Errorf("adminClientIP = %q, want 203.0.113.44", got)
	}
}

func TestAdminClientIPTrustedProxyUsesRightmostEntry(t *testing.T) {
	// A client may prepend its own X-Forwarded-For before reaching the proxy.
	// Only the right-most hop was observed by the trusted proxy; entries to its
	// left are caller-supplied and forgeable.
	r := requestFrom("127.0.0.1:5555", map[string]string{
		"X-Forwarded-For": "9.9.9.9, 203.0.113.44",
	})
	if got := adminClientIP(r, []string{"127.0.0.1"}); got != "203.0.113.44" {
		t.Errorf("adminClientIP = %q, want the proxy-observed 203.0.113.44", got)
	}
}

func TestAdminClientIPTrustedProxyCIDR(t *testing.T) {
	r := requestFrom("172.19.0.5:5555", map[string]string{"X-Forwarded-For": "203.0.113.44"})
	if got := adminClientIP(r, []string{"172.19.0.0/16"}); got != "203.0.113.44" {
		t.Errorf("CIDR trust failed: got %q", got)
	}
	// A peer outside the block must not be trusted.
	other := requestFrom("10.0.0.5:5555", map[string]string{"X-Forwarded-For": "203.0.113.44"})
	if got := adminClientIP(other, []string{"172.19.0.0/16"}); got != "10.0.0.5" {
		t.Errorf("peer outside the trusted block was believed: got %q", got)
	}
}

func TestAdminClientIPRejectsGarbageForwardedValue(t *testing.T) {
	// A non-IP header value from a trusted proxy must not become a lockout key;
	// fall back to the peer instead.
	r := requestFrom("127.0.0.1:5555", map[string]string{"X-Forwarded-For": "not-an-ip"})
	if got := adminClientIP(r, []string{"127.0.0.1"}); got != "127.0.0.1" {
		t.Errorf("adminClientIP = %q, want fallback to 127.0.0.1", got)
	}
}

func TestProxyIsTrusted(t *testing.T) {
	cases := []struct {
		peer    string
		trusted []string
		want    bool
	}{
		{"127.0.0.1", nil, false},
		{"127.0.0.1", []string{}, false},
		{"127.0.0.1", []string{"127.0.0.1"}, true},
		{"127.0.0.1", []string{"  "}, false},
		{"172.19.0.1", []string{"172.19.0.0/16"}, true},
		{"172.20.0.1", []string{"172.19.0.0/16"}, false},
		{"::1", []string{"::1"}, true},
		{"not-an-ip", []string{"127.0.0.1"}, false},
		{"127.0.0.1", []string{"garbage/99"}, false},
	}
	for _, c := range cases {
		if got := proxyIsTrusted(c.peer, c.trusted); got != c.want {
			t.Errorf("proxyIsTrusted(%q, %v) = %v, want %v", c.peer, c.trusted, got, c.want)
		}
	}
}

func TestShouldUseSecureCookieBehindTLSTerminatingProxy(t *testing.T) {
	// The reverse proxy terminates TLS, so Go sees plain HTTP; without honouring
	// X-Forwarded-Proto the session cookie would ship without Secure.
	r := requestFrom("127.0.0.1:5555", map[string]string{"X-Forwarded-Proto": "https"})
	if !shouldUseSecureCookie(r) {
		t.Error("expected Secure cookie when X-Forwarded-Proto is https")
	}
	plain := requestFrom("127.0.0.1:5555", nil)
	if shouldUseSecureCookie(plain) {
		t.Error("expected no Secure flag for plain http")
	}
}
