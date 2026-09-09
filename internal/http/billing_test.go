package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"finnri/internal/billing"
	"finnri/internal/database"
	"finnri/internal/models"
)

func TestBillingPlansExposePublicCatalog(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)

	response := performJSONRequest[struct {
		Plans []billingPlanResponse `json:"plans"`
	}](t, router, http.MethodGet, "/v1/billing/plans", "", nil, http.StatusOK)

	if len(response.Plans) != 5 {
		t.Fatalf("expected default public plan catalog, got %#v", response.Plans)
	}
	monthly := findBillingPlanResponse(response.Plans, "monthly")
	if monthly == nil {
		t.Fatal("expected monthly plan in public catalog")
	}
	if monthly.IncludedCredits != 3600 || monthly.DailyCreditLimit != 250 || monthly.PriceMinor == nil || *monthly.PriceMinor != 14900 {
		t.Fatalf("unexpected monthly limits: %#v", monthly)
	}
	if monthly.CheckoutEnabled {
		t.Fatal("checkout should stay disabled until a payment provider is configured")
	}
	if len(monthly.FeatureGates) == 0 {
		t.Fatal("expected paid feature gates in plan response")
	}
	lifetime := findBillingPlanResponse(response.Plans, "lifetime_quote")
	if lifetime == nil || lifetime.RequiresPriorPaidMonths != 3 || lifetime.CheckoutEnabled {
		t.Fatalf("unexpected lifetime quote plan: %#v", lifetime)
	}
}

func TestGuestCreditAndUsageEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)

	authResponse := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{"device_id": "phase-6-guest-device"}, http.StatusOK,
	)

	credits := performJSONRequest[creditSummaryResponse](t, router, http.MethodGet, "/v1/ai/credits", authResponse.Token, nil, http.StatusOK)
	if credits.TotalCreditsRemaining != billing.GuestTrialCredits || credits.DailyLimit != billing.GuestDailyLimit {
		t.Fatalf("unexpected initial guest credits: %#v", credits)
	}
	if credits.TrialExpiresAt == nil || len(credits.Grants) != 1 {
		t.Fatalf("expected visible trial grant and expiry: %#v", credits)
	}

	form := url.Values{"hint_text": {"coffee 120 cash"}}
	request := httptest.NewRequest(http.MethodPost, "/v1/parse", strings.NewReader(form.Encode()))
	request.Header.Set("Authorization", "Bearer "+authResponse.Token)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("parse status = %d, body = %s", response.Code, response.Body.String())
	}

	credits = performJSONRequest[creditSummaryResponse](t, router, http.MethodGet, "/v1/ai/credits", authResponse.Token, nil, http.StatusOK)
	if credits.TotalCreditsRemaining != billing.GuestTrialCredits-5 || credits.DailyCreditsUsed != 5 || credits.DailyCreditsRemaining != billing.GuestDailyLimit-5 {
		t.Fatalf("unexpected credits after parse: %#v", credits)
	}

	usage := performJSONRequest[aiUsageListResponse](t, router, http.MethodGet, "/v1/ai/usage?page=1&page_size=10", authResponse.Token, nil, http.StatusOK)
	if usage.Total != 1 || len(usage.Events) != 1 {
		t.Fatalf("expected one user-visible AI usage event, got %#v", usage)
	}
	event := usage.Events[0]
	if event.ActionCode != "transaction_parse_text" || event.Status != billing.UsageStatusSucceeded || event.FinalCredits != 5 {
		t.Fatalf("unexpected usage event: %#v", event)
	}

	var raw map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, leaked := raw["prompt"]; leaked {
		t.Fatal("parse response must not expose raw provider prompt")
	}

	if err := database.DB.Model(&models.CreditGrant{}).
		Where("source = ?", billing.GrantSourceFreeTrial).
		Update("credits_remaining", 0).Error; err != nil {
		t.Fatal(err)
	}
	credits = performJSONRequest[creditSummaryResponse](t, router, http.MethodGet, "/v1/ai/credits", authResponse.Token, nil, http.StatusOK)
	if credits.TotalCreditsRemaining != 0 || credits.DailyCreditsRemaining != 0 {
		t.Fatalf("daily credits must not exceed the usable total balance: %#v", credits)
	}
}

func TestBillingCheckoutGuards(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)

	guestAuth := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{"device_id": "checkout-guest-device"}, http.StatusOK,
	)
	guestResponse := performJSONRequest[map[string]string](
		t, router, http.MethodPost, "/v1/billing/checkout", guestAuth.Token, map[string]string{"plan_code": "monthly"}, http.StatusForbidden,
	)
	if guestResponse["error"] != "login_required_for_checkout" {
		t.Fatalf("unexpected guest checkout error: %#v", guestResponse)
	}

	user, token := createBillingTestUserSession(t)
	if user.ID == 0 || token == "" {
		t.Fatal("expected registered test user session")
	}
	lifetimeResponse := performJSONRequest[map[string]string](
		t, router, http.MethodPost, "/v1/billing/checkout", token, map[string]string{"plan_code": "lifetime_quote"}, http.StatusForbidden,
	)
	if lifetimeResponse["error"] != "lifetime_direct_purchase_not_allowed" {
		t.Fatalf("unexpected lifetime checkout error: %#v", lifetimeResponse)
	}
	monthlyResponse := performJSONRequest[map[string]string](
		t, router, http.MethodPost, "/v1/billing/checkout", token, map[string]string{"plan_code": "monthly"}, http.StatusNotImplemented,
	)
	if monthlyResponse["error"] != "payment_provider_not_configured" {
		t.Fatalf("unexpected monthly checkout response: %#v", monthlyResponse)
	}
}

func TestLifetimeQuoteRequiresEligibilityAndStoresUsageSummary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	user, token := createBillingTestUserSession(t)

	notEligible := performJSONRequest[map[string]any](
		t, router, http.MethodPost, "/v1/billing/lifetime-quote/request", token, nil, http.StatusForbidden,
	)
	if notEligible["error"] != "lifetime_quote_not_eligible" {
		t.Fatalf("unexpected not eligible response: %#v", notEligible)
	}

	now := time.Now().UTC()
	plan := models.Plan{
		Code:             "monthly_paid_test",
		Name:             "Monthly Test",
		BillingInterval:  "monthly",
		PriceMinor:       19900,
		Currency:         "INR",
		IncludedCredits:  3000,
		DailyCreditLimit: 200,
		IsPublic:         true,
		RequiresLogin:    true,
	}
	if err := database.DB.Create(&plan).Error; err != nil {
		t.Fatal(err)
	}
	subscription := models.UserSubscription{
		UserID:             user.ID,
		PlanID:             plan.ID,
		Status:             "active",
		CurrentPeriodStart: now.AddDate(0, 0, -100),
		CurrentPeriodEnd:   now.AddDate(0, 0, 1),
		Provider:           "test",
	}
	if err := database.DB.Create(&subscription).Error; err != nil {
		t.Fatal(err)
	}
	usage := models.AIUsageEvent{
		UserID:                 &user.ID,
		RequestID:              "ai_quote_usage_1",
		ActionCode:             "transaction_parse_text",
		InputKind:              "text",
		Status:                 billing.UsageStatusSucceeded,
		EstimatedCredits:       5,
		ReservedCredits:        5,
		FinalCredits:           30,
		EstimatedCostUSDMicros: 900,
		StartedAt:              now.AddDate(0, 0, -10),
	}
	if err := database.DB.Create(&usage).Error; err != nil {
		t.Fatal(err)
	}

	quote := performJSONRequest[models.LifetimeQuoteRequest](
		t, router, http.MethodPost, "/v1/billing/lifetime-quote/request", token, nil, http.StatusCreated,
	)
	if quote.UserID != user.ID || quote.Status != "requested" || quote.PaidMonthsCompleted != 3 {
		t.Fatalf("unexpected quote request: %#v", quote)
	}
	if quote.CreditsUsed != 30 || quote.AverageMonthlyCredits != 10 || quote.EstimatedCostUSDMicros != 900 {
		t.Fatalf("unexpected quote usage summary: %#v", quote)
	}
}

func findBillingPlanResponse(plans []billingPlanResponse, code string) *billingPlanResponse {
	for i := range plans {
		if plans[i].Code == code {
			return &plans[i]
		}
	}
	return nil
}

func createBillingTestUserSession(t *testing.T) (models.User, string) {
	t.Helper()

	user := models.User{UUID: generateUUID(), Username: "phase6_" + generateUUID()[:8], IsGuest: false}
	if err := database.DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	token, _, err := issueSession(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := billing.NewCreditService(database.DB).EnsureLoggedInFreeTrialGrant(user.ID); err != nil {
		t.Fatal(err)
	}
	return user, token
}

func createPaidBillingTestUserSession(t *testing.T) (models.User, string) {
	t.Helper()

	user, token := createBillingTestUserSession(t)
	if err := ensureDefaultCashAccount(user.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	plan := models.Plan{
		Code:             "paid_test_" + generateUUID()[:8],
		Name:             "Paid Test",
		BillingInterval:  "monthly",
		PriceMinor:       19900,
		Currency:         "INR",
		IncludedCredits:  3000,
		DailyCreditLimit: 200,
		IsPublic:         true,
		RequiresLogin:    true,
	}
	if err := database.DB.Create(&plan).Error; err != nil {
		t.Fatal(err)
	}
	subscription := models.UserSubscription{
		UserID:             user.ID,
		PlanID:             plan.ID,
		Status:             "active",
		CurrentPeriodStart: now.Add(-time.Hour),
		CurrentPeriodEnd:   now.AddDate(0, 1, 0),
		Provider:           "test",
	}
	if err := database.DB.Create(&subscription).Error; err != nil {
		t.Fatal(err)
	}
	return user, token
}
