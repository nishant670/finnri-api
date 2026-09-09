package http

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"finnri/internal/database"
	"finnri/internal/models"
)

// The half of the deleted-group bug that TestDeletingAGroupClearsItsBalances
// did not reach.
//
// Deleting a group already took its bills and participants. Settlements were
// left: they named a friend and never a group, so a payment recorded to close
// the group's expenses outlived the expenses it closed and kept applying its
// full amount to the running total. The Splits screen then showed
// "Create your first group" above "Overall, you are owed ₹4,000", with no group,
// no bill and no friend row anywhere that could account for the figure.
func TestDeletingAGroupClearsTheSettlementsInsideIt(t *testing.T) {
	seed := seedGroupWithOneBill(t)

	friends := performJSONRequest[[]models.SplitFriend](
		t, seed.router, http.MethodGet, "/v1/split/friends", seed.token, nil, http.StatusOK,
	)
	if len(friends) != 1 {
		t.Fatalf("expected the one friend the split created, got %#v", friends)
	}

	// Half of what they owed, paid back inside the group.
	performJSONRequest[models.SplitSettlement](
		t, seed.router, http.MethodPost, "/v1/split/settlements", seed.token,
		map[string]any{
			"friend_id": friends[0].ID,
			"group_id":  seed.groupID,
			"amount":    "2000.00",
			"direction": settlementDirectionFriendPaidUser,
			"date":      "2026-08-20",
		}, http.StatusCreated,
	)

	balances := performJSONRequest[[]splitBalance](
		t, seed.router, http.MethodGet, "/v1/split/balances", seed.token, nil, http.StatusOK,
	)
	if len(balances) != 1 || balances[0].NetBalance.String() != "2000.00" {
		t.Fatalf("expected 2000.00 outstanding after the settlement, got %#v", balances)
	}

	performJSONRequest[map[string]any](
		t, seed.router, http.MethodDelete,
		fmt.Sprintf("/v1/split/groups/%d", seed.groupID), seed.token, nil, http.StatusOK,
	)

	balances = performJSONRequest[[]splitBalance](
		t, seed.router, http.MethodGet, "/v1/split/balances", seed.token, nil, http.StatusOK,
	)
	for _, balance := range balances {
		if balance.NetBalance.String() != "0.00" {
			t.Fatalf("a deleted group still moves the total: %#v", balance)
		}
	}

	settlements := performJSONRequest[[]models.SplitSettlement](
		t, seed.router, http.MethodGet, "/v1/split/settlements", seed.token, nil, http.StatusOK,
	)
	if len(settlements) != 0 {
		t.Fatalf("expected the group's settlements to go with it, got %#v", settlements)
	}
}

// A settlement recorded straight against a friend is not a group's to delete.
// Only the ones that named the group go with it.
func TestDeletingAGroupKeepsFriendLevelSettlements(t *testing.T) {
	seed := seedGroupWithOneBill(t)

	friends := performJSONRequest[[]models.SplitFriend](
		t, seed.router, http.MethodGet, "/v1/split/friends", seed.token, nil, http.StatusOK,
	)
	performJSONRequest[models.SplitSettlement](
		t, seed.router, http.MethodPost, "/v1/split/settlements", seed.token,
		map[string]any{
			"friend_id": friends[0].ID,
			"amount":    "500.00",
			"direction": settlementDirectionFriendPaidUser,
			"date":      "2026-08-20",
		}, http.StatusCreated,
	)

	performJSONRequest[map[string]any](
		t, seed.router, http.MethodDelete,
		fmt.Sprintf("/v1/split/groups/%d", seed.groupID), seed.token, nil, http.StatusOK,
	)

	settlements := performJSONRequest[[]models.SplitSettlement](
		t, seed.router, http.MethodGet, "/v1/split/settlements", seed.token, nil, http.StatusOK,
	)
	if len(settlements) != 1 {
		t.Fatalf("a friend-level settlement named no group and must survive it, got %#v", settlements)
	}
}

// Balances are summed from bills, not from participant rows on their own.
//
// A participant whose bill is gone is a fragment of a deleted record, not an
// unpaid debt — and because every screen builds its rows from bills, it moved
// the headline figure while appearing nowhere the user could reach it.
func TestBalancesIgnoreParticipantsWhoseBillIsGone(t *testing.T) {
	seed := seedGroupWithOneBill(t)

	// Delete the bill row the way a partial failure would have: participants
	// left behind. This is the shape of the data the fix has to survive, not a
	// path the app can still take.
	if err := database.DB.Exec("DELETE FROM split_bills").Error; err != nil {
		t.Fatalf("failed to strand the participants: %v", err)
	}

	var stranded int64
	if err := database.DB.Model(&models.SplitParticipant{}).Count(&stranded).Error; err != nil {
		t.Fatalf("failed to count participants: %v", err)
	}
	if stranded == 0 {
		t.Skip("the database cascaded the participants away, so there is nothing to strand")
	}

	balances := performJSONRequest[[]splitBalance](
		t, seed.router, http.MethodGet, "/v1/split/balances", seed.token, nil, http.StatusOK,
	)
	for _, balance := range balances {
		if balance.NetBalance.String() != "0.00" {
			t.Fatalf("a participant with no bill still owes: %#v", balance)
		}
	}
}

// Merging two duplicate friends must not invalidate a composer that is already
// open on the old id.
//
// The merge archives the loser, and the transaction composer only reads its
// people list when it opens. Saving afterwards used to come back as
// "Split.Participants[0].Friend must belong to the current user" about somebody
// the user could see in the group — naming a row that no longer exists, so
// there was nothing to go and fix.
func TestEntrySplitAcceptsAFriendIDThatWasMergedAway(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)

	router := smokeRouter(t)
	_, token := createPaidBillingTestUserSession(t)
	accounts := performJSONRequest[[]models.Account](
		t, router, http.MethodGet, "/v1/accounts", token, nil, http.StatusOK,
	)

	survivor := performJSONRequest[models.SplitFriend](
		t, router, http.MethodPost, "/v1/split/friends", token,
		map[string]any{"name": "Riya Dutta", "phone": "+919812345678"}, http.StatusCreated,
	)
	duplicate := performJSONRequest[models.SplitFriend](
		t, router, http.MethodPost, "/v1/split/friends", token,
		map[string]any{"name": "Riya", "email": "riya@example.com"}, http.StatusCreated,
	)

	performJSONRequest[map[string]any](
		t, router, http.MethodPost,
		fmt.Sprintf("/v1/split/friends/%d/merge-into/%d", duplicate.ID, survivor.ID),
		token, nil, http.StatusOK,
	)

	// The composer still holds the id it was opened with.
	entry := performJSONRequest[models.Entry](
		t, router, http.MethodPost, "/v1/entries", token,
		map[string]any{
			"title":      "Groceries",
			"type":       "expense",
			"amount":     "1200.00",
			"currency":   "INR",
			"source":     "manual",
			"mode":       "UPI",
			"category":   "Food & Drinks",
			"date":       "2026-08-21",
			"account_id": accounts[0].ID,
			"split": map[string]any{
				"participants": []map[string]any{
					{"friend_id": duplicate.ID, "share_amount": "600.00"},
				},
			},
		}, http.StatusCreated,
	)

	bill := performJSONRequest[*models.SplitBill](
		t, router, http.MethodGet,
		fmt.Sprintf("/v1/split/bills/by-entry/%d", entry.ID), token, nil, http.StatusOK,
	)
	if bill == nil || len(bill.Participants) != 1 {
		t.Fatalf("expected one participant on the saved split, got %#v", bill)
	}
	// Recorded against the row that absorbed the duplicate, so the share lands
	// on the friend the user can still see rather than on an archived row.
	if bill.Participants[0].FriendID != survivor.ID {
		t.Fatalf("expected the share on friend %d, got %d", survivor.ID, bill.Participants[0].FriendID)
	}
}

// A member of a shared group can attach a transaction split to it.
//
// The standalone split composer already allowed this; the transaction composer
// applied a stricter, owner-only rule and rejected both the group and every
// person in it, because a member's own friend list never contains the owner's
// friend rows.
func TestEntrySplitAcceptsASharedGroupAndItsMembers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)

	router := smokeRouter(t)
	owner, ownerToken := createPaidBillingTestUserSession(t)
	_ = owner

	friend := performJSONRequest[models.SplitFriend](
		t, router, http.MethodPost, "/v1/split/friends", ownerToken,
		map[string]any{"name": "Joiner", "email": "joiner@example.com"}, http.StatusCreated,
	)
	group := performJSONRequest[models.SplitGroup](
		t, router, http.MethodPost, "/v1/split/groups", ownerToken,
		map[string]any{"name": "Flat 402", "kind": "home", "friend_ids": []uint{friend.ID}},
		http.StatusCreated,
	)

	member, memberToken := createPaidBillingTestUserSession(t)
	if err := database.DB.Model(&models.User{}).Where("id = ?", member.ID).
		Update("email", "joiner@example.com").Error; err != nil {
		t.Fatalf("failed to give the member the invited address: %v", err)
	}
	invite := performJSONRequest[splitGroupInviteResponse](
		t, router, http.MethodPost,
		fmt.Sprintf("/v1/split/groups/%d/invite-link", group.ID), ownerToken, nil, http.StatusOK,
	)
	accepted := performJSONRequest[splitGroupInviteAcceptResponse](
		t, router, http.MethodPost,
		fmt.Sprintf("/v1/split/invites/%s/accept", invite.Token), memberToken, nil, http.StatusOK,
	)
	if accepted.Member.ID == 0 {
		t.Fatalf("expected the member to join the group, got %#v", accepted)
	}

	accounts := performJSONRequest[[]models.Account](
		t, router, http.MethodGet, "/v1/accounts", memberToken, nil, http.StatusOK,
	)
	// The member records an expense in the owner's group, splitting it with the
	// owner's friend row — the only namespace the group has.
	performJSONRequest[models.Entry](
		t, router, http.MethodPost, "/v1/entries", memberToken,
		map[string]any{
			"title":      "Wifi bill",
			"type":       "expense",
			"amount":     "1000.00",
			"currency":   "INR",
			"source":     "manual",
			"mode":       "UPI",
			"category":   "Bills & Utilities",
			"date":       "2026-08-22",
			"account_id": accounts[0].ID,
			"split": map[string]any{
				"group_id": group.ID,
				"participants": []map[string]any{
					{"friend_id": friend.ID, "share_amount": "500.00"},
				},
			},
		}, http.StatusCreated,
	)
}
