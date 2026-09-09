package http

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"finnri/internal/models"
)

func TestExportEntriesCSVUsesFiltersAndOwnedRows(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)

	authResponse := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{"device_id": "export-owner-device"}, http.StatusOK,
	)
	otherAuth := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{"device_id": "export-other-device"}, http.StatusOK,
	)
	accounts := performJSONRequest[[]models.Account](
		t, router, http.MethodGet, "/v1/accounts", authResponse.Token, nil, http.StatusOK,
	)
	otherAccounts := performJSONRequest[[]models.Account](
		t, router, http.MethodGet, "/v1/accounts", otherAuth.Token, nil, http.StatusOK,
	)

	createExportEntry(t, router, authResponse.Token, accounts[0].ID, map[string]any{
		"title": "Coffee, beans", "type": "expense", "amount": 120.5, "currency": "INR",
		"source": "manual", "mode": "Cash", "category": "Food & Drinks", "merchant": "Cafe \"A\"",
		"date": "2026-07-12", "time": "09:15", "notes": "with, comma and \"quote\"",
		"tags": []string{"morning", "cafe"},
	})
	createExportEntry(t, router, authResponse.Token, accounts[0].ID, map[string]any{
		"title": "Metro", "type": "expense", "amount": 40, "currency": "INR",
		"source": "manual", "mode": "Cash", "category": "Transport", "merchant": "Metro",
		"date": "2026-07-13",
	})
	createExportEntry(t, router, otherAuth.Token, otherAccounts[0].ID, map[string]any{
		"title": "Other food", "type": "expense", "amount": 999, "currency": "INR",
		"source": "manual", "mode": "Cash", "category": "Food & Drinks", "merchant": "Other",
		"date": "2026-07-12",
	})

	response := performRawRequest(t, router, http.MethodGet, "/v1/entries/export?format=csv&category=Food+%26+Drinks", authResponse.Token, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("export status = %d, body = %s", response.Code, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); !strings.Contains(contentType, "text/csv") {
		t.Fatalf("expected CSV content type, got %q", contentType)
	}
	if disposition := response.Header().Get("Content-Disposition"); !strings.Contains(disposition, "finnri-entries.csv") {
		t.Fatalf("expected attachment disposition, got %q", disposition)
	}

	rows, err := csv.NewReader(strings.NewReader(response.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("failed to read CSV: %v\n%s", err, response.Body.String())
	}
	if len(rows) != 2 {
		t.Fatalf("expected header plus one filtered owner row, got %#v", rows)
	}
	header := rows[0]
	if strings.Join(header, ",") != strings.Join(entryExportCSVHeader, ",") {
		t.Fatalf("unexpected header: %#v", header)
	}
	row := rows[1]
	assertCSVValue(t, header, row, "title", "Coffee, beans")
	assertCSVValue(t, header, row, "amount", "120.50")
	assertCSVValue(t, header, row, "category", "Food & Drinks")
	assertCSVValue(t, header, row, "merchant", "Cafe \"A\"")
	assertCSVValue(t, header, row, "notes", "with, comma and \"quote\"")
	assertCSVValue(t, header, row, "tags", "morning|cafe")
	assertCSVValue(t, header, row, "account_name", "Cash")
	if strings.Contains(response.Body.String(), "Other food") || strings.Contains(response.Body.String(), "Metro") {
		t.Fatalf("export leaked unowned or unfiltered rows: %s", response.Body.String())
	}
}

func TestExportEntriesCSVSupportsEmptyAndRejectsInvalidFormat(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)

	authResponse := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{"device_id": "export-empty-device"}, http.StatusOK,
	)

	response := performRawRequest(t, router, http.MethodGet, "/v1/entries/export?format=csv", authResponse.Token, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("empty export status = %d, body = %s", response.Code, response.Body.String())
	}
	rows, err := csv.NewReader(strings.NewReader(response.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("failed to read empty CSV: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected only header row, got %#v", rows)
	}

	invalid := performRawRequest(t, router, http.MethodGet, "/v1/entries/export?format=xlsx", authResponse.Token, nil)
	if invalid.Code != http.StatusUnprocessableEntity || !strings.Contains(invalid.Body.String(), "must be csv") {
		t.Fatalf("unexpected invalid format response: status=%d body=%s", invalid.Code, invalid.Body.String())
	}
}

func createExportEntry(t *testing.T, router *gin.Engine, token string, accountID uint, payload map[string]any) {
	t.Helper()
	payload["account_id"] = accountID
	_ = performJSONRequest[models.Entry](t, router, http.MethodPost, "/v1/entries", token, payload, http.StatusCreated)
}

func performRawRequest(t *testing.T, router http.Handler, method, target, token string, body *strings.Reader) *httptest.ResponseRecorder {
	t.Helper()
	if body == nil {
		body = strings.NewReader("")
	}
	request := httptest.NewRequest(method, target, body)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func assertCSVValue(t *testing.T, header, row []string, column, want string) {
	t.Helper()
	for index, name := range header {
		if name == column {
			if index >= len(row) || row[index] != want {
				t.Fatalf("unexpected %s value: got row=%#v want %q", column, row, want)
			}
			return
		}
	}
	t.Fatalf("missing CSV column %q in header %#v", column, header)
}

// The PDF is written by hand, so the parts a reader will reject are worth
// asserting directly: the header, a cross-reference table whose offsets point
// at the objects they claim, and the trailer.
func TestExportEntriesPDFIsAWellFormedStatement(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)

	authResponse := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{"device_id": "export-pdf-device"}, http.StatusOK,
	)
	accounts := performJSONRequest[[]models.Account](
		t, router, http.MethodGet, "/v1/accounts", authResponse.Token, nil, http.StatusOK,
	)
	createExportEntry(t, router, authResponse.Token, accounts[0].ID, map[string]any{
		"title": "Coffee (large)", "type": "expense", "amount": 150, "currency": "INR",
		"source": "manual", "mode": "Cash", "category": "Food & Drinks", "merchant": "Cafe",
		"date": "2026-07-12", "time": "09:15",
	})
	createExportEntry(t, router, authResponse.Token, accounts[0].ID, map[string]any{
		"title": "Salary", "type": "income", "amount": 5000, "currency": "INR",
		"source": "manual", "mode": "Bank Account", "category": "Misc", "merchant": "Work",
		"date": "2026-07-01", "time": "10:00",
	})

	request := httptest.NewRequest(
		http.MethodGet,
		"/v1/entries/export?format=pdf&start_date=2026-07-01&end_date=2026-07-31&tz=Asia/Kolkata",
		nil,
	)
	request.Header.Set("Authorization", "Bearer "+authResponse.Token)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/pdf") {
		t.Fatalf("expected a PDF content type, got %q", got)
	}
	if got := recorder.Header().Get("Content-Disposition"); !strings.Contains(got, "finnri-statement.pdf") {
		t.Fatalf("unexpected disposition: %q", got)
	}

	body := recorder.Body.String()
	if !strings.HasPrefix(body, "%PDF-1.4") || !strings.HasSuffix(body, "%%EOF\n") {
		t.Fatal("body is not delimited as a PDF")
	}
	for _, needle := range []string{
		"Finnri statement",
		"1 Jul 2026 to 31 Jul 2026", // the period label, spelled for a human
		"2026-07-12",                // row dates stay sortable
		`Coffee \(large\)`,          // the parenthesis that would end the literal early
		"INR 5000.00",               // income counted
		"/BaseFont /Helvetica",      // a font every reader already has
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("statement is missing %q", needle)
		}
	}
	assertPDFXrefOffsets(t, body)
}

// A PDF whose xref offsets are wrong opens as a blank or damaged document in
// most readers rather than failing loudly, so the offsets are checked against
// the bytes they index.
func assertPDFXrefOffsets(t *testing.T, body string) {
	t.Helper()
	marker := strings.LastIndex(body, "startxref")
	if marker < 0 {
		t.Fatal("no startxref")
	}
	var xrefStart int
	if _, err := fmt.Sscanf(body[marker:], "startxref\n%d", &xrefStart); err != nil {
		t.Fatalf("unreadable startxref: %v", err)
	}
	if xrefStart <= 0 || xrefStart >= len(body) || !strings.HasPrefix(body[xrefStart:], "xref\n") {
		t.Fatalf("startxref %d does not point at the xref table", xrefStart)
	}

	var count int
	if _, err := fmt.Sscanf(body[xrefStart:], "xref\n0 %d", &count); err != nil {
		t.Fatalf("unreadable xref header: %v", err)
	}
	// Entries are fixed-width: "xref\n" + "0 N\n" then 20 bytes each.
	entriesAt := xrefStart + len("xref\n") + len(fmt.Sprintf("0 %d\n", count))
	for number := 1; number < count; number++ {
		entry := body[entriesAt+number*20 : entriesAt+(number+1)*20]
		var offset int
		if _, err := fmt.Sscanf(entry, "%d", &offset); err != nil {
			t.Fatalf("unreadable xref entry %d: %v", number, err)
		}
		want := fmt.Sprintf("%d 0 obj", number)
		if !strings.HasPrefix(body[offset:], want) {
			t.Fatalf("xref entry %d points at %q, not %q", number, body[offset:offset+12], want)
		}
	}
}

func TestExportEntriesRejectsAnUnknownFormat(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)
	authResponse := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{"device_id": "export-format-device"}, http.StatusOK,
	)
	request := httptest.NewRequest(http.MethodGet, "/v1/entries/export?format=xlsx", nil)
	request.Header.Set("Authorization", "Bearer "+authResponse.Token)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d", recorder.Code)
	}
}

// An empty result is a real answer to a narrow filter, and it still has to be a
// file a reader will open rather than a zero-page document.
func TestExportEntriesPDFRendersAnEmptyStatement(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)
	authResponse := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{"device_id": "export-empty-device"}, http.StatusOK,
	)
	request := httptest.NewRequest(
		http.MethodGet, "/v1/entries/export?format=pdf&start_date=2020-01-01&end_date=2020-01-31", nil,
	)
	request.Header.Set("Authorization", "Bearer "+authResponse.Token)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "0 transactions") {
		t.Error("an empty statement should say so")
	}
	assertPDFXrefOffsets(t, body)
}
