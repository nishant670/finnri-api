package http

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"finnri/internal/models"
)

// Archiving a friend has to take them out of the groups they were in.
//
// The whole reason somebody archives a friend row is that it turned out to be a
// second copy of a person already in the group under another one — which is
// exactly the state a phone-based invite and an email-based signup produce. So
// the cleanup path is the one that most needs to actually clean up.
func TestArchivingAFriendRemovesTheirGroupMembership(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)

	router := smokeRouter(t)
	_, token := createPaidBillingTestUserSession(t)

	friend := performJSONRequest[models.SplitFriend](
		t, router, http.MethodPost, "/v1/split/friends", token,
		map[string]any{"name": "Riya Dutta", "phone": "9871801518"}, http.StatusCreated,
	)
	group := performJSONRequest[models.SplitGroup](
		t, router, http.MethodPost, "/v1/split/groups", token,
		map[string]any{"name": "Goa trip", "kind": "trip", "friend_ids": []uint{friend.ID}},
		http.StatusCreated,
	)
	if len(group.Members) != 1 {
		t.Fatalf("expected the friend to be a member to begin with, got %#v", group.Members)
	}

	performJSONRequest[map[string]string](
		t, router, http.MethodDelete,
		fmt.Sprintf("/v1/split/friends/%d", friend.ID), token, nil, http.StatusOK,
	)

	groups := performJSONRequest[[]models.SplitGroup](
		t, router, http.MethodGet, "/v1/split/groups", token, nil, http.StatusOK,
	)
	for _, listed := range groups {
		if listed.ID != group.ID {
			continue
		}
		for _, member := range listed.Members {
			if member.FriendID == friend.ID {
				// The state behind the report: a group listing a member the
				// friends list will never return, because both read the same
				// `archived` flag and only one of them was filtering on it.
				t.Fatalf("archived friend is still a member of %q", listed.Name)
			}
		}
	}

	friends := performJSONRequest[[]models.SplitFriend](
		t, router, http.MethodGet, "/v1/split/friends", token, nil, http.StatusOK,
	)
	for _, listed := range friends {
		if listed.ID == friend.ID {
			t.Fatalf("archived friend is still in the friends list: %#v", listed)
		}
	}
}

// A share recorded against an archived friend is money that goes nowhere.
//
// `buildSplitBalances` walks active friends and never reaches a participant row
// belonging to an archived one, so the amount does not land in the wrong place
// — it disappears from every figure the app shows. The bill has to be refused
// rather than accepted and quietly dropped.
func TestGroupBillCannotNameAnArchivedFriend(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)

	router := smokeRouter(t)
	_, token := createPaidBillingTestUserSession(t)

	friend := performJSONRequest[models.SplitFriend](
		t, router, http.MethodPost, "/v1/split/friends", token,
		map[string]any{"name": "Riya Dutta", "phone": "9871801518"}, http.StatusCreated,
	)
	keeper := performJSONRequest[models.SplitFriend](
		t, router, http.MethodPost, "/v1/split/friends", token,
		map[string]any{"name": "Aarav", "phone": "9999999999"}, http.StatusCreated,
	)
	group := performJSONRequest[models.SplitGroup](
		t, router, http.MethodPost, "/v1/split/groups", token,
		map[string]any{
			"name":       "Goa trip",
			"kind":       "trip",
			"friend_ids": []uint{friend.ID, keeper.ID},
		}, http.StatusCreated,
	)

	performJSONRequest[map[string]string](
		t, router, http.MethodDelete,
		fmt.Sprintf("/v1/split/friends/%d", friend.ID), token, nil, http.StatusOK,
	)

	performJSONRequest[map[string]any](
		t, router, http.MethodPost, "/v1/split/bills", token,
		map[string]any{
			"group_id":     group.ID,
			"title":        "Dinner",
			"total_amount": "1000.00",
			"currency":     "INR",
			"date":         "2026-08-27",
			"participants": []map[string]any{
				{
					"friend_id":    friend.ID,
					"share_amount": "500.00",
					"direction":    splitDirectionFriendOwesUser,
				},
			},
		}, http.StatusUnprocessableEntity,
	)

	// The friend who is still there is unaffected — the rule is about the
	// archived row, not about the group.
	performJSONRequest[models.SplitBill](
		t, router, http.MethodPost, "/v1/split/bills", token,
		map[string]any{
			"group_id":     group.ID,
			"title":        "Dinner",
			"total_amount": "1000.00",
			"currency":     "INR",
			"date":         "2026-08-27",
			"participants": []map[string]any{
				{
					"friend_id":    keeper.ID,
					"share_amount": "500.00",
					"direction":    splitDirectionFriendOwesUser,
				},
			},
		}, http.StatusCreated,
	)
}
