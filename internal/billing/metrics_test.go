package billing

import (
	"errors"
	"testing"
	"time"

	"finnri/internal/ai"
	"finnri/internal/models"
)

func TestFinalizeUsageEstimatesFallbackCostAndMetrics(t *testing.T) {
	db := setupCreditTestDB(t)
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	service := NewCreditServiceWithClock(db, func() time.Time { return now })

	user := models.User{UUID: "metrics-user", Username: "metrics_user"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.EnsureLoggedInFreeTrialGrant(user.ID); err != nil {
		t.Fatal(err)
	}
	event, _, err := service.ReserveCredits(SubjectForUser(user.ID), ai.ActionTransactionParseText, "metrics-1")
	if err != nil {
		t.Fatal(err)
	}
	finalized, err := service.FinalizeUsage(event.ID, ProviderUsage{
		Status:   UsageStatusSucceeded,
		Provider: "openai",
		Model:    "gpt-4o-mini",
	})
	if err != nil {
		t.Fatal(err)
	}
	if finalized.EstimatedCostUSDMicros != int64(finalized.FinalCredits)*DefaultCreditCostUSDMicros {
		t.Fatalf("estimated cost = %d, want fallback based on credits", finalized.EstimatedCostUSDMicros)
	}

	metrics, err := BuildAIMetrics(db, now.Add(-time.Hour), now.Add(time.Hour), AlertConfig{
		DailyCostUSDMicros:         1,
		AbuseDailyCreditsThreshold: 5,
		FreeCostPerUserUSDMicros:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if metrics.TotalEvents != 1 || metrics.SuccessfulEvents != 1 || metrics.TotalCreditsCharged != 5 {
		t.Fatalf("unexpected metrics: %#v", metrics)
	}
	if metrics.ParseSuccessRate != 1 || metrics.AverageCreditsCharged != 5 {
		t.Fatalf("unexpected rate/average: %#v", metrics)
	}
	if metrics.MaxSubjectDailyCredits != 5 || len(metrics.Alerts) != 3 {
		t.Fatalf("expected threshold alerts, got metrics %#v", metrics)
	}
}

func TestReserveCreditsRecordsAllowanceDenials(t *testing.T) {
	db := setupCreditTestDB(t)
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	service := NewCreditServiceWithClock(db, func() time.Time { return now })

	user := models.User{UUID: "denied-user", Username: "denied_user"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	_, allowance, err := service.ReserveCredits(SubjectForUser(user.ID), ai.ActionTransactionParseText, "denied-1")
	if !errors.Is(err, ErrAllowanceDenied) {
		t.Fatalf("expected allowance denial, got allowance=%#v err=%v", allowance, err)
	}
	var limitEvent models.AIUsageLimitEvent
	if err := db.Where("user_id = ?", user.ID).First(&limitEvent).Error; err != nil {
		t.Fatal(err)
	}
	if limitEvent.Reason != AllowanceInsufficientCredits || limitEvent.RequiredCredits != 5 {
		t.Fatalf("unexpected limit event: %#v", limitEvent)
	}

	metrics, err := BuildAIMetrics(db, now.Add(-time.Hour), now.Add(time.Hour), AlertConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if metrics.UsersHitTotalCreditCap != 1 {
		t.Fatalf("expected one total cap hit, got %#v", metrics)
	}
}

func TestBuildAIMetricsSeparatesPaidFreeAndGuestCostPerUser(t *testing.T) {
	db := setupCreditTestDB(t)
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)

	paidPlan := models.Plan{Code: "monthly", Name: "Monthly", BillingInterval: "monthly", IncludedCredits: 3000, DailyCreditLimit: 200}
	if err := db.Create(&paidPlan).Error; err != nil {
		t.Fatal(err)
	}
	paidUser := models.User{UUID: "paid-metrics-user", Username: "paid_metrics_user"}
	freeUser := models.User{UUID: "free-metrics-user", Username: "free_metrics_user"}
	if err := db.Create(&paidUser).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&freeUser).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.UserSubscription{
		UserID:             paidUser.ID,
		PlanID:             paidPlan.ID,
		Status:             "active",
		CurrentPeriodStart: now.AddDate(0, 0, -1),
		CurrentPeriodEnd:   now.AddDate(0, 1, 0),
	}).Error; err != nil {
		t.Fatal(err)
	}
	events := []models.AIUsageEvent{
		{
			UserID:                 &paidUser.ID,
			RequestID:              "paid-cost-1",
			ActionCode:             "transaction_parse_text",
			InputKind:              "text",
			Status:                 UsageStatusSucceeded,
			EstimatedCredits:       5,
			ReservedCredits:        5,
			FinalCredits:           5,
			EstimatedCostUSDMicros: 900,
			StartedAt:              now,
		},
		{
			UserID:                 &freeUser.ID,
			RequestID:              "free-cost-1",
			ActionCode:             "transaction_parse_text",
			InputKind:              "text",
			Status:                 UsageStatusSucceeded,
			EstimatedCredits:       5,
			ReservedCredits:        5,
			FinalCredits:           5,
			EstimatedCostUSDMicros: 300,
			StartedAt:              now,
		},
		{
			GuestDeviceIDHash:      "guestcost1234567890123456789012345678901234567890123456789012",
			RequestID:              "guest-cost-1",
			ActionCode:             "transaction_parse_text",
			InputKind:              "text",
			Status:                 UsageStatusSucceeded,
			EstimatedCredits:       5,
			ReservedCredits:        5,
			FinalCredits:           5,
			EstimatedCostUSDMicros: 600,
			StartedAt:              now,
		},
	}
	if err := db.Create(&events).Error; err != nil {
		t.Fatal(err)
	}

	metrics, err := BuildAIMetrics(db, now.Add(-time.Hour), now.Add(time.Hour), AlertConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if metrics.EstimatedCostUSDMicros != 1800 || metrics.CostPerActiveUserUSDMicros != 600 {
		t.Fatalf("unexpected active user cost metrics: %#v", metrics)
	}
	if metrics.PaidUsers != 1 || metrics.CostPerPaidUserUSDMicros != 900 {
		t.Fatalf("unexpected paid user cost metrics: %#v", metrics)
	}
	if metrics.FreeUsers != 1 || metrics.FreeCostPerUserUSDMicros != 300 {
		t.Fatalf("unexpected free user cost metrics: %#v", metrics)
	}
}

func TestModelPricingOverridesCostEstimate(t *testing.T) {
	db := setupCreditTestDB(t)
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	service := NewCreditServiceWithClock(db, func() time.Time { return now })

	if err := db.Create(&models.AIModelPricing{
		Provider:             "openai",
		Model:                "gpt-4o-mini",
		Operation:            "llm",
		InputTokenUSDMicros:  2,
		OutputTokenUSDMicros: 6,
		RequestUSDMicros:     10,
		Active:               true,
	}).Error; err != nil {
		t.Fatal(err)
	}
	user := models.User{UUID: "pricing-user", Username: "pricing_user"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.EnsureLoggedInFreeTrialGrant(user.ID); err != nil {
		t.Fatal(err)
	}
	event, _, err := service.ReserveCredits(SubjectForUser(user.ID), ai.ActionTransactionParseText, "pricing-1")
	if err != nil {
		t.Fatal(err)
	}
	promptTokens := 100
	completionTokens := 20
	finalized, err := service.FinalizeUsage(event.ID, ProviderUsage{
		Status:           UsageStatusSucceeded,
		Provider:         "openai",
		Model:            "gpt-4o-mini",
		PromptTokens:     &promptTokens,
		CompletionTokens: &completionTokens,
	})
	if err != nil {
		t.Fatal(err)
	}
	if finalized.EstimatedCostUSDMicros != 330 {
		t.Fatalf("estimated cost = %d, want 330", finalized.EstimatedCostUSDMicros)
	}
}

// A month-long admin view must not add a user's days together: 4 days of 30
// credits is 120 in the window, but no single day reaches a 100-credit limit.
func TestAbuseAlertUsesBusiestSingleDayNotWindowTotal(t *testing.T) {
	db := setupCreditTestDB(t)
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)

	user := models.User{UUID: "spread-user", Username: "spread_user"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	guest := "spreadguest00000000000000000000000000000000000000000000000000000"
	newEvent := func(id string, userID *uint, guestHash string, credits int, at time.Time) models.AIUsageEvent {
		return models.AIUsageEvent{
			UserID:            userID,
			GuestDeviceIDHash: guestHash,
			RequestID:         id,
			ActionCode:        "transaction_parse_text",
			InputKind:         "text",
			Status:            UsageStatusSucceeded,
			EstimatedCredits:  credits,
			ReservedCredits:   credits,
			FinalCredits:      credits,
			StartedAt:         at,
		}
	}
	events := []models.AIUsageEvent{}
	for day := 0; day < 4; day++ {
		events = append(events, newEvent("spread-"+string(rune('a'+day)), &user.ID, "", 30, now.AddDate(0, 0, -day)))
	}
	// The guest's busiest day: two reads on the same UTC date.
	events = append(events,
		newEvent("guest-1", nil, guest, 25, now.AddDate(0, 0, -10)),
		newEvent("guest-2", nil, guest, 25, now.AddDate(0, 0, -10).Add(time.Hour)),
	)
	if err := db.Create(&events).Error; err != nil {
		t.Fatal(err)
	}

	cfg := AlertConfig{AbuseDailyCreditsThreshold: 100}
	metrics, err := BuildAIMetrics(db, now.AddDate(0, 0, -30), now.Add(time.Hour), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if metrics.TotalCreditsCharged != 170 {
		t.Fatalf("total credits = %d, want 170", metrics.TotalCreditsCharged)
	}
	if metrics.MaxSubjectDailyCredits != 50 {
		t.Fatalf("max subject daily credits = %d, want 50 (the guest's busiest day)", metrics.MaxSubjectDailyCredits)
	}
	for _, alert := range metrics.Alerts {
		if alert.Code == "ai_abuse_credit_threshold" {
			t.Fatalf("abuse alert fired for spread-out usage: %#v", metrics.Alerts)
		}
	}

	// A real one-day spike still fires.
	if err := db.Create(&[]models.AIUsageEvent{
		newEvent("spike-1", &user.ID, "", 50, now.Add(-2*time.Hour)),
		newEvent("spike-2", &user.ID, "", 50, now.Add(-time.Hour)),
	}).Error; err != nil {
		t.Fatal(err)
	}
	metrics, err = BuildAIMetrics(db, now.AddDate(0, 0, -30), now.Add(time.Hour), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if metrics.MaxSubjectDailyCredits != 130 {
		t.Fatalf("max subject daily credits = %d, want 130", metrics.MaxSubjectDailyCredits)
	}
	fired := false
	for _, alert := range metrics.Alerts {
		fired = fired || alert.Code == "ai_abuse_credit_threshold"
	}
	if !fired {
		t.Fatalf("abuse alert did not fire for a 130-credit day: %#v", metrics.Alerts)
	}
}
