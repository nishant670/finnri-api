package http

import (
	"testing"
	"time"

	"finnri/internal/database"
	"finnri/internal/models"
)

func TestSubscriptionResponseDueStates(t *testing.T) {
	now := mustDate(t, "2026-07-13")
	tests := []struct {
		name         string
		subscription models.Subscription
		wantState    string
		wantDays     int
	}{
		{"scheduled", models.Subscription{Status: "active", NextDueDate: "2026-07-20", ReminderDays: 3}, "scheduled", 7},
		{"due soon", models.Subscription{Status: "active", NextDueDate: "2026-07-15", ReminderDays: 3}, "due_soon", 2},
		{"overdue", models.Subscription{Status: "active", NextDueDate: "2026-07-12", ReminderDays: 3}, "overdue", -1},
		{"paused", models.Subscription{Status: "paused", NextDueDate: "2026-07-12", ReminderDays: 3}, "paused", -1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := buildSubscriptionResponse(test.subscription, now)
			if response.DueState != test.wantState || response.DaysUntilDue != test.wantDays {
				t.Fatalf("response = %#v", response)
			}
		})
	}
}

func TestAdvanceSubscriptionDueDate(t *testing.T) {
	paid := mustDate(t, "2026-07-13")
	tests := []struct {
		name     string
		dueDate  string
		interval string
		want     string
	}{
		{"monthly after due", "2026-07-10", "monthly", "2026-08-10"},
		{"daily catches up", "2026-07-10", "daily", "2026-07-14"},
		{"market daily skips weekend", "2026-07-31", "business_daily", "2026-08-03"},
		{"market daily skips NSE MF holiday", "2026-09-13", "business_daily", "2026-09-15"},
		{"weekly catches up", "2026-06-20", "weekly", "2026-07-18"},
		{"biweekly catches up", "2026-06-20", "biweekly", "2026-07-18"},
		{"early payment advances one cycle", "2026-07-20", "monthly", "2026-08-20"},
		{"quarterly", "2026-07-01", "quarterly", "2026-10-01"},
		{"yearly", "2026-07-01", "yearly", "2027-07-01"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := advanceSubscriptionDueDate(test.dueDate, test.interval, paid)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("next due = %s, want %s", got, test.want)
			}
		})
	}
}

func TestSubscriptionRemindersDeduplicateByDueDateAndKind(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	subscription := models.Subscription{
		UserID: user.ID, Name: "Streamly", Amount: testMoney("499"), Currency: "INR",
		BillingInterval: "monthly", NextDueDate: "2026-07-15", Status: "active", ReminderDays: 3,
	}
	if err := database.DB.Create(&subscription).Error; err != nil {
		t.Fatal(err)
	}

	created, err := syncSubscriptionReminders(user.ID, mustDate(t, "2026-07-13"))
	if err != nil {
		t.Fatal(err)
	}
	if created != 1 {
		t.Fatalf("created = %d, want 1", created)
	}
	created, err = syncSubscriptionReminders(user.ID, mustDate(t, "2026-07-13"))
	if err != nil {
		t.Fatal(err)
	}
	if created != 0 {
		t.Fatalf("duplicate created = %d, want 0", created)
	}

	assertBudgetNotificationCount(t, user.ID, "subscription.due", 1)
}

func TestSubscriptionCancelBeforeDueReminderCopy(t *testing.T) {
	subscription := models.Subscription{
		Name: "Streamly", Amount: testMoney("499"), CancelBeforeDue: true, NextDueDate: "2026-07-20",
	}
	title, body := subscriptionReminderCopy(subscription, "2026-07-18", subscriptionReminderCancelKind)
	if title != "Cancellation reminder" || body == "" {
		t.Fatalf("copy = %q %q", title, body)
	}
}

func TestDailyAutopaySuppressesDueReminderAndCreatesOccurrence(t *testing.T) {
	useSmokeDatabase(t)
	if err := database.DB.AutoMigrate(&models.Subscription{}, &models.SubscriptionOccurrence{}, &models.PushDevice{}); err != nil {
		t.Fatal(err)
	}
	user := createBudgetTestUser(t)
	account := models.Account{UserID: user.ID, Type: "bank", Name: "Savings account", Color: "#123456"}
	if err := database.DB.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	subscription := models.Subscription{
		UserID: user.ID, AccountID: &account.ID, Name: "Small cap SIP", Amount: testMoney("100"), Currency: "INR",
		BillingInterval: "business_daily", NextDueDate: "2026-07-13", Status: "active", ReminderDays: 0,
		AutoPay: true, PaymentMode: "Bank Account", TransactionTag: "Investment", PurposeType: "investment",
	}
	if err := database.DB.Create(&subscription).Error; err != nil {
		t.Fatal(err)
	}
	created, err := syncSubscriptionAutomation(user.ID, mustDate(t, "2026-07-13"))
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 1 || created[0].Entry.Type != "expense" || created[0].Entry.Mode != "Bank Account" || created[0].Entry.Tag != "Investment" {
		t.Fatalf("unexpected occurrence: %#v", created)
	}
	reminders, err := syncSubscriptionReminders(user.ID, mustDate(t, "2026-07-13"))
	if err != nil || reminders != 0 {
		t.Fatalf("reminders = %d, err = %v", reminders, err)
	}
	createdAgain, err := syncSubscriptionAutomation(user.ID, mustDate(t, "2026-07-13"))
	if err != nil || len(createdAgain) != 0 {
		t.Fatalf("duplicate occurrences = %d, err = %v", len(createdAgain), err)
	}
}

func mustDate(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
