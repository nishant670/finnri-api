package http

import (
	"strings"
	"testing"

	"finnri/internal/database"
	"finnri/internal/models"
)

func countStatementNotifications(t *testing.T, userID uint, kind string) int64 {
	t.Helper()
	var count int64
	if err := database.DB.Model(&models.Notification{}).
		Where("user_id = ? AND type = ?", userID, "card_statement."+kind).
		Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	return count
}

// The job runs every minute. Every reminder it can send must therefore be
// sent at most once, or a user gets sixty notifications an hour.
func TestCardStatementRemindersAreSentOnlyOnce(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	card := createTestCard(t, user.ID)

	statement := createTestStatement(t, user.ID, card, "2026-08-05", rupees(12400))
	_ = statement

	// Three days before the 25th, with the card's default lead time.
	first, err := syncCardStatementReminders(user.ID, mustDate(t, "2026-08-22"))
	if err != nil {
		t.Fatal(err)
	}
	if first != 1 {
		t.Fatalf("reminders on the first run = %d, want 1", first)
	}

	again, err := syncCardStatementReminders(user.ID, mustDate(t, "2026-08-22"))
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Fatalf("reminders on a repeat run = %d, want 0", again)
	}
	if got := countStatementNotifications(t, user.ID, statementReminderDueSoon); got != 1 {
		t.Fatalf("due-soon notifications = %d, want 1", got)
	}
}

// Each stage of a bill's life gets its own reminder, and they do not collapse
// into one another as the days pass.
func TestCardStatementReminderProgressesThroughItsStages(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	card := createTestCard(t, user.ID)
	createTestStatement(t, user.ID, card, "2026-08-05", rupees(12400))

	for _, step := range []struct {
		day  string
		kind string
	}{
		{"2026-08-22", statementReminderDueSoon},
		{"2026-08-25", statementReminderDueToday},
		{"2026-08-26", statementReminderOverdue},
	} {
		if _, err := syncCardStatementReminders(user.ID, mustDate(t, step.day)); err != nil {
			t.Fatal(err)
		}
		if got := countStatementNotifications(t, user.ID, step.kind); got != 1 {
			t.Fatalf("%s notifications on %s = %d, want 1", step.kind, step.day, got)
		}
	}
}

// A settled bill has nothing left to say.
func TestPaidStatementStopsReminding(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	card := createTestCard(t, user.ID)

	statement := createTestStatement(t, user.ID, card, "2026-08-05", rupees(12400))
	statement.PaidAmount = rupees(12400)
	statement.Status = statementStatusPaid
	if err := database.DB.Save(&statement).Error; err != nil {
		t.Fatal(err)
	}

	created, err := syncCardStatementReminders(user.ID, mustDate(t, "2026-08-26"))
	if err != nil {
		t.Fatal(err)
	}
	if created != 0 {
		t.Fatalf("reminders for a paid bill = %d, want 0", created)
	}
}

// A draft asks for the amount and nothing else — it has no total to be due.
func TestDraftStatementOnlyAsksForTheAmount(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	card := createTestCard(t, user.ID)

	statement := createTestStatement(t, user.ID, card, "2026-08-05", 0)
	statement.Status = statementStatusDraft
	if err := database.DB.Save(&statement).Error; err != nil {
		t.Fatal(err)
	}

	if _, err := syncCardStatementReminders(user.ID, mustDate(t, "2026-08-26")); err != nil {
		t.Fatal(err)
	}
	if got := countStatementNotifications(t, user.ID, statementReminderExpected); got != 1 {
		t.Fatalf("statement-expected notifications = %d, want 1", got)
	}
	if got := countStatementNotifications(t, user.ID, statementReminderOverdue); got != 0 {
		t.Fatalf("a draft cannot be overdue, got %d notifications", got)
	}
}

// The draft is what gives the "add your bill amount" reminder somewhere to
// point, and it must not be opened twice for the same cycle.
func TestDraftIsOpenedOncePerCycle(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	createTestCard(t, user.ID)

	created, err := openDueCardStatementDrafts(user.ID, mustDate(t, "2026-08-05"))
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 1 {
		t.Fatalf("drafts created = %d, want 1", len(created))
	}
	if created[0].StatementDate != "2026-08-05" || created[0].Status != statementStatusDraft {
		t.Fatalf("unexpected draft: %+v", created[0])
	}
	if created[0].CycleStart != "2026-07-06" || created[0].CycleEnd != "2026-08-05" {
		t.Fatalf("draft cycle = %s..%s, want 2026-07-06..2026-08-05",
			created[0].CycleStart, created[0].CycleEnd)
	}

	again, err := openDueCardStatementDrafts(user.ID, mustDate(t, "2026-08-06"))
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("drafts on a repeat run = %d, want 0", len(again))
	}
}

// Before this month's statement day, the cycle that has actually closed is
// last month's.
func TestDraftOpensForTheCycleThatHasClosed(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	createTestCard(t, user.ID)

	created, err := openDueCardStatementDrafts(user.ID, mustDate(t, "2026-08-03"))
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 1 || created[0].StatementDate != "2026-07-05" {
		t.Fatalf("draft statement date = %+v, want 2026-07-05", created)
	}
}

// A card with no statement day cannot have its cycle guessed at.
func TestNoDraftWithoutAStatementDay(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	card := models.Account{
		UserID: user.ID, Type: "credit_card", Name: "New card", Color: "#8257E5",
	}
	if err := database.DB.Create(&card).Error; err != nil {
		t.Fatal(err)
	}

	created, err := openDueCardStatementDrafts(user.ID, mustDate(t, "2026-08-05"))
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 0 {
		t.Fatalf("drafts = %d, want 0 — a cycle cannot be inferred", len(created))
	}
}

// Autopay changes the question, never the answer. Finnri must ask whether the
// debit went through rather than assuming it did: an autopay that silently
// failed but reads as paid costs a late fee and a credit score.
func TestAutopayAsksInsteadOfAssuming(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)

	card := createTestCard(t, user.ID)
	card.AutopayEnabled = true
	statement := createTestStatement(t, user.ID, card, "2026-08-05", rupees(12400))

	title, body := cardStatementReminderCopy(statement, card, statementReminderDueToday)
	if !strings.Contains(strings.ToLower(body), "did it go through") {
		t.Fatalf("autopay copy should ask for confirmation, got %q / %q", title, body)
	}

	if _, err := syncCardStatementReminders(user.ID, mustDate(t, "2026-08-25")); err != nil {
		t.Fatal(err)
	}

	// Asking must not have marked anything paid.
	var reloaded models.CardStatement
	if err := database.DB.First(&reloaded, statement.ID).Error; err != nil {
		t.Fatal(err)
	}
	if reloaded.PaidAmount != 0 || reloaded.Status == statementStatusPaid {
		t.Fatalf("autopay must not record a payment: paid=%s status=%s",
			reloaded.PaidAmount, reloaded.Status)
	}
	var payments int64
	database.DB.Model(&models.CardStatementPayment{}).
		Where("statement_id = ?", statement.ID).Count(&payments)
	if payments != 0 {
		t.Fatalf("payments recorded = %d, want 0", payments)
	}
}

// No reminder may imply Finnri can move money.
func TestReminderCopyNeverOffersToPay(t *testing.T) {
	card := models.Account{Name: "HDFC Regalia"}
	statement := models.CardStatement{
		TotalDue: rupees(12400), DueDate: "2026-08-25", StatementDate: "2026-08-05",
	}
	for _, kind := range []string{
		statementReminderExpected, statementReminderItemize,
		statementReminderDueSoon, statementReminderDueToday, statementReminderOverdue,
	} {
		title, body := cardStatementReminderCopy(statement, card, kind)
		if title == "" || body == "" {
			t.Fatalf("%s has empty copy", kind)
		}
		combined := strings.ToLower(title + " " + body)
		for _, banned := range []string{"pay now", "tap to pay", "pay bill now"} {
			if strings.Contains(combined, banned) {
				t.Fatalf("%s copy implies Finnri can pay: %q", kind, combined)
			}
		}
	}
}
