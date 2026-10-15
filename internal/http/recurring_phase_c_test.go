package http

import (
	"fmt"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"finnri/internal/config"
	"finnri/internal/database"
	"finnri/internal/models"
)

func createInsightEntry(t *testing.T, userID uint, entryType, amount, tag, purpose string) {
	t.Helper()
	parsed, err := models.ParseMoney(amount)
	if err != nil {
		t.Fatal(err)
	}
	entry := models.Entry{
		UserID: userID, Type: entryType, Amount: parsed, Currency: "INR", Source: "manual",
		Title: "Entry " + amount, Category: "Misc", Mode: "UPI", Date: "2026-09-10",
		Tag: tag, PurposeType: purpose,
	}
	if err := database.DB.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}
}

func TestInvestmentsAreMoneyOutButNotSpending(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	createInsightEntry(t, user.ID, "expense", "1000", "General", "normal_spend")
	createInsightEntry(t, user.ID, "expense", "5000", "Investment", "normal_spend") // logged with the tag
	createInsightEntry(t, user.ID, "expense", "2000", "General", "investment")      // a recurring SIP
	createInsightEntry(t, user.ID, "income", "50000", "General", "normal_spend")

	summary, err := loadDashboardSummary(user.ID, "2026-09-01", "2026-09-30")
	if err != nil {
		t.Fatal(err)
	}
	if summary.TotalSpent != 1000 || summary.TotalInvested != 7000 || summary.MoneyOut != 8000 {
		t.Fatalf("spent %v / invested %v / money out %v, want 1000 / 7000 / 8000",
			summary.TotalSpent, summary.TotalInvested, summary.MoneyOut)
	}
	if summary.TotalIncome != 50000 {
		t.Fatalf("income = %v, want 50000", summary.TotalIncome)
	}
	if summary.LifetimeTransactionCount != 4 {
		t.Fatalf("investments are still activity: lifetime count = %d, want 4", summary.LifetimeTransactionCount)
	}
}

func TestMarkPaidCountsALoanEMIAndClosesTheLastOne(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	loan := models.Subscription{
		UserID: user.ID, Name: "Bike loan", Kind: recurringKindLoan, Amount: rupees(4000),
		BillingInterval: "monthly", NextDueDate: "2026-10-05", Status: "active",
		TotalInstalments: 12, InstalmentsPaid: 10, TransactionTag: "EMI",
	}
	if err := database.DB.Create(&loan).Error; err != nil {
		t.Fatal(err)
	}
	server := &Server{cfg: &config.Config{}}
	markPaid := func() {
		response := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(response)
		ctx.Request = httptest.NewRequest("POST", "/", strings.NewReader(`{}`))
		ctx.Request.Header.Set("Content-Type", "application/json")
		ctx.Set("userID", user.ID)
		ctx.Params = gin.Params{{Key: "id", Value: strconv.FormatUint(uint64(loan.ID), 10)}}
		server.markSubscriptionPaid(ctx)
		if response.Code != 200 {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
	}

	markPaid()
	var stored models.Subscription
	database.DB.First(&stored, loan.ID)
	if stored.InstalmentsPaid != 11 || stored.Status != "active" {
		t.Fatalf("a manual payment should count the EMI: %d paid, %s", stored.InstalmentsPaid, stored.Status)
	}

	markPaid()
	database.DB.First(&stored, loan.ID)
	if stored.InstalmentsPaid != 12 || stored.Status != subscriptionStatusCancelled {
		t.Fatalf("the last EMI closes the loan: %d paid, %s", stored.InstalmentsPaid, stored.Status)
	}
	var notifications []models.Notification
	database.DB.Where("user_id = ? AND type = ?", user.ID, recurringCompletedType).Find(&notifications)
	if len(notifications) != 1 || !strings.Contains(notifications[0].Body, "all 12 EMIs are done") ||
		notifications[0].ActionURL != fmt.Sprintf("/recurring/%d", loan.ID) {
		t.Fatalf("expected one paid-off notification, got %+v", notifications)
	}
}

func TestReminderCopyNamesTheKind(t *testing.T) {
	cases := map[string]string{
		recurringKindLoan:         "EMI due soon",
		recurringKindInvestment:   "Investment due soon",
		recurringKindBill:         "Bill due soon",
		recurringKindSubscription: "Subscription due soon",
		"":                        "Subscription due soon",
	}
	for kind, want := range cases {
		title, body := subscriptionReminderCopy(models.Subscription{Name: "Thing", Kind: kind, Amount: rupees(9965)}, "2026-11-05", "due_soon")
		if title != want || !strings.Contains(body, "₹9,965") {
			t.Errorf("kind %q: %q / %q", kind, title, body)
		}
	}
}
