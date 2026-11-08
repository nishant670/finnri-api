package http

import (
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ledongthuc/pdf"

	"finnri/internal/models"
)

/*
Putting a statement page back together from where its text sits.

A PDF has no rows or columns, only glyphs at positions. Reading it well comes
down to three decisions, and each one has already cost a real statement:

  - What shares a line. Baselines are compared with a tolerance, because cells
    of one table row can sit a fraction of a point apart.
  - What is a separate cell. Runs of text stay apart, three spaces between
    them, so a Reward Points column cannot fuse with the amount beside it
    ("11" and "527.00" must not become 11527.00).
  - What belongs to the table at all. Some issuers print a summary panel down
    the right edge, so a row's baseline can be shared with "Statement Date" or
    the bill's own dates. That panel is lifted out and written after the page,
    which keeps a transaction's date from being read as the statement's.
*/

// cellSeparator is what the reader writes between two cells of one line. The
// row parser splits on it, and the model reads it as a column gap.
const cellSeparator = "   "

var cellGap = regexp.MustCompile(`\s{3,}`)

// sidePanelLabel recognises the headings of a statement's summary panel.
var sidePanelLabel = regexp.MustCompile(`(?i)^(statement (date|period)|payment due date|due date|total amount due|minimum amount due|total outstanding|previous balance)`)

// textRun is a stretch of text printed in one go, in reading order.
type textRun struct {
	x, y, end, size float64
	text            string
}

func (run textRun) trimmed() string { return strings.TrimSpace(run.text) }

// positionedPageLines lays one page out from its glyph positions. The PDF
// library panics on some malformed content streams; a page it cannot read
// costs that page and not the statement.
func positionedPageLines(page pdf.Page) (lines []string) {
	defer func() {
		if recover() != nil {
			lines = nil
		}
	}()
	return layoutStatementPage(page.Content().Text)
}

// layoutStatementPage turns glyphs into lines of cells.
func layoutStatementPage(glyphs []pdf.Text) []string {
	runs := glyphRuns(glyphs)
	if len(runs) == 0 {
		return nil
	}
	rows := groupRunsIntoLines(runs)
	panelX := sidePanelStart(runs)

	lines := make([]string, 0, len(rows))
	panel := []string{}
	for _, row := range rows {
		main, side := row, []textRun(nil)
		if panelX > 0 {
			main, side = splitAtColumn(row, panelX)
		}
		// A date-led line only gives up its right-hand cells when what is
		// left is a whole transaction. Otherwise the amount itself sits under
		// the panel's heading, as on a full-width table.
		if len(main) > 0 && len(side) > 0 {
			if _, _, dated := takeLeadingDate(joinCells(main), 0); dated {
				if _, ok := parseStatementRow(joinCells(main), 0); !ok {
					main, side = row, nil
				}
			}
		}
		if len(main) > 0 {
			lines = append(lines, joinCells(main))
		}
		if len(side) > 0 {
			panel = append(panel, joinCells(side))
		}
	}
	return append(lines, panel...)
}

// glyphRuns joins glyphs into runs in the order the PDF drew them. A run ends
// where the next glyph jumps line, moves backwards, or leaves a column-sized
// gap. Sorting whole runs rather than glyphs matters: a font with unreliable
// widths would otherwise interleave the letters of neighbouring cells.
func glyphRuns(glyphs []pdf.Text) []textRun {
	runs := []textRun{}
	var current *textRun
	for _, glyph := range glyphs {
		if glyph.S == "" {
			continue
		}
		size := glyph.FontSize
		if size <= 0 {
			size = 8
		}
		gap := 0.0
		if current != nil {
			gap = glyph.X - current.end
		}
		if current == nil || absFloat(glyph.Y-current.y) > lineTolerance(size) ||
			gap < -size*0.5 || gap >= size*2 {
			runs = append(runs, textRun{x: glyph.X, y: glyph.Y, end: glyph.X, size: size})
			current = &runs[len(runs)-1]
		} else if gap > size*0.2 && !strings.HasSuffix(current.text, " ") && glyph.S != " " {
			current.text += " "
		}
		current.text += glyph.S
		if end := glyph.X + glyph.W; end > current.end {
			current.end = end
		}
	}
	kept := runs[:0]
	for _, run := range runs {
		if run.trimmed() != "" {
			kept = append(kept, run)
		}
	}
	return kept
}

// lineTolerance is how far apart two baselines can be and still be one line:
// enough for cells a fraction of a point apart, short of a wrapped cell's
// second line.
func lineTolerance(size float64) float64 {
	if tolerance := size * 0.3; tolerance > 1.5 {
		return tolerance
	}
	return 1.5
}

func groupRunsIntoLines(runs []textRun) [][]textRun {
	sorted := append([]textRun(nil), runs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].y > sorted[j].y })

	lines := [][]textRun{}
	var anchor float64
	for _, run := range sorted {
		if len(lines) == 0 || anchor-run.y > lineTolerance(run.size) {
			lines = append(lines, []textRun{})
			anchor = run.y
		}
		lines[len(lines)-1] = append(lines[len(lines)-1], run)
	}
	for _, line := range lines {
		sort.SliceStable(line, func(i, j int) bool { return line[i].x < line[j].x })
	}
	return lines
}

// sidePanelStart is the left edge of a summary panel on the right of the
// page, or 0 when the page has none. Two headings are required, so one stray
// "Due Date" column heading cannot claim a whole column of the table.
func sidePanelStart(runs []textRun) float64 {
	right := 0.0
	for _, run := range runs {
		if run.end > right {
			right = run.end
		}
	}
	left, labels := 0.0, 0
	for _, run := range runs {
		if run.x < right*0.55 || !sidePanelLabel.MatchString(run.trimmed()) {
			continue
		}
		labels++
		if left == 0 || run.x < left {
			left = run.x
		}
	}
	if labels < 2 {
		return 0
	}
	return left
}

func splitAtColumn(row []textRun, x float64) (main, side []textRun) {
	for _, run := range row {
		if run.x < x-3 {
			main = append(main, run)
		} else {
			side = append(side, run)
		}
	}
	return main, side
}

func joinCells(cells []textRun) string {
	parts := make([]string, 0, len(cells))
	for _, cell := range cells {
		parts = append(parts, collapseSpaces(cell.text))
	}
	return strings.Join(parts, cellSeparator)
}

func absFloat(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}

// --- the dates a statement prints --------------------------------------------

const printedDate = `\d{1,2}[/\-. ](?:\d{2}|[A-Za-z]{3})[/\-. ]\d{2,4}`

var (
	statementPeriodPattern = regexp.MustCompile(`(?i)(` + printedDate + `)\s*(?:to|till|until|-|–)\s*(` + printedDate + `)`)
	statementDateLabel     = regexp.MustCompile(`(?is)\bstatement\s+date\b[^0-9]{0,40}(` + printedDate + `)`)
	dueDateLabel           = regexp.MustCompile(`(?is)\b(?:payment\s+)?due\s+(?:date|by|on)\b[^0-9]{0,40}(` + printedDate + `)`)
)

// A monthly statement covers about a month; a range far from that is some
// other pair of dates that happens to be printed with a "to" between them.
const (
	minStatementPeriodDays = 20
	maxStatementPeriodDays = 45
	maxDaysToPayment       = 60
)

// statementDateFacts are the dates a statement states about itself, read
// straight from its text. Empty means not found.
type statementDateFacts struct {
	PeriodStart, PeriodEnd, StatementDate, DueDate string
}

func parsePrintedDate(value string) string {
	date, _, ok := takeLeadingDate(strings.TrimSpace(value), 0)
	if !ok {
		return ""
	}
	return date
}

func readStatementDateFacts(text string) statementDateFacts {
	facts := statementDateFacts{}
	for _, match := range statementPeriodPattern.FindAllStringSubmatch(text, -1) {
		start, end := parsePrintedDate(match[1]), parsePrintedDate(match[2])
		if start == "" || end == "" {
			continue
		}
		if span, ok := signedDayGap(start, end); ok && span >= minStatementPeriodDays && span <= maxStatementPeriodDays {
			facts.PeriodStart, facts.PeriodEnd = start, end
			break
		}
	}
	if match := statementDateLabel.FindStringSubmatch(text); match != nil {
		facts.StatementDate = parsePrintedDate(match[1])
	}
	if match := dueDateLabel.FindStringSubmatch(text); match != nil {
		facts.DueDate = parsePrintedDate(match[1])
	}
	return facts
}

// signedDayGap is whole days from `from` to `to`, negative when `to` is earlier.
func signedDayGap(from, to string) (int, bool) {
	start, err := parseAPIDate(from)
	if err != nil {
		return 0, false
	}
	end, err := parseAPIDate(to)
	if err != nil {
		return 0, false
	}
	return int(end.Sub(start).Hours() / 24), true
}

// dateIsPrinted asks whether a date appears in the statement in any of the
// spellings Indian issuers use.
func dateIsPrinted(iso, text string) bool {
	date, err := parseStrictAPIDate(iso)
	if err != nil {
		return false
	}
	haystack := strings.ToUpper(collapseSpaces(text))
	for _, layout := range []string{
		"02/01/2006", "02-01-2006", "02.01.2006", "02/01/06", "02-01-06",
		"02 Jan 2006", "2 Jan 2006", "02-Jan-2006", "02-Jan-06", "02 Jan 06",
		"Jan 02, 2006", "Jan 2, 2006", "January 2, 2006", "2 January 2006", "02 January 2006", "2006-01-02",
	} {
		if strings.Contains(haystack, strings.ToUpper(date.Format(layout))) {
			return true
		}
	}
	return false
}

// latestStatementRowDate is the newest date among the rows, or "".
func latestStatementRowDate(lines []statementLine) string {
	latest := ""
	for _, line := range lines {
		if date := dateOnly(line.Date); date > latest {
			latest = date
		}
	}
	return latest
}

/*
applyStatementDateChecks settles the bill's dates from the evidence.

Every date is tested against the one thing that cannot lie: a statement cannot
be dated before a transaction printed on it. A bill saved with the wrong date
gets the wrong cycle, and the wrong cycle books real, already-logged spending a
second time as "Unitemized card spends".

The statement's own printed period goes first, then its labelled date, then the
model's reading. When nothing survives, the date is cleared: the form then asks
for it, which beats saving a confident wrong answer. `text` is empty for
screenshots, where only the rows can vouch for a date.
*/
func applyStatementDateChecks(summary *statementSummary, text string, lines []statementLine) {
	facts := readStatementDateFacts(text)
	latest := latestStatementRowDate(lines)

	modelDate := summary.StatementDate
	if text != "" && modelDate != "" && !dateIsPrinted(modelDate, text) {
		modelDate = ""
	}
	chosen := ""
	for _, candidate := range []string{facts.PeriodEnd, facts.StatementDate, modelDate} {
		if candidate != "" && candidate >= latest {
			chosen = candidate
			break
		}
	}
	summary.StatementDate = chosen

	cycleStart := summary.CycleStart
	if facts.PeriodStart != "" && facts.PeriodEnd == chosen {
		cycleStart = facts.PeriodStart
	}
	summary.CycleStart = ""
	if chosen == "" {
		summary.DueDate = ""
		return
	}
	if span, ok := signedDayGap(cycleStart, chosen); ok && span >= minStatementPeriodDays && span <= maxStatementPeriodDays {
		summary.CycleStart = cycleStart
	}

	due := ""
	for _, candidate := range []string{facts.DueDate, summary.DueDate} {
		if gap, ok := signedDayGap(chosen, candidate); ok && gap > 0 && gap <= maxDaysToPayment {
			due = candidate
			break
		}
	}
	summary.DueDate = due
}

// --- choosing between two readings of the rows -------------------------------

// statementRowsNet is what rows add to the bill: spending less refunds, with
// bill payments left out, the same sum the checksum makes.
func statementRowsNet(lines []statementLine) models.Money {
	var net models.Money
	for _, line := range lines {
		switch {
		case classifyLine(line) == lineKindPayment:
		case line.isCredit():
			net -= line.Amount
		default:
			net += line.Amount
		}
	}
	return net
}

// expectedStatementNet is what the rows should add up to, from the figures the
// statement prints.
func expectedStatementNet(summary statementSummary, lines []statementLine) (models.Money, bool) {
	if summary.TotalDue != nil && summary.OpeningBalance != nil {
		payments := models.Money(0)
		if summary.Payments != nil {
			payments = *summary.Payments
		} else {
			for _, line := range lines {
				if classifyLine(line) == lineKindPayment {
					payments += line.Amount
				}
			}
		}
		return *summary.TotalDue - *summary.OpeningBalance + payments, true
	}
	if summary.Purchases != nil {
		expected := *summary.Purchases
		if summary.FeesAndCharges != nil {
			expected += *summary.FeesAndCharges
		}
		return expected, true
	}
	if summary.TotalDue != nil {
		return *summary.TotalDue, true
	}
	return 0, false
}

// chooseStatementRows picks the reading of the rows to believe. Counting rows
// is not enough: a reader that fuses columns finds just as many rows as one
// that does not, with every amount wrong. The statement's own total is the
// test. A reading that adds up wins, the exact local parser first; when
// neither does, the closer one wins, then the longer.
func chooseStatementRows(local, model []statementLine, summary statementSummary) []statementLine {
	expected, known := expectedStatementNet(summary, append(append([]statementLine{}, local...), model...))
	if known {
		localGap := absMoney(statementRowsNet(local) - expected)
		modelGap := absMoney(statementRowsNet(model) - expected)
		switch {
		case len(local) > 0 && localGap <= reconcileTolerance:
			return local
		case len(model) > 0 && modelGap <= reconcileTolerance:
			return model
		case len(model) == 0:
			return local
		case len(local) == 0:
			return model
		case localGap < modelGap:
			return local
		case modelGap < localGap:
			return model
		}
	}
	if len(local) >= len(model) {
		return local
	}
	return model
}

// statementCycleWarning says a statement's rows do not fit the cycle it was
// saved with, which means its date is wrong.
type statementCycleWarning struct {
	LatestRowDate string `json:"latest_row_date"`
	CycleEnd      string `json:"cycle_end"`
	Message       string `json:"message"`
}

func cycleWarningFor(statement *models.CardStatement, lines []statementLine) *statementCycleWarning {
	latest := latestStatementRowDate(lines)
	if latest == "" || latest <= statement.CycleEnd {
		return nil
	}
	return &statementCycleWarning{
		LatestRowDate: latest,
		CycleEnd:      statement.CycleEnd,
		Message: "This statement has transactions up to " + shortDate(latest) +
			", but the bill is saved with a cycle ending " + shortDate(statement.CycleEnd) +
			". Correct the statement date so they count towards this bill.",
	}
}

func shortDate(iso string) string {
	date, err := time.Parse(apiDateLayout, iso)
	if err != nil {
		return iso
	}
	return date.Format("2 Jan")
}
