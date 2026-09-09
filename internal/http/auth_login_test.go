package http

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"finnri/internal/ai"
	"finnri/internal/billing"
	"finnri/internal/database"
	"finnri/internal/models"
)

func TestAuthLoginAcceptsLegacyRepeatedDigitPIN(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)

	email := "legacy@example.com"
	hash, err := bcrypt.GenerateFromPassword([]byte("0000"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("failed to hash legacy PIN: %v", err)
	}
	user := models.User{
		UUID:     generateUUID(),
		Email:    &email,
		PinHash:  string(hash),
		Username: "legacy_user",
	}
	if err := database.DB.Create(&user).Error; err != nil {
		t.Fatalf("failed to create legacy user: %v", err)
	}

	response := performJSONRequest[AuthResponse](
		t,
		router,
		http.MethodPost,
		"/v1/auth/login",
		"",
		map[string]string{"identifier": email, "pin": "0000"},
		http.StatusOK,
	)
	if !strings.HasPrefix(response.Token, "fnr_") {
		t.Fatalf("expected opaque session token, got %q", response.Token)
	}
	if response.User == nil || response.User.ID != user.ID || !response.User.HasPin {
		t.Fatalf("unexpected login user response: %#v", response.User)
	}
}

func TestAuthGoogleCreatesUserAndSession(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)
	stubGoogleVerifier(t, googleIdentity{
		Subject:       "google-sub-new",
		Email:         "new-google@example.com",
		EmailVerified: true,
		Name:          "New Google",
		Picture:       "https://example.com/avatar.png",
	})

	response := performJSONRequest[AuthResponse](
		t,
		router,
		http.MethodPost,
		"/v1/auth/google",
		"",
		map[string]string{"id_token": "valid-google-token", "device_id": "google-device"},
		http.StatusOK,
	)

	if !strings.HasPrefix(response.Token, "fnr_") {
		t.Fatalf("expected opaque session token, got %q", response.Token)
	}
	if response.User == nil || response.User.Email == nil || *response.User.Email != "new-google@example.com" || response.User.IsGuest {
		t.Fatalf("unexpected google user response: %#v", response.User)
	}
	if response.User.HasPin {
		t.Fatalf("google-only account should not require an existing PIN")
	}
}

func TestAuthGoogleLinksExistingEmailUser(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)
	email := "link-google@example.com"
	user := models.User{
		UUID:     generateUUID(),
		Email:    &email,
		PinHash:  "legacy-pin-hash",
		Username: "link_google_user",
	}
	if err := database.DB.Create(&user).Error; err != nil {
		t.Fatalf("failed to create existing user: %v", err)
	}
	stubGoogleVerifier(t, googleIdentity{
		Subject:       "google-sub-link",
		Email:         email,
		EmailVerified: true,
	})

	response := performJSONRequest[AuthResponse](
		t,
		router,
		http.MethodPost,
		"/v1/auth/google",
		"",
		map[string]string{"id_token": "valid-google-token"},
		http.StatusOK,
	)

	if response.User == nil || response.User.ID != user.ID {
		t.Fatalf("expected existing user to be linked, got %#v", response.User)
	}
	var linked models.User
	if err := database.DB.First(&linked, user.ID).Error; err != nil {
		t.Fatalf("failed to reload linked user: %v", err)
	}
	if linked.GoogleSubject == nil || *linked.GoogleSubject != "google-sub-link" {
		t.Fatalf("expected google subject to be linked, got %#v", linked.GoogleSubject)
	}
}

func TestAuthGoogleUpgradesGuest(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)
	guestResponse := performJSONRequest[AuthResponse](
		t,
		router,
		http.MethodPost,
		"/v1/auth/guest",
		"",
		map[string]string{"device_id": "google-guest-device"},
		http.StatusOK,
	)
	stubGoogleVerifier(t, googleIdentity{
		Subject:       "google-sub-guest",
		Email:         "guest-upgrade@example.com",
		EmailVerified: true,
	})

	response := performJSONRequest[AuthResponse](
		t,
		router,
		http.MethodPost,
		"/v1/auth/google",
		"",
		map[string]string{
			"id_token":   "valid-google-token",
			"guest_uuid": guestResponse.User.UUID,
			"device_id":  "google-guest-device",
		},
		http.StatusOK,
	)

	if response.User == nil || response.User.UUID != guestResponse.User.UUID || response.User.IsGuest {
		t.Fatalf("expected guest to be upgraded in place, got %#v", response.User)
	}
}

func TestAuthRegisterUpgradesGuestWithDataAndAICredits(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)

	deviceID := "device-sync-upgrade-12345"
	guestAuth := performJSONRequest[AuthResponse](
		t,
		router,
		http.MethodPost,
		"/v1/auth/guest",
		"",
		map[string]string{"device_id": deviceID},
		http.StatusOK,
	)
	if guestAuth.User == nil || !guestAuth.User.IsGuest {
		t.Fatalf("expected guest auth response, got %#v", guestAuth.User)
	}

	accounts := performJSONRequest[[]models.Account](
		t, router, http.MethodGet, "/v1/accounts", guestAuth.Token, nil, http.StatusOK,
	)
	if len(accounts) != 1 {
		t.Fatalf("expected guest default account, got %#v", accounts)
	}
	savedEntry := performJSONRequest[models.Entry](
		t,
		router,
		http.MethodPost,
		"/v1/entries",
		guestAuth.Token,
		map[string]any{
			"title": "Coffee", "type": "expense", "amount": 120, "currency": "INR",
			"source": "manual", "account_id": accounts[0].ID, "mode": "Cash",
			"category": "Food", "merchant": "Cafe", "date": "2026-07-12",
		},
		http.StatusCreated,
	)

	creditService := billing.NewCreditService(database.DB)
	usageEvent, _, err := creditService.ReserveCredits(
		billing.SubjectForGuestDeviceID(deviceID),
		ai.ActionTransactionParseText,
		"guest-upgrade-credit-spend",
	)
	if err != nil {
		t.Fatalf("failed to reserve guest credits: %v", err)
	}
	finalCredits := 5
	if _, err := creditService.FinalizeUsage(usageEvent.ID, billing.ProviderUsage{
		Status:       billing.UsageStatusSucceeded,
		FinalCredits: &finalCredits,
	}); err != nil {
		t.Fatalf("failed to finalize guest usage: %v", err)
	}

	email := "sync-upgrade@example.com"
	claimToken := createVerifiedClaimToken(t, "email", email)
	registered := performJSONRequest[AuthResponse](
		t,
		router,
		http.MethodPost,
		"/v1/auth/register",
		"",
		map[string]any{
			"claim_token":        claimToken,
			"pin":                "1234",
			"guest_uuid":         guestAuth.User.UUID,
			"device_id":          deviceID,
			"biometrics_enabled": true,
		},
		http.StatusCreated,
	)
	if registered.User == nil || registered.User.IsGuest || registered.User.ID != guestAuth.User.ID || registered.User.UUID != guestAuth.User.UUID {
		t.Fatalf("expected in-place guest upgrade, got %#v from guest %#v", registered.User, guestAuth.User)
	}
	if registered.User.Email == nil || *registered.User.Email != email || !registered.User.HasPin || !registered.User.BiometricsEnabled {
		t.Fatalf("registered user missing verified account fields: %#v", registered.User)
	}

	_ = performJSONRequest[map[string]string](
		t, router, http.MethodGet, "/v1/ai/credits", guestAuth.Token, nil, http.StatusUnauthorized,
	)

	dashboard := performJSONRequest[DashboardResponse](
		t,
		router,
		http.MethodGet,
		"/v1/dashboard?start_date=2026-07-12&end_date=2026-07-12",
		registered.Token,
		nil,
		http.StatusOK,
	)
	if dashboard.Summary.TransactionCount != 1 || len(dashboard.RecentTransactions) != 1 || dashboard.RecentTransactions[0].ID != savedEntry.ID {
		t.Fatalf("upgraded session did not retain guest entry: %#v", dashboard)
	}

	credits := performJSONRequest[creditSummaryResponse](
		t, router, http.MethodGet, "/v1/ai/credits", registered.Token, nil, http.StatusOK,
	)
	if credits.TotalCreditsRemaining != billing.LoggedInFreeTrialCredits-finalCredits ||
		credits.DailyLimit != billing.LoggedInFreeDailyLimit ||
		credits.DailyCreditsUsed != finalCredits ||
		credits.DailyCreditsRemaining != billing.LoggedInFreeDailyLimit-finalCredits {
		t.Fatalf("guest credits were not promoted into logged-in allowance correctly: %#v", credits)
	}

	usage := performJSONRequest[aiUsageListResponse](
		t, router, http.MethodGet, "/v1/ai/usage?page=1&page_size=10", registered.Token, nil, http.StatusOK,
	)
	if usage.Total != 1 || len(usage.Events) != 1 || usage.Events[0].ID != usageEvent.ID {
		t.Fatalf("guest AI usage history was not visible after upgrade: %#v", usage)
	}

	guestHash := billing.HashUsageKey(deviceID)
	var guestGrantCount int64
	if err := database.DB.Model(&models.CreditGrant{}).Where("guest_device_id_hash = ?", guestHash).Count(&guestGrantCount).Error; err != nil {
		t.Fatal(err)
	}
	if guestGrantCount != 0 {
		t.Fatalf("expected guest-scoped grants to be promoted, got %d", guestGrantCount)
	}
	var userGrant models.CreditGrant
	if err := database.DB.Where("user_id = ? AND source = ?", registered.User.ID, billing.GrantSourceFreeTrial).First(&userGrant).Error; err != nil {
		t.Fatalf("expected promoted user trial grant: %v", err)
	}
	if userGrant.CreditsGranted != billing.LoggedInFreeTrialCredits || userGrant.CreditsRemaining != billing.LoggedInFreeTrialCredits-finalCredits {
		t.Fatalf("unexpected promoted trial grant: %#v", userGrant)
	}
	if userGrant.ExpiresAt == nil || userGrant.ExpiresAt.Before(time.Now().UTC().Add(29*24*time.Hour)) {
		t.Fatalf("promoted trial grant did not receive a logged-in trial window: %#v", userGrant.ExpiresAt)
	}
}

func stubGoogleVerifier(t *testing.T, identity googleIdentity) {
	t.Helper()
	previous := verifyGoogleIDToken
	verifyGoogleIDToken = func(ctx context.Context, rawToken string, audiences []string) (googleIdentity, error) {
		if rawToken != "valid-google-token" {
			t.Fatalf("unexpected google token %q", rawToken)
		}
		if len(audiences) != 1 || audiences[0] != "test-google-client" {
			t.Fatalf("unexpected google audiences %#v", audiences)
		}
		return identity, nil
	}
	t.Cleanup(func() {
		verifyGoogleIDToken = previous
	})
}

func createVerifiedClaimToken(t *testing.T, identifierType, identifier string) string {
	t.Helper()

	token := generateClaimToken()
	tokenHash := hashClaimToken(token)
	now := time.Now().UTC()
	claimExpiresAt := now.Add(10 * time.Minute)
	otpExpiresAt := now.Add(10 * time.Minute)
	verification := models.AuthVerification{
		IdentifierType: identifierType,
		Identifier:     identifier,
		OTPHash:        "test-otp-hash",
		OTPExpiresAt:   otpExpiresAt,
		VerifiedAt:     &now,
		ClaimTokenHash: &tokenHash,
		ClaimExpiresAt: &claimExpiresAt,
		ClaimUsedAt:    nil,
		Attempts:       0,
	}
	if err := database.DB.Create(&verification).Error; err != nil {
		t.Fatalf("failed to create verified claim: %v", err)
	}
	return token
}
