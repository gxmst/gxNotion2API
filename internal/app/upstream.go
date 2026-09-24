package app

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

type NotionUpstream struct {
	BaseURL       string
	OriginURL     string
	HostHeader    string
	TLSServerName string
	UseEnvProxy   bool
}

func normalizeBaseURL(raw string) string {
	clean := strings.TrimSpace(raw)
	clean = strings.TrimRight(clean, "/")
	return clean
}

func (cfg AppConfig) NotionUpstream() NotionUpstream {
	return NotionUpstream{
		BaseURL:       normalizeBaseURL(firstNonEmpty(cfg.UpstreamBaseURL, "https://www.notion.so")),
		OriginURL:     normalizeBaseURL(firstNonEmpty(cfg.UpstreamOrigin, cfg.UpstreamBaseURL, "https://www.notion.so")),
		HostHeader:    strings.TrimSpace(cfg.UpstreamHost),
		TLSServerName: strings.TrimSpace(cfg.UpstreamTLSServerName),
		UseEnvProxy:   cfg.UpstreamUseEnvProxy,
	}
}

func (u NotionUpstream) HomeURL() string {
	return u.BaseURL + "/"
}

func (u NotionUpstream) LoginURL() string {
	return u.BaseURL + "/login"
}

func (u NotionUpstream) AIURL() string {
	return u.BaseURL + "/ai"
}

func (u NotionUpstream) API(path string) string {
	clean := strings.TrimLeft(strings.TrimSpace(path), "/")
	return u.BaseURL + "/api/v3/" + clean
}

func (u NotionUpstream) ApplyHost(req *http.Request) {
	if req == nil || strings.TrimSpace(u.HostHeader) == "" {
		return
	}
	req.Host = u.HostHeader
	req.Header.Set("Host", u.HostHeader)
}

func (u NotionUpstream) ProxyFunc() func(*http.Request) (*url.URL, error) {
	if u.UseEnvProxy {
		return proxyFromEnvironmentFresh
	}
	return nil
}

func (u NotionUpstream) CookieURL() *url.URL {
	parsed, err := url.Parse(u.HomeURL())
	if err != nil {
		return nil
	}
	return parsed
}

// validateUpstreamEndpoints limits the upstream set through the admin API to
// Notion's own hosts. Every account's token_v2 cookie is sent to BaseURL, so a
// single forged or mistaken config write naming another host would hand all
// credentials to it. Loopback stays allowed for local mocks; anything else has
// to be set in the config file or on the command line.
func validateUpstreamEndpoints(cfg AppConfig) error {
	for name, raw := range map[string]string{"upstream_base_url": cfg.UpstreamBaseURL, "upstream_origin": cfg.UpstreamOrigin} {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Host == "" {
			return fmt.Errorf("%s is not a valid URL", name)
		}
		host := strings.ToLower(parsed.Hostname())
		if isLoopbackHost(host) {
			continue
		}
		if parsed.Scheme != "https" || !(isDomainOrSubdomain(host, "notion.so") || isDomainOrSubdomain(host, "notion.com")) {
			return fmt.Errorf("%s must be an https notion.so or notion.com URL when changed through the admin API", name)
		}
	}
	return nil
}

func isDomainOrSubdomain(host, domain string) bool {
	return host == domain || strings.HasSuffix(host, "."+domain)
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func proxyFromEnvironmentFresh(req *http.Request) (*url.URL, error) {
	if req == nil || req.URL == nil {
		return nil, nil
	}
	keys := []string{}
	switch strings.ToLower(strings.TrimSpace(req.URL.Scheme)) {
	case "https":
		keys = []string{"HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"}
	default:
		keys = []string{"HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"}
	}
	for _, key := range keys {
		value := strings.TrimSpace(os.Getenv(key))
		if value == "" {
			continue
		}
		parsed, err := url.Parse(value)
		if err != nil {
			return nil, err
		}
		return parsed, nil
	}
	return nil, nil
}
