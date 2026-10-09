package http

import (
	"bytes"
	"context"
	"encoding/json"
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

func (p fixtureStatementReader) ExtractStatementSummary(context.Context, string, string) ([]byte, ai.Usage, error) {
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
	if read.CreditsCharged != nil {
		t.Fatal("a PDF read must stay free")
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
