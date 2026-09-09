package http

import (
	"time"

	"finnri/internal/database"
	"finnri/internal/models"
)

/*
Per-account figures derived from the ledger.

`accounts.balance` is a number the user typed once and almost never returns to,
so every account read ₹0 with a "No balance" chip after ₹40,091 of tracked
spending — while the dashboard on the next tab already knew ₹19,004 had gone
through HDFC Savings. The entries table has always been able to answer this;
nothing asked it.

Two rules hold everything here together:

  - `balance` is an *opening* balance — what the account held (or, on a card,
    owed) before the first logged transaction. It is never shown as if it were
    a current figure on its own.
  - `credit_limit` is a limit. It is never rendered where a balance would be.
    A card's headline is Outstanding, and the limit only appears as the
    denominator of "₹X of ₹Y used".
*/

// accountSummary is what an account can prove from its transactions. Every
// field is derived; nothing here is user-entered.
type accountSummary struct {
	// Month-to-date: the 1st through today, matching the dashboard's default
	// range, so the two screens cannot disagree about the same account.
	SpentThisMonth    float64 `json:"spent_this_month"`
	ReceivedThisMonth float64 `json:"received_this_month"`
	EntriesThisMonth  int     `json:"entries_this_month"`

	// Everything ever logged against this account.
	LifetimeSpent    float64 `json:"lifetime_spent"`
	LifetimeReceived float64 `json:"lifetime_received"`
	EntriesTotal     int     `json:"entries_total"`

	// The most recent entry date, YYYY-MM-DD. Empty when the account has never
	// been used — which is the only honest reason to show an account no figure.
	LastActivityDate string `json:"last_activity_date,omitempty"`

	// Credit cards only. Outstanding is what is owed; utilisation is a
	// percentage of the limit and may exceed 100 when the card is over it.
	//
	// Both are retained in their original shape for clients that already read
	// them, but once a card has a real statement they are sourced from it
	// rather than from the ledger — see Limit below.
	Outstanding       *float64 `json:"outstanding,omitempty"`
	CreditUtilisation *float64 `json:"credit_utilisation,omitempty"`

	// Credit cards only. The full limit breakdown a card screen leads with,
	// including what EMI plans have blocked and where the outstanding figure
	// came from.
	Limit *cardLimitSummary `json:"limit,omitempty"`

	// Credit cards with at least one priced statement. The bill to pay.
	CurrentStatement *currentStatementSummary `json:"current_statement,omitempty"`

	// Everything else, and only when an opening balance was actually entered:
	// opening + money in - money out.
	RunningBalance *float64 `json:"running_balance,omitempty"`
}

// accountWithSummary is the account as clients have always received it, with
// the derived figures alongside. models.Account is embedded rather than nested
// so the payload stays backwards compatible.
type accountWithSummary struct {
	models.Account
	Summary accountSummary `json:"summary"`
}

// accountLedgerTotals is one account's raw aggregates, straight from SQL.
type accountLedgerTotals struct {
	AccountID              uint
	SpentThisMonth         float64
	ReceivedThisMonth      float64
	EntriesThisMonth       int
	LifetimeSpent          float64
	LifetimeReceived       float64
	CardSpentSinceReset    float64
	CardReceivedSinceReset float64
	EntriesTotal           int
	LastActivityDate       string
}

// monthToDateWindow is the range the accounts screen reports on: the 1st of the
// current month through today. It deliberately matches parseDashboardRange's
// default so "spent this month" means the same thing on both tabs. Ending on
// today rather than the last of the month also keeps a future-dated entry from
// inflating a figure the user is reading as "so far".
func monthToDateWindow(now time.Time) (string, string) {
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	end := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	return start.Format("2006-01-02"), end.Format("2006-01-02")
}

// loadAccountLedgerTotals aggregates every account's entries in one grouped
// query, keyed by account id. Accounts with no entries are simply absent from
// the map, and a zero accountLedgerTotals is the correct answer for them.
//
// Conditional sums rather than PostgreSQL's FILTER clause: the smoke suite runs
// this same code against SQLite.
func loadAccountLedgerTotals(userID uint, monthStart, monthEnd string) (map[uint]accountLedgerTotals, error) {
	var rows []accountLedgerTotals
	if err := database.DB.Model(&models.Entry{}).
		Joins("JOIN accounts AS ledger_account ON ledger_account.id = entries.account_id").
		Select(`entries.account_id AS account_id,
			COALESCE(SUM(CASE WHEN LOWER(entries.type) = 'expense' AND entries.date >= ? AND entries.date <= ? THEN entries.amount ELSE 0 END), 0) AS spent_this_month,
			COALESCE(SUM(CASE WHEN LOWER(entries.type) <> 'expense' AND entries.date >= ? AND entries.date <= ? THEN entries.amount ELSE 0 END), 0) AS received_this_month,
			COALESCE(SUM(CASE WHEN entries.date >= ? AND entries.date <= ? THEN 1 ELSE 0 END), 0) AS entries_this_month,
			COALESCE(SUM(CASE WHEN LOWER(entries.type) = 'expense' THEN entries.amount ELSE 0 END), 0) AS lifetime_spent,
			COALESCE(SUM(CASE WHEN LOWER(entries.type) <> 'expense' THEN entries.amount ELSE 0 END), 0) AS lifetime_received,
			COALESCE(SUM(CASE WHEN LOWER(entries.type) = 'expense' AND (ledger_account.card_ledger_reset_entry_id IS NULL OR entries.id > ledger_account.card_ledger_reset_entry_id) THEN entries.amount ELSE 0 END), 0) AS card_spent_since_reset,
			COALESCE(SUM(CASE WHEN LOWER(entries.type) <> 'expense' AND (ledger_account.card_ledger_reset_entry_id IS NULL OR entries.id > ledger_account.card_ledger_reset_entry_id) THEN entries.amount ELSE 0 END), 0) AS card_received_since_reset,
			COUNT(*) AS entries_total,
			COALESCE(MAX(entries.date), '') AS last_activity_date`,
			monthStart, monthEnd,
			monthStart, monthEnd,
			monthStart, monthEnd).
		Where("entries.user_id = ? AND entries.account_id IS NOT NULL", userID).
		Group("entries.account_id").
		Scan(&rows).Error; err != nil {
		return nil, err
	}

	totals := make(map[uint]accountLedgerTotals, len(rows))
	for _, row := range rows {
		totals[row.AccountID] = row
	}
	return totals, nil
}

// summariseAccount turns one account's raw totals into the figures a screen can
// render without doing arithmetic of its own.
func summariseAccount(account models.Account, totals accountLedgerTotals, statement cardStatementContext, today string) accountSummary {
	summary := accountSummary{
		SpentThisMonth:    totals.SpentThisMonth,
		ReceivedThisMonth: totals.ReceivedThisMonth,
		EntriesThisMonth:  totals.EntriesThisMonth,
		LifetimeSpent:     totals.LifetimeSpent,
		LifetimeReceived:  totals.LifetimeReceived,
		EntriesTotal:      totals.EntriesTotal,
		LastActivityDate:  totals.LastActivityDate,
	}

	opening := account.Balance.Float64()

	if normalizeAccountType(account.Type) == "credit_card" {
		// The ledger's own answer: opening balance plus everything spent,
		// less everything credited back. Used only when the card has no
		// statement to do better.
		ledgerOutstanding := models.Money(0)
		spentForOutstanding := totals.CardSpentSinceReset
		receivedForOutstanding := totals.CardReceivedSinceReset
		if account.CardLedgerResetEntryID == nil && account.CardLedgerResetAt == nil {
			ledgerOutstanding += account.Balance
			// The explicit since-reset aggregates equal lifetime totals in SQL
			// when no reset exists. Use the lifetime fields here as well so pure
			// summary unit tests and older callers remain truthful.
			spentForOutstanding = totals.LifetimeSpent
			receivedForOutstanding = totals.LifetimeReceived
		}
		ledgerOutstanding += models.Money(spentForOutstanding * 100)
		ledgerOutstanding -= models.Money(receivedForOutstanding * 100)

		input := cardLimitInput{
			CreditLimit:         account.CreditLimit,
			LedgerOutstanding:   ledgerOutstanding,
			EMIBlockedPrincipal: statement.EMIBlockedPrincipal,
		}
		if statement.Latest != nil {
			input.HasStatement = true
			input.StatementTotalDue = statement.Latest.TotalDue
			input.StatementPaid = statement.Latest.PaidAmount
			input.SpendAfterStatement = statement.SpendAfterStatement
		}

		limit := summariseCardLimit(input)
		summary.Limit = &limit

		// Kept in their original float shape so existing clients keep working
		// while they migrate to `limit`.
		outstanding := limit.Outstanding.Float64()
		summary.Outstanding = &outstanding
		if limit.UtilisationPct != nil {
			utilisation := *limit.UtilisationPct
			if utilisation < 0 {
				utilisation = 0
			}
			summary.CreditUtilisation = &utilisation
		}

		if statement.Latest != nil {
			current := summariseCurrentStatement(*statement.Latest, today)
			summary.CurrentStatement = &current
		}
		return summary
	}

	// Without an opening balance a running balance would just be net flow
	// dressed up as a bank balance, which is the class of lie this task exists
	// to remove. Spent-this-month carries the row instead.
	if opening != 0 {
		running := opening + totals.LifetimeReceived - totals.LifetimeSpent
		summary.RunningBalance = &running
	}
	return summary
}

// summariseAccounts pairs each account with its derived figures.
func summariseAccounts(
	accounts []models.Account,
	totals map[uint]accountLedgerTotals,
	statements map[uint]cardStatementContext,
	today string,
) []accountWithSummary {
	summarised := make([]accountWithSummary, 0, len(accounts))
	for _, account := range accounts {
		summarised = append(summarised, accountWithSummary{
			Account: account,
			Summary: summariseAccount(account, totals[account.ID], statements[account.ID], today),
		})
	}
	return summarised
}

// creditCardIDs picks out the accounts worth loading statements for.
func creditCardIDs(accounts []models.Account) []uint {
	ids := make([]uint, 0, len(accounts))
	for _, account := range accounts {
		if normalizeAccountType(account.Type) == "credit_card" {
			ids = append(ids, account.ID)
		}
	}
	return ids
}
