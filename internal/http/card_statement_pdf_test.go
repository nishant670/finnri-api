package http

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	pdfcpu "github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// A real encrypted PDF, built here and read back, so the decrypt path is
// exercised rather than assumed.
func TestExtractStatementTextDecryptsAProtectedPDF(t *testing.T) {
	dir := t.TempDir()
	plainPath := filepath.Join(dir, "plain.pdf")
	if err := writeMinimalPDF(t, plainPath); err != nil {
		t.Skipf("could not build a test PDF: %v", err)
	}

	plain, err := os.ReadFile(plainPath)
	if err != nil {
		t.Fatal(err)
	}

	// Unencrypted reads without a password.
	if _, err := extractStatementText(plain, ""); err != nil {
		t.Fatalf("plain PDF did not read: %v", err)
	}

	// Now encrypt it and confirm the password is genuinely required.
	encryptedPath := filepath.Join(dir, "locked.pdf")
	conf := model.NewDefaultConfiguration()
	conf.UserPW = "statement-pw"
	conf.OwnerPW = "statement-pw"
	if err := pdfcpu.EncryptFile(plainPath, encryptedPath, conf); err != nil {
		t.Skipf("could not encrypt the test PDF: %v", err)
	}
	encrypted, err := os.ReadFile(encryptedPath)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := extractStatementText(encrypted, ""); err != errStatementPasswordRequired {
		t.Fatalf("no-password error = %v, want errStatementPasswordRequired", err)
	}
	if _, err := extractStatementText(encrypted, "wrong-one"); err != errStatementPasswordWrong {
		t.Fatalf("wrong-password error = %v, want errStatementPasswordWrong", err)
	}
	if _, err := extractStatementText(encrypted, "statement-pw"); err != nil {
		t.Fatalf("correct password still failed: %v", err)
	}
}

// writeMinimalPDF emits a tiny single-page PDF with one text run.
func writeMinimalPDF(t *testing.T, path string) error {
	t.Helper()
	return writeTextPDF(t, path, "BT /F1 12 Tf 72 720 Td (10/07/2026 SWIGGY BANGALORE 480.00) Tj ET")
}

// writeTextPDF writes a one-page PDF whose page content stream is `content`.
func writeTextPDF(t *testing.T, path, content string) error {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")

	offsets := []int{}
	object := func(body string) {
		offsets = append(offsets, buf.Len())
		buf.WriteString(body)
	}
	object("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	object("2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")
	object("3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] " +
		"/Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>\nendobj\n")
	object("4 0 obj\n<< /Length " + itoaLen(content) + " >>\nstream\n" + content + "\nendstream\nendobj\n")
	object("5 0 obj\n<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>\nendobj\n")

	xref := buf.Len()
	buf.WriteString("xref\n0 6\n0000000000 65535 f \n")
	for _, offset := range offsets {
		buf.WriteString(padOffset(offset) + " 00000 n \n")
	}
	buf.WriteString("trailer\n<< /Size 6 /Root 1 0 R >>\nstartxref\n")
	buf.WriteString(itoa(xref))
	buf.WriteString("\n%%EOF\n")

	return os.WriteFile(path, buf.Bytes(), 0o600)
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := []byte{}
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

func itoaLen(value string) string { return itoa(len(value)) }

func padOffset(offset int) string {
	text := itoa(offset)
	for len(text) < 10 {
		text = "0" + text
	}
	return text
}
