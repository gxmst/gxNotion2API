package app

import (
	"strings"
	"testing"
)

// Certificate verification must stay ON for the normal upstream, and only be
// relaxed when the operator has deliberately redirected requests elsewhere.
func TestTLSVerificationOnlySkippedForHostOverride(t *testing.T) {
	cases := []struct {
		name           string
		tlsServerName  string
		hostHeader     string
		wantSkipVerify bool
		wantServerName string
	}{
		{name: "plain upstream", wantSkipVerify: false},
		{name: "sni override", tlsServerName: "cdn.example.com", wantSkipVerify: true, wantServerName: "cdn.example.com"},
		{name: "host override", hostHeader: "www.notion.so", wantSkipVerify: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := normalizeConfig(defaultConfig())
			cfg.UpstreamTLSServerName = tc.tlsServerName
			cfg.UpstreamHost = tc.hostHeader
			upstream := cfg.NotionUpstream()
			transport := cachedNotionHTTPTransport(cfg, "probe-"+tc.name, NewProxyResolver(cfg), upstream)
			if transport.TLSClientConfig == nil {
				t.Fatal("nil TLSClientConfig")
			}
			if got := transport.TLSClientConfig.InsecureSkipVerify; got != tc.wantSkipVerify {
				t.Errorf("InsecureSkipVerify = %v, want %v", got, tc.wantSkipVerify)
			}
			if got := transport.TLSClientConfig.ServerName; got != tc.wantServerName {
				t.Errorf("ServerName = %q, want %q", got, tc.wantServerName)
			}
		})
	}
}

// The timezone reported upstream and the Accept-Language header must agree, so
// the pair does not read as an obviously synthetic client.
func TestTimezoneAndAcceptLanguageAgree(t *testing.T) {
	cfg := normalizeConfig(defaultConfig())
	if cfg.Features.Timezone == "" {
		t.Fatal("normalizeConfig left timezone empty")
	}
	if cfg.Features.AcceptLanguage == "" {
		t.Fatal("normalizeConfig left accept_language empty")
	}
	if cfg.Features.Timezone == "Asia/Shanghai" && !strings.HasPrefix(cfg.Features.AcceptLanguage, "zh-CN") {
		t.Errorf("Asia/Shanghai paired with %q", cfg.Features.AcceptLanguage)
	}

	// An explicit timezone with no explicit language derives a matching one.
	tokyo := defaultConfig()
	tokyo.Features.Timezone = "Asia/Tokyo"
	tokyo.Features.AcceptLanguage = ""
	tokyo = normalizeConfig(tokyo)
	if !strings.HasPrefix(tokyo.Features.AcceptLanguage, "ja-JP") {
		t.Errorf("Asia/Tokyo derived %q, want ja-JP first", tokyo.Features.AcceptLanguage)
	}

	// An explicit language is respected even if it disagrees; operator's call.
	custom := defaultConfig()
	custom.Features.Timezone = "Asia/Tokyo"
	custom.Features.AcceptLanguage = "en-GB,en;q=0.9"
	custom = normalizeConfig(custom)
	if custom.Features.AcceptLanguage != "en-GB,en;q=0.9" {
		t.Errorf("explicit accept_language overwritten: %q", custom.Features.AcceptLanguage)
	}
}

func TestClientUsesConfiguredTimezoneAndLanguage(t *testing.T) {
	cfg := defaultConfig()
	cfg.Features.Timezone = "Asia/Tokyo"
	cfg.Features.AcceptLanguage = ""
	cfg = normalizeConfig(cfg)

	client := &NotionAIClient{Config: cfg, Session: SessionInfo{SpaceID: "s", UserID: "u"}}
	if got := client.upstreamTimezone(); got != "Asia/Tokyo" {
		t.Errorf("upstreamTimezone = %q", got)
	}
	if got := client.acceptLanguageHeader(); !strings.HasPrefix(got, "ja-JP") {
		t.Errorf("acceptLanguageHeader = %q, want ja-JP first", got)
	}

	// A locale cookie on the account still wins over config.
	withCookie := &NotionAIClient{
		Config: cfg,
		Session: SessionInfo{
			SpaceID: "s",
			UserID:  "u",
			Cookies: []ProbeCookie{{Name: "NEXT_LOCALE", Value: "ko-KR"}},
		},
	}
	if got := withCookie.acceptLanguageHeader(); got != "ko-KR" {
		t.Errorf("cookie locale must win, got %q", got)
	}

	// The payload carries the configured timezone.
	payload, _ := client.buildInferencePayload(PromptRunRequest{Prompt: "hi"}, "thread-1", nil)
	transcript, _ := payload["transcript"].([]map[string]any)
	found := ""
	for _, step := range transcript {
		value, _ := step["value"].(map[string]any)
		if tz := strings.TrimSpace(stringValue(value["timezone"])); tz != "" {
			found = tz
			break
		}
	}
	if found != "Asia/Tokyo" {
		t.Errorf("payload timezone = %q, want Asia/Tokyo", found)
	}
}
