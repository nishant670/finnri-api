package http

import (
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"finnri/internal/billing"
	"finnri/internal/database"
	"finnri/internal/models"
)

// Test users are accounts that are not real customers: Google Play's
// pre-launch robots, the team's own accounts, and anything an admin flags. The
// console leaves them out of every count unless the request says
// include_test=true, so the headline numbers describe real people.

// adminIncludeTest reports whether this admin request asked to count test
// users as well.
func adminIncludeTest(c *gin.Context) bool {
	return c.Query("include_test") == "true"
}

// realUsersOnly narrows a query on the users table to real customers.
func realUsersOnly(query *gorm.DB, includeTest bool) *gorm.DB {
	if includeTest {
		return query
	}
	return query.Where("users.is_test = ?", false)
}

// realUserRows narrows a query on any table with a user_id column to rows that
// belong to real customers. Rows without a user (a guest's AI read recorded by
// device only) are kept.
func realUserRows(query *gorm.DB, column string, includeTest bool) *gorm.DB {
	if includeTest {
		return query
	}
	return query.Where("("+column+" IS NULL OR "+column+" NOT IN (SELECT id FROM users WHERE is_test = ?))", true)
}

func countTestUsers() int64 {
	var count int64
	_ = database.DB.Model(&models.User{}).Where("is_test = ?", true).Count(&count).Error
	return count
}

func validUserTestReason(reason string) bool {
	for _, allowed := range models.UserTestReasons {
		if reason == allowed {
			return true
		}
	}
	return false
}

// PATCH /v1/admin/users/:id/test — flag or un-flag a test account. The admin
// audit middleware records who did it.
func (s *Server) updateAdminUserTest(c *gin.Context) {
	id, ok := parseAdminID(c)
	if !ok {
		return
	}
	var input struct {
		IsTest *bool  `json:"is_test"`
		Reason string `json:"reason"`
	}
	if c.ShouldBindJSON(&input) != nil || input.IsTest == nil {
		c.JSON(422, gin.H{"error": "is_test_required"})
		return
	}
	updates := map[string]any{"is_test": false, "test_reason": "", "test_marked_at": nil}
	if *input.IsTest {
		reason := firstNonEmpty(strings.TrimSpace(input.Reason), models.UserTestReasonManual)
		if !validUserTestReason(reason) {
			c.JSON(422, gin.H{"error": "invalid_test_reason"})
			return
		}
		now := time.Now().UTC()
		updates = map[string]any{"is_test": true, "test_reason": reason, "test_marked_at": &now}
	}
	result := database.DB.Model(&models.User{}).Where("id = ?", id).Updates(updates)
	if result.Error != nil {
		c.JSON(500, gin.H{"error": "failed_update_user"})
		return
	}
	if result.RowsAffected == 0 {
		c.JSON(404, gin.H{"error": "user_not_found"})
		return
	}
	var user models.User
	_ = database.DB.First(&user, id).Error
	c.JSON(200, gin.H{"id": user.ID, "is_test": user.IsTest, "test_reason": user.TestReason, "test_marked_at": user.TestMarkedAt})
}

// Trial statuses the Trials & sales page lists. One person can be in more than
// one: ended_active is a subset of ended_unpaid, and credit_limit is
// independent of the trial clock.
const (
	trialStatusEndingSoon  = "ending_soon"
	trialStatusInTrial     = "in_trial"
	trialStatusEndedUnpaid = "ended_unpaid"
	trialStatusEndedActive = "ended_active"
	trialStatusCreditLimit = "credit_limit"
)

var trialStatuses = []string{trialStatusEndingSoon, trialStatusInTrial, trialStatusEndedUnpaid, trialStatusEndedActive, trialStatusCreditLimit}

// A trial user who opened the app within this long after their trial ended is
// still using Finnri: the warmest people to offer a pass to.
const trialStillActiveWindow = 14 * 24 * time.Hour

// How far back a refused AI read still counts as "hit the credit limit".
const trialCreditLimitWindow = 30 * 24 * time.Hour

type adminTrialRow struct {
	ID                 uint       `json:"id"`
	Username           string     `json:"username"`
	Email              string     `json:"email,omitempty"`
	IsGuest            bool       `json:"is_guest"`
	CreatedAt          time.Time  `json:"created_at"`
	LastActiveAt       *time.Time `json:"last_active_at,omitempty"`
	TrialStartedAt     *time.Time `json:"trial_started_at,omitempty"`
	TrialEndsAt        *time.Time `json:"trial_ends_at,omitempty"`
	DaysLeft           *int       `json:"days_left,omitempty"`
	TrialCredits       int        `json:"trial_credits"`
	TrialCreditsLeft   int        `json:"trial_credits_left"`
	AIReads            int64      `json:"ai_reads"`
	EntriesCount       int64      `json:"entries_count"`
	PushEnabled        bool       `json:"push_enabled"`
	EverPaid           bool       `json:"ever_paid"`
	CreditLimitHitAt   *time.Time `json:"credit_limit_hit_at,omitempty"`
	CreditLimitHits    int64      `json:"credit_limit_hits"`
	Statuses           []string   `json:"statuses"`
	CoveredByPlanCode  string     `json:"covered_by_plan_code,omitempty"`
	trialEndsAtSortKey time.Time
}

// GET /v1/admin/trials?status=ending_soon&days=7 — who to offer AI credits to.
// Real users only (include_test=true adds test accounts). Anyone with an
// active pass or plan right now, gifted ones included, is left out: there is
// nothing to sell them.
func (s *Server) listAdminTrials(c *gin.Context) {
	status := firstNonEmpty(c.Query("status"), trialStatusEndingSoon)
	known := false
	for _, value := range trialStatuses {
		known = known || value == status
	}
	if !known {
		c.JSON(422, gin.H{"error": "invalid_status"})
		return
	}
	days := 7
	if raw := strings.TrimSpace(c.Query("days")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 60 {
			c.JSON(422, gin.H{"error": "invalid_days"})
			return
		}
		days = parsed
	}
	page, pageSize := parseBillingPagination(c.Query("page"), c.Query("page_size"))

	rows, err := buildTrialRows(time.Now().UTC(), days, adminIncludeTest(c))
	if err != nil {
		c.JSON(500, gin.H{"error": "failed_load_trials"})
		return
	}
	counts := map[string]int{}
	for _, value := range trialStatuses {
		counts[value] = 0
	}
	selected := make([]adminTrialRow, 0)
	for _, row := range rows {
		for _, value := range row.Statuses {
			counts[value]++
			if value == status {
				selected = append(selected, row)
			}
		}
	}
	sortTrialRows(selected, status)
	total := len(selected)
	from := (page - 1) * pageSize
	if from > total {
		from = total
	}
	to := from + pageSize
	if to > total {
		to = total
	}
	c.JSON(http.StatusOK, gin.H{"status": status, "days": days, "counts": counts, "users": selected[from:to], "page": page, "page_size": pageSize, "total": total})
}

func sortTrialRows(rows []adminTrialRow, status string) {
	sort.SliceStable(rows, func(i, j int) bool {
		switch status {
		case trialStatusEndingSoon, trialStatusInTrial:
			return rows[i].trialEndsAtSortKey.Before(rows[j].trialEndsAtSortKey)
		case trialStatusCreditLimit:
			return timeOrZero(rows[i].CreditLimitHitAt).After(timeOrZero(rows[j].CreditLimitHitAt))
		default:
			return timeOrZero(rows[i].LastActiveAt).After(timeOrZero(rows[j].LastActiveAt))
		}
	})
}

func timeOrZero(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return *value
}

// buildTrialRows reads every free-trial grant once and classifies its owner.
// A guest's grant is keyed by the hash of its device id, a signed-in user's by
// user id; a guest who later signs in has the grant moved to the user.
func buildTrialRows(now time.Time, days int, includeTest bool) ([]adminTrialRow, error) {
	var users []models.User
	if err := realUsersOnly(database.DB.Model(&models.User{}), includeTest).Find(&users).Error; err != nil {
		return nil, err
	}
	if len(users) == 0 {
		return []adminTrialRow{}, nil
	}
	ids := make([]uint, 0, len(users))
	guestHashes := map[string]uint{}
	for _, user := range users {
		ids = append(ids, user.ID)
		if user.IsGuest && user.DeviceID != nil && strings.TrimSpace(*user.DeviceID) != "" {
			guestHashes[billing.SubjectForGuestDeviceID(*user.DeviceID).GuestDeviceIDHash] = user.ID
		}
	}

	trials := map[uint]models.CreditGrant{}
	var grants []models.CreditGrant
	if err := database.DB.Where("source = ?", billing.GrantSourceFreeTrial).Find(&grants).Error; err != nil {
		return nil, err
	}
	for _, grant := range grants {
		owner := uint(0)
		if grant.UserID != nil {
			owner = *grant.UserID
		} else if id, ok := guestHashes[grant.GuestDeviceIDHash]; ok {
			owner = id
		}
		if owner != 0 {
			trials[owner] = grant
		}
	}

	covered := map[uint]string{}
	var coveredRows []struct {
		UserID uint
		Code   string
	}
	_ = database.DB.Model(&models.UserSubscription{}).
		Select("user_subscriptions.user_id AS user_id, plans.code AS code").
		Joins("JOIN plans ON plans.id = user_subscriptions.plan_id").
		Where("user_subscriptions.status = ? AND user_subscriptions.current_period_start <= ? AND user_subscriptions.current_period_end > ?", "active", now, now).
		Scan(&coveredRows).Error
	for _, row := range coveredRows {
		covered[row.UserID] = row.Code
	}
	everPaid := map[uint]bool{}
	var paidIDs []uint
	_ = database.DB.Model(&models.UserSubscription{}).Where("provider <> ?", giftPassProvider).Distinct("user_id").Pluck("user_id", &paidIDs).Error
	for _, id := range paidIDs {
		everPaid[id] = true
	}

	type countRow struct {
		UserID uint
		Total  int64
	}
	countBy := func(model any, where string, args ...any) map[uint]int64 {
		result := map[uint]int64{}
		var rows []countRow
		query := database.DB.Model(model).Select("user_id, COUNT(*) AS total").Where("user_id IN ?", ids)
		if where != "" {
			query = query.Where(where, args...)
		}
		_ = query.Group("user_id").Scan(&rows).Error
		for _, row := range rows {
			result[row.UserID] = row.Total
		}
		return result
	}
	entries := countBy(&models.Entry{}, "")
	aiReads := countBy(&models.AIUsageEvent{}, "status = ?", "succeeded")
	push := countBy(&models.PushDevice{}, "active = ?", true)
	limitHits := countBy(&models.AIUsageLimitEvent{}, "created_at >= ?", now.Add(-trialCreditLimitWindow))
	lastLimit := map[uint]time.Time{}
	var limitRows []struct {
		UserID uint
		At     time.Time
	}
	_ = database.DB.Model(&models.AIUsageLimitEvent{}).Select("user_id, MAX(created_at) AS at").
		Where("user_id IN ? AND created_at >= ?", ids, now.Add(-trialCreditLimitWindow)).Group("user_id").Scan(&limitRows).Error
	for _, row := range limitRows {
		lastLimit[row.UserID] = row.At
	}

	rows := make([]adminTrialRow, 0, len(trials)+len(limitHits))
	for _, user := range users {
		if _, isCovered := covered[user.ID]; isCovered {
			continue
		}
		grant, hasTrial := trials[user.ID]
		row := adminTrialRow{
			ID:              user.ID,
			Username:        user.Username,
			Email:           maskEmail(ptrString(user.Email)),
			IsGuest:         user.IsGuest,
			CreatedAt:       user.CreatedAt,
			LastActiveAt:    user.LastActiveAt,
			AIReads:         aiReads[user.ID],
			EntriesCount:    entries[user.ID],
			PushEnabled:     push[user.ID] > 0,
			EverPaid:        everPaid[user.ID],
			CreditLimitHits: limitHits[user.ID],
			Statuses:        []string{},
		}
		if hasTrial {
			started := grant.ValidFrom
			row.TrialStartedAt = &started
			row.TrialCredits = grant.CreditsGranted
			row.TrialCreditsLeft = grant.CreditsRemaining
			if grant.ExpiresAt != nil {
				ends := *grant.ExpiresAt
				row.TrialEndsAt = &ends
				row.trialEndsAtSortKey = ends
				if ends.After(now) {
					left := int(math.Ceil(ends.Sub(now).Hours() / 24))
					row.DaysLeft = &left
					row.Statuses = append(row.Statuses, trialStatusInTrial)
					if !ends.After(now.AddDate(0, 0, days)) {
						row.Statuses = append(row.Statuses, trialStatusEndingSoon)
					}
				} else {
					row.TrialCreditsLeft = 0
					if !row.EverPaid {
						row.Statuses = append(row.Statuses, trialStatusEndedUnpaid)
						if user.LastActiveAt != nil && user.LastActiveAt.After(ends) && user.LastActiveAt.After(now.Add(-trialStillActiveWindow)) {
							row.Statuses = append(row.Statuses, trialStatusEndedActive)
						}
					}
				}
			}
		}
		if at, ok := lastLimit[user.ID]; ok {
			hit := at
			row.CreditLimitHitAt = &hit
			row.Statuses = append(row.Statuses, trialStatusCreditLimit)
		}
		if len(row.Statuses) > 0 {
			rows = append(rows, row)
		}
	}
	return rows, nil
}
