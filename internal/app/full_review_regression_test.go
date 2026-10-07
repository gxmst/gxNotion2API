package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestAdminMutationStorageFailureIsAtomic(t *testing.T) {
	for _, action := range []string{"rename", "edit", "delete"} {
		t.Run(action, func(t *testing.T) {
			a := newConversationRequestTestApp(t)
			entry := newCompletedConversation(t, a)
			user, _ := conversationMessageIDs(entry)
			a.State.persistConversationSnapshot(entry.ID)
			if _, err := a.State.Store.db.Exec(`CREATE TRIGGER reject_change BEFORE UPDATE ON conversations BEGIN SELECT RAISE(ABORT, 'disk write unavailable'); END`); err != nil {
				t.Fatal(err)
			}
			path := "/admin/conversations/" + entry.ID
			payload := map[string]any{"title": "changed"}
			if action != "rename" {
				path += "/messages/" + user
				payload = map[string]any{"content": "changed"}
			}
			req := conversationTestHTTPRequest(t, path, payload)
			req.Method = http.MethodPatch
			if action == "delete" {
				req.Method = http.MethodDelete
			}
			rec := httptest.NewRecorder()
			a.ServeHTTP(rec, req)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("reported success on failed storage: %d %s", rec.Code, rec.Body.String())
			}
			current, _ := a.State.conversations().Get(entry.ID)
			if !reflect.DeepEqual(current.Messages, entry.Messages) || current.Title != entry.Title || len(current.DeletedMessageIDs) != 0 {
				t.Fatal("failed mutation changed memory")
			}
			stored, found, err := a.State.Store.LoadConversation(entry.ID)
			if err != nil || !found || !reflect.DeepEqual(stored.Messages, entry.Messages) || stored.Title != entry.Title {
				t.Fatal("failed mutation changed persisted data")
			}
		})
	}
}

func TestAdminMutationsRestoreEvictedConversation(t *testing.T) {
	for _, action := range []string{"rename", "edit", "delete"} {
		t.Run(action, func(t *testing.T) {
			a := newConversationRequestTestApp(t)
			entry := newCompletedConversation(t, a)
			user, _ := conversationMessageIDs(entry)
			a.State.persistConversationSnapshot(entry.ID)
			convs := a.State.conversations()
			convs.mu.Lock()
			delete(convs.items, entry.ID)
			convs.mu.Unlock()
			path := "/admin/conversations/" + entry.ID
			payload := map[string]any{"title": "changed"}
			if action != "rename" {
				path += "/messages/" + user
				payload = map[string]any{"content": "changed"}
			}
			req := conversationTestHTTPRequest(t, path, payload)
			req.Method = http.MethodPatch
			if action == "delete" {
				req.Method = http.MethodDelete
			}
			rec := httptest.NewRecorder()
			a.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("evicted mutation: %d %s", rec.Code, rec.Body.String())
			}
			stored, found, err := a.State.Store.LoadConversation(entry.ID)
			if err != nil || !found {
				t.Fatal("mutation not persisted")
			}
			if action == "rename" && stored.Title != "changed" || action == "edit" && stored.Messages[0].Content != "changed" || action == "delete" && len(stored.Messages) != 1 {
				t.Fatalf("incorrect stored result: %+v", stored)
			}
			if action == "edit" {
				// Editing the newest visible body must also update the sidebar preview.
				_, answer := conversationMessageIDs(stored)
				if _, err := convs.SetMessageContent(entry.ID, answer, "new preview"); err != nil {
					t.Fatal(err)
				}
				if convs.List()[0].Preview != "new preview" {
					t.Fatal("sidebar kept the pre-edit preview")
				}
			}
		})
	}
}

func TestTranscriptReadersAcceptBothRecordFormats(t *testing.T) {
	for _, double := range []bool{false, true} {
		wrap := func(value map[string]any) map[string]any {
			out := map[string]any{"value": value}
			if double {
				out = map[string]any{"value": out}
			}
			return out
		}
		user := wrap(map[string]any{"created_time": float64(1700000000000), "step": map[string]any{"type": "user", "value": []any{[]any{"question"}}}})
		message, ok := extractConversationMessageFromThreadRecord("user", user)
		if !ok || message.Content != "question" || message.CreatedAt.IsZero() {
			t.Fatalf("double=%v missing history: %+v", double, message)
		}
		records := map[string]any{"thread": map[string]any{"thread": wrap(map[string]any{"file_ids": []any{"file-1"}})}, "thread_message": map[string]any{
			"answer": wrap(map[string]any{"step": map[string]any{"type": "agent-inference", "value": []any{}}, "data": map[string]any{"completed": true}}),
			"error":  wrap(map[string]any{"step": map[string]any{"type": "error", "message": "failed"}}),
		}}
		body, _ := json.Marshal(map[string]any{"recordMap": records})
		var decoded map[string]any
		_ = json.Unmarshal(body, &decoded)
		if got := threadFileIDsFromThreadRecord(decoded, "thread"); len(got) != 1 || got[0] != "file-1" {
			t.Fatal("attachment identity lost")
		}
		if !extractAgentMessages(records)["answer"].Completed {
			t.Fatal("completed answer lost")
		}
		if extractThreadErrors(records, "thread")["error"].Message != "failed" {
			t.Fatal("upstream error lost")
		}
	}
}

func TestImportedConversationDeleteUsesLocalOwnershipAndCleanup(t *testing.T) {
	a := newConversationRequestTestApp(t)
	remoteCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remoteCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	cfg, _, _ := a.State.Snapshot()
	cfg.UpstreamBaseURL = server.URL
	cfg.UpstreamOrigin = server.URL
	if err := a.State.SaveAndApply(cfg); err != nil {
		t.Fatal(err)
	}
	entry := ConversationEntry{ID: "notion_thread:imported", ThreadID: "imported", Status: "completed", AccountEmail: "removed-owner@example.com", SpaceID: "removed-space"}
	a.State.conversations().ImportRemote(entry)
	a.State.persistConversationSnapshot(entry.ID)
	if err := a.deleteAdminConversationByID(httptest.NewRequest(http.MethodDelete, "/", nil), entry.ID); err != nil {
		t.Fatal(err)
	}
	if remoteCalls != 0 {
		t.Fatal("deletion used the active account instead of the recorded owner")
	}
	if _, found, err := a.State.loadConversation(entry.ID); found || err != nil {
		t.Fatal("imported conversation remained after deletion")
	}
}

func TestRemoteMergePreservesUnfinishedLocalTurn(t *testing.T) {
	for _, status := range []string{"running", "failed"} {
		local := ConversationEntry{Status: status, Messages: []ConversationMessage{{ID: "old", Role: "assistant", Content: "old reply"}, {ID: "partial", Role: "assistant", Content: "partial reply", Status: "failed"}}}
		remote := ConversationEntry{Messages: local.Messages[:1]}
		if got := mergeConversationEntry(local, remote); len(got.Messages) != 2 || got.Messages[1].Content != "partial reply" {
			t.Fatal("upstream sync erased the unfinished local answer")
		}
	}
}
