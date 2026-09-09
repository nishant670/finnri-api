package http

import (
	"fmt"
	"net/http"
	"testing"

	"finnri/internal/database"
	"finnri/internal/models"

	"github.com/gin-gonic/gin"
)

func TestReconcileSplitIdentitiesCollapsesInviteDuplicateAfterPhoneArrives(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)

	owner, ownerToken := createPaidBillingTestUserSession(t)
	member, memberToken := createPaidBillingTestUserSession(t)
	original := performJSONRequest[models.SplitFriend](
		t, router, http.MethodPost, "/v1/split/friends", ownerToken,
		map[string]any{"name": "Wife", "phone": "98718 01518"}, http.StatusCreated,
	)
	group := performJSONRequest[models.SplitGroup](
		t, router, http.MethodPost, "/v1/split/groups", ownerToken,
		map[string]any{"name": "Home", "friend_ids": []uint{original.ID}}, http.StatusCreated,
	)
	invite := performJSONRequest[splitGroupInviteResponse](
		t, router, http.MethodPost, fmt.Sprintf("/v1/split/groups/%d/invite-link", group.ID), ownerToken,
		nil, http.StatusOK,
	)
	accepted := performJSONRequest[splitGroupInviteAcceptResponse](
		t, router, http.MethodPost, fmt.Sprintf("/v1/split/invites/%s/accept", invite.Token), memberToken,
		nil, http.StatusOK,
	)
	if accepted.Friend.ID == original.ID {
		t.Fatal("expected the contact-less member to create the historical duplicate")
	}

	phone := "+91 98718 01518"
	member.Phone = &phone
	if err := database.DB.Save(&member).Error; err != nil {
		t.Fatalf("save member phone: %v", err)
	}
	if err := reconcileSplitIdentities(database.DB, member.ID); err != nil {
		t.Fatalf("reconcile split identities: %v", err)
	}

	var survivor, duplicate models.SplitFriend
	if err := database.DB.First(&survivor, original.ID).Error; err != nil {
		t.Fatalf("load original: %v", err)
	}
	if err := database.DB.First(&duplicate, accepted.Friend.ID).Error; err != nil {
		t.Fatalf("load duplicate: %v", err)
	}
	if survivor.LinkedUserID == nil || *survivor.LinkedUserID != member.ID {
		t.Fatalf("expected original row to link to member, got %#v", survivor)
	}
	if !duplicate.Archived || duplicate.LinkedUserID != nil {
		t.Fatalf("expected invite duplicate to be archived and unlinked, got %#v", duplicate)
	}

	var memberships []models.SplitGroupMember
	if err := database.DB.Where("group_id = ?", group.ID).Find(&memberships).Error; err != nil {
		t.Fatalf("load memberships: %v", err)
	}
	if len(memberships) != 1 || memberships[0].FriendID != original.ID {
		t.Fatalf("expected one membership on the original row, got %#v", memberships)
	}
	if survivor.UserID != owner.ID {
		t.Fatalf("expected survivor to remain owner-scoped, got %#v", survivor)
	}
}

func TestMergeSplitFriendMovesMoneyAndMemberships(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	owner, token := createPaidBillingTestUserSession(t)

	survivor := performJSONRequest[models.SplitFriend](
		t, router, http.MethodPost, "/v1/split/friends", token,
		map[string]any{"name": "Riya", "phone": "9871801518"}, http.StatusCreated,
	)
	loser := performJSONRequest[models.SplitFriend](
		t, router, http.MethodPost, "/v1/split/friends", token,
		map[string]any{"name": "Riya Dutta", "email": "riya@example.com"}, http.StatusCreated,
	)
	group := performJSONRequest[models.SplitGroup](
		t, router, http.MethodPost, "/v1/split/groups", token,
		map[string]any{"name": "Trip", "friend_ids": []uint{survivor.ID, loser.ID}}, http.StatusCreated,
	)
	bill := performJSONRequest[models.SplitBill](
		t, router, http.MethodPost, "/v1/split/bills", token,
		map[string]any{
			"group_id": group.ID, "title": "Dinner", "total_amount": "500.00", "currency": "INR", "date": "2026-08-27",
			"participants": []map[string]any{{"friend_id": loser.ID, "share_amount": "250.00", "direction": splitDirectionFriendOwesUser}},
		}, http.StatusCreated,
	)
	settlement := performJSONRequest[models.SplitSettlement](
		t, router, http.MethodPost, "/v1/split/settlements", token,
		map[string]any{"friend_id": loser.ID, "amount": "50.00", "direction": settlementDirectionFriendPaidUser, "date": "2026-08-27"},
		http.StatusCreated,
	)

	performJSONRequest[map[string]any](
		t, router, http.MethodPost,
		fmt.Sprintf("/v1/split/friends/%d/merge-into/%d", loser.ID, survivor.ID), token, nil, http.StatusOK,
	)

	var participant models.SplitParticipant
	if err := database.DB.Where("bill_id = ?", bill.ID).First(&participant).Error; err != nil {
		t.Fatalf("load participant: %v", err)
	}
	if participant.FriendID != survivor.ID {
		t.Fatalf("participant still points at duplicate: %#v", participant)
	}
	var movedSettlement models.SplitSettlement
	if err := database.DB.First(&movedSettlement, settlement.ID).Error; err != nil {
		t.Fatalf("load settlement: %v", err)
	}
	if movedSettlement.FriendID != survivor.ID {
		t.Fatalf("settlement still points at duplicate: %#v", movedSettlement)
	}
	var memberships []models.SplitGroupMember
	if err := database.DB.Where("group_id = ?", group.ID).Find(&memberships).Error; err != nil {
		t.Fatalf("load memberships: %v", err)
	}
	if len(memberships) != 1 || memberships[0].FriendID != survivor.ID {
		t.Fatalf("expected duplicate group membership to collapse, got %#v", memberships)
	}
	var archived models.SplitFriend
	if err := database.DB.First(&archived, loser.ID).Error; err != nil {
		t.Fatalf("load merged friend: %v", err)
	}
	if !archived.Archived {
		t.Fatalf("expected merged friend to be archived: %#v", archived)
	}
	var refreshed models.SplitFriend
	if err := database.DB.First(&refreshed, survivor.ID).Error; err != nil {
		t.Fatalf("load survivor: %v", err)
	}
	if refreshed.Email != "riya@example.com" || refreshed.UserID != owner.ID {
		t.Fatalf("expected missing survivor contact to be preserved, got %#v", refreshed)
	}
}
