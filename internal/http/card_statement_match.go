package http

import (
	"sort"
	"strings"
	"unicode"

	"finnri/internal/models"
)

/*
Matching a statement's line items against the ledger.

The valuable output of reading a statement is not "import my transactions" —
it is **the diff**. Which of these did I already log, which did I miss, and
what does Finnri hold that the bank did not bill?

Three rules shape the algorithm:

 1. The amount must match exactly. Amounts on a card statement are effectively
    unique within a cycle, and a near-match is a different transaction, not a
    fuzzy version of the same one.
 2. Dates are allowed to drift. A transaction posts to the statement a day or
    three after it happened, and the user logged it when it happened.
 3. Descriptions only break ties. "SWIGGY BANGALORE IN" and a user's "Dinner"
    are the same purchase; refusing to match them because the words differ
    would bury the user in false "missing" rows.

Nothing is ever resolved automatically. Every bucket is shown and the user
chooses — the ledger is their own record, and deleting from it to make a diff
tidy is not this system's call.
*/

// matchDateWindowDays is how far a statement line may sit from the ledger
// entry it belongs to. Three days covers weekend posting delays without
// letting a card's monthly subscription match the previous month's.
const matchDateWindowDays = 3

// Statement line kinds. The distinction that matters most is `payment`: a
// credit for settling the bill is not a refund and must never be imported, so
// it is classified out of the diff entirely.
const (
	lineKindSpend    = "spend"
	lineKindRefund   = "refund"
	lineKindPayment  = "payment"
	lineKindFee      = "fee"
	lineKindInterest = "interest"
	lineKindEMI      = "emi"
)

// statementLine is one row off the bank's statement, from whatever source.
type statementLine struct {
	Date        string       `json:"date"`
	Description string       `json:"description"`
	Amount      models.Money `json:"amount"`
	// "expense" for a debit, "income" for a credit. Defaults to expense.
	Type string `json:"type"`
	// Kind is derived, not supplied.
	Kind string `json:"kind,omitempty"`
}

func (line statementLine) isCredit() bool {
	return strings.EqualFold(strings.TrimSpace(line.Type), "income")
}

// ledgerLine is one of Finnri's own entries, reduced to what matching needs.
type ledgerLine struct {
	EntryID  uint         `json:"entry_id"`
	Date     string       `json:"date"`
	Title    string       `json:"title"`
	Merchant string       `json:"merchant"`
	Category string       `json:"category"`
	Amount   models.Money `json:"amount"`
	Type     string       `json:"type"`
	Tag      string       `json:"tag"`
}

// matchedPair is a statement line and the entry it belongs to.
type matchedPair struct {
	Line   statementLine `json:"line"`
	Entry  ledgerLine    `json:"entry"`
	DayGap int           `json:"day_gap"`
	// Similarity is 0..1 over description tokens. Low is normal and fine — the
	// amount and date did the work.
	Similarity float64 `json:"similarity"`
}

// statementDiff is the whole comparison.
type statementDiff struct {
	Matched []matchedPair   `json:"matched"`
	Missing []statementLine `json:"missing"`
	Extra   []ledgerLine    `json:"extra"`
	// Ignored holds lines that are real but must not become transactions —
	// bill payments, which Finnri tracks on the statement rather than as card
	// entries. Surfaced so the user can see they were considered.
	Ignored []statementLine    `json:"ignored"`
	Summary statementDiffTotal `json:"summary"`
	// Present only for AI screenshot intake. A mismatch is a review warning,
	// never a reason to hide or reject the parsed rows.
	Checksum *statementChecksum `json:"checksum,omitempty"`
	Source   string             `json:"source,omitempty"`

	CreditsCharged        *int `json:"credits_charged,omitempty"`
	CreditsRemainingToday *int `json:"credits_remaining_today,omitempty"`
	CreditsRemainingTotal *int `json:"credits_remaining_total,omitempty"`
}

type statementChecksum struct {
	ParsedDebits  models.Money `json:"parsed_debits"`
	ParsedCredits models.Money `json:"parsed_credits"`
	ParsedNet     models.Money `json:"parsed_net"`
	ExpectedNet   models.Money `json:"expected_net"`
	Difference    models.Money `json:"difference"`
	Matches       bool         `json:"matches"`
	Message       string       `json:"message"`
}

type statementDiffTotal struct {
	StatementLines int `json:"statement_lines"`
	MatchedCount   int `json:"matched_count"`
	MissingCount   int `json:"missing_count"`
	ExtraCount     int `json:"extra_count"`
	IgnoredCount   int `json:"ignored_count"`
	// MissingAmount is what importing everything in Missing would add.
	MissingAmount models.Money `json:"missing_amount"`
	ExtraAmount   models.Money `json:"extra_amount"`
}

// classifyLine works out what a statement row actually is.
//
// Getting `payment` right is the point. A card payment appears on the
// statement as a credit, but Finnri deliberately does not write payments
// against the card — outstanding comes from the statement, not from ledger
// arithmetic. Importing one as an income entry would silently reduce the
// card's outstanding a second time.
func classifyLine(line statementLine) string {
	normalized := strings.ToUpper(line.Description)

	if line.isCredit() {
		for _, marker := range []string{
			"PAYMENT RECEIVED", "PAYMENT - THANK", "PAYMENT THANK", "THANK YOU",
			"AUTOPAY", "AUTO DEBIT", "AUTO-DEBIT", "NEFT CR", "IMPS CR",
			"BILL PAYMENT", "PMT RECEIVED", "RECEIVED - THANK",
		} {
			if strings.Contains(normalized, marker) {
				return lineKindPayment
			}
		}
		return lineKindRefund
	}

	for _, marker := range []string{"INTEREST", "FINANCE CHARGE", "FIN CHARGE"} {
		if strings.Contains(normalized, marker) {
			return lineKindInterest
		}
	}
	for _, marker := range []string{
		"FEE", "CHARGE", "GST", "TAX", "SURCHARGE", "PENALTY", "LATE PAY",
		"ANNUAL", "MARKUP",
	} {
		if strings.Contains(normalized, marker) {
			return lineKindFee
		}
	}
	if strings.Contains(normalized, "EMI") || strings.Contains(normalized, "INSTALMENT") ||
		strings.Contains(normalized, "INSTALLMENT") {
		return lineKindEMI
	}
	return lineKindSpend
}

// diffStatementLines produces the three buckets.
//
// Assignment is greedy over candidate pairs sorted by quality: exact amount
// and same day beats exact amount and three days apart, and a shared merchant
// name breaks any remaining tie. Each line and each entry is used at most
// once, so two identical ₹250 coffees on the same day match the two ₹250
// entries rather than both matching the first.
func diffStatementLines(lines []statementLine, entries []ledgerLine) statementDiff {
	diff := statementDiff{
		Matched: []matchedPair{},
		Missing: []statementLine{},
		Extra:   []ledgerLine{},
		Ignored: []statementLine{},
	}

	// Payments are set aside before matching: they are not spending, and they
	// have no counterpart in the ledger by design.
	matchable := make([]statementLine, 0, len(lines))
	for _, line := range lines {
		line.Kind = classifyLine(line)
		if line.Kind == lineKindPayment {
			diff.Ignored = append(diff.Ignored, line)
			continue
		}
		matchable = append(matchable, line)
	}

	type candidate struct {
		lineIndex  int
		entryIndex int
		dayGap     int
		similarity float64
	}

	candidates := []candidate{}
	for lineIndex, line := range matchable {
		for entryIndex, entry := range entries {
			if line.Amount != entry.Amount {
				continue
			}
			// A debit cannot be a credit. Without this a ₹2,000 refund would
			// happily match a ₹2,000 purchase.
			if line.isCredit() != strings.EqualFold(entry.Type, "income") {
				continue
			}
			gap, ok := dayGap(line.Date, entry.Date)
			if !ok || gap > matchDateWindowDays {
				continue
			}
			candidates = append(candidates, candidate{
				lineIndex:  lineIndex,
				entryIndex: entryIndex,
				dayGap:     gap,
				similarity: describeSimilarity(line.Description, entry.Title+" "+entry.Merchant),
			})
		}
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].dayGap != candidates[j].dayGap {
			return candidates[i].dayGap < candidates[j].dayGap
		}
		return candidates[i].similarity > candidates[j].similarity
	})

	usedLines := make(map[int]bool, len(matchable))
	usedEntries := make(map[int]bool, len(entries))
	for _, pick := range candidates {
		if usedLines[pick.lineIndex] || usedEntries[pick.entryIndex] {
			continue
		}
		usedLines[pick.lineIndex] = true
		usedEntries[pick.entryIndex] = true
		diff.Matched = append(diff.Matched, matchedPair{
			Line:       matchable[pick.lineIndex],
			Entry:      entries[pick.entryIndex],
			DayGap:     pick.dayGap,
			Similarity: pick.similarity,
		})
	}

	for index, line := range matchable {
		if !usedLines[index] {
			diff.Missing = append(diff.Missing, line)
			diff.Summary.MissingAmount += line.Amount
		}
	}
	for index, entry := range entries {
		if !usedEntries[index] {
			diff.Extra = append(diff.Extra, entry)
			diff.Summary.ExtraAmount += entry.Amount
		}
	}

	diff.Summary.StatementLines = len(lines)
	diff.Summary.MatchedCount = len(diff.Matched)
	diff.Summary.MissingCount = len(diff.Missing)
	diff.Summary.ExtraCount = len(diff.Extra)
	diff.Summary.IgnoredCount = len(diff.Ignored)
	return diff
}

// dayGap is the absolute distance in days between two dates.
func dayGap(left, right string) (int, bool) {
	leftDate, err := parseAPIDate(left)
	if err != nil {
		return 0, false
	}
	rightDate, err := parseAPIDate(right)
	if err != nil {
		return 0, false
	}
	hours := leftDate.Sub(rightDate).Hours() / 24
	if hours < 0 {
		hours = -hours
	}
	return int(hours + 0.5), true
}

// describeSimilarity is token overlap between a statement description and what
// the user called the same purchase, as a fraction of the smaller token set.
//
// Only a tie-breaker. Bank descriptions are shouty and full of processor
// noise, so most true matches score low, and requiring a high score would
// reject them.
func describeSimilarity(left, right string) float64 {
	leftTokens := descriptionTokens(left)
	rightTokens := descriptionTokens(right)
	if len(leftTokens) == 0 || len(rightTokens) == 0 {
		return 0
	}

	shared := 0
	for token := range leftTokens {
		if rightTokens[token] {
			shared++
		}
	}

	smaller := len(leftTokens)
	if len(rightTokens) < smaller {
		smaller = len(rightTokens)
	}
	return float64(shared) / float64(smaller)
}

// descriptionNoise are tokens that appear on so many statement rows they carry
// no signal about which purchase a line is.
var descriptionNoise = map[string]bool{
	"upi": true, "pos": true, "neft": true, "imps": true, "atm": true,
	"pvt": true, "ltd": true, "limited": true, "india": true, "ind": true,
	"payment": true, "purchase": true, "txn": true, "ref": true, "card": true,
	"the": true, "and": true, "for": true, "www": true, "com": true,
}

// descriptionTokens lowercases, splits on anything non-alphanumeric, and drops
// short and noise tokens.
func descriptionTokens(value string) map[string]bool {
	tokens := map[string]bool{}
	for _, field := range strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len(field) < 3 || descriptionNoise[field] {
			continue
		}
		tokens[field] = true
	}
	return tokens
}
