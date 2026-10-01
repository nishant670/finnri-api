package http

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/xeipuuv/gojsonschema"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"finnri/internal/billing"
	"finnri/internal/config"
	"finnri/internal/database"
	"finnri/internal/models"
)

func TestGuestCaptureParseConfirmSaveDashboardSmoke(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)

	router := smokeRouter(t)

	authResponse := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{
			"device_id": "smoke-test-device",
		}, http.StatusOK,
	)
	if !strings.HasPrefix(authResponse.Token, "fnr_") {
		t.Fatalf("expected opaque guest session token, got %q", authResponse.Token)
	}

	accounts := performJSONRequest[[]models.Account](
		t, router, http.MethodGet, "/v1/accounts", authResponse.Token, nil, http.StatusOK,
	)
	if len(accounts) != 1 || !accounts[0].IsDefault || accounts[0].Name != "Cash" {
		t.Fatalf("expected guest default Cash account, got %#v", accounts)
	}
	accountID := accounts[0].ID

	form := url.Values{"hint_text": {"chai 80 cash"}}
	parseRequest := httptest.NewRequest(http.MethodPost, "/v1/parse", strings.NewReader(form.Encode()))
	parseRequest.Header.Set("Authorization", "Bearer "+authResponse.Token)
	parseRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	parseResponse := httptest.NewRecorder()
	router.ServeHTTP(parseResponse, parseRequest)
	if parseResponse.Code != http.StatusOK {
		t.Fatalf("parse status = %d, body = %s", parseResponse.Code, parseResponse.Body.String())
	}

	var draft map[string]any
	if err := json.Unmarshal(parseResponse.Body.Bytes(), &draft); err != nil {
		t.Fatalf("failed to decode parse response: %v", err)
	}
	if draft["stage"] != "draft" || draft["source_text"] != "chai 80 cash" {
		t.Fatalf("unexpected parse draft: %#v", draft)
	}
	if _, persisted := draft["id"]; persisted {
		t.Fatalf("parse response should be an unpersisted draft: %#v", draft)
	}

	confirmed := map[string]any{
		"title":       draft["title"],
		"type":        draft["type"],
		"amount":      draft["amount"],
		"currency":    "INR",
		"source":      "text",
		"account_id":  accountID,
		"mode":        draft["mode"],
		"category":    draft["category"],
		"merchant":    draft["merchant"],
		"date":        draft["date"],
		"time":        "09:15",
		"source_text": draft["source_text"],
	}
	savedEntry := performJSONRequest[models.Entry](
		t, router, http.MethodPost, "/v1/entries", authResponse.Token, confirmed, http.StatusCreated,
	)
	if savedEntry.ID == 0 || savedEntry.AccountID == nil || *savedEntry.AccountID != accountID {
		t.Fatalf("saved entry did not retain the confirmed account: %#v", savedEntry)
	}

	dashboard := performJSONRequest[DashboardResponse](
		t, router, http.MethodGet,
		"/v1/dashboard?start_date=2026-07-12&end_date=2026-07-12&tz=Asia/Kolkata",
		authResponse.Token, nil, http.StatusOK,
	)
	if dashboard.Summary.TransactionCount != 1 || dashboard.Summary.TotalSpent != 80 {
		t.Fatalf("dashboard summary did not include saved entry: %#v", dashboard.Summary)
	}
	if len(dashboard.RecentTransactions) != 1 || dashboard.RecentTransactions[0].ID != savedEntry.ID {
		t.Fatalf("dashboard recent transactions did not include saved entry: %#v", dashboard.RecentTransactions)
	}
	if len(dashboard.TopCategories) != 1 || dashboard.TopCategories[0].Category != "Food & Drinks" {
		t.Fatalf("dashboard category rollup did not include parsed category: %#v", dashboard.TopCategories)
	}
}

// smokeRouter builds the full test router. The variadic configure hooks run
// after the Server is built and before any route is registered, which is how a
// test swaps in a stub mail sender or flips an OTP config flag.
func smokeRouter(t *testing.T, configure ...func(*Server, *config.Config)) *gin.Engine {
	t.Helper()

	schemaPath := filepath.Join(projectRoot(t), "schemas", "expense_entry.schema.json")
	schema, err := gojsonschema.NewSchema(gojsonschema.NewReferenceLoader("file://" + schemaPath))
	if err != nil {
		t.Fatalf("failed to load parse schema: %v", err)
	}

	cfg := &config.Config{
		TZDefault:             "Asia/Kolkata",
		AuthBearer:            "admin-test-token",
		ReqTimeoutSec:         2,
		RateLimitRPS:          1000,
		RateLimitBurst:        1000,
		MaxJSONKB:             64,
		MaxUploadMB:           1,
		MaxTranscriptChars:    1000,
		GoogleClientIDs:       []string{"test-google-client"},
		WebBaseURL:            "https://finnri.example",
		WebhookRateLimitRPS:   1000,
		WebhookRateLimitBurst: 1000,
	}
	server := &Server{
		cfg:       cfg,
		validator: schema,
		parser: fixtureParser{result: []byte(`{
			"title":"Chai",
			"amount":80,
			"type":"expense",
			"currency":"INR",
			"mode":"Cash",
			"category":"Food & Drinks",
			"merchant":"Tea Stall",
			"date":"2026-07-12"
		}`)},
	}

	for _, apply := range configure {
		apply(server, cfg)
	}

	router := gin.New()
	auth := router.Group("/v1/auth")
	auth.Use(jsonRequestLimits(cfg), rateLimit(cfg, "auth"))
	auth.POST("/guest", server.authGuest)
	auth.POST("/identify", server.authIdentify)
	auth.POST("/otp/send", server.authOtpSend)
	auth.POST("/otp/verify", server.authOtpVerify)
	auth.POST("/register", server.authRegister)
	auth.POST("/login", server.authLogin)
	auth.POST("/admin/login", server.adminLogin)
	auth.POST("/admin/logout", server.adminLogout)
	auth.POST("/google", server.authGoogle)
	auth.POST("/pin/reset", server.authPinReset)

	billingPublic := router.Group("/v1/billing")
	billingPublic.Use(jsonRequestLimits(cfg), rateLimit(cfg, "billing"))
	billingPublic.GET("/plans", server.listBillingPlans)
	billingPublic.GET("/checkout/:order_id", server.getBillingCheckoutOrder)
	billingPublic.POST("/webhook", server.handleBillingWebhook)
	router.GET("/v1/split/invites/:token/preview", server.previewSplitGroupInvite)

	admin := router.Group("/v1/admin")
	admin.Use(jsonRequestLimits(cfg), rateLimit(cfg, "admin"), server.requireAdminSession(), server.adminAuditMiddleware())
	admin.GET("/me", server.getAdminMe)
	admin.GET("/overview", server.getAdminOverview)
	admin.GET("/users", server.listAdminUsers)
	admin.GET("/users/:id", server.getAdminUser)
	admin.GET("/users/:id/ai-usage", server.listAdminUserAIUsage)
	admin.GET("/users/:id/credits", server.getAdminUserCredits)
	admin.GET("/users/:id/activity", server.getAdminUserActivity)
	admin.POST("/users/:id/credits/adjustments", requireAdminRole(models.AdminRoleSupport), server.createUserCreditAdjustment)
	admin.GET("/ai/metrics", server.getAIMetrics)
	admin.GET("/ai/metrics/timeseries", server.getAIMetricsTimeseries)
	admin.GET("/ai/usage", server.listAdminAIUsage)
	admin.GET("/ai/limit-events", server.listAdminAILimitEvents)
	admin.GET("/ai/model-pricing", server.listAIModelPricing)
	admin.PUT("/ai/model-pricing", requireAdminRole(models.AdminRoleOwner), server.upsertAIModelPricing)
	admin.POST("/credits/adjustments", requireAdminRole(models.AdminRoleSupport), server.createCreditAdjustment)
	admin.GET("/credits/summary", server.getAdminCreditsSummary)
	admin.GET("/credits/ledger", server.listAdminCreditLedger)
	admin.GET("/subscriptions", server.listAdminSubscriptions)
	admin.GET("/revenue", server.getAdminRevenue)
	admin.GET("/plans", server.listAdminPlans)
	admin.PUT("/plans/:code", requireAdminRole(models.AdminRoleOwner), server.updateAdminPlan)
	admin.GET("/billing/lifetime-quotes", server.listLifetimeQuoteRequests)
	admin.PATCH("/billing/lifetime-quotes/:id", requireAdminRole(models.AdminRoleSupport), server.updateLifetimeQuoteRequest)
	admin.GET("/ai/abuse-blocks", server.listAIAbuseBlocks)
	admin.POST("/ai/abuse-blocks", requireAdminRole(models.AdminRoleSupport), server.createAIAbuseBlock)
	admin.PATCH("/ai/abuse-blocks/:id", requireAdminRole(models.AdminRoleSupport), server.updateAIAbuseBlock)
	admin.GET("/feedback", server.listAdminFeedback)
	admin.GET("/feedback/stats", server.getAdminFeedbackStats)
	admin.PATCH("/feedback/:id", requireAdminRole(models.AdminRoleSupport), server.updateAdminFeedback)
	admin.GET("/analytics/signups", server.getAdminSignups)
	admin.GET("/analytics/activation", server.getAdminActivation)
	admin.GET("/analytics/retention", server.getAdminRetention)
	admin.GET("/analytics/engagement", server.getAdminEngagement)
	admin.GET("/analytics/feature-adoption", server.getAdminFeatureAdoption)
	admin.GET("/audit-log", server.listAdminAuditLog)
	admin.GET("/health", server.getAdminHealth)
	admin.GET("/export/:resource", server.exportAdminCSV)
	admin.GET("/admin-users", requireAdminRole(models.AdminRoleOwner), server.listAdminIdentities)
	admin.POST("/admin-users", requireAdminRole(models.AdminRoleOwner), server.createAdminIdentity)
	admin.PATCH("/admin-users/:id", requireAdminRole(models.AdminRoleOwner), server.updateAdminIdentity)

	authorized := router.Group("/v1")
	authorized.Use(AuthMiddleware())
	authorized.POST("/parse", uploadRequestLimits(cfg), rateLimit(cfg, "ai"), server.handleParse)
	authorized.GET("/billing/status", server.getBillingStatus)
	authorized.POST("/billing/checkout", server.createBillingCheckout)
	authorized.POST("/billing/lifetime-quote/request", server.requestLifetimeQuote)
	authorized.GET("/ai/usage", server.listAIUsage)
	authorized.GET("/ai/credits", server.getAICredits)
	authorized.GET("/accounts", server.listAccounts)
	authorized.POST("/accounts/:id/paid-off", server.markCardPaidOff)
	budgets := authorized.Group("/budgets", server.requireEntitlement(billing.FeatureBudgets))
	budgets.POST("", server.createBudget)
	budgets.GET("", server.listBudgets)
	budgets.PUT("/:id", server.updateBudget)
	budgets.DELETE("/:id", server.deleteBudget)
	authorized.POST("/subscriptions", server.createSubscription)
	authorized.GET("/subscriptions", server.listSubscriptions)
	authorized.POST("/subscriptions/reminders", server.requireEntitlement(billing.FeatureSubscriptionReminders), server.createSubscriptionReminders)
	authorized.POST("/feedback", server.createFeedback)
	authorized.POST("/auth/logout", server.authLogout)
	authorized.POST("/auth/sessions/revoke-all", server.authRevokeAllSessions)
	authorized.POST("/entries", server.saveEntry)
	// The list route was missing here, which is why nothing covered listEntries
	// and why the mode whitelist could disagree with the save validation for as
	// long as it did.
	authorized.GET("/entries", server.listEntries)
	authorized.GET("/entries/export", server.exportEntriesCSV)
	authorized.GET("/merchants/suggestions", server.listMerchantSuggestions)
	authorized.GET("/categories", server.listCategories)
	authorized.GET("/reports/transactions/summary", server.getTransactionSummaryReport)
	authorized.GET("/entries/:id", server.getEntry)
	authorized.PUT("/entries/:id", server.updateEntry)
	authorized.DELETE("/entries/:id", server.deleteEntry)
	authorized.GET("/notifications", server.listNotifications)
	authorized.GET("/notifications/unread-count", server.getUnreadNotificationCount)
	authorized.GET("/dashboard", server.getDashboard)
	authorized.GET("/insights", server.requireEntitlement(billing.FeatureAdvancedInsights), server.getInsights)
	authorized.POST("/recurring-candidates/decision", server.saveRecurringCandidateDecision)
	authorized.POST("/recurring-candidates/track", server.trackRecurringCandidates)
	// Mirrors handlers.go: the invite endpoints are deliberately outside the
	// split entitlement, so the smoke router has to keep them outside it too.
	invites := authorized.Group("/split")
	invites.GET("/pending-invites", server.listPendingSplitGroupInvites)
	invites.GET("/invites/:token", server.getSplitGroupInvite)
	invites.POST("/invites/:token/accept", server.acceptSplitGroupInvite)
	split := authorized.Group("/split", server.requireEntitlement(billing.FeatureSplitLedger))
	split.POST("/friends", server.createSplitFriend)
	split.GET("/friends", server.listSplitFriends)
	split.PUT("/friends/:id", server.updateSplitFriend)
	split.POST("/friends/:id/merge-into/:target", server.mergeSplitFriend)
	split.DELETE("/friends/:id", server.archiveSplitFriend)
	split.POST("/groups", server.createSplitGroup)
	split.GET("/groups", server.listSplitGroups)
	split.PUT("/groups/:id", server.updateSplitGroup)
	split.PUT("/groups/:id/default-split", server.updateSplitGroupDefaultSplit)
	split.DELETE("/groups/:id", server.archiveSplitGroup)
	split.POST("/groups/:id/invite-link", server.createSplitGroupInvite)
	split.GET("/groups/:id/invites", server.listSplitGroupDirectInvites)
	split.POST("/groups/:id/invites", server.createSplitGroupDirectInvite)
	split.DELETE("/groups/:id/invites/:invite_id", server.revokeSplitGroupDirectInvite)
	split.POST("/groups/:id/leave", server.leaveSplitGroup)
	split.POST("/bills", server.createSplitBill)
	split.GET("/bills", server.listSplitBills)
	split.GET("/bills/by-entry/:entry_id", server.getSplitBillByEntry)
	split.PUT("/bills/:id", server.updateSplitBill)
	split.DELETE("/bills/:id", server.deleteSplitBill)
	split.POST("/settlements", server.createSplitSettlement)
	split.GET("/settlements", server.listSplitSettlements)
	split.GET("/settlements/pending", server.listPendingSplitSettlements)
	split.POST("/settlements/:id/confirm", server.confirmSplitSettlement)
	split.POST("/settlements/:id/deny", server.denySplitSettlement)
	split.GET("/activity", server.listSplitActivity)
	split.GET("/balances", server.listSplitBalances)
	authorized.POST("/tools/emi/calculate", server.calculateEMI)
	authorized.POST("/accounts/:id/emi-plans", server.createCardEMIPlan)
	authorized.GET("/refundables", server.listRefundables)
	authorized.PATCH("/refundables/:id", server.updateRefundStatus)
	return router
}

func useSmokeDatabase(t *testing.T) {
	t.Helper()

	previous := database.DB
	db, err := gorm.Open(sqlite.Open("file:finnri_smoke?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open smoke database: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("failed to access smoke database handle: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)

	if err := db.AutoMigrate(
		&models.User{},
		&models.AuthSession{},
		&models.AuthVerification{},
		&models.AdminUser{},
		&models.AdminAuditLog{},
		&models.AdminDailyMetric{},
		&models.Plan{},
		&models.UserSubscription{},
		&models.CreditGrant{},
		&models.AIUsageEvent{},
		&models.CreditLedger{},
		&models.DailyCreditUsage{},
		&models.GuestUsageKey{},
		&models.LifetimeQuoteRequest{},
		&models.AIUsageLimitEvent{},
		&models.AIModelPricing{},
		&models.AIAbuseBlock{},
		&models.Account{},
		&models.Entry{},
		&models.QuickPrompt{},
		&models.Notification{},
		&models.Feedback{},
		&models.Budget{},
		&models.BudgetAlert{},
		&models.MonthlyReview{},
		&models.Subscription{},
		&models.SubscriptionReminder{},
		&models.SubscriptionOccurrence{},
		&models.CardStatement{},
		&models.CardStatementPayment{},
		&models.CardStatementReminder{},
		&models.CardEMIPlan{},
		&models.CardEMIInstallment{},
		&models.RecurringCandidateDecision{},
		&models.PushDevice{},
		&models.SplitFriend{},
		&models.SplitGroup{},
		&models.SplitGroupMember{},
		&models.SplitGroupInvite{},
		&models.SplitGroupDirectInvite{},
		&models.SplitGroupUserMember{},
		&models.SplitGroupMemberLink{},
		&models.SplitBill{},
		&models.SplitParticipant{},
		&models.SplitSettlement{},
		&models.SplitFriendMerge{},
		&models.Payment{},
		&models.PaymentWebhookEvent{},
	); err != nil {
		t.Fatalf("failed to migrate smoke database: %v", err)
	}

	database.DB = db
	t.Cleanup(func() {
		database.DB = previous
		if err := sqlDB.Close(); err != nil {
			t.Fatalf("failed to close smoke database: %v", err)
		}
	})
}

func performJSONRequest[T any](
	t *testing.T,
	router *gin.Engine,
	method string,
	target string,
	token string,
	body any,
	expectedStatus int,
) T {
	t.Helper()

	var requestBody *bytes.Reader
	if body == nil {
		requestBody = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("failed to encode request body: %v", err)
		}
		requestBody = bytes.NewReader(encoded)
	}

	request := httptest.NewRequest(method, target, requestBody)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != expectedStatus {
		t.Fatalf("%s %s status = %d, body = %s", method, target, response.Code, response.Body.String())
	}

	var decoded T
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("failed to decode %s %s response: %v; body = %s", method, target, err, response.Body.String())
	}
	return decoded
}
