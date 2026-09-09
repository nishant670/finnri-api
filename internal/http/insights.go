package http

import (
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"finnri/internal/database"
	"finnri/internal/models"

	"github.com/gin-gonic/gin"
)

type DashboardPeriod struct {
	Start string `json:"start"`
	End   string `json:"end"`
	// The window every change on this response is measured against, named so
	// the app can state it rather than saying "than last period".
	PreviousStart   string `json:"previous_start"`
	PreviousEnd     string `json:"previous_end"`
	ComparisonKind  string `json:"comparison_kind"`
	ComparisonLabel string `json:"comparison_label"`
}

type DashboardSummary struct {
	TotalSpent       float64 `json:"total_spent"`
	TotalIncome      float64 `json:"total_income"`
	DailyAverage     float64 `json:"daily_average"`
	TransactionCount int     `json:"transaction_count"`
	// Lifetime activity drives progressive disclosure; the selected period's
	// count still describes only the period.
	LifetimeTransactionCount int `json:"lifetime_transaction_count"`
	// Expenses only, where TransactionCount is every entry in the window.
	// Anything that pairs a count with a *spend* figure has to use this one:
	// "₹67,955 across 39 transactions" counted a salary inside a sentence about
	// money going out, so the two halves described different sets of rows.
	// TransactionCount keeps its meaning — the dashboard, Insights and the
	// weekly review use it as a plain activity count, which it is.
	ExpenseCount int `json:"expense_count"`
	// What the same window spent last time, and how this compares — the
	// headline version of what TopCategories already carries per category.
	// It lived only inside a `period_comparison` insight card before, which is
	// rankable and dedupable, so a caller wanting the plain number had to hope
	// the card survived. Same floor as everywhere else: SpendChange is 0 and
	// SpendChangeComparable false when the base is too thin to divide by.
	PreviousTotalSpent    float64 `json:"previous_total_spent"`
	SpendChange           float64 `json:"spend_change"`
	SpendChangeComparable bool    `json:"spend_change_comparable"`
}

type DashboardCategory struct {
	Category   string  `json:"category"`
	Amount     float64 `json:"amount"`
	Percentage float64 `json:"percentage"`
	Change     float64 `json:"change"`
	// False when the previous period was too thin to divide by. `change` is 0
	// in that case, which the app already reads as "show no trend".
	ChangeComparable bool `json:"change_comparable"`
}

type DashboardMerchant struct {
	Merchant         string  `json:"merchant"`
	Amount           float64 `json:"amount"`
	TransactionCount int     `json:"transaction_count"`
}

type DashboardAccount struct {
	AccountID   *uint   `json:"account_id"`
	AccountName string  `json:"account_name"`
	Amount      float64 `json:"amount"`
	Percentage  float64 `json:"percentage"`
}

type DashboardBudgetStatus struct {
	BudgetID              uint    `json:"budget_id"`
	Name                  string  `json:"name"`
	Category              string  `json:"category"`
	LimitAmount           float64 `json:"limit_amount"`
	SpentAmount           float64 `json:"spent_amount"`
	RemainingAmount       float64 `json:"remaining_amount"`
	Percentage            float64 `json:"percentage"`
	AlertThresholdPercent int     `json:"alert_threshold_percent"`
	DaysLeft              int     `json:"days_left"`
	Status                string  `json:"status"`
}

type DashboardDailySpend struct {
	Date   string  `json:"date"`
	Amount float64 `json:"amount"`
	Count  int     `json:"count"`
}

type DashboardReviewItem struct {
	models.Entry
	CategorySuggestions []string `json:"category_suggestions"`
}

type DashboardRecurringCandidate struct {
	CandidateKey     string  `json:"candidate_key"`
	Label            string  `json:"label"`
	Merchant         string  `json:"merchant"`
	Category         string  `json:"category"`
	AverageAmount    float64 `json:"average_amount"`
	IntervalGuess    string  `json:"interval_guess"`
	Confidence       float64 `json:"confidence"`
	Occurrences      int     `json:"occurrences"`
	LastSeenDate     string  `json:"last_seen_date"`
	NextExpectedDate string  `json:"next_expected_date"`
	ReviewDue        bool    `json:"review_due"`
}

type InsightCard struct {
	Kind             string   `json:"kind"`
	Severity         string   `json:"severity"`
	Title            string   `json:"title"`
	Body             string   `json:"body"`
	Explanation      string   `json:"explanation,omitempty"`
	ActionLabel      string   `json:"action_label,omitempty"`
	Category         string   `json:"category,omitempty"`
	Merchant         string   `json:"merchant,omitempty"`
	BudgetID         *uint    `json:"budget_id,omitempty"`
	AccountID        *uint    `json:"account_id,omitempty"`
	AccountName      string   `json:"account_name,omitempty"`
	Amount           *float64 `json:"amount,omitempty"`
	LimitAmount      *float64 `json:"limit_amount,omitempty"`
	RemainingAmount  *float64 `json:"remaining_amount,omitempty"`
	Status           string   `json:"status,omitempty"`
	Percentage       *float64 `json:"percentage,omitempty"`
	ChangePercentage *float64 `json:"change_percentage,omitempty"`
	TransactionCount *int     `json:"transaction_count,omitempty"`
	NextExpectedDate string   `json:"next_expected_date,omitempty"`
	Confidence       *float64 `json:"confidence,omitempty"`
}

type DashboardResponse struct {
	Period  DashboardPeriod  `json:"period"`
	Summary DashboardSummary `json:"summary"`
	// The complete expense breakdown for the window, highest first — not the
	// top five it is still named after. A donut that charts five of eight
	// categories is not a breakdown, and a caller could not tell whether the
	// residual was one category or a dozen. Bounded by dashboardCategoryLimit;
	// past that, the residual is `summary.total_spent` minus this list, which
	// is what "Other" is built from.
	TopCategories       []DashboardCategory           `json:"top_categories"`
	TopMerchants        []DashboardMerchant           `json:"top_merchants"`
	AccountSpending     []DashboardAccount            `json:"account_spending"`
	BudgetStatuses      []DashboardBudgetStatus       `json:"budget_statuses"`
	DailySpending       []DashboardDailySpend         `json:"daily_spending"`
	RecentTransactions  []models.Entry                `json:"recent_transactions"`
	ReviewItems         []DashboardReviewItem         `json:"review_items"`
	Insights            []InsightCard                 `json:"insights"`
	RecurringCandidates []DashboardRecurringCandidate `json:"recurring_candidates"`
	// Overview is deliberately not filtered by the selected period. See
	// DashboardOverview: the tab used to go blank every month until the first
	// transaction was captured, and a window with nothing in it is not the same
	// thing as an account with nothing in it.
	Overview DashboardOverview `json:"overview"`
}

type dashboardRange struct {
	Start          time.Time
	End            time.Time
	PreviousStart  time.Time
	PreviousEnd    time.Time
	Days           int
	ComparisonKind string
}

func (r dashboardRange) period() DashboardPeriod {
	return DashboardPeriod{
		Start:           r.Start.Format("2006-01-02"),
		End:             r.End.Format("2006-01-02"),
		PreviousStart:   r.PreviousStart.Format("2006-01-02"),
		PreviousEnd:     r.PreviousEnd.Format("2006-01-02"),
		ComparisonKind:  r.ComparisonKind,
		ComparisonLabel: comparisonLabel(r),
	}
}

func parseDashboardRange(startValue, endValue string, now time.Time) (dashboardRange, map[string]string) {
	fields := map[string]string{}
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	end := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	if startValue != "" {
		parsed, err := time.ParseInLocation("2006-01-02", startValue, now.Location())
		if err != nil {
			fields["start_date"] = "must use YYYY-MM-DD"
		} else {
			start = parsed
		}
	}
	if endValue != "" {
		parsed, err := time.ParseInLocation("2006-01-02", endValue, now.Location())
		if err != nil {
			fields["end_date"] = "must use YYYY-MM-DD"
		} else {
			end = parsed
		}
	}
	if len(fields) == 0 && start.After(end) {
		fields["end_date"] = "must be on or after start_date"
	}
	days := int(end.Sub(start).Hours()/24) + 1
	if len(fields) == 0 && days > 366 {
		fields["start_date"] = "range must not exceed 366 days"
	}
	previousStart, previousEnd, comparisonKind := previousComparisonWindow(start, end)
	return dashboardRange{
		Start: start, End: end, PreviousStart: previousStart,
		PreviousEnd: previousEnd, Days: days, ComparisonKind: comparisonKind,
	}, fields
}

func (s *Server) getDashboard(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	location := loadLocationOrIndia(c.Query("tz"), s.cfg.TZDefault)
	dateRange, fields := parseDashboardRange(c.Query("start_date"), c.Query("end_date"), time.Now().In(location))
	if len(fields) > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_range", "fields": fields})
		return
	}

	dashboard, err := buildDashboardFromDB(userID, dateRange)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_dashboard"})
		return
	}
	var budgets []models.Budget
	if err := database.DB.
		Where("user_id = ? AND active = ? AND period = ?", userID, true, budgetPeriodMonthly).
		Order("category asc, name asc").
		Find(&budgets).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_dashboard"})
		return
	}

	postProcessingEntries, err := loadDashboardPostProcessingEntries(
		userID,
		dateRange.PreviousStart.Format("2006-01-02"),
		dateRange.End.Format("2006-01-02"),
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_dashboard"})
		return
	}
	dashboard.ReviewItems = dashboardReviewItems(
		currentDashboardEntries(postProcessingEntries, dateRange),
		postProcessingEntries,
	)
	applyBudgetStatuses(postProcessingEntries, budgets, dateRange, &dashboard)
	if err := suppressReviewedRecurringCandidates(userID, &dashboard, dateRange.End); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_dashboard"})
		return
	}
	// Built from "now" rather than from the selected range: the point of the
	// overview is to be the thing that is still true when the range is empty.
	overview, err := loadDashboardOverview(userID, time.Now().In(location))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_load_dashboard"})
		return
	}
	dashboard.Overview = overview
	c.JSON(http.StatusOK, dashboard)
}

func (s *Server) getInsights(c *gin.Context) {
	s.getDashboard(c)
}

func buildDashboardFromDB(userID uint, dateRange dashboardRange) (DashboardResponse, error) {
	start := dateRange.Start.Format("2006-01-02")
	end := dateRange.End.Format("2006-01-02")
	previousStart := dateRange.PreviousStart.Format("2006-01-02")
	previousEnd := dateRange.PreviousEnd.Format("2006-01-02")

	response := DashboardResponse{
		Period:        dateRange.period(),
		TopCategories: []DashboardCategory{}, TopMerchants: []DashboardMerchant{},
		AccountSpending: []DashboardAccount{}, BudgetStatuses: []DashboardBudgetStatus{},
		DailySpending: []DashboardDailySpend{}, RecentTransactions: []models.Entry{},
		ReviewItems: []DashboardReviewItem{},
		Insights:    []InsightCard{}, RecurringCandidates: []DashboardRecurringCandidate{},
	}

	summary, err := loadDashboardSummary(userID, start, end)
	if err != nil {
		return DashboardResponse{}, err
	}
	response.Summary = summary
	if dateRange.Days > 0 {
		response.Summary.DailyAverage = response.Summary.TotalSpent / float64(dateRange.Days)
	}

	categoryPrevious, err := loadDashboardPreviousCategoryTotals(userID, previousStart, previousEnd)
	if err != nil {
		return DashboardResponse{}, err
	}
	response.TopCategories, err = loadDashboardTopCategories(userID, start, end, response.Summary.TotalSpent, categoryPrevious)
	if err != nil {
		return DashboardResponse{}, err
	}
	response.TopMerchants, err = loadDashboardTopMerchants(userID, start, end)
	if err != nil {
		return DashboardResponse{}, err
	}
	response.AccountSpending, err = loadDashboardAccountSpending(userID, start, end, response.Summary.TotalSpent)
	if err != nil {
		return DashboardResponse{}, err
	}
	response.DailySpending, err = loadDashboardDailySpending(userID, dateRange)
	if err != nil {
		return DashboardResponse{}, err
	}
	response.RecentTransactions, err = loadDashboardRecentTransactions(userID, start, end)
	if err != nil {
		return DashboardResponse{}, err
	}

	recurringEntries, err := loadDashboardRecurringEntries(userID, previousStart, end)
	if err != nil {
		return DashboardResponse{}, err
	}
	expenseAmounts, err := loadDashboardExpenseAmounts(userID, start, end)
	if err != nil {
		return DashboardResponse{}, err
	}
	previousSpent, err := loadDashboardPreviousExpenseTotal(userID, previousStart, previousEnd)
	if err != nil {
		return DashboardResponse{}, err
	}
	applySpendComparison(&response.Summary, previousSpent)
	response.RecurringCandidates = detectRecurringCandidates(recurringEntries, dateRange)
	response.Insights = buildInsightCards(response, dateRange, previousSpent, categoryPrevious, expenseAmounts)
	return response, nil
}

// notCardPaymentClause keeps credit card settlements out of every spending
// and income rollup on this screen.
//
// Paying a card bill is not spending. The spending already happened,
// transaction by transaction, when the card was used; the payment is the same
// money leaving the bank account afterwards. Counted again it would double
// every rupee that passes through a credit card, and — because the bank-side
// leg of a payment is an expense — it would also report a ₹12,400 bill
// payment as ₹12,400 of fresh spending in the month it was paid.
//
// Per-account balances deliberately still count these entries: the money
// really did leave the bank account. It is only the totals that must not.
const notCardPaymentClause = " AND COALESCE(purpose_type, '') <> '" + cardPaymentPurposeType + "'"

type dashboardSummaryRow struct {
	TotalSpent       float64
	TotalIncome      float64
	TransactionCount int
	ExpenseCount     int
}

func loadDashboardSummary(userID uint, start, end string) (DashboardSummary, error) {
	var row dashboardSummaryRow
	err := database.DB.Model(&models.Entry{}).
		Select(`COALESCE(SUM(CASE WHEN LOWER(type) = 'expense' THEN amount ELSE 0 END), 0) AS total_spent,
			COALESCE(SUM(CASE WHEN LOWER(type) = 'income' THEN amount ELSE 0 END), 0) AS total_income,
			COUNT(*) AS transaction_count,
			COUNT(CASE WHEN LOWER(type) = 'expense' THEN 1 END) AS expense_count`).
		Where("user_id = ? AND date >= ? AND date <= ?"+notCardPaymentClause+"", userID, start, end).
		Scan(&row).Error
	if err != nil {
		return DashboardSummary{}, err
	}
	var lifetimeCount int64
	if err := database.DB.Model(&models.Entry{}).
		Where("user_id = ?"+notCardPaymentClause, userID).
		Count(&lifetimeCount).Error; err != nil {
		return DashboardSummary{}, err
	}
	return DashboardSummary{
		TotalSpent:               row.TotalSpent,
		TotalIncome:              row.TotalIncome,
		TransactionCount:         row.TransactionCount,
		ExpenseCount:             row.ExpenseCount,
		LifetimeTransactionCount: int(lifetimeCount),
	}, nil
}

type dashboardCategoryRow struct {
	Category string
	Amount   float64
	Count    int
}

// The baseline carries a transaction count as well as a total, because the
// floor that keeps four-digit percentages off the screen needs both.
func loadDashboardPreviousCategoryTotals(userID uint, start, end string) (map[string]comparisonBase, error) {
	var rows []dashboardCategoryRow
	if err := database.DB.Model(&models.Entry{}).
		Select("CASE WHEN TRIM(category) = '' THEN 'Uncategorized' ELSE TRIM(category) END AS category, COALESCE(SUM(amount), 0) AS amount, COUNT(*) AS count").
		Where("user_id = ? AND date >= ? AND date <= ?"+notCardPaymentClause+" AND LOWER(type) = ?", userID, start, end, "expense").
		Group("CASE WHEN TRIM(category) = '' THEN 'Uncategorized' ELSE TRIM(category) END").
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	totals := map[string]comparisonBase{}
	for _, row := range rows {
		totals[normalizedLabel(row.Category, "Uncategorized")] = comparisonBase{
			Amount: row.Amount,
			Count:  row.Count,
		}
	}
	return totals, nil
}

// dashboardCategoryLimit bounds the breakdown without pretending to rank it.
// The canonical set is 8 and users may add their own, so this has to clear the
// realistic case comfortably; it exists only so a user with hundreds of
// hand-typed categories cannot make the payload unbounded. Anything past the
// 25th largest category is a rounding error, and it is still counted — in
// `summary.total_spent`, which is what the app's "Other" is derived from.
const dashboardCategoryLimit = 25

func loadDashboardTopCategories(userID uint, start, end string, totalSpent float64, previous map[string]comparisonBase) ([]DashboardCategory, error) {
	var rows []dashboardCategoryRow
	if err := database.DB.Model(&models.Entry{}).
		Select("CASE WHEN TRIM(category) = '' THEN 'Uncategorized' ELSE TRIM(category) END AS category, COALESCE(SUM(amount), 0) AS amount").
		Where("user_id = ? AND date >= ? AND date <= ?"+notCardPaymentClause+" AND LOWER(type) = ?", userID, start, end, "expense").
		Group("CASE WHEN TRIM(category) = '' THEN 'Uncategorized' ELSE TRIM(category) END").
		Order("amount DESC").
		Limit(dashboardCategoryLimit).
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	categories := make([]DashboardCategory, 0, len(rows))
	for _, row := range rows {
		category := normalizedLabel(row.Category, "Uncategorized")
		categories = append(categories, newDashboardCategory(
			category, row.Amount, totalSpent, previous[category],
		))
	}
	return categories, nil
}

// newDashboardCategory is the one place a category change is decided, so the
// floor cannot be applied on one code path and forgotten on the other.
func newDashboardCategory(
	category string,
	amount, totalSpent float64,
	base comparisonBase,
) DashboardCategory {
	entry := DashboardCategory{
		Category:   category,
		Amount:     amount,
		Percentage: safePercentage(amount, totalSpent),
	}
	if base.publishable() {
		entry.Change = percentageChange(amount, base.Amount)
		entry.ChangeComparable = true
	}
	return entry
}

func loadDashboardTopMerchants(userID uint, start, end string) ([]DashboardMerchant, error) {
	var rows []DashboardMerchant
	if err := database.DB.Model(&models.Entry{}).
		Select("TRIM(merchant) AS merchant, COALESCE(SUM(amount), 0) AS amount, COUNT(*) AS transaction_count").
		Where("user_id = ? AND date >= ? AND date <= ?"+notCardPaymentClause+" AND LOWER(type) = ? AND TRIM(merchant) <> ?", userID, start, end, "expense", "").
		Group("TRIM(merchant)").
		Order("amount DESC").
		Limit(5).
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []DashboardMerchant{}
	}
	return rows, nil
}

type dashboardAccountRow struct {
	AccountID   sql.NullInt64
	AccountName string
	Amount      float64
}

func loadDashboardAccountSpending(userID uint, start, end string, totalSpent float64) ([]DashboardAccount, error) {
	var rows []dashboardAccountRow
	if err := database.DB.Table("entries").
		Select(`entries.account_id AS account_id,
			CASE
				WHEN entries.account_id IS NULL THEN 'Unassigned'
				WHEN TRIM(accounts.name) <> '' THEN accounts.name
				ELSE entries.mode
			END AS account_name,
			COALESCE(SUM(entries.amount), 0) AS amount`).
		Joins("LEFT JOIN accounts ON accounts.id = entries.account_id AND accounts.user_id = entries.user_id").
		Where("entries.user_id = ? AND entries.date >= ? AND entries.date <= ? AND LOWER(entries.type) = ?", userID, start, end, "expense").
		Group(`entries.account_id,
			CASE
				WHEN entries.account_id IS NULL THEN 'Unassigned'
				WHEN TRIM(accounts.name) <> '' THEN accounts.name
				ELSE entries.mode
			END`).
		Order("amount DESC").
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	accounts := make([]DashboardAccount, 0, len(rows))
	for _, row := range rows {
		var accountID *uint
		if row.AccountID.Valid {
			id := uint(row.AccountID.Int64)
			accountID = &id
		}
		accounts = append(accounts, DashboardAccount{
			AccountID:   accountID,
			AccountName: normalizedLabel(row.AccountName, "Unassigned"),
			Amount:      row.Amount,
			Percentage:  safePercentage(row.Amount, totalSpent),
		})
	}
	return accounts, nil
}

type dashboardDailySpendRow struct {
	Date   string
	Amount float64
	Count  int
}

func loadDashboardDailySpending(userID uint, dateRange dashboardRange) ([]DashboardDailySpend, error) {
	start := dateRange.Start.Format("2006-01-02")
	end := dateRange.End.Format("2006-01-02")
	var rows []dashboardDailySpendRow
	if err := database.DB.Model(&models.Entry{}).
		Select("date, COALESCE(SUM(amount), 0) AS amount, COUNT(*) AS count").
		Where("user_id = ? AND date >= ? AND date <= ?"+notCardPaymentClause+" AND LOWER(type) = ?", userID, start, end, "expense").
		Group("date").
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	byDate := map[string]DashboardDailySpend{}
	for _, row := range rows {
		byDate[row.Date] = DashboardDailySpend{Date: row.Date, Amount: row.Amount, Count: row.Count}
	}
	daily := make([]DashboardDailySpend, 0, dateRange.Days)
	for day := dateRange.Start; !day.After(dateRange.End); day = day.AddDate(0, 0, 1) {
		date := day.Format("2006-01-02")
		if spend, ok := byDate[date]; ok {
			daily = append(daily, spend)
			continue
		}
		daily = append(daily, DashboardDailySpend{Date: date})
	}
	return daily, nil
}

func loadDashboardRecentTransactions(userID uint, start, end string) ([]models.Entry, error) {
	var entries []models.Entry
	if err := database.DB.Preload("Account").
		Where("user_id = ? AND date >= ? AND date <= ?"+notCardPaymentClause+"", userID, start, end).
		Order("date desc, created_at desc").
		Limit(5).
		Find(&entries).Error; err != nil {
		return nil, err
	}
	if entries == nil {
		entries = []models.Entry{}
	}
	return entries, nil
}

func loadDashboardPostProcessingEntries(userID uint, start, end string) ([]models.Entry, error) {
	var entries []models.Entry
	if err := database.DB.Preload("Account").
		Where("user_id = ? AND date >= ? AND date <= ?"+notCardPaymentClause+"", userID, start, end).
		Order("date desc, created_at desc").
		Find(&entries).Error; err != nil {
		return nil, err
	}
	if entries == nil {
		entries = []models.Entry{}
	}
	return entries, nil
}

func currentDashboardEntries(entries []models.Entry, dateRange dashboardRange) []models.Entry {
	start := dateRange.Start.Format("2006-01-02")
	end := dateRange.End.Format("2006-01-02")
	current := make([]models.Entry, 0, len(entries))
	for _, entry := range entries {
		if entry.Date >= start && entry.Date <= end {
			current = append(current, entry)
		}
	}
	sort.Slice(current, func(i, j int) bool {
		if current[i].Date == current[j].Date {
			return current[i].CreatedAt.After(current[j].CreatedAt)
		}
		return current[i].Date > current[j].Date
	})
	return current
}

func loadDashboardRecurringEntries(userID uint, start, end string) ([]models.Entry, error) {
	var entries []models.Entry
	if err := database.DB.Preload("Account").
		Where("user_id = ? AND date >= ? AND date <= ?"+notCardPaymentClause+" AND LOWER(type) = ?", userID, start, end, "expense").
		Order("date asc, created_at asc").
		Find(&entries).Error; err != nil {
		return nil, err
	}
	if entries == nil {
		entries = []models.Entry{}
	}
	return entries, nil
}

func loadDashboardExpenseAmounts(userID uint, start, end string) ([]float64, error) {
	var amounts []float64
	if err := database.DB.Model(&models.Entry{}).
		Select("amount").
		Where("user_id = ? AND date >= ? AND date <= ?"+notCardPaymentClause+" AND LOWER(type) = ?", userID, start, end, "expense").
		Scan(&amounts).Error; err != nil {
		return nil, err
	}
	if amounts == nil {
		amounts = []float64{}
	}
	return amounts, nil
}

func loadDashboardPreviousExpenseTotal(userID uint, start, end string) (comparisonBase, error) {
	var row struct {
		Amount float64
		Count  int
	}
	err := database.DB.Model(&models.Entry{}).
		Select("COALESCE(SUM(amount), 0) AS amount, COUNT(*) AS count").
		Where("user_id = ? AND date >= ? AND date <= ?"+notCardPaymentClause+" AND LOWER(type) = ?", userID, start, end, "expense").
		Scan(&row).Error
	return comparisonBase{Amount: row.Amount, Count: row.Count}, err
}

func buildDashboard(entries []models.Entry, dateRange dashboardRange) DashboardResponse {
	start := dateRange.Start.Format("2006-01-02")
	end := dateRange.End.Format("2006-01-02")
	previousStart := dateRange.PreviousStart.Format("2006-01-02")
	previousEnd := dateRange.PreviousEnd.Format("2006-01-02")

	current := make([]models.Entry, 0)
	previous := make([]models.Entry, 0)
	for _, entry := range entries {
		switch {
		case entry.Date >= start && entry.Date <= end:
			current = append(current, entry)
		case entry.Date >= previousStart && entry.Date <= previousEnd:
			previous = append(previous, entry)
		}
	}

	response := DashboardResponse{
		Period:        dateRange.period(),
		TopCategories: []DashboardCategory{}, TopMerchants: []DashboardMerchant{},
		AccountSpending: []DashboardAccount{}, BudgetStatuses: []DashboardBudgetStatus{},
		DailySpending:      []DashboardDailySpend{},
		RecentTransactions: []models.Entry{},
		ReviewItems:        []DashboardReviewItem{},
		Insights:           []InsightCard{}, RecurringCandidates: []DashboardRecurringCandidate{},
	}
	response.Summary.LifetimeTransactionCount = len(entries)

	categoryCurrent := map[string]float64{}
	categoryPrevious := map[string]comparisonBase{}
	merchantCurrent := map[string]*DashboardMerchant{}
	accountCurrent := map[string]*DashboardAccount{}
	dailyCurrent := map[string]*DashboardDailySpend{}
	expenseAmounts := []float64{}

	for _, entry := range previous {
		if strings.EqualFold(entry.Type, "expense") {
			label := normalizedLabel(entry.Category, "Uncategorized")
			base := categoryPrevious[label]
			base.Amount += entry.Amount.Float64()
			base.Count++
			categoryPrevious[label] = base
		}
	}
	for _, entry := range current {
		response.Summary.TransactionCount++
		amount := entry.Amount.Float64()
		if strings.EqualFold(entry.Type, "income") {
			response.Summary.TotalIncome += amount
			continue
		}
		if !strings.EqualFold(entry.Type, "expense") {
			continue
		}
		response.Summary.TotalSpent += amount
		response.Summary.ExpenseCount++
		expenseAmounts = append(expenseAmounts, amount)
		if dailyCurrent[entry.Date] == nil {
			dailyCurrent[entry.Date] = &DashboardDailySpend{Date: entry.Date}
		}
		dailyCurrent[entry.Date].Amount += amount
		dailyCurrent[entry.Date].Count++

		category := normalizedLabel(entry.Category, "Uncategorized")
		categoryCurrent[category] += amount

		merchant := strings.TrimSpace(entry.Merchant)
		if merchant != "" {
			if merchantCurrent[merchant] == nil {
				merchantCurrent[merchant] = &DashboardMerchant{Merchant: merchant}
			}
			merchantCurrent[merchant].Amount += amount
			merchantCurrent[merchant].TransactionCount++
		}

		accountKey, accountName := "unassigned", "Unassigned"
		var accountID *uint
		if entry.AccountID != nil {
			accountID = entry.AccountID
			accountKey = fmt.Sprintf("%d", *entry.AccountID)
			accountName = entry.Mode
			if entry.Account != nil && strings.TrimSpace(entry.Account.Name) != "" {
				accountName = entry.Account.Name
			}
		}
		if accountCurrent[accountKey] == nil {
			accountCurrent[accountKey] = &DashboardAccount{AccountID: accountID, AccountName: accountName}
		}
		accountCurrent[accountKey].Amount += amount
	}

	if dateRange.Days > 0 {
		response.Summary.DailyAverage = response.Summary.TotalSpent / float64(dateRange.Days)
	}
	for day := dateRange.Start; !day.After(dateRange.End); day = day.AddDate(0, 0, 1) {
		date := day.Format("2006-01-02")
		if dailyCurrent[date] != nil {
			response.DailySpending = append(response.DailySpending, *dailyCurrent[date])
			continue
		}
		response.DailySpending = append(response.DailySpending, DashboardDailySpend{Date: date})
	}
	for category, amount := range categoryCurrent {
		response.TopCategories = append(response.TopCategories, newDashboardCategory(
			category, amount, response.Summary.TotalSpent, categoryPrevious[category],
		))
	}
	sort.Slice(response.TopCategories, func(i, j int) bool {
		return response.TopCategories[i].Amount > response.TopCategories[j].Amount
	})
	if len(response.TopCategories) > dashboardCategoryLimit {
		response.TopCategories = response.TopCategories[:dashboardCategoryLimit]
	}

	for _, merchant := range merchantCurrent {
		response.TopMerchants = append(response.TopMerchants, *merchant)
	}
	sort.Slice(response.TopMerchants, func(i, j int) bool {
		return response.TopMerchants[i].Amount > response.TopMerchants[j].Amount
	})
	if len(response.TopMerchants) > 5 {
		response.TopMerchants = response.TopMerchants[:5]
	}

	for _, account := range accountCurrent {
		account.Percentage = safePercentage(account.Amount, response.Summary.TotalSpent)
		response.AccountSpending = append(response.AccountSpending, *account)
	}
	sort.Slice(response.AccountSpending, func(i, j int) bool {
		return response.AccountSpending[i].Amount > response.AccountSpending[j].Amount
	})

	sort.Slice(current, func(i, j int) bool {
		if current[i].Date == current[j].Date {
			return current[i].CreatedAt.After(current[j].CreatedAt)
		}
		return current[i].Date > current[j].Date
	})
	response.ReviewItems = dashboardReviewItems(current, entries)
	if len(current) > 5 {
		current = current[:5]
	}
	response.RecentTransactions = current
	previousSpent := previousExpenseBase(previous)
	applySpendComparison(&response.Summary, previousSpent)
	response.RecurringCandidates = detectRecurringCandidates(entries, dateRange)
	response.Insights = buildInsightCards(
		response, dateRange, previousSpent, categoryPrevious, expenseAmounts,
	)
	return response
}

func dashboardReviewItems(currentEntries, allEntries []models.Entry) []DashboardReviewItem {
	items := []DashboardReviewItem{}
	for _, entry := range currentEntries {
		category := strings.TrimSpace(entry.Category)
		if category == "" || strings.EqualFold(category, "uncategorized") || entry.AccountID == nil {
			items = append(items, DashboardReviewItem{
				Entry:               entry,
				CategorySuggestions: categorySuggestionsForReviewItem(entry, allEntries),
			})
			if len(items) == 10 {
				return items
			}
		}
	}
	return items
}

func categorySuggestionsForReviewItem(target models.Entry, entries []models.Entry) []string {
	type scoredCategory struct {
		category string
		score    int
	}
	categoryScores := map[string]int{}
	targetMerchant := normalizeInsightMatch(target.Merchant)
	targetTitle := normalizeInsightMatch(target.Title)

	for _, entry := range entries {
		if entry.ID != 0 && target.ID != 0 && entry.ID == target.ID {
			continue
		}
		category := normalizedLabel(entry.Category, "")
		if category == "" || strings.EqualFold(category, "uncategorized") {
			continue
		}

		score := 0
		if targetMerchant != "" && normalizeInsightMatch(entry.Merchant) == targetMerchant {
			score += 3
		}
		if targetTitle != "" && normalizeInsightMatch(entry.Title) == targetTitle {
			score += 2
		}
		if score == 0 {
			continue
		}
		categoryScores[category] += score
	}

	scored := make([]scoredCategory, 0, len(categoryScores))
	for category, score := range categoryScores {
		scored = append(scored, scoredCategory{category: category, score: score})
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		return scored[i].category < scored[j].category
	})

	suggestions := []string{}
	for _, item := range scored {
		suggestions = append(suggestions, item.category)
		if len(suggestions) == 3 {
			break
		}
	}
	return suggestions
}

func normalizeInsightMatch(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), " ")
}

// insightCategoryCandidates is how far down the breakdown an alert may look.
// It is the old TopCategories cap, kept here so widening that list for the
// charts could not quietly change which alerts users see.
const insightCategoryCandidates = 5

func firstN[T any](items []T, n int) []T {
	if len(items) <= n {
		return items
	}
	return items[:n]
}

func buildInsightCards(
	dashboard DashboardResponse,
	dateRange dashboardRange,
	previousSpent comparisonBase,
	categoryPrevious map[string]comparisonBase,
	expenseAmounts []float64,
) []InsightCard {
	cards := []InsightCard{}
	previousLabel := formatComparisonWindow(dateRange.PreviousStart, dateRange.PreviousEnd)

	// No card at all when the baseline is too thin. A comparison the user
	// cannot trust is worse than no comparison: it is the whole reason the
	// Insights tab stopped being believable.
	if previousSpent.publishable() && dashboard.Summary.TotalSpent > 0 {
		change := percentageChange(dashboard.Summary.TotalSpent, previousSpent.Amount)
		amount := dashboard.Summary.TotalSpent
		cards = append(cards, InsightCard{
			Kind: "period_comparison", Severity: "info", Title: "Period comparison",
			Body: fmt.Sprintf("Spending is %s %s than %s.",
				formatChangeMagnitude(change), changeDirection(change), previousLabel),
			Explanation:      comparisonExplanation(dateRange),
			ActionLabel:      "Review period transactions",
			Amount:           &amount,
			ChangePercentage: &change,
		})
	}

	// Only the largest few categories can raise an alert. TopCategories now
	// carries the whole breakdown so the donut can chart it, but a category
	// ranked twelfth rising 20% is arithmetic, not something worth putting on
	// the screen — and the loop takes the first match, so without this bound a
	// long tail could out-shout the categories that actually move the total.
	for _, category := range firstN(dashboard.TopCategories, insightCategoryCandidates) {
		if category.ChangeComparable && category.Change >= 20 {
			amount := category.Amount
			percentage := category.Percentage
			change := category.Change
			cards = append(cards, InsightCard{
				Kind: "category_increase", Severity: "warning", Title: category.Category + " increased",
				Body: fmt.Sprintf("%s spending is %s higher than %s.",
					category.Category, formatChangeMagnitude(change), previousLabel),
				Explanation: fmt.Sprintf(
					"Flags a category that rose at least 20%%. %s",
					comparisonExplanation(dateRange),
				),
				ActionLabel:      "Open category",
				Category:         category.Category,
				Amount:           &amount,
				Percentage:       &percentage,
				ChangePercentage: &change,
			})
			break
		}
	}
	if len(dashboard.TopMerchants) > 0 {
		top := dashboard.TopMerchants[0]
		amount := top.Amount
		count := top.TransactionCount
		cards = append(cards, InsightCard{
			Kind: "top_merchant", Severity: "info", Title: "Top merchant",
			Body:             fmt.Sprintf("%s led confirmed spending in the selected period.", top.Merchant),
			Explanation:      "Finds the merchant with the highest confirmed expense total in the selected period.",
			ActionLabel:      "Open merchant",
			Merchant:         top.Merchant,
			Amount:           &amount,
			TransactionCount: &count,
		})
	}
	if len(dashboard.AccountSpending) > 0 {
		top := dashboard.AccountSpending[0]
		amount := top.Amount
		percentage := top.Percentage
		cards = append(cards, InsightCard{
			Kind: "account_usage", Severity: "info", Title: "Most-used account",
			Body:        fmt.Sprintf("%s handled %.0f%% of spending.", top.AccountName, top.Percentage),
			Explanation: "Ranks linked accounts and payment sources by confirmed expense total.",
			ActionLabel: "Review account spend",
			AccountID:   top.AccountID,
			AccountName: top.AccountName,
			Amount:      &amount,
			Percentage:  &percentage,
		})
	}
	if unusual := unusualExpense(expenseAmounts); unusual > 0 {
		amount := unusual
		cards = append(cards, InsightCard{
			Kind: "unusual_spending", Severity: "warning", Title: "Unusual spending",
			Body:        "One confirmed expense was substantially above this period's average.",
			Explanation: "Flags an expense that is substantially above this period's average expense size.",
			ActionLabel: "Find large expenses",
			Amount:      &amount,
		})
	}
	if len(dashboard.RecurringCandidates) > 0 {
		candidate := dashboard.RecurringCandidates[0]
		amount := candidate.AverageAmount
		confidence := candidate.Confidence
		cards = append(cards, InsightCard{
			Kind: "recurring_candidate", Severity: "info", Title: "Recurring spend to review",
			Body:             fmt.Sprintf("%s appears to repeat and is ready for review.", candidate.Label),
			Explanation:      "Detects repeated merchant or category patterns that look weekly or monthly.",
			ActionLabel:      "Review recurring pattern",
			Category:         candidate.Category,
			Merchant:         candidate.Merchant,
			Amount:           &amount,
			NextExpectedDate: candidate.NextExpectedDate,
			Confidence:       &confidence,
		})
	}
	return cards
}

func applyBudgetStatuses(entries []models.Entry, budgets []models.Budget, dateRange dashboardRange, dashboard *DashboardResponse) {
	if dashboard == nil || len(budgets) == 0 {
		return
	}
	currentStart := dateRange.Start.Format("2006-01-02")
	currentEnd := dateRange.End.Format("2006-01-02")
	daysLeft := budgetDaysLeft(dateRange.End)
	statuses := make([]DashboardBudgetStatus, 0, len(budgets))
	for _, budget := range budgets {
		spent := budgetSpendFromEntries(entries, budget, currentStart, currentEnd)
		limit := budget.LimitAmount.Float64()
		spentValue := spent.Float64()
		remaining := math.Max(0, limit-spentValue)
		status := "safe"
		if spent >= budget.LimitAmount {
			status = "exceeded"
		} else {
			threshold := budget.AlertThresholdPercent
			if threshold == 0 {
				threshold = defaultBudgetAlertThreshold
			}
			if safePercentage(spentValue, limit) >= float64(threshold) {
				status = "watch"
			}
		}
		threshold := budget.AlertThresholdPercent
		if threshold == 0 {
			threshold = defaultBudgetAlertThreshold
		}
		statuses = append(statuses, DashboardBudgetStatus{
			BudgetID: budget.ID, Name: budget.Name, Category: budget.Category,
			LimitAmount: limit, SpentAmount: spentValue, RemainingAmount: remaining,
			Percentage: safePercentage(spentValue, limit), AlertThresholdPercent: threshold,
			DaysLeft: daysLeft, Status: status,
		})
	}
	sort.Slice(statuses, func(i, j int) bool {
		if budgetStatusRank(statuses[i].Status) != budgetStatusRank(statuses[j].Status) {
			return budgetStatusRank(statuses[i].Status) > budgetStatusRank(statuses[j].Status)
		}
		return statuses[i].Percentage > statuses[j].Percentage
	})
	dashboard.BudgetStatuses = statuses
	dashboard.Insights = append(dashboard.Insights, buildBudgetInsightCards(statuses)...)
}

func budgetSpendFromEntries(entries []models.Entry, budget models.Budget, start, end string) models.Money {
	total := models.Money(0)
	category := strings.TrimSpace(budget.Category)
	for _, entry := range entries {
		if entry.Date < start || entry.Date > end || !strings.EqualFold(entry.Type, "expense") {
			continue
		}
		if category != "" && !strings.EqualFold(strings.TrimSpace(entry.Category), category) {
			continue
		}
		total += entry.Amount
	}
	return total
}

func budgetDaysLeft(end time.Time) int {
	monthEnd := time.Date(end.Year(), end.Month()+1, 0, 0, 0, 0, 0, end.Location())
	left := int(monthEnd.Sub(end).Hours() / 24)
	if left < 0 {
		return 0
	}
	return left
}

func budgetStatusRank(status string) int {
	switch status {
	case "exceeded":
		return 3
	case "watch":
		return 2
	default:
		return 1
	}
}

func buildBudgetInsightCards(statuses []DashboardBudgetStatus) []InsightCard {
	cards := []InsightCard{}
	for _, status := range statuses {
		if status.Status != "watch" && status.Status != "exceeded" {
			continue
		}
		budgetID := status.BudgetID
		spent := status.SpentAmount
		limit := status.LimitAmount
		remaining := status.RemainingAmount
		percentage := status.Percentage
		target := status.Category
		if strings.TrimSpace(target) == "" {
			target = status.Name
		}
		kind := "budget_watch"
		severity := "warning"
		title := target + " budget nearing limit"
		body := fmt.Sprintf("%s spending is nearing its monthly limit.", target)
		if status.Status == "exceeded" {
			kind = "budget_exceeded"
			title = target + " budget exceeded"
			body = fmt.Sprintf("%s spending has exceeded its monthly limit.", target)
		}
		cards = append(cards, InsightCard{
			Kind:            kind,
			Severity:        severity,
			Title:           title,
			Body:            body,
			Explanation:     "Compares confirmed expenses in the selected period against your active monthly budget.",
			ActionLabel:     "Review budget",
			Category:        status.Category,
			BudgetID:        &budgetID,
			Amount:          &spent,
			LimitAmount:     &limit,
			RemainingAmount: &remaining,
			Percentage:      &percentage,
			Status:          status.Status,
		})
	}
	return cards
}

type recurringGroup struct {
	label    string
	merchant string
	category string
	entries  []models.Entry
}

type recurringInterval struct {
	name string
	days int
}

func detectRecurringCandidates(entries []models.Entry, dateRange dashboardRange) []DashboardRecurringCandidate {
	groups := map[string]*recurringGroup{}
	for _, entry := range entries {
		if !strings.EqualFold(entry.Type, "expense") {
			continue
		}
		if _, err := time.ParseInLocation("2006-01-02", entry.Date, dateRange.Start.Location()); err != nil {
			continue
		}
		label := recurringLabel(entry)
		if label == "" {
			continue
		}
		category := normalizedLabel(entry.Category, "Uncategorized")
		key := strings.ToLower(label) + "|" + strings.ToLower(category)
		if groups[key] == nil {
			groups[key] = &recurringGroup{
				label: label, merchant: strings.TrimSpace(entry.Merchant), category: category,
			}
		}
		groups[key].entries = append(groups[key].entries, entry)
	}

	candidates := []DashboardRecurringCandidate{}
	for _, group := range groups {
		if candidate, ok := recurringCandidateFromGroup(group, dateRange); ok {
			candidate.CandidateKey = recurringCandidateKey(candidate.Label, candidate.Category)
			candidates = append(candidates, candidate)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].ReviewDue != candidates[j].ReviewDue {
			return candidates[i].ReviewDue
		}
		if candidates[i].Confidence != candidates[j].Confidence {
			return candidates[i].Confidence > candidates[j].Confidence
		}
		return candidates[i].NextExpectedDate < candidates[j].NextExpectedDate
	})
	if len(candidates) > 5 {
		candidates = candidates[:5]
	}
	return candidates
}

func suppressReviewedRecurringCandidates(userID uint, dashboard *DashboardResponse, rangeEnd time.Time) error {
	if dashboard == nil || len(dashboard.RecurringCandidates) == 0 {
		return nil
	}
	filtered, err := filterReviewedRecurringCandidates(userID, dashboard.RecurringCandidates, rangeEnd)
	if err != nil {
		return err
	}
	dashboard.RecurringCandidates = filtered
	dashboard.Insights = replaceRecurringInsight(dashboard.Insights, filtered)
	return nil
}

// filterReviewedRecurringCandidates drops the candidates the user has already
// answered for — dismissed, tracked, or snoozed past the end of the range —
// along with any that a live subscription already covers.
//
// The dashboard and the one-tap track endpoint both need exactly this list:
// the card offers what comes back from here, so tracking has to resolve the
// same keys against the same set or the count on the card and the number of
// subscriptions created would disagree.
func filterReviewedRecurringCandidates(
	userID uint,
	candidates []DashboardRecurringCandidate,
	rangeEnd time.Time,
) ([]DashboardRecurringCandidate, error) {
	if len(candidates) == 0 {
		return []DashboardRecurringCandidate{}, nil
	}

	var decisions []models.RecurringCandidateDecision
	if err := database.DB.Where("user_id = ?", userID).Find(&decisions).Error; err != nil {
		return nil, err
	}
	decisionByKey := map[string]models.RecurringCandidateDecision{}
	for _, decision := range decisions {
		decisionByKey[decision.CandidateKey] = decision
	}

	var subscriptions []models.Subscription
	if err := database.DB.Where("user_id = ? AND status IN ?", userID, []string{"active", "paused"}).Find(&subscriptions).Error; err != nil {
		return nil, err
	}

	filtered := make([]DashboardRecurringCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.CandidateKey == "" {
			candidate.CandidateKey = recurringCandidateKey(candidate.Label, candidate.Category)
		}
		if recurringCandidateHasSubscription(candidate, subscriptions) {
			continue
		}
		decision, exists := decisionByKey[candidate.CandidateKey]
		if exists {
			switch decision.Decision {
			case recurringDecisionDismissed, recurringDecisionTracked:
				continue
			case recurringDecisionSnoozed:
				if decision.SnoozedUntil == nil || !decision.SnoozedUntil.Before(rangeEnd) {
					continue
				}
			}
		}
		filtered = append(filtered, candidate)
	}
	return filtered, nil
}

func replaceRecurringInsight(cards []InsightCard, candidates []DashboardRecurringCandidate) []InsightCard {
	filtered := make([]InsightCard, 0, len(cards))
	for _, card := range cards {
		if card.Kind != "recurring_candidate" {
			filtered = append(filtered, card)
		}
	}
	if len(candidates) == 0 {
		return filtered
	}
	candidate := candidates[0]
	amount := candidate.AverageAmount
	confidence := candidate.Confidence
	return append(filtered, InsightCard{
		Kind:             "recurring_candidate",
		Severity:         "info",
		Title:            "Recurring spend to review",
		Body:             fmt.Sprintf("%s appears to repeat and is ready for review.", candidate.Label),
		Explanation:      "Detects repeated merchant or category patterns that look weekly or monthly.",
		ActionLabel:      "Review recurring pattern",
		Category:         candidate.Category,
		Merchant:         candidate.Merchant,
		Amount:           &amount,
		NextExpectedDate: candidate.NextExpectedDate,
		Confidence:       &confidence,
	})
}

func recurringCandidateHasSubscription(candidate DashboardRecurringCandidate, subscriptions []models.Subscription) bool {
	candidateMerchant := normalizeRecurringKeyPart(candidate.Merchant)
	candidateLabel := normalizeRecurringKeyPart(candidate.Label)
	candidateCategory := normalizeRecurringKeyPart(candidate.Category)
	for _, subscription := range subscriptions {
		subMerchant := normalizeRecurringKeyPart(subscription.Merchant)
		subName := normalizeRecurringKeyPart(subscription.Name)
		subCategory := normalizeRecurringKeyPart(subscription.Category)
		if candidateMerchant != "" && (candidateMerchant == subMerchant || candidateMerchant == subName) {
			return true
		}
		if candidateLabel != "" && (candidateLabel == subMerchant || candidateLabel == subName) {
			return true
		}
		if candidateCategory != "" && candidateCategory == subCategory && candidateMerchant == "" {
			return true
		}
	}
	return false
}

func recurringCandidateKey(label, category string) string {
	return normalizeRecurringKeyPart(label) + "|" + normalizeRecurringKeyPart(category)
}

func normalizeRecurringKeyPart(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), " ")
}

func recurringCandidateFromGroup(group *recurringGroup, dateRange dashboardRange) (DashboardRecurringCandidate, bool) {
	entries := append([]models.Entry(nil), group.entries...)
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Date < entries[j].Date
	})
	if len(entries) < 2 {
		return DashboardRecurringCandidate{}, false
	}

	amounts := make([]float64, 0, len(entries))
	dates := make([]time.Time, 0, len(entries))
	for _, entry := range entries {
		date, err := time.ParseInLocation("2006-01-02", entry.Date, dateRange.Start.Location())
		if err != nil {
			return DashboardRecurringCandidate{}, false
		}
		dates = append(dates, date)
		amounts = append(amounts, entry.Amount.Float64())
	}
	if !recurringAmountsAreStable(amounts) {
		return DashboardRecurringCandidate{}, false
	}

	interval, ok := guessRecurringInterval(dates)
	if !ok {
		return DashboardRecurringCandidate{}, false
	}

	average := averageFloat(amounts)
	lastSeen := dates[len(dates)-1]
	nextExpected := nextRecurringDate(lastSeen, interval)
	confidence := recurringConfidence(len(entries), amounts)
	reviewDue := !nextExpected.Before(dateRange.Start) && !nextExpected.After(dateRange.End.AddDate(0, 0, 7))

	return DashboardRecurringCandidate{
		Label:            group.label,
		Merchant:         group.merchant,
		Category:         group.category,
		AverageAmount:    average,
		IntervalGuess:    interval.name,
		Confidence:       confidence,
		Occurrences:      len(entries),
		LastSeenDate:     lastSeen.Format("2006-01-02"),
		NextExpectedDate: nextExpected.Format("2006-01-02"),
		ReviewDue:        reviewDue,
	}, true
}

func recurringLabel(entry models.Entry) string {
	for _, value := range []string{entry.Merchant, entry.Title} {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func guessRecurringInterval(dates []time.Time) (recurringInterval, bool) {
	if len(dates) < 2 {
		return recurringInterval{}, false
	}
	intervals := make([]int, 0, len(dates)-1)
	for i := 1; i < len(dates); i++ {
		days := int(dates[i].Sub(dates[i-1]).Hours() / 24)
		if days <= 0 {
			return recurringInterval{}, false
		}
		intervals = append(intervals, days)
	}
	average := averageInt(intervals)
	switch {
	case average >= 6 && average <= 8 && intervalSpreadWithin(intervals, 2):
		return recurringInterval{name: "weekly", days: 7}, true
	case average >= 27 && average <= 35 && intervalSpreadWithin(intervals, 5):
		return recurringInterval{name: "monthly", days: 30}, true
	default:
		return recurringInterval{}, false
	}
}

func nextRecurringDate(lastSeen time.Time, interval recurringInterval) time.Time {
	if interval.name == "monthly" {
		return lastSeen.AddDate(0, 1, 0)
	}
	return lastSeen.AddDate(0, 0, interval.days)
}

func recurringAmountsAreStable(amounts []float64) bool {
	if len(amounts) < 2 {
		return false
	}
	minimum, maximum := amounts[0], amounts[0]
	for _, amount := range amounts[1:] {
		if amount < minimum {
			minimum = amount
		}
		if amount > maximum {
			maximum = amount
		}
	}
	average := averageFloat(amounts)
	if average <= 0 {
		return false
	}
	return (maximum - minimum) <= math.Max(20, average*0.15)
}

func recurringConfidence(occurrences int, amounts []float64) float64 {
	confidence := 0.65
	if occurrences >= 3 {
		confidence = 0.80
	}
	if recurringAmountSpread(amounts) <= 0.05 {
		confidence += 0.10
	}
	if occurrences >= 4 {
		confidence += 0.05
	}
	return math.Min(confidence, 0.95)
}

func recurringAmountSpread(amounts []float64) float64 {
	average := averageFloat(amounts)
	if average <= 0 {
		return 1
	}
	minimum, maximum := amounts[0], amounts[0]
	for _, amount := range amounts[1:] {
		if amount < minimum {
			minimum = amount
		}
		if amount > maximum {
			maximum = amount
		}
	}
	return (maximum - minimum) / average
}

func averageFloat(values []float64) float64 {
	total := 0.0
	for _, value := range values {
		total += value
	}
	return total / float64(len(values))
}

func averageInt(values []int) int {
	total := 0
	for _, value := range values {
		total += value
	}
	return int(math.Round(float64(total) / float64(len(values))))
}

func intervalSpreadWithin(values []int, tolerance int) bool {
	minimum, maximum := values[0], values[0]
	for _, value := range values[1:] {
		if value < minimum {
			minimum = value
		}
		if value > maximum {
			maximum = value
		}
	}
	return maximum-minimum <= tolerance
}

func normalizedLabel(value, fallback string) string {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		return trimmed
	}
	return fallback
}

func previousExpenseBase(entries []models.Entry) comparisonBase {
	base := comparisonBase{}
	for _, entry := range entries {
		if strings.EqualFold(entry.Type, "expense") {
			base.Amount += entry.Amount.Float64()
			base.Count++
		}
	}
	return base
}

func safePercentage(value, total float64) float64 {
	if total <= 0 {
		return 0
	}
	return value / total * 100
}

func percentageChange(current, previous float64) float64 {
	if previous <= 0 {
		if current > 0 {
			return 100
		}
		return 0
	}
	return (current - previous) / previous * 100
}

func unusualExpense(amounts []float64) float64 {
	if len(amounts) < 3 {
		return 0
	}
	total := 0.0
	maximum := 0.0
	for _, amount := range amounts {
		total += amount
		if amount > maximum {
			maximum = amount
		}
	}
	average := total / float64(len(amounts))
	if maximum >= average*2 {
		return maximum
	}
	return 0
}
