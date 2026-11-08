package http

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"finnri/internal/models"
)

// Only AI work is paid. Budgets, insights and subscription reminders cost
// nothing to run, so a user without a pass gets all of them, and any number
// of budgets.
func TestFreeUserGetsNonAIFeatures(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	authResponse := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{"device_id": "free-non-ai-features"}, http.StatusOK,
	)
	token := authResponse.Token

	_ = performJSONRequest[map[string]any](t, router, http.MethodGet, "/v1/insights?start_date=2026-07-01&end_date=2026-07-31", token, nil, http.StatusOK)
	_ = performJSONRequest[map[string]any](t, router, http.MethodPost, "/v1/subscriptions/reminders", token, nil, http.StatusOK)

	var created []models.Budget
	for i := 0; i < 3; i++ {
		budget := performJSONRequest[models.Budget](t, router, http.MethodPost, "/v1/budgets", token, validBudgetPayload(), http.StatusCreated)
		if budget.ID == 0 {
			t.Fatalf("expected budget %d to be created, got %#v", i+1, budget)
		}
		created = append(created, budget)
	}

	listed := performJSONRequest[[]models.Budget](t, router, http.MethodGet, "/v1/budgets", token, nil, http.StatusOK)
	if len(listed) != len(created) {
		t.Fatalf("expected %d budgets, got %#v", len(created), listed)
	}
	path := fmt.Sprintf("/v1/budgets/%d", created[0].ID)
	_ = performJSONRequest[models.Budget](t, router, http.MethodPut, path, token, validBudgetPayload(), http.StatusOK)
	_ = performJSONRequest[map[string]any](t, router, http.MethodDelete, path, token, nil, http.StatusOK)
}

func TestInlineEntrySplitStaysFree(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	authResponse := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{"device_id": "phase-7-inline-split"}, http.StatusOK,
	)
	accounts := performJSONRequest[[]models.Account](t, router, http.MethodGet, "/v1/accounts", authResponse.Token, nil, http.StatusOK)

	entry := performJSONRequest[models.Entry](
		t, router, http.MethodPost, "/v1/entries", authResponse.Token,
		map[string]any{
			"title":      "Dinner",
			"type":       "expense",
			"amount":     "1000.00",
			"currency":   "INR",
			"source":     "manual",
			"mode":       "Cash",
			"category":   "Food",
			"date":       "2026-07-13",
			"account_id": accounts[0].ID,
			"split": map[string]any{
				"participants": []map[string]any{
					{"friend": map[string]any{"name": "Riya"}, "share_amount": "500.00"},
				},
			},
		},
		http.StatusCreated,
	)
	if entry.ID == 0 {
		t.Fatalf("expected free inline split entry creation, got %#v", entry)
	}
}

func TestManualCoreFeaturesStayFree(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	authResponse := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{"device_id": "phase-7-free-core"}, http.StatusOK,
	)

	accounts := performJSONRequest[[]models.Account](t, router, http.MethodGet, "/v1/accounts", authResponse.Token, nil, http.StatusOK)
	if len(accounts) == 0 {
		t.Fatal("expected free account access")
	}
	entry := performJSONRequest[models.Entry](
		t, router, http.MethodPost, "/v1/entries", authResponse.Token,
		map[string]any{
			"title":      "Chai",
			"type":       "expense",
			"amount":     "80.00",
			"currency":   "INR",
			"source":     "manual",
			"mode":       "Cash",
			"category":   "Food",
			"date":       "2026-07-13",
			"account_id": accounts[0].ID,
		},
		http.StatusCreated,
	)
	if entry.ID == 0 {
		t.Fatalf("expected free manual entry create, got %#v", entry)
	}
	_ = performJSONRequest[DashboardResponse](
		t, router, http.MethodGet, "/v1/dashboard?start_date=2026-07-01&end_date=2026-07-31", authResponse.Token, nil, http.StatusOK,
	)
	group := performJSONRequest[models.SplitGroup](
		t, router, http.MethodPost, "/v1/split/groups", authResponse.Token,
		map[string]any{"name": "Trip", "friend_ids": []uint{}}, http.StatusCreated,
	)
	if group.ID == 0 {
		t.Fatalf("expected free split group create, got %#v", group)
	}
	_ = performJSONRequest[[]models.SplitGroup](
		t, router, http.MethodGet, "/v1/split/groups", authResponse.Token, nil, http.StatusOK,
	)
}

func TestPaidUserCanAccessGatedFeatures(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, token := createPaidBillingTestUserSession(t)

	_ = performJSONRequest[DashboardResponse](
		t, router, http.MethodGet, "/v1/insights?start_date=2026-07-01&end_date=2026-07-31", token, nil, http.StatusOK,
	)
	budget := performJSONRequest[models.Budget](t, router, http.MethodPost, "/v1/budgets", token, validBudgetPayload(), http.StatusCreated)
	if budget.ID == 0 {
		t.Fatalf("expected paid budget create, got %#v", budget)
	}
	_ = performJSONRequest[[]models.Budget](t, router, http.MethodGet, "/v1/budgets", token, nil, http.StatusOK)
	_ = performJSONRequest[[]splitBalance](t, router, http.MethodGet, "/v1/split/balances", token, nil, http.StatusOK)

	nextDue := time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02")
	_ = performJSONRequest[subscriptionResponse](
		t, router, http.MethodPost, "/v1/subscriptions", token,
		map[string]any{
			"name":             "Streamly",
			"amount":           "499.00",
			"currency":         "INR",
			"billing_interval": "monthly",
			"next_due_date":    nextDue,
			"status":           "active",
			"reminder_days":    3,
		},
		http.StatusCreated,
	)
	reminders := performJSONRequest[map[string]any](t, router, http.MethodPost, "/v1/subscriptions/reminders", token, nil, http.StatusOK)
	if _, ok := reminders["created"]; !ok {
		t.Fatalf("expected reminders response, got %#v", reminders)
	}
}

func validBudgetPayload() map[string]any {
	return map[string]any{
		"name":                    "Food",
		"period":                  "monthly",
		"category":                "Food",
		"limit_amount":            "1000.00",
		"currency":                "INR",
		"alert_threshold_percent": 80,
		"active":                  true,
	}
}
