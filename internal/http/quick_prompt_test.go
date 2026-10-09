package http

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"finnri/internal/database"
	"finnri/internal/models"
)

func createWalletAccount(t *testing.T, userID uint, name string) models.Account {
	t.Helper()
	account := models.Account{UserID: userID, Type: "wallet", Name: name}
	if err := database.DB.Create(&account).Error; err != nil {
		t.Fatalf("failed to create wallet: %v", err)
	}
	return account
}

func TestQuickPromptKeepsEveryFieldTheEditorShows(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	user, token := createBillingTestUserSession(t)
	paytm := createWalletAccount(t, user.ID, "Paytm Wallet")

	created := performJSONRequest[models.QuickPrompt](t, router, http.MethodPost, "/v1/quick-prompts", token, map[string]any{
		"title":      "  Metro Recharge ",
		"amount":     300,
		"type":       "Expense",
		"mode":       "Wallets",
		"category":   "Travel",
		"account_id": paytm.ID,
		"merchant":   "Delhi Metro",
		"tag":        "General",
		"notes":      "Monthly top-up",
		"icon":       "train",
	}, http.StatusCreated)

	if created.Title != "Metro Recharge" || created.Type != "expense" || created.Mode != "Wallets" {
		t.Fatalf("prompt not normalised: %#v", created)
	}
	if created.AccountID == nil || *created.AccountID != paytm.ID {
		t.Fatalf("the chosen wallet must be kept: %#v", created.AccountID)
	}
	if created.Merchant != "Delhi Metro" || created.Tag != "General" || created.Notes != "Monthly top-up" {
		t.Fatalf("merchant, tag and notes must be kept: %#v", created)
	}

	listed := performJSONRequest[[]models.QuickPrompt](t, router, http.MethodGet, "/v1/quick-prompts", token, nil, http.StatusOK)
	if len(listed) != 1 || listed[0].AccountID == nil || *listed[0].AccountID != paytm.ID || listed[0].Merchant != "Delhi Metro" {
		t.Fatalf("list must return the saved details: %#v", listed)
	}
}

func TestQuickPromptCanBeIncome(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, token := createBillingTestUserSession(t)

	created := performJSONRequest[models.QuickPrompt](t, router, http.MethodPost, "/v1/quick-prompts", token, map[string]any{
		"title": "Tuition fee", "amount": 2000, "type": "income", "mode": "UPI", "category": "Salary",
	}, http.StatusCreated)
	if created.Type != "income" {
		t.Fatalf("an income prompt was saved as %q", created.Type)
	}
}

func TestQuickPromptWithoutATypeIsAnExpense(t *testing.T) {
	// What every app build before this change sends.
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, token := createBillingTestUserSession(t)

	created := performJSONRequest[models.QuickPrompt](t, router, http.MethodPost, "/v1/quick-prompts", token, map[string]any{
		"title": "Chai", "amount": 20, "mode": "Cash", "category": "Food & Drinks",
	}, http.StatusCreated)
	if created.Type != "expense" || created.AccountID != nil {
		t.Fatalf("unexpected defaults: %#v", created)
	}
}

func TestQuickPromptRejectsSomeoneElsesAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, token := createBillingTestUserSession(t)
	other, _ := createBillingTestUserSession(t)
	theirs := createWalletAccount(t, other.ID, "Their wallet")

	response := performJSONRequest[map[string]any](t, router, http.MethodPost, "/v1/quick-prompts", token, map[string]any{
		"title": "Metro", "amount": 300, "mode": "Wallets", "category": "Travel", "account_id": theirs.ID,
	}, http.StatusUnprocessableEntity)
	fields, _ := response["fields"].(map[string]any)
	if response["error"] != "invalid_quick_prompt" || fields["account_id"] == nil {
		t.Fatalf("unexpected response: %#v", response)
	}
}

func TestQuickPromptRejectsAnUnknownType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, token := createBillingTestUserSession(t)

	response := performJSONRequest[map[string]any](t, router, http.MethodPost, "/v1/quick-prompts", token, map[string]any{
		"title": "Metro", "amount": 300, "type": "transfer", "mode": "UPI", "category": "Travel",
	}, http.StatusUnprocessableEntity)
	if fields, _ := response["fields"].(map[string]any); fields["type"] == nil {
		t.Fatalf("unexpected response: %#v", response)
	}
}

func TestQuickPromptUpdateSwitchesWalletAndKeepsWhatIsNotSent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	user, token := createBillingTestUserSession(t)
	paytm := createWalletAccount(t, user.ID, "Paytm Wallet")
	amazon := createWalletAccount(t, user.ID, "Amazon Pay")

	created := performJSONRequest[models.QuickPrompt](t, router, http.MethodPost, "/v1/quick-prompts", token, map[string]any{
		"title": "Metro Recharge", "amount": 300, "mode": "Wallets", "category": "Travel",
		"account_id": paytm.ID, "merchant": "Delhi Metro", "notes": "Monthly top-up",
	}, http.StatusCreated)
	path := fmt.Sprintf("/v1/quick-prompts/%d", created.ID)

	// The reported case: same prompt, a different wallet.
	switched := performJSONRequest[models.QuickPrompt](t, router, http.MethodPut, path, token, map[string]any{
		"title": "Metro Recharge", "amount": 300, "mode": "Wallets", "category": "Travel", "account_id": amazon.ID,
	}, http.StatusOK)
	if switched.AccountID == nil || *switched.AccountID != amazon.ID {
		t.Fatalf("the wallet did not switch: %#v", switched.AccountID)
	}
	// An older app build sends only title, amount, mode and category. What it
	// cannot see must survive its edit.
	if switched.Merchant != "Delhi Metro" || switched.Notes != "Monthly top-up" {
		t.Fatalf("fields the client did not send were wiped: %#v", switched)
	}

	cleared := performJSONRequest[models.QuickPrompt](t, router, http.MethodPut, path, token, map[string]any{
		"account_id": nil,
	}, http.StatusOK)
	if cleared.AccountID != nil {
		t.Fatalf("an explicit null should fall back to the mode's default account: %#v", cleared.AccountID)
	}
}
