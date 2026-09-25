package app

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newAdminStaticTestApp points the WebUI static root at a throwaway tree that
// also holds a sibling directory whose name merely starts with the same text as
// the static root. That sibling is what the old prefix check let through.
func newAdminStaticTestApp(t *testing.T) *App {
	t.Helper()
	root := t.TempDir()
	staticDir := filepath.Join(root, "admin")
	if err := os.MkdirAll(staticDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staticDir, "index.html"), []byte("<html>admin console</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(staticDir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staticDir, "assets", "app.js"), []byte("console.log('admin asset');"), 0o644); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(root, "admin-backup")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sibling, "db.json"), []byte(`{"secret":"should-not-be-served"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// resolveStaticAdminDir honours this override before the configured value.
	t.Setenv("NOTION2API_STATIC_ADMIN_DIR", staticDir)
	return newConversationRequestTestApp(t)
}

// TestAdminStaticFilePathRejectsSiblingPrefixTraversal is the regression for the
// unauthenticated read this route allowed. serveAdminStatic runs before the auth
// check, and the old guard was a bare HasPrefix against the static root:
// "../admin-backup/db.json" cleaned to "static/admin-backup/db.json", which
// still starts with "static/admin", so a neighbouring file was accepted.
//
// This asserts the guard itself rather than the HTTP status, because
// http.ServeFile happens to reject a literal ".." element on its own. Testing
// through the handler would therefore pass even with the guard removed.
func TestAdminStaticFilePathRejectsSiblingPrefixTraversal(t *testing.T) {
	staticDir := filepath.Join("srv", "static", "admin")
	for _, path := range []string{
		"../admin-backup/db.json",
		"./../admin-backup/db.json",
		"assets/../../admin-backup/db.json",
	} {
		if full, ok := adminStaticFilePath(staticDir, path); ok {
			t.Fatalf("adminStaticFilePath(%q) = %q, true; want the path rejected", path, full)
		}
	}
}

// TestAdminStaticFilePathRejectsEscapeOutsideTheRoot covers the ordinary climb
// out of the static root.
func TestAdminStaticFilePathRejectsEscapeOutsideTheRoot(t *testing.T) {
	staticDir := filepath.Join("srv", "static", "admin")
	for _, path := range []string{
		"../../etc/passwd",
		"../../../etc/passwd",
		"assets/../../../../etc/passwd",
	} {
		if full, ok := adminStaticFilePath(staticDir, path); ok {
			t.Fatalf("adminStaticFilePath(%q) = %q, true; want the path rejected", path, full)
		}
	}
}

// TestAdminStaticFilePathKeepsFilesInsideTheRoot is the control case: the
// boundary check must not reject legitimate WebUI assets.
func TestAdminStaticFilePathKeepsFilesInsideTheRoot(t *testing.T) {
	staticDir := filepath.Join("srv", "static", "admin")
	full, ok := adminStaticFilePath(staticDir, "assets/app.js")
	if !ok {
		t.Fatal("a normal asset path was rejected")
	}
	want := filepath.Join(staticDir, "assets", "app.js")
	if full != want {
		t.Fatalf("full = %q, want %q", full, want)
	}
}

// TestAdminStaticServesFilesInsideTheRoot exercises the route end to end. The
// asset path deliberately avoids "index.html", which http.ServeFile answers
// with a redirect.
func TestAdminStaticServesFilesInsideTheRoot(t *testing.T) {
	app := newAdminStaticTestApp(t)
	recorder := httptest.NewRecorder()
	app.handleAdmin(recorder, httptest.NewRequest(http.MethodGet, "/admin/assets/app.js", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "admin asset") {
		t.Fatalf("body = %q, want the served asset", recorder.Body.String())
	}
	if got := recorder.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Fatalf("Cache-Control = %q, want the immutable asset policy", got)
	}
}

// TestAdminStaticServesIndexAtTheRoot keeps the SPA entry point working, since
// the boundary fix sits right next to it.
func TestAdminStaticServesIndexAtTheRoot(t *testing.T) {
	app := newAdminStaticTestApp(t)
	recorder := httptest.NewRecorder()
	app.handleAdmin(recorder, httptest.NewRequest(http.MethodGet, "/admin/", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "admin console") {
		t.Fatalf("body = %q, want index.html", recorder.Body.String())
	}
}

// TestAdminStaticRouteDoesNotServeNeighbouringFiles checks the route as a whole:
// whatever the status code, the sibling file's contents must never come back.
func TestAdminStaticRouteDoesNotServeNeighbouringFiles(t *testing.T) {
	app := newAdminStaticTestApp(t)
	for _, target := range []string{
		"/admin/../admin-backup/db.json",
		"/admin/./../admin-backup/db.json",
		"/admin/../../etc/passwd",
		"/admin/..%2F..%2Fetc%2Fpasswd",
	} {
		recorder := httptest.NewRecorder()
		app.handleAdmin(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404", target, recorder.Code)
		}
		if body := recorder.Body.String(); strings.Contains(body, "should-not-be-served") || strings.Contains(body, "root:") {
			t.Fatalf("%s leaked a file outside the static root: %s", target, body)
		}
	}
}
