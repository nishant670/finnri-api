package http

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"finnri/internal/config"
	"finnri/internal/database"
	"finnri/internal/models"
)

func TestRecurringOverviewIsReachableWithASessionToken(t *testing.T) {
	if !skipsStaticBearer("/v1/recurring") {
		t.Fatal("/v1/recurring must accept the app's session token, or it answers 401 in every deployed environment")
	}
}

func TestInferRecurringKindFilesWhatOlderClientsCreate(t *testing.T) {
	cases := []struct {
		tag, purpose string
		instalments  int
		want         string
	}{
		{"EMI", "normal_spend", 0, recurringKindLoan},
		{"Subscription", "normal_spend", 24, recurringKindLoan},
		{"Investment", "", 0, recurringKindInvestment},
		{"General", "investment", 0, recurringKindInvestment},
		{"Subscription", "normal_spend", 0, recurringKindSubscription},
	}
	for _, test := range cases {
		if got := inferRecurringKind(test.tag, test.purpose, test.instalments); got != test.want {
			t.Errorf("inferRecurringKind(%q, %q, %d) = %q, want %q", test.tag, test.purpose, test.instalments, got, test.want)
		}
	}
}

func TestSubscriptionUpdateWithoutLoanFieldsKeepsThem(t *testing.T) {
	stored := models.Subscription{
		Kind: recurringKindLoan, Lender: "HDFC", Principal: rupees(300000), AnnualRatePct: 12, LoanType: "car",
	}
	// What the current app sends when it renames an item: no loan fields.
	input := subscriptionInput{Name: "Car loan EMI", Amount: rupees(9965), NextDueDate: "2026-11-05", TransactionTag: "EMI"}
	input.apply(&stored)

	if stored.Kind != recurringKindLoan || stored.Lender != "HDFC" || stored.Principal != rupees(300000) ||
		stored.AnnualRatePct != 12 || stored.LoanType != "car" {
		t.Fatalf("an older client's edit must not wipe loan details: %+v", stored)
	}
}

func TestNewInvestmentDefaultsItsTagAndPurpose(t *testing.T) {
	kind := "sip"
	var created models.Subscription
	subscriptionInput{Name: "Index fund", Amount: rupees(5000), NextDueDate: "2026-11-10", Kind: &kind}.apply(&created)
	if created.Kind != recurringKindInvestment || created.TransactionTag != "Investment" || created.PurposeType != "investment" {
		t.Fatalf("unexpected defaults: kind=%q tag=%q purpose=%q", created.Kind, created.TransactionTag, created.PurposeType)
	}
}

func TestRecurringInputRejectsImpossibleLoanDetails(t *testing.T) {
	rate, loanType, start := 75.0, "yacht", "05-11-2026"
	fields := subscriptionInput{
		Name: "Loan", Amount: rupees(1000), NextDueDate: "2026-11-05",
		AnnualRatePct: &rate, LoanType: &loanType, StartDate: &start,
	}.validate()
	for _, field := range []string{"annual_rate_pct", "loan_type", "start_date"} {
		if fields[field] == "" {
			t.Errorf("expected a validation error for %s, got %v", field, fields)
		}
	}
}

func TestLoanScheduleFigures(t *testing.T) {
	loan := models.Subscription{
		Kind: recurringKindLoan, Amount: rupees(9965), BillingInterval: subscriptionIntervalMonthly,
		NextDueDate: "2026-11-05", TotalInstalments: 36, InstalmentsPaid: 12,
		Principal: rupees(300000), AnnualRatePct: 12,
	}
	schedule := computeRecurringSchedule(loan, mustDate(t, "2026-10-20"))

	if schedule.RemainingInstalments != 24 || schedule.EndDate != "2028-10-05" || schedule.Completed {
		t.Fatalf("unexpected schedule: %+v", schedule)
	}
	if schedule.OutstandingPrincipal == nil || *schedule.OutstandingPrincipal != models.Money(21166637) {
		t.Fatalf("outstanding = %v, want ₹2,11,666.37", schedule.OutstandingPrincipal)
	}
	if *schedule.TotalInterest != rupees(9965*36-300000) || *schedule.AmountPaid != rupees(9965*12) {
		t.Fatalf("interest %v / paid %v", *schedule.TotalInterest, *schedule.AmountPaid)
	}

	loan.InstalmentsPaid = 36
	loan.LastChargedDate = "2028-10-05"
	done := computeRecurringSchedule(loan, mustDate(t, "2028-10-06"))
	if !done.Completed || *done.OutstandingPrincipal != 0 || done.EndDate != "2028-10-05" {
		t.Fatalf("a finished loan should read as completed: %+v", done)
	}
}

func TestMonthlyEquivalentAcrossIntervals(t *testing.T) {
	cases := map[string]models.Money{
		subscriptionIntervalYearly:    rupees(1200),
		subscriptionIntervalQuarterly: rupees(300),
		subscriptionIntervalMonthly:   rupees(100),
	}
	for interval, amount := range cases {
		if got := monthlyEquivalent(amount, interval); got != rupees(100) {
			t.Errorf("%s: %v, want ₹100", interval, got)
		}
	}
}

func TestInvestmentCountsWhatHasGoneInSinceItStarted(t *testing.T) {
	sip := models.Subscription{
		Kind: recurringKindInvestment, Amount: rupees(5000), BillingInterval: subscriptionIntervalMonthly,
		StartDate: "2026-01-10", LastChargedDate: "2026-10-10",
	}
	schedule := computeRecurringSchedule(sip, mustDate(t, "2026-10-20"))
	if schedule.InvestedSoFar == nil || *schedule.InvestedSoFar != rupees(50000) {
		t.Fatalf("Jan–Oct is 10 instalments of ₹5,000: %v", schedule.InvestedSoFar)
	}
}

func TestListRecurringSummarisesActiveItemsByKind(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	future := time.Now().AddDate(0, 0, 20).Format(apiDateLayout)
	soon := time.Now().AddDate(0, 0, 3).Format(apiDateLayout)
	for _, item := range []models.Subscription{
		{UserID: user.ID, Name: "Car loan", Kind: recurringKindLoan, Amount: rupees(9965), BillingInterval: "monthly", NextDueDate: future, Status: "active", TotalInstalments: 36, InstalmentsPaid: 12},
		{UserID: user.ID, Name: "Index SIP", Kind: recurringKindInvestment, Amount: rupees(5000), BillingInterval: "monthly", NextDueDate: soon, Status: "active"},
		{UserID: user.ID, Name: "Prime", Kind: recurringKindSubscription, Amount: rupees(1499), BillingInterval: "yearly", NextDueDate: future, Status: "active"},
		{UserID: user.ID, Name: "Old gym", Kind: recurringKindSubscription, Amount: rupees(999), BillingInterval: "monthly", NextDueDate: future, Status: "cancelled"},
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
	if len(overview.Items) != 4 || overview.CardEMIs == nil {
		t.Fatalf("all items listed, card EMIs as an array: %+v", overview)
	}
	summary := overview.Summary
	if summary.ActiveCount != 3 {
		t.Fatalf("cancelled items are not committed spend: %+v", summary)
	}
	wantMonthly := rupees(9965) + rupees(5000) + models.Money(12492) // ₹1,499 a year ≈ ₹124.92 a month
	if summary.MonthlyTotal != wantMonthly || summary.ByKind[recurringKindInvestment] != rupees(5000) {
		t.Fatalf("monthly total = %v (by kind %v), want %v", summary.MonthlyTotal, summary.ByKind, wantMonthly)
	}
	if summary.NextDue == nil || summary.NextDue.Name != "Index SIP" {
		t.Fatalf("next due should be the SIP in 3 days: %+v", summary.NextDue)
	}
}
