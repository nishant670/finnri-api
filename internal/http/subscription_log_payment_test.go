package http

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"finnri/internal/database"
	"finnri/internal/models"
)

// The report: a PPF instalment added with today as its next payment never
// reached the transactions, and one added for next month left this month's
// out. Creating it can now log the payment already made.
func TestCreatingRecurringCanLogThePaymentAlreadyMade(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	user, token := createPaidBillingTestUserSession(t)

	today := time.Now().UTC()
	todayISO := today.Format("2006-01-02")
	nextMonth := today.AddDate(0, 1, 0).Format("2006-01-02")
	ppf := func(nextDue, logOn string) map[string]any {
		return map[string]any{
			"name":             "PPF",
			"merchant":         "PPF",
			"amount":           "3000.00",
			"billing_interval": "monthly",
			"next_due_date":    nextDue,
			"kind":             "investment",
			"payment_mode":     "Bank Account",
			"log_payment_on":   logOn,
		}
	}
	entriesFor := func(subscriptionID uint) []models.Entry {
		t.Helper()
		var occurrences []models.SubscriptionOccurrence
		if err := database.DB.Preload("Entry").
			Where("subscription_id = ?", subscriptionID).Find(&occurrences).Error; err != nil {
			t.Fatal(err)
		}
		entries := make([]models.Entry, 0, len(occurrences))
		for _, occurrence := range occurrences {
			if occurrence.Status != subscriptionOccurrenceConfirmed || occurrence.ConfirmedAt == nil {
				t.Fatalf("a payment the user logged needs no confirming: %#v", occurrence)
			}
			entries = append(entries, occurrence.Entry)
		}
		return entries
	}

	t.Run("due today: logs it and moves the schedule on", func(t *testing.T) {
		created := performJSONRequest[subscriptionResponse](t, router, http.MethodPost,
			"/v1/subscriptions", token, ppf(todayISO, todayISO), http.StatusCreated)
		if created.NextDueDate != nextMonth || created.LastChargedDate != todayISO {
			t.Fatalf("today's payment is made, so the next is a month on: %#v", created.Subscription)
		}
		entries := entriesFor(created.ID)
		if len(entries) != 1 || entries[0].Date != todayISO || entries[0].Amount.String() != "3000.00" ||
			entries[0].PurposeType != "investment" || entries[0].Tag != "Investment" || entries[0].UserID != user.ID {
			t.Fatalf("expected one ₹3,000 investment entry dated today, got %#v", entries)
		}
	})

	t.Run("next one next month: logs this month's and leaves the schedule", func(t *testing.T) {
		created := performJSONRequest[subscriptionResponse](t, router, http.MethodPost,
			"/v1/subscriptions", token, ppf(nextMonth, todayISO), http.StatusCreated)
		if created.NextDueDate != nextMonth {
			t.Fatalf("an earlier payment must not move the next one: %#v", created.Subscription)
		}
		if entries := entriesFor(created.ID); len(entries) != 1 || entries[0].Date != todayISO {
			t.Fatalf("expected this month's payment logged today, got %#v", entries)
		}
	})

	t.Run("without it, nothing is logged", func(t *testing.T) {
		created := performJSONRequest[subscriptionResponse](t, router, http.MethodPost,
			"/v1/subscriptions", token, ppf(nextMonth, ""), http.StatusCreated)
		if entries := entriesFor(created.ID); len(entries) != 0 {
			t.Fatalf("nothing was asked to be logged, got %#v", entries)
		}
	})

	t.Run("refuses a date after the next payment or in the future", func(t *testing.T) {
		performJSONRequest[map[string]any](t, router, http.MethodPost, "/v1/subscriptions", token,
			ppf(todayISO, nextMonth), http.StatusUnprocessableEntity)
		farAhead := today.AddDate(0, 2, 0).Format("2006-01-02")
		performJSONRequest[map[string]any](t, router, http.MethodPost, "/v1/subscriptions", token,
			ppf(farAhead, today.AddDate(0, 0, 5).Format("2006-01-02")), http.StatusUnprocessableEntity)
	})
}

// Autopay must not add a second copy of a payment the user already logged.
func TestAutopaySkipsAPaymentAlreadyLogged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	user, token := createPaidBillingTestUserSession(t)

	account := models.Account{UserID: user.ID, Name: "HDFC Bank", Type: "bank"}
	if err := database.DB.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	todayISO := time.Now().UTC().Format("2006-01-02")
	created := performJSONRequest[subscriptionResponse](t, router, http.MethodPost,
		"/v1/subscriptions", token, map[string]any{
			"name":             "Nifty 50 SIP",
			"amount":           "5000.00",
			"billing_interval": "monthly",
			"next_due_date":    todayISO,
			"kind":             "investment",
			"autopay":          true,
			"payment_mode":     "Bank Account",
			"account_id":       account.ID,
			"log_payment_on":   todayISO,
		}, http.StatusCreated)

	if _, err := syncSubscriptionAutomation(created.UserID, time.Now()); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := database.DB.Model(&models.SubscriptionOccurrence{}).
		Where("subscription_id = ?", created.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected the logged payment alone, got %d occurrences", count)
	}
}

// Marking a recurring payment paid moved its date on and wrote nothing, so
// the payment never reached the transactions. Asked to, it now does — from
// the account named for this payment — and an older app that does not ask
// keeps the old behaviour.
func TestMarkPaidCanLogTheTransaction(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	user, token := createPaidBillingTestUserSession(t)

	bank := models.Account{UserID: user.ID, Name: "SBI Savings", Type: "bank"}
	if err := database.DB.Create(&bank).Error; err != nil {
		t.Fatal(err)
	}
	netflix := performJSONRequest[subscriptionResponse](t, router, http.MethodPost,
		"/v1/subscriptions", token, map[string]any{
			"name":             "Netflix",
			"merchant":         "Netflix",
			"category":         "Entertainment",
			"amount":           "649.00",
			"billing_interval": "monthly",
			"next_due_date":    "2026-10-20",
			"payment_mode":     "Cash",
		}, http.StatusCreated)

	paid := performJSONRequest[subscriptionResponse](t, router, http.MethodPost,
		fmt.Sprintf("/v1/subscriptions/%d/mark-paid", netflix.ID), token, map[string]any{
			"paid_date":       "2026-10-15",
			"log_transaction": true,
			"account_id":      bank.ID,
			"payment_mode":    "Bank Account",
		}, http.StatusOK)
	if paid.NextDueDate != "2026-11-20" || paid.LoggedEntryID == nil {
		t.Fatalf("expected the schedule moved on and an entry logged: %#v", paid)
	}
	var entry models.Entry
	if err := database.DB.First(&entry, *paid.LoggedEntryID).Error; err != nil {
		t.Fatal(err)
	}
	if entry.Date != "2026-10-15" || entry.Amount.String() != "649.00" || entry.Type != "expense" ||
		entry.AccountID == nil || *entry.AccountID != bank.ID || entry.Mode != "Bank Account" ||
		entry.Category != "Entertainment" {
		t.Fatalf("expected ₹649 from SBI on the day it was paid, got %#v", entry)
	}
	var occurrence models.SubscriptionOccurrence
	if err := database.DB.Where("entry_id = ?", entry.ID).First(&occurrence).Error; err != nil {
		t.Fatal(err)
	}
	if occurrence.DueDate[:10] != "2026-10-20" || occurrence.Status != subscriptionOccurrenceConfirmed {
		t.Fatalf("the payment should settle the 20th, confirmed: %#v", occurrence)
	}
	var stored models.Subscription
	if err := database.DB.First(&stored, netflix.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.AccountID != nil || stored.PaymentMode != "Cash" {
		t.Fatalf("this payment's account must not change the item's: %#v", stored)
	}

	// Without log_transaction it only moves the date, as before.
	again := performJSONRequest[subscriptionResponse](t, router, http.MethodPost,
		fmt.Sprintf("/v1/subscriptions/%d/mark-paid", netflix.ID), token,
		map[string]any{"paid_date": "2026-11-18"}, http.StatusOK)
	if again.NextDueDate != "2026-12-20" || again.LoggedEntryID != nil {
		t.Fatalf("expected the date moved and nothing logged: %#v", again)
	}
	var count int64
	database.DB.Model(&models.SubscriptionOccurrence{}).Where("subscription_id = ?", netflix.ID).Count(&count)
	if count != 1 {
		t.Fatalf("expected the one logged payment, got %d", count)
	}

	// Someone else's account is refused.
	performJSONRequest[map[string]any](t, router, http.MethodPost,
		fmt.Sprintf("/v1/subscriptions/%d/mark-paid", netflix.ID), token,
		map[string]any{"log_transaction": true, "account_id": bank.ID + 999}, http.StatusUnprocessableEntity)
}

// The two entries the server writes on its own must carry a source the
// database allows. SQLite enforces no CHECK, so this is the guard: when
// 'recurring' and 'statement' were missing from entries_source_check, every
// autopay entry and statement import failed in PostgreSQL alone.
func TestServerWrittenEntriesUseAnAllowedSource(t *testing.T) {
	allowed := map[string]bool{}
	for _, source := range models.EntrySources {
		allowed[source] = true
	}
	recurring := newSubscriptionEntry(models.Subscription{Name: "Netflix", Amount: 649}, "2026-10-18")
	imported := buildEntryFromStatementLine(
		&models.CardStatement{AccountID: 1, StatementDate: "2026-10-01", Currency: "INR"},
		statementLine{Date: "2026-09-20", Description: "SWIGGY", Amount: 420},
	)
	for name, source := range map[string]string{"recurring": recurring.Source, "statement import": imported.Source} {
		if !allowed[source] {
			t.Errorf("%s entry has source %q, which entries_source_check rejects", name, source)
		}
	}
}
