package http

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"finnri/internal/database"
	"finnri/internal/models"
)

// layoutPDF is a page of text cells at fixed positions.
func layoutPDF(cells ...string) []byte {
	var page strings.Builder
	for _, cell := range cells {
		var x, y float64
		var value string
		parts := strings.SplitN(cell, "|", 3)
		fmt.Sscanf(parts[0], "%f", &x)
		fmt.Sscanf(parts[1], "%f", &y)
		value = parts[2]
		fmt.Fprintf(&page, "BT /F1 8.0 Tf 1 0 0 1 %.2f %.2f Tm (%s) Tj ET\n", x, y, escapePDFText(value))
	}
	return assemblePDF([]string{page.String()})
}

// Payment gateways put PAYMENTS in the merchant's name, so a refund from one
// is still a refund.
func TestGatewayMerchantCreditIsARefund(t *testing.T) {
	for _, description := range []string{"RAZORPAY PAYMENTS BANGALORE", "PAYU PAYMENTS PVT LTD", "BILLDESK PAYMENT GURGAON"} {
		line := statementLine{Description: description, Type: "income"}
		if got := classifyLine(line); got != lineKindRefund {
			t.Errorf("%q = %s, want %s", description, got, lineKindRefund)
		}
	}
	for _, description := range []string{"PAYMENT 88213", "PAYMENT - UPI", "CC PAYMENT", "ONLINE PAYMENT", "NEFT 1234"} {
		line := statementLine{Description: description, Type: "income"}
		if got := classifyLine(line); got != lineKindPayment {
			t.Errorf("%q = %s, want %s", description, got, lineKindPayment)
		}
	}
}

// A column before the merchant, like a serial number or a posting date, must
// not become the transaction's name.
func TestRowNameSkipsColumnsThatAreNotText(t *testing.T) {
	cases := map[string]string{
		"05/10/2026   10213455   AMAZON SELLER SERVICES   12   1,250.00": "AMAZON SELLER SERVICES",
		"05/10/2026   06/10/2026   SWIGGY BANGALORE   1,250.00":          "SWIGGY BANGALORE",
		"05/10/2026   SWIGGY BANGALORE   FOOD   1,250.00":                "SWIGGY BANGALORE",
	}
	for raw, want := range cases {
		line, ok := parseStatementRow(raw, 2026)
		if !ok {
			t.Errorf("%q did not parse", raw)
			continue
		}
		if line.Description != want {
			t.Errorf("%q name = %q, want %q", raw, line.Description, want)
		}
		if line.Amount != mustMoney(t, "1250") {
			t.Errorf("%q amount = %s", raw, line.Amount)
		}
	}
}

// A credit marker in its own column still marks the row a credit.
func TestCreditMarkerInItsOwnCell(t *testing.T) {
	line, ok := parseStatementRow("05/10/2026   AMAZON REFUND   1,250.00   CR", 2026)
	if !ok {
		t.Fatal("did not parse")
	}
	if line.Type != "income" {
		t.Errorf("type = %s, want income", line.Type)
	}
}

// Both readings carry the same payment row. Counting it once each would throw
// the expected figure off and hand the choice to the wrong reading.
func TestChooseStatementRowsCountsPaymentsOnce(t *testing.T) {
	total, opening := mustMoney(t, "3000"), mustMoney(t, "5000")
	summary := statementSummary{TotalDue: &total, OpeningBalance: &opening}
	payment := statementLine{Date: "2026-10-05", Description: "PAYMENT RECEIVED", Amount: mustMoney(t, "5000"), Type: "income"}
	wrong := []statementLine{payment, {Date: "2026-10-23", Description: "SHOP", Amount: mustMoney(t, "3527"), Type: "expense"}}
	right := []statementLine{payment, {Date: "2026-10-23", Description: "SHOP", Amount: mustMoney(t, "3000"), Type: "expense"}}
	got := chooseStatementRows(wrong, right, summary)
	if got[1].Amount != mustMoney(t, "3000") {
		t.Errorf("chose the reading with %s", got[1].Amount)
	}
}

// A strip of summary headings across the page is not a panel down its edge.
func TestSummaryStripDoesNotCutTheTable(t *testing.T) {
	data := layoutPDF(
		"40|700|Statement Date", "40|690|05/11/2026",
		"330|700|Total Amount Due", "330|690|527.00",
		"460|700|Minimum Amount Due", "460|690|100.00",
		"40|600|Date", "100|600|Details", "320|600|Points", "480|600|Amount",
		"40|580|23/10/2026", "100|580|SWIGGY BANGALORE", "320|580|11", "480|580|527.00",
	)
	text, err := readPDFText(data)
	if err != nil {
		t.Fatal(err)
	}
	rows := parseStatementText(text, 2026)
	if len(rows) != 1 || rows[0].Amount != mustMoney(t, "527") {
		t.Fatalf("rows = %+v\n%s", rows, text)
	}
}

// Other date ranges get printed on a statement, offers above all.
func TestDateChecksIgnoreAnOffersDates(t *testing.T) {
	text := "Offer valid 01/11/2026 to 30/11/2026\nStatement Date 05/11/2026\n"
	summary := statementSummary{}
	applyStatementDateChecks(&summary, text, []statementLine{{Date: "2026-10-23"}})
	if summary.StatementDate != "2026-11-05" {
		t.Errorf("statement date = %q, want 2026-11-05", summary.StatementDate)
	}
}

func TestDateChecksSkipTheNextStatementDate(t *testing.T) {
	text := "Next Statement Date 05/12/2026\nStatement Date 05/11/2026\n"
	summary := statementSummary{}
	applyStatementDateChecks(&summary, text, []statementLine{{Date: "2026-10-23"}})
	if summary.StatementDate != "2026-11-05" {
		t.Errorf("statement date = %q, want 2026-11-05", summary.StatementDate)
	}
}

// Headings in one row, values in the next: the first date after "Statement
// Date" can be the due date, when the due date's column comes first.
func TestDateChecksHeaderRowWithDueDateFirst(t *testing.T) {
	text := "Payment Due Date   Statement Date\n25/11/2026   05/11/2026\n"
	summary := statementSummary{StatementDate: "2026-11-05"}
	applyStatementDateChecks(&summary, text, []statementLine{{Date: "2026-10-23"}})
	if summary.StatementDate != "2026-11-05" {
		t.Errorf("statement date = %q, want 2026-11-05", summary.StatementDate)
	}
}

// The period is still found when it is labelled, or ends on the statement date.
func TestDateChecksKeepARealPeriod(t *testing.T) {
	for _, text := range []string{
		"Statement Period 06/10/2026 To 05/11/2026\n",
		"06/10/2026 - 05/11/2026\nStatement Date 05/11/2026\n",
	} {
		summary := statementSummary{}
		applyStatementDateChecks(&summary, text, []statementLine{{Date: "2026-10-23"}})
		if summary.StatementDate != "2026-11-05" || summary.CycleStart != "2026-10-06" {
			t.Errorf("%q: date = %q, start = %q", text, summary.StatementDate, summary.CycleStart)
		}
	}
}

func deleteStatementRequest(t *testing.T, userID, statementID uint, query string) {
	t.Helper()
	router := statementTestRouter(t, userID)
	request := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/statements/%d%s", statementID, query), nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestRemovingImportedRowsTakesTheirSplitBills(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	card := createTestCard(t, user.ID)
	statement := createTestStatement(t, user.ID, card, "2026-08-05", rupees(200))
	line := statementLine{Date: "2026-07-12", Description: "SHOP", Amount: rupees(200), Type: "expense"}
	imported := buildEntryFromStatementLine(&statement, line)
	if err := database.DB.Create(&imported).Error; err != nil {
		t.Fatal(err)
	}
	bill := models.SplitBill{UserID: user.ID, EntryID: &imported.ID, Title: "SHOP", TotalAmount: rupees(200), Currency: "INR", Date: "2026-07-12"}
	if err := database.DB.Create(&bill).Error; err != nil {
		t.Fatal(err)
	}

	deleteStatementRequest(t, user.ID, statement.ID, "?remove_imported=true")

	var bills int64
	database.DB.Model(&models.SplitBill{}).Where("id = ?", bill.ID).Count(&bills)
	if bills != 0 {
		t.Error("the split bill outlived its transaction")
	}
}

func TestDeletingABillClearsItsReminders(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	card := createTestCard(t, user.ID)
	statement := createTestStatement(t, user.ID, card, "2026-08-05", rupees(200))
	notification := models.Notification{UserID: user.ID, Type: "card_statement.due_soon", Title: "Bill due", Body: "Soon"}
	if err := database.DB.Create(&notification).Error; err != nil {
		t.Fatal(err)
	}
	reminder := models.CardStatementReminder{UserID: user.ID, StatementID: statement.ID, Kind: "due_soon", NotificationID: &notification.ID}
	if err := database.DB.Create(&reminder).Error; err != nil {
		t.Fatal(err)
	}

	deleteStatementRequest(t, user.ID, statement.ID, "")

	var stored models.Notification
	if err := database.DB.First(&stored, notification.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.ResolvedAt == nil {
		t.Error("the reminder is still asking about a deleted bill")
	}
}

func TestDeletingAPaidBillOwesItsUnlinkedInstalmentsAgain(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	card := createTestCard(t, user.ID)
	statement := createTestStatement(t, user.ID, card, "2026-08-05", rupees(200))
	if err := database.DB.Model(&statement).Update("status", statementStatusPaid).Error; err != nil {
		t.Fatal(err)
	}
	paidInstalment := func(due string) models.CardEMIInstallment {
		installment := models.CardEMIInstallment{PlanID: 1, UserID: user.ID, AccountID: card.ID, Seq: 1, DueDate: due,
			Amount: rupees(100), PrincipalPart: rupees(100), Status: emiInstallmentPaid}
		if err := database.DB.Create(&installment).Error; err != nil {
			t.Fatal(err)
		}
		return installment
	}
	inCycle := paidInstalment(statement.CycleEnd)
	older := paidInstalment("2026-03-05")

	deleteStatementRequest(t, user.ID, statement.ID, "")

	var stored models.CardEMIInstallment
	database.DB.First(&stored, inCycle.ID)
	if stored.Status != emiInstallmentBilled {
		t.Errorf("in-cycle instalment = %s, want billed", stored.Status)
	}
	stored = models.CardEMIInstallment{}
	database.DB.First(&stored, older.ID)
	if stored.Status != emiInstallmentPaid {
		t.Errorf("older instalment = %s, want paid", stored.Status)
	}
}
