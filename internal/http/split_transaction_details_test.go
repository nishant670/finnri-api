package http

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"finnri/internal/database"
	"finnri/internal/models"
	"github.com/gin-gonic/gin"
)

func TestStandaloneSplitDetailsSurviveEditAndDoNotCreatePersonalSpend(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	user, token := createPaidBillingTestUserSession(t)
	stranger, _ := createPaidBillingTestUserSession(t)
	friend := performJSONRequest[models.SplitFriend](t, router, http.MethodPost, "/v1/split/friends", token,
		map[string]any{"name": "Riya"}, http.StatusCreated)
	receiptName := strings.Repeat("a", 32) + ".jpg"
	payload := map[string]any{
		"title": "Dinner", "total_amount": "1000.50", "date": "2026-09-19", "mode": "UPI",
		"category": "Food & Drinks", "merchant": "Cafe", "tag": "Lending", "time": "20:30",
		"attachment":   "https://example.com/uploads/" + receiptName,
		"participants": []map[string]any{{"friend_id": friend.ID, "share_amount": "400.25", "direction": splitDirectionUserOwesFriend}},
	}
	bill := performJSONRequest[models.SplitBill](t, router, http.MethodPost, "/v1/split/bills", token, payload, http.StatusCreated)
	if bill.Mode != "UPI" || bill.Merchant != "Cafe" || bill.Category != "Food & Drinks" || bill.Tag != "Lending" || bill.Time != "20:30" || bill.Attachment == "" {
		t.Fatalf("details were lost: %#v", bill)
	}
	if !userCanReadUpload(user.ID, receiptName) || userCanReadUpload(stranger.ID, receiptName) {
		t.Fatal("split receipt access must follow bill access")
	}
	// An older client edits only the original fields. New details survive omission.
	for _, field := range []string{"mode", "category", "merchant", "tag", "time", "attachment"} {
		delete(payload, field)
	}
	payload["title"] = "Updated dinner"
	updated := performJSONRequest[models.SplitBill](t, router, http.MethodPut, fmt.Sprintf("/v1/split/bills/%d", bill.ID), token, payload, http.StatusOK)
	if updated.Mode != bill.Mode || updated.Merchant != bill.Merchant || updated.Attachment != bill.Attachment || updated.Tag != bill.Tag {
		t.Fatalf("legacy edit cleared details: %#v", updated)
	}
	payload["merchant"], payload["attachment"] = "", ""
	cleared := performJSONRequest[models.SplitBill](t, router, http.MethodPut, fmt.Sprintf("/v1/split/bills/%d", bill.ID), token, payload, http.StatusOK)
	if cleared.Merchant != "" || cleared.Attachment != "" {
		t.Fatal("explicit clear did not clear details")
	}
	var entries int64
	if err := database.DB.Model(&models.Entry{}).Where("user_id = ?", user.ID).Count(&entries).Error; err != nil {
		t.Fatal(err)
	}
	if entries != 0 {
		t.Fatalf("friend-paid bill created %d personal entries", entries)
	}
	balances := performJSONRequest[[]splitBalance](t, router, http.MethodGet, "/v1/split/balances", token, nil, http.StatusOK)
	if len(balances) != 1 || balances[0].NetBalance.String() != "-400.25" {
		t.Fatalf("details changed the debt: %#v", balances)
	}
}

func TestEntryBackedSplitEditKeepsBillIdentityAndSharedDetails(t *testing.T) {
	seed := seedGroupWithOneBill(t)
	bills := performJSONRequest[[]models.SplitBill](t, seed.router, http.MethodGet, "/v1/split/bills", seed.token, nil, http.StatusOK)
	original := bills[0]
	entry := performJSONRequest[models.Entry](t, seed.router, http.MethodPut, fmt.Sprintf("/v1/entries/%d", seed.entryID), seed.token,
		map[string]any{
			"title": "Dinner with receipt", "amount": "9000.00", "merchant": "Restaurant", "mode": "UPI", "tag": "Lending", "time": "21:15",
			"split": map[string]any{"group_id": seed.groupID, "notes": "Shared note", "participants": []map[string]any{
				{"friend_id": original.Participants[0].FriendID, "share_amount": "3100.25", "direction": splitDirectionFriendOwesUser},
			}},
		}, http.StatusOK)
	bills = performJSONRequest[[]models.SplitBill](t, seed.router, http.MethodGet, "/v1/split/bills", seed.token, nil, http.StatusOK)
	if len(bills) != 1 || bills[0].ID != original.ID || bills[0].EntryID == nil || *bills[0].EntryID != seed.entryID {
		t.Fatalf("linked bill was replaced or detached: %#v", bills)
	}
	bill := bills[0]
	if bill.Mode != entry.Mode || bill.Merchant != entry.Merchant || bill.Tag != entry.Tag || bill.Time != entry.Time || bill.TotalAmount != entry.Amount || bill.Participants[0].ShareAmount.String() != "3100.25" {
		t.Fatalf("transaction and split diverged: entry=%#v bill=%#v", entry, bill)
	}
	// The old split endpoint also retains the transaction link when entry_id is omitted.
	updated := performJSONRequest[models.SplitBill](t, seed.router, http.MethodPut, fmt.Sprintf("/v1/split/bills/%d", bill.ID), seed.token,
		map[string]any{"title": "Edited from old client", "total_amount": "9100.00", "date": "2026-09-20", "group_id": seed.groupID,
			"participants": []map[string]any{{"friend_id": original.Participants[0].FriendID, "share_amount": "3100.25", "direction": splitDirectionFriendOwesUser}},
		}, http.StatusOK)
	if updated.EntryID == nil || *updated.EntryID != seed.entryID || updated.Mode != entry.Mode {
		t.Fatalf("legacy split edit detached transaction: %#v", updated)
	}
	entry = performJSONRequest[models.Entry](t, seed.router, http.MethodGet, fmt.Sprintf("/v1/entries/%d", seed.entryID), seed.token, nil, http.StatusOK)
	if entry.Title != updated.Title || entry.Amount != updated.TotalAmount {
		t.Fatal("legacy split edit left a stale personal transaction")
	}
}

func TestSplitDetailsValidateBeforeSaving(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, token := createPaidBillingTestUserSession(t)
	friend := performJSONRequest[models.SplitFriend](t, router, http.MethodPost, "/v1/split/friends", token, map[string]any{"name": "Riya"}, http.StatusCreated)
	for _, invalid := range []map[string]any{{"mode": "not a mode"}, {"time": "25:71"}, {"attachment": "file:///private/receipt.jpg"}} {
		payload := map[string]any{"title": "Dinner", "total_amount": "100", "date": "2026-09-19", "participants": []map[string]any{{"friend_id": friend.ID, "share_amount": "50", "direction": splitDirectionFriendOwesUser}}}
		for key, value := range invalid {
			payload[key] = value
		}
		performJSONRequest[map[string]any](t, router, http.MethodPost, "/v1/split/bills", token, payload, http.StatusUnprocessableEntity)
	}
	var count int64
	database.DB.Model(&models.SplitBill{}).Count(&count)
	if count != 0 {
		t.Fatal("invalid details were persisted")
	}
}

func TestLinkedSplitRefundValidationRollsBackBothRecords(t *testing.T) {
	seed := seedGroupWithOneBill(t)
	entryPath := fmt.Sprintf("/v1/entries/%d", seed.entryID)
	performJSONRequest[models.Entry](t, seed.router, http.MethodPut, entryPath, seed.token,
		map[string]any{"tag": "Refundable", "refundable_amount": "5000.00", "refund_expected_on": "2026-12-01", "refund_status": "pending"}, http.StatusOK)
	bills := performJSONRequest[[]models.SplitBill](t, seed.router, http.MethodGet, "/v1/split/bills", seed.token, nil, http.StatusOK)
	original := bills[0]
	participants := []map[string]any{{"friend_id": original.Participants[0].FriendID, "share_amount": "400.00", "direction": splitDirectionFriendOwesUser}}
	performJSONRequest[map[string]any](t, seed.router, http.MethodPut, fmt.Sprintf("/v1/split/bills/%d", original.ID), seed.token,
		map[string]any{"title": "Invalid reduction", "total_amount": "1000.00", "date": "2026-09-19", "group_id": seed.groupID, "participants": participants}, http.StatusUnprocessableEntity)
	entry := performJSONRequest[models.Entry](t, seed.router, http.MethodGet, entryPath, seed.token, nil, http.StatusOK)
	if entry.Amount.String() != "8000.00" || entry.RefundableAmount.String() != "5000.00" {
		t.Fatal("failed split edit changed the transaction")
	}
	bills = performJSONRequest[[]models.SplitBill](t, seed.router, http.MethodGet, "/v1/split/bills", seed.token, nil, http.StatusOK)
	if bills[0].TotalAmount != original.TotalAmount || bills[0].Participants[0].ShareAmount != original.Participants[0].ShareAmount {
		t.Fatal("failed edit changed the split")
	}
	entry = performJSONRequest[models.Entry](t, seed.router, http.MethodPut, entryPath, seed.token,
		map[string]any{"amount": "1000.00", "refundable_amount": "500.00", "split": map[string]any{"group_id": seed.groupID, "participants": participants}}, http.StatusOK)
	if entry.RefundableAmount.String() != "500.00" || entry.Amount.String() != "1000.00" {
		t.Fatal("shared editor did not update refund and amount")
	}
}
