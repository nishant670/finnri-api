package http

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

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

// expoStub stands in for Expo and hands each batch of messages to the test.
func expoStub(t *testing.T) <-chan []map[string]any {
	t.Helper()
	received := make(chan []map[string]any, 8)
	expo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var messages []map[string]any
		_ = json.NewDecoder(r.Body).Decode(&messages)
		received <- messages
		_, _ = w.Write([]byte(`{"data":[{"status":"ok"}]}`))
	}))
	previous := expoPushURL
	expoPushURL = expo.URL
	t.Cleanup(func() {
		expoPushURL = previous
		expo.Close()
	})
	return received
}

func awaitPush(t *testing.T, received <-chan []map[string]any) map[string]any {
	t.Helper()
	select {
	case messages := <-received:
		if len(messages) != 1 {
			t.Fatalf("expected one message, got %d", len(messages))
		}
		return messages[0]
	case <-time.After(2 * time.Second):
		t.Fatal("no push reached Expo")
		return nil
	}
}

func registerTestPushDevice(t *testing.T, userID uint) {
	t.Helper()
	device := models.PushDevice{UserID: userID, Token: fmt.Sprintf("ExponentPushToken[user-%d]", userID), Platform: "android", Active: true}
	if err := database.DB.Create(&device).Error; err != nil {
		t.Fatal(err)
	}
}

func TestCreateNotificationPushesWithItsTypeAndCategory(t *testing.T) {
	useSmokeDatabase(t)
	user, _ := createBillingTestUserSession(t)
	registerTestPushDevice(t, user.ID)
	received := expoStub(t)

	if err := createNotification(user.ID, "split.group_invite.received", "Join Goa on Finnri", "Asha invited you to a split group.", "/invite/split/abc"); err != nil {
		t.Fatal(err)
	}

	message := awaitPush(t, received)
	if message["channelId"] != pushChannelSplits || message["title"] != "Join Goa on Finnri" {
		t.Fatalf("unexpected message: %#v", message)
	}
	data, _ := message["data"].(map[string]any)
	if data["type"] != "split.group_invite.received" || data["action_url"] != "/invite/split/abc" {
		t.Fatalf("a tap needs the type and destination: %#v", data)
	}
}

func TestAccountSetupNudgeStaysInApp(t *testing.T) {
	useSmokeDatabase(t)
	user, _ := createBillingTestUserSession(t)
	registerTestPushDevice(t, user.ID)
	received := expoStub(t)

	if err := createNotification(user.ID, "account.needs_setup", "Finish setting up HDFC", "Add the missing details.", "/accounts/1"); err != nil {
		t.Fatal(err)
	}

	var rows int64
	database.DB.Model(&models.Notification{}).Where("user_id = ?", user.ID).Count(&rows)
	if rows != 1 {
		t.Fatalf("the in-app row should still be written, got %d", rows)
	}
	select {
	case messages := <-received:
		t.Fatalf("this nudge should not push: %#v", messages)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestBudgetAlertPushesOnceWhenFirstCrossed(t *testing.T) {
	useSmokeDatabase(t)
	user, _ := createBillingTestUserSession(t)
	registerTestPushDevice(t, user.ID)
	received := expoStub(t)
	budget := models.Budget{UserID: user.ID, Category: "Fuel", LimitAmount: models.Money(100000)}
	budget.ID = 41

	for range 2 {
		if err := createBudgetAlertIfNeeded(database.DB, budget, "2026-11-01", budgetExceededKind, models.Money(120000)); err != nil {
			t.Fatal(err)
		}
	}

	message := awaitPush(t, received)
	data, _ := message["data"].(map[string]any)
	if data["type"] != "budget."+budgetExceededKind || message["channelId"] != pushChannelUpdates {
		t.Fatalf("unexpected message: %#v", message)
	}
	select {
	case <-received:
		t.Fatal("the same alert should push once")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestPushChannelFor(t *testing.T) {
	cases := map[string]string{
		"split.settlement.recorded":         pushChannelSplits,
		subscriptionAutopayNotificationType: pushChannelAutopay,
		"budget.exceeded":                   pushChannelUpdates,
		monthlyReviewNotificationType:       pushChannelUpdates,
		giftPassNotificationType:            pushChannelUpdates,
		"subscription.due_soon":             pushChannelReminders,
		"card_statement.due":                pushChannelReminders,
		annualFeeReminderType:               pushChannelReminders,
		refundReminderType:                  pushChannelReminders,
		"":                                  pushChannelReminders,
	}
	for notificationType, want := range cases {
		if got := pushChannelFor(map[string]any{"type": notificationType}); got != want {
			t.Errorf("%q: got %s, want %s", notificationType, got, want)
		}
	}
}

func TestPushReceiptsSwitchOffOnlyGonePhones(t *testing.T) {
	useSmokeDatabase(t)
	user, _ := createBillingTestUserSession(t)
	ok := models.PushDevice{UserID: user.ID, Token: "ExponentPushToken[ok]", Platform: "android", Active: true}
	gone := models.PushDevice{UserID: user.ID, Token: "ExponentPushToken[gone]", Platform: "android", Active: true}
	badKey := models.PushDevice{UserID: user.ID, Token: "ExponentPushToken[bad-key]", Platform: "android", Active: true}
	for _, device := range []*models.PushDevice{&ok, &gone, &badKey} {
		if err := database.DB.Create(device).Error; err != nil {
			t.Fatal(err)
		}
	}

	var asked map[string][]string
	expo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&asked)
		_, _ = w.Write([]byte(`{"data":{
			"t-ok":{"status":"ok"},
			"t-gone":{"status":"error","message":"not registered","details":{"error":"DeviceNotRegistered"}},
			"t-key":{"status":"error","message":"could not authenticate with FCM","details":{"error":"InvalidCredentials"}}
		}}`))
	}))
	defer expo.Close()
	previous := expoPushReceiptsURL
	expoPushReceiptsURL = expo.URL
	defer func() { expoPushReceiptsURL = previous }()

	checkPushReceipts(database.DB, user.ID, map[string]models.PushDevice{"t-ok": ok, "t-gone": gone, "t-key": badKey})

	if len(asked["ids"]) != 3 {
		t.Fatalf("expected all three tickets to be asked about: %#v", asked)
	}
	database.DB.First(&ok, ok.ID)
	database.DB.First(&gone, gone.ID)
	database.DB.First(&badKey, badKey.ID)
	if !ok.Active || gone.Active || !badKey.Active {
		t.Fatalf("only the gone phone should be switched off: ok=%v gone=%v badKey=%v", ok.Active, gone.Active, badKey.Active)
	}
}
