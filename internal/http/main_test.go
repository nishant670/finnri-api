package http

import (
	"os"
	"testing"
	"time"
)

// TestMain runs this package's tests with the process clock in UTC.
//
// The tests use SQLite, which stores a timestamp as text in whatever zone the
// value carried and compares those strings as strings. Postgres compares
// instants. So a query like "created_at < now" could answer differently here
// than in production, and differently again depending on where the machine
// sat: one activation test passed only east of UTC, three checkout tests only
// from UTC eastwards, and the first CI run — in UTC — was the first time
// anybody saw it. With every value written in UTC the text order is the time
// order, and the suite gives one answer on a laptop in India, a runner in UTC
// and anywhere else.
//
// Handlers that care about a place ask for it by name (Asia/Kolkata), so they
// are unaffected.
func TestMain(m *testing.M) {
	time.Local = time.UTC
	os.Exit(m.Run())
}
