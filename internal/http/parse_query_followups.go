package http

import (
	"fmt"
	"math"
	"strings"
	timepkg "time"
)

// ledgerAnswerComparison is the same number for the window before (P64, board
// R12-Ask-Proposals): "12% less than 1–21 Sep". ChangePercent is nil when the
// earlier window had nothing, where a percentage would be meaningless.
type ledgerAnswerComparison struct {
	Amount        float64  `json:"amount"`
	Label         string   `json:"label"`
	StartDate     string   `json:"start_date"`
	EndDate       string   `json:"end_date"`
	ChangePercent *float64 `json:"change_percent,omitempty"`
}

// comparisonWindow is the window an answer is compared against: the same days
// of the period before. "This month so far" (1–21 Nov) is set against 1–21 Oct,
// not all of October, so a month in progress is not made to look thrifty. All
// time has nothing before it.
func comparisonWindow(question ledgerQuestion) (start, end, label string, ok bool) {
	if question.StartDate == "" || question.EndDate == "" {
		return "", "", "", false
	}
	from, errFrom := timepkg.Parse("2006-01-02", question.StartDate)
	to, errTo := timepkg.Parse("2006-01-02", question.EndDate)
	if errFrom != nil || errTo != nil || to.Before(from) {
		return "", "", "", false
	}

	var before, beforeEnd timepkg.Time
	switch question.PeriodKind {
	case "this_month", "last_month", "month_of":
		before = timepkg.Date(from.Year(), from.Month()-1, 1, 0, 0, 0, 0, timepkg.UTC)
		lastOfMonth := timepkg.Date(before.Year(), before.Month()+1, 0, 0, 0, 0, 0, timepkg.UTC)
		beforeEnd = lastOfMonth
		// A month in progress is set against the same days of the month
		// before, cut at that month's end.
		if question.PeriodKind == "this_month" && to.Day() < lastOfMonth.Day() {
			beforeEnd = timepkg.Date(before.Year(), before.Month(), to.Day(), 0, 0, 0, 0, timepkg.UTC)
		}
	case "this_year", "last_year":
		before = from.AddDate(-1, 0, 0)
		beforeEnd = to.AddDate(-1, 0, 0)
	default:
		days := int(to.Sub(from).Hours()/24) + 1
		beforeEnd = from.AddDate(0, 0, -1)
		before = beforeEnd.AddDate(0, 0, -(days - 1))
	}
	return before.Format("2006-01-02"), beforeEnd.Format("2006-01-02"), windowLabel(before, beforeEnd), true
}

// windowLabel is "1–21 Sep", "September" for a whole month, or "28 Oct–3 Nov".
func windowLabel(start, end timepkg.Time) string {
	lastOfMonth := timepkg.Date(start.Year(), start.Month()+1, 0, 0, 0, 0, 0, timepkg.UTC)
	switch {
	case start.Day() == 1 && end.Equal(lastOfMonth):
		return start.Format("January")
	case start.Equal(end):
		return start.Format("2 Jan")
	case start.Month() == end.Month() && start.Year() == end.Year():
		return fmt.Sprintf("%d–%s", start.Day(), end.Format("2 Jan"))
	default:
		return start.Format("2 Jan") + "–" + end.Format("2 Jan")
	}
}

// comparableMetric says which answers get a comparison: the totals and counts.
// An average or a single largest entry against last month's is a different
// question, and is left to be asked.
func comparableMetric(metric string) bool {
	switch metric {
	case metricSpendTotal, metricIncomeTotal, metricNet, metricCount, metricBreakdown:
		return true
	}
	return false
}

// metricValue picks the answer's number out of a summary.
func metricValue(metric, entryType string, summary TransactionReportSummary) float64 {
	switch metric {
	case metricIncomeTotal:
		return summary.TotalIncome
	case metricNet:
		return summary.NetCashflow
	case metricCount:
		return float64(summary.TransactionCount)
	case metricBreakdown:
		if entryType == "income" {
			return summary.TotalIncome
		}
		return summary.TotalExpense
	default:
		return summary.TotalExpense
	}
}

// loadAnswerComparison works the same question out for the window before.
func loadAnswerComparison(userID uint, question ledgerQuestion, current float64) (*ledgerAnswerComparison, error) {
	if !comparableMetric(question.Metric) {
		return nil, nil
	}
	start, end, label, ok := comparisonWindow(question)
	if !ok {
		return nil, nil
	}
	earlier := question
	earlier.StartDate, earlier.EndDate = start, end
	values := earlier.questionFilterValues()
	query, invalid := buildFilteredEntriesQuery(userID, values, false)
	if len(invalid) > 0 {
		return nil, nil
	}
	summary, err := loadTransactionReportSummary(query)
	if err != nil {
		return nil, err
	}
	previous := metricValue(question.Metric, values.Get("type"), summary)
	comparison := &ledgerAnswerComparison{Amount: previous, Label: label, StartDate: start, EndDate: end}
	if previous != 0 {
		change := math.Round((current-previous)/math.Abs(previous)*1000) / 10
		comparison.ChangePercent = &change
	}
	return comparison, nil
}

// followUpQuestions are the chips under an answer (P65): where it went, the
// biggest one, and the same for the period before — each worded as a
// follow-up, which the app sends with the question it follows (P67).
func followUpQuestions(question ledgerQuestion) []string {
	previous := "And last month?"
	switch question.PeriodKind {
	case "this_week", "last_week":
		previous = "And the week before?"
	case "today", "yesterday":
		previous = "And the day before?"
	case "this_year", "last_year":
		previous = "And the year before?"
	case "all_time":
		previous = ""
	}
	questions := []string{}
	if question.Metric != metricBreakdown {
		questions = append(questions, "Where did it go?")
	}
	if question.Metric != metricLargest {
		questions = append(questions, "Biggest one?")
	}
	if previous != "" {
		questions = append(questions, previous)
	}
	return questions
}

// maxContextQuestionRunes keeps the earlier question to a sentence.
const maxContextQuestionRunes = 300

// withFollowUpContext gives the model the question this message may follow
// (P67): "And last month?" means nothing alone and everything after "How much
// did I spend on food this month?". Only the Ask screen sends it, and only the
// model sees it — the transcript the user typed is still what is checked,
// stored as source_text and shown back. A message that is not a follow-up is
// read as itself; the preamble says so.
func withFollowUpContext(transcript, previous string) string {
	previous = strings.TrimSpace(previous)
	if previous == "" || strings.TrimSpace(transcript) == "" {
		return transcript
	}
	if runes := []rune(previous); len(runes) > maxContextQuestionRunes {
		previous = string(runes[:maxContextQuestionRunes])
	}
	return fmt.Sprintf(
		"[Earlier question, for context only: %q. If the message below is a follow-up to it — \"and last month?\", \"where did it go?\", \"biggest one?\" — answer it as that question with the change applied, keeping its subject and filters. Otherwise read the message on its own.]\n%s",
		previous,
		transcript,
	)
}
