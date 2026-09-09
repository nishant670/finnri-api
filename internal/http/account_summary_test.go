package http

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"finnri/internal/database"
	"finnri/internal/models"
)

func mustMoney(t *testing.T, value string) models.Money {
	t.Helper()
	amount, err := models.ParseMoney(value)
	if err != nil {
		t.Fatalf("ParseMoney(%q): %v", value, err)
	}
	return amount
}

func TestMonthToDateWindowRunsFromTheFirstToToday(t *testing.T) {
	now := time.Date(2026, time.August, 11, 22, 14, 0, 0, time.UTC)
	start, end := monthToDateWindow(now)
	if start != "2026-08-01" || end != "2026-08-11" {
		t.Fatalf("window = %s..%s, want 2026-08-01..2026-08-11", start, end)
	}
}

// The bare credit limit reading as a balance is the specific failure this task
// exists to remove: the audit's card showed ₹2,00,000 formatted exactly like
// money the user had. A card reports Outstanding; the limit is only ever a
// denominator.
func TestSummariseAccountReportsCreditCardOutstandingNotTheLimit(t *testing.T) {
	account := models.Account{
		ID:          7,
		Type:        "credit_card",
		CreditLimit: mustMoney(t, "200000"),
		Balance:     mustMoney(t, "1500"), // owed before the first logged entry
	}
	totals := accountLedgerTotals{
		LifetimeSpent:    24000,
		LifetimeReceived: 5000, // a payment against the card
	}

	summary := summariseAccount(account, totals, cardStatementContext{}, testToday)

	if summary.Outstanding == nil {
		t.Fatal("credit card summary has no outstanding figure")
	}
	if *summary.Outstanding != 20500 {
		t.Fatalf("outstanding = %v, want 20500 (1500 opening + 24000 spent - 5000 paid)", *summary.Outstanding)
	}
	if summary.CreditUtilisation == nil {
		t.Fatal("credit card with a limit has no utilisation")
	}
	if *summary.CreditUtilisation != 10.25 {
		t.Fatalf("utilisation = %v, want 10.25", *summary.CreditUtilisation)
	}
	if summary.RunningBalance != nil {
		t.Fatalf("a card must not carry a running balance, got %v", *summary.RunningBalance)
	}
}

func TestSummariseAccountOmitsUtilisationWithoutALimit(t *testing.T) {
	account := models.Account{ID: 8, Type: "credit_card"}
	summary := summariseAccount(account, accountLedgerTotals{LifetimeSpent: 3000}, cardStatementContext{}, testToday)

	if summary.Outstanding == nil || *summary.Outstanding != 3000 {
		t.Fatalf("outstanding = %v, want 3000", summary.Outstanding)
	}
	if summary.CreditUtilisation != nil {
		t.Fatalf("utilisation cannot be computed without a limit, got %v", *summary.CreditUtilisation)
	}
}

func TestSummariseAccountStartsStatementlessCardAfterPaidOffReset(t *testing.T) {
	resetAt := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	account := models.Account{
		Type: "credit_card", Balance: mustMoney(t, "1500"), CardLedgerResetAt: &resetAt,
	}
	summary := summariseAccount(account, accountLedgerTotals{
		LifetimeSpent: 24000, LifetimeReceived: 5000,
		CardSpentSinceReset: 800, CardReceivedSinceReset: 100,
	}, cardStatementContext{}, testToday)
	if summary.Outstanding == nil || *summary.Outstanding != 700 {
		t.Fatalf("outstanding after reset = %v, want 700 from only later entries", summary.Outstanding)
	}
}

// A card in credit has no bar to draw. Over the limit does, and it is worth
// showing, so only the negative side is clamped.
func TestSummariseAccountClampsNegativeUtilisationButNotOverLimit(t *testing.T) {
	inCredit := summariseAccount(
		models.Account{Type: "credit_card", CreditLimit: mustMoney(t, "10000")},
		accountLedgerTotals{LifetimeReceived: 500}, cardStatementContext{}, testToday,
	)
	if inCredit.CreditUtilisation == nil || *inCredit.CreditUtilisation != 0 {
		t.Fatalf("utilisation for a card in credit = %v, want 0", inCredit.CreditUtilisation)
	}

	overLimit := summariseAccount(
		models.Account{Type: "credit_card", CreditLimit: mustMoney(t, "10000")},
		accountLedgerTotals{LifetimeSpent: 12000}, cardStatementContext{}, testToday,
	)
	if overLimit.CreditUtilisation == nil || *overLimit.CreditUtilisation != 120 {
		t.Fatalf("utilisation over the limit = %v, want 120", overLimit.CreditUtilisation)
	}
}

func TestSummariseAccountRunsABalanceOnlyFromAnOpeningBalance(t *testing.T) {
	totals := accountLedgerTotals{LifetimeSpent: 4000, LifetimeReceived: 60000}

	withOpening := summariseAccount(
		models.Account{Type: "bank", Balance: mustMoney(t, "12000")},
		totals, cardStatementContext{}, testToday,
	)
	if withOpening.RunningBalance == nil || *withOpening.RunningBalance != 68000 {
		t.Fatalf("running balance = %v, want 68000 (12000 + 60000 - 4000)", withOpening.RunningBalance)
	}

	// Without an opening balance a running balance is net flow wearing a
	// balance's clothes, so none is published.
	withoutOpening := summariseAccount(models.Account{Type: "bank"}, totals, cardStatementContext{}, testToday)
	if withoutOpening.RunningBalance != nil {
		t.Fatalf("running balance without an opening balance = %v, want none", *withoutOpening.RunningBalance)
	}
	if withoutOpening.Outstanding != nil {
		t.Fatalf("a bank account must not carry an outstanding figure, got %v", *withoutOpening.Outstanding)
	}
}

// The end-to-end shape: real entries, the real query, the real endpoint. This
// is the regression that matters — the audit's accounts screen read ₹0 on every
// row while the ledger held the answer.
func TestListAccountsDerivesPerAccountFiguresFromTheLedger(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)

	auth := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{
			"device_id": "account-summary-device",
		}, http.StatusOK,
	)

	card := models.Account{
		UserID:      auth.User.ID,
		Type:        "credit_card",
		Name:        "HDFC Card",
		CreditLimit: mustMoney(t, "200000"),
	}
	unused := models.Account{UserID: auth.User.ID, Type: "wallet", Name: "Paytm"}
	if err := database.DB.Create(&card).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.DB.Create(&unused).Error; err != nil {
		t.Fatal(err)
	}

	// Anchor the fixture to the month the request will actually ask about.
	now := time.Now()
	thisMonth := func(day int) string {
		return time.Date(now.Year(), now.Month(), day, 0, 0, 0, 0, now.Location()).Format("2006-01-02")
	}
	lastMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).AddDate(0, -1, 0)

	entries := []models.Entry{
		{UserID: auth.User.ID, AccountID: &card.ID, Amount: mustMoney(t, "18000"), Type: "expense", Date: thisMonth(1), Currency: "INR", Source: "manual"},
		{UserID: auth.User.ID, AccountID: &card.ID, Amount: mustMoney(t, "2500"), Type: "expense", Date: thisMonth(1), Currency: "INR", Source: "manual"},
		{UserID: auth.User.ID, AccountID: &card.ID, Amount: mustMoney(t, "1000"), Type: "income", Date: thisMonth(1), Currency: "INR", Source: "manual"},
		// Older than the month-to-date window: counts lifetime, not this month.
		{UserID: auth.User.ID, AccountID: &card.ID, Amount: mustMoney(t, "4000"), Type: "expense", Date: lastMonth.Format("2006-01-02"), Currency: "INR", Source: "manual"},
	}
	if err := database.DB.Create(&entries).Error; err != nil {
		t.Fatal(err)
	}

	accounts := performJSONRequest[[]accountWithSummary](
		t, router, http.MethodGet, "/v1/accounts?tz=Asia/Kolkata", auth.Token, nil, http.StatusOK,
	)

	byName := map[string]accountWithSummary{}
	for _, account := range accounts {
		byName[account.Name] = account
	}

	cardResult, ok := byName["HDFC Card"]
	if !ok {
		t.Fatalf("credit card missing from %#v", accounts)
	}
	if cardResult.Summary.SpentThisMonth != 20500 {
		t.Fatalf("spent this month = %v, want 20500", cardResult.Summary.SpentThisMonth)
	}
	if cardResult.Summary.ReceivedThisMonth != 1000 {
		t.Fatalf("received this month = %v, want 1000", cardResult.Summary.ReceivedThisMonth)
	}
	if cardResult.Summary.EntriesThisMonth != 3 {
		t.Fatalf("entries this month = %d, want 3", cardResult.Summary.EntriesThisMonth)
	}
	if cardResult.Summary.LifetimeSpent != 24500 {
		t.Fatalf("lifetime spent = %v, want 24500", cardResult.Summary.LifetimeSpent)
	}
	if cardResult.Summary.EntriesTotal != 4 {
		t.Fatalf("entries total = %d, want 4", cardResult.Summary.EntriesTotal)
	}
	if cardResult.Summary.LastActivityDate != thisMonth(1) {
		t.Fatalf("last activity = %q, want %q", cardResult.Summary.LastActivityDate, thisMonth(1))
	}
	if cardResult.Summary.Outstanding == nil || *cardResult.Summary.Outstanding != 23500 {
		t.Fatalf("outstanding = %v, want 23500 (24500 spent - 1000 paid)", cardResult.Summary.Outstanding)
	}
	// The limit is still on the wire — it is just never the headline.
	if got := cardResult.CreditLimit.String(); got != "200000.00" {
		t.Fatalf("credit limit = %s, want 200000.00", got)
	}

	unusedResult, ok := byName["Paytm"]
	if !ok {
		t.Fatalf("wallet missing from %#v", accounts)
	}
	if unusedResult.Summary.EntriesTotal != 0 || unusedResult.Summary.SpentThisMonth != 0 {
		t.Fatalf("an account with no entries must report zeroes, got %#v", unusedResult.Summary)
	}
	if unusedResult.Summary.LastActivityDate != "" {
		t.Fatalf("last activity for an unused account = %q, want empty", unusedResult.Summary.LastActivityDate)
	}
}

func TestMarkCardPaidOffResetsOnlyTheStatementlessLedgerFallback(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)
	auth := performJSONRequest[AuthResponse](t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{
		"device_id": "card-paid-off-device",
	}, http.StatusOK)
	card := models.Account{UserID: auth.User.ID, Type: "credit_card", Name: "Ledger card"}
	if err := database.DB.Create(&card).Error; err != nil {
		t.Fatal(err)
	}
	before := models.Entry{
		UserID: auth.User.ID, AccountID: &card.ID, Type: "expense", Amount: mustMoney(t, "4000"),
		Date: testToday, Currency: "INR", Source: "manual",
	}
	if err := database.DB.Create(&before).Error; err != nil {
		t.Fatal(err)
	}
	performJSONRequest[models.Account](
		t, router, http.MethodPost, fmt.Sprintf("/v1/accounts/%d/paid-off", card.ID), auth.Token, nil, http.StatusOK,
	)
	after := models.Entry{
		UserID: auth.User.ID, AccountID: &card.ID, Type: "expense", Amount: mustMoney(t, "600"),
		Date: testToday, Currency: "INR", Source: "manual",
	}
	if err := database.DB.Create(&after).Error; err != nil {
		t.Fatal(err)
	}
	accounts := performJSONRequest[[]accountWithSummary](
		t, router, http.MethodGet, "/v1/accounts?tz=Asia/Kolkata", auth.Token, nil, http.StatusOK,
	)
	for _, result := range accounts {
		if result.ID == card.ID {
			if result.Summary.Outstanding == nil || *result.Summary.Outstanding != 600 {
				t.Fatalf("outstanding after paid-off reset = %v, want 600", result.Summary.Outstanding)
			}
			return
		}
	}
	t.Fatal("card missing from account response")
}

// Accounts are per-user, and so are their figures.
func TestListAccountsSummaryIgnoresOtherUsersEntries(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)

	owner := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{
			"device_id": "account-summary-owner",
		}, http.StatusOK,
	)
	intruder := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{
			"device_id": "account-summary-intruder",
		}, http.StatusOK,
	)

	ownerAccounts := performJSONRequest[[]accountWithSummary](
		t, router, http.MethodGet, "/v1/accounts", owner.Token, nil, http.StatusOK,
	)
	if len(ownerAccounts) == 0 {
		t.Fatal("expected a default guest account")
	}
	accountID := ownerAccounts[0].ID

	today := time.Now().Format("2006-01-02")
	// Same account id, other user: the query filters on user_id, so this must
	// not land on the owner's row.
	if err := database.DB.Create(&models.Entry{
		UserID: intruder.User.ID, AccountID: &accountID, Amount: mustMoney(t, "9999"),
		Type: "expense", Date: today, Currency: "INR", Source: "manual",
	}).Error; err != nil {
		t.Fatal(err)
	}

	ownerAccounts = performJSONRequest[[]accountWithSummary](
		t, router, http.MethodGet, "/v1/accounts", owner.Token, nil, http.StatusOK,
	)
	if ownerAccounts[0].Summary.EntriesTotal != 0 {
		t.Fatalf("owner picked up another user's entry: %#v", ownerAccounts[0].Summary)
	}
}

// The accounts screen and the Insights tab must not disagree about the same
// account over the same window. Both read month-to-date from the 1st to today.
func TestAccountSummaryMatchesDashboardAccountSpending(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)

	auth := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{
			"device_id": "account-summary-parity",
		}, http.StatusOK,
	)
	accounts := performJSONRequest[[]accountWithSummary](
		t, router, http.MethodGet, "/v1/accounts", auth.Token, nil, http.StatusOK,
	)
	accountID := accounts[0].ID

	now := time.Now()
	monthStart, monthEnd := monthToDateWindow(now)
	if err := database.DB.Create(&[]models.Entry{
		{UserID: auth.User.ID, AccountID: &accountID, Amount: mustMoney(t, "1250"), Type: "expense", Date: monthStart, Currency: "INR", Source: "manual"},
		{UserID: auth.User.ID, AccountID: &accountID, Amount: mustMoney(t, "750"), Type: "expense", Date: monthEnd, Currency: "INR", Source: "manual"},
	}).Error; err != nil {
		t.Fatal(err)
	}

	accounts = performJSONRequest[[]accountWithSummary](
		t, router, http.MethodGet, "/v1/accounts?tz=Asia/Kolkata", auth.Token, nil, http.StatusOK,
	)
	dashboard := performJSONRequest[DashboardResponse](
		t, router, http.MethodGet,
		fmt.Sprintf("/v1/dashboard?start_date=%s&end_date=%s&tz=Asia/Kolkata", monthStart, monthEnd),
		auth.Token, nil, http.StatusOK,
	)

	var dashboardAmount float64
	for _, account := range dashboard.AccountSpending {
		if account.AccountID != nil && *account.AccountID == accountID {
			dashboardAmount = account.Amount
		}
	}
	if dashboardAmount != accounts[0].Summary.SpentThisMonth {
		t.Fatalf("accounts screen says %v, dashboard says %v for the same account and window",
			accounts[0].Summary.SpentThisMonth, dashboardAmount)
	}
	if dashboardAmount != 2000 {
		t.Fatalf("expected 2000 spent this month, got %v", dashboardAmount)
	}
}

// testToday fixes the date these summaries are computed against, so a test
// cannot start failing because the calendar moved.
const testToday = "2026-08-18"
