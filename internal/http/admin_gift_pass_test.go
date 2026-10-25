package http

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"finnri/internal/billing"
	"finnri/internal/database"
	"finnri/internal/models"

	"github.com/gin-gonic/gin"
)

type giftPassResponse struct {
	Subscriptions []models.UserSubscription `json:"subscriptions"`
	Created       bool                      `json:"created"`
}

func postGiftPass(t *testing.T, router *gin.Engine, token string, userID uint, idempotencyKey string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/admin/users/"+strconv.Itoa(int(userID))+"/passes", bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func decodeGiftPass(t *testing.T, response *httptest.ResponseRecorder, expectedStatus int) giftPassResponse {
	t.Helper()
	if response.Code != expectedStatus {
		t.Fatalf("gift pass status = %d, want %d, body = %s", response.Code, expectedStatus, response.Body.String())
	}
	var decoded giftPassResponse
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("failed to decode gift pass response: %v; body = %s", err, response.Body.String())
	}
	return decoded
}

func TestAdminGiftPassUnlocksPaidPlanAndGrantsCredits(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	support, supportToken := createAdminSessionForTest(t, models.AdminRoleSupport)
	monthly := seedPurchasablePlan(t, "monthly", "monthly", 14900, 3600)
	user, userToken := createBillingTestUserSession(t)

	locked, err := billing.NewEntitlementService(database.DB).Check(billing.SubjectForUser(user.ID), billing.FeatureBudgets)
	if err != nil || locked.Allowed {
		t.Fatalf("budgets should be locked before the gift: %#v %v", locked, err)
	}

	gift := decodeGiftPass(t, postGiftPass(t, router, supportToken, user.ID, "gift-key-1", map[string]any{
		"plan_code": "monthly",
		"note":      "Family — thanks for testing",
	}), http.StatusCreated)
	if !gift.Created || len(gift.Subscriptions) != 1 {
		t.Fatalf("unexpected gift response: %#v", gift)
	}
	subscription := gift.Subscriptions[0]
	if subscription.Provider != giftPassProvider || subscription.Status != "active" || subscription.PlanID != monthly.ID {
		t.Fatalf("gift should be an active monthly pass marked as a gift: %#v", subscription)
	}
	if length := subscription.CurrentPeriodEnd.Sub(subscription.CurrentPeriodStart); length != 30*24*time.Hour {
		t.Fatalf("monthly gift should run 30 days, got %s", length)
	}

	unlocked, err := billing.NewEntitlementService(database.DB).Check(billing.SubjectForUser(user.ID), billing.FeatureBudgets)
	if err != nil || !unlocked.Allowed {
		t.Fatalf("budgets should unlock with a gifted pass: %#v %v", unlocked, err)
	}

	status := performJSONRequest[billingStatusResponse](t, router, http.MethodGet, "/v1/billing/status", userToken, nil, http.StatusOK)
	if status.SubscriptionStatus != "active" || status.Plan == nil || status.Plan.Code != "monthly" {
		t.Fatalf("billing status should show the gifted pass: %#v", status)
	}

	var grant models.CreditGrant
	if err := database.DB.Where("subscription_id = ?", subscription.ID).First(&grant).Error; err != nil {
		t.Fatalf("gift should grant the plan's credits: %v", err)
	}
	if grant.CreditsGranted != 3600 || grant.ExpiresAt == nil || !grant.ExpiresAt.Equal(subscription.CurrentPeriodEnd) {
		t.Fatalf("unexpected gift credit grant: %#v", grant)
	}

	// A double-click or retry with the same key gives nothing more.
	replay := decodeGiftPass(t, postGiftPass(t, router, supportToken, user.ID, "gift-key-1", map[string]any{
		"plan_code": "monthly",
		"note":      "Family — thanks for testing",
	}), http.StatusOK)
	if replay.Created || len(replay.Subscriptions) != 1 || replay.Subscriptions[0].ID != subscription.ID {
		t.Fatalf("replay should return the original gift: %#v", replay)
	}
	var rows, grants int64
	database.DB.Model(&models.UserSubscription{}).Where("user_id = ?", user.ID).Count(&rows)
	database.DB.Model(&models.CreditGrant{}).Where("subscription_id = ?", subscription.ID).Count(&grants)
	if rows != 1 || grants != 1 {
		t.Fatalf("replay created extra rows: subscriptions=%d grants=%d", rows, grants)
	}

	var audit models.AdminAuditLog
	if err := database.DB.Where("admin_user_id = ? AND action = ?", support.ID, "post_users_passes").Order("created_at ASC").First(&audit).Error; err != nil {
		t.Fatalf("gift should be audited: %v", err)
	}
	if !strings.Contains(audit.Payload, "thanks for testing") || !strings.Contains(audit.Payload, `"plan_code":"monthly"`) {
		t.Fatalf("audit row should carry the plan and note: %s", audit.Payload)
	}
}

func TestAdminGiftPassQueuesBehindRunningPass(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, supportToken := createAdminSessionForTest(t, models.AdminRoleSupport)
	monthly := seedPurchasablePlan(t, "monthly", "monthly", 14900, 3600)
	seedPurchasablePlan(t, "weekly_pass", "weekly", 7900, 800)
	user, _ := createBillingTestUserSession(t)

	now := time.Now().UTC()
	paid := models.UserSubscription{
		UserID: user.ID, PlanID: monthly.ID, Status: "active", Provider: "razorpay",
		CurrentPeriodStart: now.Add(-24 * time.Hour), CurrentPeriodEnd: now.Add(10 * 24 * time.Hour), CancelAtPeriodEnd: true,
	}
	if err := database.DB.Create(&paid).Error; err != nil {
		t.Fatal(err)
	}

	gift := decodeGiftPass(t, postGiftPass(t, router, supportToken, user.ID, "gift-weekly", map[string]any{
		"plan_code": "weekly_pass",
		"periods":   2,
		"note":      "Friend",
	}), http.StatusCreated)
	if len(gift.Subscriptions) != 2 {
		t.Fatalf("expected two weekly periods, got %#v", gift.Subscriptions)
	}
	first, second := gift.Subscriptions[0], gift.Subscriptions[1]
	if !first.CurrentPeriodStart.Equal(paid.CurrentPeriodEnd) {
		t.Fatalf("gift should start when the paid pass ends: %s vs %s", first.CurrentPeriodStart, paid.CurrentPeriodEnd)
	}
	if !second.CurrentPeriodStart.Equal(first.CurrentPeriodEnd) || second.CurrentPeriodEnd.Sub(second.CurrentPeriodStart) != 7*24*time.Hour {
		t.Fatalf("second week should follow the first: %#v", gift.Subscriptions)
	}
	var grants int64
	database.DB.Model(&models.CreditGrant{}).Where("subscription_id IN ? AND credits_granted = ?", []uint{first.ID, second.ID}, 800).Count(&grants)
	if grants != 2 {
		t.Fatalf("each gifted week should carry its own 800 credits, got %d grants", grants)
	}
}

func TestAdminGiftPassValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, viewerToken := createAdminSessionForTest(t, models.AdminRoleViewer)
	_, supportToken := createAdminSessionForTest(t, models.AdminRoleSupport)
	seedPurchasablePlan(t, "monthly", "monthly", 14900, 3600)
	seedPurchasablePlan(t, "lifetime_quote", "lifetime_quote", 499900, 5000)
	user, _ := createBillingTestUserSession(t)
	guest := models.User{UUID: generateUUID(), Username: "guest_" + generateUUID()[:8], IsGuest: true}
	if err := database.DB.Create(&guest).Error; err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		token  string
		userID uint
		body   map[string]any
		status int
		field  string
	}{
		{"viewer cannot gift", viewerToken, user.ID, map[string]any{"plan_code": "monthly", "note": "x"}, http.StatusForbidden, ""},
		{"note required", supportToken, user.ID, map[string]any{"plan_code": "monthly", "note": "  "}, http.StatusUnprocessableEntity, "note"},
		{"note too long", supportToken, user.ID, map[string]any{"plan_code": "monthly", "note": strings.Repeat("a", 201)}, http.StatusUnprocessableEntity, "note"},
		{"too many periods", supportToken, user.ID, map[string]any{"plan_code": "monthly", "periods": 13, "note": "x"}, http.StatusUnprocessableEntity, "periods"},
		{"unknown plan", supportToken, user.ID, map[string]any{"plan_code": "nope", "note": "x"}, http.StatusUnprocessableEntity, "plan_code"},
		{"no fixed period", supportToken, user.ID, map[string]any{"plan_code": "lifetime_quote", "note": "x"}, http.StatusUnprocessableEntity, "plan_code"},
		{"missing user", supportToken, 999999, map[string]any{"plan_code": "monthly", "note": "x"}, http.StatusNotFound, ""},
		{"guest", supportToken, guest.ID, map[string]any{"plan_code": "monthly", "note": "x"}, http.StatusUnprocessableEntity, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := postGiftPass(t, router, tc.token, tc.userID, "", tc.body)
			if response.Code != tc.status {
				t.Fatalf("status = %d, want %d, body = %s", response.Code, tc.status, response.Body.String())
			}
			if tc.field != "" && !strings.Contains(response.Body.String(), `"`+tc.field+`"`) {
				t.Fatalf("expected a %s field error, body = %s", tc.field, response.Body.String())
			}
		})
	}
	var rows int64
	database.DB.Model(&models.UserSubscription{}).Count(&rows)
	if rows != 0 {
		t.Fatalf("rejected gifts should create nothing, found %d subscriptions", rows)
	}
}

func TestGiftedPassesStayOutOfRevenueAndLifetimeEligibility(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, ownerToken := createAdminSessionForTest(t, models.AdminRoleOwner)
	monthly := seedPurchasablePlan(t, "monthly", "monthly", 14900, 3600)
	paidUser, _ := createBillingTestUserSession(t)
	giftedUser, _ := createBillingTestUserSession(t)

	now := time.Now().UTC()
	rows := []models.UserSubscription{
		{UserID: paidUser.ID, PlanID: monthly.ID, Status: "active", Provider: "razorpay", CurrentPeriodStart: now.Add(-time.Hour), CurrentPeriodEnd: now.Add(29 * 24 * time.Hour), CancelAtPeriodEnd: true},
		{UserID: giftedUser.ID, PlanID: monthly.ID, Status: "active", Provider: giftPassProvider, ProviderSubscriptionID: "gift:test:1", CurrentPeriodStart: now.Add(-time.Hour), CurrentPeriodEnd: now.Add(29 * 24 * time.Hour), CancelAtPeriodEnd: true},
		// Four months of gifts already over would clear the lifetime bar if they counted.
		{UserID: giftedUser.ID, PlanID: monthly.ID, Status: "expired", Provider: giftPassProvider, ProviderSubscriptionID: "gift:test:2", CurrentPeriodStart: now.Add(-125 * 24 * time.Hour), CurrentPeriodEnd: now.Add(-2 * time.Hour), CancelAtPeriodEnd: true},
	}
	if err := database.DB.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}

	revenue := performJSONRequest[adminRevenueSnapshot](t, router, http.MethodGet, "/v1/admin/revenue", ownerToken, nil, http.StatusOK)
	if revenue.ActiveSubscribers != 1 || revenue.MRRMinor != 14900 || revenue.GiftedActive != 1 {
		t.Fatalf("revenue should count only the paid pass and report the gift apart: %#v", revenue)
	}

	months, err := paidMonthsCompleted(giftedUser.ID)
	if err != nil {
		t.Fatal(err)
	}
	if months != 0 {
		t.Fatalf("gifted months should not count toward lifetime eligibility, got %d", months)
	}

	detail := performJSONRequest[struct {
		Subscriptions []models.UserSubscription `json:"subscriptions"`
		Economics     struct {
			MonthlyPlanValueMinor int64 `json:"monthly_plan_value_minor"`
		} `json:"economics"`
	}](t, router, http.MethodGet, "/v1/admin/users/"+strconv.Itoa(int(giftedUser.ID)), ownerToken, nil, http.StatusOK)
	if len(detail.Subscriptions) != 2 || detail.Economics.MonthlyPlanValueMinor != 0 {
		t.Fatalf("user detail should list both gifts and value them at zero: %#v", detail)
	}
}
