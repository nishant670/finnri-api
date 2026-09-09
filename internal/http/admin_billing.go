package http

import (
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"finnri/internal/database"
	"finnri/internal/models"
)

type adminRevenueSnapshot struct {
	Basis                string          `json:"basis"`
	Currency             string          `json:"currency"`
	ActiveSubscribers    int             `json:"active_subscribers"`
	MRRMinor             int64           `json:"modelled_mrr_minor"`
	ARRMinor             int64           `json:"modelled_arr_minor"`
	ARPUMinor            int64           `json:"modelled_arpu_minor"`
	PaidConversion       float64         `json:"paid_conversion_percent"`
	LifetimeOneTimeMinor int64           `json:"lifetime_one_time_minor"`
	NewInWindow          int64           `json:"new_in_window"`
	ChurnedInWindow      int64           `json:"inferred_churned_in_window"`
	RenewalsDue7Days     int64           `json:"renewals_due_7_days"`
	RenewalsDue30Days    int64           `json:"renewals_due_30_days"`
	CancelAtPeriodEnd    int64           `json:"cancel_at_period_end"`
	ByPlan               map[string]any  `json:"by_plan"`
	Trend                []adminMRRPoint `json:"trend"`
}

type adminMRRPoint struct {
	Date              string `json:"date"`
	MRRMinor          int64  `json:"modelled_mrr_minor"`
	ActiveSubscribers int    `json:"active_subscribers"`
}

func buildRevenueSnapshot(start, end time.Time) adminRevenueSnapshot {
	now := time.Now().UTC()
	result := adminRevenueSnapshot{Basis: "modelled_from_plan_prices", Currency: "INR", ByPlan: map[string]any{}}
	var subs []models.UserSubscription
	_ = database.DB.Preload("Plan").Where("status = ? AND current_period_end > ?", "active", now).Find(&subs).Error
	for _, sub := range subs {
		factor := monthlyFactor(sub.Plan.BillingInterval)
		if sub.Plan.BillingInterval == "lifetime_quote" {
			result.LifetimeOneTimeMinor += sub.Plan.PriceMinor
		} else {
			result.MRRMinor += int64(math.Round(float64(sub.Plan.PriceMinor) * factor))
		}
		planRow, _ := result.ByPlan[sub.Plan.Code].(map[string]any)
		if planRow == nil {
			planRow = map[string]any{"subscribers": 0, "modelled_mrr_minor": int64(0)}
		}
		planRow["subscribers"] = planRow["subscribers"].(int) + 1
		planRow["modelled_mrr_minor"] = planRow["modelled_mrr_minor"].(int64) + int64(math.Round(float64(sub.Plan.PriceMinor)*factor))
		result.ByPlan[sub.Plan.Code] = planRow
	}
	result.ActiveSubscribers = len(subs)
	result.ARRMinor = result.MRRMinor * 12
	if result.ActiveSubscribers > 0 {
		result.ARPUMinor = result.MRRMinor / int64(result.ActiveSubscribers)
	}
	_, registered, _ := userCounts()
	result.PaidConversion = ratio(float64(result.ActiveSubscribers), float64(registered))
	_ = database.DB.Model(&models.UserSubscription{}).Where("created_at >= ? AND created_at < ?", start, end).Count(&result.NewInWindow).Error
	_ = database.DB.Model(&models.UserSubscription{}).Where("current_period_end >= ? AND current_period_end < ? AND status <> ?", start, end, "active").Count(&result.ChurnedInWindow).Error
	_ = database.DB.Model(&models.UserSubscription{}).Where("status = ? AND current_period_end > ? AND current_period_end <= ?", "active", now, now.Add(7*24*time.Hour)).Count(&result.RenewalsDue7Days).Error
	_ = database.DB.Model(&models.UserSubscription{}).Where("status = ? AND current_period_end > ? AND current_period_end <= ?", "active", now, now.Add(30*24*time.Hour)).Count(&result.RenewalsDue30Days).Error
	_ = database.DB.Model(&models.UserSubscription{}).Where("status = ? AND cancel_at_period_end = ?", "active", true).Count(&result.CancelAtPeriodEnd).Error
	var historical []models.UserSubscription
	_ = database.DB.Preload("Plan").Find(&historical).Error
	trendStart := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
	if end.Sub(trendStart) > 366*24*time.Hour {
		trendStart = end.Add(-366 * 24 * time.Hour)
	}
	for day := trendStart; day.Before(end); day = day.AddDate(0, 0, 1) {
		point := adminMRRPoint{Date: day.Format("2006-01-02")}
		at := day.AddDate(0, 0, 1)
		for _, sub := range historical {
			if sub.Status == "active" && !sub.CreatedAt.After(at) && !sub.CurrentPeriodStart.After(at) && sub.CurrentPeriodEnd.After(day) && sub.Plan.BillingInterval != "lifetime_quote" {
				point.ActiveSubscribers++
				point.MRRMinor += int64(math.Round(float64(sub.Plan.PriceMinor) * monthlyFactor(sub.Plan.BillingInterval)))
			}
		}
		result.Trend = append(result.Trend, point)
	}
	return result
}

func monthlyFactor(interval string) float64 {
	switch interval {
	case "weekly":
		return 4.345
	case "monthly":
		return 1
	case "quarterly":
		return 1.0 / 3
	case "yearly":
		return 1.0 / 12
	default:
		return 0
	}
}

func (s *Server) listAdminSubscriptions(c *gin.Context) {
	page, pageSize := parseBillingPagination(c.Query("page"), c.Query("page_size"))
	query := database.DB.Model(&models.UserSubscription{})
	if status := strings.TrimSpace(c.Query("status")); status != "" {
		query = query.Where("status = ?", status)
	}
	if plan := strings.TrimSpace(c.Query("plan")); plan != "" {
		query = query.Where("plan_id IN (SELECT id FROM plans WHERE code = ?)", plan)
	}
	var total int64
	_ = query.Count(&total).Error
	var rows []models.UserSubscription
	if err := query.Preload("Plan").Preload("User").Order("created_at DESC").Limit(pageSize).Offset((page - 1) * pageSize).Find(&rows).Error; err != nil {
		c.JSON(500, gin.H{"error": "failed_list_subscriptions"})
		return
	}
	items := make([]gin.H, 0, len(rows))
	for _, row := range rows {
		items = append(items, gin.H{"subscription": row, "user": gin.H{"id": row.User.ID, "username": row.User.Username, "email": maskEmail(ptrString(row.User.Email))}})
	}
	c.JSON(200, gin.H{"subscriptions": items, "page": page, "page_size": pageSize, "total": total})
}

func (s *Server) getAdminRevenue(c *gin.Context) {
	start, end, ok := parseMetricsWindow(c)
	if !ok {
		return
	}
	c.JSON(http.StatusOK, buildRevenueSnapshot(start, end))
}

func (s *Server) listAdminPlans(c *gin.Context) {
	var rows []models.Plan
	if err := database.DB.Order("id ASC").Find(&rows).Error; err != nil {
		c.JSON(500, gin.H{"error": "failed_list_plans"})
		return
	}
	c.JSON(200, gin.H{"plans": rows, "basis": "modelled_from_plan_prices"})
}

func (s *Server) updateAdminPlan(c *gin.Context) {
	code := strings.TrimSpace(c.Param("code"))
	var input struct {
		Name             *string `json:"name"`
		PriceMinor       *int64  `json:"price_minor"`
		ListPriceMinor   *int64  `json:"list_price_minor"`
		IncludedCredits  *int    `json:"included_credits"`
		DailyCreditLimit *int    `json:"daily_credit_limit"`
		IsPublic         *bool   `json:"is_public"`
	}
	if c.ShouldBindJSON(&input) != nil {
		c.JSON(400, gin.H{"error": "invalid_json"})
		return
	}
	updates := map[string]any{}
	if input.Name != nil && strings.TrimSpace(*input.Name) != "" {
		updates["name"] = strings.TrimSpace(*input.Name)
	}
	if input.PriceMinor != nil && *input.PriceMinor >= 0 {
		updates["price_minor"] = *input.PriceMinor
	}
	if input.ListPriceMinor != nil && *input.ListPriceMinor >= 0 {
		updates["list_price_minor"] = *input.ListPriceMinor
	}
	if input.IncludedCredits != nil && *input.IncludedCredits >= 0 {
		updates["included_credits"] = *input.IncludedCredits
	}
	if input.DailyCreditLimit != nil && *input.DailyCreditLimit >= 0 {
		updates["daily_credit_limit"] = *input.DailyCreditLimit
	}
	if input.IsPublic != nil {
		updates["is_public"] = *input.IsPublic
	}
	if len(updates) == 0 {
		c.JSON(422, gin.H{"error": "valid_updates_required"})
		return
	}
	result := database.DB.Model(&models.Plan{}).Where("code = ?", code).Updates(updates)
	if result.Error != nil {
		c.JSON(422, gin.H{"error": "failed_update_plan"})
		return
	}
	if result.RowsAffected == 0 {
		c.JSON(404, gin.H{"error": "plan_not_found"})
		return
	}
	var plan models.Plan
	_ = database.DB.Where("code = ?", code).First(&plan).Error
	c.JSON(200, plan)
}

func (s *Server) updateLifetimeQuoteRequest(c *gin.Context) {
	id, ok := parseAdminID(c)
	if !ok {
		return
	}
	var input struct {
		Status string  `json:"status"`
		Notes  *string `json:"notes"`
	}
	if c.ShouldBindJSON(&input) != nil {
		c.JSON(400, gin.H{"error": "invalid_json"})
		return
	}
	allowed := map[string]bool{"requested": true, "reviewing": true, "quoted": true, "accepted": true, "declined": true, "closed": true}
	updates := map[string]any{}
	if input.Status != "" {
		if !allowed[input.Status] {
			c.JSON(422, gin.H{"error": "invalid_status"})
			return
		}
		updates["status"] = input.Status
	}
	if input.Notes != nil {
		updates["notes"] = strings.TrimSpace(*input.Notes)
	}
	if len(updates) == 0 {
		c.JSON(422, gin.H{"error": "updates_required"})
		return
	}
	result := database.DB.Model(&models.LifetimeQuoteRequest{}).Where("id = ?", id).Updates(updates)
	if result.RowsAffected == 0 {
		c.JSON(404, gin.H{"error": "lifetime_quote_not_found"})
		return
	}
	var row models.LifetimeQuoteRequest
	_ = database.DB.First(&row, id).Error
	c.JSON(200, row)
}
