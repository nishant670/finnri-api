package http

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"finnri/internal/models"
)

type splitActivityPage struct {
	Items []splitActivityItem `json:"items"`
	Total int                 `json:"total"`
}

// The report: two people share two groups and settle up about once a month,
// and there was nowhere to see those payments between the two of them — when
// each happened, who recorded it, and whether the other side agreed.
func TestFriendSettlementHistoryHasBothSidesAndEveryOutcome(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	ownerToken, memberToken, ownerFriendForMember, group := joinedSplitGroup(t)
	memberFriendForOwner := memberRowForOwner(t, router, memberToken, group.ID)

	// She pays him and records it; he confirms.
	fromMember := performJSONRequest[models.SplitSettlement](
		t, router, http.MethodPost, "/v1/split/settlements", memberToken,
		map[string]any{
			"friend_id":    memberFriendForOwner,
			"group_id":     group.ID,
			"amount":       "1200.00",
			"direction":    settlementDirectionUserPaidFriend,
			"date":         "2026-09-05",
			"payment_mode": "UPI",
		}, http.StatusCreated,
	)
	performJSONRequest[models.SplitSettlement](t, router, http.MethodPost,
		fmt.Sprintf("/v1/split/settlements/%d/confirm", fromMember.ID), ownerToken, nil, http.StatusOK)

	// He records one she says never happened.
	fromOwner := performJSONRequest[models.SplitSettlement](
		t, router, http.MethodPost, "/v1/split/settlements", ownerToken,
		map[string]any{
			"friend_id": ownerFriendForMember.ID,
			"group_id":  group.ID,
			"amount":    "800.00",
			"direction": settlementDirectionFriendPaidUser,
			"date":      "2026-10-07",
		}, http.StatusCreated,
	)
	performJSONRequest[models.SplitSettlement](t, router, http.MethodPost,
		fmt.Sprintf("/v1/split/settlements/%d/deny", fromOwner.ID), memberToken, nil, http.StatusOK)

	// A payment with somebody else is not part of this history.
	neighbour := performJSONRequest[models.SplitFriend](
		t, router, http.MethodPost, "/v1/split/friends", ownerToken,
		map[string]any{"name": "Neighbour"}, http.StatusCreated,
	)
	performJSONRequest[models.SplitSettlement](
		t, router, http.MethodPost, "/v1/split/settlements", ownerToken,
		map[string]any{
			"friend_id": neighbour.ID,
			"amount":    "50.00",
			"direction": settlementDirectionFriendPaidUser,
			"date":      "2026-10-08",
		}, http.StatusCreated,
	)

	history := performJSONRequest[splitActivityPage](t, router, http.MethodGet,
		fmt.Sprintf("/v1/split/activity?type=settlement&friend_id=%d", ownerFriendForMember.ID),
		ownerToken, nil, http.StatusOK)

	if history.Total != 2 || len(history.Items) != 2 {
		t.Fatalf("expected the two settlements with this friend and nothing else, got %#v", history.Items)
	}

	denied := history.Items[0]
	if denied.RecordID != fromOwner.ID || denied.Status != models.SplitSettlementDenied {
		t.Fatalf("newest first, and a denied payment stays in the history: %#v", denied)
	}
	if denied.ActorName != "" || denied.RespondedAt == nil || denied.GroupName != group.Name {
		t.Fatalf("the viewer's own settlement should name its group and when it was answered: %#v", denied)
	}
	if denied.GroupID == nil || *denied.GroupID != group.ID {
		t.Fatalf("the viewer's own settlement should carry its group id: %#v", denied.GroupID)
	}

	confirmed := history.Items[1]
	if confirmed.RecordID != fromMember.ID || confirmed.Status != models.SplitSettlementConfirmed {
		t.Fatalf("the friend's settlement should be here, confirmed: %#v", confirmed)
	}
	if confirmed.Direction != settlementDirectionFriendPaidUser || confirmed.ActorName == "" {
		t.Fatalf("her payment reads as hers, from his side, recorded by her: %#v", confirmed)
	}
	if confirmed.GroupName != group.Name || confirmed.RespondedAt == nil || confirmed.PaymentMode != "upi" {
		t.Fatalf("group, answer time and payment mode should come through: %#v", confirmed)
	}

	// The same history from her side.
	mine := performJSONRequest[splitActivityPage](t, router, http.MethodGet,
		fmt.Sprintf("/v1/split/activity?type=settlement&friend_id=%d", memberFriendForOwner),
		memberToken, nil, http.StatusOK)
	if mine.Total != 2 {
		t.Fatalf("her history with him should hold the same two payments, got %#v", mine.Items)
	}

	// The unfiltered feed is unchanged by the new parameters.
	all := performJSONRequest[splitActivityPage](t, router, http.MethodGet,
		"/v1/split/activity?page_size=100", ownerToken, nil, http.StatusOK)
	if all.Total <= history.Total {
		t.Fatalf("the full feed should still hold bills, groups and friends too: %d items", all.Total)
	}
}

func TestSplitActivityRejectsABadFriendFilter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, token := createBillingTestUserSession(t)

	for _, raw := range []string{"abc", "0", "-3"} {
		performJSONRequest[map[string]any](t, router, http.MethodGet,
			"/v1/split/activity?friend_id="+raw, token, nil, http.StatusUnprocessableEntity)
	}
}
