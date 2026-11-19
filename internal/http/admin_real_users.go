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
	// Only people who can appear in a list are read: everyone with a free
	// trial, and anyone refused an AI read recently. Guests' trials are keyed
	// by device hash, so guests are matched back by hashing their device ids.
	var grants []models.CreditGrant
	if err := database.DB.Where("source = ?", billing.GrantSourceFreeTrial).Find(&grants).Error; err != nil {
		return nil, err
	}
	guestHashes := map[string]uint{}
	var guests []struct {
		ID       uint
		DeviceID string
	}
	if err := database.DB.Model(&models.User{}).Select("id, device_id").
		Where("is_guest = ? AND device_id IS NOT NULL AND device_id <> ''", true).Scan(&guests).Error; err != nil {
		return nil, err
	}
	for _, guest := range guests {
		guestHashes[billing.HashUsageKey(guest.DeviceID)] = guest.ID
	}
	trials := map[uint]models.CreditGrant{}
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
	candidates := map[uint]struct{}{}
	for id := range trials {
		candidates[id] = struct{}{}
	}
	var limitedIDs []uint
	if err := database.DB.Model(&models.AIUsageLimitEvent{}).Distinct("user_id").
		Where("user_id IS NOT NULL AND created_at >= ?", now.Add(-trialCreditLimitWindow)).Pluck("user_id", &limitedIDs).Error; err != nil {
		return nil, err
	}
	for _, id := range limitedIDs {
		candidates[id] = struct{}{}
	}
	candidateIDs := make([]uint, 0, len(candidates))
	for id := range candidates {
		candidateIDs = append(candidateIDs, id)
	}
	users := make([]models.User, 0, len(candidateIDs))
	for _, chunk := range chunkIDs(candidateIDs) {
		var found []models.User
		if err := realUsersOnly(database.DB.Model(&models.User{}), includeTest).Where("id IN ?", chunk).Find(&found).Error; err != nil {
			return nil, err
		}
		users = append(users, found...)
	}
	if len(users) == 0 {
		return []adminTrialRow{}, nil
	}
	ids := make([]uint, 0, len(users))
	for _, user := range users {
		ids = append(ids, user.ID)
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
		for _, chunk := range chunkIDs(ids) {
			var rows []countRow
			query := database.DB.Model(model).Select("user_id, COUNT(*) AS total").Where("user_id IN ?", chunk)
			if where != "" {
				query = query.Where(where, args...)
			}
			_ = query.Group("user_id").Scan(&rows).Error
			for _, row := range rows {
				result[row.UserID] = row.Total
			}
		}
		return result
	}
	entries := countBy(&models.Entry{}, "")
	aiReads := countBy(&models.AIUsageEvent{}, "status = ?", "succeeded")
	push := countBy(&models.PushDevice{}, "active = ?", true)
	limitHits := countBy(&models.AIUsageLimitEvent{}, "created_at >= ?", now.Add(-trialCreditLimitWindow))
	lastLimit := map[uint]time.Time{}
	for _, chunk := range chunkIDs(ids) {
		var limitRows []struct {
			UserID uint
			At     time.Time
		}
		_ = database.DB.Model(&models.AIUsageLimitEvent{}).Select("user_id, MAX(created_at) AS at").
			Where("user_id IN ? AND created_at >= ?", chunk, now.Add(-trialCreditLimitWindow)).Group("user_id").Scan(&limitRows).Error
		for _, row := range limitRows {
			lastLimit[row.UserID] = row.At
		}
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

// AI usage split by who made the request. Totals always include everyone,
// since robots and test accounts cost real money; the split says where it went.
const (
	aiSegmentRegistered = "registered"
	aiSegmentGuest      = "guest"
	aiSegmentTest       = "test"
)

type adminAISegment struct {
	Events                 int64 `json:"events"`
	CreditsUsed            int64 `json:"credits_used"`
	EstimatedCostUSDMicros int64 `json:"estimated_cost_usd_micros"`
}

// aiUsageByUserType attributes each AI read in the window to a registered
// customer, a guest, or a test account. A guest's read can carry only the
// hash of its device id, so those are matched back to the guest row; a hash
// that matches no guest is still a guest's.
func aiUsageByUserType(start, end time.Time) (map[string]*adminAISegment, error) {
	split := map[string]*adminAISegment{aiSegmentRegistered: {}, aiSegmentGuest: {}, aiSegmentTest: {}}
	var rows []struct {
		UserID            *uint
		GuestDeviceIDHash string
		Events            int64
		Credits           int64
		Cost              int64
	}
	if err := database.DB.Model(&models.AIUsageEvent{}).
		Select("user_id, guest_device_id_hash, COUNT(*) AS events, COALESCE(SUM(final_credits), 0) AS credits, COALESCE(SUM(estimated_cost_usd_micros), 0) AS cost").
		Where("started_at >= ? AND started_at < ?", start, end).
		Group("user_id, guest_device_id_hash").Scan(&rows).Error; err != nil {
		return nil, err
	}
	userIDs := []uint{}
	needGuestHashes := false
	for _, row := range rows {
		if row.UserID != nil {
			userIDs = append(userIDs, *row.UserID)
		} else if strings.TrimSpace(row.GuestDeviceIDHash) != "" {
			needGuestHashes = true
		}
	}
	type userKind struct {
		ID      uint
		IsGuest bool
		IsTest  bool
	}
	kinds := map[uint]userKind{}
	for _, chunk := range chunkIDs(userIDs) {
		var found []userKind
		if err := database.DB.Model(&models.User{}).Select("id, is_guest, is_test").Where("id IN ?", chunk).Scan(&found).Error; err != nil {
			return nil, err
		}
		for _, kind := range found {
			kinds[kind.ID] = kind
		}
	}
	guestByHash := map[string]bool{} // hash -> is_test
	if needGuestHashes {
		var guests []struct {
			DeviceID string
			IsTest   bool
		}
		if err := database.DB.Model(&models.User{}).Select("device_id, is_test").
			Where("is_guest = ? AND device_id IS NOT NULL AND device_id <> ''", true).Scan(&guests).Error; err != nil {
			return nil, err
		}
		for _, guest := range guests {
			guestByHash[billing.HashUsageKey(guest.DeviceID)] = guest.IsTest
		}
	}
	for _, row := range rows {
		segment := aiSegmentGuest
		if row.UserID != nil {
			if kind, ok := kinds[*row.UserID]; ok {
				switch {
				case kind.IsTest:
					segment = aiSegmentTest
				case !kind.IsGuest:
					segment = aiSegmentRegistered
				}
			} else {
				// The account was deleted; its usage was a signed-in customer's.
				segment = aiSegmentRegistered
			}
		} else if guestByHash[row.GuestDeviceIDHash] {
			segment = aiSegmentTest
		}
		split[segment].Events += row.Events
		split[segment].CreditsUsed += row.Credits
		split[segment].EstimatedCostUSDMicros += row.Cost
	}
	return split, nil
}

// chunkIDs splits an id list so IN (...) stays far below Postgres's 65,535
// bind-parameter limit however many users there are.
func chunkIDs(ids []uint) [][]uint {
	const size = 1000
	chunks := [][]uint{}
	for start := 0; start < len(ids); start += size {
		end := start + size
		if end > len(ids) {
			end = len(ids)
		}
		chunks = append(chunks, ids[start:end])
	}
	return chunks
}
