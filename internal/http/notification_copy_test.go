package http

import (
	"strings"
	"testing"
	"time"

	"finnri/internal/models"
)

// Notifications are read, not parsed: "30 Sep" and "₹19,155", never
// "2026-09-30" and "₹19155.00" (redesign bug 7, board R10).

func TestDateForCopy(t *testing.T) {
	now := time.Date(2026, time.November, 7, 12, 0, 0, 0, indiaLocation())
	cases := map[string]string{
		"2026-09-30":           "30 Sep",
		"2026-11-07":           "7 Nov",
		"2027-01-05":           "5 Jan 2027",
		"2025-12-31":           "31 Dec 2025",
		"2026-09-30T00:00:00Z": "30 Sep",
		"not a date":           "not a date",
		"":                     "",
	}
	for input, want := range cases {
		if got := dateForCopy(input, now); got != want {
			t.Errorf("dateForCopy(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSpendingSubject(t *testing.T) {
	cases := map[string]string{
		"Food & Drinks":    "Food & Drinks spending",
		"Monthly spending": "Monthly spending",
		"Monthly Spending": "Monthly Spending",
		"Monthly":          "Monthly spending",
		"  ":               "Your spending",
	}
	for input, want := range cases {
		if got := spendingSubject(input); got != want {
			t.Errorf("spendingSubject(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestBudgetAlertCopyReadsNaturally(t *testing.T) {
	overall := models.Budget{Name: "Monthly spending", LimitAmount: models.Money(2000000)}
	_, body := budgetAlertCopy(overall, "2026-11-01", budgetExceededKind, models.Money(2150050))
	want := "Monthly spending is ₹21,500.50 against your ₹20,000 monthly budget for Nov 2026."
	if body != want {
		t.Fatalf("overall budget alert = %q, want %q", body, want)
	}

	food := models.Budget{Name: "Food budget", Category: "Food & Drinks", LimitAmount: models.Money(800000)}
	_, body = budgetAlertCopy(food, "2026-11-01", "nearing", models.Money(640000))
	want = "Food & Drinks spending has reached ₹6,400 of your ₹8,000 monthly budget for Nov 2026."
	if body != want {
		t.Fatalf("category budget alert = %q, want %q", body, want)
	}
}

func TestCardStatementReminderCopyReadsNaturally(t *testing.T) {
	card := models.Account{Name: "SBI SimplySave"}
	statement := models.CardStatement{DueDate: "2026-09-30", TotalDue: models.Money(1915500)}
	_, body := cardStatementReminderCopy(statement, card, statementReminderOverdue)
	if !strings.Contains(body, "₹19,155 on your SBI SimplySave was due on 30 Sep") {
		t.Fatalf("overdue reminder = %q", body)
	}
	if strings.Contains(body, "2026-09-30") || strings.Contains(body, ".00") {
		t.Fatalf("overdue reminder still has raw values: %q", body)
	}
}

func TestSubscriptionReminderCopyReadsNaturally(t *testing.T) {
	subscription := models.Subscription{Name: "Netflix", Amount: models.Money(64900), NextDueDate: "2026-09-30"}
	_, body := subscriptionReminderCopy(subscription, "2026-09-30", subscriptionReminderOverdueKind)
	if !strings.Contains(body, "was due on 30 Sep for ₹649") {
		t.Fatalf("subscription reminder = %q", body)
	}
}
