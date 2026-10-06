package http

import (
	"testing"

	"finnri/internal/models"
)

func TestDiffPairsSameAmountLoggedOnADifferentDayAsProbable(t *testing.T) {
	lines := []statementLine{{Date: "2026-09-12", Description: "SWIGGY BANGALORE", Amount: rupees(640), Type: "expense"}}
	entries := []ledgerLine{{EntryID: 1, Date: "2026-09-02", Title: "Dinner", Amount: rupees(640), Type: "expense"}}

	diff := diffStatementLines(lines, entries)

	if len(diff.Probable) != 1 || diff.Probable[0].Reason != probableReasonDate || diff.Probable[0].DayGap != 10 {
		t.Fatalf("expected one date-probable pair, got %+v", diff.Probable)
	}
	if len(diff.Missing) != 0 || len(diff.Extra) != 0 {
		t.Fatalf("a probable pair must not also be missing or extra: %+v", diff)
	}
}

func TestDiffPairsRoundedAmountAsProbable(t *testing.T) {
	lines := []statementLine{{Date: "2026-09-12", Description: "AMAZON PAY", Amount: models.Money(49950), Type: "expense"}}
	entries := []ledgerLine{{EntryID: 1, Date: "2026-09-12", Title: "Charger", Amount: rupees(500), Type: "expense"}}

	diff := diffStatementLines(lines, entries)

	if len(diff.Probable) != 1 || diff.Probable[0].Reason != probableReasonAmount ||
		diff.Probable[0].AmountGap != models.Money(-50) {
		t.Fatalf("expected one amount-probable pair, got %+v", diff.Probable)
	}
}

func TestDiffPairsForexMarkupOnlyWhenTheMerchantAgrees(t *testing.T) {
	line := statementLine{Date: "2026-09-12", Description: "NETFLIX.COM LOS GATOS", Amount: rupees(1035), Type: "expense"}

	named := diffStatementLines([]statementLine{line}, []ledgerLine{
		{EntryID: 1, Date: "2026-09-11", Title: "Netflix", Amount: rupees(1000), Type: "expense"},
	})
	if len(named.Probable) != 1 {
		t.Fatalf("3.5%% markup on a matching merchant should be probable: %+v", named)
	}

	unrelated := diffStatementLines([]statementLine{line}, []ledgerLine{
		{EntryID: 1, Date: "2026-09-11", Title: "Groceries", Amount: rupees(1000), Type: "expense"},
	})
	if len(unrelated.Probable) != 0 || len(unrelated.Missing) != 1 || len(unrelated.Extra) != 1 {
		t.Fatalf("a near amount with nothing in common must stay separate: %+v", unrelated)
	}
}

func TestDiffPrefersAStrictMatchOverAProbableOne(t *testing.T) {
	lines := []statementLine{{Date: "2026-09-12", Description: "UBER", Amount: rupees(300), Type: "expense"}}
	entries := []ledgerLine{
		{EntryID: 1, Date: "2026-09-01", Title: "Cab", Amount: rupees(300), Type: "expense"},
		{EntryID: 2, Date: "2026-09-12", Title: "Cab", Amount: rupees(300), Type: "expense"},
	}

	diff := diffStatementLines(lines, entries)

	if len(diff.Matched) != 1 || diff.Matched[0].Entry.EntryID != 2 {
		t.Fatalf("the same-day entry should match strictly: %+v", diff.Matched)
	}
	if len(diff.Probable) != 0 || len(diff.Extra) != 1 || diff.Extra[0].EntryID != 1 {
		t.Fatalf("the other entry is extra, not probable: %+v", diff)
	}
}

func TestDiffMatchesAcrossTheCycleEdgeButNeverReportsTheMarginAsExtra(t *testing.T) {
	lines := []statementLine{{Date: "2026-09-06", Description: "ZOMATO", Amount: rupees(450), Type: "expense"}}
	entries := []ledgerLine{
		{EntryID: 1, Date: "2026-09-04", Title: "Lunch", Amount: rupees(450), Type: "expense", OutsideCycle: true},
		{EntryID: 2, Date: "2026-09-02", Title: "Old", Amount: rupees(99), Type: "expense", OutsideCycle: true},
	}

	diff := diffStatementLines(lines, entries)

	if len(diff.Matched) != 1 || diff.Matched[0].Entry.EntryID != 1 {
		t.Fatalf("an entry logged before the cycle opened should still match: %+v", diff)
	}
	if len(diff.Extra) != 0 {
		t.Fatalf("margin entries belong to the previous bill, not this one: %+v", diff.Extra)
	}
}

func TestDiffCollectsBankCharges(t *testing.T) {
	lines := []statementLine{
		{Date: "2026-09-05", Description: "ANNUAL FEE", Amount: rupees(500), Type: "expense"},
		{Date: "2026-09-05", Description: "IGST ON ANNUAL FEE", Amount: rupees(90), Type: "expense"},
		{Date: "2026-09-06", Description: "FINANCE CHARGES", Amount: rupees(120), Type: "expense"},
		{Date: "2026-09-07", Description: "MYNTRA", Amount: rupees(1500), Type: "expense"},
	}

	diff := diffStatementLines(lines, nil)

	if diff.Summary.ChargesCount != 3 || diff.Summary.ChargesAmount != rupees(710) {
		t.Fatalf("charges = %d / %v, want 3 / ₹710", diff.Summary.ChargesCount, diff.Summary.ChargesAmount)
	}
	if len(diff.Missing) != 4 {
		t.Fatalf("charges are still importable rows: %+v", diff.Missing)
	}
}

func TestChecksumIgnoresTheBillPaymentRow(t *testing.T) {
	lines := []statementLine{
		{Date: "2026-09-08", Description: "PAYMENT RECEIVED - THANK YOU", Amount: rupees(10000), Type: "income"},
		{Date: "2026-09-10", Description: "MYNTRA", Amount: rupees(12000), Type: "expense"},
		{Date: "2026-09-11", Description: "MYNTRA REFUND", Amount: rupees(500), Type: "income"},
		{Date: "2026-09-12", Description: "LATE PAYMENT FEE", Amount: rupees(900), Type: "expense"},
	}

	// Previous bill ₹10,000 paid in full; this bill is the cycle's net spend.
	opening := rupees(10000)
	known := checksumStatementLines(lines, rupees(12400), &opening)
	if !known.Matches || known.Payments != rupees(10000) || known.ParsedNet != rupees(12400) {
		t.Fatalf("paid-in-full cycle should balance: %+v", known)
	}

	// No earlier bill in Finnri: assumed paid in full, same answer.
	assumed := checksumStatementLines(lines, rupees(12400), nil)
	if !assumed.Matches {
		t.Fatalf("assumed opening should balance too: %+v", assumed)
	}

	short := checksumStatementLines(lines[:2], rupees(12400), &opening)
	if short.Matches || short.Difference != rupees(-400) {
		t.Fatalf("a missing row must show as a gap: %+v", short)
	}
}
