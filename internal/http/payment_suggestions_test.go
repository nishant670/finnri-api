package http

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"finnri/internal/database"
	"finnri/internal/models"
)

func seedPaymentAccount(t *testing.T, userID uint, accountType, name string) models.Account {
	t.Helper()
	account := models.Account{UserID: userID, Type: accountType, Name: name}
	if err := database.DB.Create(&account).Error; err != nil {
		t.Fatalf("failed to seed account: %v", err)
	}
	return account
}

func seedPaidExpense(t *testing.T, userID uint, accountID uint, daysAgo int, title, merchant, tag string) {
	t.Helper()
	entry := models.Entry{
		UserID: userID, Type: "expense", Title: title, Merchant: merchant, Tag: tag,
		Category: "Transport", Date: time.Now().AddDate(0, 0, -daysAgo).Format("2006-01-02"),
		AccountID: &accountID,
	}
	if err := database.DB.Create(&entry).Error; err != nil {
		t.Fatalf("failed to seed entry: %v", err)
	}
}

func suggestionPath(title, merchant, tag string) string {
	return "/v1/payment-suggestion?" + url.Values{
		"type": {"expense"}, "title": {title}, "merchant": {merchant}, "tag": {tag},
	}.Encode()
}

func TestPaymentSuggestionLearnsTheFuelCard(t *testing.T) {
	// 2026-11-06 on device: fuel is always paid with the SBI BPCL card, but a
	// scanned pump bill came in as Cash because the bill did not say.
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	user, token := createBillingTestUserSession(t)
	cash := seedPaymentAccount(t, user.ID, "cash", "Cash")
	bpcl := seedPaymentAccount(t, user.ID, "credit_card", "SBI BPCL")

	// Three pumps, three spellings, one card — and one cash fill-up.
	seedPaidExpense(t, user.ID, bpcl.ID, 3, "Car Fuel", "Soami BPCL", "General")
	seedPaidExpense(t, user.ID, bpcl.ID, 20, "Petrol", "Indian Oil", "Petrol")
	seedPaidExpense(t, user.ID, bpcl.ID, 41, "Diesel top up", "", "")
	seedPaidExpense(t, user.ID, cash.ID, 2, "Groceries", "DMart", "")

	got := performJSONRequest[PaymentSuggestionResponse](t, router, http.MethodGet,
		suggestionPath("Petrol", "Bharat Petroleum", "Fuel"), token, nil, http.StatusOK)
	if got.Suggestion == nil {
		t.Fatalf("no suggestion, want the SBI BPCL card")
	}
	if got.Suggestion.AccountID != bpcl.ID || got.Suggestion.Source != "habit" || got.Suggestion.Label != "fuel" {
		t.Fatalf("suggestion = %+v, want SBI BPCL from the fuel habit", got.Suggestion)
	}
	if got.Suggestion.Matched != 3 || got.Suggestion.Total != 3 {
		t.Fatalf("counts = %d/%d, want 3/3", got.Suggestion.Matched, got.Suggestion.Total)
	}
	if got.Kind == nil || got.Kind.Label != "fuel" {
		t.Fatalf("kind = %+v, want fuel (a family beats one pump's name for a rule)", got.Kind)
	}
}

func TestPaymentSuggestionNeedsAClearHabit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	user, token := createBillingTestUserSession(t)
	cash := seedPaymentAccount(t, user.ID, "cash", "Cash")
	card := seedPaymentAccount(t, user.ID, "credit_card", "HDFC")

	cases := []struct {
		name  string
		seed  func(t *testing.T, u uint)
		title string
	}{
		{"two is not a habit", func(t *testing.T, u uint) {
			seedPaidExpense(t, u, card.ID, 1, "Swiggy order", "Swiggy", "")
			seedPaidExpense(t, u, card.ID, 2, "Swiggy order", "Swiggy", "")
		}, "Swiggy dinner"},
		{"a 2-2 split is not a habit", func(t *testing.T, u uint) {
			seedPaidExpense(t, u, card.ID, 1, "Uber", "Uber", "")
			seedPaidExpense(t, u, card.ID, 2, "Uber", "Uber", "")
			seedPaidExpense(t, u, cash.ID, 3, "Uber", "Uber", "")
			seedPaidExpense(t, u, cash.ID, 4, "Uber", "Uber", "")
		}, "Uber"},
		{"older than 90 days does not count", func(t *testing.T, u uint) {
			seedPaidExpense(t, u, card.ID, 100, "Blinkit", "Blinkit", "")
			seedPaidExpense(t, u, card.ID, 120, "Blinkit", "Blinkit", "")
			seedPaidExpense(t, u, card.ID, 150, "Blinkit", "Blinkit", "")
		}, "Blinkit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.seed(t, user.ID)
			got := performJSONRequest[PaymentSuggestionResponse](t, router, http.MethodGet,
				suggestionPath(tc.title, tc.title, ""), token, nil, http.StatusOK)
			if got.Suggestion != nil {
				t.Fatalf("suggestion = %+v, want none", got.Suggestion)
			}
		})
	}
}

func TestPaymentRuleBeatsHabitAndCoversItsFamily(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	user, token := createBillingTestUserSession(t)
	other, otherToken := createBillingTestUserSession(t)
	cash := seedPaymentAccount(t, user.ID, "cash", "Cash")
	bpcl := seedPaymentAccount(t, user.ID, "credit_card", "SBI BPCL")
	for day := 1; day <= 3; day++ {
		seedPaidExpense(t, user.ID, cash.ID, day, "Petrol", "Shell", "")
	}

	rule := performJSONRequest[models.AccountRule](t, router, http.MethodPost, "/v1/payment-rules", token,
		PaymentRuleInput{AccountID: bpcl.ID, Phrase: "  Fuel "}, http.StatusCreated)
	if rule.Phrase != "Fuel" {
		t.Fatalf("phrase = %q, want trimmed Fuel", rule.Phrase)
	}

	// "Fuel" names the family, so "Petrol" at a pump the rule never mentions
	// is covered, and the rule wins over three cash fill-ups.
	got := performJSONRequest[PaymentSuggestionResponse](t, router, http.MethodGet,
		suggestionPath("Petrol", "Shell", ""), token, nil, http.StatusOK)
	if got.Suggestion == nil || got.Suggestion.AccountID != bpcl.ID || got.Suggestion.Source != "rule" ||
		got.Suggestion.RuleID != rule.ID {
		t.Fatalf("suggestion = %+v, want rule %d → SBI BPCL", got.Suggestion, rule.ID)
	}

	// A phrase that is not a family matches as whole words only.
	performJSONRequest[models.AccountRule](t, router, http.MethodPost, "/v1/payment-rules", token,
		PaymentRuleInput{AccountID: cash.ID, Phrase: "chai"}, http.StatusCreated)
	if got := performJSONRequest[PaymentSuggestionResponse](t, router, http.MethodGet,
		suggestionPath("Chaitanya books", "", ""), token, nil, http.StatusOK); got.Suggestion != nil {
		t.Fatalf("suggestion = %+v, want none for a word that only starts with the phrase", got.Suggestion)
	}

	// Saving the same phrase again moves it rather than adding a second rule.
	moved := performJSONRequest[models.AccountRule](t, router, http.MethodPost, "/v1/payment-rules", token,
		PaymentRuleInput{AccountID: cash.ID, Phrase: "fuel"}, http.StatusOK)
	if moved.ID != rule.ID || moved.AccountID != cash.ID {
		t.Fatalf("moved = %+v, want rule %d on Cash", moved, rule.ID)
	}
	list := performJSONRequest[PaymentRulesResponse](t, router, http.MethodGet, "/v1/payment-rules", token, nil, http.StatusOK)
	if len(list.Rules) != 2 {
		t.Fatalf("rules = %+v, want chai and fuel", list.Rules)
	}

	// Another user can neither see nor delete them, nor point a rule at this
	// user's account.
	if theirs := performJSONRequest[PaymentRulesResponse](t, router, http.MethodGet, "/v1/payment-rules", otherToken, nil, http.StatusOK); len(theirs.Rules) != 0 {
		t.Fatalf("other user sees %d rules", len(theirs.Rules))
	}
	performJSONRequest[map[string]any](t, router, http.MethodDelete,
		fmt.Sprintf("/v1/payment-rules/%d", rule.ID), otherToken, nil, http.StatusNotFound)
	performJSONRequest[map[string]any](t, router, http.MethodPost, "/v1/payment-rules", otherToken,
		PaymentRuleInput{AccountID: bpcl.ID, Phrase: "fuel"}, http.StatusNotFound)
	_ = other

	deleted := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/v1/payment-rules/%d", rule.ID), nil)
	request.Header.Set("Authorization", "Bearer "+token)
	router.ServeHTTP(deleted, request)
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("DELETE rule status = %d, body = %s", deleted.Code, deleted.Body.String())
	}
}

func TestPaymentRuleValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	user, token := createBillingTestUserSession(t)
	card := seedPaymentAccount(t, user.ID, "credit_card", "HDFC")

	for _, input := range []PaymentRuleInput{
		{AccountID: card.ID, Phrase: "  "},
		{AccountID: card.ID, Phrase: "a phrase that is far too long to be a sensible rule"},
		{AccountID: 0, Phrase: "fuel"},
	} {
		performJSONRequest[map[string]any](t, router, http.MethodPost, "/v1/payment-rules", token, input, http.StatusUnprocessableEntity)
	}
}

func TestDeletingAnAccountDropsItsRules(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	user, token := createBillingTestUserSession(t)
	seedPaymentAccount(t, user.ID, "cash", "Cash")
	card := seedPaymentAccount(t, user.ID, "credit_card", "Old card")
	performJSONRequest[models.AccountRule](t, router, http.MethodPost, "/v1/payment-rules", token,
		PaymentRuleInput{AccountID: card.ID, Phrase: "fuel"}, http.StatusCreated)

	request := fmt.Sprintf("/v1/accounts/%d", card.ID)
	performJSONRequest[map[string]any](t, router, http.MethodDelete, request, token, nil, http.StatusOK)

	list := performJSONRequest[PaymentRulesResponse](t, router, http.MethodGet, "/v1/payment-rules", token, nil, http.StatusOK)
	if len(list.Rules) != 0 {
		t.Fatalf("rules = %+v, want none after the account is deleted", list.Rules)
	}
}

func TestPaymentSuggestionSkipsIncome(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, token := createBillingTestUserSession(t)
	got := performJSONRequest[PaymentSuggestionResponse](t, router, http.MethodGet,
		"/v1/payment-suggestion?type=income&title=Salary", token, nil, http.StatusOK)
	if got.Suggestion != nil {
		t.Fatalf("suggestion = %+v, want none for income", got.Suggestion)
	}
}

func TestSpendKindsReadFamiliesAsWholeWords(t *testing.T) {
	kinds := spendKindsOf("Ola Electric scooter service", "", "")
	for _, kind := range kinds {
		if kind.key == "f:cabs" {
			t.Fatalf("kinds = %+v: \"Ola\" alone reads as a cab ride", kinds)
		}
	}
}
