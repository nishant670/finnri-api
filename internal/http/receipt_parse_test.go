package http

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/xeipuuv/gojsonschema"

	"finnri/internal/ai"
	"finnri/internal/billing"
	"finnri/internal/config"
	"finnri/internal/database"
	"finnri/internal/models"
)

type fixtureReceiptParser struct {
	fixtureParser
	receipt []byte
}

func (p fixtureReceiptParser) ParseReceiptImage(context.Context, ai.StatementImage, string) ([]byte, ai.Usage, error) {
	return p.receipt, ai.Usage{}, nil
}

// A real PNG signature is enough for http.DetectContentType.
var receiptTestPNG = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)

func newReceiptServer(t *testing.T, reply string) *Server {
	t.Helper()
	schema, err := gojsonschema.NewSchema(
		gojsonschema.NewReferenceLoader("file://../../schemas/expense_entry.schema.json"),
	)
	if err != nil {
		t.Fatal(err)
	}
	return &Server{
		cfg:       &config.Config{ReqTimeoutSec: 2, TZDefault: "Asia/Kolkata"},
		validator: schema,
		parser:    fixtureReceiptParser{receipt: []byte(reply)},
	}
}

func newReceiptContext(t *testing.T, image []byte) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("image", "receipt.png")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(image)
	_ = writer.WriteField("tz", "Asia/Kolkata")
	_ = writer.Close()
	request := httptest.NewRequest("POST", "/v1/parse/receipt", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Request = request
	attachParseCreditUser(t, ctx)
	return ctx, response
}

func TestParseReceiptReturnsCaptureDraft(t *testing.T) {
	server := newReceiptServer(t, "```json\n"+`{
		"summary":"Domino's bill, Rs 548, 12 Sep","intent":"question","query":{"metric":"spend_total"},
		"type":"expense","title":"Domino's pizza","amount":548,"currency":"INR",
		"mode":"UPI","category":"Food & Drinks","merchant":"Domino's","date":"2026-09-12",
		"note":"2 pizzas, 1 drink","confidence":{"amount":0.95}
	}`+"\n```")
	ctx, response := newReceiptContext(t, receiptTestPNG)

	server.handleParseReceipt(ctx)

	if response.Code != 200 {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var draft map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &draft); err != nil {
		t.Fatal(err)
	}
	if draft["stage"] != "draft" || draft["amount"] != float64(548) || draft["merchant"] != "Domino's" {
		t.Fatalf("unexpected draft %v", draft)
	}
	if draft["source_text"] != "Domino's bill, Rs 548, 12 Sep" {
		t.Fatalf("summary should become source_text, got %v", draft["source_text"])
	}
	if _, leaked := draft["intent"]; leaked {
		t.Fatalf("receipt draft must not carry a question intent: %v", draft)
	}
	if draft["credits_charged"] != float64(25) {
		t.Fatalf("credits_charged = %v", draft["credits_charged"])
	}
}

func TestParseReceiptRejectsUnreadableTotal(t *testing.T) {
	server := newReceiptServer(t, `{"summary":"A photo of a cat","amount":null,"clarifications":["No bill found"]}`)
	ctx, response := newReceiptContext(t, receiptTestPNG)

	server.handleParseReceipt(ctx)

	if response.Code != 422 || !strings.Contains(response.Body.String(), "receipt_unreadable") {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestParseReceiptRejectsNonImage(t *testing.T) {
	server := newReceiptServer(t, `{}`)
	ctx, response := newReceiptContext(t, []byte("%PDF-1.7 not an image"))

	server.handleParseReceipt(ctx)

	if response.Code != 415 || !strings.Contains(response.Body.String(), "unsupported_receipt_image") {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestParseReceiptForeignCurrencyStaysSaveable(t *testing.T) {
	server := newReceiptServer(t, `{
		"summary":"Starbucks receipt, USD 12.50","type":"expense","title":"Coffee",
		"amount":12.5,"currency":"usd","category":"Food & Drinks","merchant":"Starbucks",
		"date":"2026-09-12","confidence":{"amount":0.9}
	}`)
	ctx, response := newReceiptContext(t, receiptTestPNG)

	server.handleParseReceipt(ctx)

	if response.Code != 200 {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var draft map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &draft); err != nil {
		t.Fatal(err)
	}
	if draft["currency"] != "INR" {
		t.Fatalf("entries are INR-only, got currency %v", draft["currency"])
	}
	if confirm, _ := draft["needs_confirmation"].(map[string]any); confirm["amount"] != true {
		t.Fatalf("a foreign amount must be flagged for confirmation: %v", draft["needs_confirmation"])
	}
	if !strings.Contains(response.Body.String(), "This bill is in USD") {
		t.Fatalf("expected a clarification naming the currency: %s", response.Body.String())
	}
}

func receiptDraftFromResponse(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if response.Code != 200 {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var draft map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &draft); err != nil {
		t.Fatal(err)
	}
	return draft
}

func TestParseReceiptFlagsAnAmountFromAPhotoWithUnrelatedPapers(t *testing.T) {
	// The reported case: a petrol bill with an unrelated HDFC card slip on top.
	server := newReceiptServer(t, `{
		"summary":"Bharat Petroleum bill, Rs 3301, 9 Oct","documents_in_photo":2,"other_papers":"unrelated",
		"other_papers_detail":[{"shop":"Anand Medicos","amount":7647.6,"date":"2026-10-05"}],
		"total_label":"Amount (Rs)","total_as_printed":"3301.00",
		"type":"expense","title":"Petrol","amount":3301,"currency":"INR","category":"Transport",
		"mode":"Credit Card","account_hint":"HDFC","card_network":"Visa",
		"merchant":"Bharat Petroleum","date":"2026-10-09","confidence":{"amount":0.98}
	}`)
	ctx, response := newReceiptContext(t, receiptTestPNG)

	server.handleParseReceipt(ctx)

	draft := receiptDraftFromResponse(t, response)
	if confirm, _ := draft["needs_confirmation"].(map[string]any); confirm["amount"] != true {
		t.Fatalf("amount should be flagged: %v", draft["needs_confirmation"])
	}
	if confidence, _ := draft["confidence"].(map[string]any); confidence["amount"].(float64) > 0.5 {
		t.Fatalf("confidence should be capped, got %v", confidence["amount"])
	}
	if !strings.Contains(response.Body.String(), "another receipt (Anand Medicos, ₹7,647.60)") {
		t.Fatalf("expected a clarification naming the other receipt: %s", response.Body.String())
	}
	// Which paper named HDFC cannot be told, so no account is offered from it.
	if _, kept := draft["account_hint"]; kept {
		t.Fatalf("account_hint should be dropped with an unrelated paper in the photo: %v", draft["account_hint"])
	}
	if _, kept := draft["card_network"]; kept {
		t.Fatalf("card_network should be dropped too: %v", draft["card_network"])
	}
	for _, key := range []string{"documents_in_photo", "other_papers", "other_papers_detail", "total_label", "total_as_printed"} {
		if _, leaked := draft[key]; leaked {
			t.Fatalf("%s should be stripped from the draft", key)
		}
	}
}

func TestParseReceiptTrustsAMatchingCardSlip(t *testing.T) {
	server := newReceiptServer(t, `{
		"summary":"Bharat Petroleum bill, Rs 3301, 9 Oct","documents_in_photo":2,"other_papers":"same_purchase",
		"other_papers_detail":[{"shop":"HDFC Bank","amount":3301,"date":"2026-10-09"}],
		"total_label":"Amount (Rs)","total_as_printed":"Rs 3,301.00",
		"type":"expense","title":"Petrol","amount":3301,"currency":"INR","mode":"Credit Card","account_hint":"HDFC",
		"merchant":"Bharat Petroleum","date":"2026-10-09","confidence":{"amount":0.98}
	}`)
	ctx, response := newReceiptContext(t, receiptTestPNG)

	server.handleParseReceipt(ctx)

	draft := receiptDraftFromResponse(t, response)
	if confirm, _ := draft["needs_confirmation"].(map[string]any); confirm["amount"] == true {
		t.Fatalf("a bill with its own card slip should not be flagged: %v", draft)
	}
	if draft["account_hint"] != "HDFC" {
		t.Fatalf("the bill's own card slip should keep its bank, got %v", draft["account_hint"])
	}
}

func TestParseReceiptDoesNotTakeSamePurchaseOnTrustWhenTotalsDiffer(t *testing.T) {
	server := newReceiptServer(t, `{
		"summary":"Bharat Petroleum bill, Rs 3301","documents_in_photo":2,"other_papers":"same_purchase",
		"other_papers_detail":[{"shop":null,"amount":7647.6,"date":null}],
		"type":"expense","title":"Petrol","amount":3301,"currency":"INR","account_hint":"HDFC",
		"merchant":"Bharat Petroleum","date":"2026-10-09","confidence":{"amount":0.98}
	}`)
	ctx, response := newReceiptContext(t, receiptTestPNG)

	server.handleParseReceipt(ctx)

	draft := receiptDraftFromResponse(t, response)
	if confirm, _ := draft["needs_confirmation"].(map[string]any); confirm["amount"] != true {
		t.Fatalf("a second paper with a different total is not the same purchase: %v", draft)
	}
	if !strings.Contains(response.Body.String(), "another receipt (₹7,647.60)") {
		t.Fatalf("expected the other total in the clarification: %s", response.Body.String())
	}
}

func TestParseReceiptFlagsAnAmountThatDisagreesWithThePrintedTotal(t *testing.T) {
	server := newReceiptServer(t, `{
		"summary":"PhonePe payment, Rs 450","documents_in_photo":1,"other_papers":"none",
		"total_label":"Paid","total_as_printed":"₹450",
		"type":"expense","title":"Groceries","amount":1450,"currency":"INR","mode":"UPI",
		"merchant":"Ramesh Kirana","date":"2026-10-11","confidence":{"amount":0.9}
	}`)
	ctx, response := newReceiptContext(t, receiptTestPNG)

	server.handleParseReceipt(ctx)

	draft := receiptDraftFromResponse(t, response)
	if confirm, _ := draft["needs_confirmation"].(map[string]any); confirm["amount"] != true {
		t.Fatalf("1450 against a printed ₹450 should be flagged: %v", draft["needs_confirmation"])
	}
}

func TestParseReceiptUnreadablePhotoCostsNothing(t *testing.T) {
	server := newReceiptServer(t, `{"summary":"A photo of a cat","amount":null,"clarifications":["No bill found"]}`)
	ctx, response := newReceiptContext(t, receiptTestPNG)
	user, _ := ctx.Get("user")
	creditService := billing.NewCreditService(database.DB)
	subject := billing.SubjectForUser(user.(*models.User).ID)
	before, err := creditService.CheckAllowance(subject, ai.ActionTransactionParseReceipt)
	if err != nil {
		t.Fatal(err)
	}

	server.handleParseReceipt(ctx)

	if response.Code != 422 || !strings.Contains(response.Body.String(), `"credits_charged":0`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	after, err := creditService.CheckAllowance(subject, ai.ActionTransactionParseReceipt)
	if err != nil {
		t.Fatal(err)
	}
	if after.AvailableCredits != before.AvailableCredits || after.DailyRemaining != before.DailyRemaining {
		t.Fatalf("credits moved: before %+v, after %+v", before, after)
	}
}

func TestParsePrintedAmount(t *testing.T) {
	cases := map[string]float64{
		"3301.00": 3301, "Rs 3,301.00": 3301, "₹450": 450, "1,26,560.00": 126560,
		"INR 548.5": 548.5, "Total (2 items) 548": 548,
	}
	for printed, want := range cases {
		got, ok := parsePrintedAmount(printed)
		if !ok || got != want {
			t.Errorf("parsePrintedAmount(%q) = %v, %v; want %v", printed, got, ok, want)
		}
	}
	for _, printed := range []string{"", "Rs", "0.00"} {
		if _, ok := parsePrintedAmount(printed); ok {
			t.Errorf("parsePrintedAmount(%q) should not read a figure", printed)
		}
	}
}
