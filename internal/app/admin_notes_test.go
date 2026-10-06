package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func noteTestPayload() map[string]any {
	record := func(id, space, kind, title string, children []string) any {
		return map[string]any{"spaceId": space, "value": map[string]any{"role": "reader", "value": map[string]any{
			"id": id, "type": kind, "alive": true, "properties": map[string]any{"title": []any{[]any{title}}}, "content": children,
		}}}
	}
	return map[string]any{"spaceId": "space-primary", "pages": []string{"page", "foreign"}, "recordMap": map[string]any{"block": map[string]any{
		"page":    record("page", "space-primary", "page", "Project notes", []string{"heading", "text", "missing"}),
		"heading": record("heading", "space-primary", "header", "Planning", nil),
		"text":    record("text", "space-primary", "text", "First milestone", nil),
		"foreign": record("foreign", "other-space", "page", "Must not expose", nil),
	}}}
}
func TestNoteReadScopesAndBounds(t *testing.T) {
	raw, _ := json.Marshal(noteTestPayload())
	list, err := parseNoteList(raw, "space-primary")
	if err != nil || len(list) != 1 || list[0].Title != "Project notes" {
		t.Fatalf("bad scoped list: %v", err)
	}
	doc, err := parseNoteDocument(raw, "page", "space-primary")
	if err != nil || !doc.Partial || !strings.Contains(doc.Text, "# Planning") || strings.Contains(doc.Text, "Must not expose") {
		t.Fatalf("bad bounded preview: %v", err)
	}
	if _, err := parseNoteDocument(raw, "foreign", "space-primary"); err == nil {
		t.Fatal("cross-workspace note accepted")
	}
	p := noteTestPayload()
	blocks := mapValue(mapValue(p["recordMap"])["block"])
	blocks["text"] = map[string]any{"spaceId": "space-primary", "value": map[string]any{"role": "reader", "value": map[string]any{
		"type": "text", "alive": true, "properties": map[string]any{"title": []any{[]any{strings.Repeat("文", 20000)}}}, "content": []string{"text"},
	}}}
	raw, _ = json.Marshal(p)
	doc, err = parseNoteDocument(raw, "page", "space-primary")
	if err != nil || !doc.Partial || len([]rune(doc.Text)) > 12020 {
		t.Fatal("preview was not bounded")
	}
}
func TestAdminNotesReadOnlyAndCache(t *testing.T) {
	app := newConversationRequestTestApp(t)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/v3/getUserSharedPagesInSpace" {
			t.Errorf("unexpected upstream operation %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(noteTestPayload())
	}))
	defer server.Close()
	cfg, _, _ := app.State.Snapshot()
	cfg.UpstreamBaseURL, cfg.UpstreamOrigin, cfg.ProxyMode = server.URL, server.URL, proxyModeOff
	if err := app.State.SaveAndApply(cfg); err != nil {
		t.Fatal(err)
	}
	request := func(method, query string, auth bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/admin/notes?email=primary@example.com&workspace_id=space-primary"+query, nil)
		if auth {
			r.Header.Set("X-Admin-Token", "test-admin-token")
		}
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		return w
	}
	if w := request("GET", "", false); w.Code == 200 {
		t.Fatal("unauthenticated read allowed")
	}
	if w := request("PUT", "", true); w.Code != 405 {
		t.Fatalf("write accepted: %d", w.Code)
	}
	for i := 0; i < 3; i++ {
		if w := request("GET", "&refresh=1", true); w.Code != 200 {
			t.Fatalf("read failed: %d", w.Code)
		}
	}
	if calls != 1 {
		t.Fatalf("repeated clicks made %d upstream requests", calls)
	}
	app.State.notesCache["primary@example.com\x00space-primary\x00"] = noteCacheEntry{
		At: time.Now().Add(-time.Minute), RetryUntil: time.Now().Add(10 * time.Minute), Status: 429, Payload: map[string]any{"detail": "cooldown"},
	}
	w := request("GET", "&refresh=1", true)
	if w.Code != 429 || w.Header().Get("Retry-After") == "" || calls != 1 {
		t.Fatal("Retry-After cooldown was not respected")
	}
}
