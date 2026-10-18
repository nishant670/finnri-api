package http

import (
	"context"
	"database/sql/driver"
	"strings"
	"time"

	"gorm.io/gorm"

	"finnri/internal/database"
	"finnri/internal/models"
)

/*
The launch offer: 75% off every plan, for a limited time.

Behind "limited time" are two limits, whichever comes first: 90 days from
LAUNCH_OFFER_STARTS_AT, or 500 people having bought at the launch price. Each
person gets the launch price once; later purchases are full price.

The price is enforced here, at checkout, when the Razorpay order is created.
What the app displays is only ever a reflection of this. The webhook checks
a capture against the order's own amount, so a discounted order is granted
exactly like any other.

That makes an open discounted order a claim on the offer, not just a captured
one. Counting only captures let one person open Monthly and Quarterly at the
launch price before paying either, then pay both; and let more than 500
people hold launch-priced orders at once. So an open order reserves its
holder's one launch price and a place under the cap, and a newer discounted
checkout supersedes the person's older open ones, which the pay page then
refuses to open. Switching plan keeps the discount; paying twice does not.
*/

const (
	launchOfferCode           = "launch_75"
	launchOfferLabel          = "Launch offer"
	launchOfferPercentOff     = 75
	launchOfferDuration       = 90 * 24 * time.Hour
	launchOfferMaxBuyers      = 500
	launchOfferShowSpotsBelow = 50
)

// launchOfferPriceMinor is 75% off, rounded down to a whole rupee so the
// discount is never less than advertised: ₹149 → ₹37, ₹799 → ₹199.
func launchOfferPriceMinor(priceMinor int64) int64 {
	discounted := priceMinor * (100 - launchOfferPercentOff) / 100
	return discounted / 100 * 100
}

// launchOfferApplies says which plans the offer covers: every purchasable
// plan, never the lifetime quote, which is not bought at a list price.
func launchOfferApplies(billingInterval string) bool {
	_, purchasable := subscriptionPeriodFor(billingInterval)
	return purchasable && billingInterval != "lifetime_quote"
}

// launchOfferWindow parses the start, accepting a date or a timestamp, and
// reports the window. ok is false when no start is configured or it is
// unreadable — both mean no offer.
func launchOfferWindow(startsAt string) (start, end time.Time, ok bool) {
	value := strings.TrimSpace(startsAt)
	if value == "" {
		return time.Time{}, time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		date, dateErr := time.ParseInLocation("2006-01-02", value, indiaLocation())
		if dateErr != nil {
			return time.Time{}, time.Time{}, false
		}
		parsed = date
	}
	return parsed, parsed.Add(launchOfferDuration), true
}

// launchOfferSupersededReason marks a discounted order replaced by a newer
// one from the same person. It is stored as a failure so nothing reuses it.
const launchOfferSupersededReason = "superseded by a newer launch-offer checkout"

// launchOfferBuyers counts people who have paid at the launch price, or hold
// a launch-priced order opened in the last checkoutOrderReuseWindow. A fully
// refunded purchase gives its place back, and an abandoned order gives it
// back once it is that old. exceptUserID is left out of the count, so the
// person whose own reservation is open does not crowd themselves out.
func launchOfferBuyers(now time.Time, exceptUserID uint) (int64, error) {
	var buyers int64
	err := database.DB.Model(&models.Payment{}).
		Where("promotion_code = ? AND user_id <> ?", launchOfferCode, exceptUserID).
		Where("status = ? OR (status = ? AND created_at > ?)",
			models.PaymentStatusCaptured, models.PaymentStatusCreated, now.Add(-checkoutOrderReuseWindow)).
		Distinct("user_id").Count(&buyers).Error
	return buyers, err
}

// supersedeOpenLaunchOrders retires a person's other open launch-priced
// orders once a new one is recorded, so only the newest can be paid.
func supersedeOpenLaunchOrders(tx *gorm.DB, userID, keepPaymentID uint, now time.Time) error {
	return tx.Model(&models.Payment{}).
		Where("user_id = ? AND promotion_code = ? AND status = ? AND id <> ?",
			userID, launchOfferCode, models.PaymentStatusCreated, keepPaymentID).
		Updates(map[string]any{
			"status":         models.PaymentStatusFailed,
			"failure_reason": launchOfferSupersededReason,
			"updated_at":     now,
		}).Error
}

// isSupersededLaunchOrder is an order a newer discounted checkout replaced.
func isSupersededLaunchOrder(payment models.Payment) bool {
	return payment.Status == models.PaymentStatusFailed && payment.FailureReason == launchOfferSupersededReason
}

// checkoutLockNamespace keys the per-person advisory lock below.
const checkoutLockNamespace int32 = 0x46_4e_52 // "FNR"

// lockCheckoutFor serialises one person's checkouts, so two taps cannot both
// find the offer unclaimed. It holds a PostgreSQL advisory lock on its own
// connection, which leaves the person's other writes unblocked; SQLite, used
// only by tests, runs one statement at a time and needs none.
func lockCheckoutFor(ctx context.Context, userID uint) (func(), error) {
	if database.DB.Dialector.Name() != "postgres" {
		return func() {}, nil
	}
	sqlDB, err := database.DB.DB()
	if err != nil {
		return nil, err
	}
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1, $2)", checkoutLockNamespace, int32(userID)); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return func() {
		if _, err := conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1, $2)", checkoutLockNamespace, int32(userID)); err != nil {
			// A session lock outlives Close on a pooled connection, so a
			// connection that could not unlock is thrown away instead.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		_ = conn.Close()
	}, nil
}

// userHasUsedLaunchOffer is the once-per-person rule: it counts purchases.
// An open order is handled at checkout, which supersedes it.
func userHasUsedLaunchOffer(userID uint) (bool, error) {
	var count int64
	err := database.DB.Model(&models.Payment{}).
		Where("user_id = ? AND promotion_code = ? AND status = ?", userID, launchOfferCode, models.PaymentStatusCaptured).
		Count(&count).Error
	return count > 0, err
}

// launchOfferState is the offer as of now, independent of who is asking.
type launchOfferState struct {
	Active    bool
	EndsAt    time.Time
	SpotsLeft int64
}

func (s *Server) currentLaunchOffer(now time.Time) (launchOfferState, error) {
	return s.currentLaunchOfferFor(now, 0)
}

// currentLaunchOfferFor is the offer as one person at checkout sees it: their
// own open reservation does not count against the cap.
func (s *Server) currentLaunchOfferFor(now time.Time, userID uint) (launchOfferState, error) {
	if s.cfg == nil {
		return launchOfferState{}, nil
	}
	start, end, ok := launchOfferWindow(s.cfg.LaunchOfferStartsAt)
	if !ok || now.Before(start) || !now.Before(end) {
		return launchOfferState{}, nil
	}
	buyers, err := launchOfferBuyers(now, userID)
	if err != nil {
		return launchOfferState{}, err
	}
	left := int64(launchOfferMaxBuyers) - buyers
	if left <= 0 {
		return launchOfferState{}, nil
	}
	return launchOfferState{Active: true, EndsAt: end, SpotsLeft: left}, nil
}

// planOfferResponse is the offer as one plan shows it.
type planOfferResponse struct {
	Code               string    `json:"code"`
	Label              string    `json:"label"`
	PercentOff         int       `json:"percent_off"`
	PriceMinor         int64     `json:"price_minor"`
	OriginalPriceMinor int64     `json:"original_price_minor"`
	EndsAt             time.Time `json:"ends_at"`
	// SpotsLeft appears only when few remain: honest urgency, not a counter
	// from day one.
	SpotsLeft *int64 `json:"spots_left,omitempty"`
}

func launchOfferForPlan(state launchOfferState, plan billingPlanResponse) *planOfferResponse {
	if !state.Active || !launchOfferApplies(plan.BillingInterval) || plan.PriceMinor == nil || *plan.PriceMinor <= 0 {
		return nil
	}
	offer := &planOfferResponse{
		Code: launchOfferCode, Label: launchOfferLabel, PercentOff: launchOfferPercentOff,
		PriceMinor: launchOfferPriceMinor(*plan.PriceMinor), OriginalPriceMinor: *plan.PriceMinor,
		EndsAt: state.EndsAt,
	}
	if state.SpotsLeft < launchOfferShowSpotsBelow {
		left := state.SpotsLeft
		offer.SpotsLeft = &left
	}
	return offer
}

// launchOfferStatusResponse tells one signed-in user whether the offer is
// theirs to take, which the public plan list cannot know.
type launchOfferStatusResponse struct {
	Active     bool      `json:"active"`
	Eligible   bool      `json:"eligible"`
	Code       string    `json:"code"`
	Label      string    `json:"label"`
	PercentOff int       `json:"percent_off"`
	EndsAt     time.Time `json:"ends_at,omitempty"`
	SpotsLeft  *int64    `json:"spots_left,omitempty"`
}

func offerLabelFor(promotionCode string) string {
	if promotionCode == launchOfferCode {
		return launchOfferLabel
	}
	return ""
}
