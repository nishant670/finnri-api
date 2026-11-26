package http

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/xeipuuv/gojsonschema"

	"finnri/internal/ai"
	"finnri/internal/billing"
	"finnri/internal/config"
	"finnri/internal/database"
	"finnri/internal/models"
)

// providerSpy is every AI call a route can make. It counts them and fails them
// all, so what a test sees is whether the provider was reached, not what it
// said.
type providerSpy struct{ calls *int }

func (p providerSpy) hit() ([]byte, ai.Usage, error) {
	*p.calls++
	return nil, ai.Usage{}, context.DeadlineExceeded
}

func (p providerSpy) Transcribe(context.Context, string, []byte, string) (string, error) {
	*p.calls++
	return "", context.DeadlineExceeded
}
func (p providerSpy) ParseText(context.Context, string, string, ai.KnownNames) ([]byte, ai.Usage, error) {
	return p.hit()
}
func (p providerSpy) ParseTextBatch(context.Context, string, string, ai.KnownNames) ([]byte, ai.Usage, error) {
	return p.hit()
}
func (p providerSpy) ParseReceiptImage(context.Context, ai.StatementImage, string) ([]byte, ai.Usage, error) {
	return p.hit()
}
func (p providerSpy) ReadStatementText(context.Context, string, string) ([]byte, ai.Usage, error) {
	return p.hit()
}
func (p providerSpy) ReadStatementImages(context.Context, []ai.StatementImage, string) ([]byte, ai.Usage, error) {
	return p.hit()
}
func (p providerSpy) ParseStatementImages(context.Context, []ai.StatementImage, string, string) ([]byte, ai.Usage, error) {
	return p.hit()
}
func (p providerSpy) ParseStatementAlert(context.Context, string, string) ([]byte, ai.Usage, error) {
	return p.hit()
}

type aiRouteRequest struct {
	contentType string
	body        []byte
	// needsCard routes act on a card; needsStatement on a saved bill of it.
	needsCard      bool
	needsStatement bool
}

func multipartAIRequest(t *testing.T, field, filename string, data []byte, extra map[string]string) aiRouteRequest {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile(field, filename)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(data)
	for name, value := range extra {
		_ = writer.WriteField(name, value)
	}
	_ = writer.Close()
	return aiRouteRequest{contentType: writer.FormDataContentType(), body: body.Bytes()}
}

// No row the local parser can read here, so a refusal cannot fall back to it.
const unreadableStatementPDF = "BT /F1 10 Tf 72 720 Td (12 Oct) Tj 0 -14 Td (AMAZON PAY INDIA) Tj 0 -14 Td (1,250.00) Tj ET"

// The local parser reads this row on its own, for free.
const readableStatementPDF = "BT /F1 12 Tf 72 720 Td (10/07/2026 SWIGGY BANGALORE 480.00) Tj ET"

func statementPDFRequest(t *testing.T, content string) aiRouteRequest {
	t.Helper()
	path := filepath.Join(t.TempDir(), "statement.pdf")
	if err := writeTextPDF(t, path, content); err != nil {
		t.Fatalf("could not build a test PDF: %v", err)
	}
	pdf, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return multipartAIRequest(t, "file", "statement.pdf", pdf, nil)
}

// Every route that pays for an AI call, with a request that gets as far as
// making it.
var aiRoutes = []struct {
	name    string
	handler func(*Server, *gin.Context)
	request func(*testing.T) aiRouteRequest
}{
	{"typed text", (*Server).handleParse, func(t *testing.T) aiRouteRequest {
		return aiRouteRequest{contentType: "application/x-www-form-urlencoded", body: []byte("hint_text=metro+45")}
	}},
	{"typed text, several at once", (*Server).handleParse, func(t *testing.T) aiRouteRequest {
		return aiRouteRequest{contentType: "application/x-www-form-urlencoded", body: []byte("hint_text=metro+45+and+chai+20&multi=1")}
	}},
	{"voice", (*Server).handleParse, func(t *testing.T) aiRouteRequest {
		return multipartAIRequest(t, "audio", "clip.m4a", []byte("not-really-audio"), nil)
	}},
	{"receipt photo", (*Server).handleParseReceipt, func(t *testing.T) aiRouteRequest {
		return multipartAIRequest(t, "image", "receipt.png", receiptTestPNG, nil)
	}},
	{"receipt photo in a batch", (*Server).handleParseReceipt, func(t *testing.T) aiRouteRequest {
		return multipartAIRequest(t, "image", "receipt.png", receiptTestPNG, nil)
	}},
	{"statement PDF", (*Server).readCardStatement, func(t *testing.T) aiRouteRequest {
		request := statementPDFRequest(t, unreadableStatementPDF)
		request.needsCard = true
		return request
	}},
	{"statement PDF, saved bill", (*Server).uploadCardStatementPDF, func(t *testing.T) aiRouteRequest {
		request := statementPDFRequest(t, unreadableStatementPDF)
		request.needsCard, request.needsStatement = true, true
		return request
	}},
	{"statement screenshots, new bill", (*Server).readCardStatement, func(t *testing.T) aiRouteRequest {
		request := multipartAIRequest(t, "images", "page1.png", receiptTestPNG, nil)
		request.needsCard = true
		return request
	}},
	{"statement screenshots, saved bill", (*Server).uploadCardStatementScreenshots, func(t *testing.T) aiRouteRequest {
		request := multipartAIRequest(t, "images", "page1.png", receiptTestPNG, nil)
		request.needsCard, request.needsStatement = true, true
		return request
	}},
	{"statement alert", (*Server).importCardStatementAlert, func(t *testing.T) aiRouteRequest {
		return aiRouteRequest{
			contentType: "application/json",
			body:        []byte(`{"text":"Total due Rs 1,250 on your card","channel":"sms","received_at":"2026-10-25"}`),
			needsCard:   true,
		}
	}},
}

// newAIRouteUser is a user who can afford anything: a paid pass for the routes
// that need one, and credits.
func newAIRouteUser(t *testing.T) models.User {
	t.Helper()
	useSmokeDatabase(t)
	user := models.User{UUID: generateUUID(), Username: "replay_" + generateUUID()[:12]}
	if err := database.DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := billing.NewCreditService(database.DB).EnsureLoggedInFreeTrialGrant(user.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	plan := models.Plan{Code: "paid_test_" + generateUUID()[:8], Name: "Paid Test", BillingInterval: "monthly", PriceMinor: 19900, Currency: "INR", IncludedCredits: 3000, DailyCreditLimit: 500}
	if err := database.DB.Create(&plan).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.DB.Create(&models.UserSubscription{UserID: user.ID, PlanID: plan.ID, Status: "active", CurrentPeriodStart: now.Add(-time.Hour), CurrentPeriodEnd: now.AddDate(0, 1, 0), Provider: "test"}).Error; err != nil {
		t.Fatal(err)
	}
	return user
}

func callAIRoute(t *testing.T, name string, handler func(*Server, *gin.Context), request aiRouteRequest, user models.User, key string) (*httptest.ResponseRecorder, int) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	schema, err := gojsonschema.NewSchema(gojsonschema.NewReferenceLoader("file://../../schemas/expense_entry.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	server := &Server{
		cfg:       &config.Config{ReqTimeoutSec: 2, TZDefault: "Asia/Kolkata", MaxUploadMB: 15, MaxTranscriptChars: 1000},
		validator: schema,
		parser:    providerSpy{calls: &calls},
	}
	httpRequest := httptest.NewRequest("POST", "/", bytes.NewReader(request.body))
	httpRequest.Header.Set("Content-Type", request.contentType)
	httpRequest.Header.Set("Idempotency-Key", key)
	if strings.Contains(name, "in a batch") {
		httpRequest.Header.Set(receiptBatchHeader, "bills-replay-test")
	}
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Request = httpRequest
	ctx.Set("user", &user)
	ctx.Set("userID", user.ID)
	if request.needsCard {
		card := models.Account{UserID: user.ID, Type: "credit_card", Name: "Regalia", Last4: "4321", StatementDay: 5, DueDay: 20}
		if err := database.DB.Create(&card).Error; err != nil {
			t.Fatal(err)
		}
		id := card.ID
		if request.needsStatement {
			statement := models.CardStatement{UserID: user.ID, AccountID: card.ID, CycleStart: "2026-09-26", CycleEnd: "2026-10-25", StatementDate: "2026-10-25", DueDate: "2026-11-12", TotalDue: rupees(1250), Status: "unpaid"}
			if err := database.DB.Create(&statement).Error; err != nil {
				t.Fatal(err)
			}
			id = statement.ID
		}
		ctx.Params = gin.Params{{Key: "id", Value: strconv.FormatUint(uint64(id), 10)}}
	}
	handler(server, ctx)
	return response, calls
}

func aiUsageCount(t *testing.T, userID uint) int64 {
	t.Helper()
	var count int64
	if err := database.DB.Model(&models.AIUsageEvent{}).Where("user_id = ?", userID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	return count
}

// A request key pays for one AI call. Sending it again — whatever became of
// the first read, whatever it was for, and with or without credits left — must
// not reach the provider.
func TestAReusedRequestKeyNeverReachesTheAIProvider(t *testing.T) {
	firstReads := []struct {
		name   string
		action ai.ActionCode
		settle func(*testing.T, *billing.CreditService, uint)
	}{
		{"succeeded", ai.ActionTransactionParseText, func(t *testing.T, credits *billing.CreditService, id uint) {
			if _, err := credits.FinalizeUsage(id, billing.ProviderUsage{Status: billing.UsageStatusSucceeded}); err != nil {
				t.Fatal(err)
			}
		}},
		{"failed and refunded", ai.ActionTransactionParseReceipt, func(t *testing.T, credits *billing.CreditService, id uint) {
			free := 0
			if _, err := credits.FinalizeUsage(id, billing.ProviderUsage{Status: billing.UsageStatusFailedAfterProvider, FinalCredits: &free}); err != nil {
				t.Fatal(err)
			}
		}},
		{"cancelled", ai.ActionTransactionParseText, func(t *testing.T, credits *billing.CreditService, id uint) {
			if _, err := credits.CancelReservation(id); err != nil {
				t.Fatal(err)
			}
		}},
		{"still being read", ai.ActionTransactionParseText, func(*testing.T, *billing.CreditService, uint) {}},
	}

	for _, route := range aiRoutes {
		t.Run(route.name, func(t *testing.T) {
			// The control: with a new key this request does reach the provider,
			// so a refusal below is about the key and nothing else.
			control := newAIRouteUser(t)
			response, calls := callAIRoute(t, route.name, route.handler, route.request(t), control, "fresh-"+generateUUID())
			if calls == 0 {
				t.Fatalf("a new key should reach the provider: %d %s", response.Code, response.Body.String())
			}

			for _, first := range firstReads {
				t.Run(first.name, func(t *testing.T) {
					user := newAIRouteUser(t)
					credits := billing.NewCreditService(database.DB)
					key := "reused-" + generateUUID()
					event, _, err := credits.ReserveCredits(billing.SubjectForUser(user.ID), first.action, key)
					if err != nil {
						t.Fatal(err)
					}
					first.settle(t, credits, event.ID)
					if err := database.DB.Model(&models.CreditGrant{}).Where("user_id = ?", user.ID).Update("credits_remaining", 0).Error; err != nil {
						t.Fatal(err)
					}

					response, calls := callAIRoute(t, route.name, route.handler, route.request(t), user, key)

					if calls != 0 {
						t.Fatalf("the provider was called %d times for a reused key: %d %s", calls, response.Code, response.Body.String())
					}
					if response.Code != 409 || !strings.Contains(response.Body.String(), "ai_request_already_used") {
						t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
					}
					if got := aiUsageCount(t, user.ID); got != 1 {
						t.Fatalf("a refused repeat must not add or remove usage rows, got %d", got)
					}
				})
			}
		})
	}
}

// Rows the free parser already has are not refused because of the key, just as
// they are not refused for want of credits. The model is still not asked.
func TestAReusedKeyStillGetsTheLocalStatementRows(t *testing.T) {
	for _, saved := range []bool{false, true} {
		name, handler := "new bill", (*Server).readCardStatement
		if saved {
			name, handler = "saved bill", (*Server).uploadCardStatementPDF
		}
		t.Run(name, func(t *testing.T) {
			user := newAIRouteUser(t)
			key := "reused-" + generateUUID()
			if _, _, err := billing.NewCreditService(database.DB).ReserveCredits(billing.SubjectForUser(user.ID), ai.ActionTransactionParseText, key); err != nil {
				t.Fatal(err)
			}
			request := statementPDFRequest(t, readableStatementPDF)
			request.needsCard, request.needsStatement = true, saved

			response, calls := callAIRoute(t, name, handler, request, user, key)

			if calls != 0 {
				t.Fatalf("the provider was called %d times for a reused key", calls)
			}
			if response.Code != 200 || !strings.Contains(response.Body.String(), "SWIGGY BANGALORE") {
				t.Fatalf("the local rows should still come back: %d %s", response.Code, response.Body.String())
			}
			if got := aiUsageCount(t, user.ID); got != 1 {
				t.Fatalf("local rows are free, got %d usage rows", got)
			}
		})
	}
}

// A guest's keys are looked up by device, not by account.
func TestAGuestsReusedKeyNeverReachesTheAIProvider(t *testing.T) {
	useSmokeDatabase(t)
	deviceID := "guest-device-" + generateUUID()
	guest := models.User{UUID: generateUUID(), Username: "guest_" + generateUUID()[:12], IsGuest: true, DeviceID: &deviceID}
	if err := database.DB.Create(&guest).Error; err != nil {
		t.Fatal(err)
	}
	credits := billing.NewCreditService(database.DB)
	if _, _, err := credits.EnsureGuestTrialGrant(deviceID, "203.0.113.7"); err != nil {
		t.Fatal(err)
	}
	text := aiRoutes[0]

	key := "guest-key-" + generateUUID()
	response, calls := callAIRoute(t, text.name, text.handler, text.request(t), guest, key)
	if calls != 1 {
		t.Fatalf("a guest's new key should reach the provider once, got %d: %d %s", calls, response.Code, response.Body.String())
	}
	response, calls = callAIRoute(t, text.name, text.handler, text.request(t), guest, key)
	if calls != 0 || response.Code != 409 || !strings.Contains(response.Body.String(), "ai_request_already_used") {
		t.Fatalf("calls = %d, status = %d, body = %s", calls, response.Code, response.Body.String())
	}
}
