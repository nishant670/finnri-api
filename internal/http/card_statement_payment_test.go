package http

import (
	"testing"

	"finnri/internal/database"
	"finnri/internal/models"
)

// Paying a card bill must not read as fresh spending. The spending already
// happened when the card was used; the payment is the same money leaving the
// bank account afterwards. Without the exclusion, a ₹12,400 bill payment
// would show up as ₹12,400 of new expense in the month it was paid, on top of
// the ₹12,400 of card transactions that caused it.
func TestCardPaymentIsNotCountedAsSpending(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	card := createTestCard(t, user.ID)

	bank := models.Account{UserID: user.ID, Type: "bank", Name: "Savings", Color: "#123456"}
	if err := database.DB.Create(&bank).Error; err != nil {
		t.Fatal(err)
	}

	// The real spending, on the card.
	createCardSpend(t, user.ID, card.ID, "2026-08-10", "12400")

	// Settling the bill from the bank account.
	payment := models.Entry{
		UserID: user.ID, AccountID: &bank.ID, Title: "Credit card payment",
		Type: "expense", Amount: testMoney("12400"), Currency: "INR", Source: "manual",
		Category: defaultCategory, PurposeType: cardPaymentPurposeType, Date: "2026-08-25",
	}
	if err := database.DB.Create(&payment).Error; err != nil {
		t.Fatal(err)
	}

	summary, err := loadDashboardSummary(user.ID, "2026-08-01", "2026-08-31")
	if err != nil {
		t.Fatal(err)
	}
	if summary.TotalSpent != 12400 {
		t.Fatalf("total spent = %v, want 12400 — the card payment was counted twice", summary.TotalSpent)
	}
	if summary.TotalIncome != 0 {
		t.Fatalf("total income = %v, want 0", summary.TotalIncome)
	}
}

// The bank account it was paid from must still see the money leave, even
// though the dashboard totals ignore it.
func TestCardPaymentStillMovesTheBankAccountBalance(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)

	bank := models.Account{
		UserID: user.ID, Type: "bank", Name: "Savings", Color: "#123456",
		Balance: testMoney("50000"),
	}
	if err := database.DB.Create(&bank).Error; err != nil {
		t.Fatal(err)
	}
	payment := models.Entry{
		UserID: user.ID, AccountID: &bank.ID, Title: "Credit card payment",
		Type: "expense", Amount: testMoney("12400"), Currency: "INR", Source: "manual",
		Category: defaultCategory, PurposeType: cardPaymentPurposeType, Date: "2026-08-25",
	}
	if err := database.DB.Create(&payment).Error; err != nil {
		t.Fatal(err)
	}

	totals, err := loadAccountLedgerTotals(user.ID, "2026-08-01", "2026-08-31")
	if err != nil {
		t.Fatal(err)
	}
	summary := summariseAccount(bank, totals[bank.ID], cardStatementContext{}, testToday)
	if summary.RunningBalance == nil {
		t.Fatal("bank account has no running balance")
	}
	if *summary.RunningBalance != 37600 {
		t.Fatalf("running balance = %v, want 37600 (50000 - 12400)", *summary.RunningBalance)
	}
}

// Once a card has a real bill, the bank's number replaces the ledger's guess.
func TestStatementOutstandingReplacesTheLedgerFigure(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	card := createTestCard(t, user.ID)

	// Finnri only knows about ₹8,200 of the cycle.
	createCardSpend(t, user.ID, card.ID, "2026-07-10", "8200")
	// And ₹3,120 spent after the cycle closed.
	createCardSpend(t, user.ID, card.ID, "2026-08-10", "3120")

	statement := createTestStatement(t, user.ID, card, "2026-08-05", rupees(12400))
	if _, err := reconcileStatement(&statement); err != nil {
		t.Fatal(err)
	}

	contexts, err := loadCardStatementContexts(user.ID, []uint{card.ID})
	if err != nil {
		t.Fatal(err)
	}
	context := contexts[card.ID]
	if context.Latest == nil {
		t.Fatal("no statement context loaded")
	}
	if context.SpendAfterStatement != rupees(3120) {
		t.Fatalf("spend after statement = %s, want %s", context.SpendAfterStatement, rupees(3120))
	}

	totals, err := loadAccountLedgerTotals(user.ID, "2026-08-01", "2026-08-31")
	if err != nil {
		t.Fatal(err)
	}
	summary := summariseAccount(card, totals[card.ID], context, testToday)

	if summary.Limit == nil {
		t.Fatal("card summary has no limit block")
	}
	if summary.Limit.OutstandingSource != outstandingFromStatement {
		t.Fatalf("outstanding source = %q, want %q",
			summary.Limit.OutstandingSource, outstandingFromStatement)
	}
	// ₹12,400 billed and unpaid, plus ₹3,120 in the new cycle.
	if summary.Limit.Outstanding != rupees(15520) {
		t.Fatalf("outstanding = %s, want %s", summary.Limit.Outstanding, rupees(15520))
	}
	if summary.Limit.AvailableLimit == nil || *summary.Limit.AvailableLimit != rupees(184480) {
		t.Fatalf("available limit = %v, want %s", summary.Limit.AvailableLimit, rupees(184480))
	}
	if summary.CurrentStatement == nil {
		t.Fatal("card summary has no current statement")
	}
	if summary.CurrentStatement.RemainingDue != rupees(12400) {
		t.Fatalf("remaining due = %s, want %s", summary.CurrentStatement.RemainingDue, rupees(12400))
	}
}

// A draft carries no amount. Treating its zero total as the bank's answer
// would report a card with a real balance as fully paid off.
func TestDraftStatementDoesNotBecomeTheOutstandingFigure(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	card := createTestCard(t, user.ID)

	createCardSpend(t, user.ID, card.ID, "2026-07-10", "8200")
	statement := createTestStatement(t, user.ID, card, "2026-08-05", 0)
	statement.Status = statementStatusDraft
	if err := database.DB.Save(&statement).Error; err != nil {
		t.Fatal(err)
	}

	contexts, err := loadCardStatementContexts(user.ID, []uint{card.ID})
	if err != nil {
		t.Fatal(err)
	}
	if contexts[card.ID].Latest != nil {
		t.Fatal("a draft must not be used as the card's statement position")
	}

	totals, err := loadAccountLedgerTotals(user.ID, "2026-07-01", "2026-08-31")
	if err != nil {
		t.Fatal(err)
	}
	summary := summariseAccount(card, totals[card.ID], contexts[card.ID], testToday)
	if summary.Limit.OutstandingSource != outstandingFromLedger {
		t.Fatalf("outstanding source = %q, want %q",
			summary.Limit.OutstandingSource, outstandingFromLedger)
	}
	if summary.Limit.Outstanding != rupees(8200) {
		t.Fatalf("outstanding = %s, want %s", summary.Limit.Outstanding, rupees(8200))
	}
}
