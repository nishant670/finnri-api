package http

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"finnri/internal/models"
)

/*
A statement PDF, written by hand.

There is no PDF dependency in `go.mod` and this task did not earn one. A
statement is a title, a summary and a table of text in one font — the part of
PDF that is genuinely simple — and the alternatives were a library that pulls a
tree of transitive modules into a service that currently has none, or generating
the document in the app, which needs `expo-print`. `expo-print` ships native
code, so adding it means a new dev client and a new release build before anyone
can see the output at all. That is the cost C9 refused to pay for shared
element transitions, for the same reason: it makes the work unverifiable on the
handset that is already attached.

The file this writes is a PDF 1.4 with one page tree, two of the fourteen
standard fonts (which every reader has, so nothing is embedded), and one content
stream per page.
*/

const (
	pdfPageWidth   = 595.0 // A4 at 72dpi, rounded to whole points.
	pdfPageHeight  = 842.0
	pdfMargin      = 48.0
	pdfLineHeight  = 15.0
	pdfBodySize    = 9.5
	pdfHeaderSize  = 18.0
	pdfSummarySize = 10.5
)

// Column origins, measured from the left edge. Amount is right-aligned against
// the page margin instead, because a column of figures that does not line up on
// its last digit is not a column of figures.
var pdfColumns = struct{ date, title, category, mode float64 }{
	date:     pdfMargin,
	title:    pdfMargin + 70,
	category: pdfMargin + 250,
	mode:     pdfMargin + 355,
}

type statementTotals struct {
	Spent  models.Money
	Income models.Money
	Count  int
}

func summariseEntries(entries []models.Entry) statementTotals {
	totals := statementTotals{Count: len(entries)}
	for _, entry := range entries {
		if strings.EqualFold(entry.Type, "income") {
			totals.Income += entry.Amount
			continue
		}
		totals.Spent += entry.Amount
	}
	return totals
}

/*
buildStatementPDF renders the filtered entries as a statement.

`period` is whatever the filters describe rather than a month the server picked:
the CSV and the PDF come off the same query, so a statement can only honestly
claim the range it was actually given.
*/
func buildStatementPDF(entries []models.Entry, period string, generatedAt time.Time) []byte {
	totals := summariseEntries(entries)

	// The header block is only drawn on page one; later pages start at the
	// table so a long statement does not repeat its own summary.
	firstPageTop := pdfPageHeight - pdfMargin - 112
	laterPageTop := pdfPageHeight - pdfMargin - 24
	rowsOnFirst := int((firstPageTop - pdfMargin) / pdfLineHeight)
	rowsOnLater := int((laterPageTop - pdfMargin) / pdfLineHeight)

	pages := paginate(len(entries), rowsOnFirst, rowsOnLater)
	contents := make([]string, 0, len(pages))
	for index, page := range pages {
		var body bytes.Buffer
		cursor := laterPageTop
		if index == 0 {
			cursor = firstPageTop
			writeStatementHeader(&body, period, totals, generatedAt)
		}
		writeTableHeader(&body, cursor)
		cursor -= pdfLineHeight * 1.4
		for _, entry := range entries[page.from:page.to] {
			writeEntryRow(&body, entry, cursor)
			cursor -= pdfLineHeight
		}
		writeFooter(&body, index+1, len(pages))
		contents = append(contents, body.String())
	}

	return assemblePDF(contents)
}

type pageSpan struct{ from, to int }

// paginate splits the rows across pages, and always yields at least one page —
// an empty statement is a real answer to a filter that matched nothing, and a
// zero-page PDF is not a file any reader will open.
func paginate(total, onFirst, onLater int) []pageSpan {
	if onFirst < 1 {
		onFirst = 1
	}
	if onLater < 1 {
		onLater = 1
	}
	if total == 0 {
		return []pageSpan{{0, 0}}
	}
	pages := []pageSpan{}
	cursor := 0
	capacity := onFirst
	for cursor < total {
		end := cursor + capacity
		if end > total {
			end = total
		}
		pages = append(pages, pageSpan{cursor, end})
		cursor = end
		capacity = onLater
	}
	return pages
}

func writeStatementHeader(body *bytes.Buffer, period string, totals statementTotals, generatedAt time.Time) {
	top := pdfPageHeight - pdfMargin - 18
	text(body, "F2", pdfHeaderSize, pdfMargin, top, "Finnri statement")
	text(body, "F1", pdfSummarySize, pdfMargin, top-20, period)
	text(body, "F1", pdfBodySize, pdfMargin, top-34,
		fmt.Sprintf("Generated %s", generatedAt.Format("2 Jan 2006, 15:04")))

	net := totals.Income - totals.Spent
	summary := fmt.Sprintf(
		"Spent %s     Income %s     Net %s     %d transaction%s",
		money(totals.Spent), money(totals.Income), money(net),
		totals.Count, plural(totals.Count),
	)
	text(body, "F2", pdfSummarySize, pdfMargin, top-58, summary)
	line(body, pdfMargin, top-70, pdfPageWidth-pdfMargin, top-70)
}

func writeTableHeader(body *bytes.Buffer, y float64) {
	text(body, "F2", pdfBodySize, pdfColumns.date, y, "DATE")
	text(body, "F2", pdfBodySize, pdfColumns.title, y, "DESCRIPTION")
	text(body, "F2", pdfBodySize, pdfColumns.category, y, "CATEGORY")
	text(body, "F2", pdfBodySize, pdfColumns.mode, y, "MODE")
	textRight(body, "F2", pdfBodySize, pdfPageWidth-pdfMargin, y, "AMOUNT")
	line(body, pdfMargin, y-5, pdfPageWidth-pdfMargin, y-5)
}

func writeEntryRow(body *bytes.Buffer, entry models.Entry, y float64) {
	label := strings.TrimSpace(entry.Title)
	if label == "" {
		label = strings.TrimSpace(entry.Merchant)
	}
	if label == "" {
		label = "Untitled"
	}
	// Income reads as a credit rather than being told apart by its category.
	// Entry amounts are stored unsigned, so the sign here is the row's type.
	signed := entry.Amount
	if !strings.EqualFold(entry.Type, "income") {
		signed = -signed
	}
	amount := money(signed)
	if signed > 0 {
		amount = "+" + amount
	}

	text(body, "F1", pdfBodySize, pdfColumns.date, y, entry.Date)
	text(body, "F1", pdfBodySize, pdfColumns.title, y, truncate(label, 34))
	text(body, "F1", pdfBodySize, pdfColumns.category, y, truncate(entry.Category, 20))
	text(body, "F1", pdfBodySize, pdfColumns.mode, y, truncate(entry.Mode, 16))
	textRight(body, "F1", pdfBodySize, pdfPageWidth-pdfMargin, y, amount)
}

func writeFooter(body *bytes.Buffer, page, of int) {
	textRight(body, "F1", 8, pdfPageWidth-pdfMargin, pdfMargin-18,
		fmt.Sprintf("Page %d of %d", page, of))
}

/*
money formats an amount for a page that has no rupee glyph.

The standard fonts are WinAnsiEncoded and `₹` is not in that set, so writing it
would emit a wrong character rather than a missing one — worse, because it looks
deliberate. `INR` is the same information in characters the reader definitely
has.
*/
func money(amount models.Money) string {
	// The sign goes in front of the currency rather than between it and the
	// digits: `INR -2753.00` reads as a currency called "INR -" for the length
	// of a glance, which is exactly the length of a glance a statement gets.
	if amount < 0 {
		return "-INR " + (-amount).String()
	}
	return "INR " + amount.String()
}

func plural(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}

func truncate(value string, max int) string {
	value = strings.TrimSpace(value)
	if len([]rune(value)) <= max {
		return value
	}
	return string([]rune(value)[:max-1]) + "…"
}

// --- the PDF itself ---------------------------------------------------------

func text(body *bytes.Buffer, font string, size, x, y float64, value string) {
	fmt.Fprintf(body, "BT /%s %.1f Tf %.2f %.2f Td (%s) Tj ET\n", font, size, x, y, escapePDFText(value))
}

// textRight lays the string out right-aligned against `right`. Helvetica's
// widths vary per glyph and the exact metrics are not worth carrying for this;
// 0.5em per character is close enough that a column of amounts lines up, which
// is the only thing the alignment is for.
func textRight(body *bytes.Buffer, font string, size, right, y float64, value string) {
	width := float64(len([]rune(value))) * size * 0.5
	text(body, font, size, right-width, y, value)
}

func line(body *bytes.Buffer, x1, y1, x2, y2 float64) {
	fmt.Fprintf(body, "0.8 w 0.75 0.75 0.75 RG %.2f %.2f m %.2f %.2f l S\n", x1, y1, x2, y2)
}

/*
escapePDFText makes a Go string safe inside a PDF literal string.

The three characters that matter are the escape itself and the two parentheses
that delimit the literal — an unbalanced `)` in a merchant name would end the
string early and corrupt every object offset after it. Anything outside Latin-1
is dropped rather than mangled, for the same reason `money` spells INR.
*/
func escapePDFText(value string) string {
	var out strings.Builder
	for _, r := range value {
		switch r {
		case '\\':
			out.WriteString(`\\`)
		case '(':
			out.WriteString(`\(`)
		case ')':
			out.WriteString(`\)`)
		case '\n', '\r', '\t':
			out.WriteByte(' ')
		default:
			if r == '…' {
				out.WriteString("...")
				continue
			}
			if r < 32 || r > 255 {
				continue
			}
			out.WriteRune(r)
		}
	}
	return out.String()
}

/*
assemblePDF writes the object graph and the cross-reference table.

The xref is a table of byte offsets into this very file, so the objects have to
be serialised before it can be written — which is why the whole document is
built in one buffer and measured as it goes rather than assembled from parts.
*/
func assemblePDF(contents []string) []byte {
	// 1 catalog, 2 page tree, 3 regular font, 4 bold font, then a page object
	// and a content object per page.
	objectCount := 4 + len(contents)*2
	objects := make([]string, objectCount+1) // 1-indexed; [0] unused.

	kids := make([]string, 0, len(contents))
	for i := range contents {
		kids = append(kids, fmt.Sprintf("%d 0 R", 5+i*2))
	}

	objects[1] = "<< /Type /Catalog /Pages 2 0 R >>"
	objects[2] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>",
		strings.Join(kids, " "), len(contents))
	objects[3] = "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>"
	objects[4] = "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica-Bold /Encoding /WinAnsiEncoding >>"

	for i, content := range contents {
		pageObj := 5 + i*2
		contentObj := pageObj + 1
		objects[pageObj] = fmt.Sprintf(
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %.0f %.0f] "+
				"/Resources << /Font << /F1 3 0 R /F2 4 0 R >> >> /Contents %d 0 R >>",
			pdfPageWidth, pdfPageHeight, contentObj,
		)
		objects[contentObj] = fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content)
	}

	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offsets := make([]int, objectCount+1)
	for number := 1; number <= objectCount; number++ {
		offsets[number] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", number, objects[number])
	}

	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n", objectCount+1)
	out.WriteString("0000000000 65535 f \n")
	for number := 1; number <= objectCount; number++ {
		fmt.Fprintf(&out, "%010d 00000 n \n", offsets[number])
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		objectCount+1, xref)

	return out.Bytes()
}
