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

// The "probably the same" tier, tried only on what the strict pass left over.
// None of these pairs is resolved automatically: each is shown beside the
// user's own entry and the user says whether it is the same purchase. They
// exist because the alternative — showing the bank line as missing *and* the
// entry as unbilled — invites a one-tap duplicate.
const (
	// probableDateWindowDays bounds a same-amount pair that the strict window
	// rejected: logged on the wrong day, or posted late across a weekend and a
	// holiday. It stays well short of a month so a subscription never pairs
	// with the previous month's charge.
	probableDateWindowDays = 10
	// probableRoundingTolerance is one rupee — a bill rounded on one side only.
	probableRoundingTolerance = models.Money(100)
	// probableForexRatio covers a foreign purchase logged at the quoted price
	// and billed with the issuer's markup and GST on top. It only applies when
	// the descriptions share a word, because near-equal small amounts are
	// common and mostly unrelated.
	probableForexRatio = 0.035
)

const (
	probableReasonDate   = "date"
	probableReasonAmount = "amount"
)

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
	// OutsideCycle marks an entry dated just before or after the cycle. It is
	// loaded so a purchase logged on the day it happened can still match a
	// line the bank posted into this cycle, but it is never reported as extra:
	// it belongs to a neighbouring bill.
	OutsideCycle bool `json:"outside_cycle,omitempty"`
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

// probablePair is a statement line that is probably an entry the user
// already logged, differently enough that the strict pass would not commit.
type probablePair struct {
	Line   statementLine `json:"line"`
	Entry  ledgerLine    `json:"entry"`
	DayGap int           `json:"day_gap"`
	// AmountGap is line minus entry: positive when the bank billed more.
	AmountGap  models.Money `json:"amount_gap"`
	Similarity float64      `json:"similarity"`
	// Reason is what differs: "date" (same amount, further apart) or
	// "amount" (close in time, slightly different amount).
	Reason string `json:"reason"`
}

// statementDiff is the whole comparison.
type statementDiff struct {
	Matched []matchedPair `json:"matched"`
	// Probable pairs are excluded from Missing and Extra. Importing one is the
	// user saying "no, that was a different purchase".
	Probable []probablePair  `json:"probable"`
	Missing  []statementLine `json:"missing"`
	Extra    []ledgerLine    `json:"extra"`
	// Ignored holds lines that are real but must not become transactions —
	// bill payments, which Finnri tracks on the statement rather than as card
	// entries. Surfaced so the user can see they were considered.
	Ignored []statementLine `json:"ignored"`
	// Charges are the bank's own fees and interest, wherever they landed —
	// missing, probable or already tracked. They are surfaced together because
	// an unexpected annual fee or late charge is worth a look on its own.
	Charges []statementLine    `json:"charges"`
	Summary statementDiffTotal `json:"summary"`
	// Reconciliation is Finnri's ledger against this bill before anything is
	// imported, so the screen can say what importing will leave unexplained.
	Reconciliation *statementReconciliation `json:"reconciliation,omitempty"`
	// Whether the rows add up to the bill. Absent while the bill has no amount
	// yet. A mismatch is a review warning, never a reason to hide or reject
	// the parsed rows.
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
	// Payments are the bill payments found among the rows, kept out of
	// ParsedNet and used to work out what the purchases should total.
	Payments models.Money `json:"payments"`
	// OpeningBalance is the previous bill's total, when Finnri has one.
	OpeningBalance *models.Money `json:"opening_balance,omitempty"`
	ExpectedNet    models.Money  `json:"expected_net"`
	Difference     models.Money  `json:"difference"`
	Matches        bool          `json:"matches"`
	Message        string        `json:"message"`
}

type statementDiffTotal struct {
	StatementLines int `json:"statement_lines"`
	MatchedCount   int `json:"matched_count"`
	MissingCount   int `json:"missing_count"`
	ExtraCount     int `json:"extra_count"`
	IgnoredCount   int `json:"ignored_count"`
	ProbableCount  int `json:"probable_count"`
	ChargesCount   int `json:"charges_count"`
	// MissingAmount is what importing everything in Missing would add.
	MissingAmount models.Money `json:"missing_amount"`
	ExtraAmount   models.Money `json:"extra_amount"`
	ChargesAmount models.Money `json:"charges_amount"`
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
		Matched:  []matchedPair{},
		Missing:  []statementLine{},
		Extra:    []ledgerLine{},
		Ignored:  []statementLine{},
		Probable: []probablePair{},
		Charges:  []statementLine{},
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
		if line.Kind == lineKindFee || line.Kind == lineKindInterest {
			diff.Charges = append(diff.Charges, line)
			diff.Summary.ChargesAmount += line.Amount
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

	diff.Probable = pairProbable(matchable, entries, usedLines, usedEntries)

	for index, line := range matchable {
		if !usedLines[index] {
			diff.Missing = append(diff.Missing, line)
			diff.Summary.MissingAmount += line.Amount
		}
	}
	for index, entry := range entries {
		if !usedEntries[index] && !entry.OutsideCycle {
			diff.Extra = append(diff.Extra, entry)
			diff.Summary.ExtraAmount += entry.Amount
		}
	}

	diff.Summary.StatementLines = len(lines)
	diff.Summary.MatchedCount = len(diff.Matched)
	diff.Summary.MissingCount = len(diff.Missing)
	diff.Summary.ExtraCount = len(diff.Extra)
	diff.Summary.IgnoredCount = len(diff.Ignored)
	diff.Summary.ProbableCount = len(diff.Probable)
	diff.Summary.ChargesCount = len(diff.Charges)
	return diff
}

// pairProbable runs the second, looser pass over whatever the strict pass left
// unpaired, and marks what it pairs as used so neither side is reported again
// as missing or extra.
//
// Ranking prefers an identical amount over a near one, then the smaller
// amount difference, then the closer date, then the shared words.
func pairProbable(lines []statementLine, entries []ledgerLine, usedLines, usedEntries map[int]bool) []probablePair {
	type candidate struct {
		lineIndex, entryIndex int
		pair                  probablePair
	}
	candidates := []candidate{}
	for lineIndex, line := range lines {
		if usedLines[lineIndex] {
			continue
		}
		for entryIndex, entry := range entries {
			if usedEntries[entryIndex] || line.isCredit() != strings.EqualFold(entry.Type, "income") {
				continue
			}
			gap, ok := dayGap(line.Date, entry.Date)
			if !ok {
				continue
			}
			similarity := describeSimilarity(line.Description, entry.Title+" "+entry.Merchant)
			amountGap := line.Amount - entry.Amount
			reason := ""
			switch {
			case amountGap == 0 && gap <= probableDateWindowDays:
				reason = probableReasonDate
			case gap <= matchDateWindowDays && absMoney(amountGap) <= probableRoundingTolerance:
				reason = probableReasonAmount
			case gap <= matchDateWindowDays && similarity > 0 &&
				float64(absMoney(amountGap)) <= probableForexRatio*float64(line.Amount):
				reason = probableReasonAmount
			}
			if reason == "" {
				continue
			}
			candidates = append(candidates, candidate{lineIndex, entryIndex, probablePair{
				Line: line, Entry: entry, DayGap: gap, AmountGap: amountGap,
				Similarity: similarity, Reason: reason,
			}})
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := candidates[i].pair, candidates[j].pair
		if (left.AmountGap == 0) != (right.AmountGap == 0) {
			return left.AmountGap == 0
		}
		if absMoney(left.AmountGap) != absMoney(right.AmountGap) {
			return absMoney(left.AmountGap) < absMoney(right.AmountGap)
		}
		if left.DayGap != right.DayGap {
			return left.DayGap < right.DayGap
		}
		return left.Similarity > right.Similarity
	})
	pairs := []probablePair{}
	for _, pick := range candidates {
		if usedLines[pick.lineIndex] || usedEntries[pick.entryIndex] {
			continue
		}
		usedLines[pick.lineIndex] = true
		usedEntries[pick.entryIndex] = true
		pairs = append(pairs, pick.pair)
	}
	return pairs
}

func absMoney(value models.Money) models.Money {
	if value < 0 {
		return -value
	}
	return value
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
