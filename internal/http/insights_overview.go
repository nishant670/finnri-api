package http

import (
	"sort"
	"time"

	"finnri/internal/database"
	"finnri/internal/models"
)

// overviewMonths is how far back the standing history reaches.
//
// Twelve is enough for a year-shaped rhythm — rent, insurance, the two months
// a year that always cost more — while staying one small query and a strip the
// app can draw without scrolling.
const overviewMonths = 12

// overviewBaselineMonths is how many of those the typical-month figure is taken
// from. A baseline that reaches back a year describes a person's habits from
// last winter as much as from now; six months is recent enough to be about the
// life they are currently living.
const overviewBaselineMonths = 6

type DashboardOverviewMonth struct {
	// Month is YYYY-MM, and Label is the short name the strip prints.
	Month  string  `json:"month"`
	Label  string  `json:"label"`
	Spent  float64 `json:"spent"`
	Income float64 `json:"income"`
	Count  int     `json:"count"`
}

// DashboardOverview is what the Insights tab knows regardless of which period
// is selected.
//
// The tab was period-scoped end to end, which meant it went blank on schedule:
// every month, from the 1st until the first transaction was captured, an
// account with a year of history rendered ₹0 four times over "Waiting for
// data". The window is the lens, not the asset — and a month is nearly
// meaningless without the months either side of it to read it against.
//
// So this block is deliberately not filtered by the selected range. It is the
// floor the screen stands on, and the baseline that makes the selected window
// mean something.
type DashboardOverview struct {
	// Whether there is any history at all. False means a genuinely new account,
	// which is the one case where an empty screen is the honest one.
	HasHistory bool `json:"has_history"`
	// The date of the first transaction ever recorded, and how many calendar
	// months of history that spans.
	TrackedSince  string `json:"tracked_since,omitempty"`
	MonthsTracked int    `json:"months_tracked"`

	LifetimeSpent            float64 `json:"lifetime_spent"`
	LifetimeIncome           float64 `json:"lifetime_income"`
	LifetimeTransactionCount int     `json:"lifetime_transaction_count"`

	// AverageMonthlySpend is the mean over the baseline months that had
	// activity; TypicalMonthlySpend is the median over the same set. The median
	// is what the app should lead with — one holiday or one insurance renewal
	// drags a six-month mean far enough to make an ordinary month look frugal.
	//
	// Both exclude the current month, which is still being lived in. Including
	// it put a four-day-old month into a three-month median and made it the
	// answer: the app announced "a typical month runs about ₹10,226" next to
	// "Jump to Sep · ₹10,226", the same figure twice, which reads as a bug
	// because it is one.
	AverageMonthlySpend float64 `json:"average_monthly_spend"`
	TypicalMonthlySpend float64 `json:"typical_monthly_spend"`
	// How many complete months actually backed those two figures. A caller must
	// check this before calling anything "typical": one or two months is a
	// sample, not a habit, and with a holiday in it the median swings further
	// than the number it replaces.
	BaselineMonths int `json:"baseline_months"`

	// RecentMonths is oldest first so the app can draw it straight as a strip.
	RecentMonths []DashboardOverviewMonth `json:"recent_months"`

	// The most recent month with any activity. This is what the app offers
	// when the selected window turns out to be empty: it is the difference
	// between a dead end and one tap to where the data actually is.
	LastActiveMonth *DashboardOverviewMonth `json:"last_active_month,omitempty"`

	// Where the money goes across the baseline window, not just this period.
	TopCategory       string  `json:"top_category,omitempty"`
	TopCategoryAmount float64 `json:"top_category_amount"`
	TopCategoryShare  float64 `json:"top_category_share"`
	// How many months the top-category window actually spans. The window asks
	// for six; an account three months old has three, and saying "over six
	// months" to that account describes a period half of which it was not
	// being used for.
	TopCategoryMonths int `json:"top_category_months"`
}

type overviewMonthRow struct {
	Month  string
	Spent  float64
	Income float64
	Count  int
}

// loadDashboardOverview builds the period-independent block.
//
// Card payments are excluded on the same grounds as everywhere else on this
// screen (see notCardPaymentClause): the spending already happened when the
// card was used, and counting the payment again doubles it.
func loadDashboardOverview(userID uint, now time.Time) (DashboardOverview, error) {
	overview := DashboardOverview{RecentMonths: []DashboardOverviewMonth{}}

	// One row per calendar month, back to the start of the window. Grouped in
	// SQL rather than in Go so a long history does not have to be read into
	// memory to be summarised.
	windowStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).
		AddDate(0, -(overviewMonths - 1), 0)

	var rows []overviewMonthRow
	if err := database.DB.Model(&models.Entry{}).
		Select(`SUBSTR(date, 1, 7) AS month,
			COALESCE(SUM(CASE WHEN LOWER(type) = 'expense' THEN amount ELSE 0 END), 0) AS spent,
			COALESCE(SUM(CASE WHEN LOWER(type) = 'income' THEN amount ELSE 0 END), 0) AS income,
			COUNT(*) AS count`).
		Where("user_id = ? AND date >= ?"+notCardPaymentClause, userID, windowStart.Format("2006-01-02")).
		Group("SUBSTR(date, 1, 7)").
		Order("month asc").
		Scan(&rows).Error; err != nil {
		return DashboardOverview{}, err
	}

	byMonth := make(map[string]overviewMonthRow, len(rows))
	for _, row := range rows {
		byMonth[row.Month] = row
	}

	// Every month in the window gets a bar, including the empty ones. A strip
	// that silently drops quiet months compresses time and makes a gap look
	// like a decline.
	for offset := 0; offset < overviewMonths; offset++ {
		month := windowStart.AddDate(0, offset, 0)
		key := month.Format("2006-01")
		row := byMonth[key]
		entry := DashboardOverviewMonth{
			Month:  key,
			Label:  month.Format("Jan"),
			Spent:  row.Spent,
			Income: row.Income,
			Count:  row.Count,
		}
		overview.RecentMonths = append(overview.RecentMonths, entry)
		if entry.Count > 0 {
			latest := entry
			overview.LastActiveMonth = &latest
		}
	}

	// Lifetime figures reach past the strip, so they are their own query.
	var lifetime struct {
		Spent  float64
		Income float64
		Count  int
		First  string
	}
	if err := database.DB.Model(&models.Entry{}).
		Select(`COALESCE(SUM(CASE WHEN LOWER(type) = 'expense' THEN amount ELSE 0 END), 0) AS spent,
			COALESCE(SUM(CASE WHEN LOWER(type) = 'income' THEN amount ELSE 0 END), 0) AS income,
			COUNT(*) AS count,
			COALESCE(MIN(date), '') AS first`).
		Where("user_id = ?"+notCardPaymentClause, userID).
		Scan(&lifetime).Error; err != nil {
		return DashboardOverview{}, err
	}

	overview.LifetimeSpent = lifetime.Spent
	overview.LifetimeIncome = lifetime.Income
	overview.LifetimeTransactionCount = lifetime.Count
	overview.HasHistory = lifetime.Count > 0
	if !overview.HasHistory {
		// Nothing to describe. Returning the empty strip rather than a
		// half-filled one keeps "new account" distinguishable from "quiet
		// month", which is the whole distinction this block exists to draw.
		overview.RecentMonths = []DashboardOverviewMonth{}
		overview.LastActiveMonth = nil
		return overview, nil
	}

	overview.TrackedSince = lifetime.First
	if first, err := time.ParseInLocation("2006-01-02", lifetime.First, now.Location()); err == nil {
		months := (now.Year()-first.Year())*12 + int(now.Month()) - int(first.Month()) + 1
		if months < 1 {
			months = 1
		}
		overview.MonthsTracked = months
	}

	// The current month is dropped before the baseline is taken: it is partial
	// by definition, and on the 4th it describes four days.
	baselineSource := overview.RecentMonths
	currentKey := now.Format("2006-01")
	if n := len(baselineSource); n > 0 && baselineSource[n-1].Month == currentKey {
		baselineSource = baselineSource[:n-1]
	}
	overview.AverageMonthlySpend, overview.TypicalMonthlySpend, overview.BaselineMonths =
		baselineMonthlySpend(baselineSource)

	topCategory, topAmount, baselineSpend, err := loadOverviewTopCategory(userID, now)
	if err != nil {
		return DashboardOverview{}, err
	}
	overview.TopCategory = topCategory
	overview.TopCategoryAmount = topAmount
	if baselineSpend > 0 {
		overview.TopCategoryShare = topAmount / baselineSpend * 100
	}
	overview.TopCategoryMonths = overviewBaselineMonths
	if overview.MonthsTracked < overview.TopCategoryMonths {
		overview.TopCategoryMonths = overview.MonthsTracked
	}
	return overview, nil
}

// baselineMonthlySpend summarises the last overviewBaselineMonths of the strip.
//
// Only months that actually had activity count. Averaging in the months before
// somebody started using the app — or the current month on its second day —
// describes the app's adoption curve rather than the person's spending, and
// always downward.
func baselineMonthlySpend(months []DashboardOverviewMonth) (mean float64, median float64, used int) {
	start := len(months) - overviewBaselineMonths
	if start < 0 {
		start = 0
	}
	amounts := make([]float64, 0, overviewBaselineMonths)
	for _, month := range months[start:] {
		if month.Count > 0 {
			amounts = append(amounts, month.Spent)
		}
	}
	if len(amounts) == 0 {
		return 0, 0, 0
	}

	total := 0.0
	for _, amount := range amounts {
		total += amount
	}
	mean = total / float64(len(amounts))

	sorted := append([]float64(nil), amounts...)
	sort.Float64s(sorted)
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		median = sorted[middle]
	} else {
		median = (sorted[middle-1] + sorted[middle]) / 2
	}
	return mean, median, len(amounts)
}

// loadOverviewTopCategory names the biggest category across the baseline
// window, and the window's total so the caller can state a share.
func loadOverviewTopCategory(userID uint, now time.Time) (string, float64, float64, error) {
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).
		AddDate(0, -(overviewBaselineMonths - 1), 0)

	var rows []dashboardCategoryRow
	if err := database.DB.Model(&models.Entry{}).
		Select("CASE WHEN TRIM(category) = '' THEN 'Uncategorized' ELSE TRIM(category) END AS category, COALESCE(SUM(amount), 0) AS amount, COUNT(*) AS count").
		Where("user_id = ? AND date >= ?"+notCardPaymentClause+" AND LOWER(type) = ?",
			userID, start.Format("2006-01-02"), "expense").
		Group("CASE WHEN TRIM(category) = '' THEN 'Uncategorized' ELSE TRIM(category) END").
		Order("amount desc").
		Scan(&rows).Error; err != nil {
		return "", 0, 0, err
	}

	total := 0.0
	for _, row := range rows {
		total += row.Amount
	}
	if len(rows) == 0 {
		return "", 0, total, nil
	}
	return normalizedLabel(rows[0].Category, "Uncategorized"), rows[0].Amount, total, nil
}
