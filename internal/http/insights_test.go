package http

import (
	"strings"
	"testing"
	"time"

	"finnri/internal/database"
	"finnri/internal/models"
)

func dashboardEntry(
	date, entryType, amount, category, merchant string,
	accountID uint, accountName string,
) models.Entry {
	parsedAmount, err := models.ParseMoney(amount)
	if err != nil {
		panic(err)
	}
	id := accountID
	return models.Entry{
		Date: date, Type: entryType, Amount: parsedAmount, Category: category,
		Merchant: merchant, AccountID: &id,
		Account: &models.Account{ID: id, Name: accountName},
	}
}

func insightByKind(cards []InsightCard, kind string) *InsightCard {
	for index := range cards {
		if cards[index].Kind == kind {
			return &cards[index]
		}
	}
	return nil
}

func assertInsightBodiesLeaveFactsStructured(t *testing.T, cards []InsightCard) {
	t.Helper()
	for _, card := range cards {
		if strings.Contains(card.Body, "₹") || strings.Contains(card.Body, "(s)") {
			t.Errorf("%s body duplicates a structured fact: %q", card.Kind, card.Body)
		}
	}
}

func TestBuildDashboardProducesFiveDeterministicTemplates(t *testing.T) {
	location := time.FixedZone("IST", 5*60*60+30*60)
	dateRange := dashboardRange{
		Start:         time.Date(2026, 7, 1, 0, 0, 0, 0, location),
		End:           time.Date(2026, 7, 3, 0, 0, 0, 0, location),
		PreviousStart: time.Date(2026, 6, 28, 0, 0, 0, 0, location),
		PreviousEnd:   time.Date(2026, 6, 30, 0, 0, 0, 0, location),
		Days:          3,
	}
	// The previous window has to clear the comparison floor (≥ ₹500 and ≥ 5
	// transactions, per category too) or the two comparison templates are
	// correctly withheld. See TestComparisonFloorSuppressesThinBaselines.
	entries := []models.Entry{
		dashboardEntry("2026-06-28", "expense", "100", "Food", "Cafe", 1, "Cash"),
		dashboardEntry("2026-06-28", "expense", "100", "Food", "Cafe", 1, "Cash"),
		dashboardEntry("2026-06-29", "expense", "100", "Food", "Cafe", 1, "Cash"),
		dashboardEntry("2026-06-29", "expense", "100", "Food", "Cafe", 1, "Cash"),
		dashboardEntry("2026-06-30", "expense", "100", "Food", "Cafe", 1, "Cash"),
		dashboardEntry("2026-06-30", "expense", "100", "Travel", "Metro", 2, "UPI"),
		dashboardEntry("2026-07-01", "expense", "100", "Food", "Cafe", 1, "Cash"),
		dashboardEntry("2026-07-02", "expense", "100", "Food", "Cafe", 1, "Cash"),
		dashboardEntry("2026-07-03", "expense", "1000", "Food", "Cafe", 1, "Cash"),
	}

	dashboard := buildDashboard(entries, dateRange)
	assertInsightBodiesLeaveFactsStructured(t, dashboard.Insights)
	kinds := map[string]bool{}
	for _, card := range dashboard.Insights {
		kinds[card.Kind] = true
	}
	for _, expected := range []string{
		"period_comparison", "category_increase", "top_merchant",
		"account_usage", "unusual_spending",
	} {
		if !kinds[expected] {
			t.Fatalf("missing %s insight: %#v", expected, dashboard.Insights)
		}
	}
	categoryInsight := insightByKind(dashboard.Insights, "category_increase")
	if categoryInsight == nil || categoryInsight.Category != "Food" || categoryInsight.Amount == nil || *categoryInsight.Amount != 1200 {
		t.Fatalf("missing structured category insight data: %#v", categoryInsight)
	}
	merchantInsight := insightByKind(dashboard.Insights, "top_merchant")
	if merchantInsight == nil || merchantInsight.Merchant != "Cafe" || merchantInsight.TransactionCount == nil || *merchantInsight.TransactionCount != 3 {
		t.Fatalf("missing structured merchant insight data: %#v", merchantInsight)
	}
	accountInsight := insightByKind(dashboard.Insights, "account_usage")
	if accountInsight == nil || accountInsight.AccountName != "Cash" || accountInsight.Percentage == nil || *accountInsight.Percentage != 100 {
		t.Fatalf("missing structured account insight data: %#v", accountInsight)
	}
	if dashboard.Summary.TotalSpent != 1200 || dashboard.Summary.DailyAverage != 400 {
		t.Fatalf("unexpected summary: %#v", dashboard.Summary)
	}
	if len(dashboard.DailySpending) != 3 {
		t.Fatalf("expected three daily spending rows, got %#v", dashboard.DailySpending)
	}
	expectedDaily := map[string]float64{
		"2026-07-01": 100,
		"2026-07-02": 100,
		"2026-07-03": 1000,
	}
	for _, daily := range dashboard.DailySpending {
		if expectedDaily[daily.Date] != daily.Amount {
			t.Fatalf("unexpected daily spending row: %#v", daily)
		}
	}
}

func TestBuildDashboardReturnsFullPeriodReviewItems(t *testing.T) {
	location := time.FixedZone("IST", 5*60*60+30*60)
	dateRange := dashboardRange{
		Start:         time.Date(2026, 7, 1, 0, 0, 0, 0, location),
		End:           time.Date(2026, 7, 10, 0, 0, 0, 0, location),
		PreviousStart: time.Date(2026, 6, 21, 0, 0, 0, 0, location),
		PreviousEnd:   time.Date(2026, 6, 30, 0, 0, 0, 0, location),
		Days:          10,
	}
	entries := []models.Entry{
		dashboardEntry("2026-06-23", "expense", "90", "Food", "Older Cleanup", 1, "Cash"),
		dashboardEntry("2026-06-24", "expense", "110", "Food", "Older Cleanup", 1, "Cash"),
		dashboardEntry("2026-06-25", "expense", "200", "Shopping", "Older Cleanup", 1, "Cash"),
		dashboardEntry("2026-07-10", "expense", "100", "Food", "Cafe 1", 1, "Cash"),
		dashboardEntry("2026-07-09", "expense", "100", "Food", "Cafe 2", 1, "Cash"),
		dashboardEntry("2026-07-08", "expense", "100", "Food", "Cafe 3", 1, "Cash"),
		dashboardEntry("2026-07-07", "expense", "100", "Food", "Cafe 4", 1, "Cash"),
		dashboardEntry("2026-07-06", "expense", "100", "Food", "Cafe 5", 1, "Cash"),
		dashboardEntry("2026-07-05", "expense", "100", "Uncategorized", "Older Cleanup", 1, "Cash"),
	}
	missingAccount := dashboardEntry("2026-07-04", "expense", "120", "Travel", "Metro", 1, "Cash")
	missingAccount.AccountID = nil
	missingAccount.Account = nil
	entries = append(entries, missingAccount)

	dashboard := buildDashboard(entries, dateRange)
	assertInsightBodiesLeaveFactsStructured(t, dashboard.Insights)
	if len(dashboard.RecentTransactions) != 5 {
		t.Fatalf("expected five recent transactions, got %#v", dashboard.RecentTransactions)
	}
	if len(dashboard.ReviewItems) != 2 {
		t.Fatalf("expected two review items from the full period, got %#v", dashboard.ReviewItems)
	}
	if dashboard.ReviewItems[0].Merchant != "Older Cleanup" || dashboard.ReviewItems[1].Merchant != "Metro" {
		t.Fatalf("unexpected review item ordering: %#v", dashboard.ReviewItems)
	}
	if len(dashboard.ReviewItems[0].CategorySuggestions) != 2 ||
		dashboard.ReviewItems[0].CategorySuggestions[0] != "Food" ||
		dashboard.ReviewItems[0].CategorySuggestions[1] != "Shopping" {
		t.Fatalf("expected category suggestions from matching history, got %#v", dashboard.ReviewItems[0].CategorySuggestions)
	}
	for _, entry := range dashboard.RecentTransactions {
		if entry.Merchant == "Older Cleanup" || entry.Merchant == "Metro" {
			t.Fatalf("review fixture should be outside recent preview: %#v", dashboard.RecentTransactions)
		}
	}
}

func TestBuildDashboardDetectsRecurringCandidatesForWeeklyReview(t *testing.T) {
	location := time.FixedZone("IST", 5*60*60+30*60)
	dateRange := dashboardRange{
		Start:         time.Date(2026, 7, 8, 0, 0, 0, 0, location),
		End:           time.Date(2026, 7, 14, 0, 0, 0, 0, location),
		PreviousStart: time.Date(2026, 7, 1, 0, 0, 0, 0, location),
		PreviousEnd:   time.Date(2026, 7, 7, 0, 0, 0, 0, location),
		Days:          7,
	}
	entries := []models.Entry{
		dashboardEntry("2026-07-01", "expense", "499", "Entertainment", "Streamly", 1, "UPI"),
		dashboardEntry("2026-07-08", "expense", "499", "Entertainment", "Streamly", 1, "UPI"),
		dashboardEntry("2026-07-10", "expense", "120", "Food", "Cafe", 1, "UPI"),
		dashboardEntry("2026-07-12", "expense", "220", "Food", "Cafe", 1, "UPI"),
	}

	dashboard := buildDashboard(entries, dateRange)
	assertInsightBodiesLeaveFactsStructured(t, dashboard.Insights)
	if len(dashboard.RecurringCandidates) != 1 {
		t.Fatalf("expected one recurring candidate, got %#v", dashboard.RecurringCandidates)
	}
	candidate := dashboard.RecurringCandidates[0]
	if candidate.Label != "Streamly" || candidate.IntervalGuess != "weekly" || !candidate.ReviewDue {
		t.Fatalf("unexpected recurring candidate: %#v", candidate)
	}
	if candidate.NextExpectedDate != "2026-07-15" || candidate.AverageAmount != 499 {
		t.Fatalf("unexpected recurring timing/amount: %#v", candidate)
	}

	kinds := map[string]bool{}
	for _, card := range dashboard.Insights {
		kinds[card.Kind] = true
	}
	if !kinds["recurring_candidate"] {
		t.Fatalf("missing recurring insight: %#v", dashboard.Insights)
	}
	recurringInsight := insightByKind(dashboard.Insights, "recurring_candidate")
	if recurringInsight == nil || recurringInsight.Merchant != "Streamly" || recurringInsight.NextExpectedDate != "2026-07-15" || recurringInsight.Confidence == nil {
		t.Fatalf("missing structured recurring insight data: %#v", recurringInsight)
	}
}

func TestApplyBudgetStatusesAddsWatchAndExceededInsights(t *testing.T) {
	location := time.FixedZone("IST", 5*60*60+30*60)
	dateRange := dashboardRange{
		Start:         time.Date(2026, 7, 1, 0, 0, 0, 0, location),
		End:           time.Date(2026, 7, 20, 0, 0, 0, 0, location),
		PreviousStart: time.Date(2026, 6, 11, 0, 0, 0, 0, location),
		PreviousEnd:   time.Date(2026, 6, 30, 0, 0, 0, 0, location),
		Days:          20,
	}
	entries := []models.Entry{
		dashboardEntry("2026-07-05", "expense", "850", "Food", "Cafe", 1, "Cash"),
		dashboardEntry("2026-07-10", "expense", "1200", "Travel", "Metro", 1, "Cash"),
		dashboardEntry("2026-07-11", "expense", "100", "Shopping", "Store", 1, "Cash"),
	}
	dashboard := buildDashboard(entries, dateRange)
	applyBudgetStatuses(entries, []models.Budget{
		{ID: 1, Name: "Food", Period: budgetPeriodMonthly, Category: "Food", LimitAmount: testMoney("1000"), AlertThresholdPercent: 80, Active: true},
		{ID: 2, Name: "Travel", Period: budgetPeriodMonthly, Category: "Travel", LimitAmount: testMoney("1000"), AlertThresholdPercent: 80, Active: true},
		{ID: 3, Name: "Shopping", Period: budgetPeriodMonthly, Category: "Shopping", LimitAmount: testMoney("1000"), AlertThresholdPercent: 80, Active: true},
	}, dateRange, &dashboard)
	assertInsightBodiesLeaveFactsStructured(t, dashboard.Insights)

	if len(dashboard.BudgetStatuses) != 3 {
		t.Fatalf("expected three budget statuses, got %#v", dashboard.BudgetStatuses)
	}
	kinds := map[string]int{}
	for _, insight := range dashboard.Insights {
		kinds[insight.Kind]++
	}
	if kinds["budget_watch"] != 1 || kinds["budget_exceeded"] != 1 {
		t.Fatalf("expected one watch and one exceeded budget insight, got %#v", dashboard.Insights)
	}
	if dashboard.BudgetStatuses[0].Status != "exceeded" || dashboard.BudgetStatuses[1].Status != "watch" {
		t.Fatalf("expected exceeded/watch statuses first, got %#v", dashboard.BudgetStatuses)
	}
}

func TestDetectRecurringCandidatesRejectsUnstableRepeats(t *testing.T) {
	location := time.FixedZone("IST", 5*60*60+30*60)
	dateRange := dashboardRange{
		Start:         time.Date(2026, 7, 1, 0, 0, 0, 0, location),
		End:           time.Date(2026, 7, 31, 0, 0, 0, 0, location),
		PreviousStart: time.Date(2026, 6, 1, 0, 0, 0, 0, location),
		PreviousEnd:   time.Date(2026, 6, 30, 0, 0, 0, 0, location),
		Days:          31,
	}
	entries := []models.Entry{
		dashboardEntry("2026-06-01", "expense", "100", "Food", "Cafe", 1, "Cash"),
		dashboardEntry("2026-06-12", "expense", "250", "Food", "Cafe", 1, "Cash"),
		dashboardEntry("2026-07-03", "expense", "600", "Food", "Cafe", 1, "Cash"),
	}

	candidates := detectRecurringCandidates(entries, dateRange)
	if len(candidates) != 0 {
		t.Fatalf("expected no unstable recurring candidates, got %#v", candidates)
	}
}

func TestBuildDashboardFromDBUsesBoundedRollupQueries(t *testing.T) {
	useSmokeDatabase(t)

	user := models.User{UUID: "dashboard-user", Username: "dashboard-user", IsGuest: true}
	otherUser := models.User{UUID: "dashboard-other", Username: "dashboard-other", IsGuest: true}
	if err := database.DB.Create(&user).Error; err != nil {
		t.Fatalf("failed to create dashboard user: %v", err)
	}
	if err := database.DB.Create(&otherUser).Error; err != nil {
		t.Fatalf("failed to create other user: %v", err)
	}

	cash := models.Account{UserID: user.ID, Type: "cash", Name: "Cash", IsDefault: true}
	upi := models.Account{UserID: user.ID, Type: "upi", Name: "UPI"}
	if err := database.DB.Create(&cash).Error; err != nil {
		t.Fatalf("failed to create cash account: %v", err)
	}
	if err := database.DB.Create(&upi).Error; err != nil {
		t.Fatalf("failed to create upi account: %v", err)
	}

	entries := []models.Entry{
		dbDashboardEntry(user.ID, cash.ID, "2026-06-29", "expense", "100", "Food", "Cafe", "Cash", "2026-06-29T09:00:00Z"),
		dbDashboardEntry(user.ID, upi.ID, "2026-06-30", "expense", "200", "Travel", "Metro", "UPI", "2026-06-30T09:00:00Z"),
		dbDashboardEntry(user.ID, cash.ID, "2026-07-01", "expense", "100", "Food", "Cafe", "Cash", "2026-07-01T09:00:00Z"),
		dbDashboardEntry(user.ID, cash.ID, "2026-07-02", "expense", "100", "Food", "Cafe", "Cash", "2026-07-02T09:00:00Z"),
		dbDashboardEntry(user.ID, cash.ID, "2026-07-03", "expense", "1000", "Food", "Cafe", "Cash", "2026-07-03T09:00:00Z"),
		dbDashboardEntry(user.ID, upi.ID, "2026-07-04", "expense", "50", "Travel", "Metro", "UPI", "2026-07-04T09:00:00Z"),
		dbDashboardEntry(user.ID, upi.ID, "2026-07-03", "income", "2500", "Salary", "Employer", "UPI", "2026-07-03T10:00:00Z"),
		dbDashboardEntry(otherUser.ID, cash.ID, "2026-07-03", "expense", "9999", "Food", "Other", "Cash", "2026-07-03T11:00:00Z"),
	}
	if err := database.DB.Create(&entries).Error; err != nil {
		t.Fatalf("failed to create dashboard entries: %v", err)
	}

	location := time.FixedZone("IST", 5*60*60+30*60)
	dateRange := dashboardRange{
		Start:         time.Date(2026, 7, 1, 0, 0, 0, 0, location),
		End:           time.Date(2026, 7, 4, 0, 0, 0, 0, location),
		PreviousStart: time.Date(2026, 6, 27, 0, 0, 0, 0, location),
		PreviousEnd:   time.Date(2026, 6, 30, 0, 0, 0, 0, location),
		Days:          4,
	}

	dashboard, err := buildDashboardFromDB(user.ID, dateRange)
	if err != nil {
		t.Fatalf("failed to build DB dashboard: %v", err)
	}

	if dashboard.Summary.TotalSpent != 1250 || dashboard.Summary.TotalIncome != 2500 ||
		dashboard.Summary.TransactionCount != 5 || dashboard.Summary.DailyAverage != 312.5 {
		t.Fatalf("unexpected summary: %#v", dashboard.Summary)
	}
	if len(dashboard.TopCategories) != 2 || dashboard.TopCategories[0].Category != "Food" ||
		dashboard.TopCategories[0].Amount != 1200 {
		t.Fatalf("unexpected categories: %#v", dashboard.TopCategories)
	}
	// Food's baseline here is one ₹100 entry. This used to report "1100%
	// higher" — the four-digit percentage that made Insights unbelievable.
	// A base that thin now yields no comparison at all.
	if dashboard.TopCategories[0].ChangeComparable || dashboard.TopCategories[0].Change != 0 {
		t.Fatalf("expected no comparison off a one-entry baseline: %#v", dashboard.TopCategories[0])
	}
	if insightByKind(dashboard.Insights, "period_comparison") != nil {
		t.Fatalf("expected no period comparison off a ₹300 two-entry baseline: %#v", dashboard.Insights)
	}
	if len(dashboard.TopMerchants) != 2 || dashboard.TopMerchants[0].Merchant != "Cafe" ||
		dashboard.TopMerchants[0].Amount != 1200 || dashboard.TopMerchants[0].TransactionCount != 3 {
		t.Fatalf("unexpected merchants: %#v", dashboard.TopMerchants)
	}
	if len(dashboard.AccountSpending) != 2 || dashboard.AccountSpending[0].AccountName != "Cash" ||
		dashboard.AccountSpending[0].Amount != 1200 {
		t.Fatalf("unexpected account spending: %#v", dashboard.AccountSpending)
	}
	if len(dashboard.RecentTransactions) != 5 || dashboard.RecentTransactions[0].Merchant != "Metro" {
		t.Fatalf("unexpected recent transactions: %#v", dashboard.RecentTransactions)
	}
	for _, category := range dashboard.TopCategories {
		if category.Amount == 9999 {
			t.Fatalf("dashboard leaked another user's entries: %#v", dashboard.TopCategories)
		}
	}
}

// The donut has to chart the whole period, and an "Other" slice has to be a
// residual the caller can compute — neither is possible while the breakdown is
// truncated to five. Both dashboard builders are checked, because a chart that
// disagrees with itself between the DB and in-memory paths is the drift this
// file has caught twice already.
func TestDashboardReturnsEveryCategoryNotJustTheTopFive(t *testing.T) {
	useSmokeDatabase(t)

	user := models.User{UUID: "breakdown-user", Username: "breakdown-user", IsGuest: true}
	if err := database.DB.Create(&user).Error; err != nil {
		t.Fatalf("failed to create user: %v", err)
	}
	cash := models.Account{UserID: user.ID, Type: "cash", Name: "Cash", IsDefault: true}
	if err := database.DB.Create(&cash).Error; err != nil {
		t.Fatalf("failed to create account: %v", err)
	}

	// Eight categories — the size of the canonical set, which is exactly the
	// case the old cap of five could not express.
	amounts := []struct {
		category string
		amount   string
	}{
		{"Bills", "800"},
		{"Food & Drinks", "700"},
		{"Shopping", "600"},
		{"Transport", "500"},
		{"Travel", "400"},
		{"Entertainment", "300"},
		{"Family/Gifts", "200"},
		{"Misc", "100"},
	}
	entries := make([]models.Entry, 0, len(amounts))
	for _, item := range amounts {
		entries = append(entries, dbDashboardEntry(
			user.ID, cash.ID, "2026-07-02", "expense", item.amount,
			item.category, "Shop", "Cash", "2026-07-02T09:00:00Z",
		))
	}
	if err := database.DB.Create(&entries).Error; err != nil {
		t.Fatalf("failed to create entries: %v", err)
	}

	location := time.FixedZone("IST", 5*60*60+30*60)
	dateRange := dashboardRange{
		Start:         time.Date(2026, 7, 1, 0, 0, 0, 0, location),
		End:           time.Date(2026, 7, 3, 0, 0, 0, 0, location),
		PreviousStart: time.Date(2026, 6, 28, 0, 0, 0, 0, location),
		PreviousEnd:   time.Date(2026, 6, 30, 0, 0, 0, 0, location),
		Days:          3,
	}

	fromDB, err := buildDashboardFromDB(user.ID, dateRange)
	if err != nil {
		t.Fatalf("failed to build DB dashboard: %v", err)
	}
	inMemory := buildDashboard(entries, dateRange)

	for name, dashboard := range map[string]DashboardResponse{"db": fromDB, "memory": inMemory} {
		if len(dashboard.TopCategories) != len(amounts) {
			t.Fatalf("%s: expected all %d categories, got %#v", name, len(amounts), dashboard.TopCategories)
		}
		if dashboard.TopCategories[0].Category != "Bills" ||
			dashboard.TopCategories[len(amounts)-1].Category != "Misc" {
			t.Fatalf("%s: breakdown is not ordered highest first: %#v", name, dashboard.TopCategories)
		}
		// The residual the app renders as "Other" must come out at zero when
		// nothing was truncated, or the donut would not add up to the total it
		// prints in its centre.
		charted := 0.0
		for _, category := range dashboard.TopCategories {
			charted += category.Amount
		}
		if charted != dashboard.Summary.TotalSpent {
			t.Fatalf("%s: breakdown %.2f does not reconcile with total spent %.2f",
				name, charted, dashboard.Summary.TotalSpent)
		}
	}
}

// TopCategories widened for the charts. Alerts must not widen with it: the
// category_increase loop takes the first match, so a small category far down
// the list could otherwise displace the categories that actually move the
// total.
func TestCategoryAlertsStayPinnedToTheLargestCategories(t *testing.T) {
	location := time.FixedZone("IST", 5*60*60+30*60)
	dateRange := dashboardRange{
		Start:         time.Date(2026, 7, 1, 0, 0, 0, 0, location),
		End:           time.Date(2026, 7, 3, 0, 0, 0, 0, location),
		PreviousStart: time.Date(2026, 6, 28, 0, 0, 0, 0, location),
		PreviousEnd:   time.Date(2026, 6, 30, 0, 0, 0, 0, location),
		Days:          3,
	}

	entries := []models.Entry{}
	// Five large categories, each flat against a baseline that clears the
	// floor, so none of them raises an alert.
	for _, category := range []string{"Bills", "Food & Drinks", "Shopping", "Transport", "Travel"} {
		for day := 28; day <= 30; day++ {
			entries = append(entries,
				dashboardEntry(fmtDate(2026, 6, day), "expense", "300", category, "Shop", 1, "Cash"),
				dashboardEntry(fmtDate(2026, 6, day), "expense", "300", category, "Shop", 1, "Cash"),
			)
		}
		for day := 1; day <= 3; day++ {
			entries = append(entries,
				dashboardEntry(fmtDate(2026, 7, day), "expense", "300", category, "Shop", 1, "Cash"),
				dashboardEntry(fmtDate(2026, 7, day), "expense", "300", category, "Shop", 1, "Cash"),
			)
		}
	}
	// A sixth, much smaller category that doubled. It clears the floor and it
	// is a real 100% rise — it is simply not one of the five that matter.
	for day := 28; day <= 30; day++ {
		entries = append(entries,
			dashboardEntry(fmtDate(2026, 6, day), "expense", "100", "Misc", "Shop", 1, "Cash"),
			dashboardEntry(fmtDate(2026, 6, day), "expense", "100", "Misc", "Shop", 1, "Cash"),
		)
	}
	for day := 1; day <= 3; day++ {
		entries = append(entries,
			dashboardEntry(fmtDate(2026, 7, day), "expense", "200", "Misc", "Shop", 1, "Cash"),
			dashboardEntry(fmtDate(2026, 7, day), "expense", "200", "Misc", "Shop", 1, "Cash"),
		)
	}

	dashboard := buildDashboard(entries, dateRange)
	if len(dashboard.TopCategories) != 6 {
		t.Fatalf("expected all six categories in the breakdown, got %#v", dashboard.TopCategories)
	}
	misc := dashboard.TopCategories[5]
	if misc.Category != "Misc" || !misc.ChangeComparable || misc.Change != 100 {
		t.Fatalf("fixture is wrong — Misc should be last and up 100%%: %#v", misc)
	}
	if card := insightByKind(dashboard.Insights, "category_increase"); card != nil {
		t.Fatalf("smallest category raised an alert it could not have raised before: %#v", card)
	}
}

func fmtDate(year int, month time.Month, day int) string {
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
}

func TestParseDashboardRangeRejectsInvalidAndOversizedRanges(t *testing.T) {
	now := time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC)
	_, fields := parseDashboardRange("2026-07-10", "2026-07-01", now)
	if fields["end_date"] == "" {
		t.Fatalf("expected inverted range error, got %v", fields)
	}
	_, fields = parseDashboardRange("2025-01-01", "2026-07-09", now)
	if fields["start_date"] == "" {
		t.Fatalf("expected maximum range error, got %v", fields)
	}
}

func dbDashboardEntry(
	userID, accountID uint,
	date, entryType, amount, category, merchant, mode, createdAt string,
) models.Entry {
	parsedAmount, err := models.ParseMoney(amount)
	if err != nil {
		panic(err)
	}
	parsedCreatedAt, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		panic(err)
	}
	return models.Entry{
		UserID: userID, AccountID: &accountID,
		Date: date, Type: entryType, Amount: parsedAmount, Category: category,
		Merchant: merchant, Mode: mode, Currency: "INR", Source: "manual",
		CreatedAt: parsedCreatedAt,
	}
}
