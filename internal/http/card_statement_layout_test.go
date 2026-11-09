package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"finnri/internal/database"
	"finnri/internal/models"
)

// indusIndStylePDF builds the layout of the statement that went wrong: a
// transaction table with a Reward Points column and a wrapped category cell,
// and a summary panel down the right edge whose text shares a baseline with
// some of the table's rows.
func indusIndStylePDF() []byte {
	var page strings.Builder
	cell := func(x, y float64, value string) {
		fmt.Fprintf(&page, "BT /F1 8.0 Tf 1 0 0 1 %.2f %.2f Tm (%s) Tj ET\n", x, y, escapePDFText(value))
	}
	cell(450, 700, "Payment Due Date")
	cell(450, 685, "25/11/2026")
	cell(450, 640, "Total Amount Due")
	cell(450, 625, "2,377.00 DR")
	cell(40, 560, "Date")
	cell(100, 560, "Transaction Details")
	cell(260, 560, "Merchant Category")
	cell(340, 560, "Reward Points")
	cell(385, 560, "Amount")
	cell(450, 560, "Statement Period")
	cell(450, 545, "06/10/2026 To 05/11/2026")

	row := func(y float64, date, details, category, points, amount string) {
		cell(40, y, date)
		cell(100, y, details)
		if category == "GROCERY &" {
			cell(260, y+4, "GROCERY &")
			cell(260, y-4, "SUPERMARKET")
		} else {
			cell(260, y, category)
		}
		cell(340, y, points)
		cell(385, y, amount)
	}
	row(520, "23/10/2026", "SWIGGY INSTAMART BENGALURU IN", "GROCERY &", "11", "527.00 DR")
	row(500, "23/10/2026", "SWIGGY INSTAMART PR BANGALORE IN", "GROCERY &", "2", "117.00 DR")
	cell(450, 500, "Statement Date")
	row(480, "28/10/2026", "SWIGGY INSTAMART BENGALURU IN", "GROCERY &", "5", "239.00 DR")
	cell(450, 480, "05/11/2026")
	row(460, "30/10/2026", "SWIGGY INSTAMART BENGALURU IN", "MERCHANDISE", "30", "1,494.00 DR")
	cell(40, 440, "Total")
	cell(340, 440, "48")
	cell(385, 440, "2,377.00")
	return assemblePDF([]string{page.String()})
}

func TestIndusIndLayoutReadsEveryRow(t *testing.T) {
	text, err := readPDFText(indusIndStylePDF())
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + text)
	rows := parseStatementText(text, 2026)
	want := map[string]bool{"527.00": true, "117.00": true, "239.00": true, "1494.00": true}
	for _, row := range rows {
		t.Logf("%s | %s | %s", row.Date, row.Description, row.Amount)
		if !want[row.Amount.String()] {
			t.Errorf("unexpected amount %s in row %q", row.Amount, row.Description)
		}
		delete(want, row.Amount.String())
	}
	if len(want) != 0 {
		t.Errorf("rows not read: %v", want)
	}
}

func TestIndusIndLayoutKeepsDescriptionsClean(t *testing.T) {
	text, err := readPDFText(indusIndStylePDF())
	if err != nil {
		t.Fatal(err)
	}
	rows := parseStatementText(text, 2026)
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4\n%s", len(rows), text)
	}
	if rows[1].Description != "SWIGGY INSTAMART PR BANGALORE IN" {
		t.Errorf("description = %q", rows[1].Description)
	}
	if net := statementRowsNet(rows); net != mustMoney(t, "2377") {
		t.Errorf("rows add up to %s, want 2377", net)
	}
}

func TestIndusIndLayoutFindsTheBillsOwnDates(t *testing.T) {
	text, err := readPDFText(indusIndStylePDF())
	if err != nil {
		t.Fatal(err)
	}
	rows := parseStatementText(text, 2026)
	// What the model got wrong: a row's date, read beside the panel heading.
	summary := statementSummary{StatementDate: "2026-10-23"}
	applyStatementDateChecks(&summary, text, rows)
	if summary.StatementDate != "2026-11-05" {
		t.Errorf("statement date = %q, want 2026-11-05", summary.StatementDate)
	}
	if summary.CycleStart != "2026-10-06" {
		t.Errorf("cycle start = %q, want 2026-10-06", summary.CycleStart)
	}
	if summary.DueDate != "2026-11-25" {
		t.Errorf("due date = %q, want 2026-11-25", summary.DueDate)
	}
}

func TestDateChecksClearAStatementDateTheRowsDisprove(t *testing.T) {
	rows := []statementLine{{Date: "2026-10-30", Description: "SHOP", Amount: mustMoney(t, "10")}}
	summary := statementSummary{StatementDate: "2026-10-23", DueDate: "2026-11-12"}
	applyStatementDateChecks(&summary, "", rows)
	if summary.StatementDate != "" || summary.DueDate != "" {
		t.Errorf("kept a disproved date: %+v", summary)
	}
}

func TestDateChecksDropADateTheStatementDoesNotPrint(t *testing.T) {
	summary := statementSummary{StatementDate: "2026-11-05"}
	applyStatementDateChecks(&summary, "nothing dated here", nil)
	if summary.StatementDate != "" {
		t.Errorf("kept an unprinted date: %q", summary.StatementDate)
	}
}

func TestChooseStatementRowsTrustsTheOneThatAddsUp(t *testing.T) {
	total := mustMoney(t, "2377")
	summary := statementSummary{TotalDue: &total}
	fused := []statementLine{
		{Date: "2026-10-23", Description: "A", Amount: mustMoney(t, "11527"), Type: "expense"},
		{Date: "2026-10-30", Description: "B", Amount: mustMoney(t, "301494"), Type: "expense"},
		{Date: "2026-10-30", Description: "C", Amount: mustMoney(t, "9"), Type: "expense"},
	}
	right := []statementLine{
		{Date: "2026-10-23", Description: "A", Amount: mustMoney(t, "883"), Type: "expense"},
		{Date: "2026-10-30", Description: "B", Amount: mustMoney(t, "1494"), Type: "expense"},
	}
	if got := chooseStatementRows(fused, right, summary); len(got) != 2 {
		t.Errorf("chose %d rows, want the two that add up", len(got))
	}
}

func TestParseRowIgnoresADateAtTheEndOfTheLine(t *testing.T) {
	if line, ok := parseStatementRow("06/10/2026 To 05/11/2026", 2026); ok {
		t.Errorf("read a statement period as a row: %+v", line)
	}
}

func TestParseRowReadsDashedMonthNames(t *testing.T) {
	line, ok := parseStatementRow("10-JUL-26 AMAZON PAY 1,250.00", 2026)
	if !ok || line.Date != "2026-07-10" {
		t.Errorf("got %+v, %v", line, ok)
	}
}

func TestClassifyLineComparesWholeWords(t *testing.T) {
	cases := map[string]string{
		"THIRD WAVE COFFEE BENGALURU": lineKindSpend,
		"AIRTEL RECHARGE":             lineKindSpend,
		"UBER TAXI":                   lineKindSpend,
		"LIC PREMIUM":                 lineKindSpend,
		"APOLLO CHEMIST":              lineKindSpend,
		"ANNUAL FEE":                  lineKindFee,
		"IGST @18%":                   lineKindFee,
		"FUEL SURCHARGE":              lineKindFee,
		"EMI 3/12 AMAZON":             lineKindEMI,
		"FINANCE CHARGES":             lineKindInterest,
	}
	for description, want := range cases {
		line := statementLine{Description: description, Type: "expense"}
		if got := classifyLine(line); got != want {
			t.Errorf("%q = %s, want %s", description, got, want)
		}
	}
}

func TestClassifyCreditsKeepsRefundsApartFromPayments(t *testing.T) {
	cases := map[string]string{
		"PAYMENT RECEIVED - THANK YOU": lineKindPayment,
		"BBPS PAYMENT":                 lineKindPayment,
		"UPI PAYMENT RECEIVED":         lineKindPayment,
		"PAYMENT REVERSAL AMAZON":      lineKindRefund,
		"AMAZON REFUND":                lineKindRefund,
	}
	for description, want := range cases {
		line := statementLine{Description: description, Type: "income"}
		if got := classifyLine(line); got != want {
			t.Errorf("%q = %s, want %s", description, got, want)
		}
	}
}

func TestCycleIgnoresACardDayFarFromTheBill(t *testing.T) {
	start, _ := statementCycle(mustDate(t, "2026-11-05"), 23)
	if got := start.Format(apiDateLayout); got != "2026-10-06" {
		t.Errorf("cycle start = %s, want 2026-10-06", got)
	}
}

// The bill from the report: saved with a row's date, then corrected. Three of
// its four purchases were already logged, so only the ₹117 is unexplained.
func TestCorrectingTheDateLeavesOnlyTheRealGap(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	card := createTestCard(t, user.ID)
	card.StatementDay = 23
	if err := database.DB.Model(&card).Update("statement_day", 23).Error; err != nil {
		t.Fatal(err)
	}
	createCardSpend(t, user.ID, card.ID, "2026-10-23", "527")
	createCardSpend(t, user.ID, card.ID, "2026-10-28", "239")
	createCardSpend(t, user.ID, card.ID, "2026-10-30", "1494")

	statement := createTestStatement(t, user.ID, card, "2026-10-23", rupees(2377))
	before, err := reconcileStatement(&statement)
	if err != nil {
		t.Fatal(err)
	}
	if before.UnitemizedAmount != rupees(1850) {
		t.Fatalf("the reported bug should reproduce: bucket = %s, want 1850", before.UnitemizedAmount)
	}

	router := statementTestRouter(t, user.ID)
	body := `{"statement_date":"2026-11-05","due_date":"2026-11-25","total_due":2377,"minimum_due":100}`
	request := httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/statements/%d", statement.ID), strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}

	var saved cardStatementResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.CycleStart != "2026-10-06" || saved.CycleEnd != "2026-11-05" {
		t.Errorf("cycle = %s to %s, want 2026-10-06 to 2026-11-05", saved.CycleStart, saved.CycleEnd)
	}
	if saved.Reconciliation.UnitemizedAmount != rupees(117) {
		t.Errorf("bucket = %s, want 117", saved.Reconciliation.UnitemizedAmount)
	}

	var bucket models.Entry
	if err := database.DB.First(&bucket, *saved.UnitemizedEntryID).Error; err != nil {
		t.Fatal(err)
	}
	if bucket.Amount != rupees(117) || dateOnly(bucket.Date) != "2026-11-05" {
		t.Errorf("bucket entry = %s on %s", bucket.Amount, bucket.Date)
	}
}

func TestDiffWarnsWhenRowsRunPastTheCycle(t *testing.T) {
	statement := &models.CardStatement{CycleEnd: "2026-10-23"}
	warning := cycleWarningFor(statement, []statementLine{{Date: "2026-10-30"}})
	if warning == nil || warning.LatestRowDate != "2026-10-30" {
		t.Fatalf("warning = %+v", warning)
	}
	if cycleWarningFor(statement, []statementLine{{Date: "2026-10-23"}}) != nil {
		t.Error("warned about a row on the last day of the cycle")
	}
}

func TestDeletingAStatementRemovesWhatItOwns(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	card := createTestCard(t, user.ID)
	createCardSpend(t, user.ID, card.ID, "2026-07-10", "500")
	statement := createTestStatement(t, user.ID, card, "2026-08-05", rupees(1500))
	if _, err := reconcileStatement(&statement); err != nil {
		t.Fatal(err)
	}
	bucketID := *statement.UnitemizedEntryID

	bank := models.Entry{UserID: user.ID, Title: "Credit card payment", Type: "expense", Amount: rupees(100),
		Currency: "INR", Source: "manual", Category: defaultCategory, Date: "2026-08-10"}
	if err := database.DB.Create(&bank).Error; err != nil {
		t.Fatal(err)
	}
	payment := models.CardStatementPayment{UserID: user.ID, StatementID: statement.ID, AccountID: card.ID,
		Amount: rupees(100), PaidOn: "2026-08-10", BankEntryID: &bank.ID}
	if err := database.DB.Create(&payment).Error; err != nil {
		t.Fatal(err)
	}
	reminder := models.CardStatementReminder{UserID: user.ID, StatementID: statement.ID, Kind: "due"}
	if err := database.DB.Create(&reminder).Error; err != nil {
		t.Fatal(err)
	}
	line := statementLine{Date: "2026-07-12", Description: "SHOP", Amount: rupees(200), Type: "expense"}
	imported := buildEntryFromStatementLine(&statement, line)
	if err := database.DB.Create(&imported).Error; err != nil {
		t.Fatal(err)
	}

	router := statementTestRouter(t, user.ID)
	request := httptest.NewRequest(http.MethodDelete,
		fmt.Sprintf("/statements/%d?remove_imported=true", statement.ID), nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}

	count := func(model any, query string, args ...any) int64 {
		var total int64
		if err := database.DB.Model(model).Where(query, args...).Count(&total).Error; err != nil {
			t.Fatal(err)
		}
		return total
	}
	if count(&models.CardStatement{}, "id = ?", statement.ID) != 0 {
		t.Error("statement survived")
	}
	if count(&models.CardStatementPayment{}, "statement_id = ?", statement.ID) != 0 {
		t.Error("payment survived")
	}
	if count(&models.CardStatementReminder{}, "statement_id = ?", statement.ID) != 0 {
		t.Error("reminder survived")
	}
	for name, id := range map[string]uint{"bucket": bucketID, "bank entry": bank.ID, "imported row": imported.ID} {
		if count(&models.Entry{}, "id = ?", id) != 0 {
			t.Errorf("%s survived", name)
		}
	}
	if count(&models.Entry{}, "user_id = ? AND title = ?", user.ID, "Card spend") != 1 {
		t.Error("the user's own transaction was removed")
	}
}

// statementTestRouter serves the statement routes as one signed-in user.
func statementTestRouter(t *testing.T, userID uint) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	server := &Server{}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userID", userID)
		c.Next()
	})
	router.PATCH("/statements/:id", server.updateCardStatement)
	router.DELETE("/statements/:id", server.deleteCardStatement)
	return router
}
