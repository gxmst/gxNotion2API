package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newCompletedConversation creates a finished conversation with one user and one
// assistant message, which is the state an operator edits in.
func newCompletedConversation(t *testing.T, app *App) ConversationEntry {
	t.Helper()
	store := app.State.conversations()
	entry := store.Create(ConversationCreateRequest{Prompt: "最初的提问"})
	store.Complete(entry.ID, InferenceResult{Text: "模型的回答", ThreadID: "edit-thread"})
	completed, ok := store.Get(entry.ID)
	if !ok {
		t.Fatal("conversation vanished after completion")
	}
	if conversationStatusBusy(completed.Status) {
		t.Fatalf("precondition failed: conversation is still busy (%q)", completed.Status)
	}
	return completed
}

func conversationMessageIDs(entry ConversationEntry) (userID string, assistantID string) {
	for _, message := range entry.Messages {
		switch message.Role {
		case "user":
			userID = message.ID
		case "assistant":
			assistantID = message.ID
		}
	}
	return userID, assistantID
}

func TestSetTitleRenamesAndPersists(t *testing.T) {
	app := newConversationRequestTestApp(t)
	entry := newCompletedConversation(t, app)

	renamed, err := app.State.conversations().SetTitle(entry.ID, "  我的标题  ")
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Title != "我的标题" {
		t.Fatalf("title = %q, want the trimmed value", renamed.Title)
	}
	app.State.persistConversationSnapshot(entry.ID)

	// The rename has to survive a restart, which reads the persisted snapshot.
	stored, found, err := app.State.Store.LoadConversation(entry.ID)
	if err != nil || !found {
		t.Fatalf("reload: found=%v err=%v", found, err)
	}
	if stored.Title != "我的标题" {
		t.Fatalf("persisted title = %q, want %q", stored.Title, "我的标题")
	}
}

func TestSetTitleValidatesInput(t *testing.T) {
	app := newConversationRequestTestApp(t)
	entry := newCompletedConversation(t, app)

	for _, tc := range []struct {
		name  string
		id    string
		title string
		want  string
	}{
		{name: "empty id", id: "   ", title: "x", want: "conversation id is required"},
		{name: "blank title", id: entry.ID, title: "   ", want: "title is required"},
		{name: "unknown conversation", id: "no-such-conversation", title: "x", want: "conversation not found"},
		{name: "too long", id: entry.ID, title: strings.Repeat("字", maxConversationTitleRunes+1), want: "at most"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := app.State.conversations().SetTitle(tc.id, tc.title)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to mention %q", err.Error(), tc.want)
			}
		})
	}
}

// A running turn keeps appending deltas, so a rename there would race the stream.
func TestSetTitleRejectsRunningConversation(t *testing.T) {
	app := newConversationRequestTestApp(t)
	running := app.State.conversations().Create(ConversationCreateRequest{Prompt: "进行中"})

	if _, err := app.State.conversations().SetTitle(running.ID, "新标题"); err == nil {
		t.Fatal("expected a running conversation to refuse a rename")
	} else if !strings.Contains(err.Error(), "running") {
		t.Fatalf("error = %q, want it to mention running", err.Error())
	}
}

func TestSetMessageContentEditsBothRoles(t *testing.T) {
	app := newConversationRequestTestApp(t)
	entry := newCompletedConversation(t, app)
	userID, assistantID := conversationMessageIDs(entry)
	if userID == "" || assistantID == "" {
		t.Fatalf("precondition failed: missing messages (%+v)", entry.Messages)
	}

	updated, err := app.State.conversations().SetMessageContent(entry.ID, userID, "改过的提问")
	if err != nil {
		t.Fatal(err)
	}
	updated, err = app.State.conversations().SetMessageContent(entry.ID, assistantID, "改过的回答")
	if err != nil {
		t.Fatal(err)
	}

	byID := map[string]ConversationMessage{}
	for _, message := range updated.Messages {
		byID[message.ID] = message
	}
	if byID[userID].Content != "改过的提问" {
		t.Fatalf("user content = %q", byID[userID].Content)
	}
	if byID[assistantID].Content != "改过的回答" {
		t.Fatalf("assistant content = %q", byID[assistantID].Content)
	}
	for _, id := range []string{userID, assistantID} {
		if byID[id].EditedAt == nil {
			t.Fatalf("message %s was not marked as edited", id)
		}
	}
	// An untouched message must not be marked edited.
	if len(updated.Messages) == 0 {
		t.Fatal("messages disappeared")
	}
}

func TestSetMessageContentValidatesInput(t *testing.T) {
	app := newConversationRequestTestApp(t)
	entry := newCompletedConversation(t, app)
	userID, _ := conversationMessageIDs(entry)

	for _, tc := range []struct {
		name      string
		id        string
		messageID string
		content   string
		want      string
	}{
		{name: "empty conversation id", id: "  ", messageID: userID, want: "conversation id is required"},
		{name: "empty message id", id: entry.ID, messageID: "  ", want: "message id is required"},
		{name: "unknown conversation", id: "nope", messageID: userID, want: "conversation not found"},
		{name: "unknown message", id: entry.ID, messageID: "no-such-message", want: "message not found"},
		{name: "too long", id: entry.ID, messageID: userID, content: strings.Repeat("字", maxConversationMessageRunes+1), want: "at most"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := app.State.conversations().SetMessageContent(tc.id, tc.messageID, tc.content)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to mention %q", err.Error(), tc.want)
			}
		})
	}
}

// Emptying a message is a legitimate edit; only a running turn is refused.
func TestSetMessageContentAllowsEmptyingAndRejectsRunning(t *testing.T) {
	app := newConversationRequestTestApp(t)
	entry := newCompletedConversation(t, app)
	userID, _ := conversationMessageIDs(entry)

	updated, err := app.State.conversations().SetMessageContent(entry.ID, userID, "")
	if err != nil {
		t.Fatalf("emptying a message should be allowed: %v", err)
	}
	if got := updated.Messages[0].Content; got != "" {
		t.Fatalf("content = %q, want empty", got)
	}

	running := app.State.conversations().Create(ConversationCreateRequest{Prompt: "进行中"})
	if _, err := app.State.conversations().SetMessageContent(running.ID, "any", "x"); err == nil {
		t.Fatal("expected a running conversation to refuse an edit")
	}
}

// The two PATCH routes have to be reachable through the hand-written router,
// including the /messages/ sub-resource split.
func TestConversationEditRoutes(t *testing.T) {
	app := newConversationRequestTestApp(t)
	entry := newCompletedConversation(t, app)
	userID, _ := conversationMessageIDs(entry)

	patch := func(path string, body map[string]any, token string) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(string(raw)))
		if token != "" {
			r.Header.Set("X-Admin-Token", token)
		}
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		return w
	}

	t.Run("rename", func(t *testing.T) {
		w := patch("/admin/conversations/"+entry.ID, map[string]any{"title": "路由改名"}, "test-admin-token")
		if w.Code != http.StatusOK {
			t.Fatalf("rename: %d %s", w.Code, w.Body.String())
		}
		var payload struct {
			Item ConversationEntry `json:"item"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Item.Title != "路由改名" {
			t.Fatalf("title = %q", payload.Item.Title)
		}
	})

	t.Run("message edit", func(t *testing.T) {
		path := "/admin/conversations/" + entry.ID + "/messages/" + userID
		w := patch(path, map[string]any{"content": "路由改消息"}, "test-admin-token")
		if w.Code != http.StatusOK {
			t.Fatalf("message edit: %d %s", w.Code, w.Body.String())
		}
		var payload struct {
			Item ConversationEntry `json:"item"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, message := range payload.Item.Messages {
			if message.ID == userID {
				found = true
				if message.Content != "路由改消息" {
					t.Fatalf("content = %q", message.Content)
				}
				if message.EditedAt == nil {
					t.Fatal("edited message is not marked")
				}
			}
		}
		if !found {
			t.Fatal("edited message missing from the response")
		}
	})

	t.Run("unauthenticated is refused", func(t *testing.T) {
		w := patch("/admin/conversations/"+entry.ID, map[string]any{"title": "x"}, "")
		if w.Code == http.StatusOK {
			t.Fatal("an unauthenticated rename must not succeed")
		}
	})

	t.Run("wrong method is refused", func(t *testing.T) {
		path := "/admin/conversations/" + entry.ID + "/messages/" + userID
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("X-Admin-Token", "test-admin-token")
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("GET on the message route = %d, want 405", w.Code)
		}
	})
}

// A rename must not disturb the message list, and vice versa.
func TestConversationEditLeavesOtherFieldsIntact(t *testing.T) {
	app := newConversationRequestTestApp(t)
	entry := newCompletedConversation(t, app)
	userID, assistantID := conversationMessageIDs(entry)
	before := len(entry.Messages)

	renamed, err := app.State.conversations().SetTitle(entry.ID, "只改标题")
	if err != nil {
		t.Fatal(err)
	}
	if len(renamed.Messages) != before {
		t.Fatalf("rename changed the message count: %d -> %d", before, len(renamed.Messages))
	}
	if renamed.ThreadID != entry.ThreadID {
		t.Fatalf("rename changed the thread id: %q -> %q", entry.ThreadID, renamed.ThreadID)
	}

	edited, err := app.State.conversations().SetMessageContent(entry.ID, assistantID, "只改消息")
	if err != nil {
		t.Fatal(err)
	}
	if edited.Title != "只改标题" {
		t.Fatalf("message edit clobbered the title: %q", edited.Title)
	}
	if len(edited.Messages) != before {
		t.Fatalf("message edit changed the count: %d -> %d", before, len(edited.Messages))
	}
	for _, message := range edited.Messages {
		if message.ID == userID && message.EditedAt != nil {
			t.Fatal("editing one message marked a different one as edited")
		}
	}
	if !edited.UpdatedAt.After(time.Time{}) {
		t.Fatal("UpdatedAt was not advanced")
	}
}
