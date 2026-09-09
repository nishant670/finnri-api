package http

import (
	"net/http"
	"testing"
	"time"

	"finnri/internal/database"
	"finnri/internal/models"
)

func TestRecurringCandidateDecisionSuppressesDashboardCandidate(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)
	authResponse := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{
			"device_id": "recurring-decision-device",
		}, http.StatusOK,
	)
	accounts := performJSONRequest[[]models.Account](
		t, router, http.MethodGet, "/v1/accounts", authResponse.Token, nil, http.StatusOK,
	)
	if len(accounts) == 0 {
		t.Fatal("expected guest account")
	}
	accountID := accounts[0].ID
	amount, err := models.ParseMoney("499")
	if err != nil {
		t.Fatal(err)
	}
	entries := []models.Entry{
		{UserID: authResponse.User.ID, AccountID: &accountID, Amount: amount, Type: "expense", Category: "Entertainment", Merchant: "Streamly", Mode: "UPI", Date: "2026-07-01", Currency: "INR", Source: "manual"},
		{UserID: authResponse.User.ID, AccountID: &accountID, Amount: amount, Type: "expense", Category: "Entertainment", Merchant: "Streamly", Mode: "UPI", Date: "2026-07-08", Currency: "INR", Source: "manual"},
	}
	if err := database.DB.Create(&entries).Error; err != nil {
		t.Fatal(err)
	}

	dashboard := performJSONRequest[DashboardResponse](
		t, router, http.MethodGet,
		"/v1/dashboard?start_date=2026-07-08&end_date=2026-07-14&tz=Asia/Kolkata",
		authResponse.Token, nil, http.StatusOK,
	)
	if len(dashboard.RecurringCandidates) != 1 || dashboard.RecurringCandidates[0].CandidateKey == "" {
		t.Fatalf("expected recurring candidate before decision, got %#v", dashboard.RecurringCandidates)
	}

	decision := performJSONRequest[models.RecurringCandidateDecision](
		t, router, http.MethodPost, "/v1/recurring-candidates/decision", authResponse.Token, map[string]string{
			"candidate_key": dashboard.RecurringCandidates[0].CandidateKey,
			"merchant":      "Streamly",
			"category":      "Entertainment",
			"decision":      "dismissed",
		}, http.StatusOK,
	)
	if decision.Decision != "dismissed" || decision.CandidateKey != dashboard.RecurringCandidates[0].CandidateKey {
		t.Fatalf("unexpected decision response: %#v", decision)
	}

	dashboard = performJSONRequest[DashboardResponse](
		t, router, http.MethodGet,
		"/v1/dashboard?start_date=2026-07-08&end_date=2026-07-14&tz=Asia/Kolkata",
		authResponse.Token, nil, http.StatusOK,
	)
	if len(dashboard.RecurringCandidates) != 0 {
		t.Fatalf("expected dismissed recurring candidate to be suppressed, got %#v", dashboard.RecurringCandidates)
	}
	for _, insight := range dashboard.Insights {
		if insight.Kind == "recurring_candidate" {
			t.Fatalf("expected recurring insight to be suppressed, got %#v", dashboard.Insights)
		}
	}
}

// The card's promise is "track them" with no form, so the endpoint has to
// produce a usable subscription from the candidate alone — and the candidate it
// just tracked must not come back on the next dashboard load.
func TestTrackRecurringCandidatesCreatesSubscriptionsWithoutAForm(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)
	authResponse := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{
			"device_id": "recurring-track-device",
		}, http.StatusOK,
	)
	accounts := performJSONRequest[[]models.Account](
		t, router, http.MethodGet, "/v1/accounts", authResponse.Token, nil, http.StatusOK,
	)
	if len(accounts) == 0 {
		t.Fatal("expected guest account")
	}
	accountID := accounts[0].ID
	amount, err := models.ParseMoney("199")
	if err != nil {
		t.Fatal(err)
	}
	entries := []models.Entry{
		{UserID: authResponse.User.ID, AccountID: &accountID, Amount: amount, Type: "expense", Category: "Entertainment", Merchant: "Streamly", Mode: "UPI", Date: "2026-07-01", Currency: "INR", Source: "manual"},
		{UserID: authResponse.User.ID, AccountID: &accountID, Amount: amount, Type: "expense", Category: "Entertainment", Merchant: "Streamly", Mode: "UPI", Date: "2026-07-08", Currency: "INR", Source: "manual"},
	}
	if err := database.DB.Create(&entries).Error; err != nil {
		t.Fatal(err)
	}

	const window = "start_date=2026-07-08&end_date=2026-07-14&tz=Asia/Kolkata"
	dashboard := performJSONRequest[DashboardResponse](
		t, router, http.MethodGet, "/v1/dashboard?"+window, authResponse.Token, nil, http.StatusOK,
	)
	if len(dashboard.RecurringCandidates) != 1 {
		t.Fatalf("expected one recurring candidate, got %#v", dashboard.RecurringCandidates)
	}
	candidate := dashboard.RecurringCandidates[0]

	tracked := performJSONRequest[trackRecurringCandidatesResponse](
		t, router, http.MethodPost, "/v1/recurring-candidates/track?tz=Asia/Kolkata", authResponse.Token,
		map[string]any{
			"candidate_keys": []string{candidate.CandidateKey},
			"start_date":     "2026-07-08",
			"end_date":       "2026-07-14",
		}, http.StatusOK,
	)
	if len(tracked.Tracked) != 1 || len(tracked.Skipped) != 0 {
		t.Fatalf("expected exactly one tracked candidate, got %#v", tracked)
	}
	created := tracked.Tracked[0].Subscription
	if created.Name != candidate.Label || created.Merchant != candidate.Merchant {
		t.Fatalf("subscription did not carry the candidate identity: %#v", created)
	}
	if created.Amount != amount {
		t.Fatalf("expected amount %s, got %s", amount, created.Amount)
	}
	if created.BillingInterval != subscriptionIntervalWeekly || created.Status != subscriptionStatusActive {
		t.Fatalf("unexpected schedule on created subscription: %#v", created)
	}
	// A candidate detected in the past must not be born overdue.
	if created.NextDueDate < time.Now().Format("2006-01-02") {
		t.Fatalf("expected next due date in the future, got %s", created.NextDueDate)
	}

	subscriptions := performJSONRequest[[]subscriptionResponse](
		t, router, http.MethodGet, "/v1/subscriptions", authResponse.Token, nil, http.StatusOK,
	)
	if len(subscriptions) != 1 {
		t.Fatalf("expected one subscription after tracking, got %d", len(subscriptions))
	}

	dashboard = performJSONRequest[DashboardResponse](
		t, router, http.MethodGet, "/v1/dashboard?"+window, authResponse.Token, nil, http.StatusOK,
	)
	if len(dashboard.RecurringCandidates) != 0 {
		t.Fatalf("expected tracked candidate to stop being offered, got %#v", dashboard.RecurringCandidates)
	}

	// Tracking the same key again is a no-op rather than a duplicate: the
	// candidate is no longer offered, so it can only be reported as skipped.
	retry := performJSONRequest[trackRecurringCandidatesResponse](
		t, router, http.MethodPost, "/v1/recurring-candidates/track?tz=Asia/Kolkata", authResponse.Token,
		map[string]any{
			"candidate_keys": []string{candidate.CandidateKey},
			"start_date":     "2026-07-08",
			"end_date":       "2026-07-14",
		}, http.StatusOK,
	)
	if len(retry.Tracked) != 0 || len(retry.Skipped) != 1 {
		t.Fatalf("expected the repeat call to skip, got %#v", retry)
	}
	subscriptions = performJSONRequest[[]subscriptionResponse](
		t, router, http.MethodGet, "/v1/subscriptions", authResponse.Token, nil, http.StatusOK,
	)
	if len(subscriptions) != 1 {
		t.Fatalf("expected no duplicate subscription, got %d", len(subscriptions))
	}
}

// Every one of these is called by the app with a user session and no static
// bearer. When AUTH_BEARER is configured — it is, in every deployed
// environment — a path missing from the skip list answers 401 before the
// handler is ever reached, which is exactly how the decision endpoint shipped.
func TestRecurringCandidateRoutesSkipTheStaticBearer(t *testing.T) {
	for _, path := range []string{
		"/v1/recurring-candidates/decision",
		"/v1/recurring-candidates/track",
		"/v1/merchants/suggestions",
	} {
		if !skipsStaticBearer(path) {
			t.Errorf("%s requires the static bearer, so the app can never reach it", path)
		}
	}
}
