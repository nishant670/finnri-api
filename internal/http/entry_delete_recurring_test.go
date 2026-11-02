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

// The report: swiping a recurring payment (Zee Entertainment, logged from its
// schedule) and deleting it always said "Delete failed". The occurrence that
// files the payment against its due date still pointed at the entry, and the
// database refused to drop a row something references. Same for deleting the
// schedule itself once it had logged anything.
//
// SQLite ignores foreign keys unless asked, which is how this hid from every
// other test; Postgres never does.
func TestDeletingARecurringPaymentEntry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	if err := database.DB.Exec("PRAGMA foreign_keys = ON").Error; err != nil {
		t.Fatal(err)
	}
	router := smokeRouter(t)
	_, token := createPaidBillingTestUserSession(t)

	today := time.Now().UTC().Format("2006-01-02")
	logged := func(name string) (subscriptionResponse, models.SubscriptionOccurrence) {
		t.Helper()
		created := performJSONRequest[subscriptionResponse](t, router, http.MethodPost,
			"/v1/subscriptions", token, map[string]any{
				"name":             name,
				"merchant":         name,
				"amount":           "299.00",
				"billing_interval": "monthly",
				"next_due_date":    today,
				"payment_mode":     "UPI",
				"log_payment_on":   today,
			}, http.StatusCreated)
		var occurrence models.SubscriptionOccurrence
		if err := database.DB.Where("subscription_id = ?", created.ID).First(&occurrence).Error; err != nil {
			t.Fatalf("expected the payment logged against the schedule: %v", err)
		}
		return created, occurrence
	}

	t.Run("the entry deletes, and takes its occurrence with it", func(t *testing.T) {
		created, occurrence := logged("Zee Entertainment")
		performJSONRequest[map[string]any](t, router, http.MethodDelete,
			fmt.Sprintf("/v1/entries/%d", occurrence.EntryID), token, nil, http.StatusOK)

		var left int64
		database.DB.Model(&models.Entry{}).Where("id = ?", occurrence.EntryID).Count(&left)
		if left != 0 {
			t.Fatal("the entry is still there")
		}
		database.DB.Model(&models.SubscriptionOccurrence{}).Where("id = ?", occurrence.ID).Count(&left)
		if left != 0 {
			t.Fatal("an occurrence without its entry would block logging that due date again")
		}
		var schedule models.Subscription
		if err := database.DB.First(&schedule, created.ID).Error; err != nil {
			t.Fatalf("deleting one payment must leave the schedule: %v", err)
		}
	})

	t.Run("an autopay entry deletes, and its notification goes too", func(t *testing.T) {
		created, _ := logged("Hotstar")
		var schedule models.Subscription
		if err := database.DB.First(&schedule, created.ID).Error; err != nil {
			t.Fatal(err)
		}
		due, _ := parseAPIDate(schedule.NextDueDate)
		occurrence, didCreate, err := createSubscriptionOccurrence(&schedule, due)
		if err != nil || !didCreate || occurrence.NotificationID == nil {
			t.Fatalf("expected autopay to add the payment and notify: %v %#v", err, occurrence)
		}
		performJSONRequest[map[string]any](t, router, http.MethodDelete,
			fmt.Sprintf("/v1/entries/%d", occurrence.EntryID), token, nil, http.StatusOK)

		var left int64
		database.DB.Model(&models.Notification{}).Where("id = ?", *occurrence.NotificationID).Count(&left)
		if left != 0 {
			t.Fatal("its Confirm and Change buttons act on an occurrence that is gone")
		}
	})

	t.Run("the schedule deletes once it has logged a payment", func(t *testing.T) {
		created, occurrence := logged("Netflix")
		performJSONRequest[map[string]any](t, router, http.MethodDelete,
			fmt.Sprintf("/v1/subscriptions/%d", created.ID), token, nil, http.StatusOK)

		var left int64
		database.DB.Model(&models.SubscriptionOccurrence{}).Where("id = ?", occurrence.ID).Count(&left)
		if left != 0 {
			t.Fatal("the schedule's occurrences should go with it")
		}
		database.DB.Model(&models.Entry{}).Where("id = ?", occurrence.EntryID).Count(&left)
		if left != 1 {
			t.Fatal("a payment already made stays in the ledger when the schedule is deleted")
		}
	})
}
