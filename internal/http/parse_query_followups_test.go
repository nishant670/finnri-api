package http

import (
	"strings"
	"testing"
)

func TestComparisonWindowUsesTheSameDaysBefore(t *testing.T) {
	cases := []struct {
		question         ledgerQuestion
		start, end, text string
	}{
		{ledgerQuestion{PeriodKind: "this_month", StartDate: "2026-10-01", EndDate: "2026-10-21"}, "2026-09-01", "2026-09-21", "1–21 Sep"},
		{ledgerQuestion{PeriodKind: "this_month", StartDate: "2026-03-01", EndDate: "2026-03-31"}, "2026-02-01", "2026-02-28", "February"},
		{ledgerQuestion{PeriodKind: "last_month", StartDate: "2026-09-01", EndDate: "2026-09-30"}, "2026-08-01", "2026-08-31", "August"},
		{ledgerQuestion{PeriodKind: "this_week", StartDate: "2026-11-02", EndDate: "2026-11-06"}, "2026-10-28", "2026-11-01", "28 Oct–1 Nov"},
		{ledgerQuestion{PeriodKind: "today", StartDate: "2026-11-06", EndDate: "2026-11-06"}, "2026-11-05", "2026-11-05", "5 Nov"},
	}
	for _, tc := range cases {
		start, end, label, ok := comparisonWindow(tc.question)
		if !ok || start != tc.start || end != tc.end || label != tc.text {
			t.Fatalf("%s: got %s..%s %q ok=%v, want %s..%s %q", tc.question.PeriodKind, start, end, label, ok, tc.start, tc.end, tc.text)
		}
	}
	if _, _, _, ok := comparisonWindow(ledgerQuestion{PeriodKind: "all_time"}); ok {
		t.Fatal("all time has nothing before it")
	}
}

func TestFollowUpQuestionsFitTheAnswer(t *testing.T) {
	got := followUpQuestions(ledgerQuestion{Metric: metricSpendTotal, PeriodKind: "this_month"})
	if strings.Join(got, "|") != "Where did it go?|Biggest one?|And last month?" {
		t.Fatalf("spend follow-ups = %v", got)
	}
	got = followUpQuestions(ledgerQuestion{Metric: metricBreakdown, PeriodKind: "this_week"})
	if strings.Join(got, "|") != "Biggest one?|And the week before?" {
		t.Fatalf("breakdown follow-ups = %v", got)
	}
}

func TestFollowUpContextReachesOnlyTheModel(t *testing.T) {
	if withFollowUpContext("And last month?", "") != "And last month?" {
		t.Fatal("no context must leave the message alone")
	}
	got := withFollowUpContext("And last month?", "How much did I spend on food this month?")
	if !strings.Contains(got, "How much did I spend on food this month?") || !strings.HasSuffix(got, "\nAnd last month?") {
		t.Fatalf("context = %q", got)
	}
}

func TestFollowUpAfterAPastMonthAsksForTheOneBefore(t *testing.T) {
	got := followUpQuestions(ledgerQuestion{Metric: metricSpendTotal, PeriodKind: "last_month"})
	if got[len(got)-1] != "And the month before?" {
		t.Fatalf("follow-ups = %v", got)
	}
}
