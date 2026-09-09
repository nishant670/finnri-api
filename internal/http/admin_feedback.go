package http

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"finnri/internal/database"
	"finnri/internal/models"
)

func (s *Server) listAdminFeedback(c *gin.Context) {
	page, pageSize := parseBillingPagination(c.Query("page"), c.Query("page_size"))
	query := database.DB.Model(&models.Feedback{})
	for column, param := range map[string]string{"status": "status", "type": "type", "area": "area", "impact": "impact"} {
		if value := strings.TrimSpace(c.Query(param)); value != "" {
			query = query.Where(column+" = ?", value)
		}
	}
	if q := strings.TrimSpace(c.Query("q")); q != "" {
		like := "%" + strings.ToLower(q) + "%"
		query = query.Where("LOWER(title) LIKE ? OR LOWER(message) LIKE ? OR LOWER(area) LIKE ?", like, like, like)
	}
	var total int64
	_ = query.Count(&total).Error
	var rows []models.Feedback
	if err := query.Preload("User").Order("created_at DESC").Limit(pageSize).Offset((page - 1) * pageSize).Find(&rows).Error; err != nil {
		c.JSON(500, gin.H{"error": "failed_list_feedback"})
		return
	}
	items := make([]gin.H, 0, len(rows))
	for _, row := range rows {
		items = append(items, gin.H{"feedback": row, "user": gin.H{"id": row.User.ID, "username": row.User.Username, "email": maskEmail(ptrString(row.User.Email))}})
	}
	c.JSON(http.StatusOK, gin.H{"feedback": items, "page": page, "page_size": pageSize, "total": total})
}

func (s *Server) getAdminFeedbackStats(c *gin.Context) {
	var rows []models.Feedback
	if err := database.DB.Find(&rows).Error; err != nil {
		c.JSON(500, gin.H{"error": "failed_load_feedback_stats"})
		return
	}
	byStatus := map[string]int{}
	byType := map[string]int{}
	byArea := map[string]int{}
	byImpact := map[string]int{}
	ages := []float64{}
	now := time.Now().UTC()
	for _, row := range rows {
		byStatus[row.Status]++
		byType[row.Type]++
		byArea[firstNonEmpty(row.Area, "unspecified")]++
		byImpact[row.Impact]++
		if row.Status != "shipped" && row.Status != "declined" {
			ages = append(ages, now.Sub(row.CreatedAt).Hours()/24)
		}
	}
	sort.Float64s(ages)
	median := 0.0
	if len(ages) > 0 {
		middle := len(ages) / 2
		if len(ages)%2 == 0 {
			median = (ages[middle-1] + ages[middle]) / 2
		} else {
			median = ages[middle]
		}
	}
	c.JSON(200, gin.H{"total": len(rows), "by_status": byStatus, "by_type": byType, "by_area": byArea, "by_impact": byImpact, "median_open_age_days": median})
}

func (s *Server) updateAdminFeedback(c *gin.Context) {
	id, ok := parseAdminID(c)
	if !ok {
		return
	}
	var input struct {
		Status     *string `json:"status"`
		AdminNotes *string `json:"admin_notes"`
	}
	if c.ShouldBindJSON(&input) != nil {
		c.JSON(400, gin.H{"error": "invalid_json"})
		return
	}
	updates := map[string]any{}
	if input.Status != nil {
		allowed := map[string]bool{"new": true, "triaged": true, "planned": true, "shipped": true, "declined": true}
		if !allowed[*input.Status] {
			c.JSON(422, gin.H{"error": "invalid_feedback_status"})
			return
		}
		updates["status"] = *input.Status
		if *input.Status == "shipped" || *input.Status == "declined" {
			if admin := currentAdmin(c); admin != nil && admin.UserID > 0 {
				updates["resolved_by"] = admin.UserID
			}
		} else {
			updates["resolved_by"] = nil
		}
	}
	if input.AdminNotes != nil {
		updates["admin_notes"] = strings.TrimSpace(*input.AdminNotes)
	}
	if len(updates) == 0 {
		c.JSON(422, gin.H{"error": "updates_required"})
		return
	}
	result := database.DB.Model(&models.Feedback{}).Where("id = ?", id).Updates(updates)
	if result.Error != nil {
		c.JSON(422, gin.H{"error": "failed_update_feedback"})
		return
	}
	if result.RowsAffected == 0 {
		c.JSON(404, gin.H{"error": "feedback_not_found"})
		return
	}
	var row models.Feedback
	_ = database.DB.First(&row, id).Error
	c.JSON(200, row)
}
