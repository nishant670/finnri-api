package models

import (
	"time"

	"gorm.io/gorm"
)

// CardStatement is one billing cycle on a credit card, as the bank reported
// it. It is a second source of truth alongside the ledger: the statement is
// authoritative for how much is owed, the ledger for what it was spent on.
//
// The unique key on (user_id, account_id, statement_date) is the idempotency
// guarantee. It stops a double-add from the UI today, and gives statement
// parsing free deduplication later — a re-parsed SMS or a re-uploaded PDF
// updates the existing row rather than creating a second one.
type CardStatement struct {
	ID        uint     `gorm:"primaryKey" json:"id"`
	UserID    uint     `gorm:"uniqueIndex:idx_card_statement_once;index;not null" json:"user_id"`
	User      User     `json:"-" gorm:"foreignKey:UserID"`
	AccountID uint     `gorm:"uniqueIndex:idx_card_statement_once;index;not null" json:"account_id"`
	Account   *Account `json:"account,omitempty" gorm:"foreignKey:AccountID"`

	// The inclusive window this statement covers. CycleEnd always equals
	// StatementDate; it is stored separately so cycle queries read plainly.
	CycleStart    string `gorm:"type:date;not null" json:"cycle_start"`
	CycleEnd      string `gorm:"type:date;not null" json:"cycle_end"`
	StatementDate string `gorm:"type:date;uniqueIndex:idx_card_statement_once;not null" json:"statement_date"`
	DueDate       string `gorm:"type:date;index;not null" json:"due_date"`

	TotalDue   Money  `gorm:"type:numeric(19,2);not null;default:0" json:"total_due"`
	MinimumDue Money  `gorm:"type:numeric(19,2);not null;default:0" json:"minimum_due"`
	Currency   string `gorm:"type:char(3);not null;default:INR" json:"currency"`

	// PaidAmount caches SUM(card_statement_payments.amount). The payment rows
	// remain the record; this is recomputed from them on every write so the
	// two can never disagree.
	PaidAmount Money `gorm:"type:numeric(19,2);not null;default:0" json:"paid_amount"`

	// draft (no amount yet) -> unpaid -> partial -> paid. "overdue" is never
	// stored: it is derived from the due date at read time, so a row cannot
	// rot into a wrong state because a job did not run.
	Status string `gorm:"type:varchar(16);not null;default:draft;index" json:"status"`

	// manual, sms or email — how the amount arrived. Manual edits win over
	// parsed values.
	Source string `gorm:"type:varchar(16);not null;default:manual" json:"source"`

	// The "Unitemized card spends" entry holding the difference between the
	// bill and what the ledger could account for. Nil once the gap is zero.
	UnitemizedEntryID *uint `gorm:"index" json:"unitemized_entry_id,omitempty"`

	Notes     string    `gorm:"type:text" json:"notes"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CardStatementPayment is one payment against one statement. Partial payments
// are simply several rows — there is no other mechanism for them.
//
// A payment writes at most one ledger entry, and only when the user says
// which account the money came from: an expense on that bank account. Nothing
// is written against the card itself, because the card's outstanding is
// derived from the statement rather than from ledger arithmetic.
type CardStatementPayment struct {
	ID          uint          `gorm:"primaryKey" json:"id"`
	UserID      uint          `gorm:"index;not null" json:"user_id"`
	User        User          `json:"-" gorm:"foreignKey:UserID"`
	StatementID uint          `gorm:"index;not null" json:"statement_id"`
	Statement   CardStatement `json:"-" gorm:"foreignKey:StatementID"`
	AccountID   uint          `gorm:"index;not null" json:"account_id"`

	// The bank account the money left, and the expense entry recorded against
	// it. Both nil when the user did not say where it came from.
	FromAccountID *uint `gorm:"index" json:"from_account_id,omitempty"`
	BankEntryID   *uint `gorm:"index" json:"bank_entry_id,omitempty"`

	Amount    Money     `gorm:"type:numeric(19,2);not null" json:"amount"`
	PaidOn    string    `gorm:"type:date;index;not null" json:"paid_on"`
	Method    string    `gorm:"type:varchar(24)" json:"method"`
	Note      string    `gorm:"type:text" json:"note"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CardStatementReminder records that one reminder of one kind has been sent
// for one statement. The unique key is the delivery guarantee: the job relies
// on the second insert failing rather than on knowing whether it has already
// run, exactly as SubscriptionReminder does.
type CardStatementReminder struct {
	ID             uint          `gorm:"primaryKey" json:"id"`
	UserID         uint          `gorm:"uniqueIndex:idx_card_statement_reminder_once;index;not null" json:"user_id"`
	User           User          `json:"-" gorm:"foreignKey:UserID"`
	StatementID    uint          `gorm:"uniqueIndex:idx_card_statement_reminder_once;index;not null" json:"statement_id"`
	Statement      CardStatement `json:"-" gorm:"foreignKey:StatementID"`
	Kind           string        `gorm:"type:varchar(24);uniqueIndex:idx_card_statement_reminder_once;not null" json:"kind"`
	NotificationID *uint         `gorm:"index" json:"notification_id,omitempty"`
	Notification   *Notification `json:"-" gorm:"foreignKey:NotificationID"`
	CreatedAt      time.Time     `json:"created_at"`
}

// See CalendarDay: a DATE column comes back as an RFC3339 timestamp, and
// these fields are published as calendar days.
func (statement *CardStatement) AfterFind(_ *gorm.DB) error {
	statement.CycleStart = CalendarDay(statement.CycleStart)
	statement.CycleEnd = CalendarDay(statement.CycleEnd)
	statement.StatementDate = CalendarDay(statement.StatementDate)
	statement.DueDate = CalendarDay(statement.DueDate)
	return nil
}

func (payment *CardStatementPayment) AfterFind(_ *gorm.DB) error {
	payment.PaidOn = CalendarDay(payment.PaidOn)
	return nil
}
