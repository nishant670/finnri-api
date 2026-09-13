package http

import "strings"

/*
Catching a question whose subject the query threw away.

"How much spent on kitchen utensils and items?" came back as a plain
spend_total with no category and no merchant on it, over all time — so the app
printed ₹3,32,538 from 43 transactions, which is the user's entire ledger,
directly underneath their question about kitchen utensils. Nothing was broken;
every layer did what it was told. The model could not map "kitchen utensils" to
any of the eight expense categories, dropped it, and answered a different and
much larger question in its place.

normalizeLedgerQuestion already refuses a category it does not recognise, for
exactly this reason — "filing a *question* under a fallback answers about the
wrong rows". The hole is the case where nothing arrives to be refused. A
dropped filter looks identical to a question that never had one, so the one
remaining witness is the user's own words.

The test is deliberately narrow and fails toward saying "I cannot answer that":
a false positive costs one unanswered question, and a false negative is a
confidently wrong total with the user's question printed above it.
*/

// subjectPrepositions introduce the thing a question is about. "in" is
// excluded on purpose: it introduces a period far more often than a subject
// ("in July", "in the last month"), and this check must never swallow those.
var subjectPrepositions = []string{" on ", " for "}

// timeWords are what can follow "on" or "for" without naming a subject. A
// question about a *when* is not a question about a *what*, and the period is
// already carried by the query.
var timeWords = map[string]bool{
	"today": true, "yesterday": true, "tomorrow": true, "this": true,
	"last": true, "the": true, "each": true, "every": true, "past": true,
	"average": true, "an": true, "a": true, "my": true, "me": true,
	"monday": true, "tuesday": true, "wednesday": true, "thursday": true,
	"friday": true, "saturday": true, "sunday": true, "weekend": true,
	"weekends": true, "weekday": true, "weekdays": true,
	"january": true, "february": true, "march": true, "april": true,
	"may": true, "june": true, "july": true, "august": true,
	"september": true, "october": true, "november": true, "december": true,
	"jan": true, "feb": true, "mar": true, "apr": true, "jun": true,
	"jul": true, "aug": true, "sep": true, "sept": true, "oct": true,
	"nov": true, "dec": true,
}

// questionSubjectWasDropped reports whether the user's words point at
// something the executed query does not filter on.
//
// It only ever fires when the query carries no subject filter at all — no
// category, no merchant, no payment mode. A question that was understood keeps
// at least one of those, so a correctly parsed "how much on food this month"
// never reaches the word matching below.
func questionSubjectWasDropped(transcript string, question ledgerQuestion) bool {
	if question.Metric == metricUnsupported {
		return false
	}
	// Something was filtered on, so nothing was silently discarded.
	if question.Category != "" || question.Merchant != "" || question.Mode != "" {
		return false
	}

	text := strings.ToLower(strings.TrimSpace(transcript))
	if text == "" {
		return false
	}
	// Padded so a preposition at the very start or end still matches, and so
	// "onions" can never be read as "on".
	padded := " " + strings.Join(strings.Fields(text), " ") + " "

	for _, preposition := range subjectPrepositions {
		index := strings.LastIndex(padded, preposition)
		if index < 0 {
			continue
		}
		if namesASubject(padded[index+len(preposition):]) {
			return true
		}
	}
	return false
}

// namesASubject decides whether what follows a preposition is a thing rather
// than a time. Only the first word is examined: "kitchen utensils and items"
// is a subject on the strength of "kitchen" alone, and "the last two weeks" is
// a period on the strength of "the".
func namesASubject(tail string) bool {
	fields := strings.Fields(tail)
	if len(fields) == 0 {
		return false
	}
	word := strings.Trim(fields[0], ".,?!'\"()")
	if word == "" {
		return false
	}
	if timeWords[word] {
		return false
	}
	// A leading digit is a date or a count, never a subject: "on 5 October",
	// "for 3 months".
	if word[0] >= '0' && word[0] <= '9' {
		return false
	}
	return true
}
