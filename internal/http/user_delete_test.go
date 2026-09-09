package http

import (
	"path/filepath"
	"testing"
	"time"

	"finnri/internal/ai"
	"finnri/internal/billing"
	"finnri/internal/database"
	"finnri/internal/models"
)

func TestDeleteUserSkipStaticBearerGate(t *testing.T) {
	if !skipsStaticBearer("/v1/user") {
		t.Fatal("user deletion should use user-session auth, not the static bearer gate")
	}
}

func TestVerificationIdentifiersForUser(t *testing.T) {
	email := " USER@Example.COM "
	phone := " +919876543210 "
	identifiers := verificationIdentifiersForUser(models.User{Email: &email, Phone: &phone})

	if len(identifiers) != 2 {
		t.Fatalf("expected two identifiers, got %v", identifiers)
	}
	if identifiers[0].identifierType != "email" || identifiers[0].identifier != "user@example.com" {
		t.Fatalf("unexpected email identifier: %#v", identifiers[0])
	}
	if identifiers[1].identifierType != "phone" || identifiers[1].identifier != "+919876543210" {
		t.Fatalf("unexpected phone identifier: %#v", identifiers[1])
	}
}

func TestLocalUploadPathFromAttachment(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{name: "relative upload", raw: "uploads/receipt.png", want: filepath.FromSlash("uploads/receipt.png"), ok: true},
		{name: "absolute upload URL", raw: "https://api.example.com/uploads/receipt.png", want: filepath.FromSlash("uploads/receipt.png"), ok: true},
		{name: "nested upload", raw: "/uploads/2026/receipt.png", want: filepath.FromSlash("uploads/2026/receipt.png"), ok: true},
		{name: "empty", raw: " ", ok: false},
		{name: "directory only", raw: "uploads", ok: false},
		{name: "path traversal", raw: "uploads/../secret.txt", ok: false},
		{name: "outside uploads", raw: "/tmp/uploads/receipt.png", ok: false},
	}

	for _, tt := range tests {
		got, ok := localUploadPathFromAttachment(tt.raw)
		if ok != tt.ok || got != tt.want {
			t.Fatalf("%s: got path=%q ok=%v, want path=%q ok=%v", tt.name, got, ok, tt.want, tt.ok)
		}
	}
}

func TestDeleteUserDataRejectsMissingUserID(t *testing.T) {
	if _, err := deleteUserData(nil, models.User{}); err == nil {
		t.Fatal("expected missing user id to be rejected before database access")
	}
}

func TestDeleteUserDataDeletesBillingAndAIRecords(t *testing.T) {
	useSmokeDatabase(t)

	deviceID := "delete-user-device"
	guestHash := billing.HashUsageKey(deviceID)
	user := models.User{UUID: generateUUID(), Username: "delete_user_" + generateUUID()[:8], DeviceID: &deviceID}
	otherUser := models.User{UUID: generateUUID(), Username: "keep_user_" + generateUUID()[:8]}
	if err := database.DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.DB.Create(&otherUser).Error; err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	plan := models.Plan{
		Code:             "delete_test_" + generateUUID()[:8],
		Name:             "Delete Test",
		BillingInterval:  "monthly",
		IncludedCredits:  3000,
		DailyCreditLimit: 200,
	}
	if err := database.DB.Create(&plan).Error; err != nil {
		t.Fatal(err)
	}
	subscription := models.UserSubscription{
		UserID:             user.ID,
		PlanID:             plan.ID,
		Status:             "active",
		CurrentPeriodStart: now.AddDate(0, -1, 0),
		CurrentPeriodEnd:   now.AddDate(0, 1, 0),
		Provider:           "test",
	}
	otherSubscription := models.UserSubscription{
		UserID:             otherUser.ID,
		PlanID:             plan.ID,
		Status:             "active",
		CurrentPeriodStart: now.AddDate(0, -1, 0),
		CurrentPeriodEnd:   now.AddDate(0, 1, 0),
		Provider:           "test",
	}
	if err := database.DB.Create(&subscription).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.DB.Create(&otherSubscription).Error; err != nil {
		t.Fatal(err)
	}

	expiresAt := now.AddDate(0, 1, 0)
	userGrant := models.CreditGrant{
		UserID:           &user.ID,
		Source:           billing.GrantSourceFreeTrial,
		CreditsGranted:   100,
		CreditsRemaining: 75,
		ValidFrom:        now.AddDate(0, -1, 0),
		ExpiresAt:        &expiresAt,
		SubscriptionID:   &subscription.ID,
	}
	guestGrant := models.CreditGrant{
		GuestDeviceIDHash: guestHash,
		Source:            billing.GrantSourceFreeTrial,
		CreditsGranted:    50,
		CreditsRemaining:  40,
		ValidFrom:         now.AddDate(0, -1, 0),
		ExpiresAt:         &expiresAt,
	}
	otherGrant := models.CreditGrant{
		UserID:           &otherUser.ID,
		Source:           billing.GrantSourceFreeTrial,
		CreditsGranted:   100,
		CreditsRemaining: 100,
		ValidFrom:        now.AddDate(0, -1, 0),
		ExpiresAt:        &expiresAt,
		SubscriptionID:   &otherSubscription.ID,
	}
	if err := database.DB.Create(&userGrant).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.DB.Create(&guestGrant).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.DB.Create(&otherGrant).Error; err != nil {
		t.Fatal(err)
	}

	usageEvent := models.AIUsageEvent{
		UserID:           &user.ID,
		RequestID:        "delete-user-ai-" + generateUUID(),
		ActionCode:       string(ai.ActionTransactionParseText),
		InputKind:        "text",
		Status:           billing.UsageStatusSucceeded,
		EstimatedCredits: 5,
		ReservedCredits:  5,
		FinalCredits:     5,
		StartedAt:        now,
	}
	guestUsageEvent := models.AIUsageEvent{
		GuestDeviceIDHash: guestHash,
		RequestID:         "delete-guest-ai-" + generateUUID(),
		ActionCode:        string(ai.ActionTransactionParseText),
		InputKind:         "text",
		Status:            billing.UsageStatusSucceeded,
		EstimatedCredits:  5,
		ReservedCredits:   5,
		FinalCredits:      5,
		StartedAt:         now,
	}
	otherUsageEvent := models.AIUsageEvent{
		UserID:           &otherUser.ID,
		RequestID:        "keep-ai-" + generateUUID(),
		ActionCode:       string(ai.ActionTransactionParseText),
		InputKind:        "text",
		Status:           billing.UsageStatusSucceeded,
		EstimatedCredits: 5,
		ReservedCredits:  5,
		FinalCredits:     5,
		StartedAt:        now,
	}
	if err := database.DB.Create(&usageEvent).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.DB.Create(&guestUsageEvent).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.DB.Create(&otherUsageEvent).Error; err != nil {
		t.Fatal(err)
	}

	ledgerRows := []models.CreditLedger{
		{UserID: &user.ID, GrantID: &userGrant.ID, AIUsageEventID: &usageEvent.ID, Direction: billing.LedgerDirectionDebit, Credits: 5, BalanceAfter: 95, ReasonCode: billing.ReasonReservationDebit, IdempotencyKey: "delete-user-ledger", CreatedAt: now},
		{GuestDeviceIDHash: guestHash, GrantID: &guestGrant.ID, AIUsageEventID: &guestUsageEvent.ID, Direction: billing.LedgerDirectionDebit, Credits: 5, BalanceAfter: 45, ReasonCode: billing.ReasonReservationDebit, IdempotencyKey: "delete-guest-ledger", CreatedAt: now},
		{UserID: &otherUser.ID, GrantID: &otherGrant.ID, AIUsageEventID: &otherUsageEvent.ID, Direction: billing.LedgerDirectionDebit, Credits: 5, BalanceAfter: 95, ReasonCode: billing.ReasonReservationDebit, IdempotencyKey: "keep-ledger", CreatedAt: now},
	}
	if err := database.DB.Create(&ledgerRows).Error; err != nil {
		t.Fatal(err)
	}

	limitEvent := models.AIUsageLimitEvent{UserID: &user.ID, ActionCode: string(ai.ActionTransactionParseText), Reason: billing.AllowanceDailyLimitReached, CreatedAt: now}
	guestLimitEvent := models.AIUsageLimitEvent{GuestDeviceIDHash: guestHash, ActionCode: string(ai.ActionTransactionParseText), Reason: billing.AllowanceDailyLimitReached, CreatedAt: now}
	dailyUsage := models.DailyCreditUsage{UserID: &user.ID, UsageDate: now.Format("2006-01-02"), CreditsUsed: 5}
	guestDailyUsage := models.DailyCreditUsage{GuestDeviceIDHash: guestHash, UsageDate: now.Format("2006-01-02"), CreditsUsed: 5}
	abuseBlock := models.AIAbuseBlock{UserID: &user.ID, Scope: "ai_parse", ReasonCode: "test", Active: true}
	guestAbuseBlock := models.AIAbuseBlock{GuestDeviceIDHash: guestHash, Scope: "ai_parse", ReasonCode: "test", Active: true}
	quoteRequest := models.LifetimeQuoteRequest{UserID: user.ID, PaidMonthsCompleted: 3, UsageWindowStart: now.AddDate(0, -3, 0), UsageWindowEnd: now}
	guestUsageKey := models.GuestUsageKey{GuestDeviceIDHash: guestHash, IPHash: billing.HashUsageKey("127.0.0.1"), FirstSeenAt: now, LastSeenAt: now, TrialGrantID: &guestGrant.ID}
	if err := database.DB.Create(&limitEvent).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.DB.Create(&guestLimitEvent).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.DB.Create(&dailyUsage).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.DB.Create(&guestDailyUsage).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.DB.Create(&abuseBlock).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.DB.Create(&guestAbuseBlock).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.DB.Create(&quoteRequest).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.DB.Create(&guestUsageKey).Error; err != nil {
		t.Fatal(err)
	}

	if _, err := deleteUserData(database.DB, user); err != nil {
		t.Fatalf("deleteUserData failed: %v", err)
	}

	assertUserBillingCount(t, &models.CreditLedger{}, user.ID, guestHash, 0)
	assertUserBillingCount(t, &models.AIUsageEvent{}, user.ID, guestHash, 0)
	assertUserBillingCount(t, &models.AIUsageLimitEvent{}, user.ID, guestHash, 0)
	assertUserBillingCount(t, &models.DailyCreditUsage{}, user.ID, guestHash, 0)
	assertUserBillingCount(t, &models.AIAbuseBlock{}, user.ID, guestHash, 0)
	assertUserBillingCount(t, &models.CreditGrant{}, user.ID, guestHash, 0)
	assertUserIDCount(t, &models.LifetimeQuoteRequest{}, user.ID, 0)
	assertUserIDCount(t, &models.UserSubscription{}, user.ID, 0)
	assertGuestHashCount(t, &models.GuestUsageKey{}, guestHash, 0)
	assertIDCount(t, &models.User{}, user.ID, 0)

	assertIDCount(t, &models.User{}, otherUser.ID, 1)
	assertUserIDCount(t, &models.UserSubscription{}, otherUser.ID, 1)
	assertUserBillingCount(t, &models.CreditGrant{}, otherUser.ID, "", 1)
	assertUserBillingCount(t, &models.AIUsageEvent{}, otherUser.ID, "", 1)
	assertUserBillingCount(t, &models.CreditLedger{}, otherUser.ID, "", 1)
}

func assertUserBillingCount(t *testing.T, model any, userID uint, guestHash string, want int64) {
	t.Helper()

	query := database.DB.Model(model).Where("user_id = ?", userID)
	if guestHash != "" {
		query = query.Or("guest_device_id_hash = ?", guestHash)
	}
	var count int64
	if err := query.Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("unexpected billing count for %T: got %d, want %d", model, count, want)
	}
}

func assertUserIDCount(t *testing.T, model any, userID uint, want int64) {
	t.Helper()

	var count int64
	if err := database.DB.Model(model).Where("user_id = ?", userID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("unexpected user-owned count for %T: got %d, want %d", model, count, want)
	}
}

func assertIDCount(t *testing.T, model any, id uint, want int64) {
	t.Helper()

	var count int64
	if err := database.DB.Model(model).Where("id = ?", id).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("unexpected user-owned count for %T: got %d, want %d", model, count, want)
	}
}

func assertGuestHashCount(t *testing.T, model any, guestHash string, want int64) {
	t.Helper()

	var count int64
	if err := database.DB.Model(model).Where("guest_device_id_hash = ?", guestHash).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("unexpected guest-hash count for %T: got %d, want %d", model, count, want)
	}
}
