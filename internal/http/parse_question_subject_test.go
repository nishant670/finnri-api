package http

import "testing"

// The reported bug: "How much spent on kitchen utensils and items?" was
// answered with ₹3,32,538 over 43 transactions — the user's entire ledger,
// printed under their question about kitchen utensils.
func TestAQuestionNamingSomethingTheQueryIgnoredIsNotAnswered(t *testing.T) {
	dropped := ledgerQuestion{Metric: metricSpendTotal, PeriodKind: "all_time"}
	if !questionSubjectWasDropped("How much spent on kitchen utensils and items?", dropped) {
		t.Fatal("a named subject with nothing filtered on must not be answered as a bare total")
	}
}

func TestAnUnderstoodQuestionIsNeverRefused(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		transcript string
		question   ledgerQuestion
	}{
		{
			"the subject landed in category",
			"How much did I spend on food this month?",
			ledgerQuestion{Metric: metricSpendTotal, Category: "Food & Drinks"},
		},
		{
			"the subject landed in merchant",
			"How much have I paid for Swiggy?",
			ledgerQuestion{Metric: metricSpendTotal, Merchant: "Swiggy"},
		},
		{
			"the subject landed in mode",
			"How much did I spend on UPI?",
			ledgerQuestion{Metric: metricSpendTotal, Mode: "UPI"},
		},
		{
			"no subject was named, so none was dropped",
			"How much did I spend this month?",
			ledgerQuestion{Metric: metricSpendTotal},
		},
		{
			"a breakdown legitimately filters on nothing",
			"Where did my money go this month?",
			ledgerQuestion{Metric: metricBreakdown, GroupBy: "category"},
		},
		{
			// "on"/"for" introduce a period at least as often as a subject, and
			// swallowing those would refuse perfectly ordinary questions.
			"on a weekday is a period, not a thing",
			"What did I spend on Monday?",
			ledgerQuestion{Metric: metricSpendTotal},
		},
		{
			"on a date is a period, not a thing",
			"How much did I spend on 5 October?",
			ledgerQuestion{Metric: metricSpendTotal},
		},
		{
			"for a duration is a period, not a thing",
			"What did I spend for the last three months?",
			ledgerQuestion{Metric: metricSpendTotal},
		},
		{
			"on average is a manner, not a thing",
			"How much do I spend on average?",
			ledgerQuestion{Metric: metricAverage},
		},
		{
			// "in" is left out of the prepositions entirely for this reason.
			"in July is a period",
			"How much did I spend in July?",
			ledgerQuestion{Metric: metricSpendTotal},
		},
		{
			"an unsupported answer is already refusing, and must not be re-reasoned",
			"Will I afford a trip on Goa next month?",
			ledgerQuestion{Metric: metricUnsupported, UnsupportedReason: "forecast"},
		},
		{
			"a word merely starting with the preposition is not the preposition",
			"How much did I spend onions",
			ledgerQuestion{Metric: metricSpendTotal},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if questionSubjectWasDropped(testCase.transcript, testCase.question) {
				t.Fatalf("refused a question it understood: %q", testCase.transcript)
			}
		})
	}
}

func TestADroppedSubjectIsCaughtWhateverTheMetric(t *testing.T) {
	// Every metric answers about some set of rows, so every metric is wrong
	// about them when the subject that chose those rows was discarded.
	for _, metric := range []string{
		metricSpendTotal, metricIncomeTotal, metricNet,
		metricCount, metricAverage, metricLargest, metricBreakdown,
	} {
		if !questionSubjectWasDropped("what did I pay for kitchen utensils", ledgerQuestion{Metric: metric}) {
			t.Fatalf("metric %q answered a question whose subject was dropped", metric)
		}
	}
}

func TestTheRefusalExplainsWhatCanBeAskedInstead(t *testing.T) {
	question := ledgerQuestion{
		Metric:            metricUnsupported,
		UnsupportedReason: "unfiltered_subject",
		PeriodKind:        "this_month",
		PeriodLabel:       "this month",
	}
	answer := unsupportedLedgerAnswer(question)
	if answer.Reason != "unfiltered_subject" {
		t.Fatalf("reason = %q, want unfiltered_subject", answer.Reason)
	}
	// A refusal with no way forward is just a dead end.
	if answer.Message == "" || answer.Message == unsupportedReasons["too_complex"] {
		t.Fatalf("message does not explain this particular refusal: %q", answer.Message)
	}
}
