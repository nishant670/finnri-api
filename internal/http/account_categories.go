package http

import (
	"errors"
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"finnri/internal/database"
	"finnri/internal/models"
)

// accountCategory is one category's spend on an account in a month, beside
// the month before it (P63, board R5-AcctCard-Proposals).
type accountCategory struct {
	Category       string  `json:"category"`
	Amount         float64 `json:"amount"`
	Percentage     float64 `json:"percentage"`
	PreviousAmount float64 `json:"previous_amount"`
}

type accountCategoriesResponse struct {
	AccountID     uint              `json:"account_id"`
	Month         string            `json:"month"`
	PreviousMonth string            `json:"previous_month"`
	Total         float64           `json:"total"`
	PreviousTotal float64           `json:"previous_total"`
	Categories    []accountCategory `json:"categories"`
}

type accountCategoryRow struct {
	Category string
	Total    float64
}

// listAccountCategories answers "where did the money on this account go this
// month" (P63): expenses paid from or charged to the account, by category, on
// the same terms as the dashboard's "spent" — no card bill payments, no
// investments — with the month before for comparison. `month` is YYYY-MM and
// defaults to the current month in `tz` (an IANA zone, defaulting as Ask does).
func (s *Server) listAccountCategories(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	accountID, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || accountID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_account_id"})
		return
	}
	var account models.Account
	if err := database.DB.Where("user_id = ? AND id = ?", userID, accountID).First(&account).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "account_not_found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "account_lookup_failed"})
		return
	}

	// The user's month, not the server's: at 2 am IST on the 1st it is
	// already the new month for them while UTC is still on the old one.
	location := s.questionLocation(c.Query("tz"))
	month := time.Now().In(location)
	if raw := c.Query("month"); raw != "" {
		parsed, parseErr := time.ParseInLocation("2006-01", raw, location)
		if parseErr != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_month", "fields": gin.H{"month": "must be YYYY-MM"}})
			return
		}
		month = parsed
	}
	start := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, location)
	previous := start.AddDate(0, -1, 0)

	current, err := accountCategoryTotals(userID, account.ID, start)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_account_categories"})
		return
	}
	before, err := accountCategoryTotals(userID, account.ID, previous)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_account_categories"})
		return
	}

	response := accountCategoriesResponse{
		AccountID:     account.ID,
		Month:         start.Format("2006-01"),
		PreviousMonth: previous.Format("2006-01"),
		Categories:    []accountCategory{},
	}
	for _, amount := range before {
		response.PreviousTotal += amount
	}
	for category, amount := range current {
		response.Total += amount
		response.Categories = append(response.Categories, accountCategory{
			Category:       category,
			Amount:         amount,
			PreviousAmount: before[category],
		})
	}
	for index := range response.Categories {
		if response.Total > 0 {
			response.Categories[index].Percentage = roundMoneyFloat(response.Categories[index].Amount / response.Total * 100)
		}
	}
	sort.SliceStable(response.Categories, func(i, j int) bool {
		if response.Categories[i].Amount == response.Categories[j].Amount {
			return response.Categories[i].Category < response.Categories[j].Category
		}
		return response.Categories[i].Amount > response.Categories[j].Amount
	})
	response.Total = roundMoneyFloat(response.Total)
	response.PreviousTotal = roundMoneyFloat(response.PreviousTotal)
	c.JSON(http.StatusOK, response)
}

func accountCategoryTotals(userID, accountID uint, monthStart time.Time) (map[string]float64, error) {
	start := monthStart.Format("2006-01-02")
	end := monthStart.AddDate(0, 1, -1).Format("2006-01-02")
	var rows []accountCategoryRow
	if err := database.DB.Model(&models.Entry{}).
		Select("COALESCE(NULLIF(TRIM(category), ''), 'Uncategorized') AS category, SUM(amount) AS total").
		Where("user_id = ? AND account_id = ? AND date >= ? AND date <= ? AND LOWER(type) = 'expense'"+spendingRollupClause,
			userID, accountID, start, end).
		Group("COALESCE(NULLIF(TRIM(category), ''), 'Uncategorized')").
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	totals := make(map[string]float64, len(rows))
	for _, row := range rows {
		totals[normalizedLabel(row.Category, "Uncategorized")] += row.Total
	}
	return totals, nil
}

func roundMoneyFloat(value float64) float64 {
	return math.Round(value*100) / 100
}
