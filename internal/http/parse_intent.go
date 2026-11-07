package http

import (
	"regexp"
	"strings"
	timepkg "time"
)

// People do not ask questions in sentences.
//
// The model is told to route every input to "capture" or "question", and it is
// good at the ones that look like questions — "how much did I spend on food
// this month" routes correctly every time. What it gets wrong is the way people
// actually type into a capture field once they trust it: "today spend". Two
// words, no verb, no question mark. The prompt calls capture the default and
// the overwhelmingly common case, which is true, so an ambiguous fragment lands
// there and the user gets a transaction form when they asked for a number.
//
// This is the deterministic half of the fix. The prompt teaches the model the
// shape; this catches the ones it still misses, without a second model call and
// without a credit. It runs only on a draft the model called a capture *and*
// that names no amount — a capture with an amount is a capture, whatever else
// the words look like, and that guard is what keeps a real expense from being
// swallowed by a question.
//
// Nothing here computes a figure or guesses at one. It produces the same
// query object the model would have produced, which then goes through
// `normalizeLedgerQuestion` and is answered in SQL like any other.

// metricWords maps the word people lead with to the metric it asks for.
//
// Ordered longest-first at match time, so "most expensive" is not read as a
// bare "most" and "how many" is not read as "how".
var metricWords = []struct {
	phrase string
	metric string
}{
	{"most expensive", metricLargest},
	{"biggest", metricLargest},
	{"largest", metricLargest},
	{"highest", metricLargest},
	{"breakdown", metricBreakdown},
	{"where did", metricBreakdown},
	{"where's", metricBreakdown},
	{"top categories", metricBreakdown},
	{"how many", metricCount},
	{"average", metricAverage},
	{"avg", metricAverage},
	{"earned", metricIncomeTotal},
	{"income", metricIncomeTotal},
	{"received", metricIncomeTotal},
	{"spends", metricSpendTotal},
	{"spending", metricSpendTotal},
	{"spent", metricSpendTotal},
	{"spend", metricSpendTotal},
	{"expenses", metricSpendTotal},
	{"expense", metricSpendTotal},
	{"total", metricSpendTotal},
}

// periodWords maps the way a window is spoken to the kind the resolver takes.
//
// Longest-first for the same reason: "last month" must not match "month", and
// "this week" must not match "week". The dates themselves are never computed
// here — `resolveQuestionPeriod` does that from the user's timezone.
var periodWords = []struct {
	phrase string
	kind   string
}{
	{"last 90 days", "last_90_days"},
	{"last 30 days", "last_30_days"},
	{"last three months", "last_90_days"},
	{"last 7 days", "last_7_days"},
	{"last seven days", "last_7_days"},
	{"this month", "this_month"},
	{"last month", "last_month"},
	{"this week", "this_week"},
	{"last week", "last_week"},
	{"this year", "this_year"},
	{"last year", "last_year"},
	{"all time", "all_time"},
	{"yesterday", "yesterday"},
	{"today", "today"},
	{"ever", "all_time"},
}

// digits is what separates "spend 250 on food" from "today spend". An amount in
// the text means money moved, and money that moved is a capture.
var digits = regexp.MustCompile(`[0-9]`)

// ledgerQuestionShape reads a transcript as a bare ledger question.
//
// It returns the query object a question of that shape asks for, or false when
// the text is not one. Deliberately conservative: it needs a word that can only
// be asking about records — a metric — and it refuses anything carrying a
// number, because that is an amount far more often than it is a period.
func ledgerQuestionShape(transcript string) (map[string]any, bool) {
	text := strings.ToLower(strings.TrimSpace(transcript))
	if text == "" {
		return nil, false
	}

	// Period phrases are matched and removed first, so that the digits in
	// "last 30 days" are not mistaken for an amount by the check below.
	periodKind := ""
	for _, period := range periodWords {
		if strings.Contains(text, period.phrase) {
			periodKind = period.kind
			text = strings.Replace(text, period.phrase, " ", 1)
			break
		}
	}

	// A calendar day is not an amount: "09 October spends" asks about a day.
	// Only the date's own digits are removed — the day itself is applied to
	// the query later, from the user's timezone, by applySpokenDay.
	if match, normalized, ok := spokenDay(text, timepkg.Now()); ok {
		text = normalized[:match.start] + " " + normalized[match.end:]
	}

	if digits.MatchString(text) {
		return nil, false
	}

	metric := ""
	for _, word := range metricWords {
		if containsWord(text, word.phrase) {
			metric = word.metric
			break
		}
	}
	if metric == "" {
		return nil, false
	}

	query := map[string]any{
		"metric":   metric,
		"group_by": "none",
	}
	if metric == metricBreakdown {
		query["group_by"] = "category"
	}
	if metric == metricIncomeTotal {
		query["type"] = "income"
	} else {
		query["type"] = "expense"
	}
	// An absent period is left absent rather than guessed at here — the
	// resolver's own default is "this month", and it is the one place that
	// assumption should live.
	if periodKind != "" {
		query["period"] = map[string]any{"kind": periodKind}
	}
	return query, true
}

// containsWord matches a phrase on word boundaries.
//
// A substring match would read "spend" out of "spending" — harmless — but also
// out of "stipend", and "total" out of "totalled". The boundary check costs
// nothing and removes the whole class.
func containsWord(text, phrase string) bool {
	index := 0
	for {
		found := strings.Index(text[index:], phrase)
		if found == -1 {
			return false
		}
		start := index + found
		end := start + len(phrase)
		beforeOK := start == 0 || !isWordByte(text[start-1])
		afterOK := end == len(text) || !isWordByte(text[end])
		if beforeOK && afterOK {
			return true
		}
		index = start + 1
	}
}

func isWordByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// parsedQuestionQuery decides which direction the parse goes, and hands back
// the query when it goes to the answer side.
//
// The model's own routing is taken as read whenever it says "question". The
// backstop only ever converts the other way, and only for a draft that named no
// amount: see the note at the top of this file for why that guard is the whole
// safety argument.
func parsedQuestionQuery(entry map[string]any, transcript string) (any, bool) {
	if parsedIntent(entry) == parseIntentQuestion {
		return entry["query"], true
	}
	if amount, ok := entry["amount"].(float64); ok && amount > 0 {
		return nil, false
	}
	// A split or a subscription is a claim about money that moved, and neither
	// belongs to any question this channel can ask. Their presence outweighs
	// the wording.
	if split, ok := entry["split_candidate"].(bool); ok && split {
		return nil, false
	}
	if entry["subscription_candidate"] != nil {
		return nil, false
	}
	query, ok := ledgerQuestionShape(transcript)
	if !ok {
		return nil, false
	}
	return query, true
}
