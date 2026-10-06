package http

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"finnri/internal/config"
	"finnri/internal/database"
	"finnri/internal/models"
)

func withLaunchOfferStartedAt(start string) func(*Server, *config.Config) {
	return func(_ *Server, cfg *config.Config) { cfg.LaunchOfferStartsAt = start }
}

func yesterday() string { return time.Now().AddDate(0, 0, -1).Format("2006-01-02") }

func TestLaunchOfferPriceIsAtLeast75PercentOffInWholeRupees(t *testing.T) {
	cases := map[int64]int64{7900: 1900, 14900: 3700, 32900: 8200, 79900: 19900}
	for price, want := range cases {
		if got := launchOfferPriceMinor(price); got != want {
			t.Errorf("launch price of %d = %d, want %d", price, got, want)
		}
	}
}

func TestLaunchOfferWindowNeedsAnExplicitStart(t *testing.T) {
	if _, _, ok := launchOfferWindow(""); ok {
		t.Fatal("no start configured must mean no offer")
	}
	if _, _, ok := launchOfferWindow("next week"); ok {
		t.Fatal("an unreadable start must mean no offer")
	}
	start, end, ok := launchOfferWindow("2026-11-01")
	if !ok || end.Sub(start) != 90*24*time.Hour {
		t.Fatalf("expected a 90-day window, got %v → %v", start, end)
	}
	server := &Server{cfg: &config.Config{LaunchOfferStartsAt: "2026-11-01"}}
	before, _ := server.currentLaunchOffer(start.Add(-time.Hour))
	after, _ := server.currentLaunchOffer(end.Add(time.Hour))
	if before.Active || after.Active {
		t.Fatal("the offer runs only inside its window")
	}
}

func TestLaunchOfferChargesTheLaunchPriceOncePerPerson(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	fake := newFakeRazorpay(t)
	router := smokeRouter(t, withRazorpay(fake), withLaunchOfferStartedAt(yesterday()))
	user, token := createBillingTestUserSession(t)
	seedPurchasablePlan(t, "monthly", "monthly", 14900, 3600)
	seedPurchasablePlan(t, "quarterly", "quarterly", 32900, 11000)

	plans := performJSONRequest[struct {
		Plans []billingPlanResponse `json:"plans"`
	}](t, router, http.MethodGet, "/v1/billing/plans", "", nil, http.StatusOK)
	var monthly *billingPlanResponse
	for index := range plans.Plans {
		if plans.Plans[index].Code == "monthly" {
			monthly = &plans.Plans[index]
		}
	}
	if monthly == nil || monthly.Offer == nil || monthly.Offer.PriceMinor != 3700 ||
		monthly.Offer.OriginalPriceMinor != 14900 || monthly.Offer.PercentOff != 75 {
		t.Fatalf("plans must advertise the launch price: %+v", monthly)
	}
	if monthly.Offer.SpotsLeft != nil {
		t.Fatal("spots left stay hidden until fewer than 50 remain")
	}

	order := startCheckout(t, router, token, "monthly", http.StatusCreated)
	if order.AmountMinor != 3700 || fake.lastAmount != 3700 || order.OriginalAmountMinor != 14900 ||
		order.OfferLabel != launchOfferLabel {
		t.Fatalf("checkout must charge ₹37: %+v (provider %d)", order, fake.lastAmount)
	}
	payment := loadPayment(t, order.OrderID)
	if payment.PromotionCode != launchOfferCode || payment.OriginalAmountMinor != 14900 {
		t.Fatalf("payment must record the offer: %+v", payment)
	}

	// Razorpay settles the discounted order; the webhook grants it as usual.
	response := postWebhook(t, router, captureBody(order.OrderID, "pay_launch_1", 3700), "evt_launch_1")
	if response.Code != http.StatusOK {
		t.Fatalf("webhook status = %d, body = %s", response.Code, response.Body.String())
	}
	if _, found, _ := currentUserSubscription(user.ID); !found {
		t.Fatal("a paid launch order must grant the plan")
	}

	status := performJSONRequest[billingStatusResponse](t, router, http.MethodGet, "/v1/billing/status", token, nil, http.StatusOK)
	if status.LaunchOffer == nil || !status.LaunchOffer.Active || status.LaunchOffer.Eligible {
		t.Fatalf("after buying, the offer is still on but no longer theirs: %+v", status.LaunchOffer)
	}

	second := startCheckout(t, router, token, "quarterly", http.StatusCreated)
	if second.AmountMinor != 32900 || second.OfferLabel != "" {
		t.Fatalf("a second purchase is full price: %+v", second)
	}
}

func TestLaunchOfferEndsAtFiveHundredBuyers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	fake := newFakeRazorpay(t)
	router := smokeRouter(t, withRazorpay(fake), withLaunchOfferStartedAt(yesterday()))
	plan := seedPurchasablePlan(t, "monthly", "monthly", 14900, 3600)

	buyers := make([]models.User, 0, launchOfferMaxBuyers)
	for index := 0; index < launchOfferMaxBuyers; index++ {
		buyers = append(buyers, models.User{UUID: fmt.Sprintf("launch-%d", index), Username: fmt.Sprintf("launch_%d", index)})
	}
	if err := database.DB.CreateInBatches(&buyers, 100).Error; err != nil {
		t.Fatal(err)
	}
	payments := make([]models.Payment, 0, len(buyers))
	for index, buyer := range buyers {
		payments = append(payments, models.Payment{
			UserID: buyer.ID, PlanID: plan.ID, Provider: "razorpay",
			ProviderOrderID: fmt.Sprintf("order_launch_%d", index), Status: models.PaymentStatusCaptured,
			AmountMinor: 3700, Currency: "INR", PromotionCode: launchOfferCode, OriginalAmountMinor: 14900,
		})
	}
	if err := database.DB.CreateInBatches(&payments, 100).Error; err != nil {
		t.Fatal(err)
	}

	_, token := createBillingTestUserSession(t)
	order := startCheckout(t, router, token, "monthly", http.StatusCreated)
	if order.AmountMinor != 14900 {
		t.Fatalf("buyer 501 pays full price, got %d", order.AmountMinor)
	}
	status := performJSONRequest[billingStatusResponse](t, router, http.MethodGet, "/v1/billing/status", token, nil, http.StatusOK)
	if status.LaunchOffer != nil {
		t.Fatalf("a sold-out offer is not shown: %+v", status.LaunchOffer)
	}
}

func TestNoLaunchOfferUntilItIsSwitchedOn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	fake := newFakeRazorpay(t)
	router := smokeRouter(t, withRazorpay(fake))
	_, token := createBillingTestUserSession(t)
	seedPurchasablePlan(t, "monthly", "monthly", 14900, 3600)

	order := startCheckout(t, router, token, "monthly", http.StatusCreated)
	if order.AmountMinor != 14900 || order.OfferLabel != "" {
		t.Fatalf("without LAUNCH_OFFER_STARTS_AT nothing is discounted: %+v", order)
	}
}

func TestLaunchOfferShowsSpotsOnlyWhenFewRemain(t *testing.T) {
	plan := billingPlanResponse{BillingInterval: "monthly", PriceMinor: int64Pointer(14900)}
	plenty := launchOfferForPlan(launchOfferState{Active: true, SpotsLeft: 320}, plan)
	few := launchOfferForPlan(launchOfferState{Active: true, SpotsLeft: 37}, plan)
	if plenty.SpotsLeft != nil || few.SpotsLeft == nil || *few.SpotsLeft != 37 {
		t.Fatalf("spots: plenty=%v few=%v", plenty.SpotsLeft, few.SpotsLeft)
	}
	lifetime := launchOfferForPlan(launchOfferState{Active: true, SpotsLeft: 320},
		billingPlanResponse{BillingInterval: "lifetime_quote", PriceMinor: int64Pointer(499900)})
	if lifetime != nil {
		t.Fatal("the lifetime quote is not part of the launch offer")
	}
}
