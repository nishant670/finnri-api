package models

import "testing"

// The bug this guards: a DATE column read into a Go string arrives as an
// RFC3339 timestamp, on PostgreSQL exactly as on SQLite. The statement screen
// handed `cycle_start` straight to the transactions filter, which rejected the
// timestamp spelling — so the screen a user was sent to in order to explain a
// discrepancy could not open.
func TestCalendarDayTrimsATimestampBackToItsDay(t *testing.T) {
	for _, testCase := range []struct{ name, in, want string }{
		{"a date read back as a UTC timestamp", "2026-08-13T00:00:00Z", "2026-08-13"},
		{"with fractional seconds", "2026-08-13T00:00:00.123456Z", "2026-08-13"},
		{"with an offset", "2026-08-13T05:30:00+05:30", "2026-08-13"},
		{"already a calendar day", "2026-08-13", "2026-08-13"},
		{"empty stays empty rather than panicking", "", ""},
		{"a short value is left alone rather than sliced", "2026-08", "2026-08"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := CalendarDay(testCase.in); got != testCase.want {
				t.Fatalf("CalendarDay(%q) = %q, want %q", testCase.in, got, testCase.want)
			}
		})
	}
}

func TestCalendarDayIsIdempotent(t *testing.T) {
	// It runs on every read, and the same row is read on both dialects. Applying
	// it to its own output must never shorten the value again.
	once := CalendarDay("2026-08-13T00:00:00Z")
	if twice := CalendarDay(once); twice != once {
		t.Fatalf("second pass changed %q to %q", once, twice)
	}
}

// AfterFind is the only thing standing between the database and the JSON
// contract, so each model that publishes a calendar day has to run it.
func TestEveryDatePublishingModelNormalisesOnRead(t *testing.T) {
	timestamp := "2026-08-13T00:00:00Z"
	day := "2026-08-13"

	statement := CardStatement{CycleStart: timestamp, CycleEnd: timestamp, StatementDate: timestamp, DueDate: timestamp}
	_ = statement.AfterFind(nil)
	if statement.CycleStart != day || statement.CycleEnd != day ||
		statement.StatementDate != day || statement.DueDate != day {
		t.Fatalf("card statement still carries timestamps: %#v", statement)
	}

	payment := CardStatementPayment{PaidOn: timestamp}
	_ = payment.AfterFind(nil)
	if payment.PaidOn != day {
		t.Fatalf("payment paid_on = %q", payment.PaidOn)
	}

	plan := CardEMIPlan{PurchasedOn: timestamp, FirstInstallment: timestamp}
	_ = plan.AfterFind(nil)
	if plan.PurchasedOn != day || plan.FirstInstallment != day {
		t.Fatalf("emi plan still carries timestamps: %#v", plan)
	}

	installment := CardEMIInstallment{DueDate: timestamp}
	_ = installment.AfterFind(nil)
	if installment.DueDate != day {
		t.Fatalf("installment due_date = %q", installment.DueDate)
	}

	subscription := Subscription{NextDueDate: timestamp}
	_ = subscription.AfterFind(nil)
	if subscription.NextDueDate != day {
		t.Fatalf("subscription next_due_date = %q", subscription.NextDueDate)
	}

	occurrence := SubscriptionOccurrence{DueDate: timestamp}
	_ = occurrence.AfterFind(nil)
	if occurrence.DueDate != day {
		t.Fatalf("occurrence due_date = %q", occurrence.DueDate)
	}

	reminder := SubscriptionReminder{DueDate: timestamp}
	_ = reminder.AfterFind(nil)
	if reminder.DueDate != day {
		t.Fatalf("reminder due_date = %q", reminder.DueDate)
	}

	alert := BudgetAlert{PeriodStart: timestamp}
	_ = alert.AfterFind(nil)
	if alert.PeriodStart != day {
		t.Fatalf("budget alert period_start = %q", alert.PeriodStart)
	}

	entry := Entry{Date: timestamp}
	_ = entry.AfterFind(nil)
	if entry.Date != day {
		t.Fatalf("entry date = %q", entry.Date)
	}
}
