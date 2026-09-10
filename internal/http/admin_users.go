package http

import (
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm/clause"

	"finnri/internal/billing"
	"finnri/internal/database"
	"finnri/internal/models"
)

type adminUserListItem struct {
	ID                      uint       `json:"id"`
	Username                string     `json:"username"`
	Email                   string     `json:"email,omitempty"`
	Phone                   string     `json:"phone,omitempty"`
	IsGuest                 bool       `json:"is_guest"`
	CreatedAt               time.Time  `json:"created_at"`
	ConvertedAt             *time.Time `json:"converted_at,omitempty"`
	LastActiveAt            *time.Time `json:"last_active_at,omitempty"`
	PlanCode                string     `json:"plan_code"`
	CreditsRemaining        int        `json:"credits_remaining"`
	EntriesCount            int64      `json:"entries_count"`
	AICreditsUsed           int        `json:"ai_credits_used"`
	LifetimeAICostUSDMicros int64      `json:"lifetime_ai_cost_usd_micros"`
}

func (s *Server) getAdminOverview(c *gin.Context) {
	start, end, ok := parseMetricsWindow(c)
	if !ok {
		return
	}
	window := end.Sub(start)
	previousStart := start.Add(-window)

	total, registered, guests := userCounts()
	newUsers := countUsersCreated(start, end)
	previousNew := countUsersCreated(previousStart, start)
	active := activeUserIDs(start, end)
	dau := len(activeUserIDs(end.Add(-24*time.Hour), end))
	wau := len(activeUserIDs(end.Add(-7*24*time.Hour), end))
	mau := len(activeUserIDs(end.Add(-30*24*time.Hour), end))
	activated := int64(0)
	_ = database.DB.Model(&models.User{}).Where("is_guest = ? AND EXISTS (SELECT 1 FROM entries WHERE entries.user_id = users.id)", false).Count(&activated).Error
	activationRate := ratio(float64(activated), float64(registered))

	revenue := buildRevenueSnapshot(start, end)
	ai, _ := billing.BuildAIMetrics(database.DB, start, end, s.aiAlertConfig())
	var openFeedback int64
	_ = database.DB.Model(&models.Feedback{}).Where("status IN ?", []string{"new", "triaged", "planned"}).Count(&openFeedback).Error

	c.JSON(http.StatusOK, gin.H{
		"window":          gin.H{"start": start, "end": end},
		"users":           gin.H{"total": total, "registered": registered, "guests": guests, "new": newUsers, "new_delta_percent": percentDelta(float64(newUsers), float64(previousNew))},
		"activation_rate": activationRate,
		"engagement":      gin.H{"active_in_window": len(active), "dau": dau, "wau": wau, "mau": mau, "stickiness": ratio(float64(dau), float64(mau))},
		"subscriptions":   gin.H{"active": revenue.ActiveSubscribers, "modelled_mrr_minor": revenue.MRRMinor, "basis": revenue.Basis},
		"ai":              gin.H{"events": ai.TotalEvents, "credits_used": ai.TotalCreditsCharged, "estimated_cost_usd_micros": ai.EstimatedCostUSDMicros, "actual_cost_usd_micros": ai.ActualCostUSDMicros, "alerts": ai.Alerts},
		"open_feedback":   openFeedback,
	})
}

func userCounts() (total, registered, guests int64) {
	_ = database.DB.Model(&models.User{}).Count(&total).Error
	_ = database.DB.Model(&models.User{}).Where("is_guest = ?", false).Count(&registered).Error
	guests = total - registered
	return
}

func countUsersCreated(start, end time.Time) int64 {
	var count int64
	_ = database.DB.Model(&models.User{}).Where("created_at >= ? AND created_at < ?", start, end).Count(&count).Error
	return count
}

func ratio(numerator, denominator float64) float64 {
	if denominator <= 0 {
		return 0
	}
	return math.Round((numerator/denominator)*10000) / 100
}

func percentDelta(current, previous float64) float64 {
	if previous == 0 {
		if current == 0 {
			return 0
		}
		return 100
	}
	return math.Round(((current-previous)/previous)*1000) / 10
}

func activeUserIDs(start, end time.Time) map[uint]struct{} {
	ids := map[uint]struct{}{}
	var values []uint
	_ = database.DB.Model(&models.Entry{}).Distinct("user_id").Where("created_at >= ? AND created_at < ?", start, end).Pluck("user_id", &values).Error
	for _, id := range values {
		ids[id] = struct{}{}
	}
	values = nil
	_ = database.DB.Model(&models.AuthSession{}).Distinct("user_id").Where("created_at >= ? AND created_at < ?", start, end).Pluck("user_id", &values).Error
	for _, id := range values {
		ids[id] = struct{}{}
	}
	values = nil
	_ = database.DB.Model(&models.AIUsageEvent{}).Distinct("user_id").Where("user_id IS NOT NULL AND started_at >= ? AND started_at < ?", start, end).Pluck("user_id", &values).Error
	for _, id := range values {
		ids[id] = struct{}{}
	}
	return ids
}

func (s *Server) listAdminUsers(c *gin.Context) {
	page, pageSize := parseBillingPagination(c.Query("page"), c.Query("page_size"))
	query := database.DB.Model(&models.User{})
	if q := strings.TrimSpace(c.Query("q")); q != "" {
		like := "%" + strings.ToLower(q) + "%"
		if id, err := strconv.ParseUint(q, 10, 64); err == nil {
			query = query.Where("id = ? OR LOWER(username) LIKE ? OR LOWER(COALESCE(email, '')) LIKE ? OR LOWER(COALESCE(phone, '')) LIKE ?", id, like, like, like)
		} else {
			query = query.Where("LOWER(username) LIKE ? OR LOWER(COALESCE(email, '')) LIKE ? OR LOWER(COALESCE(phone, '')) LIKE ?", like, like, like)
		}
	}
	switch c.Query("type") {
	case "guest":
		query = query.Where("is_guest = ?", true)
	case "registered":
		query = query.Where("is_guest = ?", false)
	}
	if c.Query("activated") == "true" {
		query = query.Where("EXISTS (SELECT 1 FROM entries WHERE entries.user_id = users.id)")
	}
	if c.Query("active_30d") == "true" {
		query = query.Where("last_active_at >= ?", time.Now().UTC().Add(-30*24*time.Hour))
	}
	if plan := strings.TrimSpace(c.Query("plan")); plan != "" {
		query = query.Where("EXISTS (SELECT 1 FROM user_subscriptions us JOIN plans p ON p.id = us.plan_id WHERE us.user_id = users.id AND us.status = 'active' AND us.current_period_end > ? AND p.code = ?)", time.Now().UTC(), plan)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		c.JSON(500, gin.H{"error": "failed_count_users"})
		return
	}
	order := "created_at DESC"
	switch c.Query("sort") {
	case "created_at_asc":
		order = "created_at ASC"
	case "last_active_desc":
		order = "last_active_at DESC"
	case "username":
		order = "username ASC"
	}
	var users []models.User
	if err := query.Order(order).Limit(pageSize).Offset((page - 1) * pageSize).Find(&users).Error; err != nil {
		c.JSON(500, gin.H{"error": "failed_list_users"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"users": adminUserSummaries(users), "page": page, "page_size": pageSize, "total": total})
}

// adminUserSummaries fills a page of rows with five grouped queries instead of
// the five-per-row adminUserSummary was doing: a 25-row page cost 218 queries.
func adminUserSummaries(users []models.User) []adminUserListItem {
	items := make([]adminUserListItem, 0, len(users))
	if len(users) == 0 {
		return items
	}
	ids := make([]uint, 0, len(users))
	for _, user := range users {
		ids = append(ids, user.ID)
	}
	now := time.Now().UTC()

	// Ordered ascending so the latest period wins the last write per user. This
	// stays portable across Postgres and the SQLite used by the tests, which
	// DISTINCT ON would not.
	type planRow struct {
		UserID uint
		Code   string
	}
	plans := map[uint]string{}
	var planRows []planRow
	_ = database.DB.Model(&models.UserSubscription{}).
		Select("user_subscriptions.user_id AS user_id, plans.code AS code").
		Joins("JOIN plans ON plans.id = user_subscriptions.plan_id").
		Where("user_subscriptions.user_id IN ? AND user_subscriptions.status = ? AND user_subscriptions.current_period_end > ?", ids, "active", now).
		Order("user_subscriptions.current_period_end ASC").
		Scan(&planRows).Error
	for _, row := range planRows {
		plans[row.UserID] = row.Code
	}

	type sumRow struct {
		UserID uint
		Total  int64
	}
	credits := map[uint]int64{}
	var creditRows []sumRow
	_ = database.DB.Model(&models.CreditGrant{}).
		Select("user_id, COALESCE(SUM(credits_remaining), 0) AS total").
		Where("user_id IN ? AND valid_from <= ? AND (expires_at IS NULL OR expires_at > ?)", ids, now, now).
		Group("user_id").Scan(&creditRows).Error
	for _, row := range creditRows {
		credits[row.UserID] = row.Total
	}

	entries := map[uint]int64{}
	var entryRows []sumRow
	_ = database.DB.Model(&models.Entry{}).
		Select("user_id, COUNT(*) AS total").
		Where("user_id IN ?", ids).Group("user_id").Scan(&entryRows).Error
	for _, row := range entryRows {
		entries[row.UserID] = row.Total
	}

	type aiRow struct {
		UserID  uint
		Credits int64
		Cost    int64
	}
	aiCredits := map[uint]int64{}
	aiCost := map[uint]int64{}
	var aiRows []aiRow
	_ = database.DB.Model(&models.AIUsageEvent{}).
		Select("user_id, COALESCE(SUM(final_credits), 0) AS credits, COALESCE(SUM(COALESCE(actual_cost_usd_micros, estimated_cost_usd_micros)), 0) AS cost").
		Where("user_id IN ?", ids).Group("user_id").Scan(&aiRows).Error
	for _, row := range aiRows {
		aiCredits[row.UserID] = row.Credits
		aiCost[row.UserID] = row.Cost
	}

	for _, user := range users {
		items = append(items, adminUserListItem{
			ID:                      user.ID,
			Username:                user.Username,
			Email:                   maskEmail(ptrString(user.Email)),
			Phone:                   maskPhone(ptrString(user.Phone)),
			IsGuest:                 user.IsGuest,
			CreatedAt:               user.CreatedAt,
			ConvertedAt:             user.ConvertedAt,
			LastActiveAt:            user.LastActiveAt,
			PlanCode:                plans[user.ID],
			CreditsRemaining:        int(credits[user.ID]),
			EntriesCount:            entries[user.ID],
			AICreditsUsed:           int(aiCredits[user.ID]),
			LifetimeAICostUSDMicros: aiCost[user.ID],
		})
	}
	return items
}

// adminUserSummary is the one-row case of adminUserSummaries. Sharing the query
// keeps the list and the detail header from drifting apart.
func adminUserSummary(user models.User) adminUserListItem {
	summaries := adminUserSummaries([]models.User{user})
	if len(summaries) == 0 {
		return adminUserListItem{ID: user.ID, Username: user.Username}
	}
	return summaries[0]
}

func (s *Server) getAdminUser(c *gin.Context) {
	id, ok := parseAdminID(c)
	if !ok {
		return
	}
	var user models.User
	if err := database.DB.First(&user, id).Error; err != nil {
		c.JSON(404, gin.H{"error": "user_not_found"})
		return
	}
	summary := adminUserSummary(user)
	admin := currentAdmin(c)
	identity := gin.H{"id": user.ID, "uuid": user.UUID, "username": user.Username, "is_guest": user.IsGuest, "created_at": user.CreatedAt, "last_active_at": user.LastActiveAt, "converted_at": user.ConvertedAt, "email": summary.Email, "phone": summary.Phone}
	if admin != nil && adminRoleRank(admin.Role) >= adminRoleRank(models.AdminRoleSupport) {
		identity["email"] = user.Email
		identity["phone"] = user.Phone
		s.auditPIIView(c, id)
	}
	counts := gin.H{}
	for name, model := range map[string]any{"accounts": &models.Account{}, "entries": &models.Entry{}, "feedback": &models.Feedback{}, "ai_events": &models.AIUsageEvent{}} {
		var count int64
		_ = database.DB.Model(model).Where("user_id = ?", id).Count(&count).Error
		counts[name] = count
	}
	features := adminFeatureFlags(id)
	var feedback []models.Feedback
	_ = database.DB.Where("user_id = ?", id).Order("created_at DESC").Limit(10).Find(&feedback).Error
	var blocks []models.AIAbuseBlock
	_ = database.DB.Where("user_id = ? AND active = ?", id, true).Order("created_at DESC").Find(&blocks).Error
	var quotes []models.LifetimeQuoteRequest
	_ = database.DB.Where("user_id = ?", id).Order("created_at DESC").Find(&quotes).Error
	var subscription models.UserSubscription
	_ = database.DB.Preload("Plan").Where("user_id = ?", id).Order("current_period_end DESC").First(&subscription).Error
	credits, _ := buildCreditSummary(billing.SubjectForUser(id), true)
	monthlyRevenue := int64(0)
	if subscription.ID > 0 && subscription.Status == "active" && subscription.CurrentPeriodEnd.After(time.Now().UTC()) {
		monthlyRevenue = int64(math.Round(float64(subscription.Plan.PriceMinor) * monthlyFactor(subscription.Plan.BillingInterval)))
	}
	var recentAICost int64
	_ = database.DB.Model(&models.AIUsageEvent{}).Where("user_id = ? AND started_at >= ?", id, time.Now().UTC().Add(-30*24*time.Hour)).Select("COALESCE(SUM(COALESCE(actual_cost_usd_micros, estimated_cost_usd_micros)),0)").Scan(&recentAICost).Error
	aiCostMinorINR := int64(math.Round(float64(recentAICost) / 1_000_000 * s.cfg.USDINRRate * 100))
	c.JSON(200, gin.H{"user": identity, "summary": summary, "counts": counts, "feature_adoption": features, "subscription": subscription, "credits": credits, "recent_feedback": feedback, "active_abuse_blocks": blocks, "lifetime_quote_requests": quotes, "economics": gin.H{"basis": "modelled_from_plan_prices", "monthly_plan_value_minor": monthlyRevenue, "ai_cost_30d_usd_micros": recentAICost, "ai_cost_30d_minor_inr": aiCostMinorINR, "gross_margin_30d_minor": monthlyRevenue - aiCostMinorINR}})
}

func adminFeatureFlags(userID uint) gin.H {
	flags := gin.H{}
	for name, table := range map[string]string{"budgets": "budgets", "subscriptions": "subscriptions", "split_bills": "split_bills", "card_statements": "card_statements", "monthly_reviews": "monthly_reviews"} {
		var count int64
		_ = database.DB.Table(table).Where("user_id = ?", userID).Count(&count).Error
		flags[name] = count > 0
	}
	return flags
}

// auditPIIView records every unmasked read of a customer's email and phone.
// It used to return early for machine-token access, which left exactly the
// least accountable reads unlogged.
func (s *Server) auditPIIView(c *gin.Context, subjectID uint) {
	adminUserID, actor := auditActor(c)
	payload, _ := json.Marshal(gin.H{"fields": []string{"email", "phone"}})
	_ = database.DB.Create(&models.AdminAuditLog{AdminUserID: adminUserID, Actor: actor, Action: "view_user_pii", SubjectType: "user", SubjectID: strconv.Itoa(int(subjectID)), Payload: string(payload), IPHash: s.hashAdminIP(requestClientIP(c))}).Error
}

func maskEmail(value string) string {
	parts := strings.Split(value, "@")
	if len(parts) != 2 || parts[0] == "" {
		return value
	}
	return string([]rune(parts[0])[0]) + "***@" + parts[1]
}

func maskPhone(value string) string {
	if value == "" {
		return value
	}
	digits := []rune(value)
	if len(digits) < 7 {
		return "•••••"
	}
	return string(digits[:3]) + "•••••" + string(digits[len(digits)-4:])
}

func ptrString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func (s *Server) listAdminUserAIUsage(c *gin.Context) { s.listAIUsageForAdmin(c, c.Param("id")) }

func (s *Server) getAdminUserCredits(c *gin.Context) {
	id, ok := parseAdminID(c)
	if !ok {
		return
	}
	var grants []models.CreditGrant
	_ = database.DB.Where("user_id = ?", id).Order("created_at DESC").Find(&grants).Error
	var ledger []models.CreditLedger
	_ = database.DB.Where("user_id = ?", id).Order("created_at DESC").Limit(200).Find(&ledger).Error
	summary, err := buildCreditSummary(billing.SubjectForUser(id), true)
	if err != nil {
		c.JSON(500, gin.H{"error": "failed_load_credits"})
		return
	}
	c.JSON(200, gin.H{"summary": summary, "grants": grants, "ledger": ledger})
}

type adminActivityItem struct {
	Type     string    `json:"type"`
	ID       uint      `json:"id"`
	At       time.Time `json:"at"`
	Label    string    `json:"label"`
	Metadata gin.H     `json:"metadata,omitempty"`
}

func (s *Server) getAdminUserActivity(c *gin.Context) {
	id, ok := parseAdminID(c)
	if !ok {
		return
	}
	items := []adminActivityItem{}
	var entries []models.Entry
	_ = database.DB.Where("user_id = ?", id).Order("created_at DESC").Limit(50).Find(&entries).Error
	for _, row := range entries {
		items = append(items, adminActivityItem{Type: "entry", ID: row.ID, At: row.CreatedAt, Label: row.Title, Metadata: gin.H{"type": row.Type, "amount": row.Amount}})
	}
	var sessions []models.AuthSession
	_ = database.DB.Where("user_id = ?", id).Order("created_at DESC").Limit(50).Find(&sessions).Error
	for _, row := range sessions {
		items = append(items, adminActivityItem{Type: "session", ID: row.ID, At: row.CreatedAt, Label: "Signed in"})
	}
	var ai []models.AIUsageEvent
	_ = database.DB.Where("user_id = ?", id).Order("started_at DESC").Limit(50).Find(&ai).Error
	for _, row := range ai {
		items = append(items, adminActivityItem{Type: "ai", ID: row.ID, At: row.StartedAt, Label: row.ActionCode, Metadata: gin.H{"status": row.Status, "credits": row.FinalCredits}})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].At.After(items[j].At) })
	if len(items) > 100 {
		items = items[:100]
	}
	c.JSON(200, gin.H{"activity": items})
}

func (s *Server) createUserCreditAdjustment(c *gin.Context) {
	id, ok := parseAdminID(c)
	if !ok {
		return
	}
	var input struct {
		Credits    int    `json:"credits"`
		ReasonCode string `json:"reason_code"`
		ExpiresAt  string `json:"expires_at"`
	}
	if c.ShouldBindJSON(&input) != nil {
		c.JSON(400, gin.H{"error": "invalid_json"})
		return
	}
	var expires *time.Time
	if input.ExpiresAt != "" {
		parsed, err := time.Parse(time.RFC3339, input.ExpiresAt)
		if err != nil {
			c.JSON(422, gin.H{"error": "invalid_adjustment"})
			return
		}
		expires = &parsed
	}
	grant, created, err := billing.NewCreditService(database.DB).GrantManualAdjustment(billing.SubjectForUser(id), input.Credits, input.ReasonCode, parseIdempotencyHeader(c), expires)
	if err != nil {
		c.JSON(422, gin.H{"error": "failed_create_credit_adjustment", "message": err.Error()})
		return
	}
	c.JSON(201, gin.H{"grant": grant, "created": created})
}

func (s *Server) listAdminIdentities(c *gin.Context) {
	var rows []models.AdminUser
	if database.DB.Preload("User").Order("created_at ASC").Find(&rows).Error != nil {
		c.JSON(500, gin.H{"error": "failed_list_admin_users"})
		return
	}
	c.JSON(200, gin.H{"admin_users": rows})
}

func (s *Server) createAdminIdentity(c *gin.Context) {
	var input struct {
		UserID uint   `json:"user_id"`
		Role   string `json:"role"`
	}
	if c.ShouldBindJSON(&input) != nil || adminRoleRank(input.Role) == 0 {
		c.JSON(422, gin.H{"error": "invalid_admin_user"})
		return
	}
	actor := currentAdmin(c)
	createdBy := "admin"
	if actor != nil {
		createdBy = strconv.Itoa(int(actor.UserID))
	}
	row := models.AdminUser{UserID: input.UserID, Role: input.Role, CreatedBy: createdBy}
	if err := database.DB.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "user_id"}}, DoUpdates: clause.AssignmentColumns([]string{"role", "disabled_at", "updated_at"})}).Create(&row).Error; err != nil {
		c.JSON(422, gin.H{"error": "failed_create_admin_user", "message": err.Error()})
		return
	}
	c.JSON(201, row)
}

func (s *Server) updateAdminIdentity(c *gin.Context) {
	id, ok := parseAdminID(c)
	if !ok {
		return
	}
	var input struct {
		Role     *string `json:"role"`
		Disabled *bool   `json:"disabled"`
	}
	if c.ShouldBindJSON(&input) != nil {
		c.JSON(400, gin.H{"error": "invalid_json"})
		return
	}
	updates := map[string]any{}
	if input.Role != nil {
		if adminRoleRank(*input.Role) == 0 {
			c.JSON(422, gin.H{"error": "invalid_role"})
			return
		}
		updates["role"] = *input.Role
	}
	if input.Disabled != nil {
		if *input.Disabled {
			now := time.Now().UTC()
			updates["disabled_at"] = &now
		} else {
			updates["disabled_at"] = nil
		}
	}
	if len(updates) == 0 {
		c.JSON(422, gin.H{"error": "updates_required"})
		return
	}
	// Disabling or demoting the last active owner locks everyone out of the
	// console; recovering needs an env change and a redeploy.
	losesOwner := false
	if role, ok := updates["role"].(string); ok && role != models.AdminRoleOwner {
		losesOwner = true
	}
	if disabledAt, ok := updates["disabled_at"]; ok && disabledAt != nil {
		losesOwner = true
	}
	if losesOwner {
		var target models.AdminUser
		if err := database.DB.First(&target, id).Error; err != nil {
			c.JSON(404, gin.H{"error": "admin_user_not_found"})
			return
		}
		if target.Role == models.AdminRoleOwner && target.DisabledAt == nil {
			var remaining int64
			if err := database.DB.Model(&models.AdminUser{}).
				Where("role = ? AND disabled_at IS NULL AND id <> ?", models.AdminRoleOwner, id).
				Count(&remaining).Error; err != nil {
				c.JSON(500, gin.H{"error": "failed_count_owners"})
				return
			}
			if remaining == 0 {
				c.JSON(422, gin.H{"error": "last_owner_required", "message": "promote another owner before changing this one"})
				return
			}
		}
	}
	if err := database.DB.Model(&models.AdminUser{}).Where("id = ?", id).Updates(updates).Error; err != nil {
		c.JSON(422, gin.H{"error": "failed_update_admin_user"})
		return
	}
	var row models.AdminUser
	_ = database.DB.Preload("User").First(&row, id).Error
	c.JSON(200, row)
}
