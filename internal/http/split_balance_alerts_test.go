package http

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"finnri/internal/database"
	"finnri/internal/models"
)

func balanceAlertNotifications(t *testing.T, userID uint) []models.Notification {
	t.Helper()
	var notifications []models.Notification
	if err := database.DB.Where("user_id = ? AND type = ?", userID, "split.balance_alert").
		Order("id asc").Find(&notifications).Error; err != nil {
		t.Fatalf("load notifications: %v", err)
	}
	return notifications
}

func TestSplitGroupBalanceAlertFiresOncePerCrossing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)

	router := smokeRouter(t)
	owner, ownerToken := createPaidBillingTestUserSession(t)
	member, memberToken := createPaidBillingTestUserSession(t)

	group := performJSONRequest[models.SplitGroup](
		t, router, http.MethodPost, "/v1/split/groups", ownerToken,
		map[string]any{"name": "Goa trip", "kind": "trip", "balance_alert_amount": "1000", "friend_ids": []uint{}},
		http.StatusCreated,
	)
	if group.BalanceAlertAmount != models.Money(1000*100) {
		t.Fatalf("expected the alert amount to be stored, got %v", group.BalanceAlertAmount)
	}
	invite := performJSONRequest[splitGroupInviteResponse](
		t, router, http.MethodPost, fmt.Sprintf("/v1/split/groups/%d/invite-link", group.ID), ownerToken,
		nil, http.StatusOK,
	)
	accepted := performJSONRequest[splitGroupInviteAcceptResponse](
		t, router, http.MethodPost, fmt.Sprintf("/v1/split/invites/%s/accept", invite.Token), memberToken,
		nil, http.StatusOK,
	)
	memberFriendID := accepted.Friend.ID

	addBill := func(amount string) {
		t.Helper()
		performJSONRequest[models.SplitBill](
			t, router, http.MethodPost, "/v1/split/bills", ownerToken,
			map[string]any{
				"group_id": group.ID, "title": "Dinner", "total_amount": amount, "currency": "INR", "date": "2026-11-05",
				"participants": []map[string]any{
					{"friend_id": memberFriendID, "share_amount": amount, "direction": splitDirectionFriendOwesUser},
				},
			}, http.StatusCreated,
		)
	}

	addBill("600.00")
	if got := balanceAlertNotifications(t, owner.ID); len(got) != 0 {
		t.Fatalf("expected no alert under the limit, got %#v", got)
	}

	addBill("500.00") // 1,100: over.
	ownerAlerts := balanceAlertNotifications(t, owner.ID)
	if len(ownerAlerts) != 1 || !strings.Contains(ownerAlerts[0].Body, "owes you ₹1,100 in Goa trip, past the ₹1,000 alert.") ||
		ownerAlerts[0].ActionURL != fmt.Sprintf("/split/groups/%d", group.ID) {
		t.Fatalf("expected one owner alert for ₹1,100, got %#v", ownerAlerts)
	}
	memberAlerts := balanceAlertNotifications(t, member.ID)
	if len(memberAlerts) != 1 || !strings.HasPrefix(memberAlerts[0].Body, "You owe ") {
		t.Fatalf("expected the member to be told what they owe, got %#v", memberAlerts)
	}

	addBill("100.00") // Still over: no news.
	if got := balanceAlertNotifications(t, owner.ID); len(got) != 1 {
		t.Fatalf("expected no second alert while still over, got %d", len(got))
	}

	// A payment brings it under, which re-arms the alert.
	performJSONRequest[models.SplitSettlement](
		t, router, http.MethodPost, "/v1/split/settlements", ownerToken,
		map[string]any{
			"group_id": group.ID, "friend_id": memberFriendID, "amount": "500.00",
			"direction": settlementDirectionFriendPaidUser, "date": "2026-11-05",
		}, http.StatusCreated,
	)
	addBill("400.00") // 700 + 400 = 1,100: over again.
	if got := balanceAlertNotifications(t, owner.ID); len(got) != 2 {
		t.Fatalf("expected a fresh alert after dropping under and crossing again, got %d", len(got))
	}

	// Turning it off forgets who was told.
	performJSONRequest[models.SplitGroup](
		t, router, http.MethodPut, fmt.Sprintf("/v1/split/groups/%d", group.ID), ownerToken,
		map[string]any{"name": "Goa trip", "kind": "trip", "balance_alert_amount": "0", "friend_ids": []uint{memberFriendID}},
		http.StatusOK,
	)
	var remaining int64
	database.DB.Model(&models.SplitGroupBalanceAlert{}).Where("group_id = ?", group.ID).Count(&remaining)
	if remaining != 0 {
		t.Fatalf("expected no alert rows once the alert is off, got %d", remaining)
	}

	// Lowering the limit under a balance that already passes it says so.
	performJSONRequest[models.SplitGroup](
		t, router, http.MethodPut, fmt.Sprintf("/v1/split/groups/%d", group.ID), ownerToken,
		map[string]any{"name": "Goa trip", "kind": "trip", "balance_alert_amount": "200", "friend_ids": []uint{memberFriendID}},
		http.StatusOK,
	)
	if got := balanceAlertNotifications(t, owner.ID); len(got) != 3 {
		t.Fatalf("expected an alert when the limit is set under the balance, got %d", len(got))
	}

	// An edit without the field leaves the limit alone.
	kept := performJSONRequest[models.SplitGroup](
		t, router, http.MethodPut, fmt.Sprintf("/v1/split/groups/%d", group.ID), ownerToken,
		map[string]any{"name": "Goa", "kind": "trip", "friend_ids": []uint{memberFriendID}},
		http.StatusOK,
	)
	if kept.BalanceAlertAmount != models.Money(200*100) {
		t.Fatalf("expected an edit without the field to keep the limit, got %v", kept.BalanceAlertAmount)
	}

	performJSONRequest[map[string]any](
		t, router, http.MethodPost, "/v1/split/groups", ownerToken,
		map[string]any{"name": "Bad", "kind": "trip", "balance_alert_amount": "-5", "friend_ids": []uint{}},
		http.StatusUnprocessableEntity,
	)
}
