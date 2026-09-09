package http

import (
	"testing"
	"time"

	"finnri/internal/ai"
	"finnri/internal/billing"
	"finnri/internal/config"
	"finnri/internal/database"
	"finnri/internal/models"
)

func TestRunMaintenanceOnceExpiresCreditsAndPurgesAnonymousGuests(t *testing.T) {
	useSmokeDatabase(t)

	now := time.Now().UTC()
	guestHash := billing.HashUsageKey("maintenance-old-guest")
	expiredAt := now.Add(-48 * time.Hour)
	oldGuestTime := now.Add(-10 * 24 * time.Hour)

	user := models.User{UUID: generateUUID(), Username: "maintenance_user_" + generateUUID()[:8]}
	if err := database.DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	userGrant := models.CreditGrant{
		UserID:           &user.ID,
		Source:           billing.GrantSourceFreeTrial,
		CreditsGranted:   100,
		CreditsRemaining: 25,
		ValidFrom:        now.Add(-30 * 24 * time.Hour),
		ExpiresAt:        &expiredAt,
	}
	guestGrant := models.CreditGrant{
		GuestDeviceIDHash: guestHash,
		Source:            billing.GrantSourceFreeTrial,
		CreditsGranted:    50,
		CreditsRemaining:  0,
		ValidFrom:         oldGuestTime.Add(-30 * 24 * time.Hour),
		ExpiresAt:         &oldGuestTime,
	}
	if err := database.DB.Create(&userGrant).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.DB.Create(&guestGrant).Error; err != nil {
		t.Fatal(err)
	}
	guestUsage := models.AIUsageEvent{
		GuestDeviceIDHash: guestHash,
		RequestID:         "maintenance-old-guest-usage",
		ActionCode:        string(ai.ActionTransactionParseText),
		InputKind:         "text",
		Status:            billing.UsageStatusSucceeded,
		EstimatedCredits:  5,
		ReservedCredits:   5,
		FinalCredits:      5,
		StartedAt:         oldGuestTime,
	}
	if err := database.DB.Create(&guestUsage).Error; err != nil {
		t.Fatal(err)
	}
	ledger := models.CreditLedger{
		GuestDeviceIDHash: guestHash,
		GrantID:           &guestGrant.ID,
		AIUsageEventID:    &guestUsage.ID,
		Direction:         billing.LedgerDirectionDebit,
		Credits:           5,
		BalanceAfter:      45,
		ReasonCode:        billing.ReasonReservationDebit,
		IdempotencyKey:    "maintenance-old-guest-ledger",
		CreatedAt:         oldGuestTime,
	}
	if err := database.DB.Create(&ledger).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.DB.Create(&models.AIUsageLimitEvent{GuestDeviceIDHash: guestHash, ActionCode: string(ai.ActionTransactionParseText), Reason: billing.AllowanceDailyLimitReached, CreatedAt: oldGuestTime}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.DB.Create(&models.DailyCreditUsage{GuestDeviceIDHash: guestHash, UsageDate: oldGuestTime.Format("2006-01-02"), CreditsUsed: 5}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.DB.Create(&models.GuestUsageKey{GuestDeviceIDHash: guestHash, IPHash: billing.HashUsageKey("203.0.113.50"), FirstSeenAt: oldGuestTime, LastSeenAt: oldGuestTime, TrialGrantID: &guestGrant.ID}).Error; err != nil {
		t.Fatal(err)
	}

	result, err := runMaintenanceOnce(&config.Config{AnonymousGuestRetentionDays: 7})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExpiredCreditGrants != 1 || result.AnonymousGuestRetention.AIUsageEvents != 1 || result.AnonymousGuestRetention.GuestUsageKeys != 1 {
		t.Fatalf("unexpected maintenance result: %#v", result)
	}

	var reloadedGrant models.CreditGrant
	if err := database.DB.First(&reloadedGrant, userGrant.ID).Error; err != nil {
		t.Fatal(err)
	}
	if reloadedGrant.CreditsRemaining != 0 {
		t.Fatalf("expected expired user grant balance to be zeroed, got %#v", reloadedGrant)
	}
	assertMaintenanceGuestCount(t, &models.AIUsageEvent{}, guestHash, 0)
	assertMaintenanceGuestCount(t, &models.AIUsageLimitEvent{}, guestHash, 0)
	assertMaintenanceGuestCount(t, &models.DailyCreditUsage{}, guestHash, 0)
	assertMaintenanceGuestCount(t, &models.GuestUsageKey{}, guestHash, 0)
	assertMaintenanceGuestCount(t, &models.CreditLedger{}, guestHash, 0)
	assertMaintenanceGuestCount(t, &models.CreditGrant{}, guestHash, 0)
}

func assertMaintenanceGuestCount(t *testing.T, model any, guestHash string, want int64) {
	t.Helper()

	var count int64
	if err := database.DB.Model(model).Where("guest_device_id_hash = ?", guestHash).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("unexpected maintenance guest count for %T: got %d, want %d", model, count, want)
	}
}
