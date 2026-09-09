package http

import (
	"testing"

	"finnri/internal/models"
)

func TestDecodeStatementAlertUsesReceivedDateWhenStatementDateMissing(t *testing.T) {
	input := statementAlertInput{
		Text:    "Your card statement total due is INR 12,345.67. Minimum due INR 600. Due by 2026-09-10.",
		Channel: "email",
	}
	parsed, err := decodeStatementAlert([]byte(`{
		"statement_date":"","due_date":"2026-09-10","total_due":12345.67,"minimum_due":600
	}`), input, "2026-08-27")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.StatementDate != "2026-08-27" || parsed.TotalDue != models.Money(1234567) {
		t.Fatalf("unexpected alert %#v", parsed)
	}
}

func TestDecodeStatementAlertRejectsAmountNotInSource(t *testing.T) {
	input := statementAlertInput{Text: "Total due INR 2,000 on 2026-08-27. Due 2026-09-10.", Channel: "sms"}
	_, err := decodeStatementAlert([]byte(`{
		"statement_date":"2026-08-27","due_date":"2026-09-10","total_due":9000,"minimum_due":0
	}`), input, "2026-08-27")
	if err == nil {
		t.Fatal("hallucinated amount must be rejected")
	}
}

func TestDecodeStatementAlertRejectsMinimumAboveTotal(t *testing.T) {
	input := statementAlertInput{Text: "Total due INR 2,000, minimum INR 3,000. Due 2026-09-10.", Channel: "sms"}
	_, err := decodeStatementAlert([]byte(`{
		"statement_date":"2026-08-27","due_date":"2026-09-10","total_due":2000,"minimum_due":3000
	}`), input, "2026-08-27")
	if err == nil {
		t.Fatal("minimum above total must be rejected")
	}
}

func TestStatementAlertAmountMatchingHandlesIndianGrouping(t *testing.T) {
	if !statementAlertContainsAmount("Amount due ₹1,23,456.78", models.Money(12345678)) {
		t.Fatal("grouped source amount must match")
	}
}
