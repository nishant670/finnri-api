package http

import (
	"strings"

	"finnri/internal/models"
)

// cardStatementInput is a bill as the user reads it off their statement. Only
// the statement date and total are really theirs to give; the cycle window is
// derived, and the due date is derived unless they correct it.
type cardStatementInput struct {
	StatementDate string       `json:"statement_date"`
	DueDate       string       `json:"due_date"`
	TotalDue      models.Money `json:"total_due"`
	MinimumDue    models.Money `json:"minimum_due"`
	Currency      string       `json:"currency"`
	Source        string       `json:"source"`
	Notes         string       `json:"notes"`
}

func (input cardStatementInput) validate() map[string]string {
	fields := map[string]string{}

	statementDate, err := parseStrictAPIDate(input.StatementDate)
	if err != nil {
		fields["statement_date"] = "must be a YYYY-MM-DD date"
	}

	if strings.TrimSpace(input.DueDate) != "" {
		dueDate, dueErr := parseStrictAPIDate(input.DueDate)
		switch {
		case dueErr != nil:
			fields["due_date"] = "must be a YYYY-MM-DD date"
		case err == nil && !dueDate.After(statementDate):
			// A bill is never due the day it bills, and never before.
			fields["due_date"] = "must be after the statement date"
		}
	}

	if input.TotalDue < 0 {
		fields["total_due"] = "must be zero or positive"
	}
	if input.MinimumDue < 0 {
		fields["minimum_due"] = "must be zero or positive"
	} else if input.MinimumDue > input.TotalDue {
		fields["minimum_due"] = "must not exceed the total due"
	}

	if normalizedStatementCurrency(input.Currency) != "INR" {
		fields["currency"] = "must be INR"
	}

	if source := normalizedStatementSource(input.Source); source == "" {
		fields["source"] = "must be manual, sms or email"
	}

	return fields
}

// cardStatementPaymentInput records that money moved. Finnri never moves it —
// this is a log of something the user did in their bank's app.
type cardStatementPaymentInput struct {
	Amount        models.Money `json:"amount"`
	PaidOn        string       `json:"paid_on"`
	Method        string       `json:"method"`
	FromAccountID *uint        `json:"from_account_id"`
	Note          string       `json:"note"`
}

func (input cardStatementPaymentInput) validate() map[string]string {
	fields := map[string]string{}

	if !input.Amount.IsPositive() {
		fields["amount"] = "must be positive"
	}
	if strings.TrimSpace(input.PaidOn) != "" {
		if _, err := parseStrictAPIDate(input.PaidOn); err != nil {
			fields["paid_on"] = "must be a YYYY-MM-DD date"
		}
	}
	if len(input.Method) > 24 {
		fields["method"] = "must be at most 24 characters"
	}

	return fields
}

func normalizedStatementCurrency(currency string) string {
	trimmed := strings.ToUpper(strings.TrimSpace(currency))
	if trimmed == "" {
		return "INR"
	}
	return trimmed
}

// normalizedStatementSource records how the amount arrived. Manual entry is
// the default and, when a statement is edited by hand, always wins over a
// value a parser supplied earlier.
func normalizedStatementSource(source string) string {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "", "manual":
		return "manual"
	case "sms":
		return "sms"
	case "email":
		return "email"
	default:
		return ""
	}
}
