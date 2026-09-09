package http

import (
	"finnri/internal/database"
	"finnri/internal/models"
)

/*
Reconciling a statement against the ledger.

The bill and the ledger answer different questions, and this file keeps them
from being confused for one another. The bill says how much is owed; the
ledger says what it was spent on. When they disagree, the bill is right about
the money and the ledger is simply incomplete.

So a gap never blocks anything and never makes a number wrong — outstanding
and available limit are already exact from the statement alone. What a gap
costs the user is the *breakdown*: some of their spending has no category and
no merchant. The one rule enforced here is that the money must not disappear
from their monthly total while it waits to be itemised, which is why the
difference is booked immediately to a single "Unitemized card spends" entry.
Itemising later shrinks that entry; nothing is ever silently dropped.
*/

const (
	statementStatusDraft   = "draft"
	statementStatusUnpaid  = "unpaid"
	statementStatusPartial = "partial"
	statementStatusPaid    = "paid"
)

const (
	reconcileBalanced = "balanced"
	reconcileUnder    = "under"
	reconcileOver     = "over"
)

// unitemizedTitle and unitemizedTag mark the bucket entry so it can be found,
// updated and excluded from the itemised total. The tag is also what lets a
// user filter these out of their transaction list.
const (
	unitemizedTitle = "Unitemized card spends"
	unitemizedTag   = "Statement"
)

// reconcileTolerance is one rupee. Statements round, and chasing the last
// paisa would mean prompting the user about nothing.
const reconcileTolerance = models.Money(100)

// statementReconciliation is what a statement can and cannot account for.
// ItemizedTotal + UnitemizedAmount + PreviousUnpaid == StatementTotal
// whenever State is "balanced", which is the invariant the UI leans on.
type statementReconciliation struct {
	CycleStart string `json:"cycle_start"`
	CycleEnd   string `json:"cycle_end"`

	// ItemizedTotal is net spend on the card inside the cycle — expenses less
	// refunds — excluding the bucket entry itself.
	ItemizedTotal models.Money `json:"itemized_total"`
	EntriesCount  int          `json:"entries_count"`

	// PreviousUnpaid is what the previous statement still owed. Issuers roll
	// it into the next bill, so it is part of what this statement asks for
	// without being spending that happened in this cycle.
	PreviousUnpaid models.Money `json:"previous_unpaid"`

	StatementTotal models.Money `json:"statement_total"`

	// UnitemizedAmount is money the bank billed that the ledger cannot
	// explain, held in the bucket entry so the monthly total stays right.
	UnitemizedAmount models.Money `json:"unitemized_amount"`

	// Gap is signed: positive when the bank knows more than Finnri does.
	Gap   models.Money `json:"gap"`
	State string       `json:"state"`
}

// computeReconciliation is the whole comparison, kept pure so the arithmetic
// can be tested without a database. `itemized` must exclude the bucket entry.
func computeReconciliation(totalDue, itemized, previousUnpaid models.Money) statementReconciliation {
	gap := totalDue - itemized - previousUnpaid

	result := statementReconciliation{
		ItemizedTotal:  itemized,
		PreviousUnpaid: previousUnpaid,
		StatementTotal: totalDue,
		Gap:            gap,
	}

	switch {
	case gap > reconcileTolerance:
		// The bank billed more than the ledger can explain. The difference
		// becomes the bucket, so the user's spending total stays truthful.
		result.State = reconcileUnder
		result.UnitemizedAmount = gap
	case gap < -reconcileTolerance:
		// Finnri holds more than the bank billed. Usually a duplicate, a
		// spend logged against the wrong card, or a transaction that posted
		// into the next cycle. Never resolved automatically — deleting a
		// user's transaction to make a number tidy is not this system's call.
		result.State = reconcileOver
	default:
		result.State = reconcileBalanced
	}

	return result
}

// deriveStatementStatus resolves a statement's payment state from its
// amounts. A draft is one the reminder job opened before the bill was known;
// it stays a draft until the user prices it, because a zero bill and an
// unknown bill are different things.
//
// "Overdue" is deliberately absent: it is a function of today's date, decided
// at read time by statementIsOverdue, so a stored row can never contradict
// the calendar.
func deriveStatementStatus(current string, totalDue, paid models.Money) string {
	if current == statementStatusDraft {
		return statementStatusDraft
	}
	switch {
	case paid >= totalDue:
		return statementStatusPaid
	case paid > 0:
		return statementStatusPartial
	default:
		return statementStatusUnpaid
	}
}

// statementIsOverdue reports whether an unsettled bill has passed its due
// date. `today` is passed in rather than read from the clock so callers in
// tests and in the reminder job agree on what day it is.
func statementIsOverdue(statement models.CardStatement, today string) bool {
	if statement.Status == statementStatusDraft || statement.Status == statementStatusPaid {
		return false
	}
	return statement.DueDate < today
}

// remainingDue is what still has to be paid, floored at zero so an overpaid
// bill reads as settled rather than as a negative amount to pay.
func remainingDue(statement models.CardStatement) models.Money {
	remaining := statement.TotalDue - statement.PaidAmount
	if remaining < 0 {
		return 0
	}
	return remaining
}

// loadCycleItemizedTotal sums net spend on a card inside a cycle, excluding
// the statement's own bucket entry — counting the bucket would make every
// statement look balanced the moment it was created.
//
// Conditional sums rather than FILTER, matching account_summary.go: the smoke
// suite runs this code against SQLite.
func loadCycleItemizedTotal(userID, accountID uint, cycleStart, cycleEnd string, excludeEntryID *uint) (models.Money, int, error) {
	var row struct {
		Spent    models.Money
		Credited models.Money
		Entries  int
	}

	query := database.DB.Model(&models.Entry{}).
		Select(`COALESCE(SUM(CASE WHEN LOWER(type) = 'expense' THEN amount ELSE 0 END), 0) AS spent,
			COALESCE(SUM(CASE WHEN LOWER(type) <> 'expense' THEN amount ELSE 0 END), 0) AS credited,
			COUNT(*) AS entries`).
		Where("user_id = ? AND account_id = ? AND date >= ? AND date <= ?",
			userID, accountID, cycleStart, cycleEnd)

	if excludeEntryID != nil {
		query = query.Where("id <> ?", *excludeEntryID)
	}

	if err := query.Scan(&row).Error; err != nil {
		return 0, 0, err
	}
	return row.Spent - row.Credited, row.Entries, nil
}

// loadPreviousUnpaid is the unpaid remainder of the statement immediately
// before this one. It is derived rather than stored: one less field for the
// user to understand, and it cannot drift out of step with the payments that
// determine it.
func loadPreviousUnpaid(userID, accountID uint, statementDate string) (models.Money, error) {
	var previous models.CardStatement
	err := database.DB.
		Where("user_id = ? AND account_id = ? AND statement_date < ? AND status <> ?",
			userID, accountID, statementDate, statementStatusDraft).
		Order("statement_date DESC").
		First(&previous).Error
	if err != nil {
		// No earlier statement is the normal case for a card's first bill.
		return 0, nil
	}
	return remainingDue(previous), nil
}

// reconcileStatement compares one statement against the ledger and brings its
// bucket entry into line with the result. It is called after every write that
// can move either side of the comparison: pricing a statement, editing its
// total, and adding or removing transactions in its cycle.
func reconcileStatement(statement *models.CardStatement) (statementReconciliation, error) {
	itemized, entries, err := loadCycleItemizedTotal(
		statement.UserID, statement.AccountID,
		statement.CycleStart, statement.CycleEnd,
		statement.UnitemizedEntryID,
	)
	if err != nil {
		return statementReconciliation{}, err
	}

	previousUnpaid, err := loadPreviousUnpaid(statement.UserID, statement.AccountID, statement.StatementDate)
	if err != nil {
		return statementReconciliation{}, err
	}

	result := computeReconciliation(statement.TotalDue, itemized, previousUnpaid)
	result.CycleStart = statement.CycleStart
	result.CycleEnd = statement.CycleEnd
	result.EntriesCount = entries

	// A draft has no amount to reconcile against yet.
	if statement.Status == statementStatusDraft {
		return result, nil
	}

	if err := syncUnitemizedEntry(statement, result.UnitemizedAmount); err != nil {
		return statementReconciliation{}, err
	}
	return result, nil
}

// syncUnitemizedEntry makes the bucket entry hold exactly `amount`, creating,
// updating or removing it as the gap moves. Itemising transactions shrinks it
// on its own: each new entry raises the itemised total, which lowers the gap,
// which lowers the bucket on the next reconcile.
func syncUnitemizedEntry(statement *models.CardStatement, amount models.Money) error {
	if amount <= 0 {
		return clearUnitemizedEntry(statement)
	}

	if statement.UnitemizedEntryID != nil {
		result := database.DB.Model(&models.Entry{}).
			Where("id = ? AND user_id = ?", *statement.UnitemizedEntryID, statement.UserID).
			Updates(map[string]any{"amount": amount, "date": statement.CycleEnd})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected > 0 {
			return nil
		}
		// The user deleted it from their transaction list. Fall through and
		// make a new one rather than losing track of the money.
		statement.UnitemizedEntryID = nil
	}

	entry := models.Entry{
		Title:    unitemizedTitle,
		Type:     "expense",
		Amount:   amount,
		Currency: statement.Currency,
		Source:   "manual",
		Category: defaultCategory,
		Tag:      unitemizedTag,
		Notes:    "Billed on your statement but not itemised in Finnri yet.",
		Date:     statement.CycleEnd,
		// Deliberately on the card, so the cycle's own totals see it.
		AccountID: &statement.AccountID,
		UserID:    statement.UserID,
	}
	if err := database.DB.Create(&entry).Error; err != nil {
		return err
	}

	statement.UnitemizedEntryID = &entry.ID
	return database.DB.Model(statement).
		Update("unitemized_entry_id", entry.ID).Error
}

// clearUnitemizedEntry removes the bucket once everything is accounted for.
func clearUnitemizedEntry(statement *models.CardStatement) error {
	if statement.UnitemizedEntryID == nil {
		return nil
	}
	entryID := *statement.UnitemizedEntryID
	statement.UnitemizedEntryID = nil
	if err := database.DB.Model(statement).
		Update("unitemized_entry_id", nil).Error; err != nil {
		return err
	}
	return database.DB.
		Where("id = ? AND user_id = ?", entryID, statement.UserID).
		Delete(&models.Entry{}).Error
}
