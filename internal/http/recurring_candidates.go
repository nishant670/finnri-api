package http

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm/clause"

	"finnri/internal/database"
	"finnri/internal/models"
)

const (
	recurringDecisionDismissed = "dismissed"
	recurringDecisionSnoozed   = "snoozed"
	recurringDecisionTracked   = "tracked"
)

type recurringCandidateDecisionInput struct {
	CandidateKey string `json:"candidate_key"`
	Merchant     string `json:"merchant"`
	Category     string `json:"category"`
	Decision     string `json:"decision"`
	SnoozedUntil string `json:"snoozed_until"`
}

func (s *Server) saveRecurringCandidateDecision(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	var input recurringCandidateDecisionInput
	if err := c.ShouldBindJSON(&input); err != nil {
		if requestBodyTooLarge(err) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request_body_too_large"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}

	decision, fields := input.toModel(userID)
	if len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_recurring_candidate_decision", "fields": fields})
		return
	}

	if err := database.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "user_id"}, {Name: "candidate_key"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"merchant", "category", "decision", "snoozed_until", "last_reviewed_at", "updated_at",
		}),
	}).Create(&decision).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_save_recurring_candidate_decision"})
		return
	}
	c.JSON(http.StatusOK, decision)
}

func (input recurringCandidateDecisionInput) toModel(userID uint) (models.RecurringCandidateDecision, gin.H) {
	fields := gin.H{}
	merchant := strings.TrimSpace(input.Merchant)
	category := strings.TrimSpace(input.Category)
	candidateKey := strings.TrimSpace(input.CandidateKey)
	if candidateKey == "" {
		candidateKey = recurringCandidateKey(merchant, category)
	}
	if candidateKey == "|" || candidateKey == "" {
		fields["candidate_key"] = "is required"
	}

	decision := normalizeRecurringDecision(input.Decision)
	if decision == "" {
		fields["decision"] = "must be dismissed, snoozed, or tracked"
	}

	var snoozedUntil *time.Time
	if decision == recurringDecisionSnoozed {
		parsed, err := time.Parse("2006-01-02", strings.TrimSpace(input.SnoozedUntil))
		if err != nil {
			fields["snoozed_until"] = "must use YYYY-MM-DD"
		} else {
			snoozedUntil = &parsed
		}
	}

	now := time.Now().UTC()
	return models.RecurringCandidateDecision{
		UserID:         userID,
		CandidateKey:   candidateKey,
		Merchant:       merchant,
		Category:       category,
		Decision:       decision,
		SnoozedUntil:   snoozedUntil,
		LastReviewedAt: now,
	}, fields
}

func normalizeRecurringDecision(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case recurringDecisionDismissed:
		return recurringDecisionDismissed
	case recurringDecisionSnoozed:
		return recurringDecisionSnoozed
	case recurringDecisionTracked:
		return recurringDecisionTracked
	default:
		return ""
	}
}

type trackRecurringCandidatesInput struct {
	CandidateKeys []string `json:"candidate_keys"`
	StartDate     string   `json:"start_date"`
	EndDate       string   `json:"end_date"`
}

type trackedRecurringCandidate struct {
	CandidateKey string               `json:"candidate_key"`
	Subscription subscriptionResponse `json:"subscription"`
}

type skippedRecurringCandidate struct {
	CandidateKey string `json:"candidate_key"`
	Reason       string `json:"reason"`
}

type trackRecurringCandidatesResponse struct {
	Tracked []trackedRecurringCandidate `json:"tracked"`
	Skipped []skippedRecurringCandidate `json:"skipped"`
}

// trackRecurringCandidates turns detected patterns into subscriptions in one
// call, which is the whole point of the feature: the card on the Subscriptions
// screen offers "track these 4" and the user answers once, instead of filling
// the create form four times.
//
// The candidate figures are recomputed here rather than accepted from the
// request. The client only names keys, so the amount, interval and next date
// written onto each subscription are the same ones the server put on the card,
// and a stale or edited client payload cannot write a number the ledger does
// not support.
func (s *Server) trackRecurringCandidates(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	var input trackRecurringCandidatesInput
	if err := c.ShouldBindJSON(&input); err != nil {
		if requestBodyTooLarge(err) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request_body_too_large"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}

	location := loadLocationOrIndia(c.Query("tz"), s.cfg.TZDefault)
	now := time.Now().In(location)
	dateRange, fields := parseDashboardRange(input.StartDate, input.EndDate, now)
	if len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_range", "fields": fields})
		return
	}

	candidates, err := loadRecurringCandidates(userID, dateRange)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_recurring_candidates"})
		return
	}
	candidateByKey := map[string]DashboardRecurringCandidate{}
	for _, candidate := range candidates {
		candidateByKey[candidate.CandidateKey] = candidate
	}

	// An empty list means "all of them" — that is the card's primary action,
	// and it saves the client from echoing back keys it just received.
	requested := normalizeRequestedCandidateKeys(input.CandidateKeys)
	if len(requested) == 0 {
		requested = make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			requested = append(requested, candidate.CandidateKey)
		}
	}

	response := trackRecurringCandidatesResponse{
		Tracked: []trackedRecurringCandidate{},
		Skipped: []skippedRecurringCandidate{},
	}
	for _, key := range requested {
		candidate, ok := candidateByKey[key]
		if !ok {
			response.Skipped = append(response.Skipped, skippedRecurringCandidate{
				CandidateKey: key, Reason: "not_a_current_candidate",
			})
			continue
		}
		subscription, err := createSubscriptionFromCandidate(userID, candidate, now)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_create_subscription"})
			return
		}
		if err := recordRecurringCandidateDecision(userID, candidate, recurringDecisionTracked); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_save_recurring_candidate_decision"})
			return
		}
		response.Tracked = append(response.Tracked, trackedRecurringCandidate{
			CandidateKey: candidate.CandidateKey,
			Subscription: buildSubscriptionResponse(subscription, now),
		})
	}

	c.JSON(http.StatusOK, response)
}

func normalizeRequestedCandidateKeys(keys []string) []string {
	seen := map[string]bool{}
	normalized := make([]string, 0, len(keys))
	for _, key := range keys {
		trimmed := strings.TrimSpace(key)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		normalized = append(normalized, trimmed)
	}
	return normalized
}

// loadRecurringCandidates rebuilds the same list the dashboard card shows —
// detected over the comparison window, then filtered by decisions already made
// and by subscriptions that already cover the merchant.
func loadRecurringCandidates(userID uint, dateRange dashboardRange) ([]DashboardRecurringCandidate, error) {
	entries, err := loadDashboardRecurringEntries(
		userID,
		dateRange.PreviousStart.Format("2006-01-02"),
		dateRange.End.Format("2006-01-02"),
	)
	if err != nil {
		return nil, err
	}
	return filterReviewedRecurringCandidates(userID, detectRecurringCandidates(entries, dateRange), dateRange.End)
}

func createSubscriptionFromCandidate(
	userID uint,
	candidate DashboardRecurringCandidate,
	now time.Time,
) (models.Subscription, error) {
	amount, err := models.ParseMoney(strconv.FormatFloat(candidate.AverageAmount, 'f', 2, 64))
	if err != nil {
		return models.Subscription{}, err
	}

	interval := normalizeSubscriptionInterval(candidate.IntervalGuess)
	if interval == "" || interval == subscriptionIntervalDaily || interval == subscriptionIntervalBusinessDaily {
		// Detection only ever guesses weekly or monthly, and the daily
		// schedules require an Autopay account this path has no way to pick.
		interval = subscriptionIntervalMonthly
	}
	reminderDays := defaultSubscriptionReminderDays
	if interval == subscriptionIntervalWeekly {
		reminderDays = 1
	}

	subscription := models.Subscription{
		UserID:          userID,
		Name:            candidate.Label,
		Merchant:        strings.TrimSpace(candidate.Merchant),
		Category:        candidate.Category,
		Amount:          amount,
		Currency:        "INR",
		BillingInterval: interval,
		NextDueDate:     nextDueDateFromCandidate(candidate, interval, now),
		LastChargedDate: candidate.LastSeenDate,
		Status:          subscriptionStatusActive,
		ReminderDays:    reminderDays,
		PaymentMode:     "Cash",
		TransactionTag:  "Subscription",
		PurposeType:     "normal_spend",
		Notes: fmt.Sprintf(
			"Detected from %d similar transactions. Confidence %d%%.",
			candidate.Occurrences,
			int(math.Round(candidate.Confidence*100)),
		),
	}
	if err := database.DB.Create(&subscription).Error; err != nil {
		return models.Subscription{}, err
	}
	return subscription, nil
}

// nextDueDateFromCandidate rolls the detected next occurrence forward past
// today. The expected date can already be in the past — a pattern detected
// while reviewing last month's spend — and a subscription created overdue
// would fire an overdue reminder the moment it is saved, which is not what
// "track this" asked for.
func nextDueDateFromCandidate(candidate DashboardRecurringCandidate, interval string, now time.Time) string {
	next, err := parseAPIDate(candidate.NextExpectedDate)
	if err != nil {
		next = addSubscriptionInterval(truncateDate(now), interval)
	}
	today := truncateDate(now)
	for next.Before(today) {
		next = addSubscriptionInterval(next, interval)
	}
	return next.Format("2006-01-02")
}

func recordRecurringCandidateDecision(
	userID uint,
	candidate DashboardRecurringCandidate,
	decision string,
) error {
	return database.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "user_id"}, {Name: "candidate_key"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"merchant", "category", "decision", "snoozed_until", "last_reviewed_at", "updated_at",
		}),
	}).Create(&models.RecurringCandidateDecision{
		UserID:         userID,
		CandidateKey:   candidate.CandidateKey,
		Merchant:       strings.TrimSpace(candidate.Merchant),
		Category:       candidate.Category,
		Decision:       decision,
		LastReviewedAt: time.Now().UTC(),
	}).Error
}
