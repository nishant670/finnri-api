package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"finnri/internal/database"
	"finnri/internal/models"
)

func TestSendUserPushSwitchesOffTokensExpoNoLongerKnows(t *testing.T) {
	useSmokeDatabase(t)
	user, _ := createBillingTestUserSession(t)
	other, _ := createBillingTestUserSession(t)

	live := models.PushDevice{UserID: user.ID, Token: "ExponentPushToken[live]", Platform: "android", Active: true}
	gone := models.PushDevice{UserID: user.ID, Token: "ExponentPushToken[gone]", Platform: "android", Active: true}
	if err := database.DB.Create(&live).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.DB.Create(&gone).Error; err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	var sent []map[string]any
	expo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewDecoder(r.Body).Decode(&sent)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"status":"ok","id":"ticket-1"},
			{"status":"error","message":"\"ExponentPushToken[gone]\" is not a registered push notification recipient","details":{"error":"DeviceNotRegistered"}}
		]}`))
	}))
	defer expo.Close()
	previous := expoPushURL
	expoPushURL = expo.URL
	defer func() { expoPushURL = previous }()

	sendUserPush(database.DB, user.ID, "You've been gifted a free pass", "A free monthly pass is yours.", map[string]any{"action_url": "/billing"})

	if calls.Load() != 1 || len(sent) != 2 {
		t.Fatalf("expected one send carrying both devices, got %d calls and %d messages", calls.Load(), len(sent))
	}
	if sent[0]["priority"] != "high" || sent[0]["to"] != live.Token {
		t.Fatalf("unexpected message: %#v", sent[0])
	}
	if data, _ := sent[0]["data"].(map[string]any); data["action_url"] != "/billing" {
		t.Fatalf("the push should carry its destination: %#v", sent[0]["data"])
	}

	database.DB.First(&live, live.ID)
	database.DB.First(&gone, gone.ID)
	if !live.Active || gone.Active {
		t.Fatalf("only the unregistered token should be switched off: live=%v gone=%v", live.Active, gone.Active)
	}

	// Someone with no registered phone sends nothing.
	sendUserPush(database.DB, other.ID, "title", "body", nil)
	if calls.Load() != 1 {
		t.Fatalf("a user with no device should not reach Expo, calls=%d", calls.Load())
	}
}
