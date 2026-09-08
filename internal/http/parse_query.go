package http

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	timepkg "time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"finance-parser-go/internal/ai"
	"finance-parser-go/internal/billing"
	"finance-parser-go/internal/models"
)

// The parse channel carries two directions, not one.
//
// The parser already understands "spent 450 on groceries at dmart via upi
// yesterday". The same sentence structure, asked backwards — "how much did I
// spend on groceries this month" — is the question the app has always implied
// it could answer and never could. Both arrive through /v1/parse, on the same
// input surface, through the same model call, and are told apart by "intent".
//
// The division of labour is the whole design: **the model decides what to look
// up and the database decides what the number is.** The model never sees the
// ledger and never emits a figure. Everything below takes the model's
// description of a query, resolves it into the exact query parameters the
// transaction list already takes, and computes the answer in SQL. That is what
// makes "never a wrong number" enforceable rather than hoped for — there is no
// path by which a model-authored number could reach the screen, because no
// model-authored number is ever read.
const (
	parseIntentCapture  = "capture"
	parseIntentQuestion = "question"
)

const (
	metricSpendTotal  = "spend_total"
	metricIncomeTotal = "income_total"
	metricNet         = "net"
	metricCount       = "count"
	metricAverage     = "average"
	metricLargest     = "largest"
	metricBreakdown   = "breakdown"
	metricUnsupported = "unsupported"
)

const (
	answerStatusAnswered    = "answered"
	answerStatusNoData      = "no_data"
	answerStatusUnsupported = "unsupported"
)

// answerBreakdownLimit is what fits on a card without becoming a report. The
// tap-through is there for the rest.
const answerBreakdownLimit = 5

// exampleQuestions is the one place the app's suggestion copy comes from, so a
// question that fails always fails pointing at questions that work.
var exampleQuestions = []string{
	"How much did I spend on food this month?",
	"What did I pay Swiggy in July?",
	"Where did my money go last month?",
}

// parsedIntent reads the model's routing decision, defaulting to capture.
//
// Defaulting matters more than it looks. An older prompt, a truncated
// completion or a model that simply omits the key all land here, and capture is
// the direction where a wrong guess is recoverable — the user sees a draft they
// can discard. Guessing "question" instead would swallow a real expense.
func parsedIntent(entry map[string]any) string {
	if value, ok := entry["intent"].(string); ok && strings.EqualFold(strings.TrimSpace(value), parseIntentQuestion) {
		return parseIntentQuestion
	}
	return parseIntentCapture
}

// ledgerQuestion is a validated question: every field here either came from a
// closed vocabulary or was checked against one.
type ledgerQuestion struct {
	Metric    string
	EntryType string
	Category  string
	Merchant  string
	Mode      string
	GroupBy   string

	PeriodKind  string
	PeriodLabel string
	StartDate   string
	EndDate     string

	// UnsupportedReason is set when the question is real but unanswerable from
	// the ledger. It is a code, never model prose — the sentence the user reads
	// is written here, in Go.
	UnsupportedReason string
}

type ledgerAnswerPeriod struct {
	Kind      string `json:"kind"`
	Label     string `json:"label"`
	StartDate string `json:"start_date,omitempty"`
	EndDate   string `json:"end_date,omitempty"`
}

type ledgerAnswerSlice struct {
	Label            string  `json:"label"`
	Amount           float64 `json:"amount"`
	TransactionCount int     `json:"transaction_count"`
	Percentage       float64 `json:"percentage"`
}

type ledgerAnswerEntry struct {
	EntryID  uint    `json:"entry_id"`
	Title    string  `json:"title"`
	Merchant string  `json:"merchant"`
	Category string  `json:"category"`
	Amount   float64 `json:"amount"`
	Date     string  `json:"date"`
}

// ledgerAnswer carries numbers and labels, never a formatted amount.
//
// The app owns money formatting — one formatter, per the project's conventions
// — so a rupee sign never crosses the wire. Period labels do, because the
// period is part of *what was asked* rather than part of the number, and the
// card and the scope line have to describe the same window in the same words.
type ledgerAnswer struct {
	Status           string              `json:"status"`
	Metric           string              `json:"metric"`
	Amount           *float64            `json:"amount"`
	Currency         string              `json:"currency"`
	TransactionCount int                 `json:"transaction_count"`
	Subject          string              `json:"subject"`
	EntryType        string              `json:"entry_type,omitempty"`
	Period           ledgerAnswerPeriod  `json:"period"`
	GroupBy          string              `json:"group_by,omitempty"`
	Breakdown        []ledgerAnswerSlice `json:"breakdown"`
	LargestEntry     *ledgerAnswerEntry  `json:"largest_entry,omitempty"`
	Reason           string              `json:"reason,omitempty"`
	Message          string              `json:"message,omitempty"`
	Suggestions      []string            `json:"suggestions"`

	// Filters is the query the number was computed over, in the transaction
	// list's own parameter names. The app hands it straight to the list, so the
	// rows behind the answer are the rows the answer counted.
	Filters map[string]string `json:"filters"`
}

// normalizeLedgerQuestion turns the model's "query" object into something that
// can be executed, rejecting anything it does not recognise.
//
// Nothing here is lenient. An unrecognised metric, group_by or period kind
// becomes an unsupported answer rather than a default, because every default
// available would answer a different question from the one that was asked — and
// a confidently wrong total is the failure this whole task is written against.
func normalizeLedgerQuestion(raw any, now timepkg.Time) ledgerQuestion {
	question := ledgerQuestion{Metric: metricUnsupported, GroupBy: "none"}

	values, ok := raw.(map[string]any)
	if !ok || values == nil {
		question.UnsupportedReason = "too_complex"
		question.PeriodKind, question.PeriodLabel, question.StartDate, question.EndDate = resolveQuestionPeriod(nil, now)
		return question
	}

	question.PeriodKind, question.PeriodLabel, question.StartDate, question.EndDate = resolveQuestionPeriod(values["period"], now)

	metric, _ := values["metric"].(string)
	switch strings.ToLower(strings.TrimSpace(metric)) {
	case metricSpendTotal, metricIncomeTotal, metricNet, metricCount, metricAverage, metricLargest, metricBreakdown:
		question.Metric = strings.ToLower(strings.TrimSpace(metric))
	default:
		question.UnsupportedReason = normalizeUnsupportedReason(values["unsupported_reason"])
		return question
	}

	if entryType, ok := values["type"].(string); ok {
		switch strings.ToLower(strings.TrimSpace(entryType)) {
		case "expense":
			question.EntryType = "expense"
		case "income":
			question.EntryType = "income"
		}
	}

	// The category is resolved against the same canonical list the ledger is
	// written with. An unrecognised one is not filed under Misc the way a
	// capture would be: filing a *question* under a fallback answers about the
	// wrong rows, so it becomes unsupported instead.
	if category, ok := values["category"].(string); ok && strings.TrimSpace(category) != "" {
		resolved, resolvedOK := canonicalCategory(category)
		if !resolvedOK {
			question.Metric = metricUnsupported
			question.UnsupportedReason = "too_complex"
			return question
		}
		question.Category = resolved
	}

	if merchant, ok := values["merchant"].(string); ok {
		question.Merchant = strings.TrimSpace(merchant)
		if len(question.Merchant) > 200 {
			question.Merchant = ""
		}
	}
	if mode, ok := values["mode"].(string); ok && strings.TrimSpace(mode) != "" {
		if resolved, resolvedOK := canonicalParseMode(mode); resolvedOK {
			question.Mode = resolved
		}
	}
	groupBy, _ := values["group_by"].(string)
	switch strings.ToLower(strings.TrimSpace(groupBy)) {
	case "category", "merchant", "month":
		question.GroupBy = strings.ToLower(strings.TrimSpace(groupBy))
	default:
		question.GroupBy = "none"
	}
	if question.Metric == metricBreakdown && question.GroupBy == "none" {
		question.GroupBy = "category"
	}

	return question
}

var unsupportedReasons = map[string]string{
	"forecast":    "Finnri answers from what is already recorded, so it cannot tell you about spending that has not happened yet.",
	"advice":      "Finnri can tell you what you spent, but it does not give financial advice.",
	"non_ledger":  "Finnri only knows the transactions in this app, so it cannot answer that one.",
	"splits":      "Split balances live on the Splits screen — this channel reads your transactions, not who owes whom.",
	"budgets":     "Budget progress lives on the Budgets screen — this channel reads your transactions, not your limits.",
	"too_complex": "That is more than Finnri can work out in one question yet. Try asking for one thing at a time.",
}

func normalizeUnsupportedReason(raw any) string {
	reason, _ := raw.(string)
	reason = strings.ToLower(strings.TrimSpace(reason))
	if _, ok := unsupportedReasons[reason]; ok {
		return reason
	}
	return "too_complex"
}

// resolveQuestionPeriod turns a period kind into real dates, in the user's
// timezone, from the server's clock.
//
// The model is deliberately not asked to do this arithmetic. "This month" on
// the 1st, "last week" across a year boundary and "yesterday" in a timezone the
// model was told about in a sentence are all places a language model quietly
// gets a date wrong, and a date that is wrong by one day is a total that is
// wrong by however much was spent that day — with nothing on screen to suggest
// it. So the model picks from a closed vocabulary of period *names* and the
// dates are computed here.
//
// An absent or unrecognised period resolves to this month rather than all time.
// Both are guesses; this one is the guess whose number is small enough to be
// questioned, and the label travels with the answer so the assumption is stated
// on the card rather than hidden inside it.
func resolveQuestionPeriod(raw any, now timepkg.Time) (kind, label, startDate, endDate string) {
	values, _ := raw.(map[string]any)
	requested := "this_month"
	if values != nil {
		if value, ok := values["kind"].(string); ok && strings.TrimSpace(value) != "" {
			requested = strings.ToLower(strings.TrimSpace(value))
		}
	}

	today := timepkg.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	day := func(t timepkg.Time) string { return t.Format("2006-01-02") }
	monthStart := timepkg.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, today.Location())
	// Monday-first weeks. Go's Sunday=0 makes the shift explicit rather than
	// leaving Sunday to land in the week it is about to start.
	weekday := (int(today.Weekday()) + 6) % 7
	weekStart := today.AddDate(0, 0, -weekday)

	switch requested {
	case "today":
		return requested, "today", day(today), day(today)
	case "yesterday":
		yesterday := today.AddDate(0, 0, -1)
		return requested, "yesterday", day(yesterday), day(yesterday)
	case "this_week":
		return requested, "this week", day(weekStart), day(today)
	case "last_week":
		start := weekStart.AddDate(0, 0, -7)
		return requested, "last week", day(start), day(start.AddDate(0, 0, 6))
	case "last_month":
		start := monthStart.AddDate(0, -1, 0)
		return requested, "last month", day(start), day(monthStart.AddDate(0, 0, -1))
	case "last_7_days":
		return requested, "the last 7 days", day(today.AddDate(0, 0, -6)), day(today)
	case "last_30_days":
		return requested, "the last 30 days", day(today.AddDate(0, 0, -29)), day(today)
	case "last_90_days":
		return requested, "the last 90 days", day(today.AddDate(0, 0, -89)), day(today)
	case "this_year":
		start := timepkg.Date(today.Year(), timepkg.January, 1, 0, 0, 0, 0, today.Location())
		return requested, "this year", day(start), day(today)
	case "last_year":
		start := timepkg.Date(today.Year()-1, timepkg.January, 1, 0, 0, 0, 0, today.Location())
		return requested, fmt.Sprintf("%d", today.Year()-1), day(start), day(timepkg.Date(today.Year()-1, timepkg.December, 31, 0, 0, 0, 0, today.Location()))
	case "all_time":
		return requested, "all time", "", ""
	case "month_of":
		// The model names the month by handing back any date inside it; the
		// bounds are derived rather than trusted, so a model that writes the
		// 31st of a 30-day month still gets the right window.
		anchor, ok := questionDate(values, "start")
		if !ok {
			anchor, ok = questionDate(values, "end")
		}
		if !ok {
			break
		}
		start := timepkg.Date(anchor.Year(), anchor.Month(), 1, 0, 0, 0, 0, today.Location())
		end := start.AddDate(0, 1, -1)
		label := start.Format("January 2006")
		if start.Year() == today.Year() {
			label = start.Format("January")
		}
		return requested, label, day(start), day(end)
	case "custom":
		start, startOK := questionDate(values, "start")
		end, endOK := questionDate(values, "end")
		if !startOK || !endOK || end.Before(start) {
			break
		}
		return requested, start.Format("2 Jan") + " to " + end.Format("2 Jan 2006"), day(start), day(end)
	}

	return "this_month", "this month", day(monthStart), day(today)
}

func questionDate(values map[string]any, field string) (timepkg.Time, bool) {
	if values == nil {
		return timepkg.Time{}, false
	}
	raw, ok := values[field].(string)
	if !ok || strings.TrimSpace(raw) == "" {
		return timepkg.Time{}, false
	}
	parsed, err := timepkg.Parse("2006-01-02", strings.TrimSpace(raw))
	if err != nil {
		return timepkg.Time{}, false
	}
	return parsed, true
}

// questionFilterValues is the single translation from a question to a query.
//
// Its output is used twice and only built once: to run the aggregate, and as
// the payload the app gives the transaction list when the user taps the answer.
// One map, two uses — so the list cannot open on a different set of rows from
// the one that produced the number.
func (q ledgerQuestion) questionFilterValues() url.Values {
	values := url.Values{}

	entryType := q.EntryType
	switch q.Metric {
	case metricIncomeTotal:
		entryType = "income"
	case metricSpendTotal, metricAverage, metricLargest, metricBreakdown:
		if entryType == "" {
			entryType = "expense"
		}
	case metricNet:
		// Net is income against spending, so constraining it to one of them
		// would answer a different question.
		entryType = ""
	}
	if entryType != "" {
		values.Set("type", entryType)
	}

	if q.Category != "" {
		values.Set("category", q.Category)
	}
	if q.Merchant != "" {
		// The list's own free-text filter, which spans title, merchant and
		// notes. A dedicated merchant predicate would match fewer rows here
		// than the search box matches there, and the two would disagree.
		values.Set("q", q.Merchant)
	}
	// There is deliberately no tag filter. The API has one, but the app's
	// filter sheet does not, so an answer scoped by tag could not hand the
	// transaction list the scope it was computed with — the tap-through would
	// open a broader set of rows than the number counted. Rather than show a
	// figure the list cannot corroborate, tag-shaped questions fall to the
	// category filter or to unsupported. Adding a tag facet to the sheet would
	// lift that restriction.
	if q.Mode != "" {
		values.Set("mode", q.Mode)
	}
	if q.StartDate != "" {
		values.Set("start_date", q.StartDate)
	}
	if q.EndDate != "" {
		values.Set("end_date", q.EndDate)
	}
	return values
}

func flattenFilterValues(values url.Values) map[string]string {
	flat := make(map[string]string, len(values))
	for key := range values {
		flat[key] = values.Get(key)
	}
	return flat
}

// unsupportedLedgerAnswer is the graceful failure: it says what Finnri cannot
// do, offers questions it can, and carries no number at all.
func unsupportedLedgerAnswer(question ledgerQuestion) ledgerAnswer {
	reason := question.UnsupportedReason
	message, ok := unsupportedReasons[reason]
	if !ok {
		reason = "too_complex"
		message = unsupportedReasons[reason]
	}
	return ledgerAnswer{
		Status:      answerStatusUnsupported,
		Metric:      metricUnsupported,
		Currency:    "INR",
		Reason:      reason,
		Message:     message,
		Suggestions: exampleQuestions,
		Breakdown:   []ledgerAnswerSlice{},
		Period: ledgerAnswerPeriod{
			Kind:      question.PeriodKind,
			Label:     question.PeriodLabel,
			StartDate: question.StartDate,
			EndDate:   question.EndDate,
		},
		Filters: map[string]string{},
	}
}

// answerLedgerQuestion computes the number.
func answerLedgerQuestion(userID uint, question ledgerQuestion) (ledgerAnswer, error) {
	if question.Metric == metricUnsupported {
		return unsupportedLedgerAnswer(question), nil
	}

	values := question.questionFilterValues()
	query, invalid := buildFilteredEntriesQuery(userID, values, false)
	if len(invalid) > 0 {
		// The question resolved into filters the list itself would reject. That
		// is a bug in this file rather than in the question, and the honest
		// answer is still no number.
		question.UnsupportedReason = "too_complex"
		return unsupportedLedgerAnswer(question), nil
	}

	summary, err := loadTransactionReportSummary(query)
	if err != nil {
		return ledgerAnswer{}, err
	}

	answer := ledgerAnswer{
		Status:      answerStatusAnswered,
		Metric:      question.Metric,
		Currency:    "INR",
		Subject:     question.answerSubject(),
		EntryType:   values.Get("type"),
		GroupBy:     question.GroupBy,
		Breakdown:   []ledgerAnswerSlice{},
		Suggestions: []string{},
		Filters:     flattenFilterValues(values),
		Period: ledgerAnswerPeriod{
			Kind:      question.PeriodKind,
			Label:     question.PeriodLabel,
			StartDate: question.StartDate,
			EndDate:   question.EndDate,
		},
	}

	if summary.TransactionCount == 0 {
		answer.Status = answerStatusNoData
		answer.TransactionCount = 0
		answer.Suggestions = exampleQuestions
		return answer, nil
	}

	amount := 0.0
	switch question.Metric {
	case metricSpendTotal:
		amount = summary.TotalExpense
		answer.TransactionCount = summary.ExpenseCount
	case metricIncomeTotal:
		amount = summary.TotalIncome
		answer.TransactionCount = summary.IncomeCount
	case metricNet:
		amount = summary.NetCashflow
		answer.TransactionCount = summary.TransactionCount
	case metricCount:
		answer.TransactionCount = summary.TransactionCount
	case metricAverage:
		total, count := summary.TotalExpense, summary.ExpenseCount
		if values.Get("type") == "income" {
			total, count = summary.TotalIncome, summary.IncomeCount
		}
		answer.TransactionCount = count
		if count == 0 {
			answer.Status = answerStatusNoData
			answer.Suggestions = exampleQuestions
			return answer, nil
		}
		amount = total / float64(count)
	case metricLargest:
		entryType := values.Get("type")
		if entryType == "" {
			entryType = "expense"
		}
		largest, found, largestErr := loadLargestEntry(query, entryType)
		if largestErr != nil {
			return ledgerAnswer{}, largestErr
		}
		if !found {
			answer.Status = answerStatusNoData
			answer.Suggestions = exampleQuestions
			return answer, nil
		}
		amount = largest.Amount
		answer.LargestEntry = &largest
		answer.TransactionCount = summary.TransactionCount
	case metricBreakdown:
		if values.Get("type") == "income" {
			amount = summary.TotalIncome
			answer.TransactionCount = summary.IncomeCount
		} else {
			amount = summary.TotalExpense
			answer.TransactionCount = summary.ExpenseCount
		}
	}

	if question.Metric != metricCount {
		answer.Amount = &amount
	}

	// One transaction behind a number is a transaction the card can simply
	// show. "See the transaction" is a button asking the reader to go and find
	// out what it was, when the row itself is what they were going to look at —
	// and the row is one query the answer has already narrowed to a single
	// result. Only ever the sole row: two or more and the list is the right
	// destination.
	if answer.TransactionCount == 1 && answer.LargestEntry == nil {
		entryType := values.Get("type")
		if entryType == "" {
			entryType = "expense"
		}
		only, found, onlyErr := loadLargestEntry(query, entryType)
		if onlyErr != nil {
			return ledgerAnswer{}, onlyErr
		}
		if found {
			answer.LargestEntry = &only
		}
	}

	breakdown, err := loadAnswerBreakdown(query, question, values.Get("type"), amount)
	if err != nil {
		return ledgerAnswer{}, err
	}
	answer.Breakdown = breakdown

	return answer, nil
}

// answerSubject is what the card names, in the ledger's own vocabulary.
func (q ledgerQuestion) answerSubject() string {
	parts := make([]string, 0, 3)
	if q.Category != "" {
		parts = append(parts, q.Category)
	}
	if q.Merchant != "" {
		parts = append(parts, q.Merchant)
	}
	if q.Mode != "" && len(parts) == 0 {
		parts = append(parts, q.Mode)
	}
	return strings.Join(parts, " · ")
}

func loadAnswerBreakdown(
	query *gorm.DB,
	question ledgerQuestion,
	entryType string,
	total float64,
) ([]ledgerAnswerSlice, error) {
	if question.GroupBy == "none" {
		return []ledgerAnswerSlice{}, nil
	}
	if entryType == "" {
		entryType = "expense"
	}

	if question.GroupBy == "month" {
		months, err := loadTransactionReportMonths(query)
		if err != nil {
			return nil, err
		}
		slices := make([]ledgerAnswerSlice, 0, len(months))
		for _, month := range months {
			value := month.Expense
			if entryType == "income" {
				value = month.Income
			}
			slices = append(slices, ledgerAnswerSlice{
				Label:            month.Month,
				Amount:           value,
				TransactionCount: month.TransactionCount,
				Percentage:       safePercentage(value, total),
			})
		}
		return capBreakdown(slices), nil
	}

	rows, err := loadTransactionReportBreakdown(query, question.GroupBy, entryType, total)
	if err != nil {
		return nil, err
	}
	slices := make([]ledgerAnswerSlice, 0, len(rows))
	for _, row := range rows {
		slices = append(slices, ledgerAnswerSlice{
			Label:            row.Label,
			Amount:           row.Amount,
			TransactionCount: row.TransactionCount,
			Percentage:       row.Percentage,
		})
	}
	return capBreakdown(slices), nil
}

// capBreakdown keeps the card a card. A single-row breakdown is dropped
// entirely: "Food & Drinks — 100%" under a total that is already Food & Drinks
// tells the reader nothing they did not just ask for.
func capBreakdown(slices []ledgerAnswerSlice) []ledgerAnswerSlice {
	if len(slices) < 2 {
		return []ledgerAnswerSlice{}
	}
	if len(slices) > answerBreakdownLimit {
		return slices[:answerBreakdownLimit]
	}
	return slices
}

func loadLargestEntry(query *gorm.DB, entryType string) (ledgerAnswerEntry, bool, error) {
	var entry models.Entry
	err := query.Session(&gorm.Session{}).
		Where("LOWER(entries.type) = ?", entryType).
		Order("entries.amount desc, entries.created_at desc").
		Limit(1).
		Find(&entry).Error
	if err != nil {
		return ledgerAnswerEntry{}, false, err
	}
	if entry.ID == 0 {
		return ledgerAnswerEntry{}, false, nil
	}
	return ledgerAnswerEntry{
		EntryID:  entry.ID,
		Title:    entry.Title,
		Merchant: entry.Merchant,
		Category: entry.Category,
		Amount:   entry.Amount.Float64(),
		Date:     entry.Date,
	}, true, nil
}

type answeredQuestionRequest struct {
	userID     uint
	transcript string
	tz         string
	rawQuery   any

	creditService *billing.CreditService
	usageEventID  uint
	subject       billing.CreditSubject
	actionCode    ai.ActionCode
	inputChars    int
	responseBytes int
	audioSize     int64
	// What the provider reported for the call that produced this question, so
	// answering one costs the same to account for as capturing a transaction.
	reported ai.Usage
}

// answerParsedQuestion completes the question direction of /v1/parse.
//
// It answers with 200 even when it cannot answer the question. An unanswerable
// question is not a failed request — the model call worked, the classification
// worked, and the user gets a real reply telling them what Finnri can do
// instead. Returning 4xx would put it through the app's error banner, which is
// where "Unable to load X right now" lives, and a question Finnri understood
// well enough to decline is not an error of that kind.
func (s *Server) answerParsedQuestion(c *gin.Context, request answeredQuestionRequest) {
	question := normalizeLedgerQuestion(request.rawQuery, timepkg.Now().In(s.questionLocation(request.tz)))

	questionPrompt, questionCompletion, questionTotal := tokenPointers(request.reported)

	answer, err := answerLedgerQuestion(request.userID, question)
	if err != nil {
		// The provider call succeeded and was paid for; the ledger read did
		// not. Charging for it and then showing nothing would be the worse of
		// the two, so the reservation is released.
		_, _ = request.creditService.FinalizeUsage(request.usageEventID, billing.ProviderUsage{
			Status:           billing.UsageStatusFailedAfterProvider,
			ErrorCode:        "question_lookup_failed",
			Provider:         "openai",
			Model:            s.cfg.OpenAILlmModel,
			InputChars:       &request.inputChars,
			ResponseBytes:    &request.responseBytes,
			AudioBytes:       optionalInt64(request.audioSize),
			PromptTokens:     questionPrompt,
			CompletionTokens: questionCompletion,
			TotalTokens:      questionTotal,
		})
		log.Printf("question lookup error: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "question_lookup_failed"})
		return
	}

	credits, err := s.finalizeParseSuccess(
		request.creditService,
		request.usageEventID,
		request.subject,
		request.actionCode,
		request.inputChars,
		request.responseBytes,
		request.audioSize,
		request.reported,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "credit_finalization_failed"})
		return
	}

	// "answer" rather than "draft". The app branches on this and nothing else:
	// a payload with this stage must never reach the transaction form.
	payload := gin.H{
		"stage":       "answer",
		"intent":      parseIntentQuestion,
		"source_text": request.transcript,
		"answer":      answer,
	}
	for key, value := range credits {
		payload[key] = value
	}
	c.JSON(http.StatusOK, payload)
}

func (s *Server) questionLocation(tz string) *timepkg.Location {
	if location, err := timepkg.LoadLocation(tz); err == nil && location != nil {
		return location
	}
	if s != nil && s.cfg != nil {
		if location, err := timepkg.LoadLocation(s.cfg.TZDefault); err == nil && location != nil {
			return location
		}
	}
	if location, err := timepkg.LoadLocation("Asia/Kolkata"); err == nil && location != nil {
		return location
	}
	return timepkg.FixedZone("IST", 5*3600+1800)
}
