package app

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type loginTransportRequest struct {
	Method           string            `json:"method"`
	URL              string            `json:"url"`
	Headers          map[string]string `json:"headers"`
	Body             string            `json:"body,omitempty"`
	Cookies          []ProbeCookie     `json:"cookies"`
	BrowserProfile   string            `json:"browser_profile,omitempty"`
	Proxy            string            `json:"proxy,omitempty"`
	RequestTimeoutMS int               `json:"request_timeout_ms"`
}

type loginTransportResponse struct {
	Status      int               `json:"status"`
	ContentType string            `json:"content_type"`
	Headers     map[string]string `json:"headers"`
	Body        string            `json:"body"`
	SetCookies  []ProbeCookie     `json:"set_cookies"`
}

type loginHTTPSession struct {
	*http.Client
	ProxyResolver          *ProxyResolver
	AccountEmail           string
	Timeout                time.Duration
	Upstream               NotionUpstream
	UseSurfHelperTransport bool
}

var (
	loginTransportRunSurfRequest     = runLoginHelperRequestWithSurf
	loginTransportRunFallbackRequest = runLoginHelperRequestWithSurf
)

func loginTransportDoRequest(ctx context.Context, session *loginHTTPSession, method string, targetURL string, headers map[string]string, body []byte) (int, http.Header, []byte, error) {
	if session == nil {
		return 0, nil, nil, fmt.Errorf("login session is nil")
	}
	request, err := buildLoginTransportRequest(session, method, targetURL, headers, body)
	if err != nil {
		return 0, nil, nil, err
	}
	var resp *loginTransportResponse
	resp, err = loginTransportRunSurfRequest(ctx, request)
	if err != nil {
		return 0, nil, nil, err
	}
	if session.Client != nil {
		applyLoginTransportSetCookies(session.Jar, targetURL, resp.SetCookies)
	}
	return resp.Status, loginTransportHTTPHeader(resp.Headers), []byte(resp.Body), nil
}

func buildLoginTransportRequest(session *loginHTTPSession, method string, targetURL string, headers map[string]string, body []byte) (loginTransportRequest, error) {
	cookies := []ProbeCookie{}
	if session != nil && session.Client != nil {
		cookies = probeCookiesFromJar(session.Jar, targetURL)
	}
	proxyValue := ""
	if session != nil && session.ProxyResolver != nil {
		parsed, parseErr := url.Parse(targetURL)
		if parseErr != nil {
			return loginTransportRequest{}, fmt.Errorf("invalid login target URL: %w", parseErr)
		}
		proxyURL, _, resolveErr := session.ProxyResolver.ResolveProxyForRequest(session.AccountEmail, parsed)
		if resolveErr != nil {
			return loginTransportRequest{}, resolveErr
		}
		if proxyURL != nil {
			proxyValue = proxyURL.String()
		}
	}
	timeoutMS := 60000
	if session != nil && session.Timeout > 0 {
		timeoutMS = int(session.Timeout / time.Millisecond)
	}
	if timeoutMS < 30000 {
		timeoutMS = 30000
	}
	cleanHeaders := make(map[string]string, len(headers))
	for k, v := range headers {
		if strings.TrimSpace(k) == "" {
			continue
		}
		cleanHeaders[k] = v
	}
	return loginTransportRequest{
		Method:           strings.ToUpper(strings.TrimSpace(method)),
		URL:              targetURL,
		Headers:          cleanHeaders,
		Body:             string(body),
		Cookies:          cookies,
		BrowserProfile:   notionTransportDefaultBrowserProfile,
		Proxy:            proxyValue,
		RequestTimeoutMS: timeoutMS,
	}, nil
}

func applyLoginTransportSetCookies(jar http.CookieJar, targetURL string, setCookies []ProbeCookie) {
	if jar == nil || len(setCookies) == 0 {
		return
	}
	parsed, err := url.Parse(targetURL)
	if err != nil {
		return
	}
	items := make([]*http.Cookie, 0, len(setCookies))
	for _, c := range setCookies {
		name := strings.TrimSpace(c.Name)
		if name == "" {
			continue
		}
		items = append(items, &http.Cookie{Name: name, Value: cookieRequestValue(c.Value), Path: "/"})
	}
	if len(items) > 0 {
		jar.SetCookies(parsed, items)
	}
}

func loginTransportHTTPHeader(headers map[string]string) http.Header {
	out := http.Header{}
	for k, v := range headers {
		out.Set(k, v)
	}
	return out
}
