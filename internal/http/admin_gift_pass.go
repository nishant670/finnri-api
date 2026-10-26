package http

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
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

var (
	errGiftPassNotFound = errors.New("gift pass not found")
	errNotAGiftPass     = errors.New("not a gifted pass")
	errGiftPassEnded    = errors.New("gift pass already ended")
	errPaidPassQueuedOn = errors.New("paid pass queued after gift")
)

// revokeAdminGiftPass ends a gifted pass now, or cancels it outright if it
// hasn't started, and takes back its unspent credits.
//
// Only gifts can be revoked here — a paid pass ends through a refund, which
// moves money. A gift with a paid pass queued behind it is refused: the paid
// pass would keep its later start, and the person would sit without a pass
// for the gap in between.
func (s *Server) revokeAdminGiftPass(c *gin.Context) {
	userID, ok := parseAdminID(c)
	if !ok {
		return
	}
	passID, err := strconv.ParseUint(c.Param("pass_id"), 10, 64)
	if err != nil || passID == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
		return
	}
	var input struct {
		Note string `json:"note"`
	}
	if err := c.ShouldBindJSON(&input); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	note := strings.TrimSpace(input.Note)
	if utf8.RuneCountInString(note) > maxGiftPassNoteRunes {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_gift_revoke", "fields": gin.H{"note": fmt.Sprintf("must be %d characters or fewer", maxGiftPassNoteRunes)}})
		return
	}

	now := time.Now().UTC()
	var pass models.UserSubscription
	wasRunning := false
	creditsRevoked := 0
	err = database.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Preload("Plan").Where("id = ? AND user_id = ?", passID, userID).First(&pass).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return errGiftPassNotFound
		} else if err != nil {
			return err
		}
		if pass.Provider != giftPassProvider {
			return errNotAGiftPass
		}
		if pass.Status != "active" || !pass.CurrentPeriodEnd.After(now) {
			return errGiftPassEnded
		}
		var paidAfter int64
		if err := tx.Model(&models.UserSubscription{}).
			Where("user_id = ? AND provider <> ? AND status IN ? AND current_period_start >= ?",
				userID, giftPassProvider, []string{"trialing", "active"}, pass.CurrentPeriodEnd).
			Count(&paidAfter).Error; err != nil {
			return err
		}
		if paidAfter > 0 {
			return errPaidPassQueuedOn
		}

		updates := map[string]any{"status": "cancelled", "updated_at": now}
		// A running gift ends now; a queued one keeps its dates, since a
		// cancelled row is never read as a pass anyway.
		if now.After(pass.CurrentPeriodStart) {
			wasRunning = true
			updates["current_period_end"] = now
		}
		if err := tx.Model(&models.UserSubscription{}).Where("id = ?", pass.ID).Updates(updates).Error; err != nil {
			return err
		}
		creditsRevoked, err = billing.RevokeGiftedPassCreditsTx(tx, pass.ID)
		if err != nil {
			return err
		}
		return tx.Preload("Plan").First(&pass, pass.ID).Error
	})
	switch {
	case errors.Is(err, errGiftPassNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "pass_not_found"})
		return
	case errors.Is(err, errNotAGiftPass):
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "not_a_gift", "message": "Only gifted passes can be revoked. A paid pass ends through a refund."})
		return
	case errors.Is(err, errGiftPassEnded):
		c.JSON(http.StatusConflict, gin.H{"error": "gift_already_ended", "message": "This gift has already ended or been revoked."})
		return
	case errors.Is(err, errPaidPassQueuedOn):
		c.JSON(http.StatusConflict, gin.H{"error": "paid_pass_queued_after", "message": "A paid pass is queued after this gift. Revoking it would leave a gap before the paid pass starts."})
		return
	case err != nil:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_revoke_gift_pass"})
		return
	}

	c.Set(adminAuditDetailKey, gin.H{
		"pass_id":         pass.ID,
		"plan_code":       pass.Plan.Code,
		"was_running":     wasRunning,
		"credits_revoked": creditsRevoked,
		"note":            note,
	})
	c.JSON(http.StatusOK, gin.H{"subscription": pass, "credits_revoked": creditsRevoked})
}
