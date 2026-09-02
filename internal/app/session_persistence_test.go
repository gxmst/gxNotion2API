package app

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestPersistSessionProbePreservesNewerSessionData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probe.json")
	refreshed := probePayload{
		Email:         "user@example.com",
		UserID:        "new-user",
		SpaceID:       "paid-space",
		SpaceViewID:   "paid-view",
		ClientVersion: "new-version",
		Cookies:       []ProbeCookie{{Name: "token_v2", Value: "new-cookie"}},
	}
	if err := writePrivatePrettyJSONFile(path, refreshed); err != nil {
		t.Fatal(err)
	}
	client := &NotionAIClient{Session: SessionInfo{
		ProbePath:     path,
		ClientVersion: "old-version",
		UserID:        "old-user",
		UserEmail:     "user@example.com",
		UserName:      "backfilled name",
		SpaceID:       "free-space",
		SpaceViewID:   "free-view",
		SpaceName:     "backfilled space name",
		Cookies:       []ProbeCookie{{Name: "token_v2", Value: "old-cookie"}},
	}}
	if err := client.persistSessionProbe(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got probePayload
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.UserID != "new-user" || got.SpaceID != "paid-space" || got.SpaceViewID != "paid-view" || got.ClientVersion != "new-version" {
		t.Fatalf("newer session fields were overwritten: %+v", got)
	}
	if len(got.Cookies) != 1 || got.Cookies[0].Value != "new-cookie" {
		t.Fatalf("newer cookies were overwritten: %+v", got.Cookies)
	}
	if got.UserName != "backfilled name" || got.SpaceName != "backfilled space name" {
		t.Fatalf("blank metadata was not backfilled: %+v", got)
	}
}

func TestSessionRetryableErrorDoesNotMatchGenericLoginText(t *testing.T) {
	err := &notionAPIError{StatusCode: http.StatusInternalServerError, Message: "login service session lookup failed"}
	if isSessionRetryableError(err) {
		t.Fatal("generic upstream login/session text must not replay an inference request")
	}
	if !isSessionRetryableError(&notionAPIError{StatusCode: http.StatusUnauthorized}) {
		t.Fatal("401 must remain session retryable")
	}
	if !isSessionRetryableError(&notionAPIError{StatusCode: http.StatusBadRequest, Message: "notion-client-version is stale"}) {
		t.Fatal("explicit client-version failure must remain session retryable")
	}
}

func TestTransientPollingErrorClassification(t *testing.T) {
	for _, status := range []int{http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusBadGateway} {
		if !isTransientPollingError(&notionAPIError{StatusCode: status}) {
			t.Fatalf("status %d should be retried while polling", status)
		}
	}
	if isTransientPollingError(&notionAPIError{StatusCode: http.StatusBadRequest}) {
		t.Fatal("400 should not be retried while polling")
	}
	if isTransientPollingError(errors.New("plain failure")) {
		t.Fatal("plain errors should not be retried while polling")
	}
	if !isTransientPollingError(&net.DNSError{IsTimeout: true}) {
		t.Fatal("network timeout should be retried while polling")
	}
}
