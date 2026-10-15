package http

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"finnri/internal/database"
	"finnri/internal/models"
)

// The report: an expense added today for the 13th sat below one added
// yesterday for the 14th, because the feed was ordered on the bill's date.
// Activity is the order things happened in; an edit counts as happening.
func TestSplitActivityIsOrderedByWhenThingsHappened(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, token := createPaidBillingTestUserSession(t)

	friend := performJSONRequest[models.SplitFriend](
		t, router, http.MethodPost, "/v1/split/friends", token,
		map[string]any{"name": "Biwi"}, http.StatusCreated,
	)
	billFor := func(title, date string) map[string]any {
		return map[string]any{
			"title":        title,
			"total_amount": "200.00",
			"currency":     "INR",
			"date":         date,
			"participants": []map[string]any{{
				"friend_id":    friend.ID,
				"share_amount": "100.00",
				"direction":    splitDirectionFriendOwesUser,
			}},
		}
	}
	footwear := performJSONRequest[models.SplitBill](t, router, http.MethodPost,
		"/v1/split/bills", token, billFor("Riya Footwear", "2026-10-14"), http.StatusCreated)
	uber := performJSONRequest[models.SplitBill](t, router, http.MethodPost,
		"/v1/split/bills", token, billFor("Uber ride", "2026-10-13"), http.StatusCreated)
	// Footwear went in yesterday, the older Uber ride an hour ago.
	now := time.Now()
	setBillTimes := func(id uint, at time.Time) {
		t.Helper()
		if err := database.DB.Model(&models.SplitBill{}).Where("id = ?", id).
			UpdateColumns(map[string]any{"created_at": at, "updated_at": at}).Error; err != nil {
			t.Fatal(err)
		}
	}
	setBillTimes(footwear.ID, now.Add(-24*time.Hour))
	setBillTimes(uber.ID, now.Add(-time.Hour))

	billOrder := func(query string) []uint {
		page := performJSONRequest[splitActivityPage](t, router, http.MethodGet,
			"/v1/split/activity?page_size=100"+query, token, nil, http.StatusOK)
		var ids []uint
		for _, item := range page.Items {
			if item.Type == "bill" {
				ids = append(ids, item.RecordID)
				if item.ActivityAt.IsZero() {
					t.Fatalf("%s carries no activity time", item.ID)
				}
			}
		}
		return ids
	}

	if got := billOrder(""); fmt.Sprint(got) != fmt.Sprint([]uint{uber.ID, footwear.ID}) {
		t.Fatalf("the expense added last should lead, whatever its date: got %v", got)
	}
	if got := billOrder("&order=date"); fmt.Sprint(got) != fmt.Sprint([]uint{footwear.ID, uber.ID}) {
		t.Fatalf("order=date should still read by the bill's date: got %v", got)
	}

	// Editing yesterday's expense is the newest thing that happened.
	edit := billFor("Riya Footwear", "2026-10-14")
	edit["total_amount"] = "240.00"
	edit["participants"] = []map[string]any{{
		"friend_id":    friend.ID,
		"share_amount": "120.00",
		"direction":    splitDirectionFriendOwesUser,
	}}
	performJSONRequest[models.SplitBill](t, router, http.MethodPut,
		fmt.Sprintf("/v1/split/bills/%d", footwear.ID), token, edit, http.StatusOK)
	if got := billOrder(""); fmt.Sprint(got) != fmt.Sprint([]uint{footwear.ID, uber.ID}) {
		t.Fatalf("an edited expense should move to the top: got %v", got)
	}
}
