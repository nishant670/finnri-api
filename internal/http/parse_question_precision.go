package http

import (
	"regexp"
	"strconv"
	"strings"
	timepkg "time"
	"unicode"

	"finnri/internal/database"
)

/*
Answering the question that was asked, not a nearby one.

Two answers from device testing (2026-11-06) were wrong without anything
being broken:

  - "How much spent on fuel till now??" came back as every Transport row ever —
    e-rickshaws, cabs and metro recharges included — because the prompt told the
    model "petrol and cabs are Transport", and Transport was the only filter a
    question could carry for it. The card read "Transport · 27 transactions",
    which is true and not what was asked.
  - "09 October spends" came back as all of October, ₹2,45,685 over 136 rows,
    because the period vocabulary had a month and no single day. The model
    picked the nearest thing it had.

So a question can now carry keywords — the item's own words, matched against
title, merchant and notes the way the list's search box matches them — and a
period can be one day. And because the second failure is one the user's words
settle beyond doubt, the server reads an explicit date out of the transcript and
holds the model to it.
*/

// searchAlternativeSeparator splits a list search into alternatives: a row
// matching any of them is a match. The answer card hands its keywords to the
// transaction list joined this way, so the list opens on the rows it counted.
const searchAlternativeSeparator = "|"

const (
	maxQuestionKeywords      = 6
	maxQuestionKeywordRunes  = 40
	maxSearchAlternatives    = 8
	maxQuestionSubjectLength = 40
)

// cleanQuestionKeyword makes a model-supplied word safe to search with: one
// line, lower case, no separator and no LIKE wildcards, and short.
func cleanQuestionKeyword(raw string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case '|', '%', '_', '"', '\\':
			return ' '
		}
		if unicode.IsControl(r) {
			return ' '
		}
		return unicode.ToLower(r)
	}, raw)
	cleaned = strings.Trim(strings.Join(strings.Fields(cleaned), " "), ".,?!'()")
	runes := []rune(cleaned)
	if len(runes) < 2 || len(runes) > maxQuestionKeywordRunes {
		return ""
	}
	return cleaned
}

// normalizeQuestionKeywords reads the model's keyword list, keeping at most a
// handful of distinct words. A keyword that already contains a shorter one is
// dropped: "petrol" matches every row "petrol pump" would.
func normalizeQuestionKeywords(raw any) []string {
	var candidates []string
	switch value := raw.(type) {
	case string:
		candidates = []string{value}
	case []any:
		for _, item := range value {
			if text, ok := item.(string); ok {
				candidates = append(candidates, text)
			}
		}
	case []string:
		candidates = value
	}

	keywords := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		keyword := cleanQuestionKeyword(candidate)
		if keyword == "" {
			continue
		}
		keywords = append(keywords, keyword)
	}

	kept := make([]string, 0, len(keywords))
	for i, keyword := range keywords {
		redundant := false
		for j, other := range keywords {
			if i == j {
				continue
			}
			if other == keyword && j < i {
				redundant = true
				break
			}
			if other != keyword && strings.Contains(keyword, other) {
				redundant = true
				break
			}
		}
		if !redundant {
			kept = append(kept, keyword)
		}
		if len(kept) == maxQuestionKeywords {
			break
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

// questionSubjectLabel is what the card calls the thing: the model's short
// name for it when it gave one, otherwise the first keyword, capitalised.
func questionSubjectLabel(raw any, keywords []string) string {
	if len(keywords) == 0 {
		return ""
	}
	label, _ := raw.(string)
	label = strings.Join(strings.Fields(strings.NewReplacer("|", " ", "\n", " ").Replace(label)), " ")
	if label == "" || len([]rune(label)) > maxQuestionSubjectLength {
		label = keywords[0]
	}
	runes := []rune(label)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

// keywordSearch is the list search for a set of keywords. A lone short word
// keeps a trailing separator so the list matches it as a whole word too:
// "emi" must not count "premium".
func keywordSearch(keywords []string) string {
	search := strings.Join(keywords, searchAlternativeSeparator)
	if len(keywords) == 1 && len([]rune(keywords[0])) <= maxWholeWordTermRunes {
		search += searchAlternativeSeparator
	}
	return search
}

// maxWholeWordTermRunes is the length at or under which an alternative is
// matched as a whole word. "ola" inside "coca cola", "cab" inside "cabbage"
// and "cng" inside nothing useful are the reason; longer words are matched
// anywhere, as the search box always has.
const maxWholeWordTermRunes = 3

// searchTermClause is the condition one alternative adds to a list search.
func searchTermClause(term string) (string, []any) {
	term = strings.ToLower(term)
	columns := []string{"entries.title", "entries.merchant", "entries.notes"}
	clauses := make([]string, 0, len(columns))
	args := make([]any, 0, len(columns)*4)
	if len([]rune(term)) <= maxWholeWordTermRunes {
		for _, column := range columns {
			clauses = append(clauses, "LOWER("+column+") = ? OR LOWER("+column+") LIKE ? OR LOWER("+column+") LIKE ? OR LOWER("+column+") LIKE ?")
			args = append(args, term, term+" %", "% "+term, "% "+term+" %")
		}
	} else {
		pattern := "%" + term + "%"
		for _, column := range columns {
			clauses = append(clauses, "LOWER("+column+") LIKE ?")
			args = append(args, pattern)
		}
	}
	return strings.Join(clauses, " OR "), args
}

// searchAlternatives splits a list search into the terms any one of which
// makes a match. A search without the separator is one term, as it always was.
func searchAlternatives(search string) []string {
	if !strings.Contains(search, searchAlternativeSeparator) {
		return []string{search}
	}
	terms := []string{}
	for _, part := range strings.Split(search, searchAlternativeSeparator) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		terms = append(terms, part)
		if len(terms) == maxSearchAlternatives {
			break
		}
	}
	return terms
}

// resolveUserQuestionCategory settles a category the model named that is not
// built in. If the user made a category by that name, the question is about
// it. Otherwise the word is searched for, so "how much on crypto" counts the
// rows that mention crypto instead of refusing or answering about Misc.
func resolveUserQuestionCategory(userID uint, question *ledgerQuestion) {
	unknown := question.UnknownCategory
	question.UnknownCategory = ""
	if unknown == "" || question.Metric == metricUnsupported {
		return
	}

	if database.DB != nil {
		if custom, err := customCategoriesFor(database.DB, userID); err == nil {
			for _, list := range [][]string{custom.Expense, custom.Income} {
				for _, name := range list {
					if strings.EqualFold(strings.TrimSpace(name), unknown) {
						question.Category = name
						return
					}
				}
			}
		}
	}

	if question.Merchant == "" && len(question.Keywords) == 0 {
		keywords := keywordFamilies[unknown]
		if keywords == nil {
			keywords = []string{unknown}
		}
		question.Keywords = normalizeQuestionKeywords(keywords)
		question.SubjectLabel = questionSubjectLabel(unknown, question.Keywords)
	}
}

// preferUserCategory answers an item question from the user's own category
// when they made one for it. Someone who files fuel under a "Fuel" category
// has said which rows are fuel, more reliably than any title search can.
func preferUserCategory(userID uint, question *ledgerQuestion) {
	if question.Metric == metricUnsupported || question.Category != "" ||
		question.Merchant != "" || len(question.Keywords) == 0 || database.DB == nil {
		return
	}
	custom, err := customCategoriesFor(database.DB, userID)
	if err != nil {
		return
	}
	names := append([]string{question.SubjectLabel}, question.Keywords...)
	for _, list := range [][]string{custom.Expense, custom.Income} {
		for _, category := range list {
			for _, name := range names {
				if name != "" && strings.EqualFold(strings.TrimSpace(category), strings.TrimSpace(name)) {
					question.Category = category
					question.Keywords = nil
					question.SubjectLabel = ""
					return
				}
			}
		}
	}
}

// monthNumbers maps every way a month is written to its number.
var monthNumbers = map[string]timepkg.Month{
	"jan": 1, "january": 1, "feb": 2, "february": 2, "mar": 3, "march": 3,
	"apr": 4, "april": 4, "may": 5, "jun": 6, "june": 6, "jul": 7, "july": 7,
	"aug": 8, "august": 8, "sep": 9, "sept": 9, "september": 9,
	"oct": 10, "october": 10, "nov": 11, "november": 11, "dec": 12, "december": 12,
}

const monthAlternation = `january|february|march|april|may|june|july|august|september|sept|october|november|december|jan|feb|mar|apr|jun|jul|aug|sep|oct|nov|dec`

var (
	// "9 October", "09 oct", "9th of October", "9 Oct 2025".
	dayThenMonth = regexp.MustCompile(`\b(\d{1,2})(?:st|nd|rd|th)?\s*(?:of\s+)?(` + monthAlternation + `)\b\.?(?:,?\s*(\d{4})\b)?`)
	// "October 9", "oct 9th", "October 9, 2025".
	monthThenDay = regexp.MustCompile(`\b(` + monthAlternation + `)\b\.?\s*(\d{1,2})(?:st|nd|rd|th)?\b(?:,?\s*(\d{4})\b)?`)
	// "2026-10-09".
	isoDay = regexp.MustCompile(`\b(\d{4})-(\d{2})-(\d{2})\b`)
)

// rangeLeadIns, said right before a date, make it one end of a window rather
// than the window: "1 to 9 October", "since 9 Oct", "week of 9 October".
// They are only read next to the date — "paid to Swiggy on 9 October" is still
// a question about that one day.
var rangeLeadIns = []string{
	"to", "till", "until", "upto", "up to", "from", "since", "after", "before",
	"through", "and", "-", "–", "week of", "month of",
}

// rangeTails, said right after a date, do the same: "9 Oct onwards".
var rangeTails = []string{"to ", "till ", "until ", "upto ", "up to ", "onwards", "onward", "-", "–", "and "}

type spokenDayMatch struct {
	date  timepkg.Time
	start int
	end   int
}

// findSpokenDays lists every explicit calendar day in the text.
func findSpokenDays(text string, now timepkg.Time) []spokenDayMatch {
	today := timepkg.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	matches := []spokenDayMatch{}
	add := func(day, year int, month timepkg.Month, explicitYear bool, start, end int) {
		if month < 1 || month > 12 || day < 1 || day > 31 {
			return
		}
		date := timepkg.Date(year, month, day, 0, 0, 0, 0, now.Location())
		if date.Day() != day {
			// 31 September and friends.
			return
		}
		if !explicitYear && date.After(today) {
			date = date.AddDate(-1, 0, 0)
		}
		for _, existing := range matches {
			if start < existing.end && existing.start < end {
				return
			}
		}
		matches = append(matches, spokenDayMatch{date: date, start: start, end: end})
	}

	for _, index := range dayThenMonth.FindAllStringSubmatchIndex(text, -1) {
		day, _ := strconv.Atoi(text[index[2]:index[3]])
		month := monthNumbers[text[index[4]:index[5]]]
		year, explicit := today.Year(), false
		if index[6] >= 0 {
			year, _ = strconv.Atoi(text[index[6]:index[7]])
			explicit = true
		}
		add(day, year, month, explicit, index[0], index[1])
	}
	for _, index := range monthThenDay.FindAllStringSubmatchIndex(text, -1) {
		month := monthNumbers[text[index[2]:index[3]]]
		day, _ := strconv.Atoi(text[index[4]:index[5]])
		year, explicit := today.Year(), false
		if index[6] >= 0 {
			year, _ = strconv.Atoi(text[index[6]:index[7]])
			explicit = true
		}
		add(day, year, month, explicit, index[0], index[1])
	}
	for _, index := range isoDay.FindAllStringSubmatchIndex(text, -1) {
		year, _ := strconv.Atoi(text[index[2]:index[3]])
		month, _ := strconv.Atoi(text[index[4]:index[5]])
		day, _ := strconv.Atoi(text[index[6]:index[7]])
		add(day, year, timepkg.Month(month), true, index[0], index[1])
	}
	return matches
}

// spokenDay is the one calendar day a question names, when it names exactly
// one and is not describing a range around it.
func spokenDay(transcript string, now timepkg.Time) (spokenDayMatch, string, bool) {
	text := strings.ToLower(strings.Join(strings.Fields(transcript), " "))
	if text == "" {
		return spokenDayMatch{}, text, false
	}
	if containsWord(text, "between") {
		return spokenDayMatch{}, text, false
	}
	matches := findSpokenDays(text, now)
	if len(matches) != 1 {
		return spokenDayMatch{}, text, false
	}
	match := matches[0]
	before := strings.TrimSpace(text[:match.start])
	for _, leadIn := range rangeLeadIns {
		if before == leadIn || strings.HasSuffix(before, " "+leadIn) ||
			((leadIn == "-" || leadIn == "–") && strings.HasSuffix(before, leadIn)) {
			return spokenDayMatch{}, text, false
		}
	}
	after := strings.TrimSpace(text[match.end:])
	for _, tail := range rangeTails {
		if strings.HasPrefix(after+" ", tail) {
			return spokenDayMatch{}, text, false
		}
	}
	return match, text, true
}

// applySpokenDay holds the question to the day the user named. "09 October
// spends" answered as all of October is a different question with a much
// bigger number, and the date in the text is not open to interpretation.
func applySpokenDay(transcript string, question ledgerQuestion, now timepkg.Time) ledgerQuestion {
	if question.Metric == metricUnsupported {
		return question
	}
	match, _, ok := spokenDay(transcript, now)
	if !ok {
		return question
	}
	date := match.date.Format("2006-01-02")
	if question.StartDate == date && question.EndDate == date {
		return question
	}
	question.PeriodKind, question.PeriodLabel, question.StartDate, question.EndDate =
		resolveQuestionPeriod(map[string]any{"kind": "day", "start": date}, now)
	return question
}

// broadCategoryWords are the words that name a whole category rather than
// something inside it. "food" is Food & Drinks; "groceries" is a part of it,
// and totalling the whole category for it answers a bigger question than the
// one asked — the "fuel" answer that counted every rickshaw was this mistake.
var broadCategoryWords = map[string]bool{
	"food": true, "food & drink": true, "food and drinks": true, "food and drink": true,
	"transportation": true, "commute": true,
	"trip": true, "trips": true, "vacation": true, "holiday": true,
	"bill":   true,
	"family": true, "gift": true, "gifts": true,
	"miscellaneous": true,
	"salary income": true, "wages": true, "freelancing": true,
	"interest income": true, "dividends": true, "dividend income": true,
}

// broadCategory resolves a word to a category only when the word names the
// whole category: a built-in name, or one of the broad synonyms above.
//
// Names are compared on their letters alone, so "Family & Gifts", "family
// gifts" and "Food and Drinks" find Family/Gifts and Food & Drinks.
func broadCategory(value string) (string, bool) {
	normalized := strings.ToLower(strings.Join(strings.Fields(value), " "))
	if normalized == "" {
		return "", false
	}
	key := categoryLetters(normalized)
	for _, list := range [][]string{canonicalCategories, canonicalIncomeCategories} {
		for _, category := range list {
			if categoryLetters(category) == key {
				return category, true
			}
		}
	}
	for word := range broadCategoryWords {
		if categoryLetters(word) == key {
			return canonicalCategory(word)
		}
	}
	return "", false
}

// categoryLetters keeps the letters of a category name, dropping "and".
func categoryLetters(value string) string {
	words := strings.FieldsFunc(strings.ToLower(value), func(r rune) bool { return !unicode.IsLetter(r) })
	kept := words[:0]
	for _, word := range words {
		if word != "and" {
			kept = append(kept, word)
		}
	}
	return strings.Join(kept, "")
}

// categoryNamedIn finds a whole category the user's words name, for a query
// that came back filtering on nothing: "transport this month" is a question
// about Transport, whatever the model left out. "Other" is never read this
// way — the word is far too common to mean the category.
func categoryNamedIn(transcript string) (string, bool) {
	text := strings.ToLower(strings.Join(strings.Fields(transcript), " "))
	if text == "" {
		return "", false
	}
	candidates := make([]string, 0, len(broadCategoryWords)+16)
	for word := range broadCategoryWords {
		candidates = append(candidates, word)
	}
	for _, list := range [][]string{canonicalCategories, canonicalIncomeCategories} {
		for _, category := range list {
			if category == defaultIncomeCategory {
				continue
			}
			candidates = append(candidates, strings.ToLower(category))
		}
	}
	candidates = append(candidates, "bills", "family gifts", "food drinks")
	// Longest first, so "food and drinks" is not read as "food" and stop.
	best := ""
	for _, candidate := range candidates {
		if len(candidate) > len(best) && containsWord(text, candidate) {
			best = candidate
		}
	}
	if best == "" {
		return "", false
	}
	if best == "family gifts" {
		return "Family/Gifts", true
	}
	if best == "food drinks" {
		return "Food & Drinks", true
	}
	return canonicalCategory(best)
}

// applyNamedCategory restores a category the user named and the query lost.
func applyNamedCategory(transcript string, question *ledgerQuestion) {
	if question.Metric == metricUnsupported {
		return
	}
	if question.Category != "" || question.Merchant != "" || question.Mode != "" ||
		len(question.Keywords) > 0 || question.UnknownCategory != "" {
		return
	}
	// Net is income against spending; one category would make it either.
	if question.Metric == metricNet {
		return
	}
	category, ok := categoryNamedIn(transcript)
	if !ok {
		return
	}
	// A category only answers a question pointed the same way: "salary" on a
	// spending total would count nothing and look like an answer.
	_, isIncome := canonicalCategoryForType(category, "income")
	if questionType := question.questionFilterValues().Get("type"); questionType != "" &&
		(questionType == "income") != isIncome {
		return
	}
	question.Category = category
}

// keywordFamilies widen a narrow word to the spellings the same purchase is
// usually logged under, for the server-side narrowing below.
var keywordFamilies = map[string][]string{
	"fuel":      {"fuel", "petrol", "diesel", "cng"},
	"petrol":    {"fuel", "petrol", "diesel", "cng"},
	"diesel":    {"fuel", "petrol", "diesel", "cng"},
	"cab":       {"cab", "cabs", "taxi", "uber", "ola", "rapido"},
	"cabs":      {"cab", "cabs", "taxi", "uber", "ola", "rapido"},
	"taxi":      {"cab", "cabs", "taxi", "uber", "ola", "rapido"},
	"auto":      {"auto", "rickshaw"},
	"rickshaw":  {"auto", "rickshaw"},
	"groceries": {"grocery", "groceries", "kirana", "vegetables"},
	"grocery":   {"grocery", "groceries", "kirana", "vegetables"},
	"movies":    {"movie", "cinema", "pvr", "inox"},
	"movie":     {"movie", "cinema", "pvr", "inox"},
	"flights":   {"flight", "airline", "indigo", "air india"},
	"flight":    {"flight", "airline", "indigo", "air india"},
}

// applyNarrowSubject undoes a category the model widened a narrow word into.
// "How much spent on fuel" filed under Transport counts every rickshaw and
// metro recharge; when the user said "fuel" and never said "transport", the
// question is about fuel, and fuel is what gets searched for.
func applyNarrowSubject(transcript string, question *ledgerQuestion) {
	if question.Metric == metricUnsupported || question.Category == "" || question.Merchant != "" {
		return
	}
	if named, ok := categoryNamedIn(transcript); ok && named == question.Category {
		return
	}
	// Keywords and a category the user never said: the category only hides
	// the item's rows filed anywhere else — petrol logged under Misc.
	if len(question.Keywords) > 0 {
		if _, builtIn := broadCategory(question.Category); builtIn {
			question.Category = ""
		}
		return
	}
	text := strings.ToLower(strings.Join(strings.Fields(transcript), " "))
	narrow := ""
	for alias, category := range categoryAliases {
		if category != question.Category || broadCategoryWords[alias] {
			continue
		}
		if len(alias) > len(narrow) && containsWord(text, alias) {
			narrow = alias
		}
	}
	if narrow == "" {
		return
	}
	keywords := keywordFamilies[narrow]
	if keywords == nil {
		keywords = []string{narrow}
	}
	question.Category = ""
	question.Keywords = normalizeQuestionKeywords(keywords)
	question.SubjectLabel = questionSubjectLabel(narrow, question.Keywords)
}
