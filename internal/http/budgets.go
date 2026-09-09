package http

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"finnri/internal/database"
	"finnri/internal/models"
)

const (
	budgetWarningKind  = "warning"
	budgetExceededKind = "exceeded"
)

func (s *Server) createBudget(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	var input budgetInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	if fields := input.validate(); len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_budget", "fields": fields})
		return
	}

	budget := models.Budget{UserID: userID}
	input.apply(&budget)
	if err := database.DB.Create(&budget).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_create_budget"})
		return
	}
	c.JSON(http.StatusCreated, budget)
}

func (s *Server) listBudgets(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	var budgets []models.Budget
	if err := database.DB.Where("user_id = ?", userID).Order("active desc, name asc").Find(&budgets).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_budgets"})
		return
	}
	c.JSON(http.StatusOK, budgets)
}

func (s *Server) updateBudget(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	var budget models.Budget
	if err := database.DB.Where("id = ? AND user_id = ?", id, userID).First(&budget).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "budget not found"})
		return
	}

	var input budgetInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	if fields := input.validate(); len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_budget", "fields": fields})
		return
	}

	input.apply(&budget)
	if err := database.DB.Save(&budget).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_update_budget"})
		return
	}
	c.JSON(http.StatusOK, budget)
}

func (s *Server) deleteBudget(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	err = database.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ? AND budget_id = ?", userID, id).Delete(&models.BudgetAlert{}).Error; err != nil {
			return err
		}
		result := tx.Where("id = ? AND user_id = ?", id, userID).Delete(&models.Budget{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return nil
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "budget not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_delete_budget"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "budget deleted"})
}

func maybeCreateBudgetAlertsForEntry(entry models.Entry) error {
	if !strings.EqualFold(entry.Type, "expense") {
		return nil
	}
	entryDate, err := time.Parse("2006-01-02", entry.Date)
	if err != nil {
		return nil
	}
	periodStart := time.Date(entryDate.Year(), entryDate.Month(), 1, 0, 0, 0, 0, time.UTC)
	periodEnd := periodStart.AddDate(0, 1, -1)
	periodStartText := periodStart.Format("2006-01-02")
	periodEndText := periodEnd.Format("2006-01-02")

	var budgets []models.Budget
	if err := database.DB.
		Where("user_id = ? AND active = ? AND period = ?", entry.UserID, true, budgetPeriodMonthly).
		Where("category = '' OR LOWER(category) = LOWER(?)", strings.TrimSpace(entry.Category)).
		Find(&budgets).Error; err != nil {
		return err
	}

	for _, budget := range budgets {
		total, err := budgetPeriodSpend(database.DB, entry.UserID, budget, periodStartText, periodEndText)
		if err != nil {
			return err
		}
		kind := budgetAlertKind(budget, total)
		if kind == "" {
			continue
		}
		if err := createBudgetAlertIfNeeded(database.DB, budget, periodStartText, kind, total); err != nil {
			return err
		}
	}
	return nil
}

func budgetPeriodSpend(db *gorm.DB, userID uint, budget models.Budget, periodStart, periodEnd string) (models.Money, error) {
	var entries []models.Entry
	query := db.Where("user_id = ? AND type = ? AND date >= ? AND date <= ?", userID, "expense", periodStart, periodEnd)
	if strings.TrimSpace(budget.Category) != "" {
		query = query.Where("LOWER(category) = LOWER(?)", strings.TrimSpace(budget.Category))
	}
	if err := query.Find(&entries).Error; err != nil {
		return 0, err
	}
	total := models.Money(0)
	for _, entry := range entries {
		total += entry.Amount
	}
	return total, nil
}

func budgetAlertKind(budget models.Budget, spend models.Money) string {
	if spend >= budget.LimitAmount {
		return budgetExceededKind
	}
	threshold := budget.AlertThresholdPercent
	if threshold == 0 {
		threshold = defaultBudgetAlertThreshold
	}
	thresholdAmount := budget.LimitAmount * models.Money(threshold) / 100
	if spend >= thresholdAmount {
		return budgetWarningKind
	}
	return ""
}

func createBudgetAlertIfNeeded(db *gorm.DB, budget models.Budget, periodStart, kind string, spend models.Money) error {
	return db.Transaction(func(tx *gorm.DB) error {
		var existing int64
		if err := tx.Model(&models.BudgetAlert{}).
			Where("user_id = ? AND budget_id = ? AND period_start = ? AND kind = ?",
				budget.UserID, budget.ID, periodStart, kind).
			Count(&existing).Error; err != nil {
			return err
		}
		if existing > 0 {
			return nil
		}

		title, body := budgetAlertCopy(budget, periodStart, kind, spend)
		notification := models.Notification{
			UserID: budget.UserID, Type: "budget." + kind,
			Title: title, Body: body, ActionURL: "/notifications",
		}
		if err := tx.Create(&notification).Error; err != nil {
			return err
		}
		alert := models.BudgetAlert{
			UserID: budget.UserID, BudgetID: budget.ID, PeriodStart: periodStart,
			Kind: kind, SpendAmount: spend, LimitAmount: budget.LimitAmount,
			NotificationID: &notification.ID,
		}
		return tx.Create(&alert).Error
	})
}

func budgetAlertCopy(budget models.Budget, periodStart, kind string, spend models.Money) (string, string) {
	target := budgetTargetLabel(budget)
	periodLabel := budgetPeriodLabel(periodStart)
	switch kind {
	case budgetExceededKind:
		return "Budget exceeded", fmt.Sprintf("%s spending is ₹%s against your ₹%s monthly budget for %s.", target, spend.String(), budget.LimitAmount.String(), periodLabel)
	default:
		return "Budget nearing limit", fmt.Sprintf("%s spending has reached ₹%s of your ₹%s monthly budget for %s.", target, spend.String(), budget.LimitAmount.String(), periodLabel)
	}
}

func budgetTargetLabel(budget models.Budget) string {
	if category := strings.TrimSpace(budget.Category); category != "" {
		return category
	}
	if name := strings.TrimSpace(budget.Name); name != "" {
		return name
	}
	return "Monthly"
}

func budgetPeriodLabel(periodStart string) string {
	parsed, err := time.Parse("2006-01-02", periodStart)
	if err != nil {
		return periodStart
	}
	return parsed.Format("Jan 2006")
}
