package http

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"finnri/internal/database"
	"finnri/internal/models"
)

func postJSONWithKey(router *gin.Engine, path, token, key string, payload any) *httptest.ResponseRecorder {
	body, _ := json.Marshal(payload)
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

// The app gives each step of a save 30 s. A recurring create whose answer came
// late is sent again with the same key, and must not make a second item.
func TestRecurringCreateSentAgainWithItsKeyMakesOne(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	user, token := createPaidBillingTestUserSession(t)
	_, otherToken := createPaidBillingTestUserSession(t)
	nextMonth := time.Now().UTC().AddDate(0, 1, 0).Format("2006-01-02")
	netflix := map[string]any{
		"name": "Netflix", "amount": "649.00", "billing_interval": "monthly",
		"next_due_date": nextMonth, "kind": "subscription",
	}
	count := func(userID uint) int64 {
		var n int64
		database.DB.Model(&models.Subscription{}).Where("user_id = ?", userID).Count(&n)
		return n
	}

	first := postJSONWithKey(router, "/v1/subscriptions", token, "day-abc-day-1:recurring", netflix)
	if first.Code != http.StatusCreated {
		t.Fatalf("first create = %d: %s", first.Code, first.Body.String())
	}
	again := postJSONWithKey(router, "/v1/subscriptions", token, "day-abc-day-1:recurring", netflix)
	if again.Code != http.StatusOK || again.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("resend = %d (replayed %q): %s", again.Code, again.Header().Get("Idempotency-Replayed"), again.Body.String())
	}
	var a, b subscriptionResponse
	_ = json.Unmarshal(first.Body.Bytes(), &a)
	_ = json.Unmarshal(again.Body.Bytes(), &b)
	if a.ID == 0 || a.ID != b.ID {
		t.Fatalf("resend answered with %d, want the first item %d", b.ID, a.ID)
	}
	if got := count(user.ID); got != 1 {
		t.Fatalf("%d recurring items after a resend, want 1", got)
	}

	// Another key, or none, is another item, as before.
	if res := postJSONWithKey(router, "/v1/subscriptions", token, "day-abc-day-2:recurring", netflix); res.Code != http.StatusCreated {
		t.Fatalf("a new key = %d, want 201", res.Code)
	}
	if res := postJSONWithKey(router, "/v1/subscriptions", token, "", netflix); res.Code != http.StatusCreated {
		t.Fatalf("no key = %d, want 201", res.Code)
	}
	if got := count(user.ID); got != 3 {
		t.Fatalf("%d recurring items, want 3", got)
	}

	// A key is the user's own: someone else's equal key is not a replay.
	if res := postJSONWithKey(router, "/v1/subscriptions", otherToken, "day-abc-day-1:recurring", netflix); res.Code != http.StatusCreated {
		t.Fatalf("another user's equal key = %d, want 201", res.Code)
	}
}

// Converting to EMI deletes the purchase, so a convert sent again used to find
// no purchase and fail with 422 for good. It now gets the plan already made.
func TestEMIConvertSentAgainGetsThePlanAlreadyMade(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)
	user, token := createPaidBillingTestUserSession(t)
	card := createTestCard(t, user.ID)
	source := createCardSpend(t, user.ID, card.ID, "2099-01-31", "60000.00")
	payload := map[string]any{
		"title": "Phone", "principal": "60000.00", "annual_rate_pct": 0,
		"tenure_months": 12, "purchased_on": "2099-01-31", "source_entry_id": source.ID,
	}
	path := fmt.Sprintf("/v1/accounts/%d/emi-plans", card.ID)

	created := performJSONRequest[cardEMIPlanResponse](t, router, http.MethodPost, path, token, payload, http.StatusCreated)
	again := postJSONWithKey(router, path, token, "", payload)
	if again.Code != http.StatusOK || again.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatalf("resend = %d: %s", again.Code, again.Body.String())
	}
	var replayed cardEMIPlanResponse
	_ = json.Unmarshal(again.Body.Bytes(), &replayed)
	if replayed.ID != created.ID {
		t.Fatalf("resend answered with plan %d, want %d", replayed.ID, created.ID)
	}
	var plans int64
	database.DB.Model(&models.CardEMIPlan{}).Where("user_id = ?", user.ID).Count(&plans)
	if plans != 1 {
		t.Fatalf("%d plans after a resend, want 1", plans)
	}
}
