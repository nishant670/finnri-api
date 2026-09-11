package http

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"finnri/internal/database"
	"finnri/internal/models"
)

// joinedSplitGroup sets up the shape every test below needs: an owner with a
// friend row for the member, a group, and the member having accepted the
// invite. Returns the group as the *member* sees it.
func joinedSplitGroup(t *testing.T) (
	ownerToken string,
	memberToken string,
	ownerFriendForMember models.SplitFriend,
	group models.SplitGroup,
) {
	t.Helper()
	router := smokeRouter(t)
	_, ownerToken = createPaidBillingTestUserSession(t)
	member, memberToken := createPaidBillingTestUserSession(t)

	// The smoke fixture builds users with a username and nothing else, so give
	// the member a number to be recognised by. Saved on the friend row too, so
	// acceptance lands on the row the owner has been splitting against instead
	// of raising a duplicate — that duplicate is a separate bug with its own
	// test, and it would mask what this one is checking.
	phone := "+91 98200 11223"
	member.Phone = &phone
	if err := database.DB.Save(&member).Error; err != nil {
		t.Fatalf("save member phone: %v", err)
	}
	ownerFriendForMember = performJSONRequest[models.SplitFriend](
		t, router, http.MethodPost, "/v1/split/friends", ownerToken,
		map[string]any{"name": "Wife", "phone": phone}, http.StatusCreated,
	)
	created := performJSONRequest[models.SplitGroup](
		t, router, http.MethodPost, "/v1/split/groups", ownerToken,
		map[string]any{"name": "Home", "friend_ids": []uint{ownerFriendForMember.ID}}, http.StatusCreated,
	)
	invite := performJSONRequest[splitGroupInviteResponse](
		t, router, http.MethodPost, fmt.Sprintf("/v1/split/groups/%d/invite-link", created.ID), ownerToken,
		nil, http.StatusOK,
	)
	accepted := performJSONRequest[splitGroupInviteAcceptResponse](
		t, router, http.MethodPost, fmt.Sprintf("/v1/split/invites/%s/accept", invite.Token), memberToken,
		nil, http.StatusOK,
	)
	if accepted.Friend.ID != ownerFriendForMember.ID {
		t.Fatalf("expected acceptance to reuse the owner's existing row %d, got %d",
			ownerFriendForMember.ID, accepted.Friend.ID)
	}
	return ownerToken, memberToken, accepted.Friend, accepted.Group
}

// The reported bug, end to end: a member opening a shared group could name
// nobody but herself, because the roster is written in the owner's namespace
// and the owner is never one of their own friend rows. She saw two copies of
// herself — "you" plus her own member row — and no owner at all.
func TestSharedSplitGroupGivesMemberARowForTheOwner(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)

	_, memberToken, ownerFriendForMember, _ := joinedSplitGroup(t)

	groups := performJSONRequest[[]models.SplitGroup](
		t, router, http.MethodGet, "/v1/split/groups", memberToken, nil, http.StatusOK,
	)
	if len(groups) != 1 {
		t.Fatalf("expected the member to see one shared group, got %#v", groups)
	}
	shared := groups[0]

	if shared.ViewerRole != "member" {
		t.Fatalf("expected viewer_role=member, got %q", shared.ViewerRole)
	}
	ownerSlotFriendID, ok := shared.ViewerSlotFriends[models.SplitGroupDefaultSplitOwnerSlot]
	if !ok || ownerSlotFriendID == 0 {
		t.Fatalf("member has no friend row for the group owner: %#v", shared.ViewerSlotFriends)
	}

	// The member must never be handed a row standing for herself: that is the
	// second copy of her that turned a two-person split into a three-way one.
	selfSlot := splitGroupSlotForFriend(ownerFriendForMember.ID)
	if _, ok := shared.ViewerSlotFriends[selfSlot]; ok {
		t.Fatalf("member was given a link to her own slot %q: %#v", selfSlot, shared.ViewerSlotFriends)
	}
	if shared.ViewerFriendID == nil || *shared.ViewerFriendID != ownerFriendForMember.ID {
		t.Fatalf("expected viewer_friend_id to name the member's own row, got %#v", shared.ViewerFriendID)
	}

	// And the row she was given has to be one she actually owns, or she cannot
	// put it on a bill.
	friends := performJSONRequest[[]models.SplitFriend](
		t, router, http.MethodGet, "/v1/split/friends", memberToken, nil, http.StatusOK,
	)
	var owned bool
	for _, friend := range friends {
		if friend.ID == ownerSlotFriendID {
			owned = true
		}
	}
	if !owned {
		t.Fatalf("owner slot %d is not in the member's own friend list %#v", ownerSlotFriendID, friends)
	}
}

// The point of the row: a member can record that the owner owes them, and it
// lands somewhere their own balances can actually reach.
func TestMemberCanBillTheGroupOwnerAndSeeTheBalance(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)

	_, memberToken, _, group := joinedSplitGroup(t)

	shared := performJSONRequest[[]models.SplitGroup](
		t, router, http.MethodGet, "/v1/split/groups", memberToken, nil, http.StatusOK,
	)[0]
	ownerSlotFriendID := shared.ViewerSlotFriends[models.SplitGroupDefaultSplitOwnerSlot]

	bill := performJSONRequest[models.SplitBill](
		t, router, http.MethodPost, "/v1/split/bills", memberToken,
		map[string]any{
			"title":        "Groceries",
			"total_amount": "1000.00",
			"currency":     "INR",
			"date":         "2026-09-10",
			"group_id":     group.ID,
			"participants": []map[string]any{
				{
					"friend_id":    ownerSlotFriendID,
					"share_amount": "600.00",
					"direction":    splitDirectionFriendOwesUser,
				},
			},
		}, http.StatusCreated,
	)
	if bill.ID == 0 || len(bill.Participants) != 1 {
		t.Fatalf("member could not bill the group owner: %#v", bill)
	}

	// Before the links existed this came back empty: the share named a row the
	// member did not own, and buildSplitBalances walks the friends you own.
	balances := performJSONRequest[[]splitBalance](
		t, router, http.MethodGet, "/v1/split/balances", memberToken, nil, http.StatusOK,
	)
	var owedByOwner string
	for _, balance := range balances {
		if balance.Friend.ID == ownerSlotFriendID {
			owedByOwner = balance.NetBalance.String()
		}
	}
	if owedByOwner != "600.00" {
		t.Fatalf("expected the owner to owe the member 600.00, got %q from %#v", owedByOwner, balances)
	}
}

// A member splitting into a shared group must not be able to extend its roster.
// Those rows were written in the member's name, and the owner's roster rewrite
// only ever deletes its own — so they stuck permanently with no way out.
func TestMemberEntrySplitDoesNotWriteGroupMembership(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)

	ownerToken, memberToken, _, group := joinedSplitGroup(t)

	shared := performJSONRequest[[]models.SplitGroup](
		t, router, http.MethodGet, "/v1/split/groups", memberToken, nil, http.StatusOK,
	)[0]
	ownerSlotFriendID := shared.ViewerSlotFriends[models.SplitGroupDefaultSplitOwnerSlot]

	before := performJSONRequest[[]models.SplitGroup](
		t, router, http.MethodGet, "/v1/split/groups", ownerToken, nil, http.StatusOK,
	)[0]

	performJSONRequest[models.Entry](
		t, router, http.MethodPost, "/v1/entries", memberToken,
		map[string]any{
			"title":    "Dinner",
			"type":     "expense",
			"amount":   "800.00",
			"currency": "INR",
			"mode":     "Cash",
			"category": "Food & Drinks",
			"date":     "2026-09-10",
			"split": map[string]any{
				"group_id": group.ID,
				"participants": []map[string]any{
					{
						"friend_id":    ownerSlotFriendID,
						"share_amount": "400.00",
						"direction":    splitDirectionFriendOwesUser,
					},
				},
			},
		}, http.StatusCreated,
	)

	after := performJSONRequest[[]models.SplitGroup](
		t, router, http.MethodGet, "/v1/split/groups", ownerToken, nil, http.StatusOK,
	)[0]
	if len(after.Members) != len(before.Members) {
		t.Fatalf("member's entry split changed the owner's roster from %d to %d members",
			len(before.Members), len(after.Members))
	}

	// And nothing in the roster may belong to anybody but the owner, or the
	// owner's next roster rewrite will silently leave it behind.
	var strays int64
	if err := database.DB.Model(&models.SplitGroupMember{}).
		Where("group_id = ? AND user_id <> ?", group.ID, before.UserID).
		Count(&strays).Error; err != nil {
		t.Fatalf("count stray memberships: %v", err)
	}
	if strays != 0 {
		t.Fatalf("expected no membership rows owned by a non-owner, found %d", strays)
	}
}

// Groups that were already shared before slot links existed have to heal
// themselves: their owners have no reason to touch the roster again.
func TestPreexistingSharedGroupBackfillsMemberLinksOnRead(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)

	_, memberToken, _, group := joinedSplitGroup(t)

	// Wind the group back to what it looked like before this feature: a
	// membership, and no translation of the roster for anybody.
	if err := database.DB.Where("group_id = ?", group.ID).
		Delete(&models.SplitGroupMemberLink{}).Error; err != nil {
		t.Fatalf("clear links: %v", err)
	}

	shared := performJSONRequest[[]models.SplitGroup](
		t, router, http.MethodGet, "/v1/split/groups", memberToken, nil, http.StatusOK,
	)
	if len(shared) != 1 {
		t.Fatalf("expected one shared group, got %#v", shared)
	}
	if shared[0].ViewerSlotFriends[models.SplitGroupDefaultSplitOwnerSlot] == 0 {
		t.Fatalf("reading the group did not backfill the owner link: %#v", shared[0].ViewerSlotFriends)
	}
}

// A failed or interrupted migration can leave some translations behind. The
// presence of one must not suppress healing of the rest of the roster.
func TestPreexistingSharedGroupBackfillsPartiallyMissingMemberLinks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)

	ownerToken, memberToken, _, group := joinedSplitGroup(t)
	second := performJSONRequest[models.SplitFriend](
		t, router, http.MethodPost, "/v1/split/friends", ownerToken,
		map[string]any{"name": "Roommate", "phone": "+91 98200 44556"}, http.StatusCreated,
	)
	performJSONRequest[models.SplitGroup](
		t, router, http.MethodPut, fmt.Sprintf("/v1/split/groups/%d", group.ID), ownerToken,
		map[string]any{"name": "Home", "friend_ids": []uint{group.Members[0].FriendID, second.ID}}, http.StatusOK,
	)

	secondSlot := splitGroupSlotForFriend(second.ID)
	if err := database.DB.Where("group_id = ? AND user_id IN (?) AND slot = ?", group.ID,
		database.DB.Model(&models.SplitGroupUserMember{}).Select("user_id").Where("group_id = ?", group.ID), secondSlot).
		Delete(&models.SplitGroupMemberLink{}).Error; err != nil {
		t.Fatalf("delete one translated slot: %v", err)
	}

	shared := performJSONRequest[[]models.SplitGroup](
		t, router, http.MethodGet, "/v1/split/groups", memberToken, nil, http.StatusOK,
	)
	if len(shared) != 1 || shared[0].ViewerSlotFriends[secondSlot] == 0 {
		t.Fatalf("reading the group did not repair its missing slot %q: %#v", secondSlot, shared)
	}
}
