package http

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"finnri/internal/models"
)

// A group with one split expense on it, and the ids needed to check what
// survives its deletion.
type groupWithOneBill struct {
	router  *gin.Engine
	token   string
	groupID uint
	entryID uint
}

func seedGroupWithOneBill(t *testing.T) groupWithOneBill {
	t.Helper()
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)

	router := smokeRouter(t)
	_, token := createPaidBillingTestUserSession(t)
	accounts := performJSONRequest[[]models.Account](
		t, router, http.MethodGet, "/v1/accounts", token, nil, http.StatusOK,
	)
	if len(accounts) == 0 {
		t.Fatal("guest account was not created")
	}

	entry := performJSONRequest[models.Entry](
		t, router, http.MethodPost, "/v1/entries", token,
		map[string]any{
			"title":      "Trip dinner",
			"type":       "expense",
			"amount":     "8000.00",
			"currency":   "INR",
			"source":     "manual",
			"mode":       "UPI",
			"category":   "Food & Drinks",
			"date":       "2026-08-13",
			"account_id": accounts[0].ID,
			"split": map[string]any{
				"group_name": "Goa trip",
				"participants": []map[string]any{
					{"friend": map[string]any{"name": "Riya"}, "share_amount": "4000.00"},
				},
			},
		}, http.StatusCreated,
	)

	bills := performJSONRequest[[]models.SplitBill](
		t, router, http.MethodGet, "/v1/split/bills", token, nil, http.StatusOK,
	)
	if len(bills) != 1 || bills[0].GroupID == nil {
		t.Fatalf("expected one grouped split bill, got %#v", bills)
	}

	// The state the bug was reported from: the friend owes half.
	balances := performJSONRequest[[]splitBalance](
		t, router, http.MethodGet, "/v1/split/balances", token, nil, http.StatusOK,
	)
	if len(balances) != 1 || balances[0].NetBalance.String() != "4000.00" {
		t.Fatalf("expected a 4000.00 balance before the delete, got %#v", balances)
	}

	return groupWithOneBill{router: router, token: token, groupID: *bills[0].GroupID, entryID: entry.ID}
}

// The reported bug, as a test: a deleted group kept feeding the overall figure.
//
// The group row was archived and nothing else was touched, so its participants
// stayed in the table `buildSplitBalances` sums — and the Split screen showed
// "settled up" on a group that no longer existed, over a total that still
// counted it.
func TestDeletingAGroupClearsItsBalances(t *testing.T) {
	seed := seedGroupWithOneBill(t)

	performJSONRequest[map[string]any](
		t, seed.router, http.MethodDelete,
		fmt.Sprintf("/v1/split/groups/%d", seed.groupID), seed.token, nil, http.StatusOK,
	)

	balances := performJSONRequest[[]splitBalance](
		t, seed.router, http.MethodGet, "/v1/split/balances", seed.token, nil, http.StatusOK,
	)
	for _, balance := range balances {
		if balance.NetBalance.String() != "0.00" {
			t.Fatalf("deleted group still owes: %#v", balance)
		}
	}

	bills := performJSONRequest[[]models.SplitBill](
		t, seed.router, http.MethodGet, "/v1/split/bills", seed.token, nil, http.StatusOK,
	)
	if len(bills) != 0 {
		t.Fatalf("expected the group's bills to go with it, got %#v", bills)
	}
}

// Keeping is the default, and the default destroys nothing.
//
// The transaction is the user's own record that money left their account, which
// it did — deleting a group settles who owed whom, not what was spent. An older
// app build sends no parameter at all and has to land here.
func TestDeletingAGroupKeepsItsTransactionsByDefault(t *testing.T) {
	seed := seedGroupWithOneBill(t)

	performJSONRequest[map[string]any](
		t, seed.router, http.MethodDelete,
		fmt.Sprintf("/v1/split/groups/%d", seed.groupID), seed.token, nil, http.StatusOK,
	)

	entry := performJSONRequest[models.Entry](
		t, seed.router, http.MethodGet,
		fmt.Sprintf("/v1/entries/%d", seed.entryID), seed.token, nil, http.StatusOK,
	)
	if entry.ID != seed.entryID {
		t.Fatalf("expected the transaction to survive the group, got %#v", entry)
	}

	// And it is no longer a split: the bill it belonged to is gone, so nothing
	// claims a share of it.
	orphan := performJSONRequest[*models.SplitBill](
		t, seed.router, http.MethodGet,
		fmt.Sprintf("/v1/split/bills/by-entry/%d", seed.entryID), seed.token, nil, http.StatusOK,
	)
	if orphan != nil {
		t.Fatalf("expected no split bill behind the kept entry, got %#v", orphan)
	}
}

// The other half of the choice, taken only when it is asked for by name.
func TestDeletingAGroupCanTakeItsTransactionsToo(t *testing.T) {
	seed := seedGroupWithOneBill(t)

	result := performJSONRequest[map[string]any](
		t, seed.router, http.MethodDelete,
		fmt.Sprintf("/v1/split/groups/%d?entries=delete", seed.groupID), seed.token, nil, http.StatusOK,
	)
	// Reported rather than assumed, so the app can say what it actually did.
	if count, _ := result["deleted_entries"].(float64); count != 1 {
		t.Fatalf("expected one deleted entry reported, got %#v", result["deleted_entries"])
	}

	performJSONRequest[map[string]any](
		t, seed.router, http.MethodGet,
		fmt.Sprintf("/v1/entries/%d", seed.entryID), seed.token, nil, http.StatusNotFound,
	)
}

// An unreadable disposition is refused rather than guessed at — guessing here
// is the difference between keeping a transaction and destroying it.
func TestDeletingAGroupRejectsAnUnknownEntryDisposition(t *testing.T) {
	seed := seedGroupWithOneBill(t)

	performJSONRequest[map[string]any](
		t, seed.router, http.MethodDelete,
		fmt.Sprintf("/v1/split/groups/%d?entries=maybe", seed.groupID), seed.token, nil,
		http.StatusUnprocessableEntity,
	)

	// Nothing happened: the group is still there and still owed.
	balances := performJSONRequest[[]splitBalance](
		t, seed.router, http.MethodGet, "/v1/split/balances", seed.token, nil, http.StatusOK,
	)
	if len(balances) != 1 || balances[0].NetBalance.String() != "4000.00" {
		t.Fatalf("a rejected delete must change nothing, got %#v", balances)
	}
}
