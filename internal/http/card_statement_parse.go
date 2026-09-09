package http

import (
	"regexp"
	"strings"
	"time"

	"finnri/internal/models"
)

/*
Turning statement text into line items.

Indian card statements vary by issuer but share a shape: a date, a
description, and an amount, with credits marked rather than signed. So the
parser looks for exactly that shape and ignores everything it cannot read —
headers, footers, marketing, reward-point tables.

Ignoring is the right failure mode. A row this misreads becomes a wrong
transaction the user has to notice and undo; a row it skips shows up as an
unaccounted difference they can add by hand. Skipping is recoverable, guessing
is not.
*/

// statementDatePatterns are the orderings issuers actually use. Two-digit
// years are read as 20xx, which is safe for a statement.
var statementDatePatterns = []struct {
	pattern *regexp.Regexp
	layouts []string
}{
	{
		// 10/07/2026, 10-07-2026, 10.07.26
		pattern: regexp.MustCompile(`^(\d{2})[/\-.](\d{2})[/\-.](\d{2,4})`),
		layouts: []string{"02/01/2006", "02/01/06"},
	},
	{
		// 10 Jul 2026, 10-JUL-26
		pattern: regexp.MustCompile(`^(\d{1,2})[\s\-]([A-Za-z]{3})[\s\-](\d{2,4})`),
		layouts: []string{"2 Jan 2006", "2 Jan 06"},
	},
	{
		// 2026-07-10
		pattern: regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})`),
		layouts: []string{"2006-01-02"},
	},
}

// amountPattern matches the trailing amount, with optional grouping, decimals
// and a credit marker. The marker is what tells a refund from a purchase:
// statements print magnitudes and annotate direction.
var amountPattern = regexp.MustCompile(
	`(?i)(?:(?:INR|Rs\.?|₹)\s*)?((?:\d{1,3}(?:,\d{2,3})+|\d+)(?:\.\d{1,2})?)\s*(CR|DR|C|D)?\s*$`)

// creditMarkers are what an issuer prints against a credit.
var creditMarkers = map[string]bool{"CR": true, "C": true}

// parseStatementText reads whatever line items it can recognise.
//
// `fallbackYear` fills in for the date formats that omit one; the statement's
// own cycle supplies it.
func parseStatementText(text string, fallbackYear int) []statementLine {
	lines := []statementLine{}

	for _, raw := range strings.Split(text, "\n") {
		line, ok := parseStatementRow(raw, fallbackYear)
		if !ok {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

// parseStatementRow reads one row, or reports that it is not one.
func parseStatementRow(raw string, fallbackYear int) (statementLine, bool) {
	row := strings.TrimSpace(raw)
	if row == "" {
		return statementLine{}, false
	}

	date, rest, ok := takeLeadingDate(row, fallbackYear)
	if !ok {
		return statementLine{}, false
	}

	amountMatch := amountPattern.FindStringSubmatch(rest)
	if amountMatch == nil {
		return statementLine{}, false
	}

	amount, err := models.ParseMoney(strings.ReplaceAll(amountMatch[1], ",", ""))
	if err != nil || amount <= 0 {
		return statementLine{}, false
	}

	description := strings.TrimSpace(rest[:len(rest)-len(amountMatch[0])])
	description = strings.Trim(description, " \t·-|")
	if description == "" {
		// A date and an amount with nothing between them is a running total or
		// a carried balance, not a transaction.
		return statementLine{}, false
	}

	entryType := "expense"
	if creditMarkers[strings.ToUpper(amountMatch[2])] {
		entryType = "income"
	}

	line := statementLine{
		Date:        date,
		Description: collapseSpaces(description),
		Amount:      amount,
		Type:        entryType,
	}
	line.Kind = classifyLine(line)
	return line, true
}

// takeLeadingDate reads a date off the front of a row and returns the rest.
func takeLeadingDate(row string, fallbackYear int) (string, string, bool) {
	for _, candidate := range statementDatePatterns {
		match := candidate.pattern.FindString(row)
		if match == "" {
			continue
		}
		normalized := strings.NewReplacer("-", "/", ".", "/").Replace(match)
		for _, layout := range candidate.layouts {
			parsed, err := time.Parse(strings.NewReplacer("-", "/", ".", "/").Replace(layout), normalized)
			if err != nil {
				continue
			}
			if parsed.Year() == 0 && fallbackYear > 0 {
				parsed = parsed.AddDate(fallbackYear, 0, 0)
			}
			return parsed.Format(apiDateLayout), strings.TrimSpace(row[len(match):]), true
		}
	}
	return "", "", false
}

func collapseSpaces(value string) string {
	return strings.Join(strings.Fields(value), " ")
}
