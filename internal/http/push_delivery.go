package http

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"finnri/internal/models"

	"gorm.io/gorm"
)

// Android notification categories. Each is one row in the phone's settings, so
// someone tired of budget alerts can turn those off and keep their settlement
// requests. The app creates them in lib/push-notifications.ts. An id the phone
// doesn't know falls back to Android's "Miscellaneous", so deploying this
// before the app is safe.
const (
	pushChannelAutopay   = "autopay"
	pushChannelReminders = "reminders"
	pushChannelSplits    = "splits"
	pushChannelUpdates   = "updates"
)

// pushChannelFor picks the category from the notification type a push carries.
func pushChannelFor(data map[string]any) string {
	notificationType, _ := data["type"].(string)
	switch {
	case strings.HasPrefix(notificationType, "split."):
		return pushChannelSplits
	case notificationType == subscriptionAutopayNotificationType:
		return pushChannelAutopay
	case strings.HasPrefix(notificationType, "budget."),
		notificationType == monthlyReviewNotificationType,
		notificationType == giftPassNotificationType:
		return pushChannelUpdates
	default:
		return pushChannelReminders
	}
}

// notificationStaysInApp lists the types not worth interrupting a locked phone
// for. A nudge to finish an account's details can wait until the app is open.
func notificationStaysInApp(notificationType string) bool {
	return notificationType == "account.needs_setup"
}

// expoPushReceiptsURL is where Expo reports what happened after it accepted a
// message. A variable so tests can point it at a local server.
var expoPushReceiptsURL = "https://exp.host/--/api/v2/push/getReceipts"

// expoPushReceiptDelay is how long Expo asks senders to wait before reading
// receipts.
const expoPushReceiptDelay = 15 * time.Minute

type expoPushReceipts struct {
	Data map[string]struct {
		Status  string `json:"status"`
		Message string `json:"message"`
		Details struct {
			Error string `json:"error"`
		} `json:"details"`
	} `json:"data"`
}

// schedulePushReceiptCheck reads the outcome of a send once Expo has it.
//
// The ticket from the send only says Expo accepted the message. Whether Firebase
// took it is known only here: a missing or wrong FCM key and a phone that
// uninstalled the app both look like success until the receipt is read. A
// deploy inside the 15-minute wait drops the check, which costs one log line
// and leaves a stale token for the next send to find.
func schedulePushReceiptCheck(db *gorm.DB, userID uint, pending map[string]models.PushDevice) {
	if len(pending) == 0 {
		return
	}
	time.AfterFunc(expoPushReceiptDelay, func() {
		checkPushReceipts(db, userID, pending)
	})
}

// checkPushReceipts logs each failed delivery and switches off tokens Expo says
// are gone. pending maps Expo's ticket id to the phone it was sent to.
func checkPushReceipts(db *gorm.DB, userID uint, pending map[string]models.PushDevice) {
	ids := make([]string, 0, len(pending))
	for id := range pending {
		ids = append(ids, id)
	}
	payload, _ := json.Marshal(map[string]any{"ids": ids})
	req, err := http.NewRequest(http.MethodPost, expoPushReceiptsURL, bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		log.Printf("[WARN] push receipts failed user=%d: %v", userID, err)
		return
	}
	defer response.Body.Close()
	if response.StatusCode >= http.StatusMultipleChoices {
		log.Printf("[WARN] push receipts refused user=%d status=%d", userID, response.StatusCode)
		return
	}

	var receipts expoPushReceipts
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&receipts); err != nil {
		log.Printf("[WARN] push receipts unreadable user=%d: %v", userID, err)
		return
	}
	delivered := 0
	for id, receipt := range receipts.Data {
		device, known := pending[id]
		if !known {
			continue
		}
		if receipt.Status == "ok" {
			delivered++
			continue
		}
		log.Printf("[WARN] push undelivered user=%d device=%d platform=%s: %s %s",
			userID, device.ID, device.Platform, receipt.Details.Error, receipt.Message)
		deactivateGonePushDevice(db, device, receipt.Details.Error)
	}
	log.Printf("[INFO] push receipts user=%d delivered=%d of %d", userID, delivered, len(pending))
}

// deactivateGonePushDevice stops sending to a phone Expo no longer knows.
func deactivateGonePushDevice(db *gorm.DB, device models.PushDevice, expoError string) {
	if expoError != "DeviceNotRegistered" {
		return
	}
	_ = db.Model(&models.PushDevice{}).Where("id = ?", device.ID).Update("active", false).Error
}
