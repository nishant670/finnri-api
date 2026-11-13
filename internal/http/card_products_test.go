package http

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"finnri/internal/database"
	"finnri/internal/models"
)

func seedCardCatalogue(t *testing.T) {
	t.Helper()
	if err := database.SeedCardProducts(database.DB); err != nil {
		t.Fatalf("failed to seed the card catalogue: %v", err)
	}
}

func TestCardProductSeedIsTheLaunchCatalogue(t *testing.T) {
	products, err := database.CardProductSeed()
	if err != nil {
		t.Fatal(err)
	}
	// 40 drawings in Round 19, less the four that are the owner's own cards.
	if len(products) != 36 {
		t.Fatalf("launch catalogue has %d products, want 36", len(products))
	}
	migration, err := os.ReadFile(filepath.Join(projectRoot(t), "migrations", "0076_create_card_products.sql"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, product := range products {
		if seen[product.ID] {
			t.Fatalf("%s is in the seed twice", product.ID)
		}
		seen[product.ID] = true
		if !cardProductIDPattern.MatchString(product.ID) || !strings.HasPrefix(product.ID, strings.SplitN(product.IssuerID, "-", 2)[0]) {
			t.Fatalf("%s is not a slug that starts with its bank (%s)", product.ID, product.IssuerID)
		}
		if issuer, ok := cardIssuerFor(product.IssuerID); !ok || issuer != product.IssuerID {
			t.Fatalf("%s names issuer %q, which is not a credit card provider", product.ID, product.IssuerID)
		}
		if product.Name == "" || !slices.Contains(models.CardTones, product.Tone) ||
			!slices.Contains(models.CardOrientations, product.Orientation) || !cardDrawingPattern.MatchString(product.Drawing) {
			t.Fatalf("%s has incomplete art: %#v", product.ID, product)
		}
		// Every launch drawing was approved against its real card (13 Nov 2026).
		if !product.ArtReviewed {
			t.Fatalf("%s should start with its art reviewed", product.ID)
		}
		if product.AnnualFee != nil || product.JoiningFee != nil || product.FeeWaiverSpend != nil {
			t.Fatalf("%s carries a fee with no source; fees come from the issuer's terms in the admin", product.ID)
		}
		// The migration file is the written record of the same rows.
		if !strings.Contains(string(migration), "('"+product.ID+"', '"+product.IssuerID+"'") || !strings.Contains(string(migration), "TRUE, "+strconv.Itoa(product.SortOrder)+")") {
			t.Fatalf("migrations/0076 does not list %s", product.ID)
		}
	}
	for _, id := range []string{"hdfc-millennia", "sbi-bpcl", "axis-magnus", "au-lit", "indusind-cred"} {
		if !seen[id] {
			t.Fatalf("launch catalogue is missing %s", id)
		}
	}
}

func TestCardProductsListByIssuerMostHeldFirst(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, token := createBillingTestUserSession(t)
	seedCardCatalogue(t)

	hdfc := performJSONRequest[[]models.CardProduct](t, router, http.MethodGet, "/v1/card-products?issuer=hdfc", token, nil, http.StatusOK)
	if len(hdfc) != 10 || hdfc[0].ID != "hdfc-millennia" || hdfc[9].ID != "hdfc-tata-neu-infinity" {
		t.Fatalf("HDFC's cards should come in catalogue order, got %d starting %v", len(hdfc), hdfc)
	}
	for _, product := range hdfc {
		if product.IssuerID != "hdfc" {
			t.Fatalf("issuer filter leaked %s", product.ID)
		}
	}
	if hdfc[9].Orientation != models.CardOrientationPortrait || hdfc[0].Drawing != "dotgrid" || hdfc[0].Tone != "solid" {
		t.Fatalf("art fields did not come through: %#v %#v", hdfc[0], hdfc[9])
	}

	// A bank's name works as well as its id.
	byName := performJSONRequest[[]models.CardProduct](t, router, http.MethodGet, "/v1/card-products?issuer=AU%20Bank", token, nil, http.StatusOK)
	if len(byName) != 2 || byName[0].ID != "au-lit" {
		t.Fatalf("AU Bank should resolve to au-small-finance, got %v", byName)
	}
	// A bank with no catalogued cards yet is an empty list, not an error.
	kotak := performJSONRequest[[]models.CardProduct](t, router, http.MethodGet, "/v1/card-products?issuer=kotak", token, nil, http.StatusOK)
	if len(kotak) != 0 {
		t.Fatalf("kotak has no catalogued cards, got %v", kotak)
	}
	performJSONRequest[map[string]any](t, router, http.MethodGet, "/v1/card-products?issuer=nobody", token, nil, http.StatusUnprocessableEntity)
	// Google Pay issues no credit cards.
	performJSONRequest[map[string]any](t, router, http.MethodGet, "/v1/card-products?issuer=google-pay", token, nil, http.StatusUnprocessableEntity)

	// Hidden products leave the picker; re-seeding neither duplicates rows nor
	// undoes an admin's edit.
	if err := database.DB.Model(&models.CardProduct{}).Where("id = ?", "hdfc-swiggy").Update("hidden", true).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.DB.Model(&models.CardProduct{}).Where("id = ?", "hdfc-millennia").Update("name", "Millennia (edited)").Error; err != nil {
		t.Fatal(err)
	}
	seedCardCatalogue(t)
	all := performJSONRequest[[]models.CardProduct](t, router, http.MethodGet, "/v1/card-products", token, nil, http.StatusOK)
	if len(all) != 35 {
		t.Fatalf("whole catalogue less one hidden card should be 35, got %d", len(all))
	}
	for _, product := range all {
		if product.ID == "hdfc-swiggy" {
			t.Fatal("a hidden product must not be offered")
		}
		if product.ID == "hdfc-millennia" && product.Name != "Millennia (edited)" {
			t.Fatalf("re-seeding overwrote an admin edit: %q", product.Name)
		}
	}
}

func TestAccountLinksItsCardProduct(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, token := createBillingTestUserSession(t)
	seedCardCatalogue(t)
	fee, waiver := models.Money(50000), models.Money(10000000)
	if err := database.DB.Model(&models.CardProduct{}).Where("id = ?", "hdfc-millennia").
		Updates(map[string]any{"annual_fee": fee, "fee_waiver_spend": waiver, "source_urls": models.StringList{"https://www.hdfcbank.com/millennia"}}).Error; err != nil {
		t.Fatal(err)
	}

	card := map[string]any{
		"type": "credit_card", "name": "HDFC Millennia", "provider_id": "hdfc", "last4": "4321",
		"card_product_id": "hdfc-millennia", "card_network": "Visa",
	}
	created := performJSONRequest[models.Account](t, router, http.MethodPost, "/v1/accounts", token, card, http.StatusCreated)
	if created.CardProductID == nil || *created.CardProductID != "hdfc-millennia" || created.CardNetwork != "visa" {
		t.Fatalf("product and network should be stored, got %#v", created)
	}
	if created.CardProduct == nil || created.CardProduct.Drawing != "dotgrid" || created.CardProduct.Name != "Millennia" {
		t.Fatalf("the account should carry its product's art, got %#v", created.CardProduct)
	}
	if created.AnnualFee != fee || created.FeeWaiverSpend != waiver {
		t.Fatalf("picking the product fills the fee and waiver, got %v / %v", created.AnnualFee, created.FeeWaiverSpend)
	}

	// What the user typed beats the catalogue.
	typed := map[string]any{
		"type": "credit_card", "name": "Second Millennia", "provider_id": "hdfc",
		"card_product_id": "hdfc-millennia", "annual_fee": 1000,
	}
	own := performJSONRequest[models.Account](t, router, http.MethodPost, "/v1/accounts", token, typed, http.StatusCreated)
	if own.AnnualFee != models.Money(100000) || own.FeeWaiverSpend != waiver {
		t.Fatalf("a typed fee wins and the blank waiver fills, got %v / %v", own.AnnualFee, own.FeeWaiverSpend)
	}

	// Picking the card names the bank when the client did not.
	bankless := performJSONRequest[models.Account](t, router, http.MethodPost, "/v1/accounts", token, map[string]any{
		"type": "credit_card", "name": "Magnus", "card_product_id": "axis-magnus",
	}, http.StatusCreated)
	if bankless.ProviderID != "axis" || bankless.Provider != "Axis Bank" || bankless.CardProduct.Orientation != "portrait" {
		t.Fatalf("the product should set the bank, got %#v", bankless)
	}

	for name, body := range map[string]map[string]any{
		"another bank's card": {"type": "credit_card", "name": "x", "provider_id": "sbi", "card_product_id": "hdfc-millennia"},
		"unknown product":     {"type": "credit_card", "name": "x", "provider_id": "hdfc", "card_product_id": "hdfc-imaginary"},
		"not a credit card":   {"type": "debit_card", "name": "x", "provider_id": "hdfc", "card_product_id": "hdfc-millennia"},
		"unknown network":     {"type": "credit_card", "name": "x", "provider_id": "hdfc", "card_network": "discover"},
	} {
		response := performJSONRequest[map[string]any](t, router, http.MethodPost, "/v1/accounts", token, body, http.StatusUnprocessableEntity)
		if fields, _ := response["fields"].(map[string]any); len(fields) == 0 {
			t.Fatalf("%s: expected field errors, got %v", name, response)
		}
	}

	// An older client's edit leaves out the field: the link stays.
	path := "/v1/accounts/" + strconv.FormatUint(uint64(created.ID), 10)
	renamed := performJSONRequest[models.Account](t, router, http.MethodPut, path, token, map[string]any{
		"type": "credit_card", "name": "Millennia card", "provider_id": "hdfc", "last4": "4321",
		"annual_fee": 500, "fee_waiver_spend": 100000,
	}, http.StatusOK)
	if renamed.CardProductID == nil || renamed.CardNetwork != "visa" || renamed.CardProduct == nil {
		t.Fatalf("an edit without the fields must keep them, got %#v", renamed)
	}
	listed := performJSONRequest[[]models.Account](t, router, http.MethodGet, "/v1/accounts", token, nil, http.StatusOK)
	foundListed := false
	for _, account := range listed {
		if account.ID == created.ID {
			foundListed = account.CardProduct != nil && account.CardProduct.ID == "hdfc-millennia"
		}
	}
	if !foundListed {
		t.Fatalf("GET /v1/accounts should carry the product, got %#v", listed)
	}

	// The same older client moving the card to another bank drops the stale link.
	moved := performJSONRequest[models.Account](t, router, http.MethodPut, path, token, map[string]any{
		"type": "credit_card", "name": "Millennia card", "provider_id": "sbi",
	}, http.StatusOK)
	if moved.CardProductID != nil || moved.CardProduct != nil {
		t.Fatalf("a card moved to SBI cannot stay an HDFC Millennia, got %#v", moved)
	}

	// "My card isn't listed": an empty id clears it; turning a card into a
	// debit card clears product and network.
	relinked := performJSONRequest[models.Account](t, router, http.MethodPut, path, token, map[string]any{
		"type": "credit_card", "name": "SBI", "provider_id": "sbi", "card_product_id": "sbi-bpcl", "card_network": "rupay",
	}, http.StatusOK)
	if relinked.CardProductID == nil || *relinked.CardProductID != "sbi-bpcl" {
		t.Fatalf("relink failed: %#v", relinked)
	}
	cleared := performJSONRequest[models.Account](t, router, http.MethodPut, path, token, map[string]any{
		"type": "credit_card", "name": "SBI", "provider_id": "sbi", "card_product_id": "",
	}, http.StatusOK)
	if cleared.CardProductID != nil || cleared.CardNetwork != "rupay" {
		t.Fatalf("an empty id clears only the product, got %#v", cleared)
	}
	debit := performJSONRequest[models.Account](t, router, http.MethodPut, path, token, map[string]any{
		"type": "debit_card", "name": "SBI debit", "provider_id": "sbi",
	}, http.StatusOK)
	if debit.CardProductID != nil || debit.CardNetwork != "" {
		t.Fatalf("a debit card carries no card product or network, got %#v", debit)
	}

	// A hidden product stays on a card that already has it but cannot be newly picked.
	if err := database.DB.Model(&models.CardProduct{}).Where("id = ?", "hdfc-millennia").Update("hidden", true).Error; err != nil {
		t.Fatal(err)
	}
	performJSONRequest[map[string]any](t, router, http.MethodPost, "/v1/accounts", token, map[string]any{
		"type": "credit_card", "name": "x", "provider_id": "hdfc", "card_product_id": "hdfc-millennia",
	}, http.StatusUnprocessableEntity)
	kept := performJSONRequest[models.Account](t, router, http.MethodPut, "/v1/accounts/"+strconv.FormatUint(uint64(own.ID), 10), token, map[string]any{
		"type": "credit_card", "name": "Second Millennia", "provider_id": "hdfc", "card_product_id": "hdfc-millennia",
	}, http.StatusOK)
	if kept.CardProductID == nil {
		t.Fatal("hiding a product must not break a card that already uses it")
	}
}

func TestAdminEditsTheCardCatalogue(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, support := createAdminSessionForTest(t, models.AdminRoleSupport)
	seedCardCatalogue(t)

	product := map[string]any{
		"id": "hdfc-tata-neu-plus", "issuer_id": "HDFC Bank", "name": "  Tata Neu   Plus ", "tone": "Tint",
		"drawing": "coins", "orientation": "portrait", "sort_order": 11,
	}
	created := decodeRelease[models.CardProduct](t, releaseRequest(t, router, http.MethodPost, "/v1/admin/card-products", support, product), http.StatusCreated)
	if created.IssuerID != "hdfc" || created.Name != "Tata Neu Plus" || created.Tone != "tint" || len(created.SourceURLs) != 0 {
		t.Fatalf("admin create should clean its input, got %#v", created)
	}
	decodeRelease[map[string]any](t, releaseRequest(t, router, http.MethodPost, "/v1/admin/card-products", support, product), http.StatusConflict)

	// Fees need the issuer page they came from.
	product["annual_fee"] = 1499
	decodeRelease[map[string]any](t, releaseRequest(t, router, http.MethodPut, "/v1/admin/card-products/hdfc-tata-neu-plus", support, product), http.StatusUnprocessableEntity)
	product["source_urls"] = []string{"https://www.hdfcbank.com/tata-neu-plus", " "}
	product["last_verified_at"] = "2026-11-13"
	product["art_reviewed"] = true
	updated := decodeRelease[models.CardProduct](t, releaseRequest(t, router, http.MethodPut, "/v1/admin/card-products/hdfc-tata-neu-plus", support, product), http.StatusOK)
	if updated.AnnualFee == nil || *updated.AnnualFee != models.Money(149900) || len(updated.SourceURLs) != 1 ||
		updated.LastVerifiedAt == nil || !updated.ArtReviewed {
		t.Fatalf("fees, sources and the check date should save, got %#v", updated)
	}

	// Clearing is an edit: false and null are written, not skipped.
	delete(product, "annual_fee")
	product["art_reviewed"] = false
	product["hidden"] = true
	cleared := decodeRelease[models.CardProduct](t, releaseRequest(t, router, http.MethodPut, "/v1/admin/card-products/hdfc-tata-neu-plus", support, product), http.StatusOK)
	if cleared.AnnualFee != nil || cleared.ArtReviewed || !cleared.Hidden {
		t.Fatalf("a cleared fee and an unticked box should save, got %#v", cleared)
	}

	product["id"] = "hdfc-renamed"
	decodeRelease[map[string]any](t, releaseRequest(t, router, http.MethodPut, "/v1/admin/card-products/hdfc-tata-neu-plus", support, product), http.StatusUnprocessableEntity)
	decodeRelease[map[string]any](t, releaseRequest(t, router, http.MethodPut, "/v1/admin/card-products/nope", support, product), http.StatusNotFound)

	for name, bad := range map[string]map[string]any{
		"bad slug":      {"id": "HDFC Millennia", "issuer_id": "hdfc", "name": "x", "tone": "deep", "drawing": "dotgrid"},
		"not an issuer": {"id": "gpay-card", "issuer_id": "google-pay", "name": "x", "tone": "deep", "drawing": "dotgrid"},
		"bad tone":      {"id": "hdfc-x", "issuer_id": "hdfc", "name": "x", "tone": "gold", "drawing": "dotgrid"},
		"bad source":    {"id": "hdfc-x", "issuer_id": "hdfc", "name": "x", "tone": "deep", "drawing": "dotgrid", "source_urls": []string{"javascript:alert(1)"}},
	} {
		response := decodeRelease[map[string]any](t, releaseRequest(t, router, http.MethodPost, "/v1/admin/card-products", support, bad), http.StatusUnprocessableEntity)
		if fields, _ := response["fields"].(map[string]any); len(fields) == 0 {
			t.Fatalf("%s: expected field errors, got %v", name, response)
		}
	}

	// A product cards point at cannot move to another bank.
	user, _ := createBillingTestUserSession(t)
	linked := "hdfc-millennia"
	if err := database.DB.Create(&models.Account{UserID: user.ID, Type: "credit_card", Name: "Millennia", ProviderID: "hdfc", CardProductID: &linked}).Error; err != nil {
		t.Fatal(err)
	}
	move := map[string]any{"issuer_id": "axis", "name": "Millennia", "tone": "solid", "drawing": "dotgrid"}
	decodeRelease[map[string]any](t, releaseRequest(t, router, http.MethodPut, "/v1/admin/card-products/hdfc-millennia", support, move), http.StatusUnprocessableEntity)

	type adminList struct {
		Products       []models.CardProduct `json:"products"`
		CardsByProduct map[string]int64     `json:"cards_by_product"`
	}
	listed := decodeRelease[adminList](t, releaseRequest(t, router, http.MethodGet, "/v1/admin/card-products?issuer=hdfc", support, nil), http.StatusOK)
	if len(listed.Products) != 11 || listed.CardsByProduct["hdfc-millennia"] != 1 {
		t.Fatalf("admin sees hidden products too and how many cards use each, got %d / %v", len(listed.Products), listed.CardsByProduct)
	}

	// Viewers read; only support and owner write.
	_, viewer := createAdminSessionForTest(t, models.AdminRoleViewer)
	decodeRelease[map[string]any](t, releaseRequest(t, router, http.MethodPut, "/v1/admin/card-products/hdfc-millennia", viewer, product), http.StatusForbidden)
}

func TestPaidInFullStreakCountsBillsPaidInARow(t *testing.T) {
	today := "2026-11-13"
	paid := func(due string) statementMark { return statementMark{Status: statementStatusPaid, DueDate: due} }
	for name, tc := range map[string]struct {
		history []statementMark
		want    int
	}{
		"no bills":                    {nil, 0},
		"three paid":                  {[]statementMark{paid("2026-11-05"), paid("2026-10-05"), paid("2026-09-05")}, 3},
		"open bill not yet due":       {[]statementMark{{Status: "unpaid", DueDate: "2026-11-25"}, paid("2026-10-25"), paid("2026-09-25")}, 2},
		"open bill past due":          {[]statementMark{{Status: "unpaid", DueDate: "2026-11-10"}, paid("2026-10-10")}, 0},
		"a partial breaks the run":    {[]statementMark{paid("2026-11-05"), {Status: "partial", DueDate: "2026-10-05"}, paid("2026-09-05")}, 1},
		"partial bill, still not due": {[]statementMark{{Status: "partial", DueDate: "2026-11-20"}, paid("2026-10-20")}, 1},
	} {
		if got := paidInFullStreak(tc.history, today); got != tc.want {
			t.Fatalf("%s: streak = %d, want %d", name, got, tc.want)
		}
	}
}
