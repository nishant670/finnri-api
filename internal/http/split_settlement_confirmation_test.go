package http

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"finnri/internal/database"
	"finnri/internal/models"
)

// pendingSettlementsFor reads the decisions waiting on one user.
func pendingSettlementsFor(t *testing.T, router *gin.Engine, token string) []models.SplitSettlement {
	t.Helper()
	payload := performJSONRequest[struct {
		Settlements []models.SplitSettlement `json:"settlements"`
	}](t, router, http.MethodGet, "/v1/split/settlements/pending", token, nil, http.StatusOK)
	return payload.Settlements
}

func notificationTypesFor(t *testing.T, userID uint) []string {
	t.Helper()
	var notifications []models.Notification
	if err := database.DB.Where("user_id = ?", userID).Order("id").Find(&notifications).Error; err != nil {
		t.Fatalf("read notifications: %v", err)
	}
	types := make([]string, 0, len(notifications))
	for _, notification := range notifications {
		types = append(types, notification.Type)
	}
	return types
}

func hasNotificationType(types []string, want string) bool {
	for _, value := range types {
		if value == want {
			return true
		}
	}
	return false
}

// The reported hole: a settlement was one person's word, applied to both
// ledgers. The friend it named was never asked and never told — the only trace
// was a line in an activity feed that records everything else too.
func TestSettlementAgainstAFinnriFriendWaitsForTheirDecision(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)

	ownerToken, memberToken, ownerFriendForMember, group := joinedSplitGroup(t)
	memberFriendForOwner := memberRowForOwner(t, router, memberToken, group.ID)

	var member models.SplitGroupUserMember
	if err := database.DB.Where("group_id = ?", group.ID).
		Where("user_id <> ?", group.UserID).First(&member).Error; err != nil {
		t.Fatalf("find the joined member: %v", err)
	}

	settlement := performJSONRequest[models.SplitSettlement](
		t, router, http.MethodPost, "/v1/split/settlements", ownerToken,
		map[string]any{
			"friend_id": ownerFriendForMember.ID,
			"group_id":  group.ID,
			"amount":    "600.00",
			"direction": settlementDirectionFriendPaidUser,
			"date":      "2026-09-12",
		}, http.StatusCreated,
	)
	if settlement.Status != models.SplitSettlementPending {
		t.Fatalf("expected a settlement against a Finnri friend to be pending, got %q", settlement.Status)
	}
	if settlement.CounterpartyUserID == nil || *settlement.CounterpartyUserID != member.UserID {
		t.Fatalf("expected the joined member to be the counterparty, got %#v", settlement.CounterpartyUserID)
	}

	// It reaches them where they will actually see it — the global bell — and
	// not only in a feed.
	if types := notificationTypesFor(t, member.UserID); !hasNotificationType(types, "split.settlement.recorded") {
		t.Fatalf("expected the member to be notified of the settlement, got %#v", types)
	}

	pending := pendingSettlementsFor(t, router, memberToken)
	if len(pending) != 1 || pending[0].ID != settlement.ID {
		t.Fatalf("expected one decision waiting on the member, got %#v", pending)
	}
	if pending[0].RecordedByName == "" {
		t.Fatal("a decision prompt that cannot name who recorded the payment is unanswerable")
	}
	if pending[0].GroupName != group.Name {
		t.Fatalf("expected the prompt to name the group %q, got %q", group.Name, pending[0].GroupName)
	}

	// Nobody else can answer for them — not even the person who recorded it.
	performJSONRequest[map[string]any](t, router, http.MethodPost,
		fmt.Sprintf("/v1/split/settlements/%d/confirm", settlement.ID), ownerToken, nil, http.StatusNotFound)

	confirmed := performJSONRequest[models.SplitSettlement](
		t, router, http.MethodPost,
		fmt.Sprintf("/v1/split/settlements/%d/confirm", settlement.ID), memberToken, nil, http.StatusOK,
	)
	if confirmed.Status != models.SplitSettlementConfirmed || confirmed.RespondedAt == nil {
		t.Fatalf("expected a confirmed, timestamped settlement, got %#v", confirmed)
	}
	if types := notificationTypesFor(t, group.UserID); !hasNotificationType(types, "split.settlement.confirmed") {
		t.Fatalf("expected the recorder to hear that it was confirmed, got %#v", types)
	}
	// And it is answered: asking twice cannot produce a second notification.
	performJSONRequest[map[string]any](t, router, http.MethodPost,
		fmt.Sprintf("/v1/split/settlements/%d/deny", settlement.ID), memberToken, nil, http.StatusConflict)
	if left := pendingSettlementsFor(t, router, memberToken); len(left) != 0 {
		t.Fatalf("expected nothing left to decide, got %#v", left)
	}

	_ = memberFriendForOwner
}

// A denial has to put the money back. Leaving the row counting would mean the
// app agreed with a payment the person it names says never happened.
func TestDeniedSettlementIsRemovedFromBothBalances(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)

	ownerToken, memberToken, ownerFriendForMember, group := joinedSplitGroup(t)
	memberFriendForOwner := memberRowForOwner(t, router, memberToken, group.ID)

	// The owner lays out 1000 and puts 600 of it on the member.
	performJSONRequest[models.SplitBill](
		t, router, http.MethodPost, "/v1/split/bills", ownerToken,
		map[string]any{
			"title": "Dinner", "total_amount": "1000.00", "currency": "INR",
			"date": "2026-09-10", "group_id": group.ID,
			"participants": []map[string]any{
				{
					"friend_id":    ownerFriendForMember.ID,
					"share_amount": "600.00",
					"direction":    splitDirectionFriendOwesUser,
				},
			},
		}, http.StatusCreated,
	)
	before := balanceFor(t, router, ownerToken, ownerFriendForMember.ID).NetBalance

	settlement := performJSONRequest[models.SplitSettlement](
		t, router, http.MethodPost, "/v1/split/settlements", ownerToken,
		map[string]any{
			"friend_id": ownerFriendForMember.ID,
			"group_id":  group.ID,
			"amount":    "600.00",
			"direction": settlementDirectionFriendPaidUser,
			"date":      "2026-09-12",
		}, http.StatusCreated,
	)

	// Pending still counts: the payment is being asserted to have happened, and
	// a ledger that waited for a tap would disagree with the money.
	// Money is minor units, so six hundred rupees is 60000.
	if settled := balanceFor(t, router, ownerToken, ownerFriendForMember.ID).NetBalance; settled != before-60000 {
		t.Fatalf("expected a pending settlement to close the balance, got %v from %v", settled, before)
	}

	performJSONRequest[models.SplitSettlement](
		t, router, http.MethodPost,
		fmt.Sprintf("/v1/split/settlements/%d/deny", settlement.ID), memberToken, nil, http.StatusOK,
	)

	if after := balanceFor(t, router, ownerToken, ownerFriendForMember.ID).NetBalance; after != before {
		t.Fatalf("expected the denial to restore the balance to %v, got %v", before, after)
	}
	// And from the member's side of the same ledger, which reads the owner's
	// settlement rows restated rather than its own.
	if memberSide := balanceFor(t, router, memberToken, memberFriendForOwner).NetBalance; memberSide != -before {
		t.Fatalf("expected the member to owe %v again after denying, got %v", -before, memberSide)
	}
	if types := notificationTypesFor(t, group.UserID); !hasNotificationType(types, "split.settlement.denied") {
		t.Fatalf("expected the recorder to hear that it was denied, got %#v", types)
	}
}

// Nobody to ask is not the same as nobody answered. A friend row standing for
// somebody without a Finnri account would otherwise sit pending forever.
func TestSettlementAgainstAnOfflineFriendIsConfirmedOnCreation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)

	_, token := createPaidBillingTestUserSession(t)
	friend := performJSONRequest[models.SplitFriend](
		t, router, http.MethodPost, "/v1/split/friends", token,
		map[string]any{"name": "Neighbour"}, http.StatusCreated,
	)
	settlement := performJSONRequest[models.SplitSettlement](
		t, router, http.MethodPost, "/v1/split/settlements", token,
		map[string]any{
			"friend_id": friend.ID,
			"amount":    "250.00",
			"direction": settlementDirectionUserPaidFriend,
			"date":      "2026-09-12",
		}, http.StatusCreated,
	)
	if settlement.Status != models.SplitSettlementConfirmed {
		t.Fatalf("expected a settlement with nobody to confirm it to be confirmed, got %q", settlement.Status)
	}
	if settlement.CounterpartyUserID != nil {
		t.Fatalf("expected no counterparty, got %#v", settlement.CounterpartyUserID)
	}
}

// The dot on the Splits screen is driven by this count, so it has to answer
// "what is waiting on you in splits" and not "what have you ever done".
func TestUnreadNotificationCountNarrowsToATypePrefix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)

	user, token := createPaidBillingTestUserSession(t)
	if err := createNotification(user.ID, "split.settlement.recorded", "Settled", "Body", ""); err != nil {
		t.Fatalf("create split notification: %v", err)
	}
	if err := createNotification(user.ID, "budget.exceeded", "Budget", "Body", ""); err != nil {
		t.Fatalf("create budget notification: %v", err)
	}

	all := performJSONRequest[struct {
		UnreadCount int `json:"unread_count"`
	}](t, router, http.MethodGet, "/v1/notifications/unread-count", token, nil, http.StatusOK)
	if all.UnreadCount != 2 {
		t.Fatalf("expected both notifications unread, got %d", all.UnreadCount)
	}

	splits := performJSONRequest[struct {
		UnreadCount int `json:"unread_count"`
	}](t, router, http.MethodGet, "/v1/notifications/unread-count?type_prefix=split.", token, nil, http.StatusOK)
	if splits.UnreadCount != 1 {
		t.Fatalf("expected only the split notification to count, got %d", splits.UnreadCount)
	}
}
