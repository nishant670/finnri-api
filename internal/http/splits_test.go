package http

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"finnri/internal/database"
	"finnri/internal/models"
)

func TestSplitLedgerFlowComputesFriendBalances(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)

	router := smokeRouter(t)
	_, token := createPaidBillingTestUserSession(t)

	friend := performJSONRequest[models.SplitFriend](
		t, router, http.MethodPost, "/v1/split/friends", token,
		map[string]any{"name": "Aarav", "phone": "+919999999999"}, http.StatusCreated,
	)
	if friend.ID == 0 || friend.Name != "Aarav" {
		t.Fatalf("unexpected split friend: %#v", friend)
	}

	bill := performJSONRequest[models.SplitBill](
		t, router, http.MethodPost, "/v1/split/bills", token,
		map[string]any{
			"title":        "Dinner",
			"total_amount": "1000.00",
			"currency":     "INR",
			"date":         "2026-07-12",
			"participants": []map[string]any{
				{
					"friend_id":    friend.ID,
					"share_amount": "400.00",
					"direction":    splitDirectionFriendOwesUser,
				},
			},
		}, http.StatusCreated,
	)
	if bill.ID == 0 || len(bill.Participants) != 1 {
		t.Fatalf("unexpected split bill: %#v", bill)
	}

	settlement := performJSONRequest[models.SplitSettlement](
		t, router, http.MethodPost, "/v1/split/settlements", token,
		map[string]any{
			"friend_id": friend.ID,
			"amount":    "150.00",
			"direction": settlementDirectionFriendPaidUser,
			"date":      "2026-07-13",
		}, http.StatusCreated,
	)
	if settlement.ID == 0 || settlement.FriendID != friend.ID {
		t.Fatalf("unexpected split settlement: %#v", settlement)
	}

	balances := performJSONRequest[[]splitBalance](
		t, router, http.MethodGet, "/v1/split/balances", token, nil, http.StatusOK,
	)
	if len(balances) != 1 {
		t.Fatalf("expected one split balance, got %#v", balances)
	}
	if balances[0].TotalOwedByFriend.String() != "250.00" || balances[0].NetBalance.String() != "250.00" {
		t.Fatalf("unexpected split balance: %#v", balances[0])
	}

	activity := performJSONRequest[struct {
		Items []splitActivityItem `json:"items"`
	}](
		t, router, http.MethodGet, "/v1/split/activity", token, nil, http.StatusOK,
	)
	if len(activity.Items) < 3 {
		t.Fatalf("expected split activity for friend, bill, and settlement, got %#v", activity.Items)
	}
	seenTypes := map[string]bool{}
	for _, item := range activity.Items {
		seenTypes[item.Type] = true
	}
	for _, activityType := range []string{"friend_created", "bill", "settlement"} {
		if !seenTypes[activityType] {
			t.Fatalf("expected activity type %s in %#v", activityType, activity.Items)
		}
	}
}

func TestSplitGroupCanBeCreatedBeforeMembers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)

	router := smokeRouter(t)
	_, token := createPaidBillingTestUserSession(t)

	group := performJSONRequest[models.SplitGroup](
		t, router, http.MethodPost, "/v1/split/groups", token,
		map[string]any{"name": "Bubu Dudu", "friend_ids": []uint{}}, http.StatusCreated,
	)
	if group.ID == 0 || group.Name != "Bubu Dudu" || len(group.Members) != 0 {
		t.Fatalf("expected empty split group to be created, got %#v", group)
	}
	activity := performJSONRequest[struct {
		Items []splitActivityItem `json:"items"`
	}](
		t, router, http.MethodGet, "/v1/split/activity", token, nil, http.StatusOK,
	)
	if len(activity.Items) == 0 || activity.Items[0].Type != "group_created" || activity.Items[0].Group == nil {
		t.Fatalf("expected group creation activity, got %#v", activity.Items)
	}
}

func TestSplitGroupInviteLinkCanBeCreatedAndReused(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)

	router := smokeRouter(t)
	_, token := createPaidBillingTestUserSession(t)

	group := performJSONRequest[models.SplitGroup](
		t, router, http.MethodPost, "/v1/split/groups", token,
		map[string]any{"name": "Bhaukaal", "friend_ids": []uint{}}, http.StatusCreated,
	)

	firstInvite := performJSONRequest[splitGroupInviteResponse](
		t, router, http.MethodPost, fmt.Sprintf("/v1/split/groups/%d/invite-link", group.ID), token,
		nil, http.StatusOK,
	)
	if firstInvite.Token == "" || firstInvite.URL == "" || firstInvite.DeepLink == "" {
		t.Fatalf("expected invite link response, got %#v", firstInvite)
	}
	if firstInvite.Group.ID != group.ID {
		t.Fatalf("expected invite group %d, got %#v", group.ID, firstInvite.Group)
	}

	secondInvite := performJSONRequest[splitGroupInviteResponse](
		t, router, http.MethodPost, fmt.Sprintf("/v1/split/groups/%d/invite-link", group.ID), token,
		nil, http.StatusOK,
	)
	if secondInvite.Token != firstInvite.Token {
		t.Fatalf("expected active invite to be reused, first=%q second=%q", firstInvite.Token, secondInvite.Token)
	}
}

func TestSplitGroupInviteCanBeViewedAndAccepted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)

	router := smokeRouter(t)
	owner, ownerToken := createPaidBillingTestUserSession(t)
	guest, guestToken := createPaidBillingTestUserSession(t)
	email := "guest@example.com"
	if err := database.DB.Model(&guest).Update("email", email).Error; err != nil {
		t.Fatalf("failed to set guest email: %v", err)
	}

	group := performJSONRequest[models.SplitGroup](
		t, router, http.MethodPost, "/v1/split/groups", ownerToken,
		map[string]any{"name": "Bhaukaal", "friend_ids": []uint{}}, http.StatusCreated,
	)
	directInvite := performJSONRequest[splitGroupDirectInviteResponse](
		t, router, http.MethodPost, fmt.Sprintf("/v1/split/groups/%d/invites", group.ID), ownerToken,
		map[string]any{"email": email}, http.StatusCreated,
	)
	if directInvite.ID == 0 || !directInvite.MatchedUser || !directInvite.NotificationSent || directInvite.URL == "" || directInvite.Message == "" {
		t.Fatalf("expected matched direct invite response, got %#v", directInvite)
	}
	duplicateDirectInvite := performJSONRequest[splitGroupDirectInviteResponse](
		t, router, http.MethodPost, fmt.Sprintf("/v1/split/groups/%d/invites", group.ID), ownerToken,
		map[string]any{"email": email}, http.StatusCreated,
	)
	if duplicateDirectInvite.ID != directInvite.ID {
		t.Fatalf("expected duplicate direct invite to be reused, first=%d second=%d", directInvite.ID, duplicateDirectInvite.ID)
	}
	pendingInvites := performJSONRequest[[]splitGroupDirectInviteResponse](
		t, router, http.MethodGet, fmt.Sprintf("/v1/split/groups/%d/invites", group.ID), ownerToken,
		nil, http.StatusOK,
	)
	if len(pendingInvites) != 1 || pendingInvites[0].ID != directInvite.ID || !pendingInvites[0].MatchedUser {
		t.Fatalf("expected one pending direct invite, got %#v", pendingInvites)
	}
	if pendingInvites[0].URL == "" || pendingInvites[0].Message == "" {
		t.Fatalf("expected pending invite to include share details, got %#v", pendingInvites[0])
	}
	performJSONRequest[map[string]string](
		t, router, http.MethodGet, fmt.Sprintf("/v1/split/groups/%d/invites", group.ID), guestToken,
		nil, http.StatusNotFound,
	)
	performJSONRequest[map[string]string](
		t, router, http.MethodDelete, fmt.Sprintf("/v1/split/groups/%d/invites/%d", group.ID, directInvite.ID), guestToken,
		nil, http.StatusNotFound,
	)
	performJSONRequest[map[string]string](
		t, router, http.MethodDelete, fmt.Sprintf("/v1/split/groups/%d/invites/%d", group.ID, directInvite.ID), ownerToken,
		nil, http.StatusOK,
	)
	pendingInvitesAfterRevoke := performJSONRequest[[]splitGroupDirectInviteResponse](
		t, router, http.MethodGet, fmt.Sprintf("/v1/split/groups/%d/invites", group.ID), ownerToken,
		nil, http.StatusOK,
	)
	if len(pendingInvitesAfterRevoke) != 0 {
		t.Fatalf("expected revoked direct invite to leave pending list, got %#v", pendingInvitesAfterRevoke)
	}
	var receivedNotification models.Notification
	if err := database.DB.Where("user_id = ? AND type = ?", guest.ID, "split.group_invite.received").
		First(&receivedNotification).Error; err != nil {
		t.Fatalf("expected guest invite notification: %v", err)
	}

	invite := performJSONRequest[splitGroupInviteResponse](
		t, router, http.MethodPost, fmt.Sprintf("/v1/split/groups/%d/invite-link", group.ID), ownerToken,
		nil, http.StatusOK,
	)
	if invite.URL != directInvite.URL {
		t.Fatalf("expected direct invite to use active group invite link, direct=%q link=%q", directInvite.URL, invite.URL)
	}

	details := performJSONRequest[splitGroupInviteDetailsResponse](
		t, router, http.MethodGet, fmt.Sprintf("/v1/split/invites/%s", invite.Token), guestToken,
		nil, http.StatusOK,
	)
	if details.Group.ID != group.ID || details.OwnerName == "" {
		t.Fatalf("unexpected invite details: %#v", details)
	}

	accepted := performJSONRequest[splitGroupInviteAcceptResponse](
		t, router, http.MethodPost, fmt.Sprintf("/v1/split/invites/%s/accept", invite.Token), guestToken,
		nil, http.StatusOK,
	)
	if accepted.Group.ID != group.ID || accepted.Friend.ID == 0 || accepted.Member.ID == 0 {
		t.Fatalf("unexpected invite accept response: %#v", accepted)
	}
	if accepted.Friend.UserID != owner.ID || accepted.Friend.Email != email {
		t.Fatalf("expected owner-scoped friend for invitee, got %#v", accepted.Friend)
	}

	guestGroups := performJSONRequest[[]models.SplitGroup](
		t, router, http.MethodGet, "/v1/split/groups", guestToken,
		nil, http.StatusOK,
	)
	guestCanSeeGroup := false
	for _, guestGroup := range guestGroups {
		if guestGroup.ID == group.ID {
			if guestGroup.ViewerRole != "member" || guestGroup.ViewerCanManage || !guestGroup.ViewerCanAddExpense {
				t.Fatalf("expected guest group member permissions, got %#v", guestGroup)
			}
			guestCanSeeGroup = true
			break
		}
	}
	if !guestCanSeeGroup {
		t.Fatalf("expected guest to see accepted group, got %#v", guestGroups)
	}

	bill := performJSONRequest[models.SplitBill](
		t, router, http.MethodPost, "/v1/split/bills", ownerToken,
		map[string]any{
			"group_id":     group.ID,
			"title":        "Dinner",
			"total_amount": "1200.00",
			"currency":     "INR",
			"date":         "2026-07-27",
			"participants": []map[string]any{
				{
					"friend_id":    accepted.Friend.ID,
					"share_amount": "600.00",
					"direction":    splitDirectionFriendOwesUser,
				},
			},
		}, http.StatusCreated,
	)
	guestBills := performJSONRequest[[]models.SplitBill](
		t, router, http.MethodGet, "/v1/split/bills", guestToken,
		nil, http.StatusOK,
	)
	guestCanSeeBill := false
	for _, guestBill := range guestBills {
		if guestBill.ID == bill.ID && guestBill.GroupID != nil && *guestBill.GroupID == group.ID {
			if guestBill.ViewerCanEdit || guestBill.ViewerCanDelete {
				t.Fatalf("expected guest not to edit owner-created bill, got %#v", guestBill)
			}
			guestCanSeeBill = true
			break
		}
	}
	if !guestCanSeeBill {
		t.Fatalf("expected guest to see shared group bill, got %#v", guestBills)
	}

	guestBill := performJSONRequest[models.SplitBill](
		t, router, http.MethodPost, "/v1/split/bills", guestToken,
		map[string]any{
			"group_id":     group.ID,
			"title":        "Snacks",
			"total_amount": "400.00",
			"currency":     "INR",
			"date":         "2026-07-28",
			"participants": []map[string]any{
				{
					"friend_id":    accepted.Friend.ID,
					"share_amount": "200.00",
					"direction":    splitDirectionFriendOwesUser,
				},
			},
		}, http.StatusCreated,
	)
	if guestBill.UserID != guest.ID || guestBill.GroupID == nil || *guestBill.GroupID != group.ID {
		t.Fatalf("expected guest-created bill in shared group, got %#v", guestBill)
	}
	if !guestBill.ViewerCanEdit || !guestBill.ViewerCanDelete {
		t.Fatalf("expected guest to manage own bill, got %#v", guestBill)
	}

	ownerBills := performJSONRequest[[]models.SplitBill](
		t, router, http.MethodGet, "/v1/split/bills", ownerToken,
		nil, http.StatusOK,
	)
	ownerCanSeeGuestBill := false
	for _, ownerBill := range ownerBills {
		if ownerBill.ID == guestBill.ID && ownerBill.GroupID != nil && *ownerBill.GroupID == group.ID {
			if ownerBill.ViewerCanEdit || ownerBill.ViewerCanDelete {
				t.Fatalf("expected owner not to edit guest-created bill, got %#v", ownerBill)
			}
			ownerCanSeeGuestBill = true
			break
		}
	}
	if !ownerCanSeeGuestBill {
		t.Fatalf("expected owner to see guest-created group bill, got %#v", ownerBills)
	}

	performJSONRequest[map[string]string](
		t, router, http.MethodPost, fmt.Sprintf("/v1/split/groups/%d/leave", group.ID), guestToken,
		nil, http.StatusOK,
	)

	guestGroupsAfterLeave := performJSONRequest[[]models.SplitGroup](
		t, router, http.MethodGet, "/v1/split/groups", guestToken,
		nil, http.StatusOK,
	)
	for _, guestGroup := range guestGroupsAfterLeave {
		if guestGroup.ID == group.ID {
			t.Fatalf("expected guest group to disappear after leaving, got %#v", guestGroupsAfterLeave)
		}
	}

	guestBillsAfterLeave := performJSONRequest[[]models.SplitBill](
		t, router, http.MethodGet, "/v1/split/bills", guestToken,
		nil, http.StatusOK,
	)
	for _, guestBillAfterLeave := range guestBillsAfterLeave {
		if guestBillAfterLeave.GroupID != nil && *guestBillAfterLeave.GroupID == group.ID {
			t.Fatalf("expected shared group bills to disappear after leaving, got %#v", guestBillsAfterLeave)
		}
	}

	ownerGroupsAfterLeave := performJSONRequest[[]models.SplitGroup](
		t, router, http.MethodGet, "/v1/split/groups", ownerToken,
		nil, http.StatusOK,
	)
	ownerStillHasGroup := false
	for _, ownerGroup := range ownerGroupsAfterLeave {
		if ownerGroup.ID == group.ID {
			ownerStillHasGroup = true
			break
		}
	}
	if !ownerStillHasGroup {
		t.Fatalf("expected owner to keep group after guest leaves, got %#v", ownerGroupsAfterLeave)
	}

	performJSONRequest[map[string]string](
		t, router, http.MethodPost, fmt.Sprintf("/v1/split/groups/%d/leave", group.ID), ownerToken,
		nil, http.StatusUnprocessableEntity,
	)

	var notification models.Notification
	if err := database.DB.Where("user_id = ? AND type = ?", owner.ID, "split.group_invite.accepted").
		First(&notification).Error; err != nil {
		t.Fatalf("expected owner notification: %v", err)
	}
}

func TestSplitBillInputRejectsInvalidShares(t *testing.T) {
	amount, _ := models.ParseMoney("100.00")
	share, _ := models.ParseMoney("70.00")
	input := splitBillInput{
		Title:       "Cab",
		TotalAmount: amount,
		Currency:    "INR",
		Date:        "2026-07-12",
		Participants: []splitParticipantInput{
			{FriendID: 1, ShareAmount: share, Direction: splitDirectionFriendOwesUser},
			{FriendID: 2, ShareAmount: share, Direction: splitDirectionFriendOwesUser},
		},
	}

	fields := input.validate()
	if fields["participants"] == "" {
		t.Fatalf("expected shares over total to be rejected, got %v", fields)
	}
}

func TestEntryCreateCanCreateLinkedSplitWithInlineFriend(t *testing.T) {
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
			"amount":     "2000.00",
			"currency":   "INR",
			"source":     "manual",
			"mode":       "UPI",
			"category":   "Food",
			"merchant":   "Cafe",
			"date":       "2026-07-13",
			"time":       "21:00",
			"account_id": accounts[0].ID,
			"split": map[string]any{
				"group_name": "Goa trip",
				"participants": []map[string]any{
					{
						"friend":       map[string]any{"name": "Riya"},
						"share_amount": "1000.00",
					},
				},
			},
		}, http.StatusCreated,
	)
	if entry.ID == 0 {
		t.Fatalf("entry was not created: %#v", entry)
	}

	bills := performJSONRequest[[]models.SplitBill](
		t, router, http.MethodGet, "/v1/split/bills", token, nil, http.StatusOK,
	)
	if len(bills) != 1 || bills[0].EntryID == nil || *bills[0].EntryID != entry.ID {
		t.Fatalf("expected one linked split bill, got %#v", bills)
	}
	if bills[0].GroupID == nil || len(bills[0].Participants) != 1 {
		t.Fatalf("expected linked group and participant, got %#v", bills[0])
	}
	linkedBill := performJSONRequest[models.SplitBill](
		t, router, http.MethodGet, fmt.Sprintf("/v1/split/bills/by-entry/%d", entry.ID), token, nil, http.StatusOK,
	)
	if linkedBill.ID != bills[0].ID || linkedBill.EntryID == nil || *linkedBill.EntryID != entry.ID {
		t.Fatalf("expected entry-scoped split bill, got %#v", linkedBill)
	}

	balances := performJSONRequest[[]splitBalance](
		t, router, http.MethodGet, "/v1/split/balances", token, nil, http.StatusOK,
	)
	if len(balances) != 1 || balances[0].NetBalance.String() != "1000.00" {
		t.Fatalf("expected friend to owe 1000.00, got %#v", balances)
	}

	updatedBill := performJSONRequest[models.SplitBill](
		t, router, http.MethodPut, fmt.Sprintf("/v1/split/bills/%d", bills[0].ID), token,
		map[string]any{
			"entry_id":     entry.ID,
			"group_id":     *bills[0].GroupID,
			"title":        "Trip dinner",
			"total_amount": "2000.00",
			"currency":     "INR",
			"date":         "2026-07-13",
			"participants": []map[string]any{
				{
					"friend_id":    balances[0].Friend.ID,
					"share_amount": "750.00",
					"direction":    splitDirectionFriendOwesUser,
				},
			},
		}, http.StatusOK,
	)
	if len(updatedBill.Participants) != 1 || updatedBill.Participants[0].ShareAmount.String() != "750.00" {
		t.Fatalf("expected updated participant share, got %#v", updatedBill.Participants)
	}

	performJSONRequest[map[string]string](
		t, router, http.MethodDelete, fmt.Sprintf("/v1/split/bills/%d", bills[0].ID), token, nil, http.StatusOK,
	)
	bills = performJSONRequest[[]models.SplitBill](
		t, router, http.MethodGet, "/v1/split/bills", token, nil, http.StatusOK,
	)
	if len(bills) != 0 {
		t.Fatalf("expected linked split bill to be deleted, got %#v", bills)
	}
	missingBill := performJSONRequest[*models.SplitBill](
		t, router, http.MethodGet, fmt.Sprintf("/v1/split/bills/by-entry/%d", entry.ID), token, nil, http.StatusOK,
	)
	if missingBill != nil {
		t.Fatalf("expected no entry-scoped split bill after delete, got %#v", missingBill)
	}
}

func TestEntryUpdateAndDeleteManageLinkedSplitAtomically(t *testing.T) {
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

	entryPayload := map[string]any{
		"title":      "Dinner",
		"type":       "expense",
		"amount":     "1800.00",
		"currency":   "INR",
		"source":     "manual",
		"mode":       "UPI",
		"category":   "Food",
		"merchant":   "Cafe",
		"date":       "2026-07-14",
		"time":       "20:00",
		"account_id": accounts[0].ID,
	}
	entry := performJSONRequest[models.Entry](
		t, router, http.MethodPost, "/v1/entries", token, entryPayload, http.StatusCreated,
	)

	entryPayload["split"] = map[string]any{
		"group_name": "Dinner crew",
		"participants": []map[string]any{
			{
				"friend":       map[string]any{"name": "Kabir"},
				"share_amount": "900.00",
				"direction":    splitDirectionFriendOwesUser,
			},
		},
	}
	performJSONRequest[models.Entry](
		t, router, http.MethodPut, fmt.Sprintf("/v1/entries/%d", entry.ID), token, entryPayload, http.StatusOK,
	)
	bills := performJSONRequest[[]models.SplitBill](
		t, router, http.MethodGet, "/v1/split/bills", token, nil, http.StatusOK,
	)
	if len(bills) != 1 || bills[0].EntryID == nil || *bills[0].EntryID != entry.ID {
		t.Fatalf("expected update to create linked split bill, got %#v", bills)
	}

	entryPayload["split"] = nil
	performJSONRequest[models.Entry](
		t, router, http.MethodPut, fmt.Sprintf("/v1/entries/%d", entry.ID), token, entryPayload, http.StatusOK,
	)
	bills = performJSONRequest[[]models.SplitBill](
		t, router, http.MethodGet, "/v1/split/bills", token, nil, http.StatusOK,
	)
	if len(bills) != 0 {
		t.Fatalf("expected split:null to remove linked split bill, got %#v", bills)
	}

	entryPayload["split"] = map[string]any{
		"participants": []map[string]any{
			{
				"friend":       map[string]any{"name": "Kabir"},
				"share_amount": "600.00",
				"direction":    splitDirectionFriendOwesUser,
			},
		},
	}
	performJSONRequest[models.Entry](
		t, router, http.MethodPut, fmt.Sprintf("/v1/entries/%d", entry.ID), token, entryPayload, http.StatusOK,
	)
	performJSONRequest[map[string]string](
		t, router, http.MethodDelete, fmt.Sprintf("/v1/entries/%d", entry.ID), token, nil, http.StatusOK,
	)
	bills = performJSONRequest[[]models.SplitBill](
		t, router, http.MethodGet, "/v1/split/bills", token, nil, http.StatusOK,
	)
	if len(bills) != 0 {
		t.Fatalf("expected deleting entry to remove linked split bill, got %#v", bills)
	}
}

func TestSplitGroupKindAndDefaultSplitAreSharedWithMembers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)

	router := smokeRouter(t)
	owner, ownerToken := createPaidBillingTestUserSession(t)
	_, memberToken := createPaidBillingTestUserSession(t)

	friend := performJSONRequest[models.SplitFriend](
		t, router, http.MethodPost, "/v1/split/friends", ownerToken,
		map[string]any{"name": "Priya", "email": "priya@example.com"}, http.StatusCreated,
	)

	group := performJSONRequest[models.SplitGroup](
		t, router, http.MethodPost, "/v1/split/groups", ownerToken,
		map[string]any{"name": "Home", "kind": "home", "friend_ids": []uint{friend.ID}},
		http.StatusCreated,
	)
	if group.Kind != "home" {
		t.Fatalf("expected the selected group kind to be stored, got %q", group.Kind)
	}
	if group.DefaultSplit != nil {
		t.Fatalf("expected a new group to carry no default split, got %#v", group.DefaultSplit)
	}

	performJSONRequest[map[string]any](
		t, router, http.MethodPost, "/v1/split/groups", ownerToken,
		map[string]any{"name": "Nowhere", "kind": "vacation", "friend_ids": []uint{}},
		http.StatusUnprocessableEntity,
	)

	friendSlot := fmt.Sprintf("%d", friend.ID)
	performJSONRequest[map[string]any](
		t, router, http.MethodPut, fmt.Sprintf("/v1/split/groups/%d/default-split", group.ID), ownerToken,
		map[string]any{"default_split": map[string]any{
			"payer": "owner",
			"tab":   "percentages",
			"participants": []map[string]any{
				{"slot": "owner", "weight": "60"},
				{"slot": friendSlot, "weight": "30"},
			},
		}},
		http.StatusUnprocessableEntity,
	)

	saved := performJSONRequest[models.SplitGroup](
		t, router, http.MethodPut, fmt.Sprintf("/v1/split/groups/%d/default-split", group.ID), ownerToken,
		map[string]any{"default_split": map[string]any{
			"payer": "owner",
			"tab":   "percentages",
			"participants": []map[string]any{
				{"slot": "owner", "weight": "60"},
				{"slot": friendSlot, "weight": "40"},
			},
		}},
		http.StatusOK,
	)
	if saved.DefaultSplit == nil || saved.DefaultSplit.Tab != "percentages" ||
		len(saved.DefaultSplit.Participants) != 2 {
		t.Fatalf("unexpected saved default split: %#v", saved.DefaultSplit)
	}

	// The invitee joins, which is where the owner's friend row becomes linked to
	// their account — the link a member needs to find their own slot.
	invite := performJSONRequest[splitGroupInviteResponse](
		t, router, http.MethodPost, fmt.Sprintf("/v1/split/groups/%d/invite-link", group.ID), ownerToken,
		nil, http.StatusOK,
	)
	accepted := performJSONRequest[splitGroupInviteAcceptResponse](
		t, router, http.MethodPost, fmt.Sprintf("/v1/split/invites/%s/accept", invite.Token), memberToken,
		nil, http.StatusOK,
	)
	if accepted.Friend.LinkedUserID == nil {
		t.Fatalf("expected invite acceptance to link the friend row to the joining user")
	}

	memberGroups := performJSONRequest[[]models.SplitGroup](
		t, router, http.MethodGet, "/v1/split/groups", memberToken, nil, http.StatusOK,
	)
	var seen *models.SplitGroup
	for index := range memberGroups {
		if memberGroups[index].ID == group.ID {
			seen = &memberGroups[index]
		}
	}
	if seen == nil {
		t.Fatalf("expected the member to see the shared group")
	}
	if seen.Kind != "home" {
		t.Fatalf("expected the member to see the group kind, got %q", seen.Kind)
	}
	if seen.DefaultSplit == nil || seen.DefaultSplit.Payer != "owner" ||
		seen.DefaultSplit.Tab != "percentages" {
		t.Fatalf("expected the member to see the shared default split, got %#v", seen.DefaultSplit)
	}
	if seen.OwnerName == "" {
		t.Fatalf("expected the member to be told who owns the group")
	}
	if seen.ViewerFriendID == nil || *seen.ViewerFriendID != accepted.Friend.ID {
		t.Fatalf("expected the member to be told which slot is theirs, got %#v", seen.ViewerFriendID)
	}

	// A member can change the default: it describes how the group divides its
	// costs, not a preference of whoever created the group.
	memberSaved := performJSONRequest[models.SplitGroup](
		t, router, http.MethodPut, fmt.Sprintf("/v1/split/groups/%d/default-split", group.ID), memberToken,
		map[string]any{"default_split": map[string]any{
			"payer": friendSlot,
			"tab":   "shares",
			"participants": []map[string]any{
				{"slot": "owner", "weight": "2"},
				{"slot": friendSlot, "weight": "1"},
			},
		}},
		http.StatusOK,
	)
	if memberSaved.DefaultSplit == nil || memberSaved.DefaultSplit.Payer != friendSlot {
		t.Fatalf("expected the member's change to be stored, got %#v", memberSaved.DefaultSplit)
	}

	ownerView := performJSONRequest[[]models.SplitGroup](
		t, router, http.MethodGet, "/v1/split/groups", ownerToken, nil, http.StatusOK,
	)
	if len(ownerView) == 0 || ownerView[0].DefaultSplit == nil ||
		ownerView[0].DefaultSplit.Tab != "shares" {
		t.Fatalf("expected the owner to see the member's change, got %#v", ownerView)
	}
	if ownerView[0].ViewerFriendID != nil {
		t.Fatalf("the owner is never one of their own friend rows, got %#v", ownerView[0].ViewerFriendID)
	}
	if ownerView[0].OwnerName == "" || owner.ID != ownerView[0].UserID {
		t.Fatalf("unexpected owner view: %#v", ownerView[0])
	}

	cleared := performJSONRequest[models.SplitGroup](
		t, router, http.MethodPut, fmt.Sprintf("/v1/split/groups/%d/default-split", group.ID), ownerToken,
		map[string]any{"default_split": nil}, http.StatusOK,
	)
	if cleared.DefaultSplit != nil {
		t.Fatalf("expected the default split to be cleared, got %#v", cleared.DefaultSplit)
	}
}

func TestAddingExistingFriendToGroupInvitesAndPreservesTheirLedger(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)

	router := smokeRouter(t)
	owner, ownerToken := createPaidBillingTestUserSession(t)
	partner, partnerToken := createPaidBillingTestUserSession(t)
	partnerEmail := "partner@example.com"
	if err := database.DB.Model(&partner).Update("email", partnerEmail).Error; err != nil {
		t.Fatalf("failed to set partner email: %v", err)
	}

	// The friend the owner has been splitting against, saved with the address
	// the partner's account uses.
	friend := performJSONRequest[models.SplitFriend](
		t, router, http.MethodPost, "/v1/split/friends", ownerToken,
		map[string]any{"name": "Wife", "email": partnerEmail}, http.StatusCreated,
	)
	// Somebody with no way to be reached at all.
	unreachable := performJSONRequest[models.SplitFriend](
		t, router, http.MethodPost, "/v1/split/friends", ownerToken,
		map[string]any{"name": "Flatmate"}, http.StatusCreated,
	)

	group := performJSONRequest[models.SplitGroup](
		t, router, http.MethodPost, "/v1/split/groups", ownerToken,
		map[string]any{
			"name":       "Couple",
			"kind":       "couple",
			"friend_ids": []uint{friend.ID, unreachable.ID},
		},
		http.StatusCreated,
	)

	statuses := map[uint]string{}
	for _, invited := range group.MemberInvites {
		statuses[invited.FriendID] = invited.Status
	}
	if statuses[friend.ID] != models.SplitMemberInviteNotified {
		t.Fatalf("expected the added friend to be notified, got %q", statuses[friend.ID])
	}
	if statuses[unreachable.ID] != models.SplitMemberInviteNoContact {
		t.Fatalf("expected a friend with no contact details to be reported as unreachable, got %q",
			statuses[unreachable.ID])
	}

	var notification models.Notification
	if err := database.DB.Where("user_id = ? AND type = ?", partner.ID, "split.group_invite.received").
		First(&notification).Error; err != nil {
		t.Fatalf("expected the added member to be notified: %v", err)
	}

	// Adding somebody is not the same as letting them in.
	beforeAccept := performJSONRequest[[]models.SplitGroup](
		t, router, http.MethodGet, "/v1/split/groups", partnerToken, nil, http.StatusOK,
	)
	for _, visible := range beforeAccept {
		if visible.ID == group.ID {
			t.Fatalf("the group must stay invisible until the invite is accepted")
		}
	}

	// The owner keeps splitting against her in the meantime.
	performJSONRequest[models.SplitBill](
		t, router, http.MethodPost, "/v1/split/bills", ownerToken,
		map[string]any{
			"title":        "Groceries",
			"total_amount": "1000.00",
			"currency":     "INR",
			"date":         "2026-08-01",
			"group_id":     group.ID,
			"participants": []map[string]any{
				{
					"friend_id":    friend.ID,
					"share_amount": "400.00",
					"direction":    splitDirectionFriendOwesUser,
				},
			},
		},
		http.StatusCreated,
	)

	invite := performJSONRequest[splitGroupInviteResponse](
		t, router, http.MethodPost, fmt.Sprintf("/v1/split/groups/%d/invite-link", group.ID), ownerToken,
		nil, http.StatusOK,
	)
	accepted := performJSONRequest[splitGroupInviteAcceptResponse](
		t, router, http.MethodPost, fmt.Sprintf("/v1/split/invites/%s/accept", invite.Token), partnerToken,
		nil, http.StatusOK,
	)
	if accepted.Friend.ID != friend.ID {
		t.Fatalf("acceptance must reuse the friend row already carrying her expenses, got %d want %d",
			accepted.Friend.ID, friend.ID)
	}

	var friendRows int64
	if err := database.DB.Model(&models.SplitFriend{}).
		Where("user_id = ? AND email = ?", owner.ID, partnerEmail).
		Count(&friendRows).Error; err != nil {
		t.Fatalf("failed to count friend rows: %v", err)
	}
	if friendRows != 1 {
		t.Fatalf("expected acceptance to leave one friend row, found %d", friendRows)
	}

	// The invite has been used, so it should not still be pending.
	pending := performJSONRequest[[]splitGroupDirectInviteResponse](
		t, router, http.MethodGet, fmt.Sprintf("/v1/split/groups/%d/invites", group.ID), ownerToken,
		nil, http.StatusOK,
	)
	if len(pending) != 0 {
		t.Fatalf("expected no pending invites after acceptance, got %#v", pending)
	}

	afterAccept := performJSONRequest[[]models.SplitGroup](
		t, router, http.MethodGet, "/v1/split/groups", partnerToken, nil, http.StatusOK,
	)
	seen := false
	for _, visible := range afterAccept {
		if visible.ID == group.ID {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("expected the group to appear once the invite is accepted")
	}

	partnerBills := performJSONRequest[[]models.SplitBill](
		t, router, http.MethodGet, "/v1/split/bills", partnerToken, nil, http.StatusOK,
	)
	if len(partnerBills) != 1 || partnerBills[0].Title != "Groceries" {
		t.Fatalf("expected the expense recorded before acceptance to be visible, got %#v", partnerBills)
	}

	// Saving the same roster again must not re-notify anyone.
	if err := database.DB.Where("user_id = ?", partner.ID).Delete(&models.Notification{}).Error; err != nil {
		t.Fatalf("failed to clear notifications: %v", err)
	}
	resaved := performJSONRequest[models.SplitGroup](
		t, router, http.MethodPut, fmt.Sprintf("/v1/split/groups/%d", group.ID), ownerToken,
		map[string]any{
			"name":       "Couple",
			"kind":       "couple",
			"friend_ids": []uint{friend.ID, unreachable.ID},
		},
		http.StatusOK,
	)
	if len(resaved.MemberInvites) != 0 {
		t.Fatalf("expected no invites for an unchanged roster, got %#v", resaved.MemberInvites)
	}
	var repeatNotifications int64
	if err := database.DB.Model(&models.Notification{}).
		Where("user_id = ?", partner.ID).
		Count(&repeatNotifications).Error; err != nil {
		t.Fatalf("failed to count notifications: %v", err)
	}
	if repeatNotifications != 0 {
		t.Fatalf("expected no repeat notification, got %d", repeatNotifications)
	}
}

func TestAddingFriendWithoutAnAccountRaisesAnInviteToShare(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)

	router := smokeRouter(t)
	_, ownerToken := createPaidBillingTestUserSession(t)

	friend := performJSONRequest[models.SplitFriend](
		t, router, http.MethodPost, "/v1/split/friends", ownerToken,
		map[string]any{"name": "Arjun", "email": "arjun@example.com"}, http.StatusCreated,
	)
	group := performJSONRequest[models.SplitGroup](
		t, router, http.MethodPost, "/v1/split/groups", ownerToken,
		map[string]any{"name": "Goa", "kind": "trip", "friend_ids": []uint{friend.ID}},
		http.StatusCreated,
	)

	if len(group.MemberInvites) != 1 ||
		group.MemberInvites[0].Status != models.SplitMemberInviteLinkNeeded {
		t.Fatalf("expected an invite the owner has to share, got %#v", group.MemberInvites)
	}

	pending := performJSONRequest[[]splitGroupDirectInviteResponse](
		t, router, http.MethodGet, fmt.Sprintf("/v1/split/groups/%d/invites", group.ID), ownerToken,
		nil, http.StatusOK,
	)
	if len(pending) != 1 || pending[0].URL == "" {
		t.Fatalf("expected one shareable pending invite, got %#v", pending)
	}
}

func TestPendingInvitesSurfaceUntilAcceptedThenReadTheNotification(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)

	router := smokeRouter(t)
	_, ownerToken := createPaidBillingTestUserSession(t)
	partner, partnerToken := createPaidBillingTestUserSession(t)
	partnerEmail := "join-me@example.com"
	if err := database.DB.Model(&partner).Update("email", partnerEmail).Error; err != nil {
		t.Fatalf("failed to set partner email: %v", err)
	}

	friend := performJSONRequest[models.SplitFriend](
		t, router, http.MethodPost, "/v1/split/friends", ownerToken,
		map[string]any{"name": "Partner", "email": partnerEmail}, http.StatusCreated,
	)
	group := performJSONRequest[models.SplitGroup](
		t, router, http.MethodPost, "/v1/split/groups", ownerToken,
		map[string]any{"name": "Couple", "kind": "couple", "friend_ids": []uint{friend.ID}},
		http.StatusCreated,
	)

	pending := performJSONRequest[[]splitPendingInviteResponse](
		t, router, http.MethodGet, "/v1/split/pending-invites", partnerToken, nil, http.StatusOK,
	)
	if len(pending) != 1 {
		t.Fatalf("expected one invite waiting on the invitee, got %#v", pending)
	}
	if pending[0].GroupName != "Couple" || pending[0].OwnerName == "" || pending[0].Token == "" {
		t.Fatalf("an invite prompt needs the group, who sent it, and a token: %#v", pending[0])
	}

	// Nobody else is being asked to join.
	ownerPending := performJSONRequest[[]splitPendingInviteResponse](
		t, router, http.MethodGet, "/v1/split/pending-invites", ownerToken, nil, http.StatusOK,
	)
	if len(ownerPending) != 0 {
		t.Fatalf("the owner should not be invited to their own group, got %#v", ownerPending)
	}

	// "Check later" changes nothing on the server: the notification stays
	// unread and the invite keeps being offered.
	var unread int64
	if err := database.DB.Model(&models.Notification{}).
		Where("user_id = ? AND read_at IS NULL", partner.ID).
		Count(&unread).Error; err != nil {
		t.Fatalf("failed to count unread notifications: %v", err)
	}
	if unread != 1 {
		t.Fatalf("expected the invite notification to be waiting unread, got %d", unread)
	}

	performJSONRequest[splitGroupInviteAcceptResponse](
		t, router, http.MethodPost, fmt.Sprintf("/v1/split/invites/%s/accept", pending[0].Token),
		partnerToken, nil, http.StatusOK,
	)

	afterAccept := performJSONRequest[[]splitPendingInviteResponse](
		t, router, http.MethodGet, "/v1/split/pending-invites", partnerToken, nil, http.StatusOK,
	)
	if len(afterAccept) != 0 {
		t.Fatalf("an accepted invite must stop being offered, got %#v", afterAccept)
	}

	var stillUnread int64
	if err := database.DB.Model(&models.Notification{}).
		Where("user_id = ? AND type = ? AND read_at IS NULL", partner.ID, "split.group_invite.received").
		Count(&stillUnread).Error; err != nil {
		t.Fatalf("failed to count unread notifications: %v", err)
	}
	if stillUnread != 0 {
		t.Fatalf("accepting should mark the invite notification read, %d still unread", stillUnread)
	}

	joined := performJSONRequest[[]models.SplitGroup](
		t, router, http.MethodGet, "/v1/split/groups", partnerToken, nil, http.StatusOK,
	)
	seen := false
	for _, visible := range joined {
		if visible.ID == group.ID {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("accepting from the prompt should grant access to the group")
	}
}

func TestInviteeOnNoPaidPlanCanSeeAndAcceptAnInvite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)

	router := smokeRouter(t)
	_, ownerToken := createPaidBillingTestUserSession(t)
	// Deliberately not a paid session: being invited is not using the feature,
	// and an invitation must never arrive as a bill.
	invitee, inviteeToken := createBillingTestUserSession(t)
	inviteeEmail := "unpaid-invitee@example.com"
	if err := database.DB.Model(&invitee).Update("email", inviteeEmail).Error; err != nil {
		t.Fatalf("failed to set invitee email: %v", err)
	}

	friend := performJSONRequest[models.SplitFriend](
		t, router, http.MethodPost, "/v1/split/friends", ownerToken,
		map[string]any{"name": "Invitee", "email": inviteeEmail}, http.StatusCreated,
	)
	group := performJSONRequest[models.SplitGroup](
		t, router, http.MethodPost, "/v1/split/groups", ownerToken,
		map[string]any{"name": "Couple", "kind": "couple", "friend_ids": []uint{friend.ID}},
		http.StatusCreated,
	)

	pending := performJSONRequest[[]splitPendingInviteResponse](
		t, router, http.MethodGet, "/v1/split/pending-invites", inviteeToken, nil, http.StatusOK,
	)
	if len(pending) != 1 {
		t.Fatalf("an unpaid invitee must still be shown the invite, got %#v", pending)
	}

	details := performJSONRequest[splitGroupInviteDetailsResponse](
		t, router, http.MethodGet, fmt.Sprintf("/v1/split/invites/%s", pending[0].Token),
		inviteeToken, nil, http.StatusOK,
	)
	if details.Group.ID != group.ID {
		t.Fatalf("an unpaid invitee must be able to read who invited them: %#v", details)
	}

	accepted := performJSONRequest[splitGroupInviteAcceptResponse](
		t, router, http.MethodPost, fmt.Sprintf("/v1/split/invites/%s/accept", pending[0].Token),
		inviteeToken, nil, http.StatusOK,
	)
	if accepted.Group.ID != group.ID {
		t.Fatalf("an unpaid invitee must be able to accept: %#v", accepted)
	}
}
