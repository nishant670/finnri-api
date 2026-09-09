package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"finnri/internal/models"
)

func stringPointer(value string) *string { return &value }

func TestAccountProviderAliasesResolveToStableID(t *testing.T) {
	tests := map[string]string{
		"HDFC": "hdfc", "hdfc bank": "hdfc", "HDFC card": "hdfc",
		"Federal": "federal", "IDFC First Bank": "idfc-first", "BoB": "bank-of-baroda",
		"Digibank": "dbs", "scapia credit card": "scapia", "One Card credit card": "onecard",
		"Jupiter Federal Bank": "jupiter", "Fi Money": "fi",
	}
	for alias, expectedID := range tests {
		provider, ok := matchAccountProvider(alias)
		if !ok || provider.ID != expectedID {
			t.Fatalf("alias %q resolved to %#v, %v; want %q", alias, provider, ok, expectedID)
		}
	}
}

func TestExpandedAccountProvidersExposeCorrectTypes(t *testing.T) {
	tests := map[string][]string{
		"federal": {"bank", "credit_card", "debit_card"},
		"scapia":  {"credit_card"},
		"slice":   {"credit_card"},
		"onecard": {"credit_card"},
		"uni":     {"credit_card"},
		"jupiter": {"bank", "debit_card"},
		"fi":      {"bank", "debit_card"},
	}
	for id, supportedTypes := range tests {
		provider, ok := accountProviderByID(id)
		if !ok {
			t.Fatalf("provider %q missing", id)
		}
		for _, accountType := range supportedTypes {
			if !providerSupportsType(provider, accountType) {
				t.Fatalf("provider %q does not support %q: %#v", id, accountType, provider.TypeSupport)
			}
		}
	}
}

func TestListAccountProvidersFiltersBySupportedType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/v1/account-providers", listAccountProviders)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/account-providers?type=upi", nil)
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	var providers []accountProvider
	if err := json.Unmarshal(response.Body.Bytes(), &providers); err != nil {
		t.Fatal(err)
	}
	if len(providers) == 0 {
		t.Fatal("expected UPI providers")
	}
	for _, provider := range providers {
		if !providerSupportsType(provider, "upi") {
			t.Fatalf("non-UPI provider leaked into response: %#v", provider)
		}
	}
}

func TestAccountInputWritesStructuredMetadataAndLegacyFallback(t *testing.T) {
	input := accountInput{
		Type: "credit_card", Name: "Travel card", Provider: "HDFC card",
		Identifier: "1234", Last4: stringPointer("1234"),
	}
	if fields := input.validate(); len(fields) != 0 {
		t.Fatalf("unexpected fields %#v", fields)
	}
	account := models.Account{}
	input.apply(&account)
	if account.ProviderID != "hdfc" || account.Provider != "HDFC Bank" {
		t.Fatalf("provider not structured: %#v", account)
	}
	if account.Last4 != "1234" || account.Identifier != "1234" {
		t.Fatalf("metadata fallback not kept: %#v", account)
	}
}

func TestLegacyAccountEditUpdatesStructuredIdentifier(t *testing.T) {
	account := models.Account{Type: "credit_card", Last4: "1111", ProviderID: "hdfc", Provider: "HDFC Bank"}
	accountInput{Type: "credit_card", Name: "Card", Provider: "Custom issuer", Identifier: "9999"}.apply(&account)
	if account.Last4 != "9999" || account.ProviderID != "" || account.Provider != "Custom issuer" {
		t.Fatalf("legacy edit was not preserved: %#v", account)
	}
}

func TestAccountInputRejectsMetadataForWrongType(t *testing.T) {
	input := accountInput{Type: "wallet", Name: "Wallet", Last4: stringPointer("1234")}
	if input.validate()["last4"] == "" {
		t.Fatal("wallet last4 must be rejected")
	}
}
