package http

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"finnri/internal/billing"
	"finnri/internal/config"
	"finnri/internal/database"
	"finnri/internal/models"
	"finnri/internal/payments"
)

const (
	testKeyID         = "rzp_test_key"
	testKeySecret     = "rzp_test_secret"
	testWebhookSecret = "whsec_test_secret"
)

// fakeRazorpay stands in for the provider's orders API so a checkout can be
// driven end to end without a network.
type fakeRazorpay struct {
	server     *httptest.Server
	orderCount int64
	lastAmount int64
}

func newFakeRazorpay(t *testing.T) *fakeRazorpay {
	t.Helper()
	fake := &fakeRazorpay{}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Amount   int64  `json:"amount"`
			Currency string `json:"currency"`
			Receipt  string `json:"receipt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		atomic.StoreInt64(&fake.lastAmount, payload.Amount)
		id := atomic.AddInt64(&fake.orderCount, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"order_fake_%d","entity":"order","amount":%d,"currency":%q,"status":"created","receipt":%q}`,
			id, payload.Amount, payload.Currency, payload.Receipt)
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeRazorpay) orders() int64 { return atomic.LoadInt64(&f.orderCount) }

// withRazorpay configures the router as a deployment that has real keys.
func withRazorpay(fake *fakeRazorpay) func(*Server, *config.Config) {
	return func(s *Server, cfg *config.Config) {
		cfg.RazorpayKeyID = testKeyID
		cfg.RazorpayKeySecret = testKeySecret
		cfg.RazorpayWebhookSecret = testWebhookSecret
		cfg.RazorpayBaseURL = fake.server.URL
		cfg.WebBaseURL = "https://finnri.example"
		s.payments = payments.NewClient(payments.RazorpayConfig{
			KeyID:         testKeyID,
			KeySecret:     testKeySecret,
			WebhookSecret: testWebhookSecret,
			BaseURL:       fake.server.URL,
		})
	}
}

func seedPurchasablePlan(t *testing.T, code, interval string, priceMinor int64, credits int) models.Plan {
	t.Helper()
	plan := models.Plan{
		Code:             code,
		Name:             strings.ToUpper(code[:1]) + code[1:],
		BillingInterval:  interval,
		PriceMinor:       priceMinor,
		ListPriceMinor:   priceMinor * 3,
		Currency:         "INR",
		IncludedCredits:  credits,
		DailyCreditLimit: 250,
		IsPublic:         true,
		RequiresLogin:    true,
	}
	if err := database.DB.Create(&plan).Error; err != nil {
		t.Fatalf("failed to seed plan: %v", err)
	}
	return plan
}

func signWebhook(body string) string {
	mac := hmac.New(sha256.New, []byte(testWebhookSecret))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

// postWebhook delivers a body with a valid signature unless one is overridden.
func postWebhook(t *testing.T, router *gin.Engine, body, eventID string, signature ...string) *httptest.ResponseRecorder {
	t.Helper()
	sig := signWebhook(body)
	if len(signature) > 0 {
		sig = signature[0]
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/billing/webhook", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Razorpay-Signature", sig)
	if eventID != "" {
		request.Header.Set("X-Razorpay-Event-Id", eventID)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func captureBody(orderID, paymentID string, amountMinor int64) string {
	return fmt.Sprintf(`{"entity":"event","event":"payment.captured","payload":{"payment":{"entity":{"id":%q,"order_id":%q,"status":"captured","amount":%d,"currency":"INR","method":"upi","notes":[]}}}}`,
		paymentID, orderID, amountMinor)
}

func startCheckout(t *testing.T, router *gin.Engine, token, planCode string, expectStatus int) checkoutOrderResponse {
	t.Helper()
	return performJSONRequest[checkoutOrderResponse](
		t, router, http.MethodPost, "/v1/billing/checkout", token,
		map[string]string{"plan_code": planCode}, expectStatus,
	)
}

func creditBalance(t *testing.T, userID uint) int {
	t.Helper()
	summary, err := buildCreditSummary(billing.SubjectForUser(userID), false)
	if err != nil {
		t.Fatalf("failed to read credit summary: %v", err)
	}
	return summary.TotalCreditsRemaining
}

func loadPayment(t *testing.T, orderID string) models.Payment {
	t.Helper()
	var payment models.Payment
	if err := database.DB.Where("provider_order_id = ?", orderID).First(&payment).Error; err != nil {
		t.Fatalf("payment %s not found: %v", orderID, err)
	}
	return payment
}

// ---------------------------------------------------------------------------
// Checkout
// ---------------------------------------------------------------------------

func TestCheckoutEnabledOnlyWhenProviderConfigured(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	fake := newFakeRazorpay(t)
	router := smokeRouter(t, withRazorpay(fake))

	response := performJSONRequest[struct {
		Plans []billingPlanResponse `json:"plans"`
	}](t, router, http.MethodGet, "/v1/billing/plans", "", nil, http.StatusOK)

	monthly := findBillingPlanResponse(response.Plans, "monthly")
	if monthly == nil || !monthly.CheckoutEnabled {
		t.Fatalf("configured provider must enable checkout: %#v", monthly)
	}
	// Lifetime is sold by quote after three paid months, never off a price
	// tag, so configuring a provider must not make it purchasable.
	lifetime := findBillingPlanResponse(response.Plans, "lifetime_quote")
	if lifetime == nil || lifetime.CheckoutEnabled {
		t.Fatalf("lifetime must stay non-purchasable: %#v", lifetime)
	}
}

func TestCheckoutCreatesOrderAndPendingPayment(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	fake := newFakeRazorpay(t)
	router := smokeRouter(t, withRazorpay(fake))
	user, token := createBillingTestUserSession(t)
	plan := seedPurchasablePlan(t, "monthly", "monthly", 14900, 3600)

	order := startCheckout(t, router, token, "monthly", http.StatusCreated)
	if order.OrderID == "" || order.KeyID != testKeyID {
		t.Fatalf("unexpected checkout response: %#v", order)
	}
	// The API secret and webhook secret must never leave the process.
	encoded, _ := json.Marshal(order)
	if strings.Contains(string(encoded), testKeySecret) || strings.Contains(string(encoded), testWebhookSecret) {
		t.Fatalf("checkout response leaked a secret: %s", encoded)
	}
	if order.AmountMinor != 14900 || fake.lastAmount != 14900 {
		t.Fatalf("expected 14900 paise, got response=%d provider=%d", order.AmountMinor, fake.lastAmount)
	}

	payment := loadPayment(t, order.OrderID)
	if payment.Status != models.PaymentStatusCreated {
		t.Fatalf("a new order must be pending, got %q", payment.Status)
	}
	if payment.UserID != user.ID || payment.PlanID != plan.ID {
		t.Fatalf("payment not attributed correctly: %#v", payment)
	}
	// Creating an order grants nothing. Only the webhook does that.
	if _, found, _ := currentUserSubscription(user.ID); found {
		t.Fatal("checkout must not activate a subscription on its own")
	}
}

// TestCheckoutReusesAnOpenOrder covers the double-click: a second attempt at
// the same intent must land on the order that already exists rather than
// opening a second one the person could also pay.
func TestCheckoutReusesAnOpenOrder(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	fake := newFakeRazorpay(t)
	router := smokeRouter(t, withRazorpay(fake))
	_, token := createBillingTestUserSession(t)
	seedPurchasablePlan(t, "monthly", "monthly", 14900, 3600)

	first := startCheckout(t, router, token, "monthly", http.StatusCreated)
	second := startCheckout(t, router, token, "monthly", http.StatusOK)

	if first.OrderID != second.OrderID {
		t.Fatalf("a repeat checkout opened a second order: %s then %s", first.OrderID, second.OrderID)
	}
	if fake.orders() != 1 {
		t.Fatalf("expected one order at the provider, got %d", fake.orders())
	}
}

func TestCheckoutRefusesPlanMissingFromTheDatabase(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	fake := newFakeRazorpay(t)
	router := smokeRouter(t, withRazorpay(fake))
	_, token := createBillingTestUserSession(t)

	// No plan rows seeded: the catalogue falls back to the in-code defaults,
	// which have no id for payments.plan_id to reference.
	response := performJSONRequest[map[string]any](
		t, router, http.MethodPost, "/v1/billing/checkout", token,
		map[string]string{"plan_code": "monthly"}, http.StatusConflict,
	)
	if response["error"] != "plan_not_seeded" {
		t.Fatalf("unexpected response: %#v", response)
	}
	if fake.orders() != 0 {
		t.Fatal("no order should be created against a plan that cannot be recorded")
	}
}

// ---------------------------------------------------------------------------
// Webhook: signature and replay
// ---------------------------------------------------------------------------

func TestWebhookRejectsBadSignature(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	fake := newFakeRazorpay(t)
	router := smokeRouter(t, withRazorpay(fake))
	user, token := createBillingTestUserSession(t)
	seedPurchasablePlan(t, "monthly", "monthly", 14900, 3600)
	order := startCheckout(t, router, token, "monthly", http.StatusCreated)

	body := captureBody(order.OrderID, "pay_forged", 14900)
	for _, signature := range []string{"", "deadbeef", signWebhook(body + " ")} {
		response := postWebhook(t, router, body, "evt_forged", signature)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("signature %q was accepted with %d", signature, response.Code)
		}
	}

	if _, found, _ := currentUserSubscription(user.ID); found {
		t.Fatal("an unsigned webhook granted a subscription")
	}
	if loadPayment(t, order.OrderID).Status != models.PaymentStatusCreated {
		t.Fatal("an unsigned webhook moved the payment")
	}
	// Rejections are recorded: a run of them is the only visible sign of a
	// rotated secret or somebody probing the endpoint.
	var rejected int64
	database.DB.Model(&models.PaymentWebhookEvent{}).Where("status = ?", models.WebhookStatusRejected).Count(&rejected)
	if rejected == 0 {
		t.Fatal("rejected webhooks must be recorded, not silently dropped")
	}
}

// TestWebhookIsIdempotent is the replay guard. Providers redeliver routinely,
// and an attacker can force it by re-sending a body whose signature still
// verifies; neither may buy a second period or a second set of credits.
func TestWebhookIsIdempotent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	fake := newFakeRazorpay(t)
	router := smokeRouter(t, withRazorpay(fake))
	user, token := createBillingTestUserSession(t)
	seedPurchasablePlan(t, "monthly", "monthly", 14900, 3600)
	order := startCheckout(t, router, token, "monthly", http.StatusCreated)

	body := captureBody(order.OrderID, "pay_1", 14900)
	if code := postWebhook(t, router, body, "evt_capture_1").Code; code != http.StatusOK {
		t.Fatalf("first delivery failed with %d", code)
	}
	balanceAfterFirst := creditBalance(t, user.ID)

	// Same event id, delivered again.
	replay := postWebhook(t, router, body, "evt_capture_1")
	if replay.Code != http.StatusOK || !strings.Contains(replay.Body.String(), "duplicate") {
		t.Fatalf("a redelivery must be acknowledged as a duplicate, got %d: %s", replay.Code, replay.Body.String())
	}
	// A different event id carrying the same capture: the payment is already
	// captured, so this must also grant nothing.
	if code := postWebhook(t, router, body, "evt_capture_2").Code; code != http.StatusOK {
		t.Fatalf("second event id failed with %d", code)
	}

	if got := creditBalance(t, user.ID); got != balanceAfterFirst {
		t.Fatalf("replays changed the balance: %d then %d", balanceAfterFirst, got)
	}
	var subscriptions int64
	database.DB.Model(&models.UserSubscription{}).Where("user_id = ?", user.ID).Count(&subscriptions)
	if subscriptions != 1 {
		t.Fatalf("expected exactly one subscription, got %d", subscriptions)
	}
}

func TestWebhookNotConfiguredStaysUnimplemented(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)

	response := postWebhook(t, router, `{"event":"payment.captured"}`, "evt_1")
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501 without provider config, got %d", response.Code)
	}
}

// ---------------------------------------------------------------------------
// Purchase, renewal, refund, failure
// ---------------------------------------------------------------------------

func TestCapturedPaymentActivatesSubscriptionAndGrantsCredits(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	fake := newFakeRazorpay(t)
	router := smokeRouter(t, withRazorpay(fake))
	user, token := createBillingTestUserSession(t)
	plan := seedPurchasablePlan(t, "monthly", "monthly", 14900, 3600)

	before := creditBalance(t, user.ID)
	order := startCheckout(t, router, token, "monthly", http.StatusCreated)
	if code := postWebhook(t, router, captureBody(order.OrderID, "pay_1", 14900), "evt_1").Code; code != http.StatusOK {
		t.Fatalf("capture webhook failed with %d", code)
	}

	subscription, found, err := currentUserSubscription(user.ID)
	if err != nil || !found {
		t.Fatalf("capture did not activate a subscription (err=%v)", err)
	}
	if subscription.Status != "active" || subscription.PlanID != plan.ID {
		t.Fatalf("unexpected subscription: %#v", subscription)
	}
	if subscription.Provider != payments.ProviderRazorpay {
		t.Fatalf("subscription should record its provider, got %q", subscription.Provider)
	}
	// A one-time order carries no mandate, so nothing auto-renews. Saying so
	// keeps the billing status honest instead of implying a recurring charge.
	if !subscription.CancelAtPeriodEnd {
		t.Fatal("a one-time purchase must not claim it will auto-renew")
	}
	gotDays := subscription.CurrentPeriodEnd.Sub(subscription.CurrentPeriodStart).Hours() / 24
	if gotDays < 29.9 || gotDays > 30.1 {
		t.Fatalf("monthly period should span 30 days, got %.2f", gotDays)
	}

	if got := creditBalance(t, user.ID) - before; got != 3600 {
		t.Fatalf("expected 3600 plan credits granted, got %d", got)
	}

	payment := loadPayment(t, order.OrderID)
	if payment.Status != models.PaymentStatusCaptured || payment.ProviderPaymentID != "pay_1" {
		t.Fatalf("payment not marked captured: %#v", payment)
	}
	if payment.SubscriptionID == nil || *payment.SubscriptionID != subscription.ID {
		t.Fatalf("payment must link the subscription it bought: %#v", payment)
	}
	if payment.CapturedAt == nil || payment.Method != "upi" {
		t.Fatalf("capture metadata not recorded: %#v", payment)
	}
}

// TestYearlyPurchaseGrantsOneTrancheNotTheLot pins the tranche policy against
// the payment path: a yearly buyer must not receive all 48,000 credits at once
// and burn them in five months.
func TestYearlyPurchaseGrantsOneTrancheNotTheLot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	fake := newFakeRazorpay(t)
	router := smokeRouter(t, withRazorpay(fake))
	user, token := createBillingTestUserSession(t)
	seedPurchasablePlan(t, "yearly", "yearly", 79900, 48000)

	before := creditBalance(t, user.ID)
	order := startCheckout(t, router, token, "yearly", http.StatusCreated)
	postWebhook(t, router, captureBody(order.OrderID, "pay_year", 79900), "evt_year")

	granted := creditBalance(t, user.ID) - before
	if granted != billing.YearlyTrancheCredits {
		t.Fatalf("a yearly purchase should grant one %d-credit tranche, got %d", billing.YearlyTrancheCredits, granted)
	}
}

// TestRenewalStacksRatherThanOverwrites: buying again while a period is still
// running must queue behind it, not throw away time already paid for.
func TestRenewalStacksRatherThanOverwrites(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	fake := newFakeRazorpay(t)
	router := smokeRouter(t, withRazorpay(fake))
	user, token := createBillingTestUserSession(t)
	seedPurchasablePlan(t, "monthly", "monthly", 14900, 3600)

	first := startCheckout(t, router, token, "monthly", http.StatusCreated)
	postWebhook(t, router, captureBody(first.OrderID, "pay_1", 14900), "evt_1")
	original, _, _ := currentUserSubscription(user.ID)

	// A second purchase needs a distinct order, so move the first one out of
	// the reuse window.
	database.DB.Model(&models.Payment{}).Where("provider_order_id = ?", first.OrderID).
		Update("created_at", time.Now().UTC().Add(-time.Hour))

	second := startCheckout(t, router, token, "monthly", http.StatusCreated)
	if second.OrderID == first.OrderID {
		t.Fatal("renewal reused the captured order")
	}
	postWebhook(t, router, captureBody(second.OrderID, "pay_2", 14900), "evt_2")

	var subscriptions []models.UserSubscription
	database.DB.Where("user_id = ?", user.ID).Order("current_period_start ASC").Find(&subscriptions)
	if len(subscriptions) != 2 {
		t.Fatalf("expected the renewal to add a period, got %d", len(subscriptions))
	}
	// The renewal begins exactly where the first period ends: no gap, and no
	// overlap that would waste a month the person paid for.
	if !subscriptions[1].CurrentPeriodStart.Equal(original.CurrentPeriodEnd) {
		t.Fatalf("renewal should start at %s, started at %s",
			original.CurrentPeriodEnd, subscriptions[1].CurrentPeriodStart)
	}
	// The queued period's credits must not be spendable yet.
	if got := creditBalance(t, user.ID); got > 3600+billing.LoggedInFreeTrialCredits {
		t.Fatalf("a future period's credits are already spendable: %d", got)
	}
}

func TestFullRefundEndsTheSubscription(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	fake := newFakeRazorpay(t)
	router := smokeRouter(t, withRazorpay(fake))
	user, token := createBillingTestUserSession(t)
	seedPurchasablePlan(t, "monthly", "monthly", 14900, 3600)

	order := startCheckout(t, router, token, "monthly", http.StatusCreated)
	postWebhook(t, router, captureBody(order.OrderID, "pay_1", 14900), "evt_1")
	if _, found, _ := currentUserSubscription(user.ID); !found {
		t.Fatal("setup failed: no active subscription to refund")
	}

	refund := `{"event":"refund.processed","payload":{"refund":{"entity":{"id":"rfnd_1","payment_id":"pay_1","amount":14900,"status":"processed"}}}}`
	if code := postWebhook(t, router, refund, "evt_refund").Code; code != http.StatusOK {
		t.Fatalf("refund webhook failed with %d", code)
	}

	if _, found, _ := currentUserSubscription(user.ID); found {
		t.Fatal("a fully refunded payment must not leave an active subscription")
	}
	payment := loadPayment(t, order.OrderID)
	if payment.Status != models.PaymentStatusRefunded || payment.AmountRefundedMinor != 14900 {
		t.Fatalf("refund not recorded on the payment: %#v", payment)
	}
}

// TestPartialRefundLeavesTheSubscriptionRunning: how much of a period a part
// refund buys is a support judgement, not an automatic one.
func TestPartialRefundLeavesTheSubscriptionRunning(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	fake := newFakeRazorpay(t)
	router := smokeRouter(t, withRazorpay(fake))
	user, token := createBillingTestUserSession(t)
	seedPurchasablePlan(t, "monthly", "monthly", 14900, 3600)

	order := startCheckout(t, router, token, "monthly", http.StatusCreated)
	postWebhook(t, router, captureBody(order.OrderID, "pay_1", 14900), "evt_1")

	refund := `{"event":"refund.processed","payload":{"refund":{"entity":{"id":"rfnd_1","payment_id":"pay_1","amount":5000,"status":"processed"}}}}`
	postWebhook(t, router, refund, "evt_refund")

	if _, found, _ := currentUserSubscription(user.ID); !found {
		t.Fatal("a partial refund must not end the subscription")
	}
	payment := loadPayment(t, order.OrderID)
	if payment.Status != models.PaymentStatusCaptured || payment.AmountRefundedMinor != 5000 {
		t.Fatalf("partial refund not recorded correctly: %#v", payment)
	}
}

func TestFailedPaymentDoesNotGrantAnything(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	fake := newFakeRazorpay(t)
	router := smokeRouter(t, withRazorpay(fake))
	user, token := createBillingTestUserSession(t)
	seedPurchasablePlan(t, "monthly", "monthly", 14900, 3600)

	order := startCheckout(t, router, token, "monthly", http.StatusCreated)
	body := fmt.Sprintf(`{"event":"payment.failed","payload":{"payment":{"entity":{"id":"pay_dead","order_id":%q,"status":"failed","amount":14900,"error_code":"BAD_REQUEST_ERROR","error_description":"Payment failed","notes":[]}}}}`, order.OrderID)
	if code := postWebhook(t, router, body, "evt_failed").Code; code != http.StatusOK {
		t.Fatalf("failure webhook returned %d", code)
	}

	payment := loadPayment(t, order.OrderID)
	if payment.Status != models.PaymentStatusFailed || payment.FailureReason != "Payment failed" {
		t.Fatalf("failure not recorded: %#v", payment)
	}
	if _, found, _ := currentUserSubscription(user.ID); found {
		t.Fatal("a failed payment granted a subscription")
	}
}

// TestLateFailureCannotUndoACapture: Razorpay can report a failed attempt on
// an order that a later attempt paid. The capture must stand.
func TestLateFailureCannotUndoACapture(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	fake := newFakeRazorpay(t)
	router := smokeRouter(t, withRazorpay(fake))
	user, token := createBillingTestUserSession(t)
	seedPurchasablePlan(t, "monthly", "monthly", 14900, 3600)

	order := startCheckout(t, router, token, "monthly", http.StatusCreated)
	postWebhook(t, router, captureBody(order.OrderID, "pay_ok", 14900), "evt_ok")

	late := fmt.Sprintf(`{"event":"payment.failed","payload":{"payment":{"entity":{"id":"pay_earlier","order_id":%q,"status":"failed","amount":14900,"error_description":"Card declined","notes":[]}}}}`, order.OrderID)
	postWebhook(t, router, late, "evt_late_failure")

	if loadPayment(t, order.OrderID).Status != models.PaymentStatusCaptured {
		t.Fatal("a late failure event undid a successful capture")
	}
	if _, found, _ := currentUserSubscription(user.ID); !found {
		t.Fatal("a late failure event revoked a paid subscription")
	}
}

// TestUnderpaidCaptureIsRejected is defence in depth. Razorpay enforces the
// order amount at its end, but a capture for less than the price must never
// buy the plan here either.
func TestUnderpaidCaptureIsRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	fake := newFakeRazorpay(t)
	router := smokeRouter(t, withRazorpay(fake))
	user, token := createBillingTestUserSession(t)
	seedPurchasablePlan(t, "yearly", "yearly", 79900, 48000)

	order := startCheckout(t, router, token, "yearly", http.StatusCreated)
	// One rupee for a ₹799 plan.
	postWebhook(t, router, captureBody(order.OrderID, "pay_cheap", 100), "evt_cheap")

	if _, found, _ := currentUserSubscription(user.ID); found {
		t.Fatal("an underpaid capture bought a subscription")
	}
	if loadPayment(t, order.OrderID).Status != models.PaymentStatusCreated {
		t.Fatal("an underpaid capture moved the payment forward")
	}
}

func TestCaptureForUnknownOrderIsAcknowledgedNotRetried(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	fake := newFakeRazorpay(t)
	router := smokeRouter(t, withRazorpay(fake))

	// An order this deployment never created — a stray event from a shared
	// Razorpay account, say. Retrying will not conjure one, so it must be
	// acknowledged rather than looping forever on a 5xx.
	response := postWebhook(t, router, captureBody("order_unknown", "pay_x", 14900), "evt_unknown")
	if response.Code != http.StatusOK {
		t.Fatalf("expected an acknowledgement, got %d", response.Code)
	}
	if !strings.Contains(response.Body.String(), models.WebhookStatusIgnored) {
		t.Fatalf("expected the event to be recorded as ignored: %s", response.Body.String())
	}
}

func TestUnhandledEventIsAcknowledged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	fake := newFakeRazorpay(t)
	router := smokeRouter(t, withRazorpay(fake))

	response := postWebhook(t, router, `{"event":"payment.authorized","payload":{}}`, "evt_auth")
	if response.Code != http.StatusOK {
		t.Fatalf("an unsubscribed event must be acknowledged, got %d", response.Code)
	}
	var event models.PaymentWebhookEvent
	if err := database.DB.Where("event_id = ?", "evt_auth").First(&event).Error; err != nil {
		t.Fatalf("event not recorded: %v", err)
	}
	if event.Status != models.WebhookStatusIgnored {
		t.Fatalf("expected ignored, got %q", event.Status)
	}
}

// ---------------------------------------------------------------------------
// The hosted pay page
// ---------------------------------------------------------------------------

// TestCheckoutHandsBackAHostedPayURL: the app opens the URL the server gives
// it rather than assembling one, so a staging build cannot send somebody to
// production's checkout.
func TestCheckoutHandsBackAHostedPayURL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	fake := newFakeRazorpay(t)
	router := smokeRouter(t, withRazorpay(fake))
	_, token := createBillingTestUserSession(t)
	seedPurchasablePlan(t, "monthly", "monthly", 14900, 3600)

	order := startCheckout(t, router, token, "monthly", http.StatusCreated)
	want := "https://finnri.example/pay?order=" + order.OrderID
	if order.CheckoutURL != want {
		t.Fatalf("expected %q, got %q", want, order.CheckoutURL)
	}
}

func TestCheckoutOmitsPayURLWithoutAWebOrigin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	fake := newFakeRazorpay(t)
	router := smokeRouter(t, func(s *Server, cfg *config.Config) {
		withRazorpay(fake)(s, cfg)
		cfg.WebBaseURL = ""
	})
	_, token := createBillingTestUserSession(t)
	seedPurchasablePlan(t, "monthly", "monthly", 14900, 3600)

	// No origin configured means no URL, rather than a broken one the app
	// would open into an error page.
	if order := startCheckout(t, router, token, "monthly", http.StatusCreated); order.CheckoutURL != "" {
		t.Fatalf("expected no checkout URL, got %q", order.CheckoutURL)
	}
}

// TestPublicOrderLookupServesThePayPage covers the endpoint the hosted page
// reads. It is deliberately unauthenticated: the page runs in a browser tab
// opened from the app, which has no Finnri session.
func TestPublicOrderLookupServesThePayPage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	fake := newFakeRazorpay(t)
	router := smokeRouter(t, withRazorpay(fake))
	_, token := createBillingTestUserSession(t)
	seedPurchasablePlan(t, "monthly", "monthly", 14900, 3600)
	order := startCheckout(t, router, token, "monthly", http.StatusCreated)

	// No Authorization header at all.
	public := performJSONRequest[map[string]any](
		t, router, http.MethodGet, "/v1/billing/checkout/"+order.OrderID, "", nil, http.StatusOK,
	)
	if public["key_id"] != testKeyID || public["order_id"] != order.OrderID {
		t.Fatalf("unexpected public order: %#v", public)
	}
	if public["amount_minor"] != float64(14900) || public["status"] != models.PaymentStatusCreated {
		t.Fatalf("unexpected public order: %#v", public)
	}

	// Nothing about the buyer, and neither secret, may appear here.
	encoded, _ := json.Marshal(public)
	for _, forbidden := range []string{testKeySecret, testWebhookSecret, "user_id", "email"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("public order leaked %q: %s", forbidden, encoded)
		}
	}
}

// TestPublicOrderLookupReportsSettledOrders lets the page say "already paid"
// instead of opening a second checkout against a settled order.
func TestPublicOrderLookupReportsSettledOrders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	fake := newFakeRazorpay(t)
	router := smokeRouter(t, withRazorpay(fake))
	_, token := createBillingTestUserSession(t)
	seedPurchasablePlan(t, "monthly", "monthly", 14900, 3600)
	order := startCheckout(t, router, token, "monthly", http.StatusCreated)
	postWebhook(t, router, captureBody(order.OrderID, "pay_1", 14900), "evt_1")

	public := performJSONRequest[map[string]any](
		t, router, http.MethodGet, "/v1/billing/checkout/"+order.OrderID, "", nil, http.StatusOK,
	)
	if public["status"] != models.PaymentStatusCaptured {
		t.Fatalf("expected a captured status, got %#v", public["status"])
	}
}

func TestPublicOrderLookupRejectsUnknownOrders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	fake := newFakeRazorpay(t)
	router := smokeRouter(t, withRazorpay(fake))

	performJSONRequest[map[string]any](
		t, router, http.MethodGet, "/v1/billing/checkout/order_nope", "", nil, http.StatusNotFound,
	)
}
