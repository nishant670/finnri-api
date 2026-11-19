package http

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"finnri/internal/billing"
	"finnri/internal/database"
	"finnri/internal/models"
)

// Bulk gifts: one admin action that gives many people the same thing, for an
// occasion (a festival, a win-back push). Each person is handled exactly like
// a single gift, keyed by the batch key plus their id, so a retried or
// double-submitted batch gifts nobody twice.

const (
	maxBulkGiftUsers        = 500
	maxBulkGiftCredits      = 10000
	maxGiftMessageTitleRune = 65
	maxGiftMessageBodyRune  = 240

	bulkGiftKindPass    = "pass"
	bulkGiftKindCredits = "credits"

	creditsGiftNotificationType = "billing.credits.gifted"
	bulkGiftReasonCode          = "admin_gift"
)

// Per-person outcomes in a bulk gift's response.
const (
	bulkGiftGifted         = "gifted"          // got what was sent
	bulkGiftCreditsInstead = "credits_instead" // a guest sent a pass got its credits
	bulkGiftAlreadyGifted  = "already_gifted"  // this batch reached them before
	bulkGiftSkipped        = "skipped"         // could not be gifted; see reason
	bulkGiftFailed         = "failed"          // an error; safe to retry the batch
)

type bulkGiftResult struct {
	UserID uint   `json:"user_id"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// creditSubjectForUser is the balance a user's credits live in: a signed-in
// user's id, or a guest's device. The app reads a guest's balance by device,
// so credits given to a guest's user id would never show up for them.
func creditSubjectForUser(user models.User) (billing.CreditSubject, bool) {
	if user.IsGuest {
		if user.DeviceID == nil || strings.TrimSpace(*user.DeviceID) == "" {
			return billing.CreditSubject{}, false
		}
		return billing.SubjectForGuestDeviceID(*user.DeviceID), true
	}
	return billing.SubjectForUser(user.ID), true
}

// giftCreditsTo grants credits to one person and tells them. The ledger's
// idempotency key makes a replay a no-op; created is false then.
func giftCreditsTo(user models.User, credits int, expiresAt *time.Time, key string, message *giftMessage) (bool, error) {
	subject, ok := creditSubjectForUser(user)
	if !ok {
		return false, errGuestWithoutDevice
	}
	_, created, err := billing.NewCreditService(database.DB).GrantManualAdjustment(subject, credits, bulkGiftReasonCode, key, expiresAt)
	if err != nil || !created {
		return created, err
	}
	title, body := creditsGiftNotificationCopy(credits, expiresAt)
	if message != nil {
		title, body = message.Title, message.Body
	}
	if err := createNotification(user.ID, creditsGiftNotificationType, title, body, giftPassActionURL); err != nil {
		log.Printf("[WARN] credits gift notification failed user=%d: %v", user.ID, err)
	}
	return true, nil
}

var errGuestWithoutDevice = errors.New("guest has no device to hold credits")

func creditsGiftNotificationCopy(credits int, expiresAt *time.Time) (string, string) {
	amount := strings.TrimPrefix(rupeesForCopy(models.Money(int64(credits)*100)), "₹")
	if expiresAt == nil {
		return "You've been gifted AI credits", fmt.Sprintf("%s AI credits are yours to scan bills, speak expenses and more.", amount)
	}
	return "You've been gifted AI credits", fmt.Sprintf("%s AI credits are yours until %s to scan bills, speak expenses and more.",
		amount, expiresAt.In(indiaLocation()).Format("2 Jan"))
}

// POST /v1/admin/gifts/bulk (support) — gift a pass or credits to many users.
func (s *Server) createAdminBulkGift(c *gin.Context) {
	var input struct {
		UserIDs   []uint `json:"user_ids"`
		Kind      string `json:"kind"`
		PlanCode  string `json:"plan_code"`
		Periods   int    `json:"periods"`
		Credits   int    `json:"credits"`
		ExpiresAt string `json:"expires_at"`
		Note      string `json:"note"`
		Message   *struct {
			Title string `json:"title"`
			Body  string `json:"body"`
		} `json:"message"`
	}
	if c.ShouldBindJSON(&input) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	key := parseIdempotencyHeader(c)
	note := strings.TrimSpace(input.Note)
	ids := uniqueIDs(input.UserIDs)

	fields := gin.H{}
	if len(ids) == 0 {
		fields["user_ids"] = "pick at least one user"
	} else if len(ids) > maxBulkGiftUsers {
		fields["user_ids"] = fmt.Sprintf("at most %d users at a time", maxBulkGiftUsers)
	}
	if key == "" {
		fields["idempotency_key"] = "is required, so a retried batch gifts nobody twice"
	} else if len(key) > maxGiftPassKeyLength {
		fields["idempotency_key"] = fmt.Sprintf("must be %d characters or fewer", maxGiftPassKeyLength)
	}
	if note == "" {
		fields["note"] = "is required"
	} else if utf8.RuneCountInString(note) > maxGiftPassNoteRunes {
		fields["note"] = fmt.Sprintf("must be %d characters or fewer", maxGiftPassNoteRunes)
	}
	var message *giftMessage
	if input.Message != nil {
		title, body := strings.TrimSpace(input.Message.Title), strings.TrimSpace(input.Message.Body)
		switch {
		case title == "" && body == "":
		case title == "" || body == "":
			fields["message"] = "needs both a title and a body, or neither"
		case utf8.RuneCountInString(title) > maxGiftMessageTitleRune:
			fields["message"] = fmt.Sprintf("title must be %d characters or fewer", maxGiftMessageTitleRune)
		case utf8.RuneCountInString(body) > maxGiftMessageBodyRune:
			fields["message"] = fmt.Sprintf("body must be %d characters or fewer", maxGiftMessageBodyRune)
		default:
			message = &giftMessage{Title: title, Body: body}
		}
	}

	var plan models.Plan
	var period time.Duration
	var expiresAt *time.Time
	switch input.Kind {
	case bulkGiftKindPass:
		if input.Periods == 0 {
			input.Periods = 1
		}
		if input.Periods < 1 || input.Periods > maxGiftPassPeriods {
			fields["periods"] = fmt.Sprintf("must be between 1 and %d", maxGiftPassPeriods)
		}
		ok := false
		if err := database.DB.Where("code = ?", strings.TrimSpace(input.PlanCode)).First(&plan).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			fields["plan_code"] = "is not a known plan"
		} else if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_billing_plan"})
			return
		} else if period, ok = subscriptionPeriodFor(plan.BillingInterval); !ok {
			fields["plan_code"] = "has no fixed period to gift"
		}
	case bulkGiftKindCredits:
		if input.Credits < 1 || input.Credits > maxBulkGiftCredits {
			fields["credits"] = fmt.Sprintf("must be between 1 and %d", maxBulkGiftCredits)
		}
		if raw := strings.TrimSpace(input.ExpiresAt); raw != "" {
			parsed, err := time.Parse(time.RFC3339, raw)
			if err != nil || !parsed.After(time.Now()) {
				fields["expires_at"] = "must be a future time"
			} else {
				utc := parsed.UTC()
				expiresAt = &utc
			}
		}
	default:
		fields["kind"] = "must be pass or credits"
	}
	if len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_bulk_gift", "fields": fields})
		return
	}

	// At most maxBulkGiftUsers ids, well inside one IN (...).
	users := map[uint]models.User{}
	var found []models.User
	if err := database.DB.Where("id IN ?", ids).Find(&found).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_users"})
		return
	}
	for _, user := range found {
		users[user.ID] = user
	}

	// A guest can't hold a pass, so a guest sent a pass gets the credits that
	// pass carries, for the same length of time.
	guestCredits := plan.IncludedCredits * input.Periods
	var guestExpiry *time.Time
	if input.Kind == bulkGiftKindPass {
		until := time.Now().UTC().Add(period * time.Duration(input.Periods))
		guestExpiry = &until
	}

	results := make([]bulkGiftResult, 0, len(ids))
	counts := map[string]int{}
	for _, id := range ids {
		result := bulkGiftResult{UserID: id}
		user, found := users[id]
		perUserKey := fmt.Sprintf("bulk:%s:%d", key, id)
		switch {
		case !found:
			result.Status, result.Reason = bulkGiftSkipped, "user_not_found"
		case input.Kind == bulkGiftKindPass && !user.IsGuest:
			_, created, err := giftPassTo(user, plan, period, input.Periods, perUserKey, message)
			result.Status, result.Reason = bulkGiftOutcome(created, err, bulkGiftGifted)
		case input.Kind == bulkGiftKindPass && guestCredits <= 0:
			result.Status, result.Reason = bulkGiftSkipped, "guest_cannot_hold_pass"
		case input.Kind == bulkGiftKindPass:
			created, err := giftCreditsTo(user, guestCredits, guestExpiry, perUserKey, guestPassMessage(message, guestCredits, guestExpiry))
			result.Status, result.Reason = bulkGiftOutcome(created, err, bulkGiftCreditsInstead)
		default:
			created, err := giftCreditsTo(user, input.Credits, expiresAt, perUserKey, message)
			result.Status, result.Reason = bulkGiftOutcome(created, err, bulkGiftGifted)
		}
		if result.Status == bulkGiftFailed {
			log.Printf("[WARN] bulk gift failed user=%d batch=%s", id, key)
		}
		counts[result.Status]++
		results = append(results, result)
	}

	c.Set(adminAuditDetailKey, gin.H{
		"kind":      input.Kind,
		"plan_code": plan.Code,
		"periods":   input.Periods,
		"credits":   input.Credits,
		"note":      note,
		"users":     len(ids),
		"counts":    counts,
		"message":   message != nil,
	})
	c.JSON(http.StatusOK, gin.H{"results": results, "counts": counts})
}

// guestPassMessage words a guest's stand-in credits. An admin's own occasion
// message is kept as written; the default pass copy would promise a pass.
func guestPassMessage(message *giftMessage, credits int, until *time.Time) *giftMessage {
	if message != nil {
		return message
	}
	title, body := creditsGiftNotificationCopy(credits, until)
	return &giftMessage{Title: title, Body: body}
}

// bulkGiftOutcome turns one person's gift into a status and reason. A guest
// with no device has nowhere for credits to live: skipped, not failed, since
// retrying won't help.
func bulkGiftOutcome(created bool, err error, success string) (string, string) {
	switch {
	case errors.Is(err, errGuestWithoutDevice):
		return bulkGiftSkipped, "guest_without_device"
	case err != nil:
		return bulkGiftFailed, "gift_failed"
	case created:
		return success, ""
	default:
		return bulkGiftAlreadyGifted, ""
	}
}

func uniqueIDs(ids []uint) []uint {
	seen := map[uint]struct{}{}
	out := make([]uint, 0, len(ids))
	for _, id := range ids {
		if id == 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
