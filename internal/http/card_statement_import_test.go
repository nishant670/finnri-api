package http

import (
	"strings"
	"testing"

	"finnri/internal/database"
	"finnri/internal/models"
)

// Importing the rows the user picked is what actually closes the gap the
// reconciliation banner is complaining about.
func TestImportingMissingLinesClosesTheGap(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	card := createTestCard(t, user.ID)

	createCardSpend(t, user.ID, card.ID, "2026-07-10", "8200")
	statement := createTestStatement(t, user.ID, card, "2026-08-05", rupees(12400))

	before, err := reconcileStatement(&statement)
	if err != nil {
		t.Fatal(err)
	}
	if before.UnitemizedAmount != rupees(4200) {
		t.Fatalf("unitemized before = %s, want %s", before.UnitemizedAmount, rupees(4200))
	}

	lines := []statementLine{
		spendLine("2026-07-15", "CROMA RETAIL", 3000),
		spendLine("2026-07-22", "SWIGGY", 1200),
	}
	for _, line := range lines {
		line.Kind = classifyLine(line)
		entry := buildEntryFromStatementLine(&statement, line)
		if err := database.DB.Create(&entry).Error; err != nil {
			t.Fatal(err)
		}
	}

	after, err := reconcileStatement(&statement)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != reconcileBalanced {
		t.Fatalf("state = %q (gap %s), want balanced", after.State, after.Gap)
	}
	if statement.UnitemizedEntryID != nil {
		t.Fatal("the bucket should be gone once the real rows are in")
	}
}

// Importing the same statement twice must not double the ledger.
func TestImportingTheSameLineTwiceIsANoOp(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	card := createTestCard(t, user.ID)
	statement := createTestStatement(t, user.ID, card, "2026-08-05", rupees(3000))

	line := spendLine("2026-07-15", "CROMA RETAIL", 3000)
	line.Kind = classifyLine(line)

	first := buildEntryFromStatementLine(&statement, line)
	if err := database.DB.Create(&first).Error; err != nil {
		t.Fatal(err)
	}

	second := buildEntryFromStatementLine(&statement, line)
	if first.IdempotencyKey == nil || second.IdempotencyKey == nil {
		t.Fatal("imported entries need an idempotency key")
	}
	if *first.IdempotencyKey != *second.IdempotencyKey {
		t.Fatalf("keys differ for the same row: %q vs %q",
			*first.IdempotencyKey, *second.IdempotencyKey)
	}

	// The unique index is what actually enforces it.
	if err := database.DB.Create(&second).Error; err == nil {
		t.Fatal("a duplicate import was accepted")
	}
}

// Two genuinely different rows must not collide on the key.
func TestDifferentLinesGetDifferentKeys(t *testing.T) {
	statement := models.CardStatement{ID: 7}
	keys := map[string]bool{}
	for _, line := range []statementLine{
		spendLine("2026-07-15", "CROMA RETAIL", 3000),
		spendLine("2026-07-15", "CROMA RETAIL", 3001),
		spendLine("2026-07-16", "CROMA RETAIL", 3000),
		spendLine("2026-07-15", "RELIANCE DIGITAL", 3000),
	} {
		key := statementLineIdempotencyKey(statement.ID, line)
		if keys[key] {
			t.Fatalf("key collision on %+v", line)
		}
		keys[key] = true
		if len(key) > 128 {
			t.Fatalf("key is %d characters, longer than the column", len(key))
		}
	}
}

// A bank description of any length has to fit the column.
func TestIdempotencyKeyStaysBounded(t *testing.T) {
	line := spendLine("2026-07-15", strings.Repeat("VERY LONG MERCHANT NAME ", 40), 3000)
	if key := statementLineIdempotencyKey(1, line); len(key) > 128 {
		t.Fatalf("key is %d characters, longer than the column", len(key))
	}
}

// Fees and interest are filed as Bills; an unrecognised spend stays on the
// confirm-first fallback rather than being guessed at.
func TestImportedLineCategories(t *testing.T) {
	fee := spendLine("2026-08-05", "LATE PAYMENT FEE", 500)
	fee.Kind = classifyLine(fee)
	if got := statementLineCategory(fee); got != "Bills" {
		t.Errorf("fee category = %q, want Bills", got)
	}

	spend := spendLine("2026-07-10", "POS 4523 IND", 3400)
	spend.Kind = classifyLine(spend)
	if got := statementLineCategory(spend); got != defaultCategory {
		t.Errorf("spend category = %q, want %q", got, defaultCategory)
	}
}

// A credit becomes an income entry, or a refund would read as spending.
func TestImportedCreditBecomesIncome(t *testing.T) {
	statement := models.CardStatement{ID: 1, AccountID: 2, UserID: 3, Currency: "INR"}
	line := creditLine("2026-07-18", "AMAZON REFUND", 800)
	line.Kind = classifyLine(line)

	entry := buildEntryFromStatementLine(&statement, line)
	if entry.Type != "income" {
		t.Fatalf("entry type = %q, want income", entry.Type)
	}
	if entry.AccountID == nil || *entry.AccountID != statement.AccountID {
		t.Fatal("imported entry is not on the card")
	}
}

// The bucket is Finnri's own placeholder, not a transaction the bank billed.
// Leaving it in the comparison would let it match a real line and hide a gap.
func TestCycleLinesExcludeTheUnitemizedBucket(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	card := createTestCard(t, user.ID)

	createCardSpend(t, user.ID, card.ID, "2026-07-10", "8200")
	statement := createTestStatement(t, user.ID, card, "2026-08-05", rupees(12400))
	if _, err := reconcileStatement(&statement); err != nil {
		t.Fatal(err)
	}
	if statement.UnitemizedEntryID == nil {
		t.Fatal("expected a bucket entry to exist for this test to mean anything")
	}

	lines, err := loadCycleLedgerLines(&statement)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range lines {
		if line.EntryID == *statement.UnitemizedEntryID {
			t.Fatal("the unitemized bucket was offered to the matcher")
		}
	}
	if len(lines) != 1 {
		t.Fatalf("cycle lines = %d, want just the real spend", len(lines))
	}
}
