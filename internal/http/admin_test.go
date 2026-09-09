package http

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"finnri/internal/billing"
	"finnri/internal/config"
	"finnri/internal/database"
	"finnri/internal/models"
)

func createAdminSessionForTest(t *testing.T, role string) (models.User, string) {
	t.Helper()
	email := "admin-" + strings.ReplaceAll(role, "_", "-") + "@example.com"
	pinHash, err := hashOptionalPIN("2468")
	if err != nil {
		t.Fatal(err)
	}
	user := models.User{UUID: generateUUID(), Email: &email, Username: "admin_" + role, PinHash: pinHash, IsGuest: false}
	if err := database.DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.DB.Create(&models.AdminUser{UserID: user.ID, Role: role, CreatedBy: "test"}).Error; err != nil {
		t.Fatal(err)
	}
	token, _, err := issueAdminSession(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	return user, token
}

func TestAdminLoginUsesShortSessionAndRejectsNonAdmins(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)

	email := "ordinary@example.com"
	pinHash, _ := hashOptionalPIN("2468")
	ordinary := models.User{UUID: generateUUID(), Email: &email, Username: "ordinary", PinHash: pinHash}
	if err := database.DB.Create(&ordinary).Error; err != nil {
		t.Fatal(err)
	}
	denied := performJSONRequest[map[string]any](t, router, http.MethodPost, "/v1/auth/admin/login", "", map[string]any{"email": email, "pin": "2468"}, http.StatusForbidden)
	if denied["error"] != "admin_access_denied" {
		t.Fatalf("unexpected non-admin response: %#v", denied)
	}

	adminEmail := "owner@example.com"
	owner := models.User{UUID: generateUUID(), Email: &adminEmail, Username: "owner", PinHash: pinHash}
	if err := database.DB.Create(&owner).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.DB.Create(&models.AdminUser{UserID: owner.ID, Role: models.AdminRoleOwner}).Error; err != nil {
		t.Fatal(err)
	}
	response := performJSONRequest[struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
		Role      string    `json:"role"`
	}](t, router, http.MethodPost, "/v1/auth/admin/login", "", map[string]any{"email": adminEmail, "pin": "2468"}, http.StatusOK)
	if !strings.HasPrefix(response.Token, "fnr_") || response.Role != models.AdminRoleOwner {
		t.Fatalf("unexpected admin login: %#v", response)
	}
	remaining := time.Until(response.ExpiresAt)
	if remaining < 7*time.Hour || remaining > 8*time.Hour+time.Minute {
		t.Fatalf("admin session TTL should be eight hours, got %s", remaining)
	}
}

func TestAdminRolesMaskPIIAndAuditMutations(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, viewerToken := createAdminSessionForTest(t, models.AdminRoleViewer)
	support, supportToken := createAdminSessionForTest(t, models.AdminRoleSupport)

	userEmail := "customer@example.com"
	userPhone := "+919876543210"
	user := models.User{UUID: generateUUID(), Email: &userEmail, Phone: &userPhone, Username: "customer"}
	if err := database.DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}

	listed := performJSONRequest[struct {
		Users []adminUserListItem `json:"users"`
	}](t, router, http.MethodGet, "/v1/admin/users?q=customer", viewerToken, nil, http.StatusOK)
	if len(listed.Users) != 1 || listed.Users[0].Email == userEmail || !strings.Contains(listed.Users[0].Email, "***") {
		t.Fatalf("user list did not mask email: %#v", listed.Users)
	}

	performJSONRequest[map[string]any](t, router, http.MethodPut, "/v1/admin/plans/monthly", viewerToken, map[string]any{"price_minor": 100}, http.StatusForbidden)
	adjustment := performJSONRequest[struct {
		Created bool `json:"created"`
	}](t, router, http.MethodPost, "/v1/admin/users/"+strconv.Itoa(int(user.ID))+"/credits/adjustments", supportToken, map[string]any{"credits": 50, "reason_code": "support_test"}, http.StatusCreated)
	if !adjustment.Created {
		t.Fatal("support credit adjustment was not created")
	}
	var audit models.AdminAuditLog
	if err := database.DB.Where("admin_user_id = ?", support.ID).Order("created_at DESC").First(&audit).Error; err != nil {
		t.Fatalf("expected support mutation audit row: %v", err)
	}
	if !strings.Contains(audit.Action, "credits_adjustments") {
		t.Fatalf("unexpected audit action: %#v", audit)
	}
}

func TestAdminEndpointsRequireConfiguredBearer(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)

	response := performJSONRequest[map[string]any](t, router, http.MethodGet, "/v1/admin/ai/metrics?date=2026-07-19", "", nil, http.StatusUnauthorized)
	if response["error"] != "admin_unauthorized" {
		t.Fatalf("unexpected admin auth response: %#v", response)
	}
}

func TestAdminMetricsAndCreditAdjustment(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, ownerToken := createAdminSessionForTest(t, models.AdminRoleOwner)
	user, _ := createBillingTestUserSession(t)

	adjustment := performJSONRequest[struct {
		Grant   models.CreditGrant `json:"grant"`
		Created bool               `json:"created"`
	}](t, router, http.MethodPost, "/v1/admin/credits/adjustments", ownerToken, map[string]any{
		"user_id":     user.ID,
		"credits":     25,
		"reason_code": "support_refund",
	}, http.StatusCreated)
	if !adjustment.Created || adjustment.Grant.CreditsRemaining != 25 {
		t.Fatalf("unexpected adjustment: %#v", adjustment)
	}

	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	event := models.AIUsageEvent{
		UserID:                 &user.ID,
		RequestID:              "ai_admin_metrics_1",
		ActionCode:             "transaction_parse_text",
		InputKind:              "text",
		Status:                 billing.UsageStatusSucceeded,
		EstimatedCredits:       5,
		ReservedCredits:        5,
		FinalCredits:           5,
		EstimatedCostUSDMicros: 500,
		StartedAt:              now,
	}
	if err := database.DB.Create(&event).Error; err != nil {
		t.Fatal(err)
	}

	metrics := performJSONRequest[billing.AIMetrics](t, router, http.MethodGet, "/v1/admin/ai/metrics?date=2026-07-19", ownerToken, nil, http.StatusOK)
	if metrics.TotalEvents != 1 || metrics.EstimatedCostUSDMicros != 500 {
		t.Fatalf("unexpected admin metrics: %#v", metrics)
	}
}

func TestAdminModelPricingAndLifetimeQuoteList(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, ownerToken := createAdminSessionForTest(t, models.AdminRoleOwner)
	user, _ := createBillingTestUserSession(t)

	invalid := performJSONRequest[map[string]any](t, router, http.MethodPut, "/v1/admin/ai/model-pricing", ownerToken, map[string]any{
		"provider":                "openai",
		"model":                   "gpt-4o-mini",
		"operation":               "unsupported",
		"input_token_usd_micros":  -1,
		"output_token_usd_micros": 6,
	}, http.StatusUnprocessableEntity)
	if invalid["error"] != "invalid_model_pricing" {
		t.Fatalf("unexpected invalid pricing response: %#v", invalid)
	}

	upserted := performJSONRequest[models.AIModelPricing](t, router, http.MethodPut, "/v1/admin/ai/model-pricing", ownerToken, map[string]any{
		"provider":                "openai",
		"model":                   "gpt-4o-mini",
		"operation":               "llm",
		"input_token_usd_micros":  2,
		"output_token_usd_micros": 6,
		"request_usd_micros":      10,
		"credit_usd_micros":       100,
	}, http.StatusOK)
	if upserted.ID == 0 || upserted.InputTokenUSDMicros != 2 {
		t.Fatalf("unexpected upserted pricing: %#v", upserted)
	}
	pricing := performJSONRequest[struct {
		Pricing []models.AIModelPricing `json:"pricing"`
	}](t, router, http.MethodGet, "/v1/admin/ai/model-pricing", ownerToken, nil, http.StatusOK)
	if len(pricing.Pricing) != 1 {
		t.Fatalf("expected one pricing row, got %#v", pricing)
	}

	quote := models.LifetimeQuoteRequest{
		UserID:              user.ID,
		Status:              "requested",
		PaidMonthsCompleted: 3,
		UsageWindowStart:    time.Now().UTC().AddDate(0, 0, -90),
		UsageWindowEnd:      time.Now().UTC(),
	}
	if err := database.DB.Create(&quote).Error; err != nil {
		t.Fatal(err)
	}
	quotes := performJSONRequest[struct {
		Requests []models.LifetimeQuoteRequest `json:"requests"`
		Total    int64                         `json:"total"`
	}](t, router, http.MethodGet, "/v1/admin/billing/lifetime-quotes?status=requested", ownerToken, nil, http.StatusOK)
	if quotes.Total != 1 || len(quotes.Requests) != 1 {
		t.Fatalf("unexpected lifetime quote list: %#v", quotes)
	}
}

func TestAdminAIAbuseBlockLifecycle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, ownerToken := createAdminSessionForTest(t, models.AdminRoleOwner)

	created := performJSONRequest[models.AIAbuseBlock](t, router, http.MethodPost, "/v1/admin/ai/abuse-blocks", ownerToken, map[string]any{
		"guest_device_id_hash": "admin-block-device-123",
		"scope":                "ai_parse",
		"reason_code":          "abuse_review",
		"notes":                "support requested temporary block",
		"created_by":           "support",
	}, http.StatusCreated)
	if created.ID == 0 || !created.Active || created.GuestDeviceIDHash == "admin-block-device-123" || len(created.GuestDeviceIDHash) != 64 {
		t.Fatalf("unexpected abuse block: %#v", created)
	}

	listed := performJSONRequest[struct {
		Blocks []models.AIAbuseBlock `json:"blocks"`
		Total  int64                 `json:"total"`
	}](t, router, http.MethodGet, "/v1/admin/ai/abuse-blocks?active=true", ownerToken, nil, http.StatusOK)
	if listed.Total != 1 || len(listed.Blocks) != 1 {
		t.Fatalf("unexpected abuse block list: %#v", listed)
	}

	updated := performJSONRequest[models.AIAbuseBlock](t, router, http.MethodPatch, "/v1/admin/ai/abuse-blocks/"+strconv.Itoa(int(created.ID)), ownerToken, map[string]any{
		"active": false,
		"notes":  "review complete",
	}, http.StatusOK)
	if updated.Active {
		t.Fatalf("expected block to be inactive: %#v", updated)
	}
}

// productionShapedRouter builds the router the way cmd/server does, including the
// outer static-bearer middleware that smokeRouter leaves off. The admin bypass
// lived precisely in the interaction between that middleware and the admin guard,
// so a test that skips it cannot see the bug.
func productionShapedRouter(t *testing.T, staticBearer, machineToken string) *gin.Engine {
	t.Helper()

	root := projectRoot(t)
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previous); err != nil {
			t.Fatal(err)
		}
	})

	gin.SetMode(gin.TestMode)
	return NewServer(&config.Config{
		AllowOrigins:       "http://localhost:3000",
		AuthBearer:         staticBearer,
		AdminStaticToken:   machineToken,
		TZDefault:          "Asia/Kolkata",
		OpenAIBaseURL:      "http://127.0.0.1",
		OpenAILlmModel:     "test-llm",
		OpenAIWhisper:      "test-whisper",
		OpenAIMaxTokens:    128,
		ReqTimeoutSec:      1,
		RateLimitRPS:       1000,
		RateLimitBurst:     1000,
		MaxJSONKB:          64,
		MaxUploadMB:        1,
		MaxTranscriptChars: 1000,
	})
}

func adminRequest(t *testing.T, router *gin.Engine, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, bytes.NewReader(nil))
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

// The static bearer is mandatory on /v1/admin, so the BFF sends it on every
// request. Treating it as proof of admin identity therefore let any visitor with
// no session at all through as owner.
func TestStaticBearerAloneIsNotAdminAccess(t *testing.T) {
	useSmokeDatabase(t)
	router := productionShapedRouter(t, "static-bearer", "")

	response := adminRequest(t, router, http.MethodGet, "/v1/admin/users", map[string]string{
		"Authorization": "Bearer static-bearer",
	})
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("static bearer without an admin session must not be admin access, got %d: %s", response.Code, response.Body.String())
	}
}

func TestAdminMachineTokenNeedsItsOwnHeader(t *testing.T) {
	useSmokeDatabase(t)
	router := productionShapedRouter(t, "static-bearer", "machine-token")

	granted := adminRequest(t, router, http.MethodGet, "/v1/admin/users", map[string]string{
		"Authorization": "Bearer static-bearer",
		"X-Admin-Token": "machine-token",
	})
	if granted.Code != http.StatusOK {
		t.Fatalf("configured machine token should be accepted, got %d: %s", granted.Code, granted.Body.String())
	}

	wrong := adminRequest(t, router, http.MethodGet, "/v1/admin/users", map[string]string{
		"Authorization": "Bearer static-bearer",
		"X-Admin-Token": "not-the-machine-token",
	})
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("a wrong machine token must be rejected, got %d", wrong.Code)
	}

	absent := adminRequest(t, router, http.MethodGet, "/v1/admin/users", map[string]string{
		"Authorization": "Bearer static-bearer",
	})
	if absent.Code != http.StatusUnauthorized {
		t.Fatalf("no machine token and no session must be rejected, got %d", absent.Code)
	}
}

// An admin's ordinary 30-day app login used to authenticate the admin API, which
// made the eight-hour admin session TTL decorative.
func TestOrdinaryAppSessionIsNotAnAdminSession(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)

	user, _ := createAdminSessionForTest(t, models.AdminRoleOwner)
	appToken := generateSessionToken()
	appSession := models.AuthSession{
		UserID:    user.ID,
		TokenHash: hashSessionToken(appToken),
		ExpiresAt: time.Now().UTC().Add(30 * 24 * time.Hour),
	}
	if err := database.DB.Create(&appSession).Error; err != nil {
		t.Fatal(err)
	}
	if appSession.Kind == adminSessionKind {
		t.Fatal("an app login should not be stored as an admin session")
	}

	response := adminRequest(t, router, http.MethodGet, "/v1/admin/users", map[string]string{
		"X-Admin-Session": appToken,
	})
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("an app session must not authenticate the admin API, got %d: %s", response.Code, response.Body.String())
	}
}

// Machine-token mutations used to be recorded against "first owner in the table",
// naming a real person as the author of something they did not do.
func TestMachineTokenMutationsAreNotAttributedToAHuman(t *testing.T) {
	useSmokeDatabase(t)
	router := productionShapedRouter(t, "static-bearer", "machine-token")

	owner, _ := createAdminSessionForTest(t, models.AdminRoleOwner)
	customer := models.User{UUID: generateUUID(), Username: "audit_customer"}
	if err := database.DB.Create(&customer).Error; err != nil {
		t.Fatal(err)
	}

	body, err := json.Marshal(map[string]any{"credits": 10, "reason_code": "machine_test"})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/admin/users/"+strconv.Itoa(int(customer.ID))+"/credits/adjustments", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer static-bearer")
	request.Header.Set("X-Admin-Token", "machine-token")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("machine credit adjustment failed: %d %s", response.Code, response.Body.String())
	}

	var entry models.AdminAuditLog
	if err := database.DB.Where("action LIKE ?", "%credits_adjustments").Order("id DESC").First(&entry).Error; err != nil {
		t.Fatalf("expected an audit row: %v", err)
	}
	if entry.AdminUserID != nil {
		t.Fatalf("machine action must not name a human, got admin_user_id=%d", *entry.AdminUserID)
	}
	if entry.Actor != "admin_static_token" {
		t.Fatalf("unexpected actor: %q", entry.Actor)
	}
	var ownerRows int64
	if err := database.DB.Model(&models.AdminAuditLog{}).Where("admin_user_id = ?", owner.ID).Count(&ownerRows).Error; err != nil {
		t.Fatal(err)
	}
	if ownerRows != 0 {
		t.Fatalf("owner %d was credited with %d actions they did not perform", owner.ID, ownerRows)
	}
}

// The export route was registered as "/export/:resource.csv", which Gin reads as
// a parameter literally named "resource.csv". c.Param("resource") was therefore
// always empty and every export answered 404 — as a text/csv attachment, so the
// browser silently saved the error body as a spreadsheet.
func TestAdminCSVExportsReturnData(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, ownerToken := createAdminSessionForTest(t, models.AdminRoleOwner)

	for _, resource := range []string{"users", "subscriptions", "ai-usage"} {
		response := adminRequest(t, router, http.MethodGet, "/v1/admin/export/"+resource+".csv", map[string]string{
			"X-Admin-Session": ownerToken,
		})
		if response.Code != http.StatusOK {
			t.Fatalf("%s export failed: %d %s", resource, response.Code, response.Body.String())
		}
		if !strings.HasPrefix(response.Header().Get("Content-Type"), "text/csv") {
			t.Fatalf("%s export is not CSV: %q", resource, response.Header().Get("Content-Type"))
		}
		if disposition := response.Header().Get("Content-Disposition"); !strings.Contains(disposition, resource+".csv") {
			t.Fatalf("%s export has an unnamed filename: %q", resource, disposition)
		}
		if body := response.Body.String(); !strings.Contains(body, "id,") {
			t.Fatalf("%s export has no header row: %q", resource, body)
		}
	}

	missing := adminRequest(t, router, http.MethodGet, "/v1/admin/export/nonsense.csv", map[string]string{
		"X-Admin-Session": ownerToken,
	})
	if missing.Code != http.StatusNotFound {
		t.Fatalf("unknown export should be 404, got %d", missing.Code)
	}
	if strings.Contains(missing.Header().Get("Content-Type"), "text/csv") {
		t.Fatal("an unknown export must not be delivered as a CSV download")
	}
}

// Disabling or demoting the only owner locked everyone out of the console, with
// no recovery short of an env change and a redeploy.
func TestTheLastOwnerCannotBeRemoved(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)
	owner, ownerToken := createAdminSessionForTest(t, models.AdminRoleOwner)

	var identity models.AdminUser
	if err := database.DB.Where("user_id = ?", owner.ID).First(&identity).Error; err != nil {
		t.Fatal(err)
	}
	id := strconv.Itoa(int(identity.ID))

	performJSONRequest[map[string]any](t, router, http.MethodPatch, "/v1/admin/admin-users/"+id, ownerToken, map[string]any{"disabled": true}, http.StatusUnprocessableEntity)
	performJSONRequest[map[string]any](t, router, http.MethodPatch, "/v1/admin/admin-users/"+id, ownerToken, map[string]any{"role": models.AdminRoleViewer}, http.StatusUnprocessableEntity)

	var stillOwner models.AdminUser
	if err := database.DB.First(&stillOwner, identity.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stillOwner.Role != models.AdminRoleOwner || stillOwner.DisabledAt != nil {
		t.Fatalf("the last owner was changed anyway: %#v", stillOwner)
	}

	// With a second owner in place the change is allowed.
	second, _ := createAdminSessionForTest(t, "owner-two")
	if err := database.DB.Model(&models.AdminUser{}).Where("user_id = ?", second.ID).Update("role", models.AdminRoleOwner).Error; err != nil {
		t.Fatal(err)
	}
	performJSONRequest[map[string]any](t, router, http.MethodPatch, "/v1/admin/admin-users/"+id, ownerToken, map[string]any{"role": models.AdminRoleViewer}, http.StatusOK)
}

// The activation funnel was rewritten from ~6 queries per cohort member to four
// batched reads. This pins the step definitions so the batching cannot quietly
// change what each step means.
func TestActivationFunnelStepsCountTheRightUsers(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, ownerToken := createAdminSessionForTest(t, models.AdminRoleOwner)

	signup := time.Now().UTC().Add(-20 * 24 * time.Hour)
	newUser := func(name string) models.User {
		user := models.User{UUID: generateUUID(), Username: name, CreatedAt: signup}
		if err := database.DB.Create(&user).Error; err != nil {
			t.Fatal(err)
		}
		if err := database.DB.Model(&user).UpdateColumn("created_at", signup).Error; err != nil {
			t.Fatal(err)
		}
		return user
	}
	addEntry := func(user models.User, at time.Time) {
		entry := models.Entry{UserID: user.ID, Title: "seeded", Type: "expense", Category: "Food & Drinks", Date: at.Format("2006-01-02")}
		if err := database.DB.Create(&entry).Error; err != nil {
			t.Fatal(err)
		}
		if err := database.DB.Model(&models.Entry{}).Where("id = ?", entry.ID).UpdateColumn("created_at", at).Error; err != nil {
			t.Fatal(err)
		}
	}

	// Reached signup only.
	newUser("stalled")

	// Reached an account, but never a transaction.
	withAccount := newUser("account_only")
	account := models.Account{UserID: withAccount.ID, Name: "Cash", Type: "cash"}
	if err := database.DB.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.DB.Model(&models.Account{}).Where("id = ?", account.ID).UpdateColumn("created_at", signup.Add(time.Hour)).Error; err != nil {
		t.Fatal(err)
	}

	// One transaction inside the window, so first_transaction but not habit_formed.
	oneEntry := newUser("one_entry")
	addEntry(oneEntry, signup.Add(2*time.Hour))

	// Five entries across three days: habit formed.
	habit := newUser("habit")
	for day := 0; day < 3; day++ {
		for repeat := 0; repeat <= day; repeat++ {
			addEntry(habit, signup.Add(time.Duration(day)*24*time.Hour+time.Duration(repeat)*time.Hour))
		}
	}

	// An entry after the seven-day deadline must not count as activation.
	late := newUser("late")
	addEntry(late, signup.Add(9*24*time.Hour))

	result := performJSONRequest[struct {
		CohortSize int `json:"cohort_size"`
		Onboarded  int `json:"onboarded"`
		Steps      []struct {
			Code  string `json:"code"`
			Users int    `json:"users"`
		} `json:"steps"`
	}](t, router, http.MethodGet, "/v1/admin/analytics/activation", ownerToken, nil, http.StatusOK)

	steps := map[string]int{}
	for _, step := range result.Steps {
		steps[step.Code] = step.Users
	}
	if result.CohortSize != 5 {
		t.Fatalf("expected a cohort of 5, got %d", result.CohortSize)
	}
	if steps["account_created"] != 1 {
		t.Fatalf("account_created should count only the seeded account, got %d", steps["account_created"])
	}
	// one_entry and habit are inside the window; late is not.
	if steps["first_transaction"] != 2 {
		t.Fatalf("first_transaction should exclude the late entry, got %d", steps["first_transaction"])
	}
	if result.Onboarded != steps["first_transaction"] {
		t.Fatalf("onboarded should equal first_transaction, got %d vs %d", result.Onboarded, steps["first_transaction"])
	}
	if steps["habit_formed"] != 1 {
		t.Fatalf("habit_formed needs 5 entries across 3 days, got %d", steps["habit_formed"])
	}
}
