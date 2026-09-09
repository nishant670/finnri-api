package http

import (
	"testing"

	"finnri/internal/models"
)

func TestDecodeStatementImageLinesNormalizesModelOutput(t *testing.T) {
	raw := []byte("```json\n" + `{"lines":[
		{"date":"2026-08-01","description":"  AMAZON   REFUND ","amount":499,"type":"INCOME"},
		{"date":"2026-08-02","description":"SWIGGY","amount":"320.50","type":"debit"}
	]}` + "\n```")
	lines, err := decodeStatementImageLines(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || lines[0].Description != "AMAZON REFUND" || lines[0].Type != "income" {
		t.Fatalf("unexpected first line %#v", lines)
	}
	if lines[1].Type != "expense" || lines[1].Amount != models.Money(32050) {
		t.Fatalf("unexpected debit %#v", lines[1])
	}
}

func TestDedupeStatementLinesReusesImportIdempotencyKey(t *testing.T) {
	duplicate := statementLine{Date: "2026-08-01", Description: "SWIGGY", Amount: models.Money(32000), Type: "expense"}
	lines := dedupeStatementLines(42, []statementLine{
		duplicate,
		duplicate,
		{Date: "2026-08-02", Description: "SWIGGY", Amount: models.Money(32000), Type: "expense"},
	})
	if len(lines) != 2 {
		t.Fatalf("expected overlapping row to collapse, got %#v", lines)
	}
	if statementLineIdempotencyKey(42, lines[0]) == statementLineIdempotencyKey(42, lines[1]) {
		t.Fatal("distinct row must retain a distinct import key")
	}
}

func TestStatementScreenshotChecksumWarnsButKeepsDiffRows(t *testing.T) {
	lines := []statementLine{
		{Date: "2026-08-01", Description: "Purchase", Amount: models.Money(100000), Type: "expense"},
		{Date: "2026-08-02", Description: "Refund", Amount: models.Money(10000), Type: "income"},
	}
	checksum := checksumStatementLines(lines, models.Money(95000), 0)
	if checksum.Matches || checksum.Difference != models.Money(-5000) {
		t.Fatalf("unexpected checksum %#v", checksum)
	}
	diff := diffStatementLines(lines, nil)
	diff.Checksum = &checksum
	if len(diff.Missing) != 2 {
		t.Fatalf("checksum warning must not block rows: %#v", diff)
	}
}

func TestStatementScreenshotChecksumAllowsRupeeRounding(t *testing.T) {
	lines := []statementLine{{Date: "2026-08-01", Description: "Purchase", Amount: models.Money(100050), Type: "expense"}}
	checksum := checksumStatementLines(lines, models.Money(100000), 0)
	if !checksum.Matches {
		t.Fatalf("one-rupee tolerance should match: %#v", checksum)
	}
}
