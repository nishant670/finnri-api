package http

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"finnri/internal/database"
	"finnri/internal/models"
	"finnri/internal/payments"
)

type billingPaymentsListResponse struct {
	Payments []billingPaymentResponse `json:"payments"`
}

// buyPass runs a real checkout and capture, so the rows under test are the
// ones the payment path writes rather than hand-made lookalikes.
func buyPass(t *testing.T, router *gin.Engine, token, planCode, paymentID string, amountMinor int64) checkoutOrderResponse {
	t.Helper()
	order := startCheckout(t, router, token, planCode, http.StatusCreated)
	if code := postWebhook(t, router, captureBody(order.OrderID, paymentID, amountMinor), "evt_"+paymentID).Code; code != http.StatusOK {
		t.Fatalf("capture webhook failed with %d", code)
	}
	return order
}

// expireSubscriptions moves every period this user holds into the past, as if
// the calendar had run on past them.
func expireSubscriptions(t *testing.T, userID uint, endedAgo time.Duration) {
	t.Helper()
	now := time.Now().UTC()
	var subscriptions []models.UserSubscription
	if err := database.DB.Where("user_id = ?", userID).Find(&subscriptions).Error; err != nil {
		t.Fatal(err)
	}
	for _, subscription := range subscriptions {
		length := subscription.CurrentPeriodEnd.Sub(subscription.CurrentPeriodStart)
		end := now.Add(-endedAgo)
		if err := database.DB.Model(&models.UserSubscription{}).Where("id = ?", subscription.ID).
			Updates(map[string]any{"current_period_start": end.Add(-length), "current_period_end": end}).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func TestBillingStatusNamesThePassThatExpired(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	fake := newFakeRazorpay(t)
	router := smokeRouter(t, withRazorpay(fake))
	user, token := createBillingTestUserSession(t)
	seedPurchasablePlan(t, "weekly", "weekly", 7900, 800)

	buyPass(t, router, token, "weekly", "pay_week", 7900)
	expireSubscriptions(t, user.ID, 72*time.Hour)

	status := performJSONRequest[billingStatusResponse](t, router, http.MethodGet, "/v1/billing/status", token, nil, http.StatusOK)
	if status.Plan != nil {
		t.Fatalf("an expired pass must not be reported as current: %#v", status.Plan)
	}
	// The trial is still on record and ended before the pass did. Without
	// last_pass, that trial is the only ending the app could name.
	if status.Credits.TrialExpiresAt == nil {
		t.Fatal("setup: expected the free trial to be on record")
	}
	if status.LastPass == nil {
		t.Fatal("billing status must name the pass that ended")
	}
	if status.LastPass.PlanName != "Weekly" || status.LastPass.BillingInterval != "weekly" || status.LastPass.EndedBy != "expired" {
		t.Fatalf("unexpected last pass: %#v", status.LastPass)
	}
	if ago := time.Since(status.LastPass.PeriodEnd); ago < 71*time.Hour || ago > 73*time.Hour {
		t.Fatalf("last pass should have ended three days ago, ended %s ago", ago)
	}
}

func TestBillingStatusOmitsLastPassWhileOneRuns(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	fake := newFakeRazorpay(t)
	router := smokeRouter(t, withRazorpay(fake))
	_, token := createBillingTestUserSession(t)
	seedPurchasablePlan(t, "weekly", "weekly", 7900, 800)

	buyPass(t, router, token, "weekly", "pay_week", 7900)

	status := performJSONRequest[billingStatusResponse](t, router, http.MethodGet, "/v1/billing/status", token, nil, http.StatusOK)
	if status.Plan == nil || status.Plan.Code != "weekly" {
		t.Fatalf("the running pass must be the current plan: %#v", status.Plan)
	}
	if status.LastPass != nil {
		t.Fatalf("last_pass is for when nothing runs, got %#v", status.LastPass)
	}
}

func TestBillingStatusOmitsLastPassForSomeoneWhoNeverBought(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, token := createBillingTestUserSession(t)

	status := performJSONRequest[billingStatusResponse](t, router, http.MethodGet, "/v1/billing/status", token, nil, http.StatusOK)
	if status.LastPass != nil {
		t.Fatalf("no purchase, no last pass: %#v", status.LastPass)
	}
}

func TestBillingStatusSaysARefundEndedThePass(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	fake := newFakeRazorpay(t)
	router := smokeRouter(t, withRazorpay(fake))
	_, token := createBillingTestUserSession(t)
	seedPurchasablePlan(t, "weekly", "weekly", 7900, 800)

	buyPass(t, router, token, "weekly", "pay_week", 7900)
	refund := `{"event":"refund.processed","payload":{"refund":{"entity":{"id":"rfnd_1","payment_id":"pay_week","amount":7900,"status":"processed"}}}}`
	if code := postWebhook(t, router, refund, "evt_refund").Code; code != http.StatusOK {
		t.Fatalf("refund webhook failed with %d", code)
	}

	status := performJSONRequest[billingStatusResponse](t, router, http.MethodGet, "/v1/billing/status", token, nil, http.StatusOK)
	if status.LastPass == nil || status.LastPass.EndedBy != "refunded" {
		t.Fatalf("a refunded pass must say so: %#v", status.LastPass)
	}
}

func TestPurchaseHistoryListsPaymentsNewestFirst(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	fake := newFakeRazorpay(t)
	router := smokeRouter(t, withRazorpay(fake))
	user, token := createBillingTestUserSession(t)
	weekly := seedPurchasablePlan(t, "weekly", "weekly", 7900, 800)
	seedPurchasablePlan(t, "monthly", "monthly", 14900, 3600)

	// A pass bought and since expired.
	bought := buyPass(t, router, token, "weekly", "pay_week", 7900)
	expireSubscriptions(t, user.ID, 72*time.Hour)

	// A later attempt the bank turned down.
	failedOrder := startCheckout(t, router, token, "monthly", http.StatusCreated)
	failure := fmt.Sprintf(`{"event":"payment.failed","payload":{"payment":{"entity":{"id":"pay_dead","order_id":%q,"status":"failed","amount":14900,"error_code":"BAD_REQUEST_ERROR","error_description":"Payment was declined by the bank","notes":[]}}}}`, failedOrder.OrderID)
	if code := postWebhook(t, router, failure, "evt_failed").Code; code != http.StatusOK {
		t.Fatalf("failure webhook returned %d", code)
	}
	// Local time, as gorm writes created_at: SQLite compares these as text, so
	// a UTC value would sort hours away from the rows around it.
	database.DB.Model(&models.Payment{}).Where("provider_order_id = ?", failedOrder.OrderID).
		Update("created_at", time.Now().Add(time.Minute))

	// An order someone opened and walked away from.
	if err := database.DB.Create(&models.Payment{
		UserID: user.ID, PlanID: weekly.ID, Provider: payments.ProviderRazorpay,
		ProviderOrderID: "order_abandoned", Status: models.PaymentStatusCreated,
		AmountMinor: 7900, Currency: "INR", CreatedAt: time.Now().Add(2 * time.Minute),
	}).Error; err != nil {
		t.Fatal(err)
	}

	// Someone else's purchase.
	other, _ := createBillingTestUserSession(t)
	if err := database.DB.Create(&models.Payment{
		UserID: other.ID, PlanID: weekly.ID, Provider: payments.ProviderRazorpay,
		ProviderOrderID: "order_other", ProviderPaymentID: "pay_other", Status: models.PaymentStatusCaptured,
		AmountMinor: 7900, Currency: "INR",
	}).Error; err != nil {
		t.Fatal(err)
	}

	history := performJSONRequest[billingPaymentsListResponse](t, router, http.MethodGet, "/v1/billing/payments", token, nil, http.StatusOK)
	if len(history.Payments) != 2 {
		t.Fatalf("expected the purchase and the failed attempt only, got %#v", history.Payments)
	}

	failed, paid := history.Payments[0], history.Payments[1]
	if failed.Status != models.PaymentStatusFailed || failed.FailureReason != "Payment was declined by the bank" || failed.PlanName != "Monthly" {
		t.Fatalf("unexpected failed row: %#v", failed)
	}
	if failed.PeriodStart != nil || failed.CapturedAt != nil {
		t.Fatalf("a failed attempt bought nothing: %#v", failed)
	}

	if paid.Status != models.PaymentStatusCaptured || paid.PlanName != "Weekly" || paid.AmountMinor != 7900 || paid.Currency != "INR" {
		t.Fatalf("unexpected purchase row: %#v", paid)
	}
	if paid.Reference != "pay_week" || paid.Method != "upi" || paid.CapturedAt == nil {
		t.Fatalf("purchase is missing its receipt details: %#v", paid)
	}
	if paid.PeriodStart == nil || paid.PeriodEnd == nil || !paid.PeriodEnd.Before(time.Now()) {
		t.Fatalf("purchase should carry the period it bought, now over: %#v", paid)
	}
	if bought.OrderID == paid.Reference {
		t.Fatal("reference should be the provider's payment id, not the order id")
	}
}

func TestPurchaseHistoryIsEmptyForAGuest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)

	guest := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{"device_id": "history-guest-device"}, http.StatusOK,
	)
	history := performJSONRequest[billingPaymentsListResponse](t, router, http.MethodGet, "/v1/billing/payments", guest.Token, nil, http.StatusOK)
	if history.Payments == nil || len(history.Payments) != 0 {
		t.Fatalf("a guest has an empty history, not a missing one: %#v", history.Payments)
	}
}
