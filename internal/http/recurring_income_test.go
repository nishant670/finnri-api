package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"finnri/internal/config"
	"finnri/internal/database"
	"finnri/internal/models"
)

// Recurring income (device feedback 2026-11-17): a tester wanted to log a
// salary as recurring and found income had no repeat option. It runs on the
// same schedule machinery as recurring payments, as kind "income".

func TestRecurringIncomeKindIsAccepted(t *testing.T) {
	for _, value := range []string{"income", "Income", "salary"} {
		if got := normalizeRecurringKind(value); got != recurringKindIncome {
			t.Errorf("normalizeRecurringKind(%q) = %q, want income", value, got)
		}
	}
	kind := "income"
	var created models.Subscription
	subscriptionInput{
		Name: "Salary", Amount: rupees(85000), NextDueDate: "2026-12-01", Kind: &kind,
		TransactionTag: "Subscription", PaymentMode: "Bank Account",
	}.apply(&created)
	if created.Kind != recurringKindIncome || created.TransactionTag != "General" {
		t.Fatalf("income keeps the neutral tag, whatever the client sends: kind=%q tag=%q", created.Kind, created.TransactionTag)
	}
}

func TestRecurringIncomeRefusesACreditCard(t *testing.T) {
	kind := "income"
	fields := subscriptionInput{
		Name: "Salary", Amount: rupees(85000), NextDueDate: "2026-12-01", Kind: &kind, PaymentMode: "Credit Card",
	}.validate()
	if fields["payment_mode"] == "" {
		t.Fatalf("income received onto a credit card should be refused: %v", fields)
	}
}

func TestRecurringIncomeWritesIncomeEntries(t *testing.T) {
	salary := models.Subscription{
		ID: 7, UserID: 3, Name: "Salary", Category: "Salary", Amount: rupees(85000),
		Kind: recurringKindIncome, PaymentMode: "Bank Account",
	}
	entry := newSubscriptionEntry(salary, "2026-12-01")
	if entry.Type != "income" || entry.Category != "Salary" || entry.Tag != "" || len(entry.Tags) != 0 {
		t.Fatalf("a salary payment is an untagged income entry: %#v", entry)
	}

	bill := models.Subscription{ID: 8, UserID: 3, Name: "Rent", Category: "Rent", Amount: rupees(20000), Kind: recurringKindBill}
	if got := newSubscriptionEntry(bill, "2026-12-01"); got.Type != "expense" {
		t.Fatalf("outflows stay expenses: %#v", got)
	}
}

func TestRecurringIncomeReminderAsksWhetherItArrived(t *testing.T) {
	salary := models.Subscription{Name: "Salary", Amount: rupees(85000), Kind: recurringKindIncome}
	title, body := subscriptionReminderCopy(salary, "2026-12-01", subscriptionReminderDueKind)
	if title != "Salary expected" || !strings.Contains(body, "Mark it received") {
		t.Fatalf("due copy = %q / %q", title, body)
	}
	title, body = subscriptionReminderCopy(salary, "2026-12-01", subscriptionReminderOverdueKind)
	if title != "Salary not marked received" || strings.Contains(strings.ToLower(body), "overdue") {
		t.Fatalf("overdue copy = %q / %q", title, body)
	}
	if got := recurringAutopayBody(models.Subscription{Name: "Salary", Amount: rupees(85000), Kind: recurringKindIncome, PaymentMode: "Bank Account"}); !strings.Contains(got, "to Bank Account") {
		t.Fatalf("autopay income names where it landed: %q", got)
	}
	if recurringAutopayTitle(recurringKindIncome) != "Income recorded" {
		t.Fatal("autopay title for income")
	}
}

func TestRecurringSummaryKeepsIncomeApart(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	soon := time.Now().AddDate(0, 0, 2).Format(apiDateLayout)
	later := time.Now().AddDate(0, 0, 10).Format(apiDateLayout)
	for _, item := range []models.Subscription{
		{UserID: user.ID, Name: "Netflix", Kind: recurringKindSubscription, Amount: rupees(649), BillingInterval: "monthly", NextDueDate: later, Status: "active"},
		{UserID: user.ID, Name: "Salary", Kind: recurringKindIncome, Amount: rupees(85000), BillingInterval: "monthly", NextDueDate: soon, Status: "active"},
		{UserID: user.ID, Name: "Old retainer", Kind: recurringKindIncome, Amount: rupees(5000), BillingInterval: "monthly", NextDueDate: soon, Status: "paused"},
	} {
		if err := database.DB.Create(&item).Error; err != nil {
			t.Fatal(err)
		}
	}
	server := &Server{cfg: &config.Config{}}
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Request = httptest.NewRequest("GET", "/v1/recurring", nil)
	ctx.Set("userID", user.ID)
	server.listRecurring(ctx)
	if response.Code != 200 {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var overview recurringOverview
	if err := json.Unmarshal(response.Body.Bytes(), &overview); err != nil {
		t.Fatal(err)
	}
	summary := overview.Summary
	if summary.MonthlyTotal != rupees(649) || summary.ActiveCount != 1 {
		t.Fatalf("income must not be counted as a commitment: %+v", summary)
	}
	if _, ok := summary.ByKind[recurringKindIncome]; ok {
		t.Fatalf("by_kind is money out only: %+v", summary.ByKind)
	}
	if summary.NextDue == nil || summary.NextDue.Name != "Netflix" {
		t.Fatalf("next due is the next payment out, not the salary: %+v", summary.NextDue)
	}
	if summary.MonthlyIncome != rupees(85000) || summary.IncomeActiveCount != 1 ||
		summary.NextIncome == nil || summary.NextIncome.Name != "Salary" {
		t.Fatalf("active income summed apart: %+v", summary)
	}
}

func TestMarkingSalaryReceivedLogsIncome(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	user, token := createPaidBillingTestUserSession(t)

	bank := models.Account{UserID: user.ID, Name: "HDFC Savings", Type: "bank"}
	if err := database.DB.Create(&bank).Error; err != nil {
		t.Fatal(err)
	}
	salary := performJSONRequest[subscriptionResponse](t, router, http.MethodPost,
		"/v1/subscriptions", token, map[string]any{
			"name":             "Salary",
			"category":         "Salary",
			"amount":           "85000.00",
			"billing_interval": "monthly",
			"next_due_date":    "2026-12-01",
			"kind":             "income",
			"payment_mode":     "Bank Account",
			"account_id":       bank.ID,
			"log_payment_on":   "2026-11-01",
		}, http.StatusCreated)
	if salary.Kind != recurringKindIncome || salary.TransactionTag != "General" {
		t.Fatalf("created as income with the neutral tag: %#v", salary.Subscription)
	}

	// The month it was set up in is logged as income, into HDFC.
	var setupEntries []models.Entry
	database.DB.Where("user_id = ? AND type = ?", user.ID, "income").Find(&setupEntries)
	if len(setupEntries) != 1 || setupEntries[0].Date != "2026-11-01" || setupEntries[0].AccountID == nil ||
		*setupEntries[0].AccountID != bank.ID || setupEntries[0].Category != "Salary" {
		t.Fatalf("expected November's salary logged as income into HDFC, got %#v", setupEntries)
	}

	// December's arrives a day late and is marked received.
	paid := performJSONRequest[subscriptionResponse](t, router, http.MethodPost,
		fmt.Sprintf("/v1/subscriptions/%d/mark-paid", salary.ID), token, map[string]any{
			"paid_date":       "2026-12-02",
			"log_transaction": true,
		}, http.StatusOK)
	if paid.NextDueDate != "2027-01-01" || paid.LoggedEntryID == nil {
		t.Fatalf("expected next salary in January and an entry logged: %#v", paid)
	}
	var entry models.Entry
	if err := database.DB.First(&entry, *paid.LoggedEntryID).Error; err != nil {
		t.Fatal(err)
	}
	if entry.Type != "income" || entry.Date != "2026-12-02" || entry.Amount.String() != "85000.00" || entry.Tag != "" {
		t.Fatalf("expected ₹85,000 income on the 2nd, got %#v", entry)
	}
}

// schema.go re-applies the kind constraint on every boot, so the migration
// alone would be undone. Both must allow income.
func TestIncomeKindIsInBothConstraintDefinitions(t *testing.T) {
	for _, path := range []string{"../database/schema.go", "../../migrations/0078_recurring_income_kind.sql"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), "'bill', 'income'") {
			t.Fatalf("%s does not allow kind 'income'", path)
		}
	}
}
