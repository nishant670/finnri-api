package http

import (
	"strings"
	"time"

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

// launchOfferBuyers counts people who have paid at the launch price. A fully
// refunded purchase gives its place back.
func launchOfferBuyers() (int64, error) {
	var buyers int64
	err := database.DB.Model(&models.Payment{}).
		Where("promotion_code = ? AND status = ?", launchOfferCode, models.PaymentStatusCaptured).
		Distinct("user_id").Count(&buyers).Error
	return buyers, err
}

// userHasUsedLaunchOffer is the once-per-person rule.
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
	if s.cfg == nil {
		return launchOfferState{}, nil
	}
	start, end, ok := launchOfferWindow(s.cfg.LaunchOfferStartsAt)
	if !ok || now.Before(start) || !now.Before(end) {
		return launchOfferState{}, nil
	}
	buyers, err := launchOfferBuyers()
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
