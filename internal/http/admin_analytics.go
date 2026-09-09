package http

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"finnri/internal/database"
	"finnri/internal/models"
)

func (s *Server) getAdminSignups(c *gin.Context) {
	start, end, ok := parseMetricsWindow(c)
	if !ok {
		return
	}
	bucket := firstNonEmpty(c.Query("bucket"), "day")
	if bucket != "day" && bucket != "week" {
		c.JSON(422, gin.H{"error": "invalid_bucket"})
		return
	}
	var users []models.User
	if err := database.DB.Where("created_at >= ? AND created_at < ?", start, end).Order("created_at ASC").Find(&users).Error; err != nil {
		c.JSON(500, gin.H{"error": "failed_load_signups"})
		return
	}
	type signupPoint struct {
		Period     string `json:"period"`
		Total      int    `json:"total"`
		Registered int    `json:"registered"`
		Guests     int    `json:"guests"`
	}
	points := map[string]*signupPoint{}
	for _, user := range users {
		period := user.CreatedAt.UTC()
		if bucket == "week" {
			period = startOfWeek(period)
		}
		key := period.Format("2006-01-02")
		point := points[key]
		if point == nil {
			point = &signupPoint{Period: key}
			points[key] = point
		}
		point.Total++
		if user.IsGuest {
			point.Guests++
		} else {
			point.Registered++
		}
	}
	keys := make([]string, 0, len(points))
	for key := range points {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]signupPoint, 0, len(keys))
	for _, key := range keys {
		result = append(result, *points[key])
	}
	c.JSON(http.StatusOK, gin.H{"series": result, "bucket": bucket})
}

func (s *Server) getAdminActivation(c *gin.Context) {
	end := time.Now().UTC()
	start := end.Add(-30 * 24 * time.Hour)
	var err error
	if raw := strings.TrimSpace(c.Query("cohort_start")); raw != "" {
		start, err = parseFlexibleDate(raw, false)
		if err != nil {
			c.JSON(422, gin.H{"error": "invalid_cohort_start"})
			return
		}
	}
	if raw := strings.TrimSpace(c.Query("cohort_end")); raw != "" {
		end, err = parseFlexibleDate(raw, true)
		if err != nil {
			c.JSON(422, gin.H{"error": "invalid_cohort_end"})
			return
		}
	}
	var users []models.User
	if database.DB.Where("created_at >= ? AND created_at < ?", start, end).Find(&users).Error != nil {
		c.JSON(500, gin.H{"error": "failed_load_activation"})
		return
	}
	steps := []gin.H{{"code": "signed_up", "label": "Signed up", "users": len(users)}}
	counts := map[string]int{"account_created": 0, "first_transaction": 0, "first_ai_capture": 0, "habit_formed": 0, "returned_day_7": 0}

	// Every step used to run its own per-user query — roughly six round trips per
	// member of the cohort. These four batched reads answer all five steps for the
	// whole cohort, and the per-user windows are applied in Go.
	ids := make([]uint, 0, len(users))
	windowStart, windowEnd := time.Time{}, time.Time{}
	for _, user := range users {
		ids = append(ids, user.ID)
		if windowStart.IsZero() || user.CreatedAt.Before(windowStart) {
			windowStart = user.CreatedAt
		}
		if reach := user.CreatedAt.Add(10 * 24 * time.Hour); windowEnd.IsZero() || reach.After(windowEnd) {
			windowEnd = reach
		}
	}
	accountsAt := cohortEventTimes(ids, windowStart, windowEnd, &models.Account{}, "created_at", nil)
	entriesAt := cohortEventTimes(ids, windowStart, windowEnd, &models.Entry{}, "created_at", nil)
	aiAt := cohortEventTimes(ids, windowStart, windowEnd, &models.AIUsageEvent{}, "started_at", map[string]any{"status": "succeeded"})
	sessionsAt := cohortEventTimes(ids, windowStart, windowEnd, &models.AuthSession{}, "created_at", nil)

	for _, user := range users {
		deadline := user.CreatedAt.Add(7 * 24 * time.Hour)
		if anyWithin(accountsAt[user.ID], user.CreatedAt, deadline) {
			counts["account_created"]++
		}
		if anyWithin(entriesAt[user.ID], user.CreatedAt, deadline) {
			counts["first_transaction"]++
		}
		if anyWithin(aiAt[user.ID], user.CreatedAt, deadline) {
			counts["first_ai_capture"]++
		}
		entryCount, entryDays := 0, map[string]struct{}{}
		for _, at := range entriesAt[user.ID] {
			if at.Before(user.CreatedAt) || at.After(deadline) {
				continue
			}
			entryCount++
			entryDays[at.UTC().Format("2006-01-02")] = struct{}{}
		}
		if entryCount >= 5 && len(entryDays) >= 3 {
			counts["habit_formed"]++
		}
		returnStart, returnEnd := user.CreatedAt.Add(5*24*time.Hour), user.CreatedAt.Add(10*24*time.Hour)
		if anyWithin(entriesAt[user.ID], returnStart, returnEnd) ||
			anyWithin(sessionsAt[user.ID], returnStart, returnEnd) ||
			anyWithin(aiAt[user.ID], returnStart, returnEnd) {
			counts["returned_day_7"]++
		}
	}
	for _, definition := range []struct{ Code, Label string }{{"account_created", "Account created"}, {"first_transaction", "First transaction"}, {"first_ai_capture", "First AI capture"}, {"habit_formed", "Habit formed"}, {"returned_day_7", "Returned day 7"}} {
		steps = append(steps, gin.H{"code": definition.Code, "label": definition.Label, "users": counts[definition.Code], "percent": ratio(float64(counts[definition.Code]), float64(len(users)))})
	}
	c.JSON(200, gin.H{"cohort_start": start, "cohort_end": end, "cohort_size": len(users), "onboarded": counts["first_transaction"], "steps": steps})
}

// cohortEventTimes reads one table once for the whole cohort and groups the
// timestamps by user, so callers can apply per-user windows without another
// query each.
func cohortEventTimes(ids []uint, start, end time.Time, model any, column string, equals map[string]any) map[uint][]time.Time {
	grouped := map[uint][]time.Time{}
	if len(ids) == 0 {
		return grouped
	}
	var rows []struct {
		UserID uint
		At     time.Time
	}
	query := database.DB.Model(model).
		Select("user_id AS user_id, "+column+" AS at").
		Where("user_id IN ? AND "+column+" >= ? AND "+column+" <= ?", ids, start, end)
	for field, value := range equals {
		query = query.Where(field+" = ?", value)
	}
	_ = query.Scan(&rows).Error
	for _, row := range rows {
		grouped[row.UserID] = append(grouped[row.UserID], row.At)
	}
	return grouped
}

func anyWithin(times []time.Time, start, end time.Time) bool {
	for _, at := range times {
		if !at.Before(start) && !at.After(end) {
			return true
		}
	}
	return false
}

func (s *Server) getAdminRetention(c *gin.Context) {
	weeks := 12
	if raw := c.Query("weeks"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 1 && parsed <= 52 {
			weeks = parsed
		} else {
			c.JSON(422, gin.H{"error": "invalid_weeks"})
			return
		}
	}
	now := time.Now().UTC()
	firstCohort := startOfWeek(now).AddDate(0, 0, -7*(weeks-1))
	var users []models.User
	if database.DB.Where("created_at >= ? AND is_guest = ?", firstCohort, false).Find(&users).Error != nil {
		c.JSON(500, gin.H{"error": "failed_load_retention"})
		return
	}
	type cohort struct {
		Week      string    `json:"cohort_week"`
		Size      int       `json:"size"`
		Retention []float64 `json:"retention"`
	}
	cohorts := map[string]*cohort{}
	userWeeks := map[uint]string{}
	for _, user := range users {
		week := startOfWeek(user.CreatedAt).Format("2006-01-02")
		row := cohorts[week]
		if row == nil {
			row = &cohort{Week: week, Retention: make([]float64, weeks)}
			cohorts[week] = row
		}
		row.Size++
		userWeeks[user.ID] = week
	}
	// One active-user read per calendar week covers every cohort at once. The
	// previous shape asked per user per week, so a grid this size cost
	// users x weeks x 3 queries and grew with the user base.
	weeklyActive := map[string]map[uint]struct{}{}
	activeInWeek := func(weekStart time.Time) map[uint]struct{} {
		key := weekStart.Format("2006-01-02")
		if cached, ok := weeklyActive[key]; ok {
			return cached
		}
		active := activeUserIDs(weekStart, weekStart.AddDate(0, 0, 7))
		weeklyActive[key] = active
		return active
	}
	for userID, week := range userWeeks {
		cohortStart, _ := time.Parse("2006-01-02", week)
		for offset := 0; offset < weeks; offset++ {
			cellStart := cohortStart.AddDate(0, 0, 7*offset)
			if cellStart.After(now) {
				break
			}
			if _, ok := activeInWeek(cellStart)[userID]; ok {
				cohorts[week].Retention[offset]++
			}
		}
	}
	keys := make([]string, 0, len(cohorts))
	for key := range cohorts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]cohort, 0, len(keys))
	for _, key := range keys {
		row := cohorts[key]
		for index, value := range row.Retention {
			row.Retention[index] = ratio(value, float64(row.Size))
		}
		result = append(result, *row)
	}
	c.JSON(200, gin.H{"weeks": weeks, "cohorts": result})
}

func (s *Server) getAdminEngagement(c *gin.Context) {
	start, end, ok := parseMetricsWindow(c)
	if !ok {
		return
	}
	// Read each day's active set once, then roll the 7- and 30-day windows up from
	// those daily sets. This was three widening scans per day in the window.
	firstDay := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
	daily := map[string]map[uint]struct{}{}
	dayKey := func(day time.Time) string { return day.Format("2006-01-02") }
	for day := firstDay.AddDate(0, 0, -29); day.Before(end); day = day.AddDate(0, 0, 1) {
		daily[dayKey(day)] = activeUserIDs(day, day.AddDate(0, 0, 1))
	}
	uniqueOver := func(endExclusive time.Time, days int) int {
		union := map[uint]struct{}{}
		for offset := 1; offset <= days; offset++ {
			for id := range daily[dayKey(endExclusive.AddDate(0, 0, -offset))] {
				union[id] = struct{}{}
			}
		}
		return len(union)
	}
	series := []gin.H{}
	for day := firstDay; day.Before(end); day = day.AddDate(0, 0, 1) {
		next := day.AddDate(0, 0, 1)
		dau := len(daily[dayKey(day)])
		wau := uniqueOver(next, 7)
		mau := uniqueOver(next, 30)
		series = append(series, gin.H{"date": dayKey(day), "dau": dau, "wau": wau, "mau": mau, "stickiness_percent": ratio(float64(dau), float64(mau))})
	}
	c.JSON(200, gin.H{"series": series})
}

func (s *Server) getAdminFeatureAdoption(c *gin.Context) {
	_, registered, _ := userCounts()
	features := []gin.H{}
	for code, table := range map[string]string{"budgets": "budgets", "subscriptions": "subscriptions", "split_bills": "split_bills", "card_statements": "card_statements", "monthly_reviews": "monthly_reviews"} {
		var users int64
		_ = database.DB.Table(table).Joins("JOIN users ON users.id = "+table+".user_id AND users.is_guest = ?", false).Distinct(table + ".user_id").Count(&users).Error
		features = append(features, gin.H{"code": code, "users": users, "percent": ratio(float64(users), float64(registered))})
	}
	sort.Slice(features, func(i, j int) bool { return features[i]["code"].(string) < features[j]["code"].(string) })
	var converted int64
	_ = database.DB.Model(&models.User{}).Where("converted_at IS NOT NULL").Count(&converted).Error
	var guests int64
	_ = database.DB.Model(&models.User{}).Where("is_guest = ?", true).Count(&guests).Error
	c.JSON(200, gin.H{"registered_users": registered, "features": features, "guest_conversion": gin.H{"converted": converted, "guest_rows_remaining": guests, "percent": ratio(float64(converted), float64(converted+guests))}})
}

func startOfWeek(value time.Time) time.Time {
	value = value.UTC()
	offset := (int(value.Weekday()) + 6) % 7
	return time.Date(value.Year(), value.Month(), value.Day()-offset, 0, 0, 0, 0, time.UTC)
}

func parseFlexibleDate(raw string, endOfDay bool) (time.Time, error) {
	if value, err := time.Parse(time.RFC3339, raw); err == nil {
		return value.UTC(), nil
	}
	value, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return time.Time{}, err
	}
	if endOfDay {
		return value.UTC().AddDate(0, 0, 1), nil
	}
	return value.UTC(), nil
}
