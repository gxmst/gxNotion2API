package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAdminMissingConversationIsDistinctFromStorageFailure(t *testing.T) {
	a := newConversationRequestTestApp(t)
	for _, storageFailed := range []bool{false, true} {
		if storageFailed {
			if err := a.State.Store.readDB().Close(); err != nil {
				t.Fatal(err)
			}
		}
		req := httptest.NewRequest(http.MethodGet, "/admin/conversations/missing?local=1", nil)
		req.Header.Set("X-Admin-Token", "test-admin-token")
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, req)
		var payload struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if !storageFailed && (rec.Code != http.StatusNotFound || payload.Code != "conversation_not_found") {
			t.Fatalf("missing record is not identifiable: %d %s", rec.Code, rec.Body.String())
		}
		if storageFailed && (rec.Code != http.StatusInternalServerError || payload.Code == "conversation_not_found") {
			t.Fatalf("storage failure would clear saved conversation: %d %s", rec.Code, rec.Body.String())
		}
	}
}
