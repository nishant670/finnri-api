package http

import (
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"finnri/internal/database"
	"finnri/internal/models"
)

// cardPaymentPurposeType marks both the bank-side debit of a card payment and
// anything else that settles a card rather than buying something.
//
// It exists because a card payment is not spending: the spending already
// happened, transaction by transaction, when the card was used. Counting the
// settlement again would double every rupee that passes through a credit
// card. insights.go excludes this purpose from income and expense totals for
// exactly that reason; per-account balances still count it, because the money
// really did leave the bank account.
const cardPaymentPurposeType = "card_payment"

// cardStatementResponse is a statement with everything a screen needs to
// render it without doing arithmetic — the same contract accountSummary sets.
type cardStatementResponse struct {
	models.CardStatement
	RemainingDue   models.Money                  `json:"remaining_due"`
	IsOverdue      bool                          `json:"is_overdue"`
	DaysToDue      int                           `json:"days_to_due"`
	Reconciliation statementReconciliation       `json:"reconciliation"`
	Payments       []models.CardStatementPayment `json:"payments"`
}

// --- routing helpers -------------------------------------------------------

// loadUserCard fetches an account and confirms it is a credit card belonging
// to the caller. Statements are meaningless on any other account type.
func loadUserCard(userID uint, accountID uint) (models.Account, error) {
	var account models.Account
	if err := database.DB.
		Where("id = ? AND user_id = ?", accountID, userID).
		First(&account).Error; err != nil {
		return account, err
	}
	if normalizeAccountType(account.Type) != "credit_card" {
		return account, gorm.ErrRecordNotFound
	}
	return account, nil
}

func parseIDParam(c *gin.Context, name string) (uint, bool) {
	id, err := strconv.ParseUint(c.Param(name), 10, 32)
	if err != nil || id == 0 {
		return 0, false
	}
	return uint(id), true
}

// --- handlers --------------------------------------------------------------

// listCardStatements is the account's billing history, newest first.
func (s *Server) listCardStatements(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	accountID, ok := parseIDParam(c, "id")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	if _, err := loadUserCard(userID, accountID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "card not found"})
		return
	}

	var statements []models.CardStatement
	if err := database.DB.
		Where("user_id = ? AND account_id = ?", userID, accountID).
		Order("statement_date DESC").
		Find(&statements).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_statements"})
		return
	}

	today := truncateDate(time.Now()).Format(apiDateLayout)
	responses := make([]cardStatementResponse, 0, len(statements))
	for index := range statements {
		// The history list does not reconcile every row: it is a read of many
		// statements and reconciling writes. Detail reconciles.
		responses = append(responses, cardStatementResponse{
			CardStatement: statements[index],
			RemainingDue:  remainingDue(statements[index]),
			IsOverdue:     statementIsOverdue(statements[index], today),
			DaysToDue:     daysBetween(today, statements[index].DueDate),
			Payments:      []models.CardStatementPayment{},
		})
	}
	c.JSON(http.StatusOK, responses)
}

// saveCardStatement creates or updates the bill for one statement date.
//
// Upsert rather than plain insert: (user, account, statement_date) is the
// natural key, so re-submitting the same month corrects it instead of
// creating a second bill. That is also what makes statement parsing safe to
// re-run later without producing duplicates.
func (s *Server) saveCardStatement(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	accountID, ok := parseIDParam(c, "id")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	account, err := loadUserCard(userID, accountID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "card not found"})
		return
	}

	var input cardStatementInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	if fields := input.validate(); len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_statement", "fields": fields})
		return
	}

	statementDate, _ := parseStrictAPIDate(input.StatementDate)
	cycleStart, cycleEnd := statementCycle(statementDate, account.StatementDay)

	dueDate := input.DueDate
	if dueDate == "" {
		dueDate = dueDateFor(statementDate, effectiveDueDay(account, statementDate)).Format(apiDateLayout)
	}

	var statement models.CardStatement
	err = database.DB.
		Where("user_id = ? AND account_id = ? AND statement_date = ?", userID, accountID, input.StatementDate).
		First(&statement).Error
	created := err != nil

	statement.UserID = userID
	statement.AccountID = accountID
	statement.StatementDate = input.StatementDate
	statement.CycleStart = cycleStart.Format(apiDateLayout)
	statement.CycleEnd = cycleEnd.Format(apiDateLayout)
	statement.DueDate = dueDate
	statement.TotalDue = input.TotalDue
	statement.MinimumDue = input.MinimumDue
	statement.Currency = normalizedStatementCurrency(input.Currency)
	statement.Source = normalizedStatementSource(input.Source)
	statement.Notes = input.Notes

	// Submitting an amount is what turns a draft into a real bill. Everything
	// downstream — reconciliation, reminders, the limit calculation — keys off
	// a statement no longer being a draft.
	if statement.Status == "" || statement.Status == statementStatusDraft {
		statement.Status = statementStatusUnpaid
	}
	statement.Status = deriveStatementStatus(statement.Status, statement.TotalDue, statement.PaidAmount)

	if err := database.DB.Save(&statement).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_save_statement"})
		return
	}

	// The first bill a card ever receives is the only evidence we have of when
	// it bills. Remember it so future cycles and reminders are right without
	// asking again.
	if account.StatementDay == 0 {
		if err := database.DB.Model(&account).
			Update("statement_day", statementDate.Day()).Error; err != nil {
			// Not fatal: the statement is saved and correct either way.
			log.Printf("card statement: could not infer statement_day for account %d: %v", account.ID, err)
		}
	}

	respondWithStatement(c, &statement, created)
}

// getCardStatement returns one statement, its payments, and a freshly
// computed reconciliation.
func (s *Server) getCardStatement(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	statementID, ok := parseIDParam(c, "id")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	var statement models.CardStatement
	if err := database.DB.
		Where("id = ? AND user_id = ?", statementID, userID).
		First(&statement).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "statement not found"})
		return
	}

	respondWithStatement(c, &statement, false)
}

// deleteCardStatement removes a bill and the bucket entry it owns. Real
// transactions in the cycle are untouched — they are the user's record of
// their own spending, not this statement's property.
func (s *Server) deleteCardStatement(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	statementID, ok := parseIDParam(c, "id")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	var statement models.CardStatement
	if err := database.DB.
		Where("id = ? AND user_id = ?", statementID, userID).
		First(&statement).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "statement not found"})
		return
	}

	if err := clearUnitemizedEntry(&statement); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_delete_statement"})
		return
	}
	if err := database.DB.Delete(&statement).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_delete_statement"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": true})
}

// recordCardStatementPayment logs a payment the user made in their bank's
// app. Partial payments are simply repeated calls.
func (s *Server) recordCardStatementPayment(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	statementID, ok := parseIDParam(c, "id")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	var statement models.CardStatement
	if err := database.DB.
		Where("id = ? AND user_id = ?", statementID, userID).
		First(&statement).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "statement not found"})
		return
	}
	if statement.Status == statementStatusDraft {
		c.JSON(http.StatusConflict, gin.H{"error": "statement_amount_missing"})
		return
	}

	var input cardStatementPaymentInput
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
		return
	}
	if fields := input.validate(); len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_payment", "fields": fields})
		return
	}

	paidOn := input.PaidOn
	if paidOn == "" {
		paidOn = truncateDate(time.Now()).Format(apiDateLayout)
	}

	// Paying a card from the same card is not a thing.
	if input.FromAccountID != nil && *input.FromAccountID == statement.AccountID {
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error":  "invalid_payment",
			"fields": gin.H{"from_account_id": "cannot be the card being paid"},
		})
		return
	}

	payment := models.CardStatementPayment{
		UserID:        userID,
		StatementID:   statement.ID,
		AccountID:     statement.AccountID,
		FromAccountID: input.FromAccountID,
		Amount:        input.Amount,
		PaidOn:        paidOn,
		Method:        input.Method,
		Note:          input.Note,
	}

	// Only when the user says where the money came from: one expense on that
	// bank account, so its balance drops. Nothing is written against the card
	// itself — its outstanding comes from the statement, not from ledger
	// arithmetic.
	if input.FromAccountID != nil {
		var source models.Account
		if err := database.DB.
			Where("id = ? AND user_id = ?", *input.FromAccountID, userID).
			First(&source).Error; err != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{
				"error":  "invalid_payment",
				"fields": gin.H{"from_account_id": "account not found"},
			})
			return
		}
		entry := models.Entry{
			Title:       "Credit card payment",
			Type:        "expense",
			Amount:      input.Amount,
			Currency:    statement.Currency,
			Source:      "manual",
			Category:    defaultCategory,
			Tag:         unitemizedTag,
			PurposeType: cardPaymentPurposeType,
			Notes:       input.Note,
			Date:        paidOn,
			AccountID:   input.FromAccountID,
			UserID:      userID,
		}
		if err := database.DB.Create(&entry).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_record_payment"})
			return
		}
		payment.BankEntryID = &entry.ID
	}

	if err := database.DB.Create(&payment).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_record_payment"})
		return
	}

	if err := refreshStatementPaidAmount(&statement); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_record_payment"})
		return
	}

	respondWithStatement(c, &statement, true)
}

// deleteCardStatementPayment reverses a logged payment, including the bank
// entry it created.
func (s *Server) deleteCardStatementPayment(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	statementID, ok := parseIDParam(c, "id")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	paymentID, ok := parseIDParam(c, "paymentId")
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	var statement models.CardStatement
	if err := database.DB.
		Where("id = ? AND user_id = ?", statementID, userID).
		First(&statement).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "statement not found"})
		return
	}

	var payment models.CardStatementPayment
	if err := database.DB.
		Where("id = ? AND user_id = ? AND statement_id = ?", paymentID, userID, statementID).
		First(&payment).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "payment not found"})
		return
	}

	if payment.BankEntryID != nil {
		if err := database.DB.
			Where("id = ? AND user_id = ?", *payment.BankEntryID, userID).
			Delete(&models.Entry{}).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_delete_payment"})
			return
		}
	}
	if err := database.DB.Delete(&payment).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_delete_payment"})
		return
	}
	if err := refreshStatementPaidAmount(&statement); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_delete_payment"})
		return
	}

	respondWithStatement(c, &statement, false)
}

// listUpcomingStatements is every unsettled bill across every card, soonest
// due first — what the home screen and the notification tray read.
func (s *Server) listUpcomingStatements(c *gin.Context) {
	userID := c.MustGet("userID").(uint)

	var statements []models.CardStatement
	if err := database.DB.
		Where("user_id = ? AND status IN ?", userID,
			[]string{statementStatusUnpaid, statementStatusPartial}).
		Order("due_date ASC").
		Find(&statements).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_list_statements"})
		return
	}

	today := truncateDate(time.Now()).Format(apiDateLayout)
	responses := make([]cardStatementResponse, 0, len(statements))
	for index := range statements {
		responses = append(responses, cardStatementResponse{
			CardStatement: statements[index],
			RemainingDue:  remainingDue(statements[index]),
			IsOverdue:     statementIsOverdue(statements[index], today),
			DaysToDue:     daysBetween(today, statements[index].DueDate),
			Payments:      []models.CardStatementPayment{},
		})
	}
	c.JSON(http.StatusOK, responses)
}

// --- shared plumbing -------------------------------------------------------

// refreshStatementPaidAmount recomputes the cached paid total from the
// payment rows and re-derives status from it. The rows are the record; this
// keeps the cache from ever disagreeing with them.
func refreshStatementPaidAmount(statement *models.CardStatement) error {
	var total models.Money
	if err := database.DB.Model(&models.CardStatementPayment{}).
		Select("COALESCE(SUM(amount), 0)").
		Where("statement_id = ? AND user_id = ?", statement.ID, statement.UserID).
		Scan(&total).Error; err != nil {
		return err
	}

	statement.PaidAmount = total
	statement.Status = deriveStatementStatus(statement.Status, statement.TotalDue, total)

	if err := database.DB.Model(statement).
		Updates(map[string]any{
			"paid_amount": statement.PaidAmount,
			"status":      statement.Status,
		}).Error; err != nil {
		return err
	}

	// Settling a bill releases the principal of every EMI instalment it
	// covered, which is the moment that headroom comes back on the card.
	// Removing a payment puts it back, so a deleted payment cannot leave the
	// limit permanently freed for instalments that are owed again.
	if statement.Status == statementStatusPaid {
		return releaseEMIPrincipalForStatement(statement)
	}
	return reblockEMIPrincipalForStatement(statement)
}

// respondWithStatement reconciles, loads payments and writes the response.
func respondWithStatement(c *gin.Context, statement *models.CardStatement, created bool) {
	reconciliation, err := reconcileStatement(statement)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_reconcile_statement"})
		return
	}

	var payments []models.CardStatementPayment
	if err := database.DB.
		Where("statement_id = ? AND user_id = ?", statement.ID, statement.UserID).
		Order("paid_on DESC, id DESC").
		Find(&payments).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_payments"})
		return
	}
	if payments == nil {
		payments = []models.CardStatementPayment{}
	}

	today := truncateDate(time.Now()).Format(apiDateLayout)
	response := cardStatementResponse{
		CardStatement:  *statement,
		RemainingDue:   remainingDue(*statement),
		IsOverdue:      statementIsOverdue(*statement, today),
		DaysToDue:      daysBetween(today, statement.DueDate),
		Reconciliation: reconciliation,
		Payments:       payments,
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	c.JSON(status, response)
}

// effectiveDueDay falls back to the statement day when the card has no due
// day set, which at least keeps the derived date inside the right month.
func effectiveDueDay(account models.Account, statementDate time.Time) int {
	if account.DueDay >= 1 && account.DueDay <= 31 {
		return account.DueDay
	}
	return effectiveStatementDay(account.StatementDay, statementDate)
}

// daysBetween counts whole days from `from` to `to`. Negative once the later
// date is in the past, which is what lets a screen say "3 days ago".
func daysBetween(from, to string) int {
	start, err := parseAPIDate(from)
	if err != nil {
		return 0
	}
	end, err := parseAPIDate(to)
	if err != nil {
		return 0
	}
	return int(end.Sub(start).Hours() / 24)
}
