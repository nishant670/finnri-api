package monitoring

import (
	"testing"

	"github.com/getsentry/sentry-go"
)

func TestInitIsInertWithoutDSN(t *testing.T) {
	t.Setenv("SENTRY_DSN", "")
	if err := Init(); err != nil {
		t.Fatalf("Init without DSN: %v", err)
	}
	if Configured() {
		t.Fatal("monitoring should be disabled without SENTRY_DSN")
	}
}

func TestSanitizeEventRemovesFinanceAndIdentityData(t *testing.T) {
	event := &sentry.Event{
		Request: &sentry.Request{
			URL:         "https://api.finnri.app/v1/split/invites/secret-token/preview",
			Data:        `{"amount":2499,"merchant":"Blue Tokai"}`,
			QueryString: "token=secret",
			Cookies:     "session=secret",
		},
		Extra: map[string]interface{}{
			"amount":   2499,
			"merchant": "Blue Tokai",
			"screen":   "transactions",
			"nested": map[string]interface{}{
				"pin":               "1234",
				"accountIdentifier": "4111",
				"harmless":          "ok",
			},
		},
		Contexts: map[string]sentry.Context{
			"entry": {"note": "coffee with Priya", "kind": "expense"},
		},
		Breadcrumbs: []*sentry.Breadcrumb{
			{Category: "console", Message: "saved 2499"},
			{Category: "navigation", Data: map[string]interface{}{"email": "someone@example.com", "screen": "home"}},
		},
	}

	got := sanitizeEvent(event, nil)
	if got.Request.URL != "" || got.Request.Data != "" || got.Request.QueryString != "" || got.Request.Cookies != "" {
		t.Fatal("request URL, payload, query, and cookies must be removed")
	}
	if got.Extra["amount"] != "[redacted]" || got.Extra["merchant"] != "[redacted]" {
		t.Fatal("financial fields were not redacted")
	}
	nested := got.Extra["nested"].(map[string]interface{})
	if nested["pin"] != "[redacted]" || nested["accountIdentifier"] != "[redacted]" || nested["harmless"] != "ok" {
		t.Fatal("nested identity redaction did not preserve harmless context")
	}
	if got.Contexts["entry"]["note"] != "[redacted]" || got.Contexts["entry"]["kind"] != "expense" {
		t.Fatal("context redaction failed")
	}
	if got.Breadcrumbs[0] != nil {
		t.Fatal("console breadcrumb should be dropped")
	}
	if got.Breadcrumbs[1].Data["email"] != "[redacted]" || got.Breadcrumbs[1].Data["screen"] != "home" {
		t.Fatal("breadcrumb redaction failed")
	}
}
