package http

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"finnri/internal/billing"
	"finnri/internal/database"
	"finnri/internal/models"
)

func (s *Server) getAIMetrics(c *gin.Context) {
	start, end, ok := parseMetricsWindow(c)
	if !ok {
		return
	}
	metrics, err := billing.BuildAIMetrics(database.DB, start, end, s.aiAlertConfig())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_build_ai_metrics"})
		return
	}
	c.JSON(http.StatusOK, metrics)
}

func (s *Server) createCreditAdjustment(c *gin.Context) {
	var input struct {
		UserID            uint   `json:"user_id"`
		GuestDeviceIDHash string `json:"guest_device_id_hash"`
		Credits           int    `json:"credits"`
		ReasonCode        string `json:"reason_code"`
		ExpiresAt         string `json:"expires_at"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	var subject billing.CreditSubject
	switch {
	case input.UserID > 0:
		subject = billing.SubjectForUser(input.UserID)
	case strings.TrimSpace(input.GuestDeviceIDHash) != "":
		subject = billing.SubjectForGuestHash(input.GuestDeviceIDHash)
	default:
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_adjustment", "fields": gin.H{"user_id": "or guest_device_id_hash is required"}})
		return
	}
	var expiresAt *time.Time
	if strings.TrimSpace(input.ExpiresAt) != "" {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(input.ExpiresAt))
		if err != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_adjustment", "fields": gin.H{"expires_at": "must be RFC3339"}})
			return
		}
		parsed = parsed.UTC()
		expiresAt = &parsed
	}

	grant, created, err := billing.NewCreditService(database.DB).GrantManualAdjustment(
		subject,
		input.Credits,
		input.ReasonCode,
		parseIdempotencyHeader(c),
		expiresAt,
	)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "failed_create_credit_adjustment", "message": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"grant": grant, "created": created})
}

func (s *Server) listLifetimeQuoteRequests(c *gin.Context) {
	page, pageSize := parseBillingPagination(c.Query("page"), c.Query("page_size"))
	query := database.DB.Model(&models.LifetimeQuoteRequest{})
	if status := strings.TrimSpace(c.Query("status")); status != "" {
		query = query.Where("status = ?", status)
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_count_lifetime_quotes"})
		return
	}
	var requests []models.LifetimeQuoteRequest
	query = database.DB.Model(&models.LifetimeQuoteRequest{})
	if status := strings.TrimSpace(c.Query("status")); status != "" {
		query = query.Where("status = ?", status)
	}
	if err := query.Order("created_at DESC").Limit(pageSize).Offset((page - 1) * pageSize).Find(&requests).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_lifetime_quotes"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"requests": requests, "page": page, "page_size": pageSize, "total": total})
}

func (s *Server) listAIAbuseBlocks(c *gin.Context) {
	page, pageSize := parseBillingPagination(c.Query("page"), c.Query("page_size"))
	query := database.DB.Model(&models.AIAbuseBlock{})
	if active := strings.TrimSpace(c.Query("active")); active != "" {
		query = query.Where("active = ?", active == "true" || active == "1")
	}
	if userID := strings.TrimSpace(c.Query("user_id")); userID != "" {
		query = query.Where("user_id = ?", userID)
	}
	if guestHash := strings.TrimSpace(c.Query("guest_device_id_hash")); guestHash != "" {
		query = query.Where("guest_device_id_hash = ?", normalizeGuestDeviceHashInput(guestHash))
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_count_ai_abuse_blocks"})
		return
	}
	var blocks []models.AIAbuseBlock
	query = database.DB.Model(&models.AIAbuseBlock{})
	if active := strings.TrimSpace(c.Query("active")); active != "" {
		query = query.Where("active = ?", active == "true" || active == "1")
	}
	if userID := strings.TrimSpace(c.Query("user_id")); userID != "" {
		query = query.Where("user_id = ?", userID)
	}
	if guestHash := strings.TrimSpace(c.Query("guest_device_id_hash")); guestHash != "" {
		query = query.Where("guest_device_id_hash = ?", normalizeGuestDeviceHashInput(guestHash))
	}
	if err := query.Order("created_at DESC").Limit(pageSize).Offset((page - 1) * pageSize).Find(&blocks).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_ai_abuse_blocks"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"blocks": blocks, "page": page, "page_size": pageSize, "total": total})
}

func (s *Server) createAIAbuseBlock(c *gin.Context) {
	var input struct {
		UserID            uint   `json:"user_id"`
		GuestDeviceIDHash string `json:"guest_device_id_hash"`
		Scope             string `json:"scope"`
		ReasonCode        string `json:"reason_code"`
		Notes             string `json:"notes"`
		ExpiresAt         string `json:"expires_at"`
		CreatedBy         string `json:"created_by"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	block, fields := aiAbuseBlockFromInput(input.UserID, input.GuestDeviceIDHash, input.Scope, input.ReasonCode, input.Notes, input.ExpiresAt, input.CreatedBy)
	if len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_ai_abuse_block", "fields": fields})
		return
	}
	if err := database.DB.Create(&block).Error; err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "failed_create_ai_abuse_block", "message": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, block)
}

func (s *Server) updateAIAbuseBlock(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "ai_abuse_block_not_found"})
		return
	}
	var input struct {
		Active    *bool  `json:"active"`
		Notes     string `json:"notes"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	updates := map[string]any{}
	if input.Active != nil {
		updates["active"] = *input.Active
	}
	if strings.TrimSpace(input.Notes) != "" {
		updates["notes"] = strings.TrimSpace(input.Notes)
	}
	if strings.TrimSpace(input.ExpiresAt) != "" {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(input.ExpiresAt))
		if err != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_ai_abuse_block", "fields": gin.H{"expires_at": "must be RFC3339"}})
			return
		}
		updates["expires_at"] = parsed.UTC()
	}
	if len(updates) == 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_ai_abuse_block", "fields": gin.H{"updates": "at least one field is required"}})
		return
	}
	var block models.AIAbuseBlock
	if err := database.DB.First(&block, id).Error; err == gorm.ErrRecordNotFound {
		c.JSON(http.StatusNotFound, gin.H{"error": "ai_abuse_block_not_found"})
		return
	} else if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_lookup_ai_abuse_block"})
		return
	}
	if err := database.DB.Model(&block).Updates(updates).Error; err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "failed_update_ai_abuse_block", "message": err.Error()})
		return
	}
	if err := database.DB.First(&block, id).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_reload_ai_abuse_block"})
		return
	}
	c.JSON(http.StatusOK, block)
}

func (s *Server) listAIModelPricing(c *gin.Context) {
	var rows []models.AIModelPricing
	if err := database.DB.Order("provider ASC, model ASC, operation ASC").Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_model_pricing"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"pricing": rows})
}

func aiAbuseBlockFromInput(userID uint, guestDeviceIDHash, scope, reasonCode, notes, expiresAtRaw, createdBy string) (models.AIAbuseBlock, gin.H) {
	fields := gin.H{}
	block := models.AIAbuseBlock{
		Scope:      strings.TrimSpace(scope),
		ReasonCode: strings.TrimSpace(reasonCode),
		Notes:      strings.TrimSpace(notes),
		Active:     true,
		CreatedBy:  strings.TrimSpace(createdBy),
	}
	if block.Scope == "" {
		block.Scope = "ai_parse"
	}
	if userID > 0 {
		block.UserID = &userID
	} else if strings.TrimSpace(guestDeviceIDHash) != "" {
		block.GuestDeviceIDHash = normalizeGuestDeviceHashInput(guestDeviceIDHash)
	} else {
		fields["user_id"] = "or guest_device_id_hash is required"
	}
	if block.Scope != "ai_parse" && block.Scope != "all_ai" {
		fields["scope"] = "must be ai_parse or all_ai"
	}
	if block.ReasonCode == "" {
		fields["reason_code"] = "is required"
	}
	if block.GuestDeviceIDHash != "" && len(block.GuestDeviceIDHash) != 64 {
		fields["guest_device_id_hash"] = "must be a raw device id or 64-character hash"
	}
	if strings.TrimSpace(expiresAtRaw) != "" {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(expiresAtRaw))
		if err != nil {
			fields["expires_at"] = "must be RFC3339"
		} else {
			parsed = parsed.UTC()
			block.ExpiresAt = &parsed
		}
	}
	return block, fields
}

func normalizeGuestDeviceHashInput(value string) string {
	value = strings.TrimSpace(value)
	if len(value) == 64 && isLowerHex(value) {
		return value
	}
	return billing.HashUsageKey(value)
}

func isLowerHex(value string) bool {
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

func (s *Server) upsertAIModelPricing(c *gin.Context) {
	var input models.AIModelPricing
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	input.Provider = strings.TrimSpace(input.Provider)
	input.Model = strings.TrimSpace(input.Model)
	input.Operation = strings.TrimSpace(input.Operation)
	fields := gin.H{}
	if input.Provider == "" {
		fields["provider"] = "is required"
	}
	if input.Model == "" {
		fields["model"] = "is required"
	}
	switch input.Operation {
	case "llm", "transcription", "credit_fallback":
	default:
		fields["operation"] = "must be llm, transcription, or credit_fallback"
	}
	if input.InputTokenUSDMicros < 0 || input.OutputTokenUSDMicros < 0 || input.AudioMinuteUSDMicros < 0 || input.RequestUSDMicros < 0 || input.CreditUSDMicros < 0 {
		fields["pricing"] = "must be non-negative"
	}
	if len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_model_pricing", "fields": fields})
		return
	}
	if input.CreditUSDMicros == 0 {
		input.CreditUSDMicros = billing.DefaultCreditCostUSDMicros
	}
	input.Active = true
	if err := database.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "provider"}, {Name: "model"}, {Name: "operation"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"input_token_usd_micros",
			"output_token_usd_micros",
			"audio_minute_usd_micros",
			"request_usd_micros",
			"credit_usd_micros",
			"active",
			"updated_at",
		}),
	}).Create(&input).Error; err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "failed_upsert_model_pricing", "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, input)
}

func (s *Server) aiAlertConfig() billing.AlertConfig {
	return billing.AlertConfig{
		DailyCostUSDMicros:         s.cfg.AIDailyCostAlertUSDMicros,
		AbuseDailyCreditsThreshold: s.cfg.AIAbuseDailyCreditsThreshold,
		FreeCostPerUserUSDMicros:   s.cfg.AIFreeCostPerUserAlertUSDMicros,
	}
}

func (s *Server) logCostControlAlerts() {
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	metrics, err := billing.BuildAIMetrics(database.DB, start, start.Add(24*time.Hour), s.aiAlertConfig())
	if err != nil {
		log.Printf("ai_cost_control_metrics_failed err=%v", err)
		return
	}
	for _, alert := range metrics.Alerts {
		encoded, err := json.Marshal(map[string]any{
			"event":                     "ai_cost_control_alert",
			"code":                      alert.Code,
			"severity":                  alert.Severity,
			"message":                   alert.Message,
			"estimated_cost_usd_micros": metrics.EstimatedCostUSDMicros,
			"free_cost_per_user_micros": metrics.FreeCostPerUserUSDMicros,
			"max_subject_daily_credits": metrics.MaxSubjectDailyCredits,
			"window_start":              metrics.WindowStart,
			"window_end":                metrics.WindowEnd,
		})
		if err != nil {
			log.Printf("ai_cost_control_alert code=%s", alert.Code)
			continue
		}
		log.Print(string(encoded))
	}
}

func parseMetricsWindow(c *gin.Context) (time.Time, time.Time, bool) {
	if date := strings.TrimSpace(c.Query("date")); date != "" {
		parsed, err := time.Parse("2006-01-02", date)
		if err != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_metrics_window", "fields": gin.H{"date": "must use YYYY-MM-DD"}})
			return time.Time{}, time.Time{}, false
		}
		start := parsed.UTC()
		return start, start.Add(24 * time.Hour), true
	}

	start := time.Now().UTC()
	end := start.Add(24 * time.Hour)
	if raw := strings.TrimSpace(c.Query("start")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_metrics_window", "fields": gin.H{"start": "must be RFC3339"}})
			return time.Time{}, time.Time{}, false
		}
		start = parsed.UTC()
	} else {
		start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
	}
	if raw := strings.TrimSpace(c.Query("end")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_metrics_window", "fields": gin.H{"end": "must be RFC3339"}})
			return time.Time{}, time.Time{}, false
		}
		end = parsed.UTC()
	} else {
		end = start.Add(24 * time.Hour)
	}
	if !end.After(start) {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_metrics_window", "fields": gin.H{"end": "must be after start"}})
		return time.Time{}, time.Time{}, false
	}
	return start, end, true
}
