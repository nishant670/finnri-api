package http

import (
	"net/http"
	"net/url"
	"testing"

	"finnri/internal/database"
	"finnri/internal/models"
)

type entryListPage struct {
	Entries        []models.Entry   `json:"entries"`
	Total          int64            `json:"total"`
	CategoryCounts map[string]int64 `json:"category_counts"`
}

// Every mode the API will save, the API must also filter on. These drifted apart
// once and "Bank Account" ended up recordable but unsearchable — the app
// rendered its 422 as "Unable to load entries right now".
func TestCanonicalModesAcceptedOnSaveAndFilter(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)

	auth := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{"device_id": "mode-parity-device"}, http.StatusOK,
	)
	accounts := performJSONRequest[[]models.Account](
		t, router, http.MethodGet, "/v1/accounts", auth.Token, nil, http.StatusOK,
	)

	for _, mode := range canonicalModes {
		createExportEntry(t, router, auth.Token, accounts[0].ID, map[string]any{
			"title": "Entry " + mode, "type": "expense", "amount": 100, "currency": "INR",
			"source": "manual", "mode": mode, "category": "Misc", "date": "2026-07-12",
		})
	}

	for _, mode := range canonicalModes {
		page := performJSONRequest[entryListPage](
			t, router, http.MethodGet, "/v1/entries?mode="+url.QueryEscape(mode),
			auth.Token, nil, http.StatusOK,
		)
		if page.Total != 1 {
			t.Fatalf("mode %q saved but filtering returned %d entries", mode, page.Total)
		}
		if page.Entries[0].Mode != mode {
			t.Fatalf("mode %q filter returned an entry with mode %q", mode, page.Entries[0].Mode)
		}
	}
}

func TestEntryModeDerivesFromOwnedAccount(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)
	auth := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{"device_id": "derived-mode-device"}, http.StatusOK,
	)

	accounts := []models.Account{
		{UserID: auth.User.ID, Type: "bank", Name: "Salary account"},
		{UserID: auth.User.ID, Type: "debit_card", Name: "Salary debit card"},
		{UserID: auth.User.ID, Type: "other", Name: "Unclassified rail"},
	}
	if err := database.DB.Create(&accounts).Error; err != nil {
		t.Fatal(err)
	}

	base := map[string]any{
		"title": "Derived mode", "type": "expense", "amount": 100, "currency": "INR",
		"source": "manual", "category": "Misc", "date": "2026-08-20",
	}
	for _, test := range []struct {
		account models.Account
		want    string
	}{
		{account: accounts[0], want: "Bank Account"},
		{account: accounts[1], want: "Bank Account"},
	} {
		payload := make(map[string]any, len(base)+1)
		for key, value := range base {
			payload[key] = value
		}
		payload["account_id"] = test.account.ID
		saved := performJSONRequest[models.Entry](
			t, router, http.MethodPost, "/v1/entries", auth.Token, payload, http.StatusCreated,
		)
		if saved.Mode != test.want {
			t.Fatalf("account type %q derived mode %q, want %q", test.account.Type, saved.Mode, test.want)
		}
	}

	otherPayload := make(map[string]any, len(base)+1)
	for key, value := range base {
		otherPayload[key] = value
	}
	otherPayload["account_id"] = accounts[2].ID
	rejected := performJSONRequest[struct {
		Fields map[string]string `json:"fields"`
	}](t, router, http.MethodPost, "/v1/entries", auth.Token, otherPayload, http.StatusUnprocessableEntity)
	if rejected.Fields["mode"] != "is required when account type is other" {
		t.Fatalf("missing mode for other account = %#v", rejected.Fields)
	}

	otherPayload["mode"] = "Wallets"
	savedOther := performJSONRequest[models.Entry](
		t, router, http.MethodPost, "/v1/entries", auth.Token, otherPayload, http.StatusCreated,
	)
	if savedOther.Mode != "Wallets" {
		t.Fatalf("explicit other-account mode = %q, want Wallets", savedOther.Mode)
	}
}

func TestModeFilterResolvesAliasesAndRejectsUnknowns(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)

	auth := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{"device_id": "mode-alias-device"}, http.StatusOK,
	)
	accounts := performJSONRequest[[]models.Account](
		t, router, http.MethodGet, "/v1/accounts", auth.Token, nil, http.StatusOK,
	)
	createExportEntry(t, router, auth.Token, accounts[0].ID, map[string]any{
		"title": "Rent", "type": "expense", "amount": 18000, "currency": "INR",
		"source": "manual", "mode": "Bank Account", "category": "Bills", "date": "2026-07-01",
	})

	page := performJSONRequest[entryListPage](
		t, router, http.MethodGet, "/v1/entries?mode=bank", auth.Token, nil, http.StatusOK,
	)
	if page.Total != 1 {
		t.Fatalf("alias 'bank' should find the Bank Account row, got %d", page.Total)
	}

	// "Debit Card" was offered by the filter sheet, matches no row and is not a
	// mode. It stays a 422 rather than being aliased onto a neighbouring mode:
	// guessing which one the user meant is how the taxonomy broke in the first place.
	rejected := performRawRequest(t, router, http.MethodGet, "/v1/entries?mode=Debit+Card", auth.Token, nil)
	if rejected.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for an unknown mode, got %d: %s", rejected.Code, rejected.Body.String())
	}
}

func TestEntrySortOrdersTheWholeFilteredSet(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)

	auth := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{"device_id": "sort-device"}, http.StatusOK,
	)
	accounts := performJSONRequest[[]models.Account](
		t, router, http.MethodGet, "/v1/accounts", auth.Token, nil, http.StatusOK,
	)

	// The biggest amount is deliberately the oldest, so a sort that quietly falls
	// back to date order cannot pass by coincidence.
	createExportEntry(t, router, auth.Token, accounts[0].ID, map[string]any{
		"title": "Rent", "type": "expense", "amount": 18000, "currency": "INR",
		"source": "manual", "mode": "Bank Account", "category": "Bills", "date": "2026-07-01",
	})
	createExportEntry(t, router, auth.Token, accounts[0].ID, map[string]any{
		"title": "Chai", "type": "expense", "amount": 20, "currency": "INR",
		"source": "manual", "mode": "Cash", "category": "Food & Drinks", "date": "2026-07-15",
	})
	createExportEntry(t, router, auth.Token, accounts[0].ID, map[string]any{
		"title": "Metro", "type": "expense", "amount": 90, "currency": "INR",
		"source": "manual", "mode": "Cash", "category": "Transport", "date": "2026-07-30",
	})

	for _, testCase := range []struct {
		sort  string
		first string
	}{
		{"", "Metro"},
		{"newest", "Metro"},
		{"oldest", "Rent"},
		{"highest", "Rent"},
		{"lowest", "Chai"},
	} {
		target := "/v1/entries"
		if testCase.sort != "" {
			target += "?sort=" + testCase.sort
		}
		page := performJSONRequest[entryListPage](t, router, http.MethodGet, target, auth.Token, nil, http.StatusOK)
		if len(page.Entries) == 0 || page.Entries[0].Title != testCase.first {
			t.Fatalf("sort=%q expected %q first, got %#v", testCase.sort, testCase.first, page.Entries)
		}
	}

	// An unrecognised sort is reported, not silently ignored: a list ordered by
	// something other than what was asked for looks like corrupt data.
	rejected := performRawRequest(t, router, http.MethodGet, "/v1/entries?sort=cheapest", auth.Token, nil)
	if rejected.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for an unknown sort, got %d: %s", rejected.Code, rejected.Body.String())
	}
}

func TestCategoryCountsIgnoreTheCategoryFilterButHonourTheRest(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)

	auth := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{"device_id": "counts-device"}, http.StatusOK,
	)
	accounts := performJSONRequest[[]models.Account](
		t, router, http.MethodGet, "/v1/accounts", auth.Token, nil, http.StatusOK,
	)

	createExportEntry(t, router, auth.Token, accounts[0].ID, map[string]any{
		"title": "Chai", "type": "expense", "amount": 20, "currency": "INR",
		"source": "manual", "mode": "Cash", "category": "Food & Drinks", "date": "2026-07-15",
	})
	createExportEntry(t, router, auth.Token, accounts[0].ID, map[string]any{
		"title": "Lunch", "type": "expense", "amount": 200, "currency": "INR",
		"source": "manual", "mode": "Cash", "category": "Food & Drinks", "date": "2026-07-16",
	})
	createExportEntry(t, router, auth.Token, accounts[0].ID, map[string]any{
		"title": "Metro", "type": "expense", "amount": 90, "currency": "INR",
		"source": "manual", "mode": "Cash", "category": "Transport", "date": "2026-07-30",
	})
	createExportEntry(t, router, auth.Token, accounts[0].ID, map[string]any{
		"title": "Salary", "type": "income", "amount": 50000, "currency": "INR",
		"source": "manual", "mode": "Bank Account", "category": "Misc", "date": "2026-07-31",
	})

	// Picking a category must not zero out every other chip.
	page := performJSONRequest[entryListPage](
		t, router, http.MethodGet, "/v1/entries?category=Transport", auth.Token, nil, http.StatusOK,
	)
	if page.Total != 1 {
		t.Fatalf("the list itself should still be filtered, got %d", page.Total)
	}
	if page.CategoryCounts["Food & Drinks"] != 2 || page.CategoryCounts["Transport"] != 1 {
		t.Fatalf("counts should ignore the category filter, got %#v", page.CategoryCounts)
	}

	// Every other filter still applies, so income drops the expense categories.
	page = performJSONRequest[entryListPage](
		t, router, http.MethodGet, "/v1/entries?type=income", auth.Token, nil, http.StatusOK,
	)
	if page.CategoryCounts["Misc"] != 1 {
		t.Fatalf("expected the income row counted under Misc, got %#v", page.CategoryCounts)
	}
	if _, present := page.CategoryCounts["Food & Drinks"]; present {
		t.Fatalf("a category with no matching rows should be absent, not zero: %#v", page.CategoryCounts)
	}
}

func TestUncategorisedFilterExcludesMisc(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)

	auth := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{"device_id": "uncategorised-device"}, http.StatusOK,
	)
	accounts := performJSONRequest[[]models.Account](
		t, router, http.MethodGet, "/v1/accounts", auth.Token, nil, http.StatusOK,
	)
	createExportEntry(t, router, auth.Token, accounts[0].ID, map[string]any{
		"title": "Odds and ends", "type": "expense", "amount": 60, "currency": "INR",
		"source": "manual", "mode": "Cash", "category": "Misc", "date": "2026-07-15",
	})

	// Only reachable by writing directly: save-time validation requires a category.
	var seeded models.Entry
	if err := database.DB.Where("title = ?", "Odds and ends").First(&seeded).Error; err != nil {
		t.Fatalf("failed to read the seeded entry: %v", err)
	}
	blank := seeded
	blank.ID = 0
	blank.Title = "Imported row"
	blank.Category = ""
	blank.IdempotencyKey = nil
	if err := database.DB.Create(&blank).Error; err != nil {
		t.Fatalf("failed to insert the uncategorised row: %v", err)
	}

	page := performJSONRequest[entryListPage](
		t, router, http.MethodGet, "/v1/entries?uncategorised=1", auth.Token, nil, http.StatusOK,
	)
	if page.Total != 1 || page.Entries[0].Title != "Imported row" {
		t.Fatalf("Misc is a category, not the absence of one: %#v", page.Entries)
	}
}

// Legacy rows still carry pre-S2 names. The filter chips are the canonical list,
// so a count keyed "Food" is a count the UI can never render.
func TestCategoryCountsFoldLegacyNamesOntoCanonicalOnes(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)

	auth := performJSONRequest[AuthResponse](
		t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{"device_id": "legacy-counts-device"}, http.StatusOK,
	)
	accounts := performJSONRequest[[]models.Account](
		t, router, http.MethodGet, "/v1/accounts", auth.Token, nil, http.StatusOK,
	)
	createExportEntry(t, router, auth.Token, accounts[0].ID, map[string]any{
		"title": "Chai", "type": "expense", "amount": 20, "currency": "INR",
		"source": "manual", "mode": "Cash", "category": "Food & Drinks", "date": "2026-07-15",
	})

	// Written straight to the table. Saving through the API would canonicalize
	// it, which is exactly why older rows are the only place this survives.
	var seeded models.Entry
	if err := database.DB.Where("title = ?", "Chai").First(&seeded).Error; err != nil {
		t.Fatalf("failed to read the seeded entry: %v", err)
	}
	legacy := seeded
	legacy.ID = 0
	legacy.Title = "Old grocery run"
	legacy.Category = "Food"
	legacy.IdempotencyKey = nil
	if err := database.DB.Create(&legacy).Error; err != nil {
		t.Fatalf("failed to insert the legacy row: %v", err)
	}
	_ = accounts

	page := performJSONRequest[entryListPage](
		t, router, http.MethodGet, "/v1/entries", auth.Token, nil, http.StatusOK,
	)
	if page.CategoryCounts["Food & Drinks"] != 2 {
		t.Fatalf("legacy 'Food' should fold into 'Food & Drinks', got %#v", page.CategoryCounts)
	}
	if _, present := page.CategoryCounts["Food"]; present {
		t.Fatalf("a legacy name should not be reported on its own, got %#v", page.CategoryCounts)
	}
}
