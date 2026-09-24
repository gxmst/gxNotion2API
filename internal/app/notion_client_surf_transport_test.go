package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRunLoginHelperRequestWithSurf_MapsStatusHeadersBodyAndSetCookies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Method; got != http.MethodPost {
			t.Fatalf("method = %s, want POST", got)
		}
		if got := r.Header.Get("X-Test"); got != "ok" {
			t.Fatalf("X-Test header = %q, want ok", got)
		}
		http.SetCookie(w, &http.Cookie{Name: "token_v2", Value: "new-value", Path: "/"})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	resp, err := runLoginHelperRequestWithSurf(context.Background(), loginTransportRequest{
		Method:           http.MethodPost,
		URL:              server.URL,
		Headers:          map[string]string{"X-Test": "ok"},
		Body:             `{"hello":"world"}`,
		RequestTimeoutMS: 30000,
	})
	if err != nil {
		t.Fatalf("runLoginHelperRequestWithSurf error: %v", err)
	}
	if resp.Status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", resp.Status, http.StatusCreated)
	}
	if !strings.Contains(strings.ToLower(resp.ContentType), "application/json") {
		t.Fatalf("content_type = %q", resp.ContentType)
	}
	if strings.TrimSpace(resp.Body) != `{"ok":true}` {
		t.Fatalf("body = %q", resp.Body)
	}
	if len(resp.SetCookies) == 0 || resp.SetCookies[0].Name != "token_v2" {
		t.Fatalf("set_cookies = %#v", resp.SetCookies)
	}
}

func TestRunLoginHelperRequestWithSurf_ContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := runLoginHelperRequestWithSurf(ctx, loginTransportRequest{
		Method:           http.MethodGet,
		URL:              "https://example.com",
		RequestTimeoutMS: 30000,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestRunLoginHelperRequestWithSurf_PreservesRedirectSetCookies(t *testing.T) {
	const cookieName = "redirect_token"
	const cookieValue = "set-on-redirect-hop"

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: cookieValue, Path: "/"})
		http.Redirect(w, r, server.URL+"/final", http.StatusFound)
	})
	mux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("done"))
	})

	resp, err := runLoginHelperRequestWithSurf(context.Background(), loginTransportRequest{
		Method:           http.MethodGet,
		URL:              server.URL + "/start",
		RequestTimeoutMS: 30000,
	})
	if err != nil {
		t.Fatalf("runLoginHelperRequestWithSurf error: %v", err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.Status, http.StatusOK)
	}
	if got := probeCookieValue(resp.SetCookies, cookieName); got != cookieValue {
		t.Fatalf("redirect cookie mismatch: got %q want %q, set_cookies=%#v", got, cookieValue, resp.SetCookies)
	}
}

func TestLoginTransportDoRequest_UsesSurfTransport(t *testing.T) {
	origSurf := loginTransportRunSurfRequest
	origFallback := loginTransportRunFallbackRequest
	defer func() {
		loginTransportRunSurfRequest = origSurf
		loginTransportRunFallbackRequest = origFallback
	}()

	surfHits := 0
	fallbackHits := 0
	loginTransportRunSurfRequest = func(_ context.Context, _ loginTransportRequest) (*loginTransportResponse, error) {
		surfHits++
		return &loginTransportResponse{
			Status:     http.StatusCreated,
			Headers:    map[string]string{"x-transport": "surf"},
			Body:       "surf",
			SetCookies: []ProbeCookie{{Name: "token_v2", Value: "surf"}},
		}, nil
	}
	loginTransportRunFallbackRequest = func(_ context.Context, _ loginTransportRequest) (*loginTransportResponse, error) {
		fallbackHits++
		return &loginTransportResponse{
			Status:     http.StatusAccepted,
			Headers:    map[string]string{"x-transport": "fallback"},
			Body:       "fallback",
			SetCookies: []ProbeCookie{{Name: "token_v2", Value: "fallback"}},
		}, nil
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New error: %v", err)
	}
	session := &loginHTTPSession{
		Client:                 &http.Client{Jar: jar},
		UseSurfHelperTransport: true,
		ProxyResolver:          nil,
		AccountEmail:           "tester@example.com",
		Timeout:                30 * time.Second,
		Upstream:               NotionUpstream{},
	}

	targetURL := "https://example.com/login"
	status, headers, body, err := loginTransportDoRequest(context.Background(), session, http.MethodGet, targetURL, map[string]string{"X-Test": "1"}, nil)
	if err != nil {
		t.Fatalf("loginTransportDoRequest error: %v", err)
	}
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want %d", status, http.StatusCreated)
	}
	if got := headers.Get("x-transport"); got != "surf" {
		t.Fatalf("x-transport = %q, want %q", got, "surf")
	}
	if got := string(body); got != "surf" {
		t.Fatalf("body = %q, want %q", got, "surf")
	}
	if surfHits != 1 {
		t.Fatalf("surf branch hits mismatch: got %d want 1", surfHits)
	}
	if fallbackHits != 0 {
		t.Fatalf("fallback branch should stay unused, got hits=%d", fallbackHits)
	}
	if got := probeCookieValue(probeCookiesFromJar(session.Jar, targetURL), "token_v2"); got != "surf" {
		t.Fatalf("session jar token_v2 = %q, want %q", got, "surf")
	}
}

func TestLoginTransportDoRequest_SurfPreservesRedirectCookiesInSessionJar(t *testing.T) {
	const cookieName = "redirect_token"
	const cookieValue = "persisted"

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: cookieValue, Path: "/"})
		http.Redirect(w, r, server.URL+"/final", http.StatusFound)
	})
	mux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New error: %v", err)
	}
	session := &loginHTTPSession{
		Client:                 &http.Client{Jar: jar},
		UseSurfHelperTransport: true,
		Timeout:                30 * time.Second,
	}

	targetURL := server.URL + "/start"
	status, _, _, err := loginTransportDoRequest(context.Background(), session, http.MethodGet, targetURL, nil, nil)
	if err != nil {
		t.Fatalf("loginTransportDoRequest error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if got := probeCookieValue(probeCookiesFromJar(session.Jar, targetURL), cookieName); got != cookieValue {
		t.Fatalf("session jar redirect cookie mismatch: got %q want %q", got, cookieValue)
	}
}

func TestRunInferenceTranscriptInBrowserWithSurf_ReturnsNDJSON(t *testing.T) {
	line := `{"type":"agent-inference","id":"m1","finishedAt":"2026-05-03T00:00:00Z","value":[{"type":"text","content":"OK"}]}` + "\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Method; got != http.MethodPost {
			t.Fatalf("method = %s, want POST", got)
		}
		if got := strings.TrimSpace(r.Header.Get("Cookie")); got == "" {
			t.Fatalf("expected cookie header to be present")
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode payload failed: %v", err)
		}
		if got := strings.TrimSpace(stringValue(payload["threadId"])); got != "t1" {
			t.Fatalf("threadId = %q, want t1", got)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(line))
	}))
	defer server.Close()

	client := newBrowserFallbackTestClient(server.URL)
	body, err := runInferenceTranscriptInBrowserWithSurf(context.Background(), client, map[string]any{"threadId": "t1"})
	if err != nil {
		t.Fatalf("runInferenceTranscriptInBrowserWithSurf error: %v", err)
	}
	if body != line {
		t.Fatalf("body mismatch: got %q want %q", body, line)
	}
}

func TestRunInferenceTranscriptInBrowserWithSurf_RejectsHTMLChallenge(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body>cloudflare cookiePart challenge</body></html>"))
	}))
	defer server.Close()

	client := newBrowserFallbackTestClient(server.URL)
	_, err := runInferenceTranscriptInBrowserWithSurf(context.Background(), client, map[string]any{"threadId": "t1"})
	if err == nil || !strings.Contains(err.Error(), "challenge/html content") {
		t.Fatalf("unexpected err: %v", err)
	}
}

// Stored cookies are captured verbatim, and some values cannot be emitted as a
// cookie value at all: Google's g_state is JSON, so it is full of double quotes.
// net/http strips those bytes itself and logs a warning on every request. We now
// strip them before handing the cookie over, so the header must stay byte-identical
// to what net/http produced on its own -- the fix removes noise, not behaviour.
func TestProbeCookieValuesSendExactlyWhatNetHTTPWould(t *testing.T) {
	// Shaped like the real g_state value: JSON, so full of quotes.
	raw := `{"i_l":0,"i_ll":1234567890,"i_b":"abc","i_e":1}`
	if !strings.Contains(raw, `"`) {
		t.Fatal("precondition failed: the sample carries no quote to strip")
	}

	headerFor := func(gState string) string {
		t.Helper()
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, "https://www.notion.so/api/v3/getSpaces", nil)
		loadProbeCookiesIntoJar(jar, req.URL, []ProbeCookie{
			{Name: "g_state", Value: gState},
			{Name: "token_v2", Value: "plain-value"},
		})
		for _, cookie := range jar.Cookies(req.URL) {
			req.AddCookie(cookie)
		}
		return req.Header.Get("Cookie")
	}

	want := headerFor(raw)                    // net/http sanitizes internally
	got := headerFor(cookieRequestValue(raw)) // we sanitize up front
	if got != want {
		t.Fatalf("pre-sanitized header differs from net/http's own result:\n got %q\nwant %q", got, want)
	}

	cleaned := cookieRequestValue(raw)
	if strings.Contains(cleaned, `"`) {
		t.Fatalf("cleaned value still carries the value's own quotes: %q", cleaned)
	}
	// The value keeps its commas, so net/http still wraps it in the one outer
	// quote pair RFC 6265 requires. That pair is legitimate; what must be gone
	// is the value's own inner quoting.
	if n := strings.Count(got, `"`); n != 2 {
		t.Fatalf("expected exactly one outer quote pair, found %d quotes in %q", n, got)
	}
	if !strings.Contains(got, "token_v2=plain-value") {
		t.Fatalf("an unrelated cookie was altered: %q", got)
	}
}

// The whole point of the change: the warning that used to fire on every single
// request must stop, while net/http still warns about a value we failed to clean.
func TestCleanedCookieValueNoLongerWarns(t *testing.T) {
	raw := `{"i_l":0,"i_ll":1234567890}`
	for _, tc := range []struct {
		name      string
		value     string
		wantQuiet bool
	}{
		{name: "cleaned value stays quiet", value: cookieRequestValue(raw), wantQuiet: true},
		{name: "raw value still warns", value: raw, wantQuiet: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var captured bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&captured)
			defer log.SetOutput(previous)

			req := httptest.NewRequest(http.MethodGet, "https://www.notion.so/api/v3/getSpaces", nil)
			req.AddCookie(&http.Cookie{Name: "g_state", Value: tc.value})

			warned := strings.Contains(captured.String(), "invalid byte")
			if tc.wantQuiet && warned {
				t.Fatalf("still warned after cleaning: %s", captured.String())
			}
			if !tc.wantQuiet && !warned {
				t.Fatal("net/http did not warn about the raw value; this test is no longer exercising the bug")
			}
		})
	}
}

// The byte rule has to match net/http's own, or we would silently change what
// gets sent.
func TestCookieRequestValueMatchesNetHTTPByteRule(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{name: "plain value untouched", in: "token_v2=abc123", want: "token_v2=abc123"},
		{name: "quotes dropped", in: `a"b"c`, want: "abc"},
		{name: "semicolon dropped", in: "a;b", want: "ab"},
		{name: "backslash dropped", in: `a\b`, want: "ab"},
		{name: "control byte dropped", in: "a\nb", want: "ab"},
		{name: "high byte dropped", in: "a\xffb", want: "ab"},
		{name: "empty stays empty", in: "", want: ""},
		{name: "only invalid bytes becomes empty", in: `"""`, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cookieRequestValue(tc.in); got != tc.want {
				t.Fatalf("cookieRequestValue(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Defining the cleaner is not enough -- the production path has to apply it, so
// assert on what actually lands in the jar.
func TestLoadProbeCookiesIntoJarStoresCleanValues(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "https://www.notion.so/api/v3/getSpaces", nil)
	loadProbeCookiesIntoJar(jar, req.URL, []ProbeCookie{
		{Name: "g_state", Value: `{"i_l":0,"i_ll":1234567890}`},
		{Name: "token_v2", Value: "plain-value"},
	})

	seen := map[string]string{}
	for _, cookie := range jar.Cookies(req.URL) {
		seen[cookie.Name] = cookie.Value
	}
	gState, ok := seen["g_state"]
	if !ok {
		t.Fatal("g_state was not loaded into the jar")
	}
	if strings.Contains(gState, `"`) {
		t.Fatalf("loadProbeCookiesIntoJar kept an unrepresentable value: %q", gState)
	}
	if seen["token_v2"] != "plain-value" {
		t.Fatalf("an unrelated cookie was altered: %q", seen["token_v2"])
	}
}

// The login transport loads cookies into its own jar; it needs the same care.
func TestApplyLoginTransportSetCookiesStoresCleanValues(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "https://www.notion.so/api/v3/getSpaces", nil)
	applyLoginTransportSetCookies(jar, req.URL.String(), []ProbeCookie{
		{Name: "g_state", Value: `{"i_l":0,"i_b":"abc"}`},
	})

	seen := map[string]string{}
	for _, cookie := range jar.Cookies(req.URL) {
		seen[cookie.Name] = cookie.Value
	}
	gState, ok := seen["g_state"]
	if !ok {
		t.Fatal("g_state was not loaded into the login transport jar")
	}
	if strings.Contains(gState, `"`) {
		t.Fatalf("applyLoginTransportSetCookies kept an unrepresentable value: %q", gState)
	}
}
