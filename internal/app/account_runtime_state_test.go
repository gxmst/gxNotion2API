package app

import (
	"testing"
	"time"
)

// A success clears the failure counter and the last error. Persisting through
// the plain merge silently restored both, so the counter could only ever grow.
func TestUpsertAccountRuntimeStateClearsFailureBookkeeping(t *testing.T) {
	cfg := AppConfig{Accounts: []NotionAccount{{
		Email:               "a@example.com",
		ProbeJSON:           "probe.json",
		UserID:              "user-1",
		SpaceID:             "space-1",
		ClientVersion:       "1.0",
		Status:              "failed",
		LastError:           "thread did not produce any agent-inference message",
		CooldownUntil:       "2026-08-31T05:00:00Z",
		ConsecutiveFailures: 5,
		TotalFailures:       5,
		TotalSuccesses:      2,
	}}}

	account, _, ok := cfg.FindAccount("a@example.com")
	if !ok {
		t.Fatal("seed account not found")
	}
	account = markAccountDispatchSuccess(account, time.Now())

	stored, index := cfg.UpsertAccountRuntimeState(account)
	if index < 0 {
		t.Fatalf("index = %d, want the existing row", index)
	}
	if stored.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures = %d, want 0", stored.ConsecutiveFailures)
	}
	if stored.LastError != "" {
		t.Errorf("LastError = %q, want empty", stored.LastError)
	}
	if stored.CooldownUntil != "" {
		t.Errorf("CooldownUntil = %q, want empty", stored.CooldownUntil)
	}
	if stored.Status != "ready" {
		t.Errorf("Status = %q, want ready", stored.Status)
	}

	// The stored slice must agree with the returned copy.
	persisted := cfg.Accounts[index]
	if persisted.ConsecutiveFailures != 0 || persisted.LastError != "" {
		t.Errorf("config still holds failures=%d lastError=%q",
			persisted.ConsecutiveFailures, persisted.LastError)
	}

	// Identity fields the caller did not touch must still be intact.
	if persisted.UserID != "user-1" || persisted.SpaceID != "space-1" || persisted.ProbeJSON == "" {
		t.Errorf("identity fields lost: %+v", persisted)
	}
	// Cumulative totals move forward, never back.
	if persisted.TotalSuccesses != 3 {
		t.Errorf("TotalSuccesses = %d, want 3", persisted.TotalSuccesses)
	}
	if persisted.TotalFailures != 5 {
		t.Errorf("TotalFailures = %d, want 5", persisted.TotalFailures)
	}
}

// The plain merge keeps its "absent means unchanged" behaviour, which the admin
// API relies on when it submits a partial edit.
func TestUpsertAccountStillTreatsZeroAsUnchanged(t *testing.T) {
	cfg := AppConfig{Accounts: []NotionAccount{{
		Email:               "b@example.com",
		ProbeJSON:           "probe.json",
		UserID:              "user-2",
		SpaceID:             "space-2",
		ConsecutiveFailures: 4,
		Priority:            7,
		HourlyQuota:         30,
	}}}

	// A partial edit that only renames the workspace.
	stored, _ := cfg.UpsertAccount(NotionAccount{
		Email:     "b@example.com",
		SpaceName: "renamed",
	})
	if stored.SpaceName != "renamed" {
		t.Errorf("SpaceName = %q, want renamed", stored.SpaceName)
	}
	if stored.ConsecutiveFailures != 4 {
		t.Errorf("ConsecutiveFailures = %d, want 4 preserved", stored.ConsecutiveFailures)
	}
	if stored.Priority != 7 || stored.HourlyQuota != 30 || stored.UserID != "user-2" {
		t.Errorf("partial edit clobbered config fields: %+v", stored)
	}
}

// A failure still records itself; the fix must not swallow the increment.
func TestUpsertAccountRuntimeStateKeepsFailureIncrement(t *testing.T) {
	cfg := AppConfig{Accounts: []NotionAccount{{
		Email:               "c@example.com",
		ProbeJSON:           "probe.json",
		ConsecutiveFailures: 1,
		TotalFailures:       1,
	}}}
	account, _, _ := cfg.FindAccount("c@example.com")
	account = markAccountDispatchFailure(account, time.Now(), errSimulatedTransport, true)

	stored, _ := cfg.UpsertAccountRuntimeState(account)
	if stored.ConsecutiveFailures != 2 {
		t.Errorf("ConsecutiveFailures = %d, want 2", stored.ConsecutiveFailures)
	}
	if stored.TotalFailures != 2 {
		t.Errorf("TotalFailures = %d, want 2", stored.TotalFailures)
	}
	if stored.LastError == "" {
		t.Error("LastError should record the failure")
	}
	if stored.Status != "expired" {
		t.Errorf("Status = %q, want expired for a retryable failure", stored.Status)
	}
}
