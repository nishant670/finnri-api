package models

// CalendarDay trims a stored date to the YYYY-MM-DD the API contract promises.
//
// # Why every date column needs this
//
// A `DATE` column read into a Go `string` does not arrive as a date. Both
// drivers hand the value back as a `time.Time`, and GORM sets a string field
// from one by formatting it RFC3339 — so `2026-08-13` comes back out of the
// database as `2026-08-13T00:00:00Z`, on PostgreSQL exactly as on SQLite.
//
// That string then leaves the API as-is. Most of the app survives it, because
// anything that renders a date through `new Date(value)` parses either
// spelling. What does not survive it is any code that treats the value as the
// calendar day it claims to be: a screen printing it raw showed users
// "2026-09-12T00:00:00Z statement", and the card statement's "Review these
// transactions" button handed `cycle_start` straight to the transactions
// filter, which rejected it — "Start Date must use YYYY-MM-DD" — so the one
// screen a user was sent to in order to explain a discrepancy could not open
// at all.
//
// `Entry` already carried this fix inline, with a comment claiming PostgreSQL
// "returns the canonical date directly". It does not. That belief is the whole
// reason the other thirteen date columns were left to leak.
//
// Idempotent: a value that is already a plain day is returned unchanged, so it
// is safe on every dialect and safe to apply twice.
func CalendarDay(value string) string {
	const dayLength = len("2006-01-02")
	if len(value) <= dayLength {
		return value
	}
	return value[:dayLength]
}
