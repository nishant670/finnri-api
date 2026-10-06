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
	"finnri/internal/config"
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
