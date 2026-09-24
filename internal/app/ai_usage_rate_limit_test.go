package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The payload is copied verbatim from a captured getCreditRateLimitStatus
// response so the parser is pinned to the real field names.
const creditRateLimitSample = `{"status":"within_limit","window":{"creditType":"basic_ai_credits","scope":"per_user","window":"6h","used":25,"limit":100},"billingPeriodWindow":{"creditType":"basic_ai_credits","scope":"per_user","cadence":"billing_period","used":12.82,"limit":100,"periodEndMs":1790096400000}}`

func TestParseCreditRateLimitStatus(t *testing.T) {
	got, err := parseCreditRateLimitStatus([]byte(creditRateLimitSample))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "within_limit" {
		t.Fatalf("status = %q", got.Status)
	}
	if got.Short == nil || got.Short.Label != "6h" || got.Short.Used != 25 || got.Short.Limit != 100 {
		t.Fatalf("short window = %+v", got.Short)
	}
	if got.Short.CreditType != "basic_ai_credits" || got.Short.Scope != "per_user" {
		t.Fatalf("short window metadata = %+v", got.Short)
	}
	if got.Long == nil || got.Long.Cadence != "billing_period" || got.Long.Used != 12.82 || got.Long.Limit != 100 {
		t.Fatalf("long window = %+v", got.Long)
	}
	if got.Long.PeriodEndMs != 1790096400000 {
		t.Fatalf("long window period end = %d", got.Long.PeriodEndMs)
	}

	// A response that names neither window is a decode failure, not an empty
	// success: reporting "no quota" would be a claim the payload never made.
	if _, err := parseCreditRateLimitStatus([]byte(`{"status":"within_limit"}`)); err == nil {
		t.Fatal("payload without any window was accepted")
	}
	// Either window alone is still usable, so a plan that only reports one
	// must not fail the whole fetch.
	only, err := parseCreditRateLimitStatus([]byte(`{"window":{"window":"5h","used":1,"limit":4}}`))
	if err != nil {
		t.Fatal(err)
	}
	if only.Short == nil || only.Short.Label != "5h" || only.Long != nil {
		t.Fatalf("single-window payload = %+v", only)
	}
}

func adminWorkspaceAIUsageRequest(query string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/admin/accounts/ai-usage/workspace"+query, nil)
	r.Header.Set("X-Admin-Token", "test-admin-token")
	return r
}

func TestAdminWorkspaceAIUsageScopesToOneWorkspace(t *testing.T) {
	app := newConversationRequestTestApp(t)
	app.workspaceAIUsageFetchOverride = func(_ context.Context, _ AppConfig, account NotionAccount) (workspaceAIUsage, error) {
		return workspaceAIUsage{
			SpaceID:         account.SpaceID,
			IsEligible:      true,
			IsEligibleKnown: true,
			RateLimit: &workspaceRateLimit{
				Status: "within_limit",
				Short:  &workspaceRateLimitWindow{Label: "6h", Used: 40, Limit: 100},
				Long:   &workspaceRateLimitWindow{Label: "billing_period", Cadence: "billing_period", Used: 10, Limit: 100, PeriodEndMs: 1790096400000},
			},
		}, nil
	}

	rec := httptest.NewRecorder()
	app.ServeHTTP(rec, adminWorkspaceAIUsageRequest("?email=primary@example.com&workspace_id=space-primary"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Report workspaceAIUsageReport `json:"report"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Report.Status != "ok" || payload.Report.SpaceID != "space-primary" {
		t.Fatalf("report = %+v", payload.Report)
	}
	if payload.Report.Usage == nil || payload.Report.Usage.RateLimit == nil {
		t.Fatalf("rate limit missing from report: %+v", payload.Report.Usage)
	}
	if short := payload.Report.Usage.RateLimit.Short; short == nil || short.Used != 40 || short.Limit != 100 {
		t.Fatalf("short window = %+v", short)
	}

	// An explicit workspace that this account does not own is a 404 rather than
	// a report for whichever workspace happened to be selected instead.
	rec = httptest.NewRecorder()
	app.ServeHTTP(rec, adminWorkspaceAIUsageRequest("?email=primary@example.com&workspace_id=space-missing"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown workspace status = %d body=%s", rec.Code, rec.Body.String())
	}

	// The endpoint is admin-only like the rest of the accounts surface.
	rec = httptest.NewRecorder()
	app.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/accounts/ai-usage/workspace?email=primary@example.com", nil))
	if rec.Code == http.StatusOK {
		t.Fatalf("unauthenticated request was served: %s", rec.Body.String())
	}
}
