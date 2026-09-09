package http

import (
	"testing"

	"finnri/internal/database"
	"finnri/internal/models"
)

func TestComputeReconciliation(t *testing.T) {
	cases := []struct {
		name           string
		totalDue       models.Money
		itemized       models.Money
		previousUnpaid models.Money
		wantState      string
		wantGap        models.Money
		wantUnitemized models.Money
	}{
		{
			name:     "everything itemised",
			totalDue: rupees(8200), itemized: rupees(8200),
			wantState: reconcileBalanced, wantGap: 0, wantUnitemized: 0,
		},
		{
			name:     "the bank knows more than Finnri",
			totalDue: rupees(12400), itemized: rupees(8200),
			wantState: reconcileUnder, wantGap: rupees(4200), wantUnitemized: rupees(4200),
		},
		{
			// Carried-forward debt is part of the bill without being spending
			// that happened in this cycle. Counting it closes the gap.
			name:     "carried-forward balance explains the difference",
			totalDue: rupees(12400), itemized: rupees(8200), previousUnpaid: rupees(4200),
			wantState: reconcileBalanced, wantGap: 0, wantUnitemized: 0,
		},
		{
			name:     "Finnri holds more than the bank billed",
			totalDue: rupees(8200), itemized: rupees(9400),
			wantState: reconcileOver, wantGap: rupees(-1200), wantUnitemized: 0,
		},
		{
			// Rounding must not produce a prompt about nothing.
			name:     "sub-rupee difference is ignored",
			totalDue: models.Money(820050), itemized: models.Money(820000),
			wantState: reconcileBalanced, wantGap: models.Money(50), wantUnitemized: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeReconciliation(tc.totalDue, tc.itemized, tc.previousUnpaid)
			if got.State != tc.wantState {
				t.Errorf("state = %q, want %q", got.State, tc.wantState)
			}
			if got.Gap != tc.wantGap {
				t.Errorf("gap = %s, want %s", got.Gap, tc.wantGap)
			}
			if got.UnitemizedAmount != tc.wantUnitemized {
				t.Errorf("unitemized = %s, want %s", got.UnitemizedAmount, tc.wantUnitemized)
			}
		})
	}
}

func TestDeriveStatementStatus(t *testing.T) {
	cases := []struct {
		name     string
		current  string
		totalDue models.Money
		paid     models.Money
		want     string
	}{
		{"a draft stays a draft until priced", statementStatusDraft, 0, 0, statementStatusDraft},
		{"nothing paid", statementStatusUnpaid, rupees(12400), 0, statementStatusUnpaid},
		{"part paid", statementStatusUnpaid, rupees(12400), rupees(5000), statementStatusPartial},
		{"paid in full", statementStatusPartial, rupees(12400), rupees(12400), statementStatusPaid},
		{"overpaid still counts as settled", statementStatusUnpaid, rupees(12400), rupees(15000), statementStatusPaid},
		{"a nil bill is settled", statementStatusUnpaid, 0, 0, statementStatusPaid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveStatementStatus(tc.current, tc.totalDue, tc.paid); got != tc.want {
				t.Fatalf("status = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStatementIsOverdue(t *testing.T) {
	cases := []struct {
		name   string
		status string
		due    string
		today  string
		want   bool
	}{
		{"unpaid and past due", statementStatusUnpaid, "2026-08-25", "2026-08-26", true},
		{"unpaid on the due date is not late", statementStatusUnpaid, "2026-08-25", "2026-08-25", false},
		{"part paid and past due is still late", statementStatusPartial, "2026-08-25", "2026-09-01", true},
		{"settled is never late", statementStatusPaid, "2026-08-25", "2026-09-01", false},
		{"a draft has nothing to be late for", statementStatusDraft, "2026-08-25", "2026-09-01", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			statement := models.CardStatement{Status: tc.status, DueDate: tc.due}
			if got := statementIsOverdue(statement, tc.today); got != tc.want {
				t.Fatalf("overdue = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- database-backed behaviour --------------------------------------------

func createTestCard(t *testing.T, userID uint) models.Account {
	t.Helper()
	account := models.Account{
		UserID: userID, Type: "credit_card", Name: "HDFC Regalia", Color: "#8257E5",
		CreditLimit: rupees(200000), StatementDay: 5, DueDay: 25,
	}
	if err := database.DB.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	return account
}

func createCardSpend(t *testing.T, userID, accountID uint, date, amount string) models.Entry {
	t.Helper()
	entry := models.Entry{
		UserID: userID, AccountID: &accountID, Title: "Card spend", Type: "expense",
		Amount: testMoney(amount), Currency: "INR", Source: "manual",
		Category: defaultCategory, Date: date,
	}
	if err := database.DB.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
	return entry
}

func createTestStatement(t *testing.T, userID uint, account models.Account, statementDate string, totalDue models.Money) models.CardStatement {
	t.Helper()
	date := mustDate(t, statementDate)
	start, end := statementCycle(date, account.StatementDay)
	statement := models.CardStatement{
		UserID: userID, AccountID: account.ID,
		CycleStart: start.Format(apiDateLayout), CycleEnd: end.Format(apiDateLayout),
		StatementDate: statementDate,
		DueDate:       dueDateFor(date, account.DueDay).Format(apiDateLayout),
		TotalDue:      totalDue, Currency: "INR",
		Status: statementStatusUnpaid, Source: "manual",
	}
	if err := database.DB.Create(&statement).Error; err != nil {
		t.Fatal(err)
	}
	return statement
}

// The money the bank billed but Finnri cannot explain must land in the
// ledger, not vanish. Otherwise the user's monthly spending total silently
// under-reports by the size of the gap.
func TestUnaccountedBillLandsInTheLedger(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	card := createTestCard(t, user.ID)

	createCardSpend(t, user.ID, card.ID, "2026-07-10", "5000")
	createCardSpend(t, user.ID, card.ID, "2026-07-28", "3200")

	statement := createTestStatement(t, user.ID, card, "2026-08-05", rupees(12400))

	result, err := reconcileStatement(&statement)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != reconcileUnder {
		t.Fatalf("state = %q, want %q", result.State, reconcileUnder)
	}
	if result.ItemizedTotal != rupees(8200) {
		t.Fatalf("itemized = %s, want %s", result.ItemizedTotal, rupees(8200))
	}
	if result.UnitemizedAmount != rupees(4200) {
		t.Fatalf("unitemized = %s, want %s", result.UnitemizedAmount, rupees(4200))
	}
	if statement.UnitemizedEntryID == nil {
		t.Fatal("no bucket entry was created")
	}

	// The bucket has to be a real expense on the card inside the cycle, or it
	// will not show up in the user's spending.
	var bucket models.Entry
	if err := database.DB.First(&bucket, *statement.UnitemizedEntryID).Error; err != nil {
		t.Fatal(err)
	}
	if bucket.Amount != rupees(4200) || bucket.Type != "expense" {
		t.Fatalf("bucket = %s %s, want 4200 expense", bucket.Amount, bucket.Type)
	}
	if bucket.AccountID == nil || *bucket.AccountID != card.ID {
		t.Fatal("bucket is not on the card")
	}
	if bucket.Date != statement.CycleEnd {
		t.Fatalf("bucket date = %s, want %s", bucket.Date, statement.CycleEnd)
	}
}

// Itemising is what shrinks the bucket. Adding the missing transactions must
// not double-count them against the statement.
func TestItemisingShrinksAndClearsTheBucket(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	card := createTestCard(t, user.ID)

	createCardSpend(t, user.ID, card.ID, "2026-07-10", "8200")
	statement := createTestStatement(t, user.ID, card, "2026-08-05", rupees(12400))

	if _, err := reconcileStatement(&statement); err != nil {
		t.Fatal(err)
	}
	bucketID := *statement.UnitemizedEntryID

	// The user remembers ₹3,000 of it.
	createCardSpend(t, user.ID, card.ID, "2026-07-15", "3000")
	result, err := reconcileStatement(&statement)
	if err != nil {
		t.Fatal(err)
	}
	if result.UnitemizedAmount != rupees(1200) {
		t.Fatalf("unitemized = %s, want %s", result.UnitemizedAmount, rupees(1200))
	}
	if statement.UnitemizedEntryID == nil || *statement.UnitemizedEntryID != bucketID {
		t.Fatal("bucket should have been updated in place, not replaced")
	}

	// And then the rest of it.
	createCardSpend(t, user.ID, card.ID, "2026-07-20", "1200")
	result, err = reconcileStatement(&statement)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != reconcileBalanced {
		t.Fatalf("state = %q, want %q", result.State, reconcileBalanced)
	}
	if statement.UnitemizedEntryID != nil {
		t.Fatal("bucket should be gone once everything is accounted for")
	}
	var remaining int64
	database.DB.Model(&models.Entry{}).Where("id = ?", bucketID).Count(&remaining)
	if remaining != 0 {
		t.Fatal("bucket entry was not deleted")
	}
}

// Reconciling twice with nothing changed must not compound: the bucket is
// excluded from the itemised total that determines its own size.
func TestReconcileIsIdempotent(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	card := createTestCard(t, user.ID)

	createCardSpend(t, user.ID, card.ID, "2026-07-10", "8200")
	statement := createTestStatement(t, user.ID, card, "2026-08-05", rupees(12400))

	first, err := reconcileStatement(&statement)
	if err != nil {
		t.Fatal(err)
	}
	second, err := reconcileStatement(&statement)
	if err != nil {
		t.Fatal(err)
	}

	if first.UnitemizedAmount != second.UnitemizedAmount {
		t.Fatalf("unitemized drifted: %s then %s", first.UnitemizedAmount, second.UnitemizedAmount)
	}
	if second.State != reconcileUnder {
		t.Fatalf("state = %q, want %q", second.State, reconcileUnder)
	}

	var buckets int64
	database.DB.Model(&models.Entry{}).
		Where("user_id = ? AND title = ?", user.ID, unitemizedTitle).Count(&buckets)
	if buckets != 1 {
		t.Fatalf("bucket entries = %d, want 1", buckets)
	}
}

// Spends outside the cycle belong to a different statement.
func TestReconcileIgnoresSpendOutsideTheCycle(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	card := createTestCard(t, user.ID)

	createCardSpend(t, user.ID, card.ID, "2026-07-05", "999")  // previous cycle
	createCardSpend(t, user.ID, card.ID, "2026-07-10", "8200") // this cycle
	createCardSpend(t, user.ID, card.ID, "2026-08-06", "777")  // next cycle

	statement := createTestStatement(t, user.ID, card, "2026-08-05", rupees(8200))
	result, err := reconcileStatement(&statement)
	if err != nil {
		t.Fatal(err)
	}
	if result.ItemizedTotal != rupees(8200) {
		t.Fatalf("itemized = %s, want %s", result.ItemizedTotal, rupees(8200))
	}
	if result.State != reconcileBalanced {
		t.Fatalf("state = %q, want %q", result.State, reconcileBalanced)
	}
}

// An unpaid remainder rolls into the next bill. Treating it as untracked
// spending would invent an expense the user never made.
func TestCarriedForwardBalanceIsNotTreatedAsUntrackedSpend(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	card := createTestCard(t, user.ID)

	july := createTestStatement(t, user.ID, card, "2026-07-05", rupees(10000))
	july.PaidAmount = rupees(6000)
	july.Status = statementStatusPartial
	if err := database.DB.Save(&july).Error; err != nil {
		t.Fatal(err)
	}

	createCardSpend(t, user.ID, card.ID, "2026-07-20", "8200")
	// ₹4,000 carried forward plus ₹8,200 of new spending.
	august := createTestStatement(t, user.ID, card, "2026-08-05", rupees(12200))

	result, err := reconcileStatement(&august)
	if err != nil {
		t.Fatal(err)
	}
	if result.PreviousUnpaid != rupees(4000) {
		t.Fatalf("previous unpaid = %s, want %s", result.PreviousUnpaid, rupees(4000))
	}
	if result.State != reconcileBalanced {
		t.Fatalf("state = %q (gap %s), want %q", result.State, result.Gap, reconcileBalanced)
	}
	if august.UnitemizedEntryID != nil {
		t.Fatal("carried-forward debt must not create a bucket entry")
	}
}

// Refunds and cashback inside the cycle reduce what the bank bills.
func TestRefundsReduceTheItemisedTotal(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	card := createTestCard(t, user.ID)

	createCardSpend(t, user.ID, card.ID, "2026-07-10", "9000")
	refund := models.Entry{
		UserID: user.ID, AccountID: &card.ID, Title: "Returned order", Type: "income",
		Amount: testMoney("800"), Currency: "INR", Source: "manual",
		Category: defaultCategory, Date: "2026-07-18",
	}
	if err := database.DB.Create(&refund).Error; err != nil {
		t.Fatal(err)
	}

	statement := createTestStatement(t, user.ID, card, "2026-08-05", rupees(8200))
	result, err := reconcileStatement(&statement)
	if err != nil {
		t.Fatal(err)
	}
	if result.ItemizedTotal != rupees(8200) {
		t.Fatalf("itemized = %s, want %s", result.ItemizedTotal, rupees(8200))
	}
	if result.State != reconcileBalanced {
		t.Fatalf("state = %q, want %q", result.State, reconcileBalanced)
	}
}

// A draft is a placeholder the reminder job opened. It has no amount, so
// reconciling it must not book a bucket for the whole cycle's spending.
func TestDraftStatementCreatesNoBucket(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	card := createTestCard(t, user.ID)

	createCardSpend(t, user.ID, card.ID, "2026-07-10", "8200")
	statement := createTestStatement(t, user.ID, card, "2026-08-05", 0)
	statement.Status = statementStatusDraft
	if err := database.DB.Save(&statement).Error; err != nil {
		t.Fatal(err)
	}

	if _, err := reconcileStatement(&statement); err != nil {
		t.Fatal(err)
	}
	if statement.UnitemizedEntryID != nil {
		t.Fatal("a draft must not create a bucket entry")
	}
}
