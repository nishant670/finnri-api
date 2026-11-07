package http

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	timepkg "time"
)

/*
Follow-ups that know exactly what they follow.

Device test, 2026-11-07: "How much spent on fuel till now??", then "09 October
spends", then the chip "And the day before?" came back as "Nothing recorded
for Fuel · yesterday". The app sent the thread as words — "How much spent on
fuel till now?? → 09 October spends" — and the model, asked to re-read that,
kept the first question's subject and counted the day back from today rather
than from the 9th.

So every answer now carries the query it was computed over (`context`), and the
app hands that back with the next message instead of the words. The model is
told the earlier query exactly. And the follow-ups Finnri itself offers — the
chips under an answer — are not left to the model at all: "Where did it go?",
"Biggest one?" and "And the day/week/month/year before?" are worked out here
from the earlier query, so the window before 9 October is the 8th, every time.
*/

// questionContextPrefix marks a context_question that is a resolved query
// rather than the words of an earlier question (which older apps still send).
const questionContextPrefix = "finnri-query:v1:"

// maxQuestionContextBytes bounds a resolved context. It is a few short fields,
// so anything longer is not one of ours.
const maxQuestionContextBytes = 2000

type questionContext struct {
	Metric      string   `json:"metric"`
	EntryType   string   `json:"type,omitempty"`
	Category    string   `json:"category,omitempty"`
	Merchant    string   `json:"merchant,omitempty"`
	Mode        string   `json:"mode,omitempty"`
	Keywords    []string `json:"keywords,omitempty"`
	Subject     string   `json:"subject,omitempty"`
	GroupBy     string   `json:"group_by,omitempty"`
	PeriodKind  string   `json:"period_kind"`
	PeriodLabel string   `json:"period_label,omitempty"`
	StartDate   string   `json:"start_date,omitempty"`
	EndDate     string   `json:"end_date,omitempty"`
}

// encodeQuestionContext is the context an answer hands back to the app.
func encodeQuestionContext(question ledgerQuestion) string {
	if question.Metric == metricUnsupported {
		return ""
	}
	raw, err := json.Marshal(questionContext{
		Metric:      question.Metric,
		EntryType:   question.questionFilterValues().Get("type"),
		Category:    question.Category,
		Merchant:    question.Merchant,
		Mode:        question.Mode,
		Keywords:    question.Keywords,
		Subject:     question.SubjectLabel,
		GroupBy:     question.GroupBy,
		PeriodKind:  question.PeriodKind,
		PeriodLabel: question.PeriodLabel,
		StartDate:   question.StartDate,
		EndDate:     question.EndDate,
	})
	if err != nil {
		return ""
	}
	return questionContextPrefix + string(raw)
}

// decodeQuestionContext reads a resolved context back, re-checking every field
// the way a model's query is checked: it came from a client, so it is input.
func decodeQuestionContext(raw string) (ledgerQuestion, bool) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, questionContextPrefix) || len(raw) > maxQuestionContextBytes {
		return ledgerQuestion{}, false
	}
	var context questionContext
	if err := json.Unmarshal([]byte(strings.TrimPrefix(raw, questionContextPrefix)), &context); err != nil {
		return ledgerQuestion{}, false
	}

	question := ledgerQuestion{GroupBy: "none"}
	switch context.Metric {
	case metricSpendTotal, metricIncomeTotal, metricNet, metricCount, metricAverage, metricLargest, metricBreakdown:
		question.Metric = context.Metric
	default:
		return ledgerQuestion{}, false
	}
	switch context.EntryType {
	case "expense", "income":
		question.EntryType = context.EntryType
	}
	if category := cleanCategoryName(context.Category); category != "" {
		if len([]rune(category)) > maxCustomCategoryLength {
			return ledgerQuestion{}, false
		}
		if resolved, ok := broadCategory(category); ok {
			category = resolved
		}
		question.Category = category
	}
	if merchant := strings.TrimSpace(context.Merchant); len(merchant) <= 200 {
		question.Merchant = merchant
	}
	if mode := strings.TrimSpace(context.Mode); mode != "" {
		if resolved, ok := canonicalParseMode(mode); ok {
			question.Mode = resolved
		}
	}
	question.Keywords = normalizeQuestionKeywords(context.Keywords)
	question.SubjectLabel = questionSubjectLabel(context.Subject, question.Keywords)
	switch context.GroupBy {
	case "category", "merchant", "month":
		question.GroupBy = context.GroupBy
	}

	if context.PeriodKind == "all_time" {
		question.PeriodKind, question.PeriodLabel = "all_time", "all time"
		return question, true
	}
	start, startErr := timepkg.Parse("2006-01-02", context.StartDate)
	end, endErr := timepkg.Parse("2006-01-02", context.EndDate)
	if startErr != nil || endErr != nil || end.Before(start) {
		return ledgerQuestion{}, false
	}
	question.PeriodKind = strings.TrimSpace(context.PeriodKind)
	question.PeriodLabel = strings.TrimSpace(context.PeriodLabel)
	if question.PeriodLabel == "" || len(question.PeriodLabel) > 60 {
		question.PeriodLabel = windowLabel(start, end)
	}
	question.StartDate, question.EndDate = context.StartDate, context.EndDate
	return question, true
}

// describeQuestionContext is the earlier query in words the model can follow.
func describeQuestionContext(question ledgerQuestion) string {
	parts := []string{"metric " + question.Metric}
	if entryType := question.questionFilterValues().Get("type"); entryType != "" {
		parts = append(parts, "type "+entryType)
	}
	if question.Category != "" {
		parts = append(parts, "category "+question.Category)
	}
	if question.Merchant != "" {
		parts = append(parts, "merchant "+question.Merchant)
	}
	if len(question.Keywords) > 0 {
		parts = append(parts, fmt.Sprintf("keywords [%s] (subject %s)", strings.Join(question.Keywords, ", "), question.SubjectLabel))
	}
	if question.Mode != "" {
		parts = append(parts, "mode "+question.Mode)
	}
	if question.StartDate == "" {
		parts = append(parts, "period all_time")
	} else {
		parts = append(parts, fmt.Sprintf("period %s to %s", question.StartDate, question.EndDate))
	}
	return strings.Join(parts, "; ")
}

var followUpPunctuation = regexp.MustCompile(`[?.!,]+`)

// chipFollowUp normalises a message to the follow-up it is, if it is one of
// the ones Finnri offers under an answer (or the same words typed).
func chipFollowUp(transcript string) string {
	text := strings.ToLower(followUpPunctuation.ReplaceAllString(transcript, " "))
	text = strings.Join(strings.Fields(text), " ")
	text = strings.TrimPrefix(text, "and ")
	text = strings.TrimPrefix(text, "what about ")
	switch text {
	case "where did it go", "where did the money go", "where did that go":
		return "where"
	case "biggest one", "the biggest one", "largest one", "biggest":
		return "biggest"
	case "last month":
		return "last_month"
	case "the day before", "day before", "the previous day":
		return "day"
	case "the week before", "week before", "the previous week":
		return "week"
	case "the month before", "month before", "the previous month":
		return "month"
	case "the year before", "year before", "the previous year":
		return "year"
	}
	return ""
}

// resolveChipFollowUp works a chip out from the query it follows, with no
// reading of the model's: the subject and filters carry over unchanged, and a
// "before" moves the window back from the earlier window, not from today.
func resolveChipFollowUp(transcript string, previous ledgerQuestion, now timepkg.Time) (ledgerQuestion, bool) {
	kind := chipFollowUp(transcript)
	if kind == "" {
		return ledgerQuestion{}, false
	}
	question := previous
	question.UnsupportedReason = ""
	// Pin the direction the earlier answer counted, so "Biggest one?" after
	// an income total looks at income.
	question.EntryType = previous.questionFilterValues().Get("type")

	switch kind {
	case "where":
		question.Metric = metricBreakdown
		question.GroupBy = "category"
		if question.Category != "" || len(question.Keywords) > 0 {
			// Already one category or one item: where it went is which shops.
			question.GroupBy = "merchant"
		}
		if question.Merchant != "" {
			question.GroupBy = "category"
		}
		return question, true
	case "biggest":
		question.Metric = metricLargest
		question.GroupBy = "none"
		return question, true
	case "last_month":
		question.PeriodKind, question.PeriodLabel, question.StartDate, question.EndDate =
			resolveQuestionPeriod(map[string]any{"kind": "last_month"}, now)
		return question, true
	}

	if previous.StartDate == "" {
		// All time has nothing before it.
		return ledgerQuestion{}, false
	}
	start, err := timepkg.Parse("2006-01-02", previous.StartDate)
	if err != nil {
		return ledgerQuestion{}, false
	}
	var values map[string]any
	switch kind {
	case "day":
		values = map[string]any{"kind": "day", "start": start.AddDate(0, 0, -1).Format("2006-01-02")}
	case "week":
		weekStart := start.AddDate(0, 0, -((int(start.Weekday())+6)%7)-7)
		values = map[string]any{"kind": "custom", "start": weekStart.Format("2006-01-02"), "end": weekStart.AddDate(0, 0, 6).Format("2006-01-02")}
	case "month":
		monthStart := timepkg.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, timepkg.UTC).AddDate(0, -1, 0)
		values = map[string]any{"kind": "month_of", "start": monthStart.Format("2006-01-02")}
	case "year":
		values = map[string]any{"kind": "custom",
			"start": fmt.Sprintf("%d-01-01", start.Year()-1),
			"end":   fmt.Sprintf("%d-12-31", start.Year()-1)}
	}
	question.PeriodKind, question.PeriodLabel, question.StartDate, question.EndDate = resolveQuestionPeriod(values, now)
	if kind == "year" {
		question.PeriodKind, question.PeriodLabel = "custom_year", fmt.Sprintf("%d", start.Year()-1)
	}
	return question, true
}
