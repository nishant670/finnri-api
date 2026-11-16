package http

import (
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"finnri/internal/database"
	"finnri/internal/models"
)

// "Autopay transaction added" kept its Confirm / Fix it buttons after Confirm
// (2026-11-11): answering it must resolve the notification, not only read it.
func TestAnsweringAnAutopayResolvesItsNotification(t *testing.T) {
	for _, action := range []string{"confirm", "revert"} {
		t.Run(action, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			useSmokeDatabase(t)
			if err := database.DB.AutoMigrate(&models.Subscription{}, &models.SubscriptionOccurrence{}, &models.PushDevice{}); err != nil {
				t.Fatal(err)
			}
			user := createBudgetTestUser(t)
			account := models.Account{UserID: user.ID, Type: "bank", Name: "SBI Account", Color: "#123456"}
			if err := database.DB.Create(&account).Error; err != nil {
				t.Fatal(err)
			}
			subscription := models.Subscription{
				UserID: user.ID, AccountID: &account.ID, Name: "Google play", Amount: testMoney("299"), Currency: "INR",
				BillingInterval: "monthly", NextDueDate: "2026-11-11", Status: "active", ReminderDays: 1,
				AutoPay: true, PaymentMode: "Bank Account", TransactionTag: "Subscription", PurposeType: "normal_spend",
			}
			if err := database.DB.Create(&subscription).Error; err != nil {
				t.Fatal(err)
			}
			created, err := syncSubscriptionAutomation(user.ID, mustDate(t, "2026-11-11"))
			if err != nil || len(created) != 1 {
				t.Fatalf("created = %d, err = %v", len(created), err)
			}
			occurrence := created[0]
			var notification models.Notification
			if err := database.DB.Where("user_id = ? AND action_url = ?", user.ID,
				fmt.Sprintf("/subscription-occurrences/%d", occurrence.ID)).First(&notification).Error; err != nil {
				t.Fatalf("autopay notification not created: %v", err)
			}
			if notification.Amount == nil || notification.Amount.String() != "299.00" ||
				notification.DueDate == nil || *notification.DueDate != "2026-11-11" {
				t.Fatalf("autopay notification amount=%v due=%v", notification.Amount, notification.DueDate)
			}

			server := &Server{}
			response := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(response)
			context.Request = httptest.NewRequest("POST", "/", nil)
			context.Set("userID", user.ID)
			context.Params = gin.Params{{Key: "id", Value: fmt.Sprint(occurrence.ID)}}
			if action == "confirm" {
				server.confirmSubscriptionOccurrence(context)
			} else {
				server.revertSubscriptionOccurrence(context)
			}
			if response.Code != 200 {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}

			if err := database.DB.First(&notification, notification.ID).Error; err != nil {
				t.Fatal(err)
			}
			if notification.ResolvedAt == nil || notification.ReadAt == nil {
				t.Fatalf("notification after %s: read_at=%v resolved_at=%v", action, notification.ReadAt, notification.ResolvedAt)
			}
		})
	}
}
