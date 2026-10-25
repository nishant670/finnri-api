package http

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"finnri/internal/billing"
	"finnri/internal/database"
	"finnri/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// giftPassProvider marks a pass an admin handed out for free. It rides in the
// provider column so every "is this paid revenue?" query can leave gifts out
// with one condition, and the admin views can label them.
const giftPassProvider = "gift"

const (
	maxGiftPassPeriods   = 12
	maxGiftPassNoteRunes = 200
	maxGiftPassKeyLength = 80
)

// createAdminGiftPass gives a user one or more periods of a plan for free.
//
// A gift is the same thing a purchase is — one user_subscriptions row per
// period, queued behind whatever the user already has, with the plan's credits
// granted the same way — minus the payment. That keeps every entitlement,
// daily-limit and credit check working without knowing gifts exist; only the
// revenue views need to tell them apart.
func (s *Server) createAdminGiftPass(c *gin.Context) {
	id, ok := parseAdminID(c)
	if !ok {
		return
	}
	var input struct {
		PlanCode string `json:"plan_code"`
		Periods  int    `json:"periods"`
		Note     string `json:"note"`
	}
	if c.ShouldBindJSON(&input) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	if input.Periods == 0 {
		input.Periods = 1
	}
	note := strings.TrimSpace(input.Note)
	key := parseIdempotencyHeader(c)

	fields := gin.H{}
	if input.Periods < 1 || input.Periods > maxGiftPassPeriods {
		fields["periods"] = fmt.Sprintf("must be between 1 and %d", maxGiftPassPeriods)
	}
	if note == "" {
		fields["note"] = "is required"
	} else if utf8.RuneCountInString(note) > maxGiftPassNoteRunes {
		fields["note"] = fmt.Sprintf("must be %d characters or fewer", maxGiftPassNoteRunes)
	}
	if len(key) > maxGiftPassKeyLength {
		fields["idempotency_key"] = fmt.Sprintf("must be %d characters or fewer", maxGiftPassKeyLength)
	}
	var plan models.Plan
	var period time.Duration
	if err := database.DB.Where("code = ?", strings.TrimSpace(input.PlanCode)).First(&plan).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		fields["plan_code"] = "is not a known plan"
	} else if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_billing_plan"})
		return
	} else if period, ok = subscriptionPeriodFor(plan.BillingInterval); !ok {
		fields["plan_code"] = "has no fixed period to gift"
	}
	if len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_gift_pass", "fields": fields})
		return
	}

	var user models.User
	if err := database.DB.First(&user, id).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "user_not_found"})
		return
	} else if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_user"})
		return
	}
	if user.IsGuest {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "guest_cannot_hold_pass", "message": "Guests have no account to hold a pass. Ask them to sign in first."})
		return
	}

	// Without a key every request is a new gift. The admin panel always sends
	// one, so a double-click or a retried request gives the pass only once.
	if key == "" {
		key = generateUUID()
	}
	periodIDs := make([]string, input.Periods)
	for index := range periodIDs {
		periodIDs[index] = fmt.Sprintf("gift:%s:%d", key, index+1)
	}

	now := time.Now().UTC()
	var subscriptions []models.UserSubscription
	created := false
	err := database.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ? AND provider = ? AND provider_subscription_id IN ?", user.ID, giftPassProvider, periodIDs).
			Order("current_period_start ASC").
			Find(&subscriptions).Error; err != nil {
			return err
		}
		if len(subscriptions) > 0 {
			return nil
		}

		start, err := nextPassPeriodStart(tx, user.ID, now)
		if err != nil {
			return err
		}
		for _, periodID := range periodIDs {
			subscription := models.UserSubscription{
				UserID:                 user.ID,
				PlanID:                 plan.ID,
				Status:                 "active",
				CurrentPeriodStart:     start,
				CurrentPeriodEnd:       start.Add(period),
				Provider:               giftPassProvider,
				ProviderSubscriptionID: periodID,
				CancelAtPeriodEnd:      true,
			}
			if err := tx.Create(&subscription).Error; err != nil {
				return err
			}
			subscriptions = append(subscriptions, subscription)
			start = subscription.CurrentPeriodEnd
		}
		created = true
		return nil
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_create_gift_pass"})
		return
	}

	// Credits are granted after the rows commit, as on a purchase. The grants
	// are idempotent per period, so a replay re-runs this to finish whatever a
	// failed first attempt left undone.
	service := billing.NewCreditService(database.DB)
	for index := range subscriptions {
		subscriptions[index].Plan = plan
		if err := grantCreditsForSubscription(service, subscriptions[index], plan, now); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_grant_gift_pass_credits"})
			return
		}
	}

	subscriptionIDs := make([]uint, 0, len(subscriptions))
	for _, subscription := range subscriptions {
		subscriptionIDs = append(subscriptionIDs, subscription.ID)
	}
	c.Set(adminAuditDetailKey, gin.H{
		"plan_code":        plan.Code,
		"periods":          len(subscriptions),
		"note":             note,
		"subscription_ids": subscriptionIDs,
		"replayed":         !created,
	})
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	c.JSON(status, gin.H{"subscriptions": subscriptions, "created": created})
}
