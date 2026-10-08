package http

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"finnri/internal/database"
	"finnri/internal/models"
)

// billingHistoryLimit caps the purchase history. A pass a week for two years
// is about a hundred rows, and nobody scrolls further back than that on a phone.
const billingHistoryLimit = 100

// pastPassResponse is the most recent paid period that has already ended.
//
// Billing status only ever described what is running now, so the day a pass
// ran out the app had nothing to say about it and fell back to the free trial
// — "Free trial ended 16 Sept" to someone who had bought a pass after that and
// watched it expire. This is what lets it say which thing actually ended.
type pastPassResponse struct {
	PlanCode        string    `json:"plan_code"`
	PlanName        string    `json:"plan_name"`
	BillingInterval string    `json:"billing_interval"`
	PeriodStart     time.Time `json:"period_start"`
	PeriodEnd       time.Time `json:"period_end"`
	// EndedBy is "expired" for a pass that ran its course and "refunded" for
	// one a full refund cut short.
	EndedBy string `json:"ended_by"`
}

// billingPaymentResponse is one row of purchase history: a payment, and the
// period it bought when it bought one.
type billingPaymentResponse struct {
	ID                  uint   `json:"id"`
	Status              string `json:"status"`
	PlanCode            string `json:"plan_code"`
	PlanName            string `json:"plan_name"`
	BillingInterval     string `json:"billing_interval"`
	AmountMinor         int64  `json:"amount_minor"`
	AmountRefundedMinor int64  `json:"amount_refunded_minor"`
	OriginalAmountMinor int64  `json:"original_amount_minor,omitempty"`
	OfferLabel          string `json:"offer_label,omitempty"`
	Currency            string `json:"currency"`
	Method              string `json:"method,omitempty"`
	// Reference is the provider's payment id — what Razorpay's receipt email
	// and its support desk both quote.
	Reference     string     `json:"reference,omitempty"`
	FailureReason string     `json:"failure_reason,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	CapturedAt    *time.Time `json:"captured_at,omitempty"`
	RefundedAt    *time.Time `json:"refunded_at,omitempty"`
	PeriodStart   *time.Time `json:"period_start,omitempty"`
	PeriodEnd     *time.Time `json:"period_end,omitempty"`
}

// lastEndedPass finds the newest paid period whose end has passed, or nil.
func lastEndedPass(userID uint, now time.Time) (*pastPassResponse, error) {
	var subscription models.UserSubscription
	result := database.DB.Preload("Plan").
		Where("user_id = ? AND status IN ? AND current_period_end <= ?",
			userID, []string{"active", "cancelled", "expired"}, now).
		Order("current_period_end DESC").
		Limit(1).
		Find(&subscription)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, nil
	}

	// Nothing marks a pass expired when it runs out — its row stays "active"
	// with an end in the past — so "cancelled" is the one status that means
	// something ended it early, and today only a full refund does that.
	endedBy := "expired"
	if subscription.Status == "cancelled" {
		var refunded int64
		if err := database.DB.Model(&models.Payment{}).
			Where("subscription_id = ? AND status = ?", subscription.ID, models.PaymentStatusRefunded).
			Count(&refunded).Error; err != nil {
			return nil, err
		}
		if refunded > 0 {
			endedBy = "refunded"
		}
	}

	return &pastPassResponse{
		PlanCode:        subscription.Plan.Code,
		PlanName:        subscription.Plan.Name,
		BillingInterval: subscription.Plan.BillingInterval,
		PeriodStart:     subscription.CurrentPeriodStart,
		PeriodEnd:       subscription.CurrentPeriodEnd,
		EndedBy:         endedBy,
	}, nil
}

// listBillingPayments is the signed-in user's purchase history, newest first.
//
// Orders that were opened and never paid are left out. A "created" row is what
// every look at the price leaves behind, and listing those would bury the
// purchases among visits. Failed attempts stay in: someone who saw a payment
// screen error wants to see that it failed and that nothing was taken.
func (s *Server) listBillingPayments(c *gin.Context) {
	user := currentUser(c)
	if user == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	response := gin.H{"payments": []billingPaymentResponse{}}
	if user.IsGuest {
		// A guest cannot buy, so there is nothing to list — but the screen
		// asking is not an error.
		c.JSON(http.StatusOK, response)
		return
	}

	var rows []models.Payment
	if err := database.DB.Preload("Plan").Preload("Subscription").
		Where("user_id = ? AND status IN ?", user.ID, []string{
			models.PaymentStatusCaptured, models.PaymentStatusRefunded, models.PaymentStatusFailed,
		}).
		Order("created_at DESC").
		Limit(billingHistoryLimit).
		Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_payments"})
		return
	}

	history := make([]billingPaymentResponse, 0, len(rows))
	for _, row := range rows {
		history = append(history, billingPaymentFromModel(row))
	}
	response["payments"] = history
	c.JSON(http.StatusOK, response)
}

func billingPaymentFromModel(payment models.Payment) billingPaymentResponse {
	item := billingPaymentResponse{
		ID:                  payment.ID,
		Status:              payment.Status,
		AmountMinor:         payment.AmountMinor,
		AmountRefundedMinor: payment.AmountRefundedMinor,
		OriginalAmountMinor: payment.OriginalAmountMinor,
		OfferLabel:          offerLabelFor(payment.PromotionCode),
		Currency:            payment.Currency,
		Method:              payment.Method,
		Reference:           payment.ProviderPaymentID,
		FailureReason:       payment.FailureReason,
		CreatedAt:           payment.CreatedAt,
		CapturedAt:          payment.CapturedAt,
		RefundedAt:          payment.RefundedAt,
	}
	if item.Reference == "" {
		item.Reference = payment.ProviderOrderID
	}
	if payment.Plan != nil {
		item.PlanCode = payment.Plan.Code
		item.PlanName = payment.Plan.Name
		item.BillingInterval = payment.Plan.BillingInterval
	}
	if payment.Subscription != nil {
		start := payment.Subscription.CurrentPeriodStart
		end := payment.Subscription.CurrentPeriodEnd
		item.PeriodStart = &start
		item.PeriodEnd = &end
	}
	return item
}
