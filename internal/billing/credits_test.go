package billing

import (
	"errors"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"finnri/internal/ai"
	"finnri/internal/models"
)

func setupCreditTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("failed to access test database handle: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(
		&models.User{},
		&models.Plan{},
		&models.UserSubscription{},
		&models.CreditGrant{},
		&models.AIUsageEvent{},
		&models.CreditLedger{},
		&models.DailyCreditUsage{},
		&models.GuestUsageKey{},
		&models.AIUsageLimitEvent{},
		&models.AIModelPricing{},
		&models.AIAbuseBlock{},
	); err != nil {
		t.Fatalf("failed to migrate test database: %v", err)
	}
	return db
}

func TestEnsureLoggedInFreeTrialGrantCreatesOnce(t *testing.T) {
	db := setupCreditTestDB(t)
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	service := NewCreditServiceWithClock(db, func() time.Time { return now })

	user := models.User{UUID: "user-1", Username: "user_1"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}

	grant, created, err := service.EnsureLoggedInFreeTrialGrant(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("expected first grant call to create a grant")
	}
	if grant.CreditsGranted != LoggedInFreeTrialCredits || grant.CreditsRemaining != LoggedInFreeTrialCredits {
		t.Fatalf("unexpected grant credits: %#v", grant)
	}
	if grant.ExpiresAt == nil || !grant.ExpiresAt.Equal(now.Add(TrialDuration)) {
		t.Fatalf("unexpected grant expiry: %#v", grant.ExpiresAt)
	}

	second, created, err := service.EnsureLoggedInFreeTrialGrant(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("expected second grant call to reuse the existing grant")
	}
	if second.ID != grant.ID {
		t.Fatalf("expected existing grant %d, got %d", grant.ID, second.ID)
	}

	var grantCount int64
	if err := db.Model(&models.CreditGrant{}).Where("user_id = ? AND source = ?", user.ID, GrantSourceFreeTrial).Count(&grantCount).Error; err != nil {
		t.Fatal(err)
	}
	if grantCount != 1 {
		t.Fatalf("expected one free trial grant, got %d", grantCount)
	}

	var ledgerCount int64
	if err := db.Model(&models.CreditLedger{}).Where("grant_id = ? AND direction = ?", grant.ID, LedgerDirectionGrant).Count(&ledgerCount).Error; err != nil {
		t.Fatal(err)
	}
	if ledgerCount != 1 {
		t.Fatalf("expected one grant ledger row, got %d", ledgerCount)
	}
}

func TestEnsureGuestTrialGrantHashesDeviceAndCreatesOnce(t *testing.T) {
	db := setupCreditTestDB(t)
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	service := NewCreditServiceWithClock(db, func() time.Time { return now })

	grant, created, err := service.EnsureGuestTrialGrant("device-123", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("expected first guest call to create a grant")
	}
	if grant.UserID != nil {
		t.Fatalf("guest grant should not have user id: %#v", grant.UserID)
	}
	if grant.GuestDeviceIDHash == "" || grant.GuestDeviceIDHash == "device-123" || len(grant.GuestDeviceIDHash) != 64 {
		t.Fatalf("guest device id should be hashed, got %q", grant.GuestDeviceIDHash)
	}
	if grant.CreditsGranted != GuestTrialCredits || grant.CreditsRemaining != GuestTrialCredits {
		t.Fatalf("unexpected guest credits: %#v", grant)
	}

	second, created, err := service.EnsureGuestTrialGrant("device-123", "203.0.113.11")
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("expected second guest call to reuse the existing grant")
	}
	if second.ID != grant.ID {
		t.Fatalf("expected existing guest grant %d, got %d", grant.ID, second.ID)
	}

	var guestKey models.GuestUsageKey
	if err := db.Where("guest_device_id_hash = ?", grant.GuestDeviceIDHash).First(&guestKey).Error; err != nil {
		t.Fatal(err)
	}
	if guestKey.IPHash != HashUsageKey("203.0.113.11") {
		t.Fatalf("expected guest ip hash to update, got %q", guestKey.IPHash)
	}
	if guestKey.TrialGrantID == nil || *guestKey.TrialGrantID != grant.ID {
		t.Fatalf("expected guest key to reference trial grant, got %#v", guestKey.TrialGrantID)
	}

	var grantCount int64
	if err := db.Model(&models.CreditGrant{}).Where("guest_device_id_hash = ? AND source = ?", grant.GuestDeviceIDHash, GrantSourceFreeTrial).Count(&grantCount).Error; err != nil {
		t.Fatal(err)
	}
	if grantCount != 1 {
		t.Fatalf("expected one guest trial grant, got %d", grantCount)
	}
}

func TestEnsureGuestTrialGrantSkipsMissingDeviceID(t *testing.T) {
	db := setupCreditTestDB(t)
	service := NewCreditService(db)

	grant, created, err := service.EnsureGuestTrialGrant("", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	if created || grant.ID != 0 {
		t.Fatalf("expected missing device id to skip guest grant, got created=%v grant=%#v", created, grant)
	}
}

func TestGrantSubscriptionPeriodCreatesOnce(t *testing.T) {
	db := setupCreditTestDB(t)
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	service := NewCreditServiceWithClock(db, func() time.Time { return now })

	user := models.User{UUID: "user-1", Username: "user_1"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	plan := models.Plan{Code: "monthly", Name: "Monthly", BillingInterval: "monthly", IncludedCredits: 3000, DailyCreditLimit: 200}
	if err := db.Create(&plan).Error; err != nil {
		t.Fatal(err)
	}
	subscription := models.UserSubscription{
		UserID:             user.ID,
		PlanID:             plan.ID,
		Status:             "active",
		CurrentPeriodStart: now,
		CurrentPeriodEnd:   now.Add(30 * 24 * time.Hour),
	}
	if err := db.Create(&subscription).Error; err != nil {
		t.Fatal(err)
	}

	grant, created, err := service.GrantSubscriptionPeriod(subscription, plan.IncludedCredits, subscription.CurrentPeriodStart, subscription.CurrentPeriodEnd)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("expected first subscription call to create a grant")
	}
	if grant.CreditsGranted != 3000 || grant.SubscriptionID == nil || *grant.SubscriptionID != subscription.ID {
		t.Fatalf("unexpected subscription grant: %#v", grant)
	}

	second, created, err := service.GrantSubscriptionPeriod(subscription, plan.IncludedCredits, subscription.CurrentPeriodStart, subscription.CurrentPeriodEnd)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("expected second subscription call to reuse existing grant")
	}
	if second.ID != grant.ID {
		t.Fatalf("expected existing grant %d, got %d", grant.ID, second.ID)
	}
}

func TestExpireCreditsMovesRemainingBalanceToLedger(t *testing.T) {
	db := setupCreditTestDB(t)
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	service := NewCreditServiceWithClock(db, func() time.Time { return now })

	user := models.User{UUID: "user-1", Username: "user_1"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	expiresAt := now.Add(-time.Hour)
	grant := models.CreditGrant{
		UserID:           &user.ID,
		Source:           GrantSourceFreeTrial,
		CreditsGranted:   100,
		CreditsRemaining: 40,
		ValidFrom:        now.Add(-TrialDuration),
		ExpiresAt:        &expiresAt,
	}
	if err := db.Create(&grant).Error; err != nil {
		t.Fatal(err)
	}

	expired, err := service.ExpireCredits()
	if err != nil {
		t.Fatal(err)
	}
	if expired != 1 {
		t.Fatalf("expected one expired grant, got %d", expired)
	}

	var reloaded models.CreditGrant
	if err := db.First(&reloaded, grant.ID).Error; err != nil {
		t.Fatal(err)
	}
	if reloaded.CreditsRemaining != 0 {
		t.Fatalf("expected expired grant balance to be zero, got %d", reloaded.CreditsRemaining)
	}

	var ledger models.CreditLedger
	if err := db.Where("grant_id = ? AND direction = ?", grant.ID, LedgerDirectionExpiry).First(&ledger).Error; err != nil {
		t.Fatal(err)
	}
	if ledger.Credits != 40 || ledger.BalanceAfter != 0 {
		t.Fatalf("unexpected expiry ledger: %#v", ledger)
	}

	expired, err = service.ExpireCredits()
	if err != nil {
		t.Fatal(err)
	}
	if expired != 0 {
		t.Fatalf("expected second expiry run to be idempotent, got %d", expired)
	}
}

func TestPurgeAnonymousGuestUsageDeletesOnlyExpiredGuestMetadata(t *testing.T) {
	db := setupCreditTestDB(t)
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	service := NewCreditServiceWithClock(db, func() time.Time { return now })

	oldGuestHash := HashUsageKey("old-guest-device")
	recentGuestHash := HashUsageKey("recent-guest-device")
	oldTime := now.Add(-(AnonymousGuestRetention + 24*time.Hour))
	recentTime := now.Add(-24 * time.Hour)
	oldDate := oldTime.Format("2006-01-02")
	recentDate := recentTime.Format("2006-01-02")

	user := models.User{UUID: "retention-user", Username: "retention_user"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}

	oldGrant := models.CreditGrant{
		GuestDeviceIDHash: oldGuestHash,
		Source:            GrantSourceFreeTrial,
		CreditsGranted:    GuestTrialCredits,
		CreditsRemaining:  0,
		ValidFrom:         oldTime.Add(-TrialDuration),
		ExpiresAt:         &oldTime,
	}
	recentGrant := models.CreditGrant{
		GuestDeviceIDHash: recentGuestHash,
		Source:            GrantSourceFreeTrial,
		CreditsGranted:    GuestTrialCredits,
		CreditsRemaining:  GuestTrialCredits,
		ValidFrom:         recentTime,
	}
	userGrant := models.CreditGrant{
		UserID:           &user.ID,
		Source:           GrantSourceFreeTrial,
		CreditsGranted:   LoggedInFreeTrialCredits,
		CreditsRemaining: LoggedInFreeTrialCredits,
		ValidFrom:        oldTime,
		ExpiresAt:        &oldTime,
	}
	if err := db.Create(&oldGrant).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&recentGrant).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&userGrant).Error; err != nil {
		t.Fatal(err)
	}

	oldUsage := models.AIUsageEvent{
		GuestDeviceIDHash: oldGuestHash,
		RequestID:         "old-guest-usage",
		ActionCode:        string(ai.ActionTransactionParseText),
		InputKind:         "text",
		Status:            UsageStatusSucceeded,
		EstimatedCredits:  5,
		ReservedCredits:   5,
		FinalCredits:      5,
		StartedAt:         oldTime,
	}
	recentUsage := models.AIUsageEvent{
		GuestDeviceIDHash: recentGuestHash,
		RequestID:         "recent-guest-usage",
		ActionCode:        string(ai.ActionTransactionParseText),
		InputKind:         "text",
		Status:            UsageStatusSucceeded,
		EstimatedCredits:  5,
		ReservedCredits:   5,
		FinalCredits:      5,
		StartedAt:         recentTime,
	}
	userUsage := models.AIUsageEvent{
		UserID:           &user.ID,
		RequestID:        "user-usage",
		ActionCode:       string(ai.ActionTransactionParseText),
		InputKind:        "text",
		Status:           UsageStatusSucceeded,
		EstimatedCredits: 5,
		ReservedCredits:  5,
		FinalCredits:     5,
		StartedAt:        oldTime,
	}
	if err := db.Create(&oldUsage).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&recentUsage).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&userUsage).Error; err != nil {
		t.Fatal(err)
	}

	ledgerRows := []models.CreditLedger{
		{GuestDeviceIDHash: oldGuestHash, GrantID: &oldGrant.ID, AIUsageEventID: &oldUsage.ID, Direction: LedgerDirectionDebit, Credits: 5, BalanceAfter: 295, ReasonCode: ReasonReservationDebit, IdempotencyKey: "old-guest-ledger", CreatedAt: oldTime},
		{GuestDeviceIDHash: recentGuestHash, GrantID: &recentGrant.ID, AIUsageEventID: &recentUsage.ID, Direction: LedgerDirectionDebit, Credits: 5, BalanceAfter: 295, ReasonCode: ReasonReservationDebit, IdempotencyKey: "recent-guest-ledger", CreatedAt: recentTime},
		{UserID: &user.ID, GrantID: &userGrant.ID, AIUsageEventID: &userUsage.ID, Direction: LedgerDirectionDebit, Credits: 5, BalanceAfter: 995, ReasonCode: ReasonReservationDebit, IdempotencyKey: "user-ledger", CreatedAt: oldTime},
	}
	if err := db.Create(&ledgerRows).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.AIUsageLimitEvent{GuestDeviceIDHash: oldGuestHash, ActionCode: string(ai.ActionTransactionParseText), Reason: AllowanceDailyLimitReached, CreatedAt: oldTime}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.AIUsageLimitEvent{GuestDeviceIDHash: recentGuestHash, ActionCode: string(ai.ActionTransactionParseText), Reason: AllowanceDailyLimitReached, CreatedAt: recentTime}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.AIUsageLimitEvent{UserID: &user.ID, ActionCode: string(ai.ActionTransactionParseText), Reason: AllowanceDailyLimitReached, CreatedAt: oldTime}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.DailyCreditUsage{GuestDeviceIDHash: oldGuestHash, UsageDate: oldDate, CreditsUsed: 5}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.DailyCreditUsage{GuestDeviceIDHash: recentGuestHash, UsageDate: recentDate, CreditsUsed: 5}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.DailyCreditUsage{UserID: &user.ID, UsageDate: oldDate, CreditsUsed: 5}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.GuestUsageKey{GuestDeviceIDHash: oldGuestHash, IPHash: HashUsageKey("203.0.113.1"), FirstSeenAt: oldTime, LastSeenAt: oldTime, TrialGrantID: &oldGrant.ID}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.GuestUsageKey{GuestDeviceIDHash: recentGuestHash, IPHash: HashUsageKey("203.0.113.2"), FirstSeenAt: recentTime, LastSeenAt: recentTime, TrialGrantID: &recentGrant.ID}).Error; err != nil {
		t.Fatal(err)
	}

	result, err := service.PurgeAnonymousGuestUsage(AnonymousGuestRetention)
	if err != nil {
		t.Fatal(err)
	}
	if result.CreditLedger != 1 || result.AIUsageEvents != 1 || result.AIUsageLimitEvents != 1 ||
		result.DailyCreditUsage != 1 || result.GuestUsageKeys != 1 || result.CreditGrants != 1 {
		t.Fatalf("unexpected purge result: %#v", result)
	}

	assertGuestRetentionCount(t, db, &models.CreditLedger{}, oldGuestHash, 0)
	assertGuestRetentionCount(t, db, &models.AIUsageEvent{}, oldGuestHash, 0)
	assertGuestRetentionCount(t, db, &models.AIUsageLimitEvent{}, oldGuestHash, 0)
	assertGuestRetentionCount(t, db, &models.DailyCreditUsage{}, oldGuestHash, 0)
	assertGuestRetentionCount(t, db, &models.GuestUsageKey{}, oldGuestHash, 0)
	assertGuestRetentionCount(t, db, &models.CreditGrant{}, oldGuestHash, 0)

	assertGuestRetentionCount(t, db, &models.CreditLedger{}, recentGuestHash, 1)
	assertGuestRetentionCount(t, db, &models.AIUsageEvent{}, recentGuestHash, 1)
	assertGuestRetentionCount(t, db, &models.AIUsageLimitEvent{}, recentGuestHash, 1)
	assertGuestRetentionCount(t, db, &models.DailyCreditUsage{}, recentGuestHash, 1)
	assertGuestRetentionCount(t, db, &models.GuestUsageKey{}, recentGuestHash, 1)
	assertGuestRetentionCount(t, db, &models.CreditGrant{}, recentGuestHash, 1)

	assertUserRetentionCount(t, db, &models.CreditLedger{}, user.ID, 1)
	assertUserRetentionCount(t, db, &models.AIUsageEvent{}, user.ID, 1)
	assertUserRetentionCount(t, db, &models.AIUsageLimitEvent{}, user.ID, 1)
	assertUserRetentionCount(t, db, &models.DailyCreditUsage{}, user.ID, 1)
	assertUserRetentionCount(t, db, &models.CreditGrant{}, user.ID, 1)
}

func TestPurgeAnonymousGuestUsageRejectsInvalidRetention(t *testing.T) {
	db := setupCreditTestDB(t)
	service := NewCreditService(db)

	if _, err := service.PurgeAnonymousGuestUsage(0); err == nil {
		t.Fatal("expected non-positive retention to be rejected")
	}
}

func assertGuestRetentionCount(t *testing.T, db *gorm.DB, model any, guestHash string, want int64) {
	t.Helper()

	var count int64
	if err := db.Model(model).Where("guest_device_id_hash = ?", guestHash).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("unexpected guest retention count for %T and %s: got %d, want %d", model, guestHash, count, want)
	}
}

func assertUserRetentionCount(t *testing.T, db *gorm.DB, model any, userID uint, want int64) {
	t.Helper()

	var count int64
	if err := db.Model(model).Where("user_id = ?", userID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("unexpected user retention count for %T and user %d: got %d, want %d", model, userID, count, want)
	}
}

func TestCheckAllowanceAllowsFreeTextParse(t *testing.T) {
	db := setupCreditTestDB(t)
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	service := NewCreditServiceWithClock(db, func() time.Time { return now })

	user := createCreditTestUser(t, db)
	if _, _, err := service.EnsureLoggedInFreeTrialGrant(user.ID); err != nil {
		t.Fatal(err)
	}

	allowance, err := service.CheckAllowance(SubjectForUser(user.ID), ai.ActionTransactionParseText)
	if err != nil {
		t.Fatal(err)
	}
	if !allowance.Allowed || allowance.Reason != AllowanceAllowed {
		t.Fatalf("expected allowance, got %#v", allowance)
	}
	if allowance.RequiredCredits != 5 || allowance.AvailableCredits != LoggedInFreeTrialCredits || allowance.DailyRemaining != LoggedInFreeDailyLimit {
		t.Fatalf("unexpected allowance details: %#v", allowance)
	}
}

func TestCheckAllowanceDeniesDailyLimit(t *testing.T) {
	db := setupCreditTestDB(t)
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	service := NewCreditServiceWithClock(db, func() time.Time { return now })

	user := createCreditTestUser(t, db)
	if _, _, err := service.EnsureLoggedInFreeTrialGrant(user.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.DailyCreditUsage{
		UserID:      &user.ID,
		UsageDate:   now.Format("2006-01-02"),
		CreditsUsed: LoggedInFreeDailyLimit - 2,
	}).Error; err != nil {
		t.Fatal(err)
	}

	allowance, err := service.CheckAllowance(SubjectForUser(user.ID), ai.ActionTransactionParseText)
	if err != nil {
		t.Fatal(err)
	}
	if allowance.Allowed || allowance.Reason != AllowanceDailyLimitReached {
		t.Fatalf("expected daily limit denial, got %#v", allowance)
	}
}

func TestCheckAllowanceSelectsGuestTrialLimits(t *testing.T) {
	db := setupCreditTestDB(t)
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	service := NewCreditServiceWithClock(db, func() time.Time { return now })

	grant, _, err := service.EnsureGuestTrialGrant("guest-device-allowance", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	subject := SubjectForGuestHash(grant.GuestDeviceIDHash)

	allowance, err := service.CheckAllowance(subject, ai.ActionTransactionParseText)
	if err != nil {
		t.Fatal(err)
	}
	if !allowance.Allowed || allowance.Reason != AllowanceAllowed {
		t.Fatalf("expected guest text allowance, got %#v", allowance)
	}
	if allowance.RequiredCredits != 5 ||
		allowance.AvailableCredits != GuestTrialCredits ||
		allowance.DailyLimit != GuestDailyLimit ||
		allowance.DailyRemaining != GuestDailyLimit ||
		allowance.PlanCode != "" ||
		allowance.PaidPlanActive {
		t.Fatalf("unexpected guest allowance details: %#v", allowance)
	}
}

func TestCheckAllowanceDeniesGuestForNonGuestAction(t *testing.T) {
	db := setupCreditTestDB(t)
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	service := NewCreditServiceWithClock(db, func() time.Time { return now })

	grant, _, err := service.EnsureGuestTrialGrant("guest-device-medium-voice", "203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	allowance, err := service.CheckAllowance(SubjectForGuestHash(grant.GuestDeviceIDHash), ai.ActionTransactionParseVoiceMedium)
	if err != nil {
		t.Fatal(err)
	}
	if allowance.Allowed || allowance.Reason != AllowanceGuestNotAllowed {
		t.Fatalf("expected guest-not-allowed denial, got %#v", allowance)
	}
	if allowance.RequiredCredits != 18 || allowance.DailyLimit != 0 || allowance.AvailableCredits != 0 {
		t.Fatalf("guest-not-allowed should stop before limit/balance lookups, got %#v", allowance)
	}
}

func TestReserveCreditsDebitsGrantDailyUsageAndIsIdempotent(t *testing.T) {
	db := setupCreditTestDB(t)
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	service := NewCreditServiceWithClock(db, func() time.Time { return now })

	user := createCreditTestUser(t, db)
	grant, _, err := service.EnsureLoggedInFreeTrialGrant(user.ID)
	if err != nil {
		t.Fatal(err)
	}

	event, allowance, err := service.ReserveCredits(SubjectForUser(user.ID), ai.ActionTransactionParseText, "parse-1")
	if err != nil {
		t.Fatal(err)
	}
	if !allowance.Allowed || event.ID == 0 || event.Status != UsageStatusReserved || event.ReservedCredits != 5 {
		t.Fatalf("unexpected reservation: event=%#v allowance=%#v", event, allowance)
	}

	var reloaded models.CreditGrant
	if err := db.First(&reloaded, grant.ID).Error; err != nil {
		t.Fatal(err)
	}
	if reloaded.CreditsRemaining != LoggedInFreeTrialCredits-5 {
		t.Fatalf("expected grant debit to 995, got %d", reloaded.CreditsRemaining)
	}
	if got := dailyUsageForTest(t, db, SubjectForUser(user.ID), now); got != 5 {
		t.Fatalf("expected daily usage 5, got %d", got)
	}

	replayed, _, err := service.ReserveCredits(SubjectForUser(user.ID), ai.ActionTransactionParseText, "parse-1")
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ID != event.ID {
		t.Fatalf("expected idempotent event %d, got %d", event.ID, replayed.ID)
	}
	if got := dailyUsageForTest(t, db, SubjectForUser(user.ID), now); got != 5 {
		t.Fatalf("expected idempotent daily usage to stay 5, got %d", got)
	}
}

func TestReserveCreditsConsumesMultipleGrants(t *testing.T) {
	db := setupCreditTestDB(t)
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	service := NewCreditServiceWithClock(db, func() time.Time { return now })

	user := createCreditTestUser(t, db)
	firstExpiry := now.Add(24 * time.Hour)
	secondExpiry := now.Add(48 * time.Hour)
	first := models.CreditGrant{UserID: &user.ID, Source: "promo", CreditsGranted: 3, CreditsRemaining: 3, ValidFrom: now.Add(-time.Hour), ExpiresAt: &firstExpiry}
	second := models.CreditGrant{UserID: &user.ID, Source: "promo", CreditsGranted: 10, CreditsRemaining: 10, ValidFrom: now.Add(-time.Hour), ExpiresAt: &secondExpiry}
	if err := db.Create(&first).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&second).Error; err != nil {
		t.Fatal(err)
	}

	event, _, err := service.ReserveCredits(SubjectForUser(user.ID), ai.ActionTransactionParseText, "multi-grant")
	if err != nil {
		t.Fatal(err)
	}
	if event.ReservedCredits != 5 {
		t.Fatalf("expected 5 reserved credits, got %d", event.ReservedCredits)
	}
	if err := db.First(&first, first.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&second, second.ID).Error; err != nil {
		t.Fatal(err)
	}
	if first.CreditsRemaining != 0 || second.CreditsRemaining != 8 {
		t.Fatalf("expected grants to be consumed by expiry order, got first=%d second=%d", first.CreditsRemaining, second.CreditsRemaining)
	}
}

func TestCancelReservationRefundsCreditsAndDailyUsage(t *testing.T) {
	db := setupCreditTestDB(t)
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	service := NewCreditServiceWithClock(db, func() time.Time { return now })

	user := createCreditTestUser(t, db)
	grant, _, err := service.EnsureLoggedInFreeTrialGrant(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	event, _, err := service.ReserveCredits(SubjectForUser(user.ID), ai.ActionTransactionParseText, "cancel-me")
	if err != nil {
		t.Fatal(err)
	}

	cancelled, err := service.CancelReservationWithStatus(event.ID, UsageStatusFailedBeforeProvider)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != UsageStatusFailedBeforeProvider || cancelled.FinalCredits != 0 {
		t.Fatalf("unexpected cancelled event: %#v", cancelled)
	}
	var reloaded models.CreditGrant
	if err := db.First(&reloaded, grant.ID).Error; err != nil {
		t.Fatal(err)
	}
	if reloaded.CreditsRemaining != LoggedInFreeTrialCredits {
		t.Fatalf("expected full refund, got %d", reloaded.CreditsRemaining)
	}
	if got := dailyUsageForTest(t, db, SubjectForUser(user.ID), now); got != 0 {
		t.Fatalf("expected daily usage refund to 0, got %d", got)
	}
	assertLedgerDirectionCount(t, db, event.ID, LedgerDirectionRefund, 1)
}

func TestFinalizeUsageRefundsLowerFinalCredits(t *testing.T) {
	db := setupCreditTestDB(t)
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	service := NewCreditServiceWithClock(db, func() time.Time { return now })

	user := createCreditTestUser(t, db)
	grant, _, err := service.EnsureLoggedInFreeTrialGrant(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	event, _, err := service.ReserveCredits(SubjectForUser(user.ID), ai.ActionTransactionParseVoiceShort, "voice-1")
	if err != nil {
		t.Fatal(err)
	}
	finalCredits := 8
	finalized, err := service.FinalizeUsage(event.ID, ProviderUsage{FinalCredits: &finalCredits})
	if err != nil {
		t.Fatal(err)
	}
	if finalized.Status != UsageStatusSucceeded || finalized.FinalCredits != 8 {
		t.Fatalf("unexpected finalized event: %#v", finalized)
	}
	var reloaded models.CreditGrant
	if err := db.First(&reloaded, grant.ID).Error; err != nil {
		t.Fatal(err)
	}
	if reloaded.CreditsRemaining != LoggedInFreeTrialCredits-8 {
		t.Fatalf("expected final cost to be 8, remaining=%d", reloaded.CreditsRemaining)
	}
	if got := dailyUsageForTest(t, db, SubjectForUser(user.ID), now); got != 8 {
		t.Fatalf("expected daily usage 8, got %d", got)
	}
	assertLedgerDirectionCount(t, db, event.ID, LedgerDirectionRefund, 1)

	replayed, err := service.FinalizeUsage(event.ID, ProviderUsage{FinalCredits: &finalCredits})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.FinalCredits != finalized.FinalCredits {
		t.Fatalf("repeated finalization changed final credits: %#v", replayed)
	}
	assertLedgerDirectionCount(t, db, event.ID, LedgerDirectionRefund, 1)
}

func TestFinalizeUsageDebitsExtraCreditsUpToActionMax(t *testing.T) {
	db := setupCreditTestDB(t)
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	service := NewCreditServiceWithClock(db, func() time.Time { return now })

	user := createCreditTestUser(t, db)
	grant, _, err := service.EnsureLoggedInFreeTrialGrant(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	event, _, err := service.ReserveCredits(SubjectForUser(user.ID), ai.ActionTransactionParseText, "text-extra")
	if err != nil {
		t.Fatal(err)
	}

	finalCredits := 999
	finalized, err := service.FinalizeUsage(event.ID, ProviderUsage{FinalCredits: &finalCredits})
	if err != nil {
		t.Fatal(err)
	}
	if finalized.FinalCredits != 10 {
		t.Fatalf("expected text parse final credits to clamp to max 10, got %#v", finalized)
	}
	var reloaded models.CreditGrant
	if err := db.First(&reloaded, grant.ID).Error; err != nil {
		t.Fatal(err)
	}
	if reloaded.CreditsRemaining != LoggedInFreeTrialCredits-10 {
		t.Fatalf("expected grant to debit max final credits, remaining=%d", reloaded.CreditsRemaining)
	}
	if got := dailyUsageForTest(t, db, SubjectForUser(user.ID), now); got != 10 {
		t.Fatalf("expected daily usage to include extra final debit, got %d", got)
	}
	assertLedgerDirectionCount(t, db, event.ID, LedgerDirectionDebit, 2)
}

func TestFinalizeUsageKeepsReservedCreditsWhenExtraDebitWouldExceedDailyLimit(t *testing.T) {
	db := setupCreditTestDB(t)
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	service := NewCreditServiceWithClock(db, func() time.Time { return now })

	user := createCreditTestUser(t, db)
	grant, _, err := service.EnsureLoggedInFreeTrialGrant(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.DailyCreditUsage{
		UserID:      &user.ID,
		UsageDate:   now.Format("2006-01-02"),
		CreditsUsed: LoggedInFreeDailyLimit - 5,
	}).Error; err != nil {
		t.Fatal(err)
	}
	event, _, err := service.ReserveCredits(SubjectForUser(user.ID), ai.ActionTransactionParseText, "text-no-extra")
	if err != nil {
		t.Fatal(err)
	}

	finalCredits := 10
	finalized, err := service.FinalizeUsage(event.ID, ProviderUsage{FinalCredits: &finalCredits})
	if err != nil {
		t.Fatal(err)
	}
	if finalized.FinalCredits != event.ReservedCredits {
		t.Fatalf("expected final credits to remain reserved amount when daily cap has no extra room, got %#v", finalized)
	}
	var reloaded models.CreditGrant
	if err := db.First(&reloaded, grant.ID).Error; err != nil {
		t.Fatal(err)
	}
	if reloaded.CreditsRemaining != LoggedInFreeTrialCredits-event.ReservedCredits {
		t.Fatalf("expected no extra grant debit, remaining=%d", reloaded.CreditsRemaining)
	}
	if got := dailyUsageForTest(t, db, SubjectForUser(user.ID), now); got != LoggedInFreeDailyLimit {
		t.Fatalf("expected daily usage to stay at limit, got %d", got)
	}
	assertLedgerDirectionCount(t, db, event.ID, LedgerDirectionDebit, 1)
}

func TestFinalizeUsageCombinesLLMAndTranscriptionCostPricing(t *testing.T) {
	db := setupCreditTestDB(t)
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	service := NewCreditServiceWithClock(db, func() time.Time { return now })

	if err := db.Create(&[]models.AIModelPricing{
		{
			Provider:             "openai",
			Model:                "gpt-4o-mini",
			Operation:            "llm",
			InputTokenUSDMicros:  2,
			OutputTokenUSDMicros: 6,
			RequestUSDMicros:     10,
			Active:               true,
		},
		{
			Provider:             "openai",
			Model:                "gpt-4o-mini-transcribe",
			Operation:            "transcription",
			AudioMinuteUSDMicros: 1000,
			RequestUSDMicros:     20,
			Active:               true,
		},
	}).Error; err != nil {
		t.Fatal(err)
	}
	user := createCreditTestUser(t, db)
	if _, _, err := service.EnsureLoggedInFreeTrialGrant(user.ID); err != nil {
		t.Fatal(err)
	}
	event, _, err := service.ReserveCredits(SubjectForUser(user.ID), ai.ActionTransactionParseVoiceShort, "voice-cost")
	if err != nil {
		t.Fatal(err)
	}

	promptTokens := 100
	completionTokens := 20
	audioDurationMs := 90000
	finalized, err := service.FinalizeUsage(event.ID, ProviderUsage{
		Provider:          "openai",
		Model:             "gpt-4o-mini",
		SecondaryProvider: "openai",
		SecondaryModel:    "gpt-4o-mini-transcribe",
		PromptTokens:      &promptTokens,
		CompletionTokens:  &completionTokens,
		AudioDurationMs:   &audioDurationMs,
	})
	if err != nil {
		t.Fatal(err)
	}
	if finalized.EstimatedCostUSDMicros != 1850 {
		t.Fatalf("estimated cost = %d, want llm 330 + transcription 1520", finalized.EstimatedCostUSDMicros)
	}
}

func TestFinalizeUsageRecordsFailedAfterProviderUsage(t *testing.T) {
	db := setupCreditTestDB(t)
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	service := NewCreditServiceWithClock(db, func() time.Time { return now })

	user := createCreditTestUser(t, db)
	if _, _, err := service.EnsureLoggedInFreeTrialGrant(user.ID); err != nil {
		t.Fatal(err)
	}
	event, _, err := service.ReserveCredits(SubjectForUser(user.ID), ai.ActionTransactionParseText, "failed-after-provider")
	if err != nil {
		t.Fatal(err)
	}
	promptTokens := 100
	completionTokens := 20
	actualCost := int64(12)
	finalized, err := service.FinalizeUsage(event.ID, ProviderUsage{
		Status:              UsageStatusFailedAfterProvider,
		Provider:            "openai",
		Model:               "gpt-4o-mini",
		ActualCostUSDMicros: &actualCost,
		PromptTokens:        &promptTokens,
		CompletionTokens:    &completionTokens,
		ErrorCode:           "schema_invalid",
	})
	if err != nil {
		t.Fatal(err)
	}
	if finalized.Status != UsageStatusFailedAfterProvider || finalized.FinalCredits != 5 || finalized.ErrorCode != "schema_invalid" {
		t.Fatalf("unexpected finalized failure: %#v", finalized)
	}
	if finalized.PromptTokens == nil || *finalized.PromptTokens != 100 || finalized.CompletionTokens == nil || *finalized.CompletionTokens != 20 {
		t.Fatalf("expected token usage to be stored: %#v", finalized)
	}
}

func TestPaidPlanDailyLimitOverridesFreeLimit(t *testing.T) {
	db := setupCreditTestDB(t)
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	service := NewCreditServiceWithClock(db, func() time.Time { return now })

	user := createCreditTestUser(t, db)
	plan := models.Plan{Code: "monthly", Name: "Monthly", BillingInterval: "monthly", IncludedCredits: 3000, DailyCreditLimit: 200}
	if err := db.Create(&plan).Error; err != nil {
		t.Fatal(err)
	}
	subscription := models.UserSubscription{
		UserID:             user.ID,
		PlanID:             plan.ID,
		Status:             "active",
		CurrentPeriodStart: now.Add(-time.Hour),
		CurrentPeriodEnd:   now.Add(30 * 24 * time.Hour),
	}
	if err := db.Create(&subscription).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.GrantSubscriptionPeriod(subscription, plan.IncludedCredits, subscription.CurrentPeriodStart, subscription.CurrentPeriodEnd); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.DailyCreditUsage{
		UserID:      &user.ID,
		UsageDate:   now.Format("2006-01-02"),
		CreditsUsed: 60,
	}).Error; err != nil {
		t.Fatal(err)
	}

	allowance, err := service.CheckAllowance(SubjectForUser(user.ID), ai.ActionTransactionParseText)
	if err != nil {
		t.Fatal(err)
	}
	if !allowance.Allowed || !allowance.PaidPlanActive || allowance.DailyLimit != 200 || allowance.PlanCode != "monthly" {
		t.Fatalf("expected paid allowance, got %#v", allowance)
	}
}

func TestCheckAllowanceInactiveSubscriptionFallsBackToFreeLimits(t *testing.T) {
	db := setupCreditTestDB(t)
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	service := NewCreditServiceWithClock(db, func() time.Time { return now })

	user := createCreditTestUser(t, db)
	if _, _, err := service.EnsureLoggedInFreeTrialGrant(user.ID); err != nil {
		t.Fatal(err)
	}
	plan := models.Plan{Code: "monthly", Name: "Monthly", BillingInterval: "monthly", IncludedCredits: 3000, DailyCreditLimit: 200}
	if err := db.Create(&plan).Error; err != nil {
		t.Fatal(err)
	}
	subscription := models.UserSubscription{
		UserID:             user.ID,
		PlanID:             plan.ID,
		Status:             "cancelled",
		CurrentPeriodStart: now.Add(-30 * 24 * time.Hour),
		CurrentPeriodEnd:   now.Add(30 * 24 * time.Hour),
	}
	if err := db.Create(&subscription).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.GrantSubscriptionPeriod(subscription, plan.IncludedCredits, subscription.CurrentPeriodStart, subscription.CurrentPeriodEnd); err != nil {
		t.Fatal(err)
	}

	allowance, err := service.CheckAllowance(SubjectForUser(user.ID), ai.ActionTransactionParseText)
	if err != nil {
		t.Fatal(err)
	}
	if !allowance.Allowed || allowance.PaidPlanActive || allowance.PlanCode != "" {
		t.Fatalf("expected free allowance fallback, got %#v", allowance)
	}
	if allowance.DailyLimit != LoggedInFreeDailyLimit || allowance.DailyRemaining != LoggedInFreeDailyLimit {
		t.Fatalf("expected free daily limits after inactive subscription, got %#v", allowance)
	}
	if allowance.AvailableCredits != LoggedInFreeTrialCredits+plan.IncludedCredits {
		t.Fatalf("expected all unexpired credits to remain usable, got %#v", allowance)
	}
}

func TestParallelReservationsDoNotOverspendGrant(t *testing.T) {
	db := setupCreditTestDB(t)
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	service := NewCreditServiceWithClock(db, func() time.Time { return now })

	user := createCreditTestUser(t, db)
	expiresAt := now.Add(24 * time.Hour)
	grant := models.CreditGrant{
		UserID:           &user.ID,
		Source:           "promo",
		CreditsGranted:   5,
		CreditsRemaining: 5,
		ValidFrom:        now.Add(-time.Hour),
		ExpiresAt:        &expiresAt,
	}
	if err := db.Create(&grant).Error; err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, key := range []string{"parallel-a", "parallel-b"} {
		wg.Add(1)
		go func(idempotencyKey string) {
			defer wg.Done()
			_, _, err := service.ReserveCredits(SubjectForUser(user.ID), ai.ActionTransactionParseText, idempotencyKey)
			errs <- err
		}(key)
	}
	wg.Wait()
	close(errs)

	successes := 0
	denials := 0
	for err := range errs {
		if err == nil {
			successes++
			continue
		}
		if errors.Is(err, ErrAllowanceDenied) {
			denials++
			continue
		}
		t.Fatalf("unexpected reservation error: %v", err)
	}
	if successes != 1 || denials != 1 {
		t.Fatalf("expected one success and one denial, got successes=%d denials=%d", successes, denials)
	}

	var reloaded models.CreditGrant
	if err := db.First(&reloaded, grant.ID).Error; err != nil {
		t.Fatal(err)
	}
	if reloaded.CreditsRemaining != 0 {
		t.Fatalf("expected grant balance to stop at zero, got %d", reloaded.CreditsRemaining)
	}
	if got := dailyUsageForTest(t, db, SubjectForUser(user.ID), now); got != 5 {
		t.Fatalf("expected daily usage 5, got %d", got)
	}
}

func TestParallelReservationsDoNotBypassDailyLimit(t *testing.T) {
	db := setupCreditTestDB(t)
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	service := NewCreditServiceWithClock(db, func() time.Time { return now })

	user := createCreditTestUser(t, db)
	if _, _, err := service.EnsureLoggedInFreeTrialGrant(user.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.DailyCreditUsage{
		UserID:      &user.ID,
		UsageDate:   now.Format("2006-01-02"),
		CreditsUsed: LoggedInFreeDailyLimit - 5,
	}).Error; err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, key := range []string{"daily-a", "daily-b"} {
		wg.Add(1)
		go func(idempotencyKey string) {
			defer wg.Done()
			_, _, err := service.ReserveCredits(SubjectForUser(user.ID), ai.ActionTransactionParseText, idempotencyKey)
			errs <- err
		}(key)
	}
	wg.Wait()
	close(errs)

	successes := 0
	denials := 0
	for err := range errs {
		if err == nil {
			successes++
			continue
		}
		if errors.Is(err, ErrAllowanceDenied) {
			denials++
			continue
		}
		t.Fatalf("unexpected reservation error: %v", err)
	}
	if successes != 1 || denials != 1 {
		t.Fatalf("expected one success and one denial, got successes=%d denials=%d", successes, denials)
	}
	if got := dailyUsageForTest(t, db, SubjectForUser(user.ID), now); got != LoggedInFreeDailyLimit {
		t.Fatalf("expected daily usage to stop at limit %d, got %d", LoggedInFreeDailyLimit, got)
	}
}

func createCreditTestUser(t *testing.T, db *gorm.DB) models.User {
	t.Helper()
	user := models.User{UUID: t.Name() + "-uuid", Username: t.Name() + "_user"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	return user
}

func dailyUsageForTest(t *testing.T, db *gorm.DB, subject CreditSubject, now time.Time) int {
	t.Helper()
	var usage models.DailyCreditUsage
	query := db.Where("usage_date = ?", now.Format("2006-01-02"))
	if subject.UserID != nil {
		query = query.Where("user_id = ?", *subject.UserID)
	} else {
		query = query.Where("guest_device_id_hash = ?", subject.GuestDeviceIDHash)
	}
	if err := query.First(&usage).Error; err == gorm.ErrRecordNotFound {
		return 0
	} else if err != nil {
		t.Fatal(err)
	}
	return usage.CreditsUsed
}

func assertLedgerDirectionCount(t *testing.T, db *gorm.DB, eventID uint, direction string, want int64) {
	t.Helper()
	var count int64
	if err := db.Model(&models.CreditLedger{}).
		Where("ai_usage_event_id = ? AND direction = ?", eventID, direction).
		Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("expected %d %s ledger rows, got %d", want, direction, count)
	}
}

// The regression this guards: every usage row landed with null tokens, so
// estimateUsageCostUSDMicros never found any to price and fell through to its
// flat per-credit fallback. With the counts supplied, the same code prices the
// call from the model's own rates.
func TestFinalizeUsageRecordsTokensAndPricesFromThem(t *testing.T) {
	db := setupCreditTestDB(t)
	now := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	service := NewCreditServiceWithClock(db, func() time.Time { return now })

	if err := db.Create(&models.AIModelPricing{
		Provider:             "openai",
		Model:                "gpt-4o-mini",
		Operation:            "llm",
		InputTokenUSDMicros:  2,
		OutputTokenUSDMicros: 8,
		RequestUSDMicros:     50,
		Active:               true,
	}).Error; err != nil {
		t.Fatal(err)
	}

	user := createCreditTestUser(t, db)
	if _, _, err := service.EnsureLoggedInFreeTrialGrant(user.ID); err != nil {
		t.Fatal(err)
	}
	event, _, err := service.ReserveCredits(SubjectForUser(user.ID), ai.ActionTransactionParseText, "parse-tokens-1")
	if err != nil {
		t.Fatal(err)
	}

	prompt, completion, total := 4387, 132, 4519
	finalized, err := service.FinalizeUsage(event.ID, ProviderUsage{
		Provider:         "openai",
		Model:            "gpt-4o-mini",
		PromptTokens:     &prompt,
		CompletionTokens: &completion,
		TotalTokens:      &total,
	})
	if err != nil {
		t.Fatal(err)
	}

	if finalized.PromptTokens == nil || *finalized.PromptTokens != prompt {
		t.Fatalf("prompt tokens = %v", finalized.PromptTokens)
	}
	if finalized.CompletionTokens == nil || *finalized.CompletionTokens != completion {
		t.Fatalf("completion tokens = %v", finalized.CompletionTokens)
	}
	if finalized.TotalTokens == nil || *finalized.TotalTokens != total {
		t.Fatalf("total tokens = %v", finalized.TotalTokens)
	}

	// 4387*2 + 132*8 + 50 request fee.
	const wantCost = int64(4387*2 + 132*8 + 50)
	if finalized.EstimatedCostUSDMicros != wantCost {
		t.Fatalf("cost = %d, want %d priced from the tokens", finalized.EstimatedCostUSDMicros, wantCost)
	}
}

// Without counts the cost must still resolve — to the per-credit fallback —
// rather than becoming zero.
func TestFinalizeUsageLeavesTokensNullWhenProviderReportedNone(t *testing.T) {
	db := setupCreditTestDB(t)
	now := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	service := NewCreditServiceWithClock(db, func() time.Time { return now })

	user := createCreditTestUser(t, db)
	if _, _, err := service.EnsureLoggedInFreeTrialGrant(user.ID); err != nil {
		t.Fatal(err)
	}
	event, _, err := service.ReserveCredits(SubjectForUser(user.ID), ai.ActionTransactionParseText, "parse-tokens-2")
	if err != nil {
		t.Fatal(err)
	}

	finalized, err := service.FinalizeUsage(event.ID, ProviderUsage{Provider: "openai", Model: "gpt-4o-mini"})
	if err != nil {
		t.Fatal(err)
	}
	if finalized.PromptTokens != nil || finalized.CompletionTokens != nil || finalized.TotalTokens != nil {
		t.Fatalf("unreported tokens should stay null, got %v/%v/%v",
			finalized.PromptTokens, finalized.CompletionTokens, finalized.TotalTokens)
	}
	if finalized.EstimatedCostUSDMicros <= 0 {
		t.Fatalf("expected the per-credit fallback to price the call, got %d", finalized.EstimatedCostUSDMicros)
	}
}
