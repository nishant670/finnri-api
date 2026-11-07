package http

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"finnri/internal/models"

	"github.com/gin-gonic/gin"
)

// askFollowUp asks a question that may follow an earlier answer, returning the
// answer and the context it carries for the next one.
func askFollowUp(t *testing.T, user models.User, query, transcript, contextQuestion string) (answerEnvelope, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	server := questionServer(questionFixture(query))
	form := url.Values{"hint_text": {transcript}, "context_question": {contextQuestion}}
	request := httptest.NewRequest("POST", "/v1/parse", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = request
	context.Set("user", &user)
	context.Set("userID", user.ID)
	server.handleParse(context)

	var envelope answerEnvelope
	var carried struct {
		Answer struct {
			Context string `json:"context"`
		} `json:"answer"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v (%s)", err, response.Body.String())
	}
	_ = json.Unmarshal(response.Body.Bytes(), &carried)
	return envelope, carried.Answer.Context
}

// The device run of 2026-11-07, step for step: fuel, then a day, then the
// chip under the day. The chip must be the 8th, with no fuel in it.
func TestTheDayBeforeChipFollowsTheDayNotTheFirstQuestion(t *testing.T) {
	user := newQuestionUser(t)
	location, _ := time.LoadLocation("Asia/Kolkata")
	today := time.Now().In(location)
	asked := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, location).AddDate(0, 0, -5)
	dayBefore := asked.AddDate(0, 0, -1)
	seedQuestionEntry(t, user.ID, models.Entry{Title: "Petrol", Type: "expense", Amount: questionMoney(t, "2000.00"), Category: "Transport", Date: asked.AddDate(0, 0, -3).Format("2006-01-02")})
	seedQuestionEntry(t, user.ID, models.Entry{Title: "Lunch", Type: "expense", Amount: questionMoney(t, "548.00"), Category: "Food & Drinks", Date: asked.Format("2006-01-02")})
	seedQuestionEntry(t, user.ID, models.Entry{Title: "Groceries", Type: "expense", Amount: questionMoney(t, "239.00"), Category: "Food & Drinks", Date: dayBefore.Format("2006-01-02")})

	_, fuelContext := askFollowUp(t, user,
		`{"metric":"spend_total","type":"expense","keywords":["fuel","petrol"],"subject":"Fuel","period":{"kind":"all_time"}}`,
		"How much spent on fuel till now??", "")
	if !strings.HasPrefix(fuelContext, questionContextPrefix) {
		t.Fatalf("answer carried no context: %q", fuelContext)
	}

	day, dayContext := askFollowUp(t, user,
		`{"metric":"spend_total","type":"expense","period":{"kind":"day","start":"`+asked.Format("2006-01-02")+`"}}`,
		asked.Format("02 January")+" spends", fuelContext)
	if day.Answer.Amount == nil || *day.Answer.Amount != 548 {
		t.Fatalf("day answer = %+v", day.Answer)
	}

	// Whatever the model makes of the chip — here, the device's own bad
	// reading: fuel, yesterday — the server works it out from the day.
	chip, _ := askFollowUp(t, user,
		`{"metric":"spend_total","type":"expense","keywords":["fuel"],"subject":"Fuel","period":{"kind":"yesterday"}}`,
		"And the day before?", dayContext)
	if chip.Answer.Amount == nil || *chip.Answer.Amount != 239 || chip.Answer.Subject != "" {
		t.Fatalf("chip answer = %+v, want 239 for %s with no subject", chip.Answer, dayBefore.Format("2006-01-02"))
	}
	if chip.Answer.Period.StartDate != dayBefore.Format("2006-01-02") || chip.Answer.Period.EndDate != dayBefore.Format("2006-01-02") {
		t.Fatalf("chip period = %s..%s, want %s", chip.Answer.Period.StartDate, chip.Answer.Period.EndDate, dayBefore.Format("2006-01-02"))
	}
}

// A chip the model routed to capture is still answered.
func TestAChipTheModelCalledACaptureIsStillAnswered(t *testing.T) {
	user := newQuestionUser(t)
	seedQuestionEntry(t, user.ID, models.Entry{Title: "Petrol", Type: "expense", Amount: questionMoney(t, "2000.00"), Category: "Transport", Merchant: "HP", Date: thisMonthDate(t, 1)})
	seedQuestionEntry(t, user.ID, models.Entry{Title: "Diesel", Type: "expense", Amount: questionMoney(t, "900.00"), Category: "Transport", Merchant: "Shell", Date: thisMonthDate(t, 1)})
	previous := ledgerQuestion{Metric: metricSpendTotal, Keywords: []string{"petrol", "diesel"}, SubjectLabel: "Fuel"}
	previous.PeriodKind, previous.PeriodLabel, previous.StartDate, previous.EndDate =
		resolveQuestionPeriod(map[string]any{"kind": "this_month"}, time.Now())

	capture := []byte(`{"stage":"draft","intent":"capture","query":null,"type":"expense","title":"Biggest one","amount":null,"currency":"INR","mode":null,"category":null,"merchant":null,"tag":null,"purpose_type":null,"tags":[],"note":null,"date":null,"time":null,"recurring_candidate":null,"subscription_candidate":null,"split_candidate":null,"confidence":{},"needs_confirmation":{},"missing_fields":["amount"],"clarifications":[]}`)
	gin.SetMode(gin.TestMode)
	server := questionServer(capture)
	form := url.Values{"hint_text": {"Biggest one?"}, "context_question": {encodeQuestionContext(previous)}}
	request := httptest.NewRequest("POST", "/v1/parse", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(response)
	context.Request = request
	context.Set("user", &user)
	context.Set("userID", user.ID)
	server.handleParse(context)

	var envelope answerEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.Stage != "answer" {
		t.Fatalf("not answered: %s", response.Body.String())
	}
	if envelope.Answer.Metric != metricLargest || envelope.Answer.LargestEntry == nil || envelope.Answer.LargestEntry.Title != "Petrol" {
		t.Fatalf("biggest = %s, want the Petrol row", response.Body.String())
	}
}

func TestChipFollowUpsMoveFromTheEarlierWindow(t *testing.T) {
	now := time.Date(2026, time.November, 7, 11, 0, 0, 0, time.UTC)
	on := func(kind, start, end string) ledgerQuestion {
		q := ledgerQuestion{Metric: metricSpendTotal, Keywords: []string{"fuel"}, SubjectLabel: "Fuel"}
		values := map[string]any{"kind": kind, "start": start, "end": end}
		q.PeriodKind, q.PeriodLabel, q.StartDate, q.EndDate = resolveQuestionPeriod(values, now)
		return q
	}
	for _, testCase := range []struct {
		name       string
		previous   ledgerQuestion
		transcript string
		start, end string
		label      string
	}{
		{"day", on("day", "2026-10-09", ""), "And the day before?", "2026-10-08", "2026-10-08", "on 8 October"},
		{"month", on("month_of", "2026-10-01", ""), "And the month before?", "2026-09-01", "2026-09-30", "September"},
		{"this month to last", on("this_month", "", ""), "And last month?", "2026-10-01", "2026-10-31", "last month"},
		{"week", on("this_week", "", ""), "and the week before", "2026-10-26", "2026-11-01", "26 Oct to 1 Nov 2026"},
		{"year", on("this_year", "", ""), "And the year before?", "2025-01-01", "2025-12-31", "2025"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got, ok := resolveChipFollowUp(testCase.transcript, testCase.previous, now)
			if !ok {
				t.Fatal("chip not resolved")
			}
			if got.StartDate != testCase.start || got.EndDate != testCase.end || got.PeriodLabel != testCase.label {
				t.Fatalf("window = %s..%s %q, want %s..%s %q", got.StartDate, got.EndDate, got.PeriodLabel, testCase.start, testCase.end, testCase.label)
			}
			if strings.Join(got.Keywords, ",") != "fuel" || got.SubjectLabel != "Fuel" {
				t.Fatalf("subject changed: %+v", got)
			}
		})
	}

	if _, ok := resolveChipFollowUp("And the day before?", on("all_time", "", ""), now); ok {
		t.Fatal("all time has no day before")
	}

	where, _ := resolveChipFollowUp("Where did it go?", on("day", "2026-10-09", ""), now)
	if where.Metric != metricBreakdown || where.GroupBy != "merchant" {
		t.Fatalf("where = %s by %s, want a breakdown by shop for one item", where.Metric, where.GroupBy)
	}
	income := ledgerQuestion{Metric: metricIncomeTotal, PeriodKind: "this_month", StartDate: "2026-11-01", EndDate: "2026-11-07"}
	biggest, _ := resolveChipFollowUp("Biggest one?", income, now)
	if biggest.Metric != metricLargest || biggest.questionFilterValues().Get("type") != "income" {
		t.Fatalf("biggest after income = %s/%s", biggest.Metric, biggest.questionFilterValues().Get("type"))
	}
}

func TestQuestionContextRoundTripsAndRejectsJunk(t *testing.T) {
	question := ledgerQuestion{Metric: metricSpendTotal, Category: "Transport", Mode: "UPI", PeriodKind: "day", PeriodLabel: "on 9 October", StartDate: "2026-10-09", EndDate: "2026-10-09", GroupBy: "none"}
	decoded, ok := decodeQuestionContext(encodeQuestionContext(question))
	if !ok || decoded.Category != "Transport" || decoded.Mode != "UPI" || decoded.StartDate != "2026-10-09" || decoded.PeriodLabel != "on 9 October" {
		t.Fatalf("round trip = %+v %v", decoded, ok)
	}
	for _, raw := range []string{
		"How much did I spend on food?",
		questionContextPrefix + "{not json",
		questionContextPrefix + `{"metric":"delete_everything","period_kind":"all_time"}`,
		questionContextPrefix + `{"metric":"spend_total","period_kind":"custom","start_date":"2026-10-09","end_date":"2026-10-01"}`,
		questionContextPrefix + strings.Repeat("x", maxQuestionContextBytes),
	} {
		if _, ok := decodeQuestionContext(raw); ok {
			t.Fatalf("accepted %q", raw[:min(len(raw), 60)])
		}
	}
	// Words from an older app still reach the model the old way.
	if prompt := followUpPrompt("And last month?", "How much on food this month?"); !strings.Contains(prompt, "How much on food this month?") {
		t.Fatalf("plain context dropped: %q", prompt)
	}
	if prompt := followUpPrompt("And last month?", encodeQuestionContext(question)); !strings.Contains(prompt, "period 2026-10-09 to 2026-10-09") || strings.Contains(prompt, questionContextPrefix) {
		t.Fatalf("structured context not described: %q", prompt)
	}
}
