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
