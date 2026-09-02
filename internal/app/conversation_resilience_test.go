package app

import (
	"testing"
	"time"
)

func TestConversationDeletionClaimBlocksContinuation(t *testing.T) {
	store := newConversationStore()
	entry := store.Create(ConversationCreateRequest{Prompt: "hello"})
	store.Complete(entry.ID, InferenceResult{Text: "answer"})
	claimed, err := store.ClaimForDeletion(entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Status != "deleting" {
		t.Fatalf("status = %q, want deleting", claimed.Status)
	}
	if _, err := store.Continue(entry.ID, ConversationCreateRequest{Prompt: "next"}); err == nil {
		t.Fatal("continuation must be blocked while deletion is in progress")
	}
	store.RestoreDeletionClaim(entry.ID, "completed")
	if _, err := store.Continue(entry.ID, ConversationCreateRequest{Prompt: "next"}); err != nil {
		t.Fatalf("continuation should resume after a failed deletion: %v", err)
	}
}

func TestLoadedRunningConversationIsReconciled(t *testing.T) {
	cfg := defaultConfig()
	cfg.APIKey = "test-key"
	cfg.Storage.SQLitePath = t.TempDir() + "/state.db"
	store, err := openSQLiteStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	entry := ConversationEntry{
		ID:           "conv-running",
		Status:       "running",
		ThreadID:     "thread-created-before-crash",
		AccountEmail: "account@example.com",
		CreatedAt:    time.Now().Add(-time.Minute),
		UpdatedAt:    time.Now().Add(-time.Minute),
	}
	if err := store.SaveConversation(entry); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	state, err := newServerState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	got, ok := state.conversations().Get(entry.ID)
	if !ok {
		t.Fatal("reconciled conversation missing")
	}
	if got.Status != "failed" || got.Error == "" {
		t.Fatalf("conversation was not reconciled: %+v", got)
	}
	if got.ThreadID != entry.ThreadID || got.AccountEmail != entry.AccountEmail {
		t.Fatalf("execution target was lost during reconciliation: %+v", got)
	}
}

func TestPreparePromptExecutionTargetPersistsThreadAndAccount(t *testing.T) {
	state := &ServerState{Conversations: newConversationStore()}
	app := &App{State: state}
	entry := state.conversations().Create(ConversationCreateRequest{Prompt: "hello"})
	request := PromptRunRequest{ConversationID: entry.ID, Prompt: "hello"}

	app.preparePromptExecutionTarget(&request, "first@example.com")
	if request.preparedThreadID == "" {
		t.Fatal("prepared thread ID was not assigned")
	}
	got, ok := state.conversations().Get(entry.ID)
	if !ok {
		t.Fatal("conversation missing")
	}
	if got.ThreadID != request.preparedThreadID || got.AccountEmail != "first@example.com" {
		t.Fatalf("execution target = thread %q account %q", got.ThreadID, got.AccountEmail)
	}

	preparedThreadID := request.preparedThreadID
	app.preparePromptExecutionTarget(&request, "second@example.com")
	got, _ = state.conversations().Get(entry.ID)
	if request.preparedThreadID != preparedThreadID || got.ThreadID != preparedThreadID {
		t.Fatalf("prepared thread changed across dispatch attempts: request=%q conversation=%q", request.preparedThreadID, got.ThreadID)
	}
	if got.AccountEmail != "second@example.com" {
		t.Fatalf("account email = %q, want second@example.com", got.AccountEmail)
	}

	request.onThreadPrepared("thread-returned-by-attachment-upload")
	got, _ = state.conversations().Get(entry.ID)
	if got.ThreadID != "thread-returned-by-attachment-upload" || got.AccountEmail != "second@example.com" {
		t.Fatalf("uploaded attachment execution target was not persisted: %+v", got)
	}
}

func TestSQLiteLoadSkipsCorruptRows(t *testing.T) {
	cfg := defaultConfig()
	cfg.Storage.SQLitePath = t.TempDir() + "/state.db"
	store, err := openSQLiteStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	if err := store.SaveConversation(ConversationEntry{ID: "good", Status: "completed", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO conversations(id, created_at, updated_at, status, data_json) VALUES(?, ?, ?, ?, ?)`, "bad", now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), "completed", "{"); err != nil {
		t.Fatal(err)
	}
	items, err := store.LoadConversations()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != "good" {
		t.Fatalf("loaded conversations = %+v", items)
	}
}
