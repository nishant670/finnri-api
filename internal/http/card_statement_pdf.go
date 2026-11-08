package http

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/ledongthuc/pdf"
	pdfcpu "github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

/*
Reading a statement PDF.

Rules this file exists to enforce, in order of how badly they would hurt if
broken:

 1. **The password is transient.** It decrypts the bytes in this request and is
    discarded with them. It is never persisted, never logged, never put in an
    error message, and never sent anywhere. There is deliberately no way to
    "remember" it: a stored statement password is a stored credential, with all
    the breach surface that implies, to save the user one typed field a month.

 2. **The file is not retained.** Extract, diff, discard. A statement is the
    user's complete spending for a month, plus their card number and address.
    Keeping a copy to save a round trip is not a trade worth making.

 3. **Card numbers are masked before anything else touches the text.** The
    parsed rows go on to matching and, in future, possibly to a model. The full
    PAN never needs to travel with them.

Nothing here retries a wrong password on the user's behalf either — a decrypt
endpoint that accepts repeated guesses is an oracle.
*/

// maxStatementPDFBytes caps what will be read into memory. Card statements are
// a few hundred kilobytes; anything far larger is not a statement.
const maxStatementPDFBytes = 12 << 20 // 12 MiB

// errStatementPasswordRequired and errStatementPasswordWrong are separated so
// the app can ask for a password rather than reporting a corrupt file, but
// neither carries the attempted password.
var (
	errStatementPasswordRequired = fmt.Errorf("statement is password protected")
	errStatementPasswordWrong    = fmt.Errorf("statement password did not work")
)

// panPattern matches a 13-19 digit card number, optionally grouped. Deliberately
// greedy about separators so "4532 1234 5678 9012" and "4532-1234-5678-9012"
// are both caught.
var panPattern = regexp.MustCompile(`\b(?:\d[ -]?){12,18}\d\b`)

// maskCardNumbers reduces any card number in the text to its last four digits.
//
// Applied to the extracted text before parsing, so nothing downstream — the
// matcher, the response, a log line, a future model call — ever holds a full
// PAN.
func maskCardNumbers(text string) string {
	return panPattern.ReplaceAllStringFunc(text, func(match string) string {
		digits := strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, match)
		if len(digits) < 13 {
			return match
		}
		return "XXXX XXXX XXXX " + digits[len(digits)-4:]
	})
}

// extractStatementText opens a statement PDF and returns its text with card
// numbers already masked.
//
// `password` is used here and nowhere else. The caller passes it straight from
// the request and keeps no copy; this function keeps none either.
func extractStatementText(data []byte, password string) (string, error) {
	// Unencrypted is the easy path and the common one for emailed summaries.
	text, err := readPDFText(data)
	if err == nil {
		return maskCardNumbers(text), nil
	}

	if password == "" {
		return "", errStatementPasswordRequired
	}

	decrypted, err := decryptPDF(data, password)
	if err != nil {
		return "", errStatementPasswordWrong
	}

	text, err = readPDFText(decrypted)
	if err != nil {
		return "", errStatementPasswordWrong
	}
	return maskCardNumbers(text), nil
}

// decryptPDF removes the standard security handler, in memory.
//
// pdfcpu handles more encryption variants than the text reader does, so it is
// used purely as a decryption stage; the plaintext bytes never reach disk.
func decryptPDF(data []byte, password string) ([]byte, error) {
	configuration := model.NewDefaultConfiguration()
	configuration.UserPW = password
	configuration.OwnerPW = password
	// Statements are read-only inputs; validating them strictly rejects
	// perfectly readable files that banks produce with minor spec violations.
	configuration.ValidationMode = model.ValidationRelaxed

	var out bytes.Buffer
	if err := pdfcpu.Decrypt(bytes.NewReader(data), &out, configuration); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// readPDFText lays each page out from where its text sits (see
// card_statement_layout.go). The library's own row reader is kept only as a
// fallback for files whose glyph positions yield no transactions at all: it
// fuses cells and cannot see some positioning, so it never outranks a layout
// that found rows.
func readPDFText(data []byte) (string, error) {
	reader, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}

	var positioned, byRow strings.Builder
	for pageIndex := 1; pageIndex <= reader.NumPage(); pageIndex++ {
		page := reader.Page(pageIndex)
		if page.V.IsNull() {
			continue
		}
		for _, line := range positionedPageLines(page) {
			positioned.WriteString(line)
			positioned.WriteByte('\n')
		}
		rows, err := page.GetTextByRow()
		if err != nil {
			// A page that will not parse is not a reason to lose the rest of
			// the statement.
			continue
		}
		for _, row := range rows {
			var line strings.Builder
			for _, word := range row.Content {
				line.WriteString(word.S)
			}
			byRow.WriteString(strings.TrimSpace(line.String()))
			byRow.WriteByte('\n')
		}
	}

	text := positioned.String()
	if strings.TrimSpace(text) == "" ||
		(len(parseStatementText(text, 0)) == 0 && len(parseStatementText(byRow.String(), 0)) > 0) {
		text = byRow.String()
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("no readable text")
	}
	return text, nil
}

// readUploadedPDF reads at most maxStatementPDFBytes and confirms the bytes
// really are a PDF, rather than trusting the filename or content type.
func readUploadedPDF(source io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(source, maxStatementPDFBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxStatementPDFBytes {
		return nil, fmt.Errorf("file is too large")
	}
	if !bytes.HasPrefix(data, []byte("%PDF-")) {
		return nil, fmt.Errorf("not a PDF")
	}
	return data, nil
}
