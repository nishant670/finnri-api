package http

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"finnri/internal/models"
)

// balanceFor reads one friend's row out of a user's balances.
func balanceFor(t *testing.T, router *gin.Engine, token string, friendID uint) splitBalance {
	t.Helper()
	balances := performJSONRequest[[]splitBalance](
		t, router, http.MethodGet, "/v1/split/balances", token, nil, http.StatusOK,
	)
	for _, balance := range balances {
		if balance.Friend.ID == friendID {
			return balance
		}
	}
	t.Fatalf("no balance row for friend %d in %#v", friendID, balances)
	return splitBalance{}
}

// memberRowForOwner is the friend row a member uses to mean the group's owner.
func memberRowForOwner(t *testing.T, router *gin.Engine, token string, groupID uint) uint {
	t.Helper()
	groups := performJSONRequest[[]models.SplitGroup](
		t, router, http.MethodGet, "/v1/split/groups", token, nil, http.StatusOK,
	)
	for _, group := range groups {
		if group.ID == groupID {
			if friendID := group.ViewerSlotFriends[models.SplitGroupDefaultSplitOwnerSlot]; friendID != 0 {
				return friendID
			}
		}
	}
	t.Fatalf("member has no row for the owner of group %d", groupID)
	return 0
}

// The reported hole: a bill records the debts of its author against friend rows
// only its author owns, so the owner summed their own bills and never saw a
// rupee of what the member recorded. Both sides read "settled up" over a group
// with money moving through it.
func TestOwnerSeesExpensesAMemberRecorded(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)

	ownerToken, memberToken, ownerFriendForMember, group := joinedSplitGroup(t)
	memberFriendForOwner := memberRowForOwner(t, router, memberToken, group.ID)

	// The member pays 1000 and puts 600 of it on the owner.
	performJSONRequest[models.SplitBill](
		t, router, http.MethodPost, "/v1/split/bills", memberToken,
		map[string]any{
			"title": "Groceries", "total_amount": "1000.00", "currency": "INR",
			"date": "2026-09-10", "group_id": group.ID,
			"participants": []map[string]any{
				{
					"friend_id":    memberFriendForOwner,
					"share_amount": "600.00",
					"direction":    splitDirectionFriendOwesUser,
				},
			},
		}, http.StatusCreated,
	)

	ownerSide := balanceFor(t, router, ownerToken, ownerFriendForMember.ID)
	if ownerSide.TotalOwedToFriend.String() != "600.00" {
		t.Fatalf("owner should owe the member 600.00, got %#v", ownerSide)
	}
	if ownerSide.NetBalance.String() != "-600.00" {
		t.Fatalf("owner net should be -600.00, got %q", ownerSide.NetBalance.String())
	}

	// The member's own side is unchanged by the fold — her row already carried
	// it, and counting it twice would be its own bug.
	memberSide := balanceFor(t, router, memberToken, memberFriendForOwner)
	if memberSide.TotalOwedByFriend.String() != "600.00" || memberSide.NetBalance.String() != "600.00" {
		t.Fatalf("member should be owed exactly 600.00 once, got %#v", memberSide)
	}
}

// Both sides of the ledger, plus the payment that closes it. The two people
// must arrive at equal and opposite figures at every step, and at zero once the
// settlement lands.
func TestSharedGroupLedgerNetsBothSidesAndSettlements(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)

	ownerToken, memberToken, ownerFriendForMember, group := joinedSplitGroup(t)
	memberFriendForOwner := memberRowForOwner(t, router, memberToken, group.ID)

	// Owner pays, member owes 400.
	performJSONRequest[models.SplitBill](
		t, router, http.MethodPost, "/v1/split/bills", ownerToken,
		map[string]any{
			"title": "Wifi", "total_amount": "1000.00", "currency": "INR",
			"date": "2026-09-09", "group_id": group.ID,
			"participants": []map[string]any{
				{
					"friend_id":    ownerFriendForMember.ID,
					"share_amount": "400.00",
					"direction":    splitDirectionFriendOwesUser,
				},
			},
		}, http.StatusCreated,
	)
	// Member pays, owner owes 600.
	performJSONRequest[models.SplitBill](
		t, router, http.MethodPost, "/v1/split/bills", memberToken,
		map[string]any{
			"title": "Groceries", "total_amount": "1000.00", "currency": "INR",
			"date": "2026-09-10", "group_id": group.ID,
			"participants": []map[string]any{
				{
					"friend_id":    memberFriendForOwner,
					"share_amount": "600.00",
					"direction":    splitDirectionFriendOwesUser,
				},
			},
		}, http.StatusCreated,
	)

	owner := balanceFor(t, router, ownerToken, ownerFriendForMember.ID)
	member := balanceFor(t, router, memberToken, memberFriendForOwner)
	if owner.NetBalance.String() != "-200.00" {
		t.Fatalf("owner net should be -200.00 (owed 400, owes 600), got %q", owner.NetBalance.String())
	}
	if member.NetBalance.String() != "200.00" {
		t.Fatalf("member net should be 200.00, got %q", member.NetBalance.String())
	}

	// The owner settles the 200 difference.
	performJSONRequest[models.SplitSettlement](
		t, router, http.MethodPost, "/v1/split/settlements", ownerToken,
		map[string]any{
			"friend_id": ownerFriendForMember.ID,
			"group_id":  group.ID,
			"amount":    "200.00",
			"direction": settlementDirectionUserPaidFriend,
			"date":      "2026-09-11",
		}, http.StatusCreated,
	)

	owner = balanceFor(t, router, ownerToken, ownerFriendForMember.ID)
	member = balanceFor(t, router, memberToken, memberFriendForOwner)
	if owner.NetBalance.String() != "0.00" {
		t.Fatalf("owner should be settled up, got %q", owner.NetBalance.String())
	}
	// The settlement was written by the owner against a row the member does not
	// own, so without folding it the member would still be showing 200 owed.
	if member.NetBalance.String() != "0.00" {
		t.Fatalf("member should be settled up, got %q", member.NetBalance.String())
	}
}

// A group nobody else is in must not pick up anything from the fold.
func TestSoloSplitBalancesAreUnaffectedByTheFold(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, token := createPaidBillingTestUserSession(t)

	friend := performJSONRequest[models.SplitFriend](
		t, router, http.MethodPost, "/v1/split/friends", token,
		map[string]any{"name": "Aarav"}, http.StatusCreated,
	)
	group := performJSONRequest[models.SplitGroup](
		t, router, http.MethodPost, "/v1/split/groups", token,
		map[string]any{"name": "Trip", "friend_ids": []uint{friend.ID}}, http.StatusCreated,
	)
	performJSONRequest[models.SplitBill](
		t, router, http.MethodPost, "/v1/split/bills", token,
		map[string]any{
			"title": "Cab", "total_amount": "500.00", "currency": "INR",
			"date": "2026-09-10", "group_id": group.ID,
			"participants": []map[string]any{
				{
					"friend_id":    friend.ID,
					"share_amount": "250.00",
					"direction":    splitDirectionFriendOwesUser,
				},
			},
		}, http.StatusCreated,
	)

	balance := balanceFor(t, router, token, friend.ID)
	if balance.NetBalance.String() != "250.00" {
		t.Fatalf("expected a plain 250.00 with no fold applied, got %#v", balance)
	}
}

// groupView reads one group out of a user's groups list.
func groupView(t *testing.T, router *gin.Engine, token string, groupID uint) models.SplitGroup {
	t.Helper()
	groups := performJSONRequest[[]models.SplitGroup](
		t, router, http.MethodGet, "/v1/split/groups", token, nil, http.StatusOK,
	)
	for _, group := range groups {
		if group.ID == groupID {
			return group
		}
	}
	t.Fatalf("group %d not in %#v", groupID, groups)
	return models.SplitGroup{}
}

// The group card's own figure. It used to be summed on the client from every
// participant row in the group, reading `direction` as absolute — so a member's
// expense arrived on the owner's card with its sign inverted, telling him she
// owed him money she had actually laid out for him.
func TestGroupBalanceIsSignedFromTheViewersSide(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)

	ownerToken, memberToken, ownerFriendForMember, group := joinedSplitGroup(t)
	memberFriendForOwner := memberRowForOwner(t, router, memberToken, group.ID)

	// The member pays and puts 600 on the owner.
	performJSONRequest[models.SplitBill](
		t, router, http.MethodPost, "/v1/split/bills", memberToken,
		map[string]any{
			"title": "Groceries", "total_amount": "1000.00", "currency": "INR",
			"date": "2026-09-10", "group_id": group.ID,
			"participants": []map[string]any{
				{
					"friend_id":    memberFriendForOwner,
					"share_amount": "600.00",
					"direction":    splitDirectionFriendOwesUser,
				},
			},
		}, http.StatusCreated,
	)

	ownerGroup := groupView(t, router, ownerToken, group.ID)
	if ownerGroup.ViewerNetBalance.String() != "-600.00" {
		t.Fatalf("owner's group card should read -600.00, got %q", ownerGroup.ViewerNetBalance.String())
	}
	if len(ownerGroup.ViewerBalances) != 1 ||
		ownerGroup.ViewerBalances[0].FriendID != ownerFriendForMember.ID ||
		ownerGroup.ViewerBalances[0].NetBalance.String() != "-600.00" {
		t.Fatalf("owner's per-person line is wrong: %#v", ownerGroup.ViewerBalances)
	}

	memberGroup := groupView(t, router, memberToken, group.ID)
	if memberGroup.ViewerNetBalance.String() != "600.00" {
		t.Fatalf("member's group card should read 600.00, got %q", memberGroup.ViewerNetBalance.String())
	}
	if len(memberGroup.ViewerBalances) != 1 ||
		memberGroup.ViewerBalances[0].FriendID != memberFriendForOwner {
		t.Fatalf("member's per-person line is wrong: %#v", memberGroup.ViewerBalances)
	}
}

// A member could add expenses to a shared group but never close one: the only
// friend rows the roster named belonged to the owner, and settlements demanded
// a row the caller owned. The slot links satisfy that, and the group's roster
// is now the rule rather than bare ownership.
func TestMemberCanSettleInsideASharedGroup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)

	ownerToken, memberToken, ownerFriendForMember, group := joinedSplitGroup(t)
	memberFriendForOwner := memberRowForOwner(t, router, memberToken, group.ID)

	// The owner pays; the member owes 400.
	performJSONRequest[models.SplitBill](
		t, router, http.MethodPost, "/v1/split/bills", ownerToken,
		map[string]any{
			"title": "Wifi", "total_amount": "1000.00", "currency": "INR",
			"date": "2026-09-09", "group_id": group.ID,
			"participants": []map[string]any{
				{
					"friend_id":    ownerFriendForMember.ID,
					"share_amount": "400.00",
					"direction":    splitDirectionFriendOwesUser,
				},
			},
		}, http.StatusCreated,
	)

	// The member pays it back.
	performJSONRequest[models.SplitSettlement](
		t, router, http.MethodPost, "/v1/split/settlements", memberToken,
		map[string]any{
			"friend_id": memberFriendForOwner,
			"group_id":  group.ID,
			"amount":    "400.00",
			"direction": settlementDirectionUserPaidFriend,
			"date":      "2026-09-11",
		}, http.StatusCreated,
	)

	if owner := balanceFor(t, router, ownerToken, ownerFriendForMember.ID); owner.NetBalance.String() != "0.00" {
		t.Fatalf("owner should be settled up after the member paid, got %q", owner.NetBalance.String())
	}
	if member := balanceFor(t, router, memberToken, memberFriendForOwner); member.NetBalance.String() != "0.00" {
		t.Fatalf("member should be settled up after paying, got %q", member.NetBalance.String())
	}
}

// Ownership alone would let a member close a group's balance against somebody
// who has nothing to do with that group.
func TestGroupSettlementMustNameSomebodyInTheGroup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)

	_, memberToken, _, group := joinedSplitGroup(t)

	outsider := performJSONRequest[models.SplitFriend](
		t, router, http.MethodPost, "/v1/split/friends", memberToken,
		map[string]any{"name": "Someone Else"}, http.StatusCreated,
	)
	performJSONRequest[map[string]any](
		t, router, http.MethodPost, "/v1/split/settlements", memberToken,
		map[string]any{
			"friend_id": outsider.ID,
			"group_id":  group.ID,
			"amount":    "100.00",
			"direction": settlementDirectionUserPaidFriend,
			"date":      "2026-09-11",
		}, http.StatusUnprocessableEntity,
	)
}

// The feed used to narrate only the viewer's own half of a shared group, while
// the group's own screen listed everybody's expenses.
func TestActivityShowsWhatTheOtherSideRecorded(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)

	ownerToken, memberToken, _, group := joinedSplitGroup(t)
	memberFriendForOwner := memberRowForOwner(t, router, memberToken, group.ID)

	performJSONRequest[models.SplitBill](
		t, router, http.MethodPost, "/v1/split/bills", memberToken,
		map[string]any{
			"title": "Groceries", "total_amount": "1000.00", "currency": "INR",
			"date": "2026-09-10", "group_id": group.ID,
			"participants": []map[string]any{
				{
					"friend_id":    memberFriendForOwner,
					"share_amount": "600.00",
					"direction":    splitDirectionFriendOwesUser,
				},
			},
		}, http.StatusCreated,
	)
	performJSONRequest[models.SplitSettlement](
		t, router, http.MethodPost, "/v1/split/settlements", memberToken,
		map[string]any{
			"friend_id": memberFriendForOwner,
			"group_id":  group.ID,
			"amount":    "100.00",
			"direction": settlementDirectionFriendPaidUser,
			"date":      "2026-09-11",
		}, http.StatusCreated,
	)

	ownerFeed := performJSONRequest[struct {
		Items []splitActivityItem `json:"items"`
	}](t, router, http.MethodGet, "/v1/split/activity", ownerToken, nil, http.StatusOK)

	var sawBill, sawSettlement bool
	for _, item := range ownerFeed.Items {
		if item.Type == "bill" && item.Title == "Groceries" {
			sawBill = true
			if item.ActorName == "" {
				t.Fatalf("the owner's feed does not say who recorded the expense: %#v", item)
			}
		}
		if item.Type == "settlement" {
			sawSettlement = true
			// The member recorded "the owner paid me". From the owner's side
			// that has to read as the owner paying, not being paid.
			if item.Direction != settlementDirectionUserPaidFriend {
				t.Fatalf("settlement not restated for the owner: %#v", item)
			}
			if item.Title == "" || item.Title == "Settlement" {
				t.Fatalf("settlement has no restated title: %#v", item)
			}
		}
	}
	if !sawBill {
		t.Fatalf("the member's expense is missing from the owner's feed: %#v", ownerFeed.Items)
	}
	if !sawSettlement {
		t.Fatalf("the member's settlement is missing from the owner's feed: %#v", ownerFeed.Items)
	}
}

// A group you joined is part of your history too; it used to appear in nobody's
// feed but its creator's.
func TestActivityIncludesGroupsTheViewerJoined(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)

	_, memberToken, _, group := joinedSplitGroup(t)

	feed := performJSONRequest[struct {
		Items []splitActivityItem `json:"items"`
	}](t, router, http.MethodGet, "/v1/split/activity", memberToken, nil, http.StatusOK)

	for _, item := range feed.Items {
		if item.Type == "group_created" && item.GroupID != nil && *item.GroupID == group.ID {
			return
		}
	}
	t.Fatalf("the group the member joined is missing from their feed: %#v", feed.Items)
}
