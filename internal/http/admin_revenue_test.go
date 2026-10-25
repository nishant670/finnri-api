package http

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"finnri/internal/database"
	"finnri/internal/models"

	"github.com/gin-gonic/gin"
)

func TestAdminRevenueReportsCashCollectedNetOfRefunds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, ownerToken := createAdminSessionForTest(t, models.AdminRoleOwner)
	monthly := seedPurchasablePlan(t, "monthly", "monthly", 14900, 3600)
	weekly := seedPurchasablePlan(t, "weekly_pass", "weekly", 7900, 800)
	yearly := seedPurchasablePlan(t, "yearly", "yearly", 79900, 48000)
	first, _ := createBillingTestUserSession(t)
	second, _ := createBillingTestUserSession(t)
	third, _ := createBillingTestUserSession(t)

	now := time.Now().UTC()
	at := func(ago time.Duration) *time.Time {
		value := now.Add(-ago)
		return &value
	}
	day := 24 * time.Hour
	payments := []models.Payment{
		// Full price.
		{UserID: first.ID, PlanID: monthly.ID, Status: models.PaymentStatusCaptured, AmountMinor: 14900, CapturedAt: at(2 * day)},
		// Launch price: counts what was paid, and the discount separately.
		{UserID: second.ID, PlanID: monthly.ID, Status: models.PaymentStatusCaptured, AmountMinor: 3725, OriginalAmountMinor: 14900, PromotionCode: launchOfferCode, CapturedAt: at(day)},
		// Fully refunded: collected, then all of it given back.
		{UserID: first.ID, PlanID: weekly.ID, Status: models.PaymentStatusRefunded, AmountMinor: 7900, AmountRefundedMinor: 7900, CapturedAt: at(3 * day), RefundedAt: at(2 * day)},
		// Partly refunded.
		{UserID: third.ID, PlanID: monthly.ID, Status: models.PaymentStatusCaptured, AmountMinor: 14900, AmountRefundedMinor: 5000, CapturedAt: at(3 * day)},
		// Before the window: lifetime only.
		{UserID: third.ID, PlanID: yearly.ID, Status: models.PaymentStatusCaptured, AmountMinor: 79900, CapturedAt: at(60 * day)},
		// Never paid: no money moved.
		{UserID: second.ID, PlanID: yearly.ID, Status: models.PaymentStatusCreated, AmountMinor: 79900},
		{UserID: second.ID, PlanID: yearly.ID, Status: models.PaymentStatusFailed, AmountMinor: 79900},
	}
	for index := range payments {
		payments[index].Provider = "razorpay"
		payments[index].ProviderOrderID = "order_cash_" + generateUUID()[:8]
		payments[index].Currency = "INR"
		if err := database.DB.Create(&payments[index]).Error; err != nil {
			t.Fatal(err)
		}
	}

	query := url.Values{"start": {now.Add(-30 * day).Format(time.RFC3339)}, "end": {now.Add(time.Minute).Format(time.RFC3339)}}
	revenue := performJSONRequest[adminRevenueSnapshot](t, router, http.MethodGet, "/v1/admin/revenue?"+query.Encode(), ownerToken, nil, http.StatusOK)
	cash := revenue.Cash

	if cash.CollectedMinor != 41425 || cash.RefundedMinor != 12900 || cash.NetMinor != 28525 {
		t.Fatalf("window cash = collected %d refunded %d net %d, want 41425 / 12900 / 28525", cash.CollectedMinor, cash.RefundedMinor, cash.NetMinor)
	}
	if cash.Payments != 4 || cash.PayingUsers != 3 {
		t.Fatalf("window should hold 4 payments from 3 people, got %d from %d", cash.Payments, cash.PayingUsers)
	}
	if cash.DiscountMinor != 11175 {
		t.Fatalf("launch discount = %d, want 11175", cash.DiscountMinor)
	}
	if cash.LifetimeCollectedMinor != 121325 || cash.LifetimeNetMinor != 108425 {
		t.Fatalf("lifetime cash = collected %d net %d, want 121325 / 108425", cash.LifetimeCollectedMinor, cash.LifetimeNetMinor)
	}
	if got := cash.ByPlan["monthly"]; got.Payments != 3 || got.NetMinor != 28525 {
		t.Fatalf("monthly cash = %#v", got)
	}
	if got := cash.ByPlan["weekly_pass"]; got.Payments != 1 || got.NetMinor != 0 {
		t.Fatalf("a fully refunded weekly pass should net to zero: %#v", got)
	}

	var trendTotal int64
	for _, point := range revenue.Trend {
		trendTotal += point.CashNetMinor
	}
	if trendTotal != cash.NetMinor {
		t.Fatalf("daily cash should add up to the window's net: %d vs %d", trendTotal, cash.NetMinor)
	}
}
