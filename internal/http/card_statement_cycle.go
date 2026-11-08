package http

import "time"

/*
Billing-cycle arithmetic for credit cards.

Every date a statement carries is derived from two numbers the user gives
once — the account's `statement_day` and `due_day` — and every one of them
stays editable afterwards, because banks shift dates for weekends and
holidays and the paper statement always wins.

The awkward cases are the ones that decide whether this is right: a card
billing on the 31st has no 31st in November, and a card billing on the 25th
and due on the 14th is due in the *following* month while a card billing on
the 2nd and due on the 22nd is due in the same one. Both fall out of the two
rules below rather than being special-cased.
*/

// apiDateLayout is the wire format every date field on a statement uses.
const apiDateLayout = "2006-01-02"

// clampDayToMonth returns `day` of the given month, pulled back to the last
// day of that month when the month is too short. Day 31 in November is the
// 30th; day 31 in a non-leap February is the 28th.
//
// Callers pass a day already validated to 1..31, but a day outside that range
// is clamped rather than rejected so no caller can produce a wild date by
// forgetting to validate.
func clampDayToMonth(year int, month time.Month, day int) time.Time {
	if day < 1 {
		day = 1
	}
	// The 0th of the next month is the last of this one.
	lastDay := time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
	if day > lastDay {
		day = lastDay
	}
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// effectiveStatementDay is the day of month a card bills on. A card that has
// never had `statement_day` set falls back to the day of the statement the
// user is entering right now, which is the only evidence available and is
// also what we offer to save back onto the account.
func effectiveStatementDay(statementDay int, statementDate time.Time) int {
	if statementDay >= 1 && statementDay <= 31 {
		return statementDay
	}
	return statementDate.Day()
}

// statementDayDriftDays is how far a bill can sit from the card's billing day
// and still be that day moved for a weekend or holiday.
const statementDayDriftDays = 7

// statementCycle returns the inclusive window a statement covers.
//
// The cycle ends on the statement date and starts the day after the previous
// statement, so consecutive cycles tile the calendar with no gap and no
// overlap. Clamping the previous month's anchor first is what keeps a card
// billing on the 31st correct: the March statement of such a card starts on
// 1 March, because February's anchor clamped to the 28th.
func statementCycle(statementDate time.Time, statementDay int) (start, end time.Time) {
	end = truncateDate(statementDate)
	day := effectiveStatementDay(statementDay, end)

	// A bill dated far from the card's usual day says the card's day is the
	// wrong one, not that the cycle is short. A card set to the 23rd holding a
	// bill dated 5 Nov would otherwise start the cycle on 24 Oct and leave
	// half the bill's spending out of it.
	if gap := clampDayToMonth(end.Year(), end.Month(), day).Sub(end).Hours() / 24; gap > statementDayDriftDays || gap < -statementDayDriftDays {
		day = end.Day()
	}

	previousAnchor := clampDayToMonth(end.Year(), end.Month()-1, day)
	start = previousAnchor.AddDate(0, 0, 1)

	// A statement dated before its own anchor (the user back-dated it, or the
	// bank moved the date earlier) would otherwise produce an empty or
	// inverted window. Fall back to a one-month lookback ending on the
	// statement date, which is never wrong by more than the shift itself.
	if !start.Before(end) {
		start = end.AddDate(0, -1, 0).AddDate(0, 0, 1)
	}
	return start, end
}

// dueDateFor is the next occurrence of `dueDay` strictly after the statement
// date, clamped to month length.
//
// "Strictly after" is the whole rule. A card billing on the 2nd and due on
// the 22nd finds its due date in the same month; a card billing on the 25th
// and due on the 14th finds the 14th already behind it and moves to the next.
// Neither case needs to know which one it is.
func dueDateFor(statementDate time.Time, dueDay int) time.Time {
	statement := truncateDate(statementDate)
	candidate := clampDayToMonth(statement.Year(), statement.Month(), dueDay)
	if candidate.After(statement) {
		return candidate
	}
	return clampDayToMonth(statement.Year(), statement.Month()+1, dueDay)
}

// nextStatementDate is the following cycle's statement date — used by the
// reminder job to open the next draft once a cycle closes.
func nextStatementDate(statementDate time.Time, statementDay int) time.Time {
	current := truncateDate(statementDate)
	day := effectiveStatementDay(statementDay, current)
	return clampDayToMonth(current.Year(), current.Month()+1, day)
}
