package http

import (
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"finnri/internal/models"
)

// The device report of 2026-11-06, first half: "09 October spends" was
// answered as all of October.
func TestANamedDayIsThatDayNotItsMonth(t *testing.T) {
	now := time.Date(2026, time.November, 6, 20, 0, 0, 0, time.UTC)
	month := ledgerQuestion{Metric: metricSpendTotal}
	month.PeriodKind, month.PeriodLabel, month.StartDate, month.EndDate =
		resolveQuestionPeriod(map[string]any{"kind": "month_of", "start": "2026-10-09"}, now)

	for _, testCase := range []struct {
		transcript string
		want       string
	}{
		{"09 October spends", "2026-10-09"},
		{"how much did I spend on 9th October", "2026-10-09"},
		{"October 9 expenses", "2026-10-09"},
		{"oct 9th spending", "2026-10-09"},
		{"spends on 9 of oct?", "2026-10-09"},
		{"9 October 2025 spends", "2025-10-09"},
		{"2026-10-09 expenses", "2026-10-09"},
		// No year, and the date is still to come this year: the last one.
		{"25 December spends", "2025-12-25"},
	} {
		t.Run(testCase.transcript, func(t *testing.T) {
			got := applySpokenDay(testCase.transcript, month, now)
			if got.StartDate != testCase.want || got.EndDate != testCase.want || got.PeriodKind != "day" {
				t.Fatalf("period = %s %s..%s, want the single day %s", got.PeriodKind, got.StartDate, got.EndDate, testCase.want)
			}
		})
	}
}

func TestADayLeftAloneWhenItIsOneEndOfARange(t *testing.T) {
	now := time.Date(2026, time.November, 6, 20, 0, 0, 0, time.UTC)
	question := ledgerQuestion{Metric: metricSpendTotal, PeriodKind: "custom", StartDate: "2026-10-01", EndDate: "2026-10-09"}
	for _, transcript := range []string{
		"spends from 1 October to 9 October",
		"spent since 9 October",
		"spending between 1 oct and 9 oct",
		"how much on 5 Oct and 9 Oct",
		"spent in October",
		"spent 500 on food",
		"food in the week of 9 October",
	} {
		if got := applySpokenDay(transcript, question, now); got.StartDate != question.StartDate || got.EndDate != question.EndDate {
			t.Fatalf("%q moved the period to %s..%s", transcript, got.StartDate, got.EndDate)
		}
	}
}

func TestDayPeriodLabels(t *testing.T) {
	now := time.Date(2026, time.November, 6, 9, 0, 0, 0, time.UTC)
	for _, testCase := range []struct {
		start, kind, label, date string
	}{
		{"2026-10-09", "day", "on 9 October", "2026-10-09"},
		{"2025-03-02", "day", "on 2 Mar 2025", "2025-03-02"},
		{"2026-11-06", "today", "today", "2026-11-06"},
		{"2026-11-05", "yesterday", "yesterday", "2026-11-05"},
		{"2026-12-25", "day", "on 25 Dec 2025", "2025-12-25"},
	} {
		kind, label, start, end := resolveQuestionPeriod(map[string]any{"kind": "day", "start": testCase.start}, now)
		if kind != testCase.kind || label != testCase.label || start != testCase.date || end != testCase.date {
			t.Fatalf("day %s = %s %q %s..%s, want %s %q %s", testCase.start, kind, label, start, end, testCase.kind, testCase.label, testCase.date)
		}
	}
}

func TestANamedMonthStillToComeIsLastYears(t *testing.T) {
	now := time.Date(2026, time.November, 6, 9, 0, 0, 0, time.UTC)
	_, label, start, end := resolveQuestionPeriod(map[string]any{"kind": "month_of", "start": "2026-12-01"}, now)
	if label != "December 2025" || start != "2025-12-01" || end != "2025-12-31" {
		t.Fatalf("December asked in November = %q %s..%s, want last December", label, start, end)
	}
}

func TestTheNoModelBackstopReadsADatedQuestion(t *testing.T) {
	if _, ok := ledgerQuestionShape("09 October spends"); !ok {
		t.Fatal(`"09 October spends" is a question about a day, not a capture`)
	}
	// An amount is still an amount.
	if _, ok := ledgerQuestionShape("spent 250 on 9 October"); ok {
		t.Fatal(`"spent 250 on 9 October" names an amount and must stay a capture`)
	}
}

func TestQuestionKeywordsAreCleanedAndDeduplicated(t *testing.T) {
	got := normalizeQuestionKeywords([]any{"Fuel", "petrol", "Petrol Pump", "fuel", "diesel|cng", "%", "x", 42})
	want := []string{"fuel", "petrol", "diesel cng"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("keywords = %#v, want %#v", got, want)
	}
	if label := questionSubjectLabel(nil, want); label != "Fuel" {
		t.Fatalf("label = %q, want Fuel", label)
	}
	if label := questionSubjectLabel("Kitchen utensils", []string{"utensil"}); label != "Kitchen utensils" {
		t.Fatalf("label = %q, want the model's subject", label)
	}
}

// The device report of 2026-11-06, second half: "How much spent on fuel till
// now??" was answered with every Transport row — rickshaws, cabs and metro.
func TestAnItemQuestionCountsOnlyThatItem(t *testing.T) {
	user := newQuestionUser(t)
	seedQuestionEntry(t, user.ID, models.Entry{Title: "Petrol", Type: "expense", Amount: questionMoney(t, "2000.00"), Category: "Transport", Date: thisMonthDate(t, 2)})
	seedQuestionEntry(t, user.ID, models.Entry{Title: "Fuel refill", Type: "expense", Amount: questionMoney(t, "1500.00"), Category: "Misc", Date: thisMonthDate(t, 3)})
	seedQuestionEntry(t, user.ID, models.Entry{Title: "Bike", Type: "expense", Amount: questionMoney(t, "700.00"), Category: "Transport", Merchant: "Indian Oil", Notes: "diesel top-up", Date: thisMonthDate(t, 3)})
	seedQuestionEntry(t, user.ID, models.Entry{Title: "E rickshaw", Type: "expense", Amount: questionMoney(t, "30.00"), Category: "Transport", Date: thisMonthDate(t, 4)})
	seedQuestionEntry(t, user.ID, models.Entry{Title: "Cab", Type: "expense", Amount: questionMoney(t, "386.00"), Category: "Transport", Date: thisMonthDate(t, 4)})

	envelope, response := askQuestion(t, user,
		`{"metric":"spend_total","type":"expense","category":null,"merchant":null,"keywords":["fuel","petrol","diesel","cng"],"subject":"Fuel","period":{"kind":"all_time"},"group_by":"none"}`,
		"How much spent on fuel till now??")

	if envelope.Answer.Status != answerStatusAnswered || envelope.Answer.Amount == nil {
		t.Fatalf("not answered: %s", response.Body.String())
	}
	if *envelope.Answer.Amount != 4200 || envelope.Answer.TransactionCount != 3 {
		t.Fatalf("fuel = %v over %d rows, want 4200 over 3 (petrol, fuel, diesel — not rickshaw or cab)", *envelope.Answer.Amount, envelope.Answer.TransactionCount)
	}
	if envelope.Answer.Subject != "Fuel" {
		t.Fatalf("subject = %q, want Fuel", envelope.Answer.Subject)
	}
	if got := envelope.Answer.Filters["q"]; got != "fuel|petrol|diesel|cng" {
		t.Fatalf("filters.q = %q, want the keyword alternatives", got)
	}

	// The tap-through: the list, given the answer's own filters, opens on the
	// rows the answer counted.
	values := url.Values{}
	for key, value := range envelope.Answer.Filters {
		values.Set(key, value)
	}
	query, invalid := buildFilteredEntriesQuery(user.ID, values, false)
	if len(invalid) > 0 {
		t.Fatalf("list rejected the answer's filters: %v", invalid)
	}
	var titles []string
	if err := query.Order("entries.amount desc").Pluck("entries.title", &titles).Error; err != nil {
		t.Fatal(err)
	}
	if strings.Join(titles, ",") != "Petrol,Fuel refill,Bike" {
		t.Fatalf("list opened on %v, want the three fuel rows", titles)
	}
}

func TestAQuestionAboutADayCountsOnlyThatDay(t *testing.T) {
	location, _ := time.LoadLocation("Asia/Kolkata")
	now := time.Now().In(location)
	asked := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, location).AddDate(0, 0, -3)
	user := newQuestionUser(t)
	seedQuestionEntry(t, user.ID, models.Entry{Title: "Lunch", Type: "expense", Amount: questionMoney(t, "548.00"), Category: "Food & Drinks", Date: asked.Format("2006-01-02")})
	seedQuestionEntry(t, user.ID, models.Entry{Title: "Snacks", Type: "expense", Amount: questionMoney(t, "70.00"), Category: "Food & Drinks", Date: asked.AddDate(0, 0, -1).Format("2006-01-02")})

	// The model read the day as its month; the server holds it to the day.
	envelope, response := askQuestion(t, user,
		`{"metric":"spend_total","type":"expense","period":{"kind":"month_of","start":"`+asked.Format("2006-01-02")+`"},"group_by":"none"}`,
		asked.Format("02 January")+" spends")

	if envelope.Answer.Amount == nil || *envelope.Answer.Amount != 548 || envelope.Answer.TransactionCount != 1 {
		t.Fatalf("answer = %s, want 548 from the one row on %s", response.Body.String(), asked.Format("2006-01-02"))
	}
	if envelope.Answer.Period.StartDate != asked.Format("2006-01-02") || envelope.Answer.Period.EndDate != asked.Format("2006-01-02") {
		t.Fatalf("period = %s..%s, want the single day", envelope.Answer.Period.StartDate, envelope.Answer.Period.EndDate)
	}
	if !strings.HasPrefix(envelope.Answer.Period.Label, "on ") {
		t.Fatalf("label = %q, want \"on <day>\"", envelope.Answer.Period.Label)
	}
}

// A category the app does not have is searched for as a word, never filed
// under Misc and never refused outright.
func TestAnUnknownCategoryIsSearchedForAsAWord(t *testing.T) {
	user := newQuestionUser(t)
	seedQuestionEntry(t, user.ID, models.Entry{Title: "Crypto purchase", Type: "expense", Amount: questionMoney(t, "5000.00"), Category: "Misc", Date: thisMonthDate(t, 1)})
	seedQuestionEntry(t, user.ID, models.Entry{Title: "Stationery", Type: "expense", Amount: questionMoney(t, "90.00"), Category: "Misc", Date: thisMonthDate(t, 1)})

	envelope, response := askQuestion(t, user,
		`{"metric":"spend_total","category":"Crypto","period":{"kind":"this_month"}}`,
		"how much did I spend on crypto this month")

	if envelope.Answer.Amount == nil || *envelope.Answer.Amount != 5000 || envelope.Answer.TransactionCount != 1 {
		t.Fatalf("answer = %s, want only the crypto row", response.Body.String())
	}
	if envelope.Answer.Filters["category"] != "" || envelope.Answer.Filters["q"] != "crypto" {
		t.Fatalf("filters = %v, want a search for crypto and no category", envelope.Answer.Filters)
	}
}

func TestACustomCategoryQuestionUsesTheUsersCategory(t *testing.T) {
	user := newQuestionUser(t)
	seedQuestionEntry(t, user.ID, models.Entry{Title: "Vet visit", Type: "expense", Amount: questionMoney(t, "1200.00"), Category: "Pets", Date: thisMonthDate(t, 1)})
	seedQuestionEntry(t, user.ID, models.Entry{Title: "Dog food", Type: "expense", Amount: questionMoney(t, "800.00"), Category: "Pets", Date: thisMonthDate(t, 1)})

	envelope, response := askQuestion(t, user,
		`{"metric":"spend_total","category":"pets","period":{"kind":"this_month"}}`,
		"how much on pets this month")

	if envelope.Answer.Amount == nil || *envelope.Answer.Amount != 2000 {
		t.Fatalf("answer = %s, want both Pets rows", response.Body.String())
	}
	if envelope.Answer.Filters["category"] != "Pets" {
		t.Fatalf("filters = %v, want the user's Pets category", envelope.Answer.Filters)
	}
}

func TestListSearchAlternatives(t *testing.T) {
	if got := searchAlternatives("chai point"); !reflect.DeepEqual(got, []string{"chai point"}) {
		t.Fatalf("plain search = %v", got)
	}
	if got := searchAlternatives(" fuel | petrol ||"); !reflect.DeepEqual(got, []string{"fuel", "petrol"}) {
		t.Fatalf("alternatives = %v", got)
	}
	if got := searchAlternatives("|"); len(got) != 0 {
		t.Fatalf("a bare separator = %v, want nothing", got)
	}
}

// The exact device failure, end to end in the server: the model filed "fuel"
// under Transport. The user never said "transport", so fuel is what counts.
func TestAFuelQuestionFiledUnderTransportIsNarrowedBack(t *testing.T) {
	question := ledgerQuestion{Metric: metricSpendTotal, Category: "Transport"}
	applyNarrowSubject("How much spent on fuel till now??", &question)
	if question.Category != "" || strings.Join(question.Keywords, "|") != "fuel|petrol|diesel|cng" || question.SubjectLabel != "Fuel" {
		t.Fatalf("question = %+v, want a fuel search", question)
	}

	kept := ledgerQuestion{Metric: metricSpendTotal, Category: "Transport"}
	applyNarrowSubject("transport spends this month", &kept)
	if kept.Category != "Transport" || len(kept.Keywords) != 0 {
		t.Fatalf("a question naming the category itself was narrowed: %+v", kept)
	}
}

func TestOnlyAWholeCategoryWordBecomesTheCategory(t *testing.T) {
	now := time.Date(2026, time.November, 6, 9, 0, 0, 0, time.UTC)
	for word, want := range map[string]string{
		"Transport": "Transport", "food": "Food & Drinks", "Food & Drinks": "Food & Drinks",
		"gifts": "Family/Gifts", "Salary": "Salary",
	} {
		question := normalizeLedgerQuestion(map[string]any{"metric": "spend_total", "category": word}, now)
		if question.Category != want || question.UnknownCategory != "" {
			t.Fatalf("category %q = %q (unknown %q), want %q", word, question.Category, question.UnknownCategory, want)
		}
	}
	for _, word := range []string{"groceries", "fuel", "petrol", "rent", "Crypto"} {
		question := normalizeLedgerQuestion(map[string]any{"metric": "spend_total", "category": word}, now)
		if question.Category != "" || question.UnknownCategory != strings.ToLower(word) {
			t.Fatalf("narrow %q became category %q", word, question.Category)
		}
	}
}

func TestACategoryTheModelDroppedIsReadFromTheWords(t *testing.T) {
	for transcript, want := range map[string]string{
		"transport this month":         "Transport",
		"food and drinks spends":       "Food & Drinks",
		"how much on bills last month": "Bills",
		"salary received this year":    "Salary",
	} {
		question := ledgerQuestion{Metric: metricSpendTotal}
		applyNamedCategory(transcript, &question)
		if question.Category != want {
			t.Fatalf("%q = %q, want %q", transcript, question.Category, want)
		}
	}
	for _, transcript := range []string{"today spend", "where did my money go", "any other spends today"} {
		question := ledgerQuestion{Metric: metricSpendTotal}
		applyNamedCategory(transcript, &question)
		if question.Category != "" {
			t.Fatalf("%q read a category %q out of nothing", transcript, question.Category)
		}
	}
}
