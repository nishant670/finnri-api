package http

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"finnri/internal/database"
	"finnri/internal/models"
)

func TestCreateCardEMIPlanConvertsOnlyMatchingSourceEntry(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)
	user, token := createPaidBillingTestUserSession(t)
	card := createTestCard(t, user.ID)
	otherCard := createTestCard(t, user.ID)
	source := createCardSpend(t, user.ID, card.ID, "2099-01-31", "67518.00")
	payload := map[string]any{
		"title": "Laptop", "principal": "67518.00", "annual_rate_pct": 0,
		"tenure_months": 6, "purchased_on": "2099-01-31", "source_entry_id": source.ID,
	}

	performJSONRequest[map[string]any](
		t, router, http.MethodPost, fmt.Sprintf("/v1/accounts/%d/emi-plans", otherCard.ID), token,
		payload, http.StatusUnprocessableEntity,
	)
	var sourceCount int64
	database.DB.Model(&models.Entry{}).Where("id = ?", source.ID).Count(&sourceCount)
	if sourceCount != 1 {
		t.Fatal("a source entry on another card was removed")
	}

	created := performJSONRequest[cardEMIPlanResponse](
		t, router, http.MethodPost, fmt.Sprintf("/v1/accounts/%d/emi-plans", card.ID), token,
		payload, http.StatusCreated,
	)
	if !strings.HasPrefix(created.FirstInstallment, "2099-02-28") {
		t.Fatalf("first instalment = %s, want the last valid day 2099-02-28", created.FirstInstallment)
	}
	database.DB.Model(&models.Entry{}).Where("id = ?", source.ID).Count(&sourceCount)
	if sourceCount != 0 {
		t.Fatal("the converted source entry still exists and would double-count spending")
	}
}

// createNoCostEMIPlan is the common case in India: a ₹60,000 purchase split
// into 12 instalments of ₹5,000 with no interest, so the principal component
// of every instalment is the whole instalment.
func createNoCostEMIPlan(t *testing.T, userID uint, card models.Account, firstInstallment string) models.CardEMIPlan {
	t.Helper()

	calculation := buildEMICalculation(emiCalculationInput{
		PrincipalAmount: rupees(60000), TenureMonths: 12, Currency: "INR",
	})
	plan := models.CardEMIPlan{
		UserID: userID, AccountID: card.ID, Title: "iPhone 17",
		Category: defaultCategory, Principal: rupees(60000), TenureMonths: 12,
		MonthlyAmount: calculation.MonthlyEMI, TotalInterest: calculation.TotalInterest,
		Currency: "INR", PurchasedOn: "2026-07-10", FirstInstallment: firstInstallment,
		Status: emiPlanActive,
	}
	if err := database.DB.Create(&plan).Error; err != nil {
		t.Fatal(err)
	}
	installments := buildInstallments(plan, calculation.Schedule)
	if err := database.DB.Create(&installments).Error; err != nil {
		t.Fatal(err)
	}
	return plan
}

func blockedFor(t *testing.T, userID uint, cardID uint) models.Money {
	t.Helper()
	blocked, err := loadBlockedPrincipal(userID, []uint{cardID})
	if err != nil {
		t.Fatal(err)
	}
	return blocked[cardID]
}

// The schedule's principal components must sum to exactly the principal, or
// the limit would never fully come back.
func TestInstallmentPrincipalSumsToThePrincipal(t *testing.T) {
	for _, rate := range []float64{0, 13.5, 24} {
		calculation := buildEMICalculation(emiCalculationInput{
			PrincipalAmount: rupees(60000), AnnualInterestRatePercent: rate,
			TenureMonths: 12, Currency: "INR",
		})
		plan := models.CardEMIPlan{FirstInstallment: "2026-08-10", TenureMonths: 12}
		installments := buildInstallments(plan, calculation.Schedule)

		total := models.Money(0)
		for _, installment := range installments {
			total += installment.PrincipalPart
		}
		if total != rupees(60000) {
			t.Errorf("rate %.1f%%: principal parts sum to %s, want %s", rate, total, rupees(60000))
		}
	}
}

// Only the principal releases limit. On an interest-bearing plan the
// instalment is larger than the principal it repays, so a card frees up less
// headroom each month than the amount it pays.
func TestInterestDoesNotReleaseLimit(t *testing.T) {
	calculation := buildEMICalculation(emiCalculationInput{
		PrincipalAmount: rupees(60000), AnnualInterestRatePercent: 18,
		TenureMonths: 12, Currency: "INR",
	})
	plan := models.CardEMIPlan{FirstInstallment: "2026-08-10", TenureMonths: 12}
	installments := buildInstallments(plan, calculation.Schedule)

	first := installments[0]
	if first.InterestPart <= 0 {
		t.Fatal("an 18% plan should charge interest in month one")
	}
	if first.PrincipalPart >= first.Amount {
		t.Fatalf("principal %s should be less than the instalment %s",
			first.PrincipalPart, first.Amount)
	}
	if first.PrincipalPart+first.InterestPart != first.Amount {
		t.Fatalf("principal %s + interest %s <> instalment %s",
			first.PrincipalPart, first.InterestPart, first.Amount)
	}
}

// Instalment due dates must not skip a month when the anchor day is longer
// than the month it lands in.
func TestInstallmentDatesClampToMonthLength(t *testing.T) {
	calculation := buildEMICalculation(emiCalculationInput{
		PrincipalAmount: rupees(30000), TenureMonths: 6, Currency: "INR",
	})
	plan := models.CardEMIPlan{FirstInstallment: "2025-12-31", TenureMonths: 6}
	installments := buildInstallments(plan, calculation.Schedule)

	want := []string{"2025-12-31", "2026-01-31", "2026-02-28", "2026-03-31", "2026-04-30", "2026-05-31"}
	for index, expected := range want {
		if installments[index].DueDate != expected {
			t.Errorf("instalment %d due %s, want %s", index+1, installments[index].DueDate, expected)
		}
	}
}

// The whole principal blocks the limit the moment a plan is created.
func TestNewPlanBlocksTheFullPrincipal(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	card := createTestCard(t, user.ID)

	createNoCostEMIPlan(t, user.ID, card, "2026-08-10")

	if blocked := blockedFor(t, user.ID, card.ID); blocked != rupees(60000) {
		t.Fatalf("blocked principal = %s, want %s", blocked, rupees(60000))
	}
}

// The month between an instalment being billed and the bill being paid is
// where the limit could be reduced twice. Billing must move the principal out
// of "blocked" at the same moment it enters the statement's total.
func TestBillingMovesPrincipalOutOfBlocked(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	card := createTestCard(t, user.ID)
	createNoCostEMIPlan(t, user.ID, card, "2026-08-10")

	billed, err := syncCardEMIInstallments(user.ID, mustDate(t, "2026-08-10"))
	if err != nil {
		t.Fatal(err)
	}
	if len(billed) != 1 {
		t.Fatalf("billed %d instalments, want 1", len(billed))
	}

	// One instalment's worth of principal has left the block.
	if blocked := blockedFor(t, user.ID, card.ID); blocked != rupees(55000) {
		t.Fatalf("blocked principal = %s, want %s", blocked, rupees(55000))
	}

	// And it is now a real expense on the card, so the statement that bills it
	// can account for it.
	var entry models.Entry
	if err := database.DB.
		Where("user_id = ? AND account_id = ? AND tag = ?", user.ID, card.ID, emiInstallmentTag).
		First(&entry).Error; err != nil {
		t.Fatal(err)
	}
	if entry.Amount != rupees(5000) || entry.Type != "expense" {
		t.Fatalf("instalment entry = %s %s, want 5000 expense", entry.Amount, entry.Type)
	}
	if entry.Date != "2026-08-10" {
		t.Fatalf("instalment entry dated %s, want 2026-08-10", entry.Date)
	}
}

// The job runs every minute; billing must not repeat.
func TestBillingIsIdempotent(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	card := createTestCard(t, user.ID)
	createNoCostEMIPlan(t, user.ID, card, "2026-08-10")

	if _, err := syncCardEMIInstallments(user.ID, mustDate(t, "2026-08-10")); err != nil {
		t.Fatal(err)
	}
	again, err := syncCardEMIInstallments(user.ID, mustDate(t, "2026-08-10"))
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("re-billed %d instalments, want 0", len(again))
	}

	var entries int64
	database.DB.Model(&models.Entry{}).
		Where("user_id = ? AND tag = ?", user.ID, emiInstallmentTag).Count(&entries)
	if entries != 1 {
		t.Fatalf("instalment entries = %d, want 1", entries)
	}
}

// Paying the bill is what actually gives the headroom back.
func TestPayingTheBillReleasesPrincipal(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	card := createTestCard(t, user.ID)
	createNoCostEMIPlan(t, user.ID, card, "2026-07-20")

	if _, err := syncCardEMIInstallments(user.ID, mustDate(t, "2026-07-20")); err != nil {
		t.Fatal(err)
	}
	statement := createTestStatement(t, user.ID, card, "2026-08-05", rupees(5000))

	// Billed but unpaid: principal is inside the bill, not inside the block.
	if blocked := blockedFor(t, user.ID, card.ID); blocked != rupees(55000) {
		t.Fatalf("blocked before payment = %s, want %s", blocked, rupees(55000))
	}

	payment := models.CardStatementPayment{
		UserID: user.ID, StatementID: statement.ID, AccountID: card.ID,
		Amount: rupees(5000), PaidOn: "2026-08-25",
	}
	if err := database.DB.Create(&payment).Error; err != nil {
		t.Fatal(err)
	}
	if err := refreshStatementPaidAmount(&statement); err != nil {
		t.Fatal(err)
	}

	if statement.Status != statementStatusPaid {
		t.Fatalf("statement status = %s, want paid", statement.Status)
	}

	var installment models.CardEMIInstallment
	if err := database.DB.
		Where("user_id = ? AND seq = ?", user.ID, 1).First(&installment).Error; err != nil {
		t.Fatal(err)
	}
	if installment.Status != emiInstallmentPaid {
		t.Fatalf("instalment status = %s, want paid", installment.Status)
	}

	// Still ₹55,000 blocked — the released ₹5,000 has left the plan entirely
	// rather than returning to the block.
	if blocked := blockedFor(t, user.ID, card.ID); blocked != rupees(55000) {
		t.Fatalf("blocked after payment = %s, want %s", blocked, rupees(55000))
	}
}

// Removing a payment un-settles the bill, so the instalments it covered are
// owed again. Without this the limit would stay permanently released.
func TestRemovingAPaymentReblocksPrincipal(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	card := createTestCard(t, user.ID)
	createNoCostEMIPlan(t, user.ID, card, "2026-07-20")

	if _, err := syncCardEMIInstallments(user.ID, mustDate(t, "2026-07-20")); err != nil {
		t.Fatal(err)
	}
	statement := createTestStatement(t, user.ID, card, "2026-08-05", rupees(5000))

	payment := models.CardStatementPayment{
		UserID: user.ID, StatementID: statement.ID, AccountID: card.ID,
		Amount: rupees(5000), PaidOn: "2026-08-25",
	}
	if err := database.DB.Create(&payment).Error; err != nil {
		t.Fatal(err)
	}
	if err := refreshStatementPaidAmount(&statement); err != nil {
		t.Fatal(err)
	}

	// Now take it back.
	if err := database.DB.Delete(&payment).Error; err != nil {
		t.Fatal(err)
	}
	if err := refreshStatementPaidAmount(&statement); err != nil {
		t.Fatal(err)
	}

	var installment models.CardEMIInstallment
	if err := database.DB.
		Where("user_id = ? AND seq = ?", user.ID, 1).First(&installment).Error; err != nil {
		t.Fatal(err)
	}
	if installment.Status != emiInstallmentBilled {
		t.Fatalf("instalment status = %s, want billed once the payment was removed", installment.Status)
	}
}

// An EMI instalment is part of what the bank bills, so it should shrink a
// statement's unaccounted gap rather than widen it.
func TestBilledInstallmentClosesTheReconciliationGap(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	card := createTestCard(t, user.ID)
	createNoCostEMIPlan(t, user.ID, card, "2026-07-20")

	if _, err := syncCardEMIInstallments(user.ID, mustDate(t, "2026-07-20")); err != nil {
		t.Fatal(err)
	}
	createCardSpend(t, user.ID, card.ID, "2026-07-25", "3200")

	// The bank bills the ₹5,000 instalment plus ₹3,200 of ordinary spending.
	statement := createTestStatement(t, user.ID, card, "2026-08-05", rupees(8200))
	result, err := reconcileStatement(&statement)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != reconcileBalanced {
		t.Fatalf("state = %q (gap %s), want balanced — the instalment should be accounted for",
			result.State, result.Gap)
	}
}

// Foreclosure hands back every remaining rupee of principal at once.
func TestForeclosureReleasesRemainingPrincipal(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	card := createTestCard(t, user.ID)
	plan := createNoCostEMIPlan(t, user.ID, card, "2026-08-10")

	if _, err := syncCardEMIInstallments(user.ID, mustDate(t, "2026-08-10")); err != nil {
		t.Fatal(err)
	}
	if err := forecloseEMIPlan(&plan); err != nil {
		t.Fatal(err)
	}

	if blocked := blockedFor(t, user.ID, card.ID); blocked != 0 {
		t.Fatalf("blocked after foreclosure = %s, want 0", blocked)
	}

	// The instalment already billed survives: it is on a statement the user
	// still owes, and erasing it would delete a charge the bank has made.
	var billed int64
	database.DB.Model(&models.CardEMIInstallment{}).
		Where("plan_id = ? AND status = ?", plan.ID, emiInstallmentBilled).Count(&billed)
	if billed != 1 {
		t.Fatalf("billed instalments after foreclosure = %d, want 1", billed)
	}
}

// A closed plan stops holding limit even if rows linger.
func TestForeclosedPlanStopsBlockingLimit(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	card := createTestCard(t, user.ID)
	plan := createNoCostEMIPlan(t, user.ID, card, "2026-09-10")

	if err := database.DB.Model(&plan).Update("status", emiPlanForeclosed).Error; err != nil {
		t.Fatal(err)
	}
	if blocked := blockedFor(t, user.ID, card.ID); blocked != 0 {
		t.Fatalf("a foreclosed plan still blocks %s", blocked)
	}
}

// Progress is what the app renders instead of adding up a schedule itself.
func TestSummariseEMIPlanProgress(t *testing.T) {
	installments := []models.CardEMIInstallment{
		{Seq: 1, DueDate: "2026-08-10", Amount: rupees(5000), PrincipalPart: rupees(5000), Status: emiInstallmentPaid},
		{Seq: 2, DueDate: "2026-09-10", Amount: rupees(5000), PrincipalPart: rupees(5000), Status: emiInstallmentBilled},
		{Seq: 3, DueDate: "2026-10-10", Amount: rupees(5000), PrincipalPart: rupees(5000), Status: emiInstallmentScheduled},
	}
	progress := summariseEMIPlan(installments)

	if progress.InstallmentsPaid != 1 || progress.InstallmentsTotal != 3 {
		t.Fatalf("paid %d of %d, want 1 of 3", progress.InstallmentsPaid, progress.InstallmentsTotal)
	}
	if progress.PrincipalRepaid != rupees(5000) {
		t.Errorf("repaid = %s, want %s", progress.PrincipalRepaid, rupees(5000))
	}
	if progress.PrincipalRemaining != rupees(10000) {
		t.Errorf("remaining = %s, want %s", progress.PrincipalRemaining, rupees(10000))
	}
	// Only the scheduled one blocks; the billed one is inside a statement.
	if progress.BlockedPrincipal != rupees(5000) {
		t.Errorf("blocked = %s, want %s", progress.BlockedPrincipal, rupees(5000))
	}
	if progress.NextDueDate != "2026-09-10" {
		t.Errorf("next due %s, want 2026-09-10", progress.NextDueDate)
	}
}

// Blocked principal has to reach the card's available limit, or none of this
// is visible where it matters.
func TestBlockedPrincipalReducesAvailableLimit(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	card := createTestCard(t, user.ID)
	createNoCostEMIPlan(t, user.ID, card, "2026-09-10")

	contexts, err := loadCardStatementContexts(user.ID, []uint{card.ID})
	if err != nil {
		t.Fatal(err)
	}
	if contexts[card.ID].EMIBlockedPrincipal != rupees(60000) {
		t.Fatalf("context blocked = %s, want %s",
			contexts[card.ID].EMIBlockedPrincipal, rupees(60000))
	}

	totals, err := loadAccountLedgerTotals(user.ID, "2026-08-01", "2026-08-31")
	if err != nil {
		t.Fatal(err)
	}
	summary := summariseAccount(card, totals[card.ID], contexts[card.ID], testToday)

	if summary.Limit == nil || summary.Limit.AvailableLimit == nil {
		t.Fatal("card has no available limit")
	}
	// ₹2,00,000 limit less ₹60,000 blocked, with nothing outstanding.
	if *summary.Limit.AvailableLimit != rupees(140000) {
		t.Fatalf("available limit = %s, want %s",
			*summary.Limit.AvailableLimit, rupees(140000))
	}
}
