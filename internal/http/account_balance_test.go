package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"finnri/internal/database"
	"finnri/internal/models"
)

func seedLedgerAccount(t *testing.T, userID uint, accountType string) models.Account {
	t.Helper()
	account := models.Account{UserID: userID, Type: accountType, Name: "Sodexo", Provider: "Pluxee"}
	if err := database.DB.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	return account
}

func seedLedgerEntry(t *testing.T, userID, accountID uint, entryType string, amount models.Money) {
	t.Helper()
	entry := models.Entry{
		UserID: userID, AccountID: &accountID, Title: entryType, Type: entryType,
		Category: "Other", Amount: amount, Date: "2026-10-01",
	}
	if err := database.DB.Create(&entry).Error; err != nil {
		t.Fatalf("seed entry: %v", err)
	}
}

// The wallet's top-up message prints today's balance; the user should be able
// to type that and see it, not work out what the wallet held in August.
func TestSetAccountBalanceWorksBackToTheOpening(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	user, token := createBillingTestUserSession(t)
	wallet := seedLedgerAccount(t, user.ID, "wallet")
	seedLedgerEntry(t, user.ID, wallet.ID, "income", 880000)  // ₹8,800 loaded
	seedLedgerEntry(t, user.ID, wallet.ID, "expense", 278100) // ₹2,781 spent

	got := performJSONRequest[accountWithSummary](t, router, http.MethodPost,
		fmt.Sprintf("/v1/accounts/%d/balance", wallet.ID), token,
		map[string]any{"current_balance": 33184.16}, http.StatusOK)

	if got.Summary.RunningBalance == nil || *got.Summary.RunningBalance != 33184.16 {
		t.Fatalf("running balance = %v, want 33184.16", got.Summary.RunningBalance)
	}
	// 33,184.16 − 8,800 + 2,781
	if got.Balance != 2716516 || !got.BalanceTracked {
		t.Fatalf("opening = %s tracked = %v, want 27,165.16 tracked", got.Balance, got.BalanceTracked)
	}
}

// A wallet logged from its first load opens at exactly ₹0. It still has a
// balance to show.
func TestTrackedBalanceShowsEvenWhenTheOpeningIsZero(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	user, token := createBillingTestUserSession(t)
	wallet := seedLedgerAccount(t, user.ID, "wallet")
	seedLedgerEntry(t, user.ID, wallet.ID, "income", 880000)

	got := performJSONRequest[accountWithSummary](t, router, http.MethodPost,
		fmt.Sprintf("/v1/accounts/%d/balance", wallet.ID), token,
		map[string]any{"current_balance": 8800}, http.StatusOK)

	if got.Balance != 0 || got.Summary.RunningBalance == nil || *got.Summary.RunningBalance != 8800 {
		t.Fatalf("opening %s, running %v; want 0 and 8800", got.Balance, got.Summary.RunningBalance)
	}
}

func TestSetAccountBalanceRefusesCards(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	user, token := createBillingTestUserSession(t)
	card := seedLedgerAccount(t, user.ID, "credit_card")

	performJSONRequest[map[string]any](t, router, http.MethodPost,
		fmt.Sprintf("/v1/accounts/%d/balance", card.ID), token,
		map[string]any{"current_balance": 100}, http.StatusUnprocessableEntity)
}

func TestNewAccountTakesTodaysBalance(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, token := createBillingTestUserSession(t)

	got := performJSONRequest[models.Account](t, router, http.MethodPost, "/v1/accounts", token,
		map[string]any{"type": "wallet", "name": "Meal wallet", "current_balance": 0}, http.StatusCreated)
	if got.Balance != 0 || !got.BalanceTracked {
		t.Fatalf("a new wallet at ₹0 should be tracked, got %#v", got)
	}
}

// CRED flash is a credit line with a statement and a due date: a credit_card
// account, named as pay later. Picking the provider is enough.
func TestPayLaterProviderMakesAPayLaterLine(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, token := createBillingTestUserSession(t)

	got := performJSONRequest[models.Account](t, router, http.MethodPost, "/v1/accounts", token,
		map[string]any{"type": "credit_card", "name": "CRED flash", "provider_id": "cred-flash",
			"credit_limit": 15000, "due_day": 7}, http.StatusCreated)
	if got.CreditKind != "pay_later" || got.Provider != "CRED flash" {
		t.Fatalf("got kind %q provider %q", got.CreditKind, got.Provider)
	}

	// An edit from an older client, which does not send credit_kind, keeps it.
	updated := performJSONRequest[models.Account](t, router, http.MethodPut,
		fmt.Sprintf("/v1/accounts/%d", got.ID), token,
		map[string]any{"type": "credit_card", "name": "Flash", "credit_limit": 15000, "due_day": 7}, http.StatusOK)
	if updated.CreditKind != "pay_later" {
		t.Fatalf("an edit dropped the pay-later kind: %q", updated.CreditKind)
	}
}

func TestCreditKindIsOnlyForCards(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, token := createBillingTestUserSession(t)

	for _, body := range []map[string]any{
		{"type": "wallet", "name": "Paytm", "credit_kind": "pay_later"},
		{"type": "credit_card", "name": "Card", "credit_kind": "loan"},
		{"type": "credit_card", "name": "Card", "current_balance": 10},
	} {
		performJSONRequest[map[string]any](t, router, http.MethodPost, "/v1/accounts", token, body, http.StatusUnprocessableEntity)
	}
}

// The balance a top-up message prints survives to the app.
func TestParseReceiptKeepsThePrintedBalance(t *testing.T) {
	server := newReceiptServer(t, `{
		"summary":"Pluxee Card credited, ₹8800, 2026-11-04","type":"income","title":"Pluxee Card Credit",
		"amount":8800,"currency":"INR","mode":"Wallets","account_hint":"Pluxee","category":"Salary",
		"merchant":"Pluxee","date":"2026-11-04","balance_after":33184.16
	}`)
	ctx, response := newReceiptContext(t, receiptTestPNG)

	server.handleParseReceipt(ctx)

	if response.Code != 200 {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var draft map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &draft); err != nil {
		t.Fatal(err)
	}
	if draft["balance_after"] != 33184.16 || draft["mode"] != "Wallets" || draft["category"] != "Salary" {
		t.Fatalf("unexpected draft %v", draft)
	}
}
