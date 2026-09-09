package http

import (
	"strings"
	"testing"

	"finnri/internal/models"
)

func TestParseStatementRowAcrossIssuerFormats(t *testing.T) {
	cases := []struct {
		name            string
		row             string
		wantDate        string
		wantDescription string
		wantAmount      models.Money
		wantType        string
	}{
		{
			name:     "slash dates with grouped amount",
			row:      "10/07/2026  SWIGGY BANGALORE IN  1,480.00",
			wantDate: "2026-07-10", wantDescription: "SWIGGY BANGALORE IN",
			wantAmount: rupees(1480), wantType: "expense",
		},
		{
			name:     "hyphen dates",
			row:      "14-07-2026 UBER INDIA SYSTEMS 260.00",
			wantDate: "2026-07-14", wantDescription: "UBER INDIA SYSTEMS",
			wantAmount: rupees(260), wantType: "expense",
		},
		{
			name:     "month-name dates",
			row:      "20 Jul 2026  CROMA RETAIL MUMBAI  5,000.00",
			wantDate: "2026-07-20", wantDescription: "CROMA RETAIL MUMBAI",
			wantAmount: rupees(5000), wantType: "expense",
		},
		{
			name:     "ISO dates",
			row:      "2026-07-22  AMAZON PAY  349.50",
			wantDate: "2026-07-22", wantDescription: "AMAZON PAY",
			// Paise are kept: a ₹349.50 charge is not a ₹349 charge.
			wantAmount: models.Money(34950), wantType: "expense",
		},
		{
			// Statements print magnitudes and annotate direction, so the
			// marker is the only thing separating a refund from a purchase.
			name:     "a credit marked CR",
			row:      "18/07/2026  AMAZON REFUND  800.00 CR",
			wantDate: "2026-07-18", wantDescription: "AMAZON REFUND",
			wantAmount: rupees(800), wantType: "income",
		},
		{
			name:     "a debit explicitly marked DR",
			row:      "18/07/2026  BOOKMYSHOW  450.00 Dr",
			wantDate: "2026-07-18", wantDescription: "BOOKMYSHOW",
			wantAmount: rupees(450), wantType: "expense",
		},
		{
			name:     "a rupee symbol in front of the amount",
			row:      "10/07/2026  ZOMATO  ₹ 620.00",
			wantDate: "2026-07-10", wantDescription: "ZOMATO",
			wantAmount: rupees(620), wantType: "expense",
		},
		{
			name:     "lakh-grouped amounts",
			row:      "05/07/2026  JEWELLERY PURCHASE  1,25,000.00",
			wantDate: "2026-07-05", wantDescription: "JEWELLERY PURCHASE",
			wantAmount: rupees(125000), wantType: "expense",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line, ok := parseStatementRow(tc.row, 2026)
			if !ok {
				t.Fatalf("row was not recognised: %q", tc.row)
			}
			if line.Date != tc.wantDate {
				t.Errorf("date = %s, want %s", line.Date, tc.wantDate)
			}
			if line.Description != tc.wantDescription {
				t.Errorf("description = %q, want %q", line.Description, tc.wantDescription)
			}
			if line.Amount != tc.wantAmount {
				t.Errorf("amount = %s, want %s", line.Amount, tc.wantAmount)
			}
			if line.Type != tc.wantType {
				t.Errorf("type = %s, want %s", line.Type, tc.wantType)
			}
		})
	}
}

// Skipping a row it cannot read is the correct failure mode: an unread row
// shows up as an unaccounted difference the user can add by hand, while a
// misread row becomes a wrong transaction they have to notice and undo.
func TestParserIgnoresEverythingThatIsNotATransaction(t *testing.T) {
	for _, row := range []string{
		"",
		"HDFC BANK CREDIT CARD STATEMENT",
		"Statement Date: 05/08/2026",
		"Total Amount Due    12,400.00",
		"Reward Points Earned    1,240",
		"Page 1 of 4",
		"10/07/2026",
		"10/07/2026    5,000.00",
		"Please pay by 25/08/2026 to avoid charges",
	} {
		if line, ok := parseStatementRow(row, 2026); ok {
			t.Errorf("row %q was read as a transaction: %+v", row, line)
		}
	}
}

func TestParseStatementTextReadsAWholeStatement(t *testing.T) {
	text := `
HDFC BANK CREDIT CARD STATEMENT
Card Number: XXXX XXXX XXXX 4321
Statement Date: 05/08/2026

Date        Description                     Amount
10/07/2026  SWIGGY BANGALORE IN             480.00
14/07/2026  UBER INDIA SYSTEMS              260.00
18/07/2026  AMAZON REFUND                   800.00 CR
20/07/2026  EMI INSTALMENT 3/12 CROMA     5,000.00
25/07/2026  PAYMENT RECEIVED - THANK YOU  8,000.00 CR
05/08/2026  LATE PAYMENT FEE                500.00

Total Amount Due                          12,400.00
Page 1 of 2
`
	lines := parseStatementText(text, 2026)
	if len(lines) != 6 {
		for _, line := range lines {
			t.Logf("parsed: %+v", line)
		}
		t.Fatalf("parsed %d rows, want 6", len(lines))
	}

	kinds := map[string]int{}
	for _, line := range lines {
		kinds[line.Kind]++
	}
	if kinds[lineKindPayment] != 1 {
		t.Errorf("payments = %d, want 1", kinds[lineKindPayment])
	}
	if kinds[lineKindRefund] != 1 {
		t.Errorf("refunds = %d, want 1", kinds[lineKindRefund])
	}
	if kinds[lineKindFee] != 1 {
		t.Errorf("fees = %d, want 1", kinds[lineKindFee])
	}
	if kinds[lineKindEMI] != 1 {
		t.Errorf("EMI rows = %d, want 1", kinds[lineKindEMI])
	}
}

// The full card number must never survive into anything downstream.
func TestMaskCardNumbers(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"spaced groups", "Card 4532 1234 5678 9012 issued", "Card XXXX XXXX XXXX 9012 issued"},
		{"hyphen groups", "4532-1234-5678-9012", "XXXX XXXX XXXX 9012"},
		{"unbroken digits", "4532123456789012", "XXXX XXXX XXXX 9012"},
		{"15-digit Amex", "3782 822463 10005", "XXXX XXXX XXXX 0005"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := maskCardNumbers(tc.in); got != tc.want {
				t.Fatalf("masked = %q, want %q", got, tc.want)
			}
		})
	}
}

// Masking must not eat ordinary statement numbers.
func TestMaskCardNumbersLeavesAmountsAndDatesAlone(t *testing.T) {
	for _, text := range []string{
		"10/07/2026  SWIGGY  1,480.00",
		"Total Amount Due 12,400.00",
		"Reward points 1240",
		"2026-07-10",
	} {
		if got := maskCardNumbers(text); got != text {
			t.Fatalf("masking altered %q into %q", text, got)
		}
	}
}

// Neither error may carry the attempted password.
func TestPasswordErrorsRevealNothing(t *testing.T) {
	for _, err := range []error{errStatementPasswordRequired, errStatementPasswordWrong} {
		message := strings.ToLower(err.Error())
		for _, leak := range []string{"secret", "hunter2", "pw=", "password:"} {
			if strings.Contains(message, leak) {
				t.Fatalf("error message leaks credential material: %q", message)
			}
		}
	}
}

// Only real PDFs get through, and the check is on the bytes rather than the
// filename a client supplied.
func TestReadUploadedPDFRejectsNonPDFs(t *testing.T) {
	if _, err := readUploadedPDF(strings.NewReader("GIF89a not a pdf")); err == nil {
		t.Fatal("a non-PDF was accepted")
	}
	if _, err := readUploadedPDF(strings.NewReader("%PDF-1.7\nreal enough")); err != nil {
		t.Fatalf("a PDF was rejected: %v", err)
	}
}

func TestReadUploadedPDFRejectsOversizedFiles(t *testing.T) {
	oversized := "%PDF-1.7\n" + strings.Repeat("x", maxStatementPDFBytes)
	if _, err := readUploadedPDF(strings.NewReader(oversized)); err == nil {
		t.Fatal("an oversized upload was accepted")
	}
}
