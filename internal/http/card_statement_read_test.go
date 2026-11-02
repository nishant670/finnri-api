package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"finnri/internal/ai"
	"finnri/internal/config"
	"finnri/internal/database"
	"finnri/internal/models"
)

type fixtureStatementReader struct {
	fixtureParser
	summary []byte
	images  []byte
}

func (p fixtureStatementReader) ReadStatementText(context.Context, string, string) ([]byte, ai.Usage, error) {
	return p.summary, ai.Usage{}, nil
}

func (p fixtureStatementReader) ReadStatementImages(context.Context, []ai.StatementImage, string) ([]byte, ai.Usage, error) {
	return p.images, ai.Usage{}, nil
}

func TestDecodeStatementSummaryKeepsOnlyPrintedAmounts(t *testing.T) {
	text := "Statement Date 05/09/2026 Payment Due Date 25/09/2026\nTotal Amount Due Rs 12,400.00 Minimum Due 620.00\nCredit Limit 1,50,000"
	raw := []byte(`{"summary":{
		"statement_date":"2026-09-05","due_date":"2026-09-25",
		"total_due":12400,"minimum_due":620,"credit_limit":150000,
		"annual_fee":999,"renewal_month":"march","card_last4":"4321","issuer":" HDFC   Bank "
	}}`)

	summary, err := decodeStatementSummary(raw, text)
	if err != nil {
		t.Fatal(err)
	}
	if summary.TotalDue == nil || *summary.TotalDue != rupees(12400) || summary.MinimumDue == nil {
		t.Fatalf("printed amounts should survive: %+v", summary)
	}
	if summary.CreditLimit == nil || *summary.CreditLimit != rupees(150000) {
		t.Fatalf("an Indian-grouped printed limit should survive: %+v", summary.CreditLimit)
	}
	if summary.AnnualFee != nil {
		t.Fatalf("an annual fee that is not printed must be dropped, got %v", *summary.AnnualFee)
	}
	if summary.RenewalMonth != "March" || summary.CardLast4 != "4321" || summary.Issuer != "HDFC Bank" {
		t.Fatalf("unexpected normalisation: %+v", summary)
	}
}

func TestDecodeStatementSummaryMatchesWholeFiguresOnly(t *testing.T) {
	text := "Credit Limit 1,50,000 Total Due 2,345.50"
	raw := []byte(`{"summary":{"annual_fee":500,"total_due":2345.5,"credit_limit":150000}}`)
	summary, err := decodeStatementSummary(raw, text)
	if err != nil {
		t.Fatal(err)
	}
	if summary.AnnualFee != nil {
		t.Fatal("500 is inside 1,50,000 but is not a printed figure")
	}
	if summary.TotalDue == nil || *summary.TotalDue != models.Money(234550) || summary.CreditLimit == nil {
		t.Fatalf("whole printed figures should match: %+v", summary)
	}
}

func TestDecodeStatementSummaryRejectsImpossibleDates(t *testing.T) {
	raw := []byte(`{"summary":{"statement_date":"2026-09-05","due_date":"2026-09-01","total_due":100,"minimum_due":500}}`)
	summary, err := decodeStatementSummary(raw, "")
	if err != nil {
		t.Fatal(err)
	}
	if summary.StatementDate != "2026-09-05" || summary.DueDate != "" {
		t.Fatalf("a due date before the statement date must be dropped: %+v", summary)
	}
	if summary.MinimumDue != nil {
		t.Fatalf("a minimum above the total must be dropped: %+v", summary)
	}
}

func TestCompareCardWithStatementProposesOnlyDifferences(t *testing.T) {
	account := models.Account{StatementDay: 5, DueDay: 20, CreditLimit: rupees(100000), FeeMonth: "March"}
	limit, fee := rupees(150000), rupees(500)
	summary := statementSummary{
		StatementDate: "2026-09-05", DueDate: "2026-09-25", CreditLimit: &limit,
		AnnualFee: &fee, RenewalMonth: "March", CardLast4: "4321",
	}

	updates, warnings := compareCardWithStatement(account, summary)

	fields := []string{}
	for _, update := range updates {
		fields = append(fields, update.Field)
	}
	if strings.Join(fields, ",") != "due_day,credit_limit,annual_fee,last4" || len(warnings) != 0 {
		t.Fatalf("updates = %v, warnings = %v", fields, warnings)
	}
}

func TestCompareCardWithStatementWarnsAboutADifferentCard(t *testing.T) {
	limit := rupees(150000)
	updates, warnings := compareCardWithStatement(
		models.Account{Last4: "9999", CreditLimit: rupees(1)},
		statementSummary{CardLast4: "4321", CreditLimit: &limit},
	)
	if len(updates) != 0 || len(warnings) != 1 || !strings.Contains(warnings[0], "ending 4321") {
		t.Fatalf("a different card must warn and copy nothing: %v / %v", updates, warnings)
	}
}

func newStatementReadContext(t *testing.T, build func(*multipart.Writer)) (*gin.Context, *httptest.ResponseRecorder, models.Account) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	build(writer)
	_ = writer.Close()
	request := httptest.NewRequest("POST", "/v1/accounts/1/statements/read", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Request = request
	user := attachParseCreditUser(t, ctx)
	card := models.Account{UserID: user.ID, Type: "credit_card", Name: "Regalia", Last4: "4321", StatementDay: 5, DueDay: 20}
	if err := database.DB.Create(&card).Error; err != nil {
		t.Fatal(err)
	}
	ctx.Params = gin.Params{{Key: "id", Value: strconv.FormatUint(uint64(card.ID), 10)}}
	return ctx, response, card
}

func TestReadStatementPDFReturnsRowsAndAVerifiedSummary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "statement.pdf")
	if err := writeMinimalPDF(t, path); err != nil {
		t.Skipf("could not build a test PDF: %v", err)
	}
	pdf, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		cfg: &config.Config{ReqTimeoutSec: 2, TZDefault: "Asia/Kolkata"},
		parser: fixtureStatementReader{summary: []byte(`{"summary":{
			"statement_date":"2026-07-15","due_date":"2026-08-04","total_due":480,"credit_limit":200000
		}}`)},
	}
	ctx, response, _ := newStatementReadContext(t, func(writer *multipart.Writer) {
		part, _ := writer.CreateFormFile("file", "statement.pdf")
		_, _ = part.Write(pdf)
	})

	server.readCardStatement(ctx)

	if response.Code != 200 {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var read statementReadResponse
	if err := json.Unmarshal(response.Body.Bytes(), &read); err != nil {
		t.Fatal(err)
	}
	if read.Source != "pdf" || len(read.Lines) != 1 || read.Lines[0].Amount != rupees(480) {
		t.Fatalf("rows should still be read locally: %+v", read)
	}
	if read.Summary.TotalDue == nil || *read.Summary.TotalDue != rupees(480) {
		t.Fatalf("printed total should prefill: %+v", read.Summary)
	}
	if read.Summary.CreditLimit != nil {
		t.Fatalf("a limit that is not in the PDF text must be dropped: %v", *read.Summary.CreditLimit)
	}
	if read.CreditsCharged == nil || *read.CreditsCharged != 50 {
		t.Fatalf("a PDF read is an AI read and costs 50 credits, got %v", read.CreditsCharged)
	}
	found := false
	for _, update := range read.CardUpdates {
		if update.Field == "statement_day" {
			found = true
		}
	}
	if !found {
		t.Fatalf("statement day 15 vs card's 5 should be proposed: %+v", read.CardUpdates)
	}
}

func TestReadStatementPDFStillWorksWithoutAI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "statement.pdf")
	if err := writeMinimalPDF(t, path); err != nil {
		t.Skipf("could not build a test PDF: %v", err)
	}
	pdf, _ := os.ReadFile(path)
	server := &Server{
		cfg:    &config.Config{ReqTimeoutSec: 2, TZDefault: "Asia/Kolkata", AIParseDisabled: true},
		parser: fixtureStatementReader{summary: []byte(`{"summary":{"total_due":480}}`)},
	}
	ctx, response, _ := newStatementReadContext(t, func(writer *multipart.Writer) {
		part, _ := writer.CreateFormFile("file", "statement.pdf")
		_, _ = part.Write(pdf)
	})

	server.readCardStatement(ctx)

	if response.Code != 200 || !strings.Contains(response.Body.String(), "SWIGGY") ||
		strings.Contains(response.Body.String(), `"total_due"`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

type countingStatementReader struct {
	fixtureStatementReader
	calls *int
}

func (p countingStatementReader) ReadStatementImages(ctx context.Context, images []ai.StatementImage, today string) ([]byte, ai.Usage, error) {
	*p.calls++
	return p.fixtureStatementReader.ReadStatementImages(ctx, images, today)
}

func TestReadStatementScreenshotsNeedsAPlanBeforeAnyAICall(t *testing.T) {
	calls := 0
	server := &Server{
		cfg:    &config.Config{ReqTimeoutSec: 2, TZDefault: "Asia/Kolkata"},
		parser: countingStatementReader{calls: &calls},
	}
	ctx, response, _ := newStatementReadContext(t, func(writer *multipart.Writer) {
		part, _ := writer.CreateFormFile("images", "page1.png")
		_, _ = part.Write(append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...))
	})

	server.readCardStatement(ctx)

	if response.Code != 402 || !strings.Contains(response.Body.String(), "feature_locked") {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if calls != 0 {
		t.Fatal("a locked read must not reach the model")
	}
}

func TestCompareCardOffersAStatementDayToACardWithoutOne(t *testing.T) {
	updates, _ := compareCardWithStatement(models.Account{}, statementSummary{StatementDate: "2026-10-01"})
	if len(updates) != 1 || updates[0].Field != "statement_day" || updates[0].Proposed != 1 {
		t.Fatalf("a card with no statement day must be offered the 1st: %+v", updates)
	}
	// And a card that bills on the 31st is not told it moved when November
	// ends on the 30th.
	none, _ := compareCardWithStatement(models.Account{StatementDay: 31}, statementSummary{StatementDate: "2026-11-30"})
	if len(none) != 0 {
		t.Fatalf("the clamped anchor is not a move: %+v", none)
	}
}

// The Scapia case: the PDF's text layer splits each row across lines and
// prints dates without a year, so the local parser finds nothing. The model
// reads the rows, every one of them must be printed in the text, and the read
// is charged once.
func TestReadStatementPDFReadsRowsTheLocalParserCannot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "statement.pdf")
	content := "BT /F1 10 Tf 72 720 Td (12 Oct) Tj 0 -14 Td (AMAZON PAY INDIA) Tj 0 -14 Td (1,250.00) Tj " +
		"0 -14 Td (14 Oct) Tj 0 -14 Td (UBER TRIP) Tj 0 -14 Td (340.50) Tj " +
		"0 -14 Td (Total Amount Due 1,590.50) Tj ET"
	if err := writeTextPDF(t, path, content); err != nil {
		t.Skipf("could not build a test PDF: %v", err)
	}
	pdf, _ := os.ReadFile(path)
	server := &Server{
		cfg: &config.Config{ReqTimeoutSec: 2, TZDefault: "Asia/Kolkata"},
		parser: fixtureStatementReader{summary: []byte(`{"summary":{"statement_date":"2026-10-25","total_due":1590.5},
			"lines":[
				{"date":"2026-10-12","description":"AMAZON PAY INDIA","amount":1250,"type":"expense"},
				{"date":"2026-10-14","description":"UBER TRIP","amount":340.5,"type":"expense"},
				{"date":"2026-10-15","description":"INVENTED","amount":999,"type":"expense"}
			]}`)},
	}
	ctx, response, _ := newStatementReadContext(t, func(writer *multipart.Writer) {
		part, _ := writer.CreateFormFile("file", "statement.pdf")
		_, _ = part.Write(pdf)
	})

	server.readCardStatement(ctx)

	if response.Code != 200 {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var read statementReadResponse
	if err := json.Unmarshal(response.Body.Bytes(), &read); err != nil {
		t.Fatal(err)
	}
	if len(read.Lines) != 2 || read.Lines[0].Description != "AMAZON PAY INDIA" || read.Lines[1].Amount != models.Money(34050) {
		t.Fatalf("the two printed rows, and not the invented one, should come back: %+v", read.Lines)
	}
	if read.Summary.TotalDue == nil || *read.Summary.TotalDue != models.Money(159050) {
		t.Fatalf("summary should still prefill: %+v", read.Summary)
	}
	if read.CreditsCharged == nil || *read.CreditsCharged != 50 {
		t.Fatalf("charged %v, want 50", read.CreditsCharged)
	}
}

type failingStatementReader struct{ fixtureStatementReader }

func (failingStatementReader) ReadStatementText(context.Context, string, string) ([]byte, ai.Usage, error) {
	return nil, ai.Usage{}, errors.New("provider down")
}

func TestReadStatementPDFFailedReadIsFree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "statement.pdf")
	if err := writeMinimalPDF(t, path); err != nil {
		t.Skipf("could not build a test PDF: %v", err)
	}
	pdf, _ := os.ReadFile(path)
	server := &Server{
		cfg:    &config.Config{ReqTimeoutSec: 2, TZDefault: "Asia/Kolkata"},
		parser: failingStatementReader{},
	}
	ctx, response, _ := newStatementReadContext(t, func(writer *multipart.Writer) {
		part, _ := writer.CreateFormFile("file", "statement.pdf")
		_, _ = part.Write(pdf)
	})

	server.readCardStatement(ctx)

	var read statementReadResponse
	_ = json.Unmarshal(response.Body.Bytes(), &read)
	if response.Code != 200 || len(read.Lines) != 1 || read.CreditsCharged != nil {
		t.Fatalf("a failed model call should fall back to the local rows, free: %d %s", response.Code, response.Body.String())
	}
	var event models.AIUsageEvent
	if err := database.DB.Order("id DESC").First(&event).Error; err != nil {
		t.Fatal(err)
	}
	if event.FinalCredits != 0 || event.Status != "failed_after_provider" {
		t.Fatalf("the failed read must be recorded and refunded: %+v", event)
	}
}

// Checking a saved bill against its PDF is the same paid read: the Scapia
// file that the parser alone could not read must work here too, rather than
// failing again with "no transactions found".
func TestUploadStatementPDFUsesTheAIRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "statement.pdf")
	content := "BT /F1 10 Tf 72 720 Td (12 Oct) Tj 0 -14 Td (AMAZON PAY INDIA) Tj 0 -14 Td (1,250.00) Tj ET"
	if err := writeTextPDF(t, path, content); err != nil {
		t.Skipf("could not build a test PDF: %v", err)
	}
	pdf, _ := os.ReadFile(path)
	server := &Server{
		cfg: &config.Config{ReqTimeoutSec: 2, TZDefault: "Asia/Kolkata"},
		parser: fixtureStatementReader{summary: []byte(`{"summary":{},
			"lines":[{"date":"2026-10-12","description":"AMAZON PAY INDIA","amount":1250,"type":"expense"}]}`)},
	}
	ctx, response, card := newStatementReadContext(t, func(writer *multipart.Writer) {
		part, _ := writer.CreateFormFile("file", "statement.pdf")
		_, _ = part.Write(pdf)
	})
	statement := models.CardStatement{
		UserID: card.UserID, AccountID: card.ID, CycleStart: "2026-09-26", CycleEnd: "2026-10-25",
		StatementDate: "2026-10-25", DueDate: "2026-11-12", TotalDue: rupees(1250), Status: "unpaid",
	}
	if err := database.DB.Create(&statement).Error; err != nil {
		t.Fatal(err)
	}
	ctx.Params = gin.Params{{Key: "id", Value: strconv.FormatUint(uint64(statement.ID), 10)}}

	server.uploadCardStatementPDF(ctx)

	if response.Code != 200 {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var diff statementDiff
	if err := json.Unmarshal(response.Body.Bytes(), &diff); err != nil {
		t.Fatal(err)
	}
	if len(diff.Missing) != 1 || diff.Missing[0].Description != "AMAZON PAY INDIA" {
		t.Fatalf("the AI-read row should be offered as missing: %+v", diff)
	}
	if diff.CreditsCharged == nil || *diff.CreditsCharged != 50 {
		t.Fatalf("charged %v, want 50", diff.CreditsCharged)
	}
}

// A row the model dated in the wrong year is moved next to the statement date
// before it reaches the check, where a wrong year would make it "missing".
func TestReadStatementPDFCorrectsWrongYears(t *testing.T) {
	path := filepath.Join(t.TempDir(), "statement.pdf")
	content := "BT /F1 10 Tf 72 720 Td (12 Oct) Tj 0 -14 Td (AMAZON PAY INDIA) Tj 0 -14 Td (1,250.00) Tj ET"
	if err := writeTextPDF(t, path, content); err != nil {
		t.Skipf("could not build a test PDF: %v", err)
	}
	pdf, _ := os.ReadFile(path)
	server := &Server{
		cfg: &config.Config{ReqTimeoutSec: 2, TZDefault: "Asia/Kolkata"},
		parser: fixtureStatementReader{summary: []byte(`{"summary":{"statement_date":"2026-10-25"},
			"lines":[{"date":"2023-10-12","description":"AMAZON PAY INDIA","amount":1250,"type":"expense"}]}`)},
	}
	ctx, response, _ := newStatementReadContext(t, func(writer *multipart.Writer) {
		part, _ := writer.CreateFormFile("file", "statement.pdf")
		_, _ = part.Write(pdf)
	})

	server.readCardStatement(ctx)

	var read statementReadResponse
	_ = json.Unmarshal(response.Body.Bytes(), &read)
	if response.Code != 200 || len(read.Lines) != 1 || read.Lines[0].Date != "2026-10-12" {
		t.Fatalf("the row should be dated 2026-10-12: %d %s", response.Code, response.Body.String())
	}
}
