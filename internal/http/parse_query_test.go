package http

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"finnri/internal/config"
	"finnri/internal/database"
	"finnri/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/xeipuuv/gojsonschema"
)

// questionFixture builds the parser payload the model returns for a question,
// so the tests exercise the routing and the SQL rather than the prompt.
func questionFixture(query string) []byte {
	return []byte(`{
		"stage":"draft","intent":"question","query":` + query + `,
		"type":null,"title":null,"amount":null,"currency":"INR","mode":null,
		"category":null,"merchant":null,"tag":null,"purpose_type":null,"tags":[],
		"note":null,"date":null,"time":null,"recurring_candidate":null,
		"subscription_candidate":null,"split_candidate":null,
		"confidence":{},"needs_confirmation":{},"missing_fields":[],"clarifications":[]
	}`)
}

type answerEnvelope struct {
	Stage      string `json:"stage"`
	Intent     string `json:"intent"`
	SourceText string `json:"source_text"`
	Answer     struct {
		Status           string   `json:"status"`
		Metric           string   `json:"metric"`
		Amount           *float64 `json:"amount"`
		Currency         string   `json:"currency"`
		TransactionCount int      `json:"transaction_count"`
		Subject          string   `json:"subject"`
		EntryType        string   `json:"entry_type"`
		Period           struct {
			Kind      string `json:"kind"`
			Label     string `json:"label"`
			StartDate string `json:"start_date"`
			EndDate   string `json:"end_date"`
		} `json:"period"`
		GroupBy   string `json:"group_by"`
		Breakdown []struct {
			Label            string  `json:"label"`
			Amount           float64 `json:"amount"`
			TransactionCount int     `json:"transaction_count"`
		} `json:"breakdown"`
		LargestEntry *struct {
			EntryID uint    `json:"entry_id"`
			Title   string  `json:"title"`
			Amount  float64 `json:"amount"`
		} `json:"largest_entry"`
		Reason      string            `json:"reason"`
		Message     string            `json:"message"`
		Suggestions []string          `json:"suggestions"`
		Filters     map[string]string `json:"filters"`
	} `json:"answer"`
	CreditsCharged int `json:"credits_charged"`
}

func questionServer(result []byte) *Server {
	return &Server{
		cfg:    &config.Config{ReqTimeoutSec: 2, TZDefault: "Asia/Kolkata"},
		parser: fixtureParser{result: result},
	}
}

// newQuestionUser opens the smoke database and returns a user with credits, so
// a test can seed a ledger before asking anything about it.
func newQuestionUser(t *testing.T) models.User {
	t.Helper()
	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	return attachParseCreditUser(t, context)
}

// askQuestion runs a question through the real handler and decodes the answer.
func askQuestion(t *testing.T, user models.User, query, transcript string) (answerEnvelope, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	server := questionServer(questionFixture(query))
	context, response := newParseTextContext(transcript)
	context.Set("user", &user)
	context.Set("userID", user.ID)

	server.handleParse(context)

	var envelope answerEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode answer: %v (body = %s)", err, response.Body.String())
	}
	return envelope, response
}

func parseDraftSchema(t *testing.T) *gojsonschema.Schema {
	t.Helper()
	schema, err := gojsonschema.NewSchema(
		gojsonschema.NewReferenceLoader("file://../../schemas/expense_entry.schema.json"),
	)
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func seedQuestionEntry(t *testing.T, userID uint, entry models.Entry) models.Entry {
	t.Helper()
	entry.UserID = userID
	entry.Currency = "INR"
	if err := database.DB.Create(&entry).Error; err != nil {
		t.Fatalf("seed entry: %v", err)
	}
	return entry
}

func questionMoney(t *testing.T, value string) models.Money {
	t.Helper()
	parsed, err := models.ParseMoney(value)
	if err != nil {
		t.Fatalf("parse money %q: %v", value, err)
	}
	return parsed
}

// thisMonthDate returns a date inside the current month in the app's default
// timezone, which is the window an unqualified question resolves to.
func thisMonthDate(t *testing.T, dayOfMonth int) string {
	t.Helper()
	location, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().In(location)
	day := dayOfMonth
	if day > now.Day() {
		day = now.Day()
	}
	return time.Date(now.Year(), now.Month(), day, 0, 0, 0, 0, location).Format("2006-01-02")
}

func TestParsedIntentDefaultsToCapture(t *testing.T) {
	cases := []struct {
		name  string
		entry map[string]any
		want  string
	}{
		{"missing key", map[string]any{"amount": 450.0}, parseIntentCapture},
		{"explicit capture", map[string]any{"intent": "capture"}, parseIntentCapture},
		{"question", map[string]any{"intent": "question"}, parseIntentQuestion},
		{"padded and cased", map[string]any{"intent": " Question "}, parseIntentQuestion},
		// A model that invents a third value must not be able to route a real
		// expense into the question path, where it would never be saved.
		{"unknown value", map[string]any{"intent": "query"}, parseIntentCapture},
		{"wrong type", map[string]any{"intent": 3}, parseIntentCapture},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := parsedIntent(testCase.entry); got != testCase.want {
				t.Fatalf("parsedIntent = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestQuestionPeriodsResolveFromTheServerClock(t *testing.T) {
	// A Wednesday, mid-month, mid-year — so week, month and year boundaries are
	// all visible in the expectations rather than coinciding.
	now := time.Date(2026, time.August, 12, 14, 30, 0, 0, time.UTC)

	cases := []struct {
		kind      string
		values    map[string]any
		wantLabel string
		wantStart string
		wantEnd   string
	}{
		{kind: "today", wantLabel: "today", wantStart: "2026-08-12", wantEnd: "2026-08-12"},
		{kind: "yesterday", wantLabel: "yesterday", wantStart: "2026-08-11", wantEnd: "2026-08-11"},
		{kind: "this_week", wantLabel: "this week", wantStart: "2026-08-10", wantEnd: "2026-08-12"},
		{kind: "last_week", wantLabel: "last week", wantStart: "2026-08-03", wantEnd: "2026-08-09"},
		{kind: "this_month", wantLabel: "this month", wantStart: "2026-08-01", wantEnd: "2026-08-12"},
		{kind: "last_month", wantLabel: "last month", wantStart: "2026-07-01", wantEnd: "2026-07-31"},
		{kind: "last_7_days", wantLabel: "the last 7 days", wantStart: "2026-08-06", wantEnd: "2026-08-12"},
		{kind: "last_30_days", wantLabel: "the last 30 days", wantStart: "2026-07-14", wantEnd: "2026-08-12"},
		{kind: "this_year", wantLabel: "this year", wantStart: "2026-01-01", wantEnd: "2026-08-12"},
		{kind: "last_year", wantLabel: "2025", wantStart: "2025-01-01", wantEnd: "2025-12-31"},
		{kind: "all_time", wantLabel: "all time", wantStart: "", wantEnd: ""},
		// The model names a month by handing back a date inside it. The bounds
		// are derived, so a February anchor still ends on the 28th.
		{
			kind:      "month_of",
			values:    map[string]any{"kind": "month_of", "start": "2026-02-14"},
			wantLabel: "February",
			wantStart: "2026-02-01",
			wantEnd:   "2026-02-28",
		},
		{
			kind:      "month_of",
			values:    map[string]any{"kind": "month_of", "start": "2025-03-31"},
			wantLabel: "March 2025",
			wantStart: "2025-03-01",
			wantEnd:   "2025-03-31",
		},
		{
			kind:      "custom",
			values:    map[string]any{"kind": "custom", "start": "2026-06-01", "end": "2026-06-15"},
			wantLabel: "1 Jun to 15 Jun 2026",
			wantStart: "2026-06-01",
			wantEnd:   "2026-06-15",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.kind+"/"+testCase.wantLabel, func(t *testing.T) {
			values := testCase.values
			if values == nil {
				values = map[string]any{"kind": testCase.kind}
			}
			_, label, start, end := resolveQuestionPeriod(values, now)
			if label != testCase.wantLabel || start != testCase.wantStart || end != testCase.wantEnd {
				t.Fatalf("resolveQuestionPeriod(%v) = %q %q..%q, want %q %q..%q",
					values, label, start, end, testCase.wantLabel, testCase.wantStart, testCase.wantEnd)
			}
		})
	}
}

// A period the app cannot resolve must not silently become "all time" — that
// answers a question nobody asked, with a much larger number.
func TestUnresolvablePeriodFallsBackToThisMonthNotAllTime(t *testing.T) {
	now := time.Date(2026, time.August, 12, 0, 0, 0, 0, time.UTC)
	for _, values := range []map[string]any{
		nil,
		{"kind": "since_i_started"},
		{"kind": "custom"},
		{"kind": "custom", "start": "2026-08-20", "end": "2026-08-01"},
		{"kind": "month_of"},
	} {
		kind, label, start, end := resolveQuestionPeriod(values, now)
		if kind != "this_month" || label != "this month" || start != "2026-08-01" || end != "2026-08-12" {
			t.Fatalf("resolveQuestionPeriod(%v) = %q/%q %q..%q, want this month", values, kind, label, start, end)
		}
	}
}

func TestQuestionAnswersSpendTotalFromTheLedger(t *testing.T) {
	user := newQuestionUser(t)

	seedQuestionEntry(t, user.ID, models.Entry{Title: "Swiggy", Type: "expense", Amount: questionMoney(t, "450.00"), Category: "Food & Drinks", Merchant: "Swiggy", Date: thisMonthDate(t, 2)})
	seedQuestionEntry(t, user.ID, models.Entry{Title: "Zomato", Type: "expense", Amount: questionMoney(t, "300.50"), Category: "Food & Drinks", Merchant: "Zomato", Date: thisMonthDate(t, 3)})
	seedQuestionEntry(t, user.ID, models.Entry{Title: "Metro", Type: "expense", Amount: questionMoney(t, "45.00"), Category: "Transport", Merchant: "DMRC", Date: thisMonthDate(t, 3)})
	seedQuestionEntry(t, user.ID, models.Entry{Title: "Old lunch", Type: "expense", Amount: questionMoney(t, "999.00"), Category: "Food & Drinks", Merchant: "Swiggy", Date: "2020-01-05"})

	envelope, response := askQuestion(t, user,
		`{"metric":"spend_total","type":"expense","category":"Food & Drinks","merchant":null,"mode":null,"tag":null,"period":{"kind":"this_month"},"group_by":"merchant","unsupported_reason":null}`,
		"how much did I spend on food this month")

	if response.Code != 200 {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if envelope.Stage != "answer" || envelope.Intent != parseIntentQuestion {
		t.Fatalf("stage/intent = %q/%q, want answer/question", envelope.Stage, envelope.Intent)
	}
	if envelope.Answer.Status != answerStatusAnswered {
		t.Fatalf("status = %q, body = %s", envelope.Answer.Status, response.Body.String())
	}
	if envelope.Answer.Amount == nil || *envelope.Answer.Amount != 750.5 {
		t.Fatalf("amount = %v, want 750.5 (the two in-month food rows only)", envelope.Answer.Amount)
	}
	if envelope.Answer.TransactionCount != 2 {
		t.Fatalf("transaction_count = %d, want 2", envelope.Answer.TransactionCount)
	}
	if envelope.Answer.Subject != "Food & Drinks" {
		t.Fatalf("subject = %q", envelope.Answer.Subject)
	}
	if envelope.Answer.Period.Label != "this month" {
		t.Fatalf("period label = %q", envelope.Answer.Period.Label)
	}
	if len(envelope.Answer.Breakdown) != 2 || envelope.Answer.Breakdown[0].Label != "Swiggy" {
		t.Fatalf("breakdown = %+v, want Swiggy first", envelope.Answer.Breakdown)
	}
	// The card charges credits like any other AI call, because the reservation
	// happened before anyone knew which direction this was.
	if envelope.CreditsCharged <= 0 {
		t.Fatalf("credits_charged = %d, want the parse charge", envelope.CreditsCharged)
	}
}

// The number on the card and the rows behind it come from one set of filters.
// This is the failure the whole design is arranged around: an answer that
// opens a list contradicting it.
func TestAnswerFiltersOpenExactlyTheRowsItCounted(t *testing.T) {
	user := newQuestionUser(t)

	seedQuestionEntry(t, user.ID, models.Entry{Title: "Swiggy", Type: "expense", Amount: questionMoney(t, "450.00"), Category: "Food & Drinks", Merchant: "Swiggy", Mode: "UPI", Date: thisMonthDate(t, 2)})
	seedQuestionEntry(t, user.ID, models.Entry{Title: "Cafe", Type: "expense", Amount: questionMoney(t, "180.00"), Category: "Food & Drinks", Merchant: "Blue Tokai", Mode: "Cash", Date: thisMonthDate(t, 4)})
	seedQuestionEntry(t, user.ID, models.Entry{Title: "Salary", Type: "income", Amount: questionMoney(t, "90000.00"), Category: "Misc", Date: thisMonthDate(t, 1)})
	seedQuestionEntry(t, user.ID, models.Entry{Title: "Cab", Type: "expense", Amount: questionMoney(t, "260.00"), Category: "Transport", Mode: "UPI", Date: thisMonthDate(t, 5)})

	envelope, _ := askQuestion(t, user,
		`{"metric":"spend_total","type":"expense","category":"Food & Drinks","period":{"kind":"this_month"},"group_by":"merchant"}`,
		"how much on food this month")

	if envelope.Answer.Status != answerStatusAnswered {
		t.Fatalf("status = %q", envelope.Answer.Status)
	}

	// Replay the answer's own filters through the transaction list, exactly as
	// the app's tap-through does.
	values := url.Values{}
	for key, value := range envelope.Answer.Filters {
		values.Set(key, value)
	}
	listRequest := httptest.NewRequest("GET", "/v1/entries?"+values.Encode(), nil)
	listResponse := httptest.NewRecorder()
	listContext, _ := gin.CreateTestContext(listResponse)
	listContext.Request = listRequest
	listContext.Set("userID", user.ID)

	server := questionServer(nil)
	server.listEntries(listContext)

	var list struct {
		Total   int64 `json:"total"`
		Entries []struct {
			Amount models.Money `json:"amount"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(listResponse.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v (body = %s)", err, listResponse.Body.String())
	}
	if int(list.Total) != envelope.Answer.TransactionCount {
		t.Fatalf("list total = %d, answer counted %d", list.Total, envelope.Answer.TransactionCount)
	}
	listed := 0.0
	for _, entry := range list.Entries {
		listed += entry.Amount.Float64()
	}
	if envelope.Answer.Amount == nil || fmt.Sprintf("%.2f", listed) != fmt.Sprintf("%.2f", *envelope.Answer.Amount) {
		t.Fatalf("list sums to %.2f, answer says %v", listed, envelope.Answer.Amount)
	}
}

func TestQuestionMetricsComputeAgainstTheSameRows(t *testing.T) {
	user := newQuestionUser(t)

	seedQuestionEntry(t, user.ID, models.Entry{Title: "Rent", Type: "expense", Amount: questionMoney(t, "20000.00"), Category: "Bills", Merchant: "Landlord", Date: thisMonthDate(t, 1)})
	seedQuestionEntry(t, user.ID, models.Entry{Title: "Swiggy", Type: "expense", Amount: questionMoney(t, "500.00"), Category: "Food & Drinks", Merchant: "Swiggy", Date: thisMonthDate(t, 2)})
	seedQuestionEntry(t, user.ID, models.Entry{Title: "Salary", Type: "income", Amount: questionMoney(t, "90000.00"), Category: "Misc", Merchant: "Acme", Date: thisMonthDate(t, 1)})

	cases := []struct {
		name       string
		query      string
		wantAmount *float64
		wantCount  int
	}{
		{"spend_total", `{"metric":"spend_total","period":{"kind":"this_month"},"group_by":"category"}`, floatPtr(20500), 2},
		{"income_total", `{"metric":"income_total","period":{"kind":"this_month"},"group_by":"none"}`, floatPtr(90000), 1},
		{"net", `{"metric":"net","period":{"kind":"this_month"},"group_by":"none"}`, floatPtr(69500), 3},
		{"count", `{"metric":"count","period":{"kind":"this_month"},"group_by":"none"}`, nil, 3},
		{"average", `{"metric":"average","type":"expense","period":{"kind":"this_month"},"group_by":"none"}`, floatPtr(10250), 2},
		{"largest", `{"metric":"largest","type":"expense","period":{"kind":"this_month"},"group_by":"none"}`, floatPtr(20000), 2},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			envelope, response := askQuestion(t, user, testCase.query, testCase.name)
			if response.Code != 200 || envelope.Answer.Status != answerStatusAnswered {
				t.Fatalf("status = %d/%q, body = %s", response.Code, envelope.Answer.Status, response.Body.String())
			}
			if testCase.wantAmount == nil {
				if envelope.Answer.Amount != nil {
					t.Fatalf("amount = %v, want none for a count", *envelope.Answer.Amount)
				}
			} else if envelope.Answer.Amount == nil || *envelope.Answer.Amount != *testCase.wantAmount {
				t.Fatalf("amount = %v, want %v", envelope.Answer.Amount, *testCase.wantAmount)
			}
			if envelope.Answer.TransactionCount != testCase.wantCount {
				t.Fatalf("transaction_count = %d, want %d", envelope.Answer.TransactionCount, testCase.wantCount)
			}
			if testCase.name == "largest" {
				if envelope.Answer.LargestEntry == nil || envelope.Answer.LargestEntry.Title != "Rent" {
					t.Fatalf("largest_entry = %+v, want the rent row", envelope.Answer.LargestEntry)
				}
			}
		})
	}
}

func floatPtr(value float64) *float64 { return &value }

// "Where did my money come from" is the same query with the sign flipped, and
// the breakdown has to follow the type rather than always ranking expenses.
func TestIncomeBreakdownRanksIncomeNotExpenses(t *testing.T) {
	user := newQuestionUser(t)

	seedQuestionEntry(t, user.ID, models.Entry{Title: "Salary", Type: "income", Amount: questionMoney(t, "90000.00"), Category: "Misc", Merchant: "Acme", Date: thisMonthDate(t, 1)})
	seedQuestionEntry(t, user.ID, models.Entry{Title: "Freelance", Type: "income", Amount: questionMoney(t, "15000.00"), Category: "Misc", Merchant: "Studio", Date: thisMonthDate(t, 3)})
	seedQuestionEntry(t, user.ID, models.Entry{Title: "Rent", Type: "expense", Amount: questionMoney(t, "20000.00"), Category: "Bills", Merchant: "Landlord", Date: thisMonthDate(t, 1)})

	envelope, _ := askQuestion(t, user,
		`{"metric":"breakdown","type":"income","period":{"kind":"this_month"},"group_by":"merchant"}`,
		"where did my money come from this month")

	if len(envelope.Answer.Breakdown) != 2 {
		t.Fatalf("breakdown = %+v, want the two income merchants", envelope.Answer.Breakdown)
	}
	for _, slice := range envelope.Answer.Breakdown {
		if slice.Label == "Landlord" {
			t.Fatalf("expense merchant leaked into an income breakdown: %+v", envelope.Answer.Breakdown)
		}
	}
}

func TestUnsupportedQuestionAnswersWithoutANumber(t *testing.T) {
	cases := []struct {
		name   string
		query  string
		reason string
	}{
		{"forecast", `{"metric":"unsupported","unsupported_reason":"forecast","period":{"kind":"this_month"}}`, "forecast"},
		{"splits", `{"metric":"unsupported","unsupported_reason":"splits","period":{"kind":"this_month"}}`, "splits"},
		{"budgets", `{"metric":"unsupported","unsupported_reason":"budgets","period":{"kind":"this_month"}}`, "budgets"},
		// An invented reason still fails gracefully rather than falling through.
		{"unknown reason", `{"metric":"unsupported","unsupported_reason":"vibes","period":{"kind":"this_month"}}`, "too_complex"},
		// A metric the app does not have is a question it cannot answer, not a
		// question it should answer with the nearest metric it does have.
		{"unknown metric", `{"metric":"median","period":{"kind":"this_month"}}`, "too_complex"},
		{"no query at all", `null`, "too_complex"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			envelope, response := askQuestion(t, newQuestionUser(t), testCase.query, "will I be able to afford a holiday")

			if response.Code != 200 {
				t.Fatalf("status = %d, body = %s — declining a question is not a failed request",
					response.Code, response.Body.String())
			}
			if envelope.Answer.Status != answerStatusUnsupported {
				t.Fatalf("status = %q, want unsupported", envelope.Answer.Status)
			}
			if envelope.Answer.Amount != nil {
				t.Fatalf("amount = %v — an unsupported question must carry no number", *envelope.Answer.Amount)
			}
			if envelope.Answer.Reason != testCase.reason {
				t.Fatalf("reason = %q, want %q", envelope.Answer.Reason, testCase.reason)
			}
			if strings.TrimSpace(envelope.Answer.Message) == "" || len(envelope.Answer.Suggestions) == 0 {
				t.Fatalf("unsupported answer must explain and suggest: %s", response.Body.String())
			}
		})
	}
}

// A category the ledger has never heard of is not filed under Misc the way a
// capture would be — that would answer about the wrong rows.
func TestUnknownCategoryQuestionIsUnsupportedRatherThanMisc(t *testing.T) {
	envelope, _ := askQuestion(t, newQuestionUser(t),
		`{"metric":"spend_total","category":"Crypto","period":{"kind":"this_month"}}`,
		"how much did I spend on crypto this month")

	if envelope.Answer.Status != answerStatusUnsupported {
		t.Fatalf("status = %q, want unsupported", envelope.Answer.Status)
	}
	if envelope.Answer.Amount != nil {
		t.Fatalf("amount = %v, want none", *envelope.Answer.Amount)
	}
}

func TestQuestionWithNoMatchingRowsSaysSoInsteadOfZero(t *testing.T) {
	envelope, response := askQuestion(t, newQuestionUser(t),
		`{"metric":"spend_total","category":"Travel","period":{"kind":"this_month"},"group_by":"merchant"}`,
		"how much did I spend on travel this month")

	if response.Code != 200 {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if envelope.Answer.Status != answerStatusNoData {
		t.Fatalf("status = %q, want no_data", envelope.Answer.Status)
	}
	if envelope.Answer.TransactionCount != 0 {
		t.Fatalf("transaction_count = %d, want 0", envelope.Answer.TransactionCount)
	}
	// The scope is still on the card, so the user can see which window came
	// back empty and correct the question rather than the ledger.
	if envelope.Answer.Period.Label != "this month" || envelope.Answer.Filters["category"] != "Travel" {
		t.Fatalf("no-data answer lost its scope: %s", response.Body.String())
	}
}

// A question must not be able to produce a transaction, even when the model
// fills capture fields on the same payload.
func TestQuestionNeverReturnsADraft(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := questionServer([]byte(`{
		"stage":"draft","intent":"question",
		"query":{"metric":"spend_total","category":"Food & Drinks","period":{"kind":"this_month"}},
		"type":"expense","title":"Food","amount":4820,"currency":"INR","mode":"UPI",
		"category":"Food & Drinks","merchant":"Swiggy","tags":[],
		"confidence":{},"needs_confirmation":{},"missing_fields":[],"clarifications":[]
	}`))
	context, response := newParseTextContext("how much did I spend on food this month")
	attachParseCreditUser(t, context)

	server.handleParse(context)

	body := response.Body.String()
	if response.Code != 200 {
		t.Fatalf("status = %d, body = %s", response.Code, body)
	}
	if strings.Contains(body, `"stage":"draft"`) {
		t.Fatalf("a question came back as a draft: %s", body)
	}
	// 4820 was the model's own invention. Nothing on the wire may carry it.
	if strings.Contains(body, "4820") {
		t.Fatalf("a model-authored figure reached the response: %s", body)
	}
	var envelope answerEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Answer.Status != answerStatusNoData {
		t.Fatalf("status = %q, want the empty ledger's own answer", envelope.Answer.Status)
	}
}

// Capture is untouched by the new branch: the overwhelmingly common direction
// still normalises, validates and returns a draft.
func TestCaptureStillReturnsADraftWhenIntentIsPresent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := questionServer([]byte(`{
		"intent":"capture","query":null,
		"title":"Metro","amount":45,"type":"expense","currency":"INR",
		"mode":"UPI","category":"Transport","date":"2026-07-09"
	}`))
	server.validator = parseDraftSchema(t)
	context, response := newParseTextContext("metro 45 via upi")
	attachParseCreditUser(t, context)

	server.handleParse(context)

	body := response.Body.String()
	if response.Code != 200 || !strings.Contains(body, `"stage":"draft"`) {
		t.Fatalf("status = %d, body = %s", response.Code, body)
	}
	// intent and query are routing, not draft fields, and must not survive into
	// the payload the transaction form reads.
	if strings.Contains(body, `"intent"`) || strings.Contains(body, `"query"`) {
		t.Fatalf("routing keys leaked into the draft: %s", body)
	}
}
