package http

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"finnri/internal/models"
)

// The app now reads the feed a page at a time to the end, and the server
// loads each bill's participants and group only for the page it returns. Every
// page must still come back whole, and the pages together must be the feed.
func TestSplitActivityPagesAreCompleteAndEachBillIsFilledIn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	router := smokeRouter(t)
	_, token := createPaidBillingTestUserSession(t)

	friend := performJSONRequest[models.SplitFriend](
		t, router, http.MethodPost, "/v1/split/friends", token,
		map[string]any{"name": "Biwi"}, http.StatusCreated,
	)
	group := performJSONRequest[models.SplitGroup](
		t, router, http.MethodPost, "/v1/split/groups", token,
		map[string]any{"name": "Home", "friend_ids": []uint{friend.ID}}, http.StatusCreated,
	)
	for day := 1; day <= 25; day++ {
		performJSONRequest[models.SplitBill](
			t, router, http.MethodPost, "/v1/split/bills", token,
			map[string]any{
				"title":        fmt.Sprintf("Groceries %d", day),
				"total_amount": "200.00",
				"currency":     "INR",
				"date":         fmt.Sprintf("2026-09-%02d", day),
				"group_id":     group.ID,
				"participants": []map[string]any{{
					"friend_id":    friend.ID,
					"share_amount": "100.00",
					"direction":    splitDirectionFriendOwesUser,
				}},
			}, http.StatusCreated,
		)
	}

	first := performJSONRequest[splitActivityPage](t, router, http.MethodGet,
		"/v1/split/activity?page=1&page_size=5", token, nil, http.StatusOK)
	if first.Total < 27 { // 25 bills, the group and the friend
		t.Fatalf("expected the whole feed counted, got total %d", first.Total)
	}

	seen := map[string]bool{}
	bills := 0
	for page := 1; ; page++ {
		result := performJSONRequest[splitActivityPage](t, router, http.MethodGet,
			fmt.Sprintf("/v1/split/activity?page=%d&page_size=5", page), token, nil, http.StatusOK)
		if len(result.Items) == 0 {
			break
		}
		for _, item := range result.Items {
			if seen[item.ID] {
				t.Fatalf("%s came back on two pages", item.ID)
			}
			seen[item.ID] = true
			if item.Type != "bill" {
				continue
			}
			bills++
			if item.ParticipantCount != 1 || len(item.Participants) != 1 ||
				item.Participants[0].Friend.ID != friend.ID {
				t.Fatalf("page %d: %s is missing its participants: %#v", page, item.ID, item)
			}
			if item.Group == nil || item.Group.Name != "Home" {
				t.Fatalf("page %d: %s is missing its group: %#v", page, item.ID, item.Group)
			}
		}
	}
	if len(seen) != first.Total || bills != 25 {
		t.Fatalf("the pages together should be the feed: %d of %d items, %d of 25 bills",
			len(seen), first.Total, bills)
	}
}
