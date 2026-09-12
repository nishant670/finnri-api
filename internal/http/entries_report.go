package http

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type TransactionReportResponse struct {
	Summary    TransactionReportSummary      `json:"summary"`
	ByCategory []TransactionReportBreakdown  `json:"by_category"`
	ByMerchant []TransactionReportBreakdown  `json:"by_merchant"`
	ByAccount  []TransactionAccountBreakdown `json:"by_account"`
	ByMonth    []TransactionMonthlyBreakdown `json:"by_month"`
	ByType     []TransactionTypeBreakdown    `json:"by_type"`
}

type TransactionReportSummary struct {
	TotalExpense     float64 `json:"total_expense"`
	TotalIncome      float64 `json:"total_income"`
	NetCashflow      float64 `json:"net_cashflow"`
	TransactionCount int     `json:"transaction_count"`
	ExpenseCount     int     `json:"expense_count"`
	IncomeCount      int     `json:"income_count"`
}

type TransactionReportBreakdown struct {
	Key              string  `json:"key"`
	Label            string  `json:"label"`
	Amount           float64 `json:"amount"`
	Percentage       float64 `json:"percentage"`
	TransactionCount int     `json:"transaction_count"`
}

type TransactionAccountBreakdown struct {
	AccountID        *uint   `json:"account_id"`
	AccountName      string  `json:"account_name"`
	Amount           float64 `json:"amount"`
	Percentage       float64 `json:"percentage"`
	TransactionCount int     `json:"transaction_count"`
}

type TransactionMonthlyBreakdown struct {
	Month            string  `json:"month"`
	Expense          float64 `json:"expense"`
	Income           float64 `json:"income"`
	NetCashflow      float64 `json:"net_cashflow"`
	TransactionCount int     `json:"transaction_count"`
}

type TransactionTypeBreakdown struct {
	Type             string  `json:"type"`
	Amount           float64 `json:"amount"`
	TransactionCount int     `json:"transaction_count"`
}

type transactionReportSummaryRow struct {
	TotalExpense     float64
	TotalIncome      float64
	TransactionCount int
	ExpenseCount     int
	IncomeCount      int
}

type transactionReportRow struct {
	Key              string
	Label            string
	Amount           float64
	TransactionCount int
}

type transactionAccountReportRow struct {
	AccountID        *uint
	AccountName      string
	Amount           float64
	TransactionCount int
}

type transactionMonthlyReportRow struct {
	Month            string
	Expense          float64
	Income           float64
	TransactionCount int
}

type transactionTypeReportRow struct {
	Type             string
	Amount           float64
	TransactionCount int
}

func (s *Server) getTransactionSummaryReport(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	query, fields := filteredEntriesQuery(userID, c)
	if len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_filters", "fields": fields})
		return
	}

	report, err := buildTransactionSummaryReport(query)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_transaction_report"})
		return
	}
	c.JSON(http.StatusOK, report)
}

func buildTransactionSummaryReport(query *gorm.DB) (TransactionReportResponse, error) {
	summary, err := loadTransactionReportSummary(query)
	if err != nil {
		return TransactionReportResponse{}, err
	}
	categories, err := loadTransactionReportBreakdown(query, "category", "expense", summary.TotalExpense)
	if err != nil {
		return TransactionReportResponse{}, err
	}
	merchants, err := loadTransactionReportBreakdown(query, "merchant", "expense", summary.TotalExpense)
	if err != nil {
		return TransactionReportResponse{}, err
	}
	accounts, err := loadTransactionReportAccounts(query, summary.TotalExpense)
	if err != nil {
		return TransactionReportResponse{}, err
	}
	months, err := loadTransactionReportMonths(query)
	if err != nil {
		return TransactionReportResponse{}, err
	}
	types, err := loadTransactionReportTypes(query)
	if err != nil {
		return TransactionReportResponse{}, err
	}
	return TransactionReportResponse{
		Summary:    summary,
		ByCategory: categories,
		ByMerchant: merchants,
		ByAccount:  accounts,
		ByMonth:    months,
		ByType:     types,
	}, nil
}

func loadTransactionReportSummary(query *gorm.DB) (TransactionReportSummary, error) {
	var row transactionReportSummaryRow
	if err := query.Session(&gorm.Session{}).
		Select(`COALESCE(SUM(CASE WHEN LOWER(entries.type) = 'expense' THEN entries.amount ELSE 0 END), 0) AS total_expense,
			COALESCE(SUM(CASE WHEN LOWER(entries.type) = 'income' THEN entries.amount ELSE 0 END), 0) AS total_income,
			COUNT(*) AS transaction_count,
			COALESCE(SUM(CASE WHEN LOWER(entries.type) = 'expense' THEN 1 ELSE 0 END), 0) AS expense_count,
			COALESCE(SUM(CASE WHEN LOWER(entries.type) = 'income' THEN 1 ELSE 0 END), 0) AS income_count`).
		Scan(&row).Error; err != nil {
		return TransactionReportSummary{}, err
	}
	return TransactionReportSummary{
		TotalExpense:     row.TotalExpense,
		TotalIncome:      row.TotalIncome,
		NetCashflow:      row.TotalIncome - row.TotalExpense,
		TransactionCount: row.TransactionCount,
		ExpenseCount:     row.ExpenseCount,
		IncomeCount:      row.IncomeCount,
	}, nil
}

// loadTransactionReportBreakdown ranks the filtered rows by one column.
//
// entryType is a parameter rather than a hardcoded "expense" because the parse
// channel asks the same question of income — "where did my money come from last
// month" is the same query with the sign flipped, and a breakdown that silently
// only ever counted expenses would answer it with an empty list.
func loadTransactionReportBreakdown(query *gorm.DB, column, entryType string, total float64) ([]TransactionReportBreakdown, error) {
	expression := "CASE WHEN TRIM(entries.category) = '' THEN 'Uncategorized' ELSE TRIM(entries.category) END"
	fallback := "Uncategorized"
	if column == "merchant" {
		expression = "CASE WHEN TRIM(entries.merchant) = '' THEN 'Unknown merchant' ELSE TRIM(entries.merchant) END"
		fallback = "Unknown merchant"
	}

	var rows []transactionReportRow
	if err := query.Session(&gorm.Session{}).
		Select(expression+" AS label, LOWER("+expression+") AS key, COALESCE(SUM(entries.amount), 0) AS amount, COUNT(*) AS transaction_count").
		Where("LOWER(entries.type) = ?", entryType).
		Group(expression).
		Order("amount DESC").
		Limit(10).
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	breakdowns := make([]TransactionReportBreakdown, 0, len(rows))
	for _, row := range rows {
		label := normalizedLabel(row.Label, fallback)
		key := normalizedLabel(row.Key, fallback)
		breakdowns = append(breakdowns, TransactionReportBreakdown{
			Key:              key,
			Label:            label,
			Amount:           row.Amount,
			Percentage:       safePercentage(row.Amount, total),
			TransactionCount: row.TransactionCount,
		})
	}
	return breakdowns, nil
}

func loadTransactionReportAccounts(query *gorm.DB, totalExpense float64) ([]TransactionAccountBreakdown, error) {
	var rows []transactionAccountReportRow
	if err := query.Session(&gorm.Session{}).
		Joins("LEFT JOIN accounts ON accounts.id = entries.account_id AND accounts.user_id = entries.user_id").
		Select(`entries.account_id AS account_id,
			CASE
				WHEN entries.account_id IS NULL THEN 'Unassigned'
				WHEN TRIM(accounts.name) <> '' THEN accounts.name
				ELSE entries.mode
			END AS account_name,
			COALESCE(SUM(entries.amount), 0) AS amount,
			COUNT(*) AS transaction_count`).
		Where("LOWER(entries.type) = ?", "expense").
		Group(`entries.account_id,
			CASE
				WHEN entries.account_id IS NULL THEN 'Unassigned'
				WHEN TRIM(accounts.name) <> '' THEN accounts.name
				ELSE entries.mode
			END`).
		Order("amount DESC").
		Limit(10).
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	accounts := make([]TransactionAccountBreakdown, 0, len(rows))
	for _, row := range rows {
		accounts = append(accounts, TransactionAccountBreakdown{
			AccountID:        row.AccountID,
			AccountName:      normalizedLabel(row.AccountName, "Unassigned"),
			Amount:           row.Amount,
			Percentage:       safePercentage(row.Amount, totalExpense),
			TransactionCount: row.TransactionCount,
		})
	}
	return accounts, nil
}

func loadTransactionReportMonths(query *gorm.DB) ([]TransactionMonthlyBreakdown, error) {
	var rows []transactionMonthlyReportRow
	monthExpression := sqlDateMonth(query, "entries.date")
	if err := query.Session(&gorm.Session{}).
		Select(monthExpression + ` AS month,
			COALESCE(SUM(CASE WHEN LOWER(entries.type) = 'expense' THEN entries.amount ELSE 0 END), 0) AS expense,
			COALESCE(SUM(CASE WHEN LOWER(entries.type) = 'income' THEN entries.amount ELSE 0 END), 0) AS income,
			COUNT(*) AS transaction_count`).
		Group(monthExpression).
		Order("month ASC").
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	months := make([]TransactionMonthlyBreakdown, 0, len(rows))
	for _, row := range rows {
		months = append(months, TransactionMonthlyBreakdown{
			Month:            row.Month,
			Expense:          row.Expense,
			Income:           row.Income,
			NetCashflow:      row.Income - row.Expense,
			TransactionCount: row.TransactionCount,
		})
	}
	return months, nil
}

func loadTransactionReportTypes(query *gorm.DB) ([]TransactionTypeBreakdown, error) {
	var rows []transactionTypeReportRow
	if err := query.Session(&gorm.Session{}).
		Select("LOWER(entries.type) AS type, COALESCE(SUM(entries.amount), 0) AS amount, COUNT(*) AS transaction_count").
		Group("LOWER(entries.type)").
		Order("type ASC").
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	types := make([]TransactionTypeBreakdown, 0, len(rows))
	for _, row := range rows {
		types = append(types, TransactionTypeBreakdown{
			Type:             row.Type,
			Amount:           row.Amount,
			TransactionCount: row.TransactionCount,
		})
	}
	return types, nil
}
