package app

import (
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
