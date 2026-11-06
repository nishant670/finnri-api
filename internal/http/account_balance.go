package http

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"finnri/internal/database"
	"finnri/internal/models"
)

type accountBalanceInput struct {
	CurrentBalance *models.Money `json:"current_balance"`
}

// setAccountBalance records what a bank account or wallet holds today, without
// the client sending the whole account back. The app calls it from the
// "Current balance" setup step and after a scan that printed the balance
// ("Your current Meal Wallet Balance is Rs.33184.16"). It answers with the
// account and its fresh summary, so the screen shows the new running balance
// at once.
func (s *Server) setAccountBalance(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	var input accountBalanceInput
	if err := c.ShouldBindJSON(&input); err != nil || input.CurrentBalance == nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error": "invalid_balance", "fields": gin.H{"current_balance": "is required"},
		})
		return
	}
	var account models.Account
	if err := database.DB.Where("id = ? AND user_id = ?", id, userID).First(&account).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "account_not_found"})
		return
	}
	if normalizeAccountType(account.Type) == "credit_card" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error":  "invalid_balance",
			"fields": gin.H{"current_balance": "is not for cards; a card's statement says what is owed"},
		})
		return
	}

	opening, err := openingForCurrentBalance(userID, account.ID, *input.CurrentBalance)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_account_activity"})
		return
	}
	if err := database.DB.Model(&account).Updates(map[string]any{
		"balance": opening, "balance_tracked": true,
	}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_save_balance"})
		return
	}
	account.Balance = opening
	account.BalanceTracked = true
	hydrateAccountProvider(&account)

	location := loadLocationOrIndia(c.Query("tz"), s.cfg.TZDefault)
	now := time.Now().In(location)
	monthStart, monthEnd := monthToDateWindow(now)
	totals, err := loadAccountLedgerTotals(userID, monthStart, monthEnd)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_account_activity"})
		return
	}
	c.JSON(http.StatusOK, accountWithSummary{
		Account: account,
		Summary: summariseAccount(account, totals[account.ID], cardStatementContext{}, truncateDate(now).Format(apiDateLayout)),
	})
}
