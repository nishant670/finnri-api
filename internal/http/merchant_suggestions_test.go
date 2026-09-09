package http

import (
	"net/http"
	"testing"

	"finnri/internal/models"
)

func TestMerchantSuggestionsUseOwnedHistoryAndCategoryAssociation(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)

	authResponse := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{"device_id": "merchant-owner-device"}, http.StatusOK,
	)
	otherAuth := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{"device_id": "merchant-other-device"}, http.StatusOK,
	)
	accounts := performJSONRequest[[]models.Account](
		t, router, http.MethodGet, "/v1/accounts", authResponse.Token, nil, http.StatusOK,
	)
	otherAccounts := performJSONRequest[[]models.Account](
		t, router, http.MethodGet, "/v1/accounts", otherAuth.Token, nil, http.StatusOK,
	)

	createExportEntry(t, router, authResponse.Token, accounts[0].ID, map[string]any{
		"title": "Swiggy lunch", "type": "expense", "amount": 320, "currency": "INR",
		"source": "manual", "mode": "Cash", "category": "Food", "merchant": "Swiggy",
		"date": "2026-07-12",
	})
	createExportEntry(t, router, authResponse.Token, accounts[0].ID, map[string]any{
		"title": "Swiggy dinner", "type": "expense", "amount": 450, "currency": "INR",
		"source": "manual", "mode": "Cash", "category": "Food", "merchant": "swiggy",
		"date": "2026-07-20",
	})
	createExportEntry(t, router, authResponse.Token, accounts[0].ID, map[string]any{
		"title": "Instamart", "type": "expense", "amount": 800, "currency": "INR",
		"source": "manual", "mode": "Cash", "category": "Shopping", "merchant": "Swiggy Instamart",
		"date": "2026-07-21",
	})
	createExportEntry(t, router, authResponse.Token, accounts[0].ID, map[string]any{
		"title": "Coffee", "type": "expense", "amount": 120, "currency": "INR",
		"source": "manual", "mode": "Cash", "category": "Food", "merchant": "Cafe",
		"date": "2026-07-22",
	})
	createExportEntry(t, router, otherAuth.Token, otherAccounts[0].ID, map[string]any{
		"title": "Other", "type": "expense", "amount": 9999, "currency": "INR",
		"source": "manual", "mode": "Cash", "category": "Travel", "merchant": "Swiggy Other",
		"date": "2026-07-23",
	})

	response := performJSONRequest[MerchantSuggestionsResponse](
		t, router, http.MethodGet, "/v1/merchants/suggestions?q=swig&limit=5",
		authResponse.Token, nil, http.StatusOK,
	)

	if len(response.Suggestions) != 2 {
		t.Fatalf("expected two owner swig suggestions, got %#v", response.Suggestions)
	}
	// The Swiggy entries above were saved with the legacy "Food" category, which
	// older app builds still send. They come back canonicalized.
	first := response.Suggestions[0]
	if first.Merchant != "swiggy" || first.Category != "Food & Drinks" ||
		first.TransactionCount != 2 || first.LastSeenDate != "2026-07-20" {
		t.Fatalf("unexpected first suggestion: %#v", first)
	}
	second := response.Suggestions[1]
	if second.Merchant != "Swiggy Instamart" || second.Category != "Shopping" ||
		second.TransactionCount != 1 {
		t.Fatalf("unexpected second suggestion: %#v", second)
	}
	for _, suggestion := range response.Suggestions {
		if suggestion.Merchant == "Swiggy Other" || suggestion.Category == "Travel" {
			t.Fatalf("suggestions leaked another user's history: %#v", response.Suggestions)
		}
	}
}

func TestMerchantSuggestionsSupportRecentDefaultsAndRejectInvalidLimit(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)

	authResponse := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{"device_id": "merchant-empty-device"}, http.StatusOK,
	)
	accounts := performJSONRequest[[]models.Account](
		t, router, http.MethodGet, "/v1/accounts", authResponse.Token, nil, http.StatusOK,
	)

	createExportEntry(t, router, authResponse.Token, accounts[0].ID, map[string]any{
		"title": "Metro", "type": "expense", "amount": 40, "currency": "INR",
		"source": "manual", "mode": "Cash", "category": "Travel", "merchant": "Metro",
		"date": "2026-07-12",
	})

	response := performJSONRequest[MerchantSuggestionsResponse](
		t, router, http.MethodGet, "/v1/merchants/suggestions?limit=1",
		authResponse.Token, nil, http.StatusOK,
	)
	if len(response.Suggestions) != 1 || response.Suggestions[0].Merchant != "Metro" {
		t.Fatalf("unexpected default suggestions: %#v", response.Suggestions)
	}

	_ = performJSONRequest[map[string]any](
		t, router, http.MethodGet, "/v1/merchants/suggestions?limit=50",
		authResponse.Token, nil, http.StatusUnprocessableEntity,
	)
}
