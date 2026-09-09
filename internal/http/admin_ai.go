package http

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"finnri/internal/database"
	"finnri/internal/models"
)

type adminAITimeseriesPoint struct {
	Date          string  `json:"date"`
	Events        int     `json:"events"`
	Credits       int     `json:"credits"`
	CostUSDMicros int64   `json:"cost_usd_micros"`
	SuccessRate   float64 `json:"success_rate"`
	succeeded     int
}

func (s *Server) getAIMetricsTimeseries(c *gin.Context) {
	start, end, ok := parseMetricsWindow(c)
	if !ok {
		return
	}
	if bucket := firstNonEmpty(c.Query("bucket"), "day"); bucket != "day" {
		c.JSON(422, gin.H{"error": "invalid_bucket"})
		return
	}
	var events []models.AIUsageEvent
	if err := database.DB.Where("started_at >= ? AND started_at < ?", start, end).Order("started_at ASC").Find(&events).Error; err != nil {
		c.JSON(500, gin.H{"error": "failed_load_ai_timeseries"})
		return
	}
	points := map[string]*adminAITimeseriesPoint{}
	for cursor := start; cursor.Before(end); cursor = cursor.AddDate(0, 0, 1) {
		date := cursor.Format("2006-01-02")
		points[date] = &adminAITimeseriesPoint{Date: date}
	}
	for _, event := range events {
		date := event.StartedAt.UTC().Format("2006-01-02")
		point := points[date]
		if point == nil {
			point = &adminAITimeseriesPoint{Date: date}
			points[date] = point
		}
		point.Events++
		point.Credits += event.FinalCredits
		cost := event.EstimatedCostUSDMicros
		if event.ActualCostUSDMicros != nil {
			cost = *event.ActualCostUSDMicros
		}
		point.CostUSDMicros += cost
		if event.Status == "succeeded" {
			point.succeeded++
		}
	}
	result := make([]adminAITimeseriesPoint, 0, len(points))
	for cursor := start; cursor.Before(end); cursor = cursor.AddDate(0, 0, 1) {
		point := points[cursor.Format("2006-01-02")]
		point.SuccessRate = ratio(float64(point.succeeded), float64(point.Events))
		result = append(result, *point)
	}
	c.JSON(http.StatusOK, gin.H{"series": result, "bucket": "day", "start": start, "end": end})
}

func (s *Server) listAdminAIUsage(c *gin.Context) {
	s.listAIUsageForAdmin(c, strings.TrimSpace(c.Query("user_id")))
}

func (s *Server) listAIUsageForAdmin(c *gin.Context, userID string) {
	page, pageSize := parseBillingPagination(c.Query("page"), c.Query("page_size"))
	query := database.DB.Model(&models.AIUsageEvent{})
	if userID != "" {
		id, err := strconv.ParseUint(userID, 10, 64)
		if err != nil || id == 0 {
			c.JSON(422, gin.H{"error": "invalid_user_id"})
			return
		}
		query = query.Where("user_id = ?", id)
	}
	if status := strings.TrimSpace(c.Query("status")); status != "" {
		query = query.Where("status = ?", status)
	}
	if model := strings.TrimSpace(c.Query("model")); model != "" {
		query = query.Where("model = ?", model)
	}
	if action := strings.TrimSpace(c.Query("action")); action != "" {
		query = query.Where("action_code = ?", action)
	}
	if raw := strings.TrimSpace(c.Query("start")); raw != "" {
		if value, err := time.Parse(time.RFC3339, raw); err == nil {
			query = query.Where("started_at >= ?", value)
		} else {
			c.JSON(422, gin.H{"error": "invalid_start"})
			return
		}
	}
	if raw := strings.TrimSpace(c.Query("end")); raw != "" {
		if value, err := time.Parse(time.RFC3339, raw); err == nil {
			query = query.Where("started_at < ?", value)
		} else {
			c.JSON(422, gin.H{"error": "invalid_end"})
			return
		}
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		c.JSON(500, gin.H{"error": "failed_count_ai_usage"})
		return
	}
	var events []models.AIUsageEvent
	if err := query.Order("started_at DESC").Limit(pageSize).Offset((page - 1) * pageSize).Find(&events).Error; err != nil {
		c.JSON(500, gin.H{"error": "failed_list_ai_usage"})
		return
	}
	c.JSON(200, gin.H{"events": events, "page": page, "page_size": pageSize, "total": total})
}

func (s *Server) listAdminAILimitEvents(c *gin.Context) {
	page, pageSize := parseBillingPagination(c.Query("page"), c.Query("page_size"))
	query := database.DB.Model(&models.AIUsageLimitEvent{})
	if userID := strings.TrimSpace(c.Query("user_id")); userID != "" {
		query = query.Where("user_id = ?", userID)
	}
	if reason := strings.TrimSpace(c.Query("reason")); reason != "" {
		query = query.Where("reason = ?", reason)
	}
	var total int64
	_ = query.Count(&total).Error
	var rows []models.AIUsageLimitEvent
	if err := query.Order("created_at DESC").Limit(pageSize).Offset((page - 1) * pageSize).Find(&rows).Error; err != nil {
		c.JSON(500, gin.H{"error": "failed_list_limit_events"})
		return
	}
	c.JSON(200, gin.H{"events": rows, "page": page, "page_size": pageSize, "total": total})
}

func (s *Server) getAdminCreditsSummary(c *gin.Context) {
	now := time.Now().UTC()
	var grants []models.CreditGrant
	if err := database.DB.Find(&grants).Error; err != nil {
		c.JSON(500, gin.H{"error": "failed_load_credit_summary"})
		return
	}
	type sourceSummary struct {
		Granted     int `json:"granted"`
		Consumed    int `json:"consumed"`
		Expired     int `json:"expired"`
		Outstanding int `json:"outstanding"`
	}
	bySource := map[string]sourceSummary{}
	total := sourceSummary{}
	for _, grant := range grants {
		row := bySource[grant.Source]
		row.Granted += grant.CreditsGranted
		total.Granted += grant.CreditsGranted
		consumed := grant.CreditsGranted - grant.CreditsRemaining
		row.Consumed += consumed
		total.Consumed += consumed
		if grant.ExpiresAt != nil && grant.ExpiresAt.Before(now) {
			row.Expired += grant.CreditsRemaining
			total.Expired += grant.CreditsRemaining
		} else if !grant.ValidFrom.After(now) {
			row.Outstanding += grant.CreditsRemaining
			total.Outstanding += grant.CreditsRemaining
		}
		bySource[grant.Source] = row
	}
	c.JSON(200, gin.H{"totals": total, "by_source": bySource, "as_of": now})
}

func (s *Server) listAdminCreditLedger(c *gin.Context) {
	page, pageSize := parseBillingPagination(c.Query("page"), c.Query("page_size"))
	query := database.DB.Model(&models.CreditLedger{})
	if value := strings.TrimSpace(c.Query("user_id")); value != "" {
		query = query.Where("user_id = ?", value)
	}
	if value := strings.TrimSpace(c.Query("direction")); value != "" {
		query = query.Where("direction = ?", value)
	}
	if value := strings.TrimSpace(c.Query("reason_code")); value != "" {
		query = query.Where("reason_code = ?", value)
	}
	var total int64
	_ = query.Count(&total).Error
	var rows []models.CreditLedger
	if err := query.Order("created_at DESC").Limit(pageSize).Offset((page - 1) * pageSize).Find(&rows).Error; err != nil {
		c.JSON(500, gin.H{"error": "failed_list_credit_ledger"})
		return
	}
	c.JSON(200, gin.H{"ledger": rows, "page": page, "page_size": pageSize, "total": total})
}
