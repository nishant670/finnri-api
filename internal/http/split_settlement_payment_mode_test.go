package http

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"finnri/internal/database"
	"finnri/internal/models"
)

func latestNotificationBody(t *testing.T, userID uint, notificationType string) string {
	t.Helper()
	var notification models.Notification
	if err := database.DB.Where("user_id = ? AND type = ?", userID, notificationType).
		Order("id desc").First(&notification).Error; err != nil {
		t.Fatalf("no %s notification for user %d: %v", notificationType, userID, err)
	}
	return notification.Body
}

// The report: someone settled up in a shared group, and the person asked to
// confirm it could see who and how much but not how it was paid — so there
// was nothing to check it against.
func TestSettlementTellsTheOtherSideHowItWasPaid(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)

	ownerToken, memberToken, ownerFriendForMember, group := joinedSplitGroup(t)
	var member models.SplitGroupUserMember
	if err := database.DB.Where("group_id = ?", group.ID).
		Where("user_id <> ?", group.UserID).First(&member).Error; err != nil {
		t.Fatalf("find the joined member: %v", err)
	}

	settlement := performJSONRequest[models.SplitSettlement](
		t, router, http.MethodPost, "/v1/split/settlements", ownerToken,
		map[string]any{
			"friend_id":    ownerFriendForMember.ID,
			"group_id":     group.ID,
			"amount":       "500.00",
			"direction":    settlementDirectionUserPaidFriend,
			"date":         "2026-10-07",
			"payment_mode": "UPI",
			"notes":        "Groceries",
		}, http.StatusCreated,
	)
	if settlement.PaymentMode != "upi" {
		t.Fatalf("payment mode should be stored normalised, got %q", settlement.PaymentMode)
	}

	// Where the receiver is asked to decide.
	pending := pendingSettlementsFor(t, router, memberToken)
	if len(pending) != 1 || pending[0].PaymentMode != "upi" {
		t.Fatalf("the decision prompt must carry how it was paid: %#v", pending)
	}

	// What reaches their notification bell.
	body := latestNotificationBody(t, member.UserID, "split.settlement.recorded")
	if !strings.Contains(body, "says they paid you ₹500.00 by UPI.") {
		t.Fatalf("the notification should say how it was paid, got %q", body)
	}

	// And the shared group's feed, read from their side.
	feed := performJSONRequest[struct {
		Items []splitActivityItem `json:"items"`
	}](t, router, http.MethodGet, "/v1/split/activity", memberToken, nil, http.StatusOK)
	var found *splitActivityItem
	for i := range feed.Items {
		if feed.Items[i].Type == "settlement" && feed.Items[i].RecordID == settlement.ID {
			found = &feed.Items[i]
		}
	}
	if found == nil {
		t.Fatalf("the member's feed should show the settlement: %#v", feed.Items)
	}
	if found.PaymentMode != "upi" || found.Notes != "Groceries" {
		t.Fatalf("the feed row must carry how it was paid: %#v", found)
	}
}

func TestSettlementRejectsAnUnknownPaymentMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	ownerToken, _, ownerFriendForMember, group := joinedSplitGroup(t)

	response := performJSONRequest[map[string]any](
		t, router, http.MethodPost, "/v1/split/settlements", ownerToken,
		map[string]any{
			"friend_id":    ownerFriendForMember.ID,
			"group_id":     group.ID,
			"amount":       "500.00",
			"direction":    settlementDirectionUserPaidFriend,
			"date":         "2026-10-07",
			"payment_mode": "barter",
		}, http.StatusUnprocessableEntity,
	)
	if fields, _ := response["fields"].(map[string]any); fields["payment_mode"] == nil {
		t.Fatalf("expected a payment_mode field error, got %#v", response)
	}
}

// What every app build before this one sends. It must keep working, and the
// notification must not invent a way of paying.
func TestSettlementWithoutAPaymentModeStillRecords(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	ownerToken, _, ownerFriendForMember, group := joinedSplitGroup(t)
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
			"amount":    "500.00",
			"direction": settlementDirectionUserPaidFriend,
			"date":      "2026-10-07",
		}, http.StatusCreated,
	)
	if settlement.PaymentMode != "" {
		t.Fatalf("expected no payment mode, got %q", settlement.PaymentMode)
	}
	body := latestNotificationBody(t, member.UserID, "split.settlement.recorded")
	if !strings.Contains(body, "says they paid you ₹500.00.") {
		t.Fatalf("unexpected notification wording: %q", body)
	}
}
