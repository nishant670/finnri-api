package http

import (
	"bytes"
	"context"
	"errors"
	"mime/multipart"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"finnri/internal/ai"
	"finnri/internal/billing"
	"finnri/internal/config"
	"finnri/internal/database"
	"finnri/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/xeipuuv/gojsonschema"
)

type fixtureParser struct {
	result        []byte
	err           error
	transcript    string
	transcribeErr error
	usage         ai.Usage
}

func (p fixtureParser) Transcribe(context.Context, string, []byte) (string, error) {
	return p.transcript, p.transcribeErr
}

func (p fixtureParser) ParseText(context.Context, string, string) ([]byte, ai.Usage, error) {
	return p.result, p.usage, p.err
}

func TestParseHandlerMapsProviderFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := &Server{
		cfg:    &config.Config{ReqTimeoutSec: 2, TZDefault: "Asia/Kolkata"},
		parser: fixtureParser{err: errors.New("provider unavailable")},
	}
	form := url.Values{"hint_text": {"chai ke 80 rupaye"}}
	request := httptest.NewRequest("POST", "/v1/parse", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = request
	attachParseCreditUser(t, context)

	server.handleParse(context)

	if response.Code != 422 || !strings.Contains(response.Body.String(), "could_not_parse") {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestParseHandlerReturnsDraftWithCreditMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	schema, err := gojsonschema.NewSchema(
		gojsonschema.NewReferenceLoader("file://../../schemas/expense_entry.schema.json"),
	)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		cfg:       &config.Config{ReqTimeoutSec: 2, TZDefault: "Asia/Kolkata"},
		validator: schema,
		parser: fixtureParser{result: []byte(`{
			"title":"Metro","amount":45,"type":"expense","currency":"INR",
			"mode":"UPI","category":"Travel","date":"2026-07-09"
		}`)},
	}

	form := url.Values{"hint_text": {"metro 45 via upi"}}
	request := httptest.NewRequest("POST", "/v1/parse", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = request
	attachParseCreditUser(t, context)

	server.handleParse(context)

	if response.Code != 200 {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), `"id"`) {
		t.Fatalf("parse response unexpectedly looks persisted: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"credits_charged":5`) {
		t.Fatalf("parse response did not include credit metadata: %s", response.Body.String())
	}
}

func TestParseHandlerAcceptsSubscriptionAndSplitCandidates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	schema, err := gojsonschema.NewSchema(
		gojsonschema.NewReferenceLoader("file://../../schemas/expense_entry.schema.json"),
	)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		cfg:       &config.Config{ReqTimeoutSec: 2, TZDefault: "Asia/Kolkata"},
		validator: schema,
		parser: fixtureParser{result: []byte(`{
			"title":"Dinner","amount":2500,"type":"expense","currency":"INR",
			"mode":"Cash","category":"Food","date":"2026-07-17",
			"subscription_candidate":{
				"name":"Cloud Plan","merchant":"Cloud Plan","category":"Bills","amount":1500,
				"billing_interval":"monthly","next_due_date":"2026-08-17","last_charged_date":"2026-07-17",
				"reminder_days":3,"cancel_before_due":false,"notes":null,"missing_fields":[]
			},
			"split_candidate_details":{
				"group_name":null,
				"participants":[{"friend_name":"Ria","share_amount":1250,"direction":"friend_owes_user"}],
				"missing_fields":[]
			}
		}`)},
	}

	form := url.Values{"hint_text": {"paid 2500 dinner split with Ria"}}
	request := httptest.NewRequest("POST", "/v1/parse", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = request
	attachParseCreditUser(t, context)

	server.handleParse(context)

	if response.Code != 200 {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"split_candidate":true`) {
		t.Fatalf("split candidate was not preserved: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"tag":"Subscription"`) {
		t.Fatalf("subscription tag was not normalized: %s", response.Body.String())
	}
}

func TestParseHandlerNormalizesNoCostEMICreditCardDrift(t *testing.T) {
	gin.SetMode(gin.TestMode)
	schema, err := gojsonschema.NewSchema(
		gojsonschema.NewReferenceLoader("file://../../schemas/expense_entry.schema.json"),
	)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		cfg:       &config.Config{ReqTimeoutSec: 2, TZDefault: "Asia/Kolkata"},
		validator: schema,
		parser: fixtureParser{result: []byte(`{
			"title":"Titan Specs","amount":null,"type":"expense","currency":"INR",
			"mode":"Credit Card","card_network":"HDFC","account_hint":null,
			"category":"Healthcare","merchant":"Titan","purpose_type":"emi",
			"tags":"EMI","date":"2026-07-19","emi_tenure_months":6,
			"no_cost_emi":true,
			"confidence":{"amount":0.2,"emi":0.9},
			"needs_confirmation":{"amount":true,"emi":false}
		}`)},
	}

	form := url.Values{"hint_text": {"I purchased a spectacle today for my mother-in-law from Titan and converted it into six months no-cost EMI paid from HDFC credit card"}}
	request := httptest.NewRequest("POST", "/v1/parse", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = request
	attachParseCreditUser(t, context)

	server.handleParse(context)

	body := response.Body.String()
	if response.Code != 200 {
		t.Fatalf("status = %d, body = %s", response.Code, body)
	}
	for _, unexpected := range []string{"schema_invalid", "emi_tenure_months", "no_cost_emi", `"emi":`} {
		if strings.Contains(body, unexpected) {
			t.Fatalf("response contains unsupported schema drift %q: %s", unexpected, body)
		}
	}
	for _, expected := range []string{`"mode":"Credit Card"`, `"card_network":null`, `"account_hint":"HDFC"`, `"tag":"EMI"`, `"purpose_type":"normal_spend"`, `"tags":["EMI"]`, `"missing_fields":["amount","category"]`} {
		if !strings.Contains(body, expected) {
			t.Fatalf("response missing %q: %s", expected, body)
		}
	}
}

func TestParseHandlerRejectsNonTransactionalPromptClearly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	schema, err := gojsonschema.NewSchema(
		gojsonschema.NewReferenceLoader("file://../../schemas/expense_entry.schema.json"),
	)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		cfg:       &config.Config{ReqTimeoutSec: 2, TZDefault: "Asia/Kolkata"},
		validator: schema,
		parser: fixtureParser{result: []byte(`{
			"title":null,"amount":null,"type":null,"currency":"INR",
			"mode":null,"category":null,"merchant":null,"purpose_type":null,
			"tags":[],"date":null,"confidence":{},"needs_confirmation":{},
			"missing_fields":[],"clarifications":["Tell me an expense, income, bill, subscription, split, or payment to add."]
		}`)},
	}

	form := url.Values{"hint_text": {"what should I cook for dinner tonight"}}
	request := httptest.NewRequest("POST", "/v1/parse", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = request
	attachParseCreditUser(t, context)

	server.handleParse(context)

	body := response.Body.String()
	if response.Code != 422 || !strings.Contains(body, `"error":"non_transactional_prompt"`) {
		t.Fatalf("status = %d, body = %s", response.Code, body)
	}
	if strings.Contains(body, "schema_invalid") {
		t.Fatalf("non-transactional prompts should not leak schema errors: %s", body)
	}
}

func TestParseHandlerReturnsInsufficientCredits(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := &Server{
		cfg:    &config.Config{ReqTimeoutSec: 2, TZDefault: "Asia/Kolkata"},
		parser: fixtureParser{result: []byte(`{"title":"Tea","amount":80,"type":"expense","currency":"INR","mode":"Cash","category":"Food","date":"2026-07-19"}`)},
	}

	form := url.Values{"hint_text": {"chai 80 cash"}}
	request := httptest.NewRequest("POST", "/v1/parse", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = request
	attachParseUser(t, context, false)

	server.handleParse(context)

	if response.Code != 402 || !strings.Contains(response.Body.String(), `"error":"insufficient_ai_credits"`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"required_credits":5`) {
		t.Fatalf("insufficient credit response missing required credits: %s", response.Body.String())
	}
}

func TestParseHandlerReturnsDailyLimitReached(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := &Server{
		cfg:    &config.Config{ReqTimeoutSec: 2, TZDefault: "Asia/Kolkata"},
		parser: fixtureParser{result: []byte(`{"title":"Tea","amount":80,"type":"expense","currency":"INR","mode":"Cash","category":"Food","date":"2026-07-19"}`)},
	}

	form := url.Values{"hint_text": {"chai 80 cash"}}
	request := httptest.NewRequest("POST", "/v1/parse", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = request
	user := attachParseUser(t, context, true)
	if err := database.DB.Create(&models.DailyCreditUsage{
		UserID:      &user.ID,
		UsageDate:   time.Now().UTC().Format("2006-01-02"),
		CreditsUsed: billing.LoggedInFreeDailyLimit,
	}).Error; err != nil {
		t.Fatal(err)
	}

	server.handleParse(context)

	if response.Code != 429 || !strings.Contains(response.Body.String(), `"error":"daily_ai_limit_reached"`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"daily_limit":50`) {
		t.Fatalf("daily limit response missing limit: %s", response.Body.String())
	}
}

func TestParseHandlerRejectsGlobalKillSwitch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := &Server{
		cfg:    &config.Config{ReqTimeoutSec: 2, TZDefault: "Asia/Kolkata", AIParseDisabled: true},
		parser: fixtureParser{result: []byte(`{"title":"Tea","amount":80,"type":"expense","currency":"INR","mode":"Cash","category":"Food","date":"2026-07-19"}`)},
	}

	context, response := newParseTextContext("chai 80 cash")
	attachParseCreditUser(t, context)

	server.handleParse(context)

	if response.Code != 503 || !strings.Contains(response.Body.String(), `"error":"ai_parse_disabled"`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestParseHandlerBlocksGuestWithoutValidFingerprint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := &Server{
		cfg:    &config.Config{ReqTimeoutSec: 2, TZDefault: "Asia/Kolkata"},
		parser: fixtureParser{result: []byte(`{"title":"Tea","amount":80,"type":"expense","currency":"INR","mode":"Cash","category":"Food","date":"2026-07-19"}`)},
	}

	context, response := newParseTextContext("chai 80 cash")
	useSmokeDatabase(t)
	deviceID := "short"
	user := models.User{UUID: t.Name(), Username: t.Name(), IsGuest: true, DeviceID: &deviceID}
	if err := database.DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	context.Set("user", &user)
	context.Set("userID", user.ID)

	server.handleParse(context)

	if response.Code != 401 || !strings.Contains(response.Body.String(), `"error":"invalid_credit_subject"`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestParseHandlerRejectsManualAIAbuseBlock(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := &Server{
		cfg:    &config.Config{ReqTimeoutSec: 2, TZDefault: "Asia/Kolkata"},
		parser: fixtureParser{result: []byte(`{"title":"Tea","amount":80,"type":"expense","currency":"INR","mode":"Cash","category":"Food","date":"2026-07-19"}`)},
	}

	context, response := newParseTextContext("chai 80 cash")
	user := attachParseCreditUser(t, context)
	if err := database.DB.Create(&models.AIAbuseBlock{
		UserID:     &user.ID,
		Scope:      "ai_parse",
		ReasonCode: "support_block",
		Active:     true,
	}).Error; err != nil {
		t.Fatal(err)
	}

	server.handleParse(context)

	if response.Code != 403 || !strings.Contains(response.Body.String(), `"error":"ai_access_blocked"`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestParseHandlerRejectsRepeatedFailedParseCooldown(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := &Server{
		cfg: &config.Config{
			ReqTimeoutSec:                   2,
			TZDefault:                       "Asia/Kolkata",
			AIFailedParseCooldownThreshold:  2,
			AIFailedParseCooldownWindowMin:  15,
			AIFailedParseCooldownMinutes:    10,
			AIProviderFailureThreshold:      0,
			AIProviderCircuitBreakerSeconds: 0,
		},
		parser: fixtureParser{result: []byte(`{"title":"Tea","amount":80,"type":"expense","currency":"INR","mode":"Cash","category":"Food","date":"2026-07-19"}`)},
	}

	context, response := newParseTextContext("chai 80 cash")
	user := attachParseCreditUser(t, context)
	now := time.Now().UTC()
	for i := 0; i < 2; i++ {
		if err := database.DB.Create(&models.AIUsageEvent{
			UserID:           &user.ID,
			RequestID:        "cooldown-failure-" + string(rune('0'+i)),
			ActionCode:       string(ai.ActionTransactionParseText),
			InputKind:        "text",
			Status:           billing.UsageStatusFailedAfterProvider,
			EstimatedCredits: 5,
			ReservedCredits:  5,
			FinalCredits:     5,
			StartedAt:        now.Add(-time.Duration(i) * time.Minute),
		}).Error; err != nil {
			t.Fatal(err)
		}
	}

	server.handleParse(context)

	if response.Code != 429 || !strings.Contains(response.Body.String(), `"error":"ai_parse_cooldown_active"`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestParseHandlerOpensProviderCircuitBreaker(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := &Server{
		cfg: &config.Config{
			ReqTimeoutSec:                   2,
			TZDefault:                       "Asia/Kolkata",
			AIProviderFailureThreshold:      2,
			AIProviderCircuitBreakerSeconds: 60,
			AIFailedParseCooldownThreshold:  0,
			AIFailedParseCooldownWindowMin:  0,
			AIFailedParseCooldownMinutes:    0,
		},
		parser: fixtureParser{err: errors.New("provider unavailable")},
	}

	useSmokeDatabase(t)
	user := models.User{UUID: t.Name(), Username: t.Name()}
	if err := database.DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := billing.NewCreditService(database.DB).EnsureLoggedInFreeTrialGrant(user.ID); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		context, response := newParseTextContext("chai 80 cash")
		context.Set("user", &user)
		context.Set("userID", user.ID)
		server.handleParse(context)
		if response.Code != 422 {
			t.Fatalf("attempt %d status = %d, body = %s", i+1, response.Code, response.Body.String())
		}
	}

	context, response := newParseTextContext("chai 80 cash")
	context.Set("user", &user)
	context.Set("userID", user.ID)
	server.handleParse(context)
	if response.Code != 503 || !strings.Contains(response.Body.String(), `"error":"ai_provider_circuit_open"`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestParseHandlerRejectsMediumVoiceForUnpaidUser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := &Server{
		cfg: &config.Config{
			ReqTimeoutSec:              2,
			TZDefault:                  "Asia/Kolkata",
			MaxUploadMB:                2,
			AIUnpaidMaxVoiceBytes:      512 * 1024,
			AIProviderFailureThreshold: 0,
		},
		parser: fixtureParser{transcript: "chai 80 cash"},
	}

	context, response := newParseAudioContext(700 * 1024)
	attachParseCreditUser(t, context)

	server.handleParse(context)

	if response.Code != 402 || !strings.Contains(response.Body.String(), `"error":"feature_locked"`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func attachParseCreditUser(t *testing.T, context *gin.Context) models.User {
	return attachParseUser(t, context, true)
}

func attachParseUser(t *testing.T, context *gin.Context, grantCredits bool) models.User {
	t.Helper()
	useSmokeDatabase(t)
	user := models.User{UUID: t.Name(), Username: t.Name()}
	if err := database.DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	if grantCredits {
		if _, _, err := billing.NewCreditService(database.DB).EnsureLoggedInFreeTrialGrant(user.ID); err != nil {
			t.Fatal(err)
		}
	}
	context.Set("user", &user)
	context.Set("userID", user.ID)
	return user
}

func newParseTextContext(text string) (*gin.Context, *httptest.ResponseRecorder) {
	form := url.Values{"hint_text": {text}}
	request := httptest.NewRequest("POST", "/v1/parse", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = request
	return context, response
}

func newParseAudioContext(size int) (*gin.Context, *httptest.ResponseRecorder) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("audio", "recording.m4a")
	_, _ = part.Write(bytes.Repeat([]byte("a"), size))
	_ = writer.Close()
	request := httptest.NewRequest("POST", "/v1/parse", body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = request
	return context, response
}

// The end-to-end version of the split failure: the exact sentence that used to
// come back as "I could not turn that into a clean transaction", answered by a
// model doing the two things it reliably does with an unknown group — a
// participant of nulls and a hint field it named itself.
func TestParseHandlerKeepsGroupSplitCapture(t *testing.T) {
	gin.SetMode(gin.TestMode)
	schema, err := gojsonschema.NewSchema(
		gojsonschema.NewReferenceLoader("file://../../schemas/expense_entry.schema.json"),
	)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		cfg:       &config.Config{ReqTimeoutSec: 2, TZDefault: "Asia/Kolkata"},
		validator: schema,
		parser: fixtureParser{result: []byte(`{
			"title":"Rent advance","amount":10000,"type":"expense","currency":"INR",
			"mode":"UPI","category":"Bills","merchant":"Landlord","date":"2026-08-19",
			"purpose_type":"normal_spend","tags":[],"split_candidate":true,
			"split_candidate_details":{
				"group_name":"bubu-dudu",
				"participants":[{"friend_name":null,"share_amount":null,"direction":null}],
				"missing_fields":["participant_shares"]
			},
			"confidence":{"amount":0.98},"needs_confirmation":{},
			"missing_fields":[],"clarifications":[]
		}`)},
	}

	form := url.Values{"hint_text": {
		"I paid 10000 as an advance payment to landlord for rented house, paid by UPI. split this expense in the group bubu-dudu",
	}}
	request := httptest.NewRequest("POST", "/v1/parse", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = request
	attachParseCreditUser(t, context)

	server.handleParse(context)

	body := response.Body.String()
	if response.Code != 200 {
		t.Fatalf("status = %d, body = %s", response.Code, body)
	}
	for _, expected := range []string{
		`"amount":10000`,
		`"mode":"UPI"`,
		`"split_candidate":true`,
		`"group_name":"bubu-dudu"`,
		`"participants":[]`,
		`"missing_fields":["share_amount"]`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("response missing %q: %s", expected, body)
		}
	}
}

// A candidate block that is not even an object cannot be coerced. Dropping it
// is still better than dropping the transaction it was attached to.
func TestParseHandlerRecoversDraftByDroppingUnreadableCandidate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	schema, err := gojsonschema.NewSchema(
		gojsonschema.NewReferenceLoader("file://../../schemas/expense_entry.schema.json"),
	)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		cfg:       &config.Config{ReqTimeoutSec: 2, TZDefault: "Asia/Kolkata"},
		validator: schema,
		parser: fixtureParser{result: []byte(`{
			"title":"House rent","amount":10000,"type":"expense","currency":"INR",
			"mode":"UPI","category":"Bills","merchant":"Landlord","date":"2026-08-19",
			"tags":[],"subscription_candidate":"monthly on the first",
			"confidence":{},"needs_confirmation":{},"missing_fields":[],"clarifications":[]
		}`)},
	}

	form := url.Values{"hint_text": {"paid 10000 house rent by upi, happens every month"}}
	request := httptest.NewRequest("POST", "/v1/parse", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = request
	attachParseCreditUser(t, context)

	server.handleParse(context)

	body := response.Body.String()
	if response.Code != 200 {
		t.Fatalf("status = %d, body = %s", response.Code, body)
	}
	if !strings.Contains(body, `"amount":10000`) || !strings.Contains(body, `"subscription_candidate":null`) {
		t.Fatalf("draft was not recovered: %s", body)
	}
	if !strings.Contains(body, "could not read the recurring details") {
		t.Fatalf("the dropped block was not explained: %s", body)
	}
}

// "Paid my HDFC credit card bill" is a purpose the normalizer has always
// emitted and the schema never allowed, so the whole capture 422'd.
func TestParseHandlerAcceptsCardBillPayment(t *testing.T) {
	gin.SetMode(gin.TestMode)
	schema, err := gojsonschema.NewSchema(
		gojsonschema.NewReferenceLoader("file://../../schemas/expense_entry.schema.json"),
	)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		cfg:       &config.Config{ReqTimeoutSec: 2, TZDefault: "Asia/Kolkata"},
		validator: schema,
		parser: fixtureParser{result: []byte(`{
			"title":"HDFC card","amount":24500,"type":"expense","currency":"INR",
			"mode":"Bank Account","account_hint":"HDFC bank","category":"Bills",
			"merchant":"HDFC","purpose_type":"credit_card_payment","tags":[],
			"date":"2026-08-19","confidence":{},"needs_confirmation":{},
			"missing_fields":[],"clarifications":[]
		}`)},
	}

	form := url.Values{"hint_text": {"paid 24500 HDFC credit card bill from my bank account"}}
	request := httptest.NewRequest("POST", "/v1/parse", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = request
	attachParseCreditUser(t, context)

	server.handleParse(context)

	body := response.Body.String()
	if response.Code != 200 {
		t.Fatalf("status = %d, body = %s", response.Code, body)
	}
	if !strings.Contains(body, `"purpose_type":"card_payment"`) {
		t.Fatalf("card bill purpose was not preserved: %s", body)
	}
}

// The regression: the client read the provider's token counts, logged them and
// returned only the content, so the handler had nothing to record and every
// row landed with null tokens. This drives a real parse and reads the row back.
func TestParseHandlerRecordsProviderTokensOnUsageEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	schema, err := gojsonschema.NewSchema(
		gojsonschema.NewReferenceLoader("file://../../schemas/expense_entry.schema.json"),
	)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		cfg:       &config.Config{ReqTimeoutSec: 2, TZDefault: "Asia/Kolkata", OpenAILlmModel: "gpt-4o-mini"},
		validator: schema,
		parser: fixtureParser{
			result: []byte(`{
				"title":"Metro","amount":45,"type":"expense","currency":"INR",
				"mode":"UPI","category":"Travel","date":"2026-07-09"
			}`),
			usage: ai.Usage{PromptTokens: 4387, CompletionTokens: 132, TotalTokens: 4519},
		},
	}

	context, response := newParseTextContext("metro 45 via upi")
	user := attachParseCreditUser(t, context)

	server.handleParse(context)
	if response.Code != 200 {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}

	var event models.AIUsageEvent
	if err := database.DB.Where("user_id = ?", user.ID).Order("id DESC").First(&event).Error; err != nil {
		t.Fatal(err)
	}
	if event.PromptTokens == nil || *event.PromptTokens != 4387 {
		t.Fatalf("prompt_tokens = %v, want 4387", event.PromptTokens)
	}
	if event.CompletionTokens == nil || *event.CompletionTokens != 132 {
		t.Fatalf("completion_tokens = %v, want 132", event.CompletionTokens)
	}
	if event.TotalTokens == nil || *event.TotalTokens != 4519 {
		t.Fatalf("total_tokens = %v, want 4519", event.TotalTokens)
	}
	if event.Model != "gpt-4o-mini" {
		t.Fatalf("model = %q — cost cannot be priced without it", event.Model)
	}
}
