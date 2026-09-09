package http

import (
	"time"

	"finnri/internal/database"
	"finnri/internal/models"
)

/*
The statement side of an account's headline figures.

account_summary.go can only report what the ledger proves. Once a card has a
real bill, the bank's number is better, and this file is what fetches it: the
latest priced statement per card, plus anything logged since that statement
closed. summariseAccount then prefers it over the ledger formula.
*/

// currentStatementSummary is the bill a card screen leads with.
type currentStatementSummary struct {
	ID            uint         `json:"id"`
	StatementDate string       `json:"statement_date"`
	DueDate       string       `json:"due_date"`
	TotalDue      models.Money `json:"total_due"`
	MinimumDue    models.Money `json:"minimum_due"`
	PaidAmount    models.Money `json:"paid_amount"`
	RemainingDue  models.Money `json:"remaining_due"`
	Status        string       `json:"status"`
	IsOverdue     bool         `json:"is_overdue"`
	DaysToDue     int          `json:"days_to_due"`
}

// cardStatementContext is one card's statement position, assembled for the
// accounts list.
type cardStatementContext struct {
	Latest              *models.CardStatement
	SpendAfterStatement models.Money
	EMIBlockedPrincipal models.Money
}

// loadCardStatementContexts fetches the newest priced statement for every
// credit card a user owns, along with net spend logged after each one closed.
//
// Drafts are excluded: a placeholder the reminder job opened carries no
// amount, and treating a zero total as the bank's answer would report a card
// with a real balance as fully paid off.
func loadCardStatementContexts(userID uint, cardIDs []uint) (map[uint]cardStatementContext, error) {
	contexts := make(map[uint]cardStatementContext, len(cardIDs))
	if len(cardIDs) == 0 {
		return contexts, nil
	}

	// Limit held against EMI plans that have not been billed yet. A card can
	// have this without ever having had a statement, so it is loaded for every
	// card rather than only the ones with a bill.
	blocked, err := loadBlockedPrincipal(userID, cardIDs)
	if err != nil {
		return nil, err
	}
	for _, cardID := range cardIDs {
		if amount := blocked[cardID]; amount != 0 {
			contexts[cardID] = cardStatementContext{EMIBlockedPrincipal: amount}
		}
	}

	var statements []models.CardStatement
	if err := database.DB.
		Where("user_id = ? AND account_id IN ? AND status <> ?", userID, cardIDs, statementStatusDraft).
		Order("account_id ASC, statement_date DESC").
		Find(&statements).Error; err != nil {
		return nil, err
	}

	// Ordered newest-first per account, so the first row seen for an account
	// is the one that counts.
	for index := range statements {
		statement := statements[index]
		if existing, seen := contexts[statement.AccountID]; seen && existing.Latest != nil {
			continue
		}
		context := contexts[statement.AccountID]
		context.Latest = &statement
		contexts[statement.AccountID] = context
	}

	for accountID, context := range contexts {
		// Cards with blocked principal but no statement have nothing to
		// measure "since" from; their new-cycle spend is already inside the
		// ledger fallback.
		if context.Latest == nil {
			continue
		}
		spend, err := loadSpendAfter(userID, accountID, context.Latest.CycleEnd)
		if err != nil {
			return nil, err
		}
		context.SpendAfterStatement = spend
		contexts[accountID] = context
	}

	return contexts, nil
}

// loadSpendAfter is net spend on a card after a cycle closed — the new cycle,
// which the user has committed but the bank has not billed yet.
//
// Card payments are excluded. Settling a bill is not spending: the spending
// already happened, transaction by transaction, when the card was used.
func loadSpendAfter(userID, accountID uint, cycleEnd string) (models.Money, error) {
	var row struct {
		Spent    models.Money
		Credited models.Money
	}
	if err := database.DB.Model(&models.Entry{}).
		Select(`COALESCE(SUM(CASE WHEN LOWER(type) = 'expense' THEN amount ELSE 0 END), 0) AS spent,
			COALESCE(SUM(CASE WHEN LOWER(type) <> 'expense' THEN amount ELSE 0 END), 0) AS credited`).
		Where("user_id = ? AND account_id = ? AND date > ? AND COALESCE(purpose_type, '') <> ?",
			userID, accountID, cycleEnd, cardPaymentPurposeType).
		Scan(&row).Error; err != nil {
		return 0, err
	}
	return row.Spent - row.Credited, nil
}

// summariseCurrentStatement renders the bill for the accounts list.
func summariseCurrentStatement(statement models.CardStatement, today string) currentStatementSummary {
	return currentStatementSummary{
		ID:            statement.ID,
		StatementDate: statement.StatementDate,
		DueDate:       statement.DueDate,
		TotalDue:      statement.TotalDue,
		MinimumDue:    statement.MinimumDue,
		PaidAmount:    statement.PaidAmount,
		RemainingDue:  remainingDue(statement),
		Status:        statement.Status,
		IsOverdue:     statementIsOverdue(statement, today),
		DaysToDue:     daysBetween(today, statement.DueDate),
	}
}

// todayString is the date the summary is computed against, factored out so
// tests can be explicit about what day it is.
func todayString() string {
	return truncateDate(time.Now()).Format(apiDateLayout)
}
