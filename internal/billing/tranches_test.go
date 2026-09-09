package billing

import (
	"testing"
	"time"

	"finnri/internal/models"
)

func TestQuarterlyCreditsArriveInThreeMonthlyTranchesWithOneRollover(t *testing.T) {
	db := setupCreditTestDB(t)
	user := createCreditTestUser(t, db)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	plan := models.Plan{Code: "quarterly", Name: "Quarterly", BillingInterval: "quarterly", IncludedCredits: 11000}
	if err := db.Create(&plan).Error; err != nil {
		t.Fatal(err)
	}
	subscription := models.UserSubscription{
		UserID: user.ID, PlanID: plan.ID, Plan: plan, Status: "active",
		CurrentPeriodStart: start, CurrentPeriodEnd: start.Add(90 * 24 * time.Hour),
	}
	if err := db.Create(&subscription).Error; err != nil {
		t.Fatal(err)
	}

	service := NewCreditService(db)
	created, err := service.SyncSubscriptionTranches(subscription, start.Add(65*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if created != 3 {
		t.Fatalf("expected three due tranches, got %d", created)
	}
	created, err = service.SyncSubscriptionTranches(subscription, start.Add(65*24*time.Hour))
	if err != nil || created != 0 {
		t.Fatalf("expected idempotent resync, created=%d err=%v", created, err)
	}

	var grants []models.CreditGrant
	if err := db.Where("subscription_id = ?", subscription.ID).Order("valid_from").Find(&grants).Error; err != nil {
		t.Fatal(err)
	}
	if len(grants) != 3 {
		t.Fatalf("expected three grants, got %#v", grants)
	}
	for index, grant := range grants {
		wantStart := start.Add(time.Duration(index*SubscriptionTrancheDays) * 24 * time.Hour)
		wantExpiry := wantStart.Add(60 * 24 * time.Hour)
		if grant.CreditsGranted != QuarterlyTrancheCredits || !grant.ValidFrom.Equal(wantStart) || grant.ExpiresAt == nil || !grant.ExpiresAt.Equal(wantExpiry) {
			t.Fatalf("unexpected quarterly tranche %d: %#v", index, grant)
		}
	}

	var usable int
	now := start.Add(65 * 24 * time.Hour)
	if err := db.Model(&models.CreditGrant{}).
		Where("subscription_id = ? AND valid_from <= ? AND expires_at > ?", subscription.ID, now, now).
		Select("COALESCE(SUM(credits_remaining), 0)").Scan(&usable).Error; err != nil {
		t.Fatal(err)
	}
	if usable != QuarterlyTrancheCredits*2 {
		t.Fatalf("expected only current plus one rollover tranche, got %d", usable)
	}
}

func TestYearlyCreditsAreTwelveFourThousandCreditTranches(t *testing.T) {
	db := setupCreditTestDB(t)
	user := createCreditTestUser(t, db)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	plan := models.Plan{Code: "yearly", Name: "Yearly", BillingInterval: "yearly", IncludedCredits: 48000}
	if err := db.Create(&plan).Error; err != nil {
		t.Fatal(err)
	}
	subscription := models.UserSubscription{
		UserID: user.ID, PlanID: plan.ID, Plan: plan, Status: "active",
		CurrentPeriodStart: start, CurrentPeriodEnd: start.Add(360 * 24 * time.Hour),
	}
	if err := db.Create(&subscription).Error; err != nil {
		t.Fatal(err)
	}

	created, err := NewCreditService(db).SyncSubscriptionTranches(subscription, start.Add(359*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if created != 12 {
		t.Fatalf("expected twelve yearly tranches, got %d", created)
	}
	var total int
	if err := db.Model(&models.CreditGrant{}).Where("subscription_id = ?", subscription.ID).
		Select("COALESCE(SUM(credits_granted), 0)").Scan(&total).Error; err != nil {
		t.Fatal(err)
	}
	if total != 48000 {
		t.Fatalf("expected 48,000 credits across yearly tranches, got %d", total)
	}
}

func TestLifetimeCreditsGrantCurrentMonthOnlyWithNoRollover(t *testing.T) {
	db := setupCreditTestDB(t)
	user := createCreditTestUser(t, db)
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	plan := models.Plan{Code: "lifetime_quote", Name: "Lifetime", BillingInterval: "lifetime_quote", IncludedCredits: 5000}
	if err := db.Create(&plan).Error; err != nil {
		t.Fatal(err)
	}
	subscription := models.UserSubscription{
		UserID: user.ID, PlanID: plan.ID, Plan: plan, Status: "active",
		CurrentPeriodStart: start, CurrentPeriodEnd: start.AddDate(50, 0, 0),
	}
	if err := db.Create(&subscription).Error; err != nil {
		t.Fatal(err)
	}

	firstNow := start.Add(400 * 24 * time.Hour)
	service := NewCreditService(db)
	if created, err := service.SyncSubscriptionTranches(subscription, firstNow); err != nil || created != 1 {
		t.Fatalf("expected one current lifetime tranche, created=%d err=%v", created, err)
	}
	secondNow := firstNow.Add(31 * 24 * time.Hour)
	if created, err := service.SyncSubscriptionTranches(subscription, secondNow); err != nil || created != 1 {
		t.Fatalf("expected one next lifetime tranche, created=%d err=%v", created, err)
	}

	var grants []models.CreditGrant
	if err := db.Where("subscription_id = ?", subscription.ID).Order("valid_from").Find(&grants).Error; err != nil {
		t.Fatal(err)
	}
	if len(grants) != 2 {
		t.Fatalf("expected no historical lifetime backfill, got %#v", grants)
	}
	for _, grant := range grants {
		if grant.CreditsGranted != LifetimeMonthlyTrancheCredits || grant.ExpiresAt == nil || !grant.ExpiresAt.Equal(grant.ValidFrom.Add(30*24*time.Hour)) {
			t.Fatalf("unexpected lifetime tranche: %#v", grant)
		}
	}
}
