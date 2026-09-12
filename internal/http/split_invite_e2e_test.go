package http

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"finnri/internal/database"
	"finnri/internal/models"
)

// The whole invitation, from the owner typing a name to the invitee reading the
// group correctly on her own phone. Every step here has a test of its own; this
// one is about the seams between them, which is where "it works" usually stops
// being true.
func TestSplitInviteWorksAllTheWay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)

	_, ownerToken := createPaidBillingTestUserSession(t)
	invitee, inviteeToken := createPaidBillingTestUserSession(t)

	phone := "+91 98200 44556"
	invitee.Phone = &phone
	if err := database.DB.Save(&invitee).Error; err != nil {
		t.Fatalf("save invitee phone: %v", err)
	}

	// 1. The owner adds somebody they have been splitting with, by phone.
	friend := performJSONRequest[models.SplitFriend](
		t, router, http.MethodPost, "/v1/split/friends", ownerToken,
		map[string]any{"name": "Biwi", "phone": phone}, http.StatusCreated,
	)
	group := performJSONRequest[models.SplitGroup](
		t, router, http.MethodPost, "/v1/split/groups", ownerToken,
		map[string]any{"name": "Noida Home", "kind": "home", "friend_ids": []uint{friend.ID}},
		http.StatusCreated,
	)

	// 2. Adding them raises an invite, and says which kind. She has an account,
	//    so it is waiting in her notifications rather than sitting with him.
	if len(group.MemberInvites) != 1 {
		t.Fatalf("adding one person should raise one invite, got %#v", group.MemberInvites)
	}
	if group.MemberInvites[0].Status != models.SplitMemberInviteNotified {
		t.Fatalf("an invitee with an account is notified, got %q", group.MemberInvites[0].Status)
	}

	// 3. The owner can see it pending, and share the link himself if he wants.
	pendingForOwner := performJSONRequest[[]splitGroupDirectInviteResponse](
		t, router, http.MethodGet, fmt.Sprintf("/v1/split/groups/%d/invites", group.ID),
		ownerToken, nil, http.StatusOK,
	)
	if len(pendingForOwner) != 1 || pendingForOwner[0].URL == "" {
		t.Fatalf("the owner should hold one shareable invite, got %#v", pendingForOwner)
	}

	// 4. It reaches her, on open, with the group named.
	waiting := performJSONRequest[[]splitPendingInviteResponse](
		t, router, http.MethodGet, "/v1/split/pending-invites", inviteeToken, nil, http.StatusOK,
	)
	if len(waiting) != 1 || waiting[0].GroupName != "Noida Home" {
		t.Fatalf("the invitee should be offered exactly this group, got %#v", waiting)
	}
	token := waiting[0].Token

	// 5. The public preview — the web page a stranger opens — resolves without
	//    a session at all.
	preview := performJSONRequest[splitGroupInvitePreviewResponse](
		t, router, http.MethodGet, fmt.Sprintf("/v1/split/invites/%s/preview", token),
		"", nil, http.StatusOK,
	)
	if preview.GroupName != "Noida Home" || preview.MemberCount != 2 {
		t.Fatalf("preview should name the group and its size, got %#v", preview)
	}

	// 6. She accepts, and lands on the row the owner was already splitting
	//    against rather than a fresh one with a zero balance.
	accepted := performJSONRequest[splitGroupInviteAcceptResponse](
		t, router, http.MethodPost, fmt.Sprintf("/v1/split/invites/%s/accept", token),
		inviteeToken, nil, http.StatusOK,
	)
	if accepted.Friend.ID != friend.ID {
		t.Fatalf("acceptance should reuse the owner's row %d, got %d", friend.ID, accepted.Friend.ID)
	}

	// 7. The invite stops being offered, to either of them.
	if again := performJSONRequest[[]splitPendingInviteResponse](
		t, router, http.MethodGet, "/v1/split/pending-invites", inviteeToken, nil, http.StatusOK,
	); len(again) != 0 {
		t.Fatalf("an accepted invite should stop being offered, got %#v", again)
	}
	if stillPending := performJSONRequest[[]splitGroupDirectInviteResponse](
		t, router, http.MethodGet, fmt.Sprintf("/v1/split/groups/%d/invites", group.ID),
		ownerToken, nil, http.StatusOK,
	); len(stillPending) != 0 {
		t.Fatalf("the owner should no longer see a pending invite, got %#v", stillPending)
	}

	// 8. The group is now hers to read, with both people in it and the owner
	//    named by a row she owns.
	groups := performJSONRequest[[]models.SplitGroup](
		t, router, http.MethodGet, "/v1/split/groups", inviteeToken, nil, http.StatusOK,
	)
	if len(groups) != 1 || groups[0].ID != group.ID {
		t.Fatalf("the invitee should now see the group, got %#v", groups)
	}
	joined := groups[0]
	if joined.ViewerRole != "member" || joined.ViewerCanAddExpense != true {
		t.Fatalf("she joined as a member who may add expenses, got %#v", joined)
	}
	if len(joined.ViewerMembers) != 2 {
		t.Fatalf("the roster should name both people, got %#v", joined.ViewerMembers)
	}
	ownerRow := joined.ViewerMembers[0]
	if ownerRow.IsViewer || ownerRow.FriendID == 0 {
		t.Fatalf("the owner should be somebody she can name and settle with: %#v", ownerRow)
	}

	// 9. And an expense he enters reads correctly on her phone — the thing the
	//    invitation is for.
	performJSONRequest[models.SplitBill](
		t, router, http.MethodPost, "/v1/split/bills", ownerToken,
		map[string]any{
			"title": "AC on rent", "total_amount": "10000.00", "currency": "INR",
			"date": "2026-09-12", "group_id": group.ID,
			"participants": []map[string]any{
				{"friend_id": friend.ID, "share_amount": "4000.00", "direction": splitDirectionFriendOwesUser},
			},
		}, http.StatusCreated,
	)

	herBills := performJSONRequest[[]models.SplitBill](
		t, router, http.MethodGet, "/v1/split/bills", inviteeToken, nil, http.StatusOK,
	)
	if len(herBills) != 1 {
		t.Fatalf("she should see his expense, got %#v", herBills)
	}
	her := billViewerShare(t, herBills[0], func(s models.SplitBillViewerShare) bool { return s.IsViewer })
	if her.Paid.String() != "0.00" || her.Share.String() != "4000.00" {
		t.Fatalf("she paid nothing and carries 4000, got %#v", her)
	}

	// 10. Both ledgers agree, in opposite directions.
	herSide := balanceFor(t, router, inviteeToken, ownerRow.FriendID)
	if herSide.NetBalance.String() != "-4000.00" {
		t.Fatalf("she owes 4000, got %q", herSide.NetBalance.String())
	}
	hisSide := balanceFor(t, router, ownerToken, friend.ID)
	if hisSide.NetBalance.String() != "4000.00" {
		t.Fatalf("he is owed 4000, got %q", hisSide.NetBalance.String())
	}
}
