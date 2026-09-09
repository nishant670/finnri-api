package http

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"finnri/internal/billing"
	"finnri/internal/database"
	"finnri/internal/models"
	"finnri/internal/payments"
)

// checkoutOrderResponse is everything the browser needs to open Razorpay's
// checkout. It carries the publishable key only — the API secret and the
// webhook secret never leave the process.
type checkoutOrderResponse struct {
	Provider    string `json:"provider"`
	OrderID     string `json:"order_id"`
	KeyID       string `json:"key_id"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	PlanCode    string `json:"plan_code"`
	PlanName    string `json:"plan_name"`
	PaymentID   uint   `json:"payment_id"`
	// SuccessURL is where the web app should land after checkout closes. The
	// entitlement is not granted there — it is granted by the webhook — so the
	// page polls /v1/billing/status rather than trusting its own query string.
	SuccessURL string `json:"success_url,omitempty"`
	// CheckoutURL is the hosted page that opens Razorpay for this order. The
	// mobile app opens it in a browser tab rather than building the URL
	// itself, so the web origin is configured server-side in one place.
	CheckoutURL string `json:"checkout_url,omitempty"`
}

// publicCheckoutOrderResponse is what the hosted pay page reads to render an
// order it was handed by id.
//
// Everything here is already public: the key id is the publishable one the
// browser needs, and the amount is the order's own. Nothing identifies the
// buyer, so the page needs no session — which is the point, since it opens in
// a browser tab that has never signed in.
type publicCheckoutOrderResponse struct {
	Provider    string `json:"provider"`
	OrderID     string `json:"order_id"`
	KeyID       string `json:"key_id"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	PlanCode    string `json:"plan_code"`
	PlanName    string `json:"plan_name"`
	// Status lets the page say "already paid" instead of opening a second
	// checkout for an order the webhook has already settled.
	Status string `json:"status"`
}

// checkoutOrderReuseWindow lets a double-click, a back-button, or a reloaded
// checkout page land on the order that already exists rather than opening a
// second one against the same intent.
const checkoutOrderReuseWindow = 15 * time.Minute

// subscriptionPeriodFor returns how long one purchase of this plan buys.
//
// These are fixed spans rather than calendar months so that a period never
// depends on which month it was bought in — the credit tranche machinery
// already reasons in 30-day slices, and the two must not disagree.
func subscriptionPeriodFor(billingInterval string) (time.Duration, bool) {
	switch billingInterval {
	case "weekly":
		return 7 * 24 * time.Hour, true
	case "monthly":
		return 30 * 24 * time.Hour, true
	case "quarterly":
		return 90 * 24 * time.Hour, true
	case "yearly":
		return 365 * 24 * time.Hour, true
	default:
		// lifetime_quote is sold by quote after three paid months, never off a
		// price tag, so it has no self-serve period.
		return 0, false
	}
}

func (s *Server) createBillingCheckout(c *gin.Context) {
	user := currentUser(c)
	if user == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	if user.IsGuest {
		c.JSON(http.StatusForbidden, gin.H{"error": "login_required_for_checkout"})
		return
	}

	var input struct {
		PlanCode string `json:"plan_code"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	planCode := strings.TrimSpace(input.PlanCode)
	if planCode == "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_checkout", "fields": gin.H{"plan_code": "is required"}})
		return
	}

	plan, found, err := s.findPublicBillingPlan(planCode)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_billing_plan"})
		return
	}
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "plan_not_found"})
		return
	}
	if plan.BillingInterval == "lifetime_quote" {
		c.JSON(http.StatusForbidden, gin.H{"error": "lifetime_direct_purchase_not_allowed"})
		return
	}
	if !s.paymentProviderConfigured() {
		c.JSON(http.StatusNotImplemented, gin.H{
			"error":     "payment_provider_not_configured",
			"plan_code": plan.Code,
		})
		return
	}
	if _, ok := subscriptionPeriodFor(plan.BillingInterval); !ok {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "plan_not_purchasable", "plan_code": plan.Code})
		return
	}
	if plan.PriceMinor == nil || *plan.PriceMinor <= 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "plan_has_no_price", "plan_code": plan.Code})
		return
	}
	amountMinor := *plan.PriceMinor

	// payments.plan_id is a foreign key, so the plan has to exist as a row.
	// findPublicBillingPlan falls back to the in-code catalogue when the table
	// is empty, and an order created against that would have nothing to point
	// at — better to say so than to write a dangling reference.
	var planModel models.Plan
	if err := database.DB.Where("code = ?", plan.Code).First(&planModel).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusConflict, gin.H{"error": "plan_not_seeded", "plan_code": plan.Code})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_billing_plan"})
		return
	}

	if existing, ok, err := reusableCheckoutOrder(user.ID, planModel.ID, amountMinor, time.Now().UTC()); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_payment"})
		return
	} else if ok {
		c.JSON(http.StatusOK, s.checkoutResponse(existing, plan))
		return
	}

	order, err := s.razorpayClient().CreateOrder(c.Request.Context(), payments.CreateOrderRequest{
		AmountMinor: amountMinor,
		Currency:    plan.Currency,
		Receipt:     fmt.Sprintf("fnr_%d_%d", user.ID, time.Now().UTC().UnixMilli()),
		Notes: map[string]string{
			"user_id":   fmt.Sprintf("%d", user.ID),
			"plan_code": plan.Code,
		},
	})
	if err != nil {
		log.Printf("[ERROR] billing checkout order failed user=%d plan=%s: %v", user.ID, plan.Code, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "payment_provider_unavailable"})
		return
	}

	payment := models.Payment{
		UserID:          user.ID,
		PlanID:          planModel.ID,
		Provider:        payments.ProviderRazorpay,
		ProviderOrderID: order.ID,
		Status:          models.PaymentStatusCreated,
		AmountMinor:     amountMinor,
		Currency:        plan.Currency,
		Receipt:         order.Receipt,
	}
	if err := database.DB.Create(&payment).Error; err != nil {
		// The order exists at Razorpay but we cannot track it. Refusing here
		// is the honest answer: an untracked order that is later paid would
		// arrive at a webhook with nothing to match, and the person would be
		// charged with no subscription to show for it.
		log.Printf("[ERROR] billing checkout could not record order user=%d order=%s: %v", user.ID, order.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_create_payment"})
		return
	}

	c.JSON(http.StatusCreated, s.checkoutResponse(payment, plan))
}

// hostedCheckoutURL is the pay page for one order, or "" when no web origin is
// configured — in which case the client simply gets no URL to open rather than
// a broken one.
func (s *Server) hostedCheckoutURL(orderID string) string {
	if s.cfg == nil || strings.TrimSpace(s.cfg.WebBaseURL) == "" || orderID == "" {
		return ""
	}
	return strings.TrimRight(s.cfg.WebBaseURL, "/") + "/pay?order=" + url.QueryEscape(orderID)
}

// getBillingCheckoutOrder answers with the non-secret detail of one order.
//
// Public on purpose. The page that reads it runs in a browser tab opened from
// the app, which has no Finnri session and cannot be given one without putting
// a credential in a URL. Nothing here identifies the buyer, and knowing an
// order id only lets someone pay for it — a gift, not an attack.
func (s *Server) getBillingCheckoutOrder(c *gin.Context) {
	if !s.paymentProviderConfigured() {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "payment_provider_not_configured"})
		return
	}
	orderID := strings.TrimSpace(c.Param("order_id"))
	if orderID == "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_order"})
		return
	}

	var payment models.Payment
	err := database.DB.Where("provider = ? AND provider_order_id = ?", payments.ProviderRazorpay, orderID).
		First(&payment).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "order_not_found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_payment"})
		return
	}

	var plan models.Plan
	if err := database.DB.First(&plan, payment.PlanID).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_billing_plan"})
		return
	}

	c.JSON(http.StatusOK, publicCheckoutOrderResponse{
		Provider:    payment.Provider,
		OrderID:     payment.ProviderOrderID,
		KeyID:       s.razorpayClient().KeyID(),
		AmountMinor: payment.AmountMinor,
		Currency:    payment.Currency,
		PlanCode:    plan.Code,
		PlanName:    plan.Name,
		Status:      payment.Status,
	})
}

func (s *Server) checkoutResponse(payment models.Payment, plan billingPlanResponse) checkoutOrderResponse {
	successURL := ""
	if s.cfg != nil && strings.TrimSpace(s.cfg.WebBaseURL) != "" {
		successURL = strings.TrimRight(s.cfg.WebBaseURL, "/") + "/dashboard/billing"
	}
	return checkoutOrderResponse{
		Provider:    payment.Provider,
		OrderID:     payment.ProviderOrderID,
		KeyID:       s.razorpayClient().KeyID(),
		AmountMinor: payment.AmountMinor,
		Currency:    payment.Currency,
		PlanCode:    plan.Code,
		PlanName:    plan.Name,
		PaymentID:   payment.ID,
		SuccessURL:  successURL,
		CheckoutURL: s.hostedCheckoutURL(payment.ProviderOrderID),
	}
}

// reusableCheckoutOrder finds a still-open order for the same user, plan and
// amount. Matching on amount matters: a price change between the first click
// and the second must open a new order rather than silently charging the old
// price.
func reusableCheckoutOrder(userID, planID uint, amountMinor int64, now time.Time) (models.Payment, bool, error) {
	var payment models.Payment
	err := database.DB.
		Where("user_id = ? AND plan_id = ? AND status = ? AND amount_minor = ? AND created_at > ?",
			userID, planID, models.PaymentStatusCreated, amountMinor, now.Add(-checkoutOrderReuseWindow)).
		Order("created_at DESC").
		First(&payment).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return models.Payment{}, false, nil
	}
	if err != nil {
		return models.Payment{}, false, err
	}
	return payment, true, nil
}

// activateSubscriptionForPayment grants the period a captured payment bought.
//
// Every purchase gets its own user_subscriptions row rather than extending an
// existing one. Two reasons: the credit tranche schedule is computed from a
// row's own current_period_start and caps at the plan's tranche count, so
// stretching one row's end date would silently stop granting credits partway
// through a renewal; and stacking preserves time already paid for when someone
// buys again before their current period runs out.
//
// The new period therefore begins at the later of now and the end of whatever
// is currently running — a renewal queues behind the period it renews, and a
// lapsed user starts today.
func activateSubscriptionForPayment(tx *gorm.DB, payment *models.Payment, plan models.Plan, now time.Time) (models.UserSubscription, error) {
	period, ok := subscriptionPeriodFor(plan.BillingInterval)
	if !ok {
		return models.UserSubscription{}, fmt.Errorf("plan %q has no self-serve billing period", plan.Code)
	}

	start := now
	var latest models.UserSubscription
	err := tx.Where("user_id = ? AND status IN ? AND current_period_end > ?",
		payment.UserID, []string{"trialing", "active"}, now).
		Order("current_period_end DESC").
		First(&latest).Error
	if err == nil && latest.CurrentPeriodEnd.After(start) {
		start = latest.CurrentPeriodEnd
	} else if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return models.UserSubscription{}, err
	}

	subscription := models.UserSubscription{
		UserID:             payment.UserID,
		PlanID:             plan.ID,
		Status:             "active",
		CurrentPeriodStart: start,
		CurrentPeriodEnd:   start.Add(period),
		Provider:           payment.Provider,
		// One-time orders carry no mandate, so nothing auto-renews and there
		// is nothing to cancel. Saying so explicitly keeps the billing status
		// response honest instead of implying a recurring charge.
		CancelAtPeriodEnd: true,
	}
	if err := tx.Create(&subscription).Error; err != nil {
		return models.UserSubscription{}, err
	}
	return subscription, nil
}

// grantCreditsForSubscription issues the credits a newly bought period comes
// with.
//
// Long plans are tranched — the scheduled job in subscription_automation.go
// owns that, and calling it here only means the first slice lands immediately
// instead of up to a minute later. Weekly and monthly have no tranche policy,
// so their whole allowance is granted as one grant spanning the period.
func grantCreditsForSubscription(service *billing.CreditService, subscription models.UserSubscription, plan models.Plan, now time.Time) error {
	if _, ok := tranchedInterval(plan.BillingInterval); ok {
		_, err := service.SyncSubscriptionTranches(subscription, now)
		return err
	}
	if plan.IncludedCredits <= 0 {
		return nil
	}
	_, _, err := service.GrantSubscriptionPeriod(
		subscription,
		plan.IncludedCredits,
		subscription.CurrentPeriodStart,
		subscription.CurrentPeriodEnd,
	)
	return err
}

// tranchedInterval mirrors billing.tranchePolicyForInterval, which is
// unexported. Keeping the list in one predicate here means a new tranched
// interval is a two-line change rather than a silent double grant.
func tranchedInterval(billingInterval string) (bool, bool) {
	switch billingInterval {
	case "quarterly", "yearly", "lifetime_quote":
		return true, true
	default:
		return false, false
	}
}
