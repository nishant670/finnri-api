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
	for index := range buyers {
		payments = append(payments, models.Payment{
			UserID: &buyers[index].ID, PlanID: plan.ID, Provider: "razorpay",
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

// The review's case: Monthly then Quarterly at the launch price, neither paid
// yet. Only the newest order may be paid; switching plan keeps the discount.
func TestLaunchOfferReservesTheFirstOpenCheckoutPerPerson(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	fake := newFakeRazorpay(t)
	router := smokeRouter(t, withRazorpay(fake), withLaunchOfferStartedAt(yesterday()))
	user, token := createBillingTestUserSession(t)
	seedPurchasablePlan(t, "monthly", "monthly", 14900, 3600)
	seedPurchasablePlan(t, "quarterly", "quarterly", 32900, 11000)

	monthly := startCheckout(t, router, token, "monthly", http.StatusCreated)
	again := startCheckout(t, router, token, "monthly", http.StatusOK)
	if again.OrderID != monthly.OrderID {
		t.Fatalf("a second tap on the same plan reuses the open order: %s vs %s", again.OrderID, monthly.OrderID)
	}

	quarterly := startCheckout(t, router, token, "quarterly", http.StatusCreated)
	if quarterly.AmountMinor != 8200 || quarterly.OfferLabel != launchOfferLabel {
		t.Fatalf("switching plan keeps the launch price: %+v", quarterly)
	}
	if first := loadPayment(t, monthly.OrderID); !isSupersededLaunchOrder(first) {
		t.Fatalf("the earlier discounted order must be superseded: %+v", first)
	}
	if fresh := loadPayment(t, quarterly.OrderID); fresh.Status != models.PaymentStatusCreated {
		t.Fatalf("the newest order stays payable: %+v", fresh)
	}

	// The pay page refuses the superseded order, and still opens the new one.
	performJSONRequest[map[string]any](t, router, http.MethodGet, "/v1/billing/checkout/"+monthly.OrderID, "", nil, http.StatusGone)
	performJSONRequest[map[string]any](t, router, http.MethodGet, "/v1/billing/checkout/"+quarterly.OrderID, "", nil, http.StatusOK)

	// Once the newest is paid, nothing else is discounted.
	if response := postWebhook(t, router, captureBody(quarterly.OrderID, "pay_launch_q", 8200), "evt_launch_q"); response.Code != http.StatusOK {
		t.Fatalf("webhook status = %d, body = %s", response.Code, response.Body.String())
	}
	if _, found, _ := currentUserSubscription(user.ID); !found {
		t.Fatal("the paid launch order grants the plan")
	}
	later := startCheckout(t, router, token, "monthly", http.StatusCreated)
	if later.AmountMinor != 14900 || later.OfferLabel != "" {
		t.Fatalf("after one launch purchase, checkout is full price: %+v", later)
	}
}

// Open launch-priced orders hold places under the cap, so 500 people holding
// orders cannot become more than 500 launch-price buyers. The holder of the
// last place is not crowded out by their own order.
func TestLaunchOfferCapCountsOpenOrders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	fake := newFakeRazorpay(t)
	router := smokeRouter(t, withRazorpay(fake), withLaunchOfferStartedAt(yesterday()))
	plan := seedPurchasablePlan(t, "monthly", "monthly", 14900, 3600)

	buyers := make([]models.User, 0, launchOfferMaxBuyers-1)
	for index := 0; index < launchOfferMaxBuyers-1; index++ {
		buyers = append(buyers, models.User{UUID: fmt.Sprintf("cap-%d", index), Username: fmt.Sprintf("cap_%d", index)})
	}
	if err := database.DB.CreateInBatches(&buyers, 100).Error; err != nil {
		t.Fatal(err)
	}
	payments := make([]models.Payment, 0, len(buyers))
	for index := range buyers {
		payments = append(payments, models.Payment{
			UserID: &buyers[index].ID, PlanID: plan.ID, Provider: "razorpay",
			ProviderOrderID: fmt.Sprintf("order_cap_%d", index), Status: models.PaymentStatusCaptured,
			AmountMinor: 3700, Currency: "INR", PromotionCode: launchOfferCode, OriginalAmountMinor: 14900,
		})
	}
	if err := database.DB.CreateInBatches(&payments, 100).Error; err != nil {
		t.Fatal(err)
	}

	_, holderToken := createBillingTestUserSession(t)
	held := startCheckout(t, router, holderToken, "monthly", http.StatusCreated)
	if held.AmountMinor != 3700 {
		t.Fatalf("the 500th place is still open: %+v", held)
	}
	if again := startCheckout(t, router, holderToken, "monthly", http.StatusOK); again.OrderID != held.OrderID {
		t.Fatalf("the holder keeps their own place: %+v", again)
	}

	_, latecomerToken := createBillingTestUserSession(t)
	if late := startCheckout(t, router, latecomerToken, "monthly", http.StatusCreated); late.AmountMinor != 14900 {
		t.Fatalf("with every place held, the next person pays full price: %+v", late)
	}
}
