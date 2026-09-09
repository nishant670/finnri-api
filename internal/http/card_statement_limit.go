package http

import "finnri/internal/models"

/*
What a credit card has left to spend.

	available = credit_limit - outstanding - emi_blocked_principal

The interesting term is `outstanding`, which has two possible sources and
prefers the bank's:

  - Once a statement carries an amount, outstanding is what that statement
    still asks for plus anything logged since it closed. The bank's number is
    authoritative for money, so this is exact from the moment the user types
    the bill — no reconciliation needed to make it true.
  - Before any statement exists, it falls back to the ledger formula
    account_summary.go has always used. A card is simply less precise until
    its first bill is entered; nothing needs backfilling.

Only the *latest* statement is consulted. An older statement left partly
unpaid is not added on top, because issuers roll an unpaid balance forward
into the next statement's total — counting both would double it.
*/

const (
	outstandingFromStatement = "statement"
	outstandingFromLedger    = "ledger"
)

// cardLimitSummary is the card headline, computed server-side so no screen
// has to do this arithmetic. Limit-dependent fields stay nil when the user
// has not told us the limit, rather than being reported as zero.
type cardLimitSummary struct {
	Outstanding         models.Money  `json:"outstanding"`
	OutstandingSource   string        `json:"outstanding_source"`
	EMIBlockedPrincipal models.Money  `json:"emi_blocked_principal"`
	CreditLimit         models.Money  `json:"credit_limit"`
	AvailableLimit      *models.Money `json:"available_limit,omitempty"`
	UtilisationPct      *float64      `json:"utilisation_pct,omitempty"`
}

// cardLimitInput is everything the calculation needs, gathered by the caller
// so this stays a pure function and can be tested without a database.
type cardLimitInput struct {
	CreditLimit models.Money

	// StatementTotalDue and StatementPaid come from the most recent statement
	// that has an amount. HasStatement is false when the card has none yet,
	// or has only an empty draft.
	HasStatement      bool
	StatementTotalDue models.Money
	StatementPaid     models.Money

	// SpendAfterStatement is net spend (expenses less refunds) logged against
	// the card after that statement's cycle closed — the new cycle, which the
	// bank has not billed yet but the user has already committed.
	SpendAfterStatement models.Money

	// LedgerOutstanding is the account_summary.go formula: opening balance
	// plus lifetime spend less lifetime credits. Used only as the fallback.
	LedgerOutstanding models.Money

	// EMIBlockedPrincipal is principal on active EMI plans not yet billed to
	// a statement. Installments already billed are excluded because they are
	// inside StatementTotalDue; counting both would block the limit twice.
	EMIBlockedPrincipal models.Money
}

// summariseCardLimit computes the card headline.
func summariseCardLimit(input cardLimitInput) cardLimitSummary {
	summary := cardLimitSummary{
		CreditLimit:         input.CreditLimit,
		EMIBlockedPrincipal: input.EMIBlockedPrincipal,
	}

	if input.HasStatement {
		summary.Outstanding = input.StatementTotalDue - input.StatementPaid + input.SpendAfterStatement
		summary.OutstandingSource = outstandingFromStatement
	} else {
		summary.Outstanding = input.LedgerOutstanding
		summary.OutstandingSource = outstandingFromLedger
	}

	// Overpaying a bill leaves a card in credit. That is real, and the card
	// screen says so, but it is not "negative money owed" for the purpose of
	// how much limit is free — the free limit is simply the whole limit.
	committed := summary.Outstanding + summary.EMIBlockedPrincipal
	if committed < 0 {
		committed = 0
	}

	// Without a limit there is nothing to divide by and nothing to subtract
	// from. Outstanding is still reported; availability is not invented.
	if input.CreditLimit <= 0 {
		return summary
	}

	available := input.CreditLimit - committed
	summary.AvailableLimit = &available

	// Over 100% happens — issuers allow it, and hiding it would be the
	// friendlier lie. Only the in-credit case is floored, since no bar can
	// draw a negative.
	utilisation := float64(committed) / float64(input.CreditLimit) * 100
	summary.UtilisationPct = &utilisation

	return summary
}
