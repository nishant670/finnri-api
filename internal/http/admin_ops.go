package http

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"finnri/internal/database"
	"finnri/internal/models"
)

func (s *Server) listAdminAuditLog(c *gin.Context) {
	page, pageSize := parseBillingPagination(c.Query("page"), c.Query("page_size"))
	query := database.DB.Model(&models.AdminAuditLog{})
	if value := strings.TrimSpace(c.Query("admin_user_id")); value != "" {
		query = query.Where("admin_user_id = ?", value)
	}
	if value := strings.TrimSpace(c.Query("action")); value != "" {
		query = query.Where("action = ?", value)
	}
	var total int64
	_ = query.Count(&total).Error
	var rows []models.AdminAuditLog
	if err := query.Order("created_at DESC").Limit(pageSize).Offset((page - 1) * pageSize).Find(&rows).Error; err != nil {
		c.JSON(500, gin.H{"error": "failed_list_audit_log"})
		return
	}
	c.JSON(200, gin.H{"entries": rows, "page": page, "page_size": pageSize, "total": total})
}

func (s *Server) getAdminHealth(c *gin.Context) {
	started := time.Now()
	sqlDB, err := database.DB.DB()
	dbStatus := "ok"
	if err != nil || sqlDB.PingContext(c.Request.Context()) != nil {
		dbStatus = "error"
	}
	latency := time.Since(started).Milliseconds()
	circuit := s.circuitBreaker()
	circuit.mu.Lock()
	circuitState := gin.H{"open": circuit.openUntil.After(time.Now().UTC()), "open_until": circuit.openUntil, "consecutive_failures": circuit.consecutiveFailures}
	circuit.mu.Unlock()
	var pending int64
	_ = database.DB.Model(&models.Notification{}).Where("read_at IS NULL").Count(&pending).Error
	maintenanceState.RLock()
	lastRun := maintenanceState.LastRunAt
	lastError := maintenanceState.LastError
	maintenanceState.RUnlock()
	c.JSON(http.StatusOK, gin.H{"status": dbStatus, "database": gin.H{"status": dbStatus, "latency_ms": latency}, "maintenance": gin.H{"last_run_at": lastRun, "last_error": lastError}, "ai_circuit_breaker": circuitState, "pending_notifications": pending})
}

func (s *Server) exportAdminCSV(c *gin.Context) {
	resource := strings.TrimSuffix(strings.TrimSpace(c.Param("resource")), ".csv")
	switch resource {
	case "users", "subscriptions", "ai-usage":
	default:
		c.JSON(http.StatusNotFound, gin.H{"error": "export_not_found"})
		return
	}
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="finnri-admin-%s.csv"`, resource))
	writer := csv.NewWriter(c.Writer)
	defer writer.Flush()
	switch resource {
	case "users":
		_ = writer.Write([]string{"id", "username", "email_masked", "phone_masked", "is_guest", "created_at", "last_active_at"})
		var rows []models.User
		if database.DB.Order("id ASC").Limit(10000).Find(&rows).Error != nil {
			c.JSON(500, gin.H{"error": "failed_export_users"})
			return
		}
		for _, row := range rows {
			last := ""
			if row.LastActiveAt != nil {
				last = row.LastActiveAt.UTC().Format(time.RFC3339)
			}
			_ = writer.Write([]string{strconv.Itoa(int(row.ID)), row.Username, maskEmail(ptrString(row.Email)), maskPhone(ptrString(row.Phone)), strconv.FormatBool(row.IsGuest), row.CreatedAt.UTC().Format(time.RFC3339), last})
		}
	case "subscriptions":
		_ = writer.Write([]string{"id", "user_id", "plan", "status", "period_start", "period_end", "cancel_at_period_end"})
		var rows []models.UserSubscription
		if database.DB.Preload("Plan").Order("id ASC").Limit(10000).Find(&rows).Error != nil {
			c.JSON(500, gin.H{"error": "failed_export_subscriptions"})
			return
		}
		for _, row := range rows {
			_ = writer.Write([]string{strconv.Itoa(int(row.ID)), strconv.Itoa(int(row.UserID)), row.Plan.Code, row.Status, row.CurrentPeriodStart.UTC().Format(time.RFC3339), row.CurrentPeriodEnd.UTC().Format(time.RFC3339), strconv.FormatBool(row.CancelAtPeriodEnd)})
		}
	case "ai-usage":
		_ = writer.Write([]string{"id", "user_id", "request_id", "action", "status", "model", "credits", "estimated_cost_usd_micros", "actual_cost_usd_micros", "started_at"})
		var rows []models.AIUsageEvent
		if database.DB.Order("started_at DESC").Limit(10000).Find(&rows).Error != nil {
			c.JSON(500, gin.H{"error": "failed_export_ai_usage"})
			return
		}
		for _, row := range rows {
			userID := ""
			if row.UserID != nil {
				userID = strconv.Itoa(int(*row.UserID))
			}
			actual := ""
			if row.ActualCostUSDMicros != nil {
				actual = strconv.FormatInt(*row.ActualCostUSDMicros, 10)
			}
			_ = writer.Write([]string{strconv.Itoa(int(row.ID)), userID, row.RequestID, row.ActionCode, row.Status, row.Model, strconv.Itoa(row.FinalCredits), strconv.FormatInt(row.EstimatedCostUSDMicros, 10), actual, row.StartedAt.UTC().Format(time.RFC3339)})
		}
	}
}
