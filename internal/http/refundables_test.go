package http

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"finnri/internal/database"
	"finnri/internal/models"
)

func TestRefundableCanBeListedAndSettled(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)
	user, token := createPaidBillingTestUserSession(t)
	amount := rupees(5000)
	expected := "2026-12-11"
	status := refundStatusPending
	entry := models.Entry{
		UserID: user.ID, Title: "Security deposit", Type: "expense",
		Amount: rupees(10000), Currency: "INR", Source: "manual", Mode: "Cash",
		Category: defaultCategory, Tag: "Refundable", Date: "2026-09-11",
		RefundableAmount: &amount, RefundExpectedOn: &expected, RefundStatus: &status,
	}
	if err := database.DB.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}

	pending := performJSONRequest[[]models.Entry](
		t, router, http.MethodGet, "/v1/refundables", token, nil, http.StatusOK,
	)
	if len(pending) != 1 || pending[0].ID != entry.ID {
		t.Fatalf("pending refundables = %#v", pending)
	}
	updated := performJSONRequest[models.Entry](
		t, router, http.MethodPatch, fmt.Sprintf("/v1/refundables/%d", entry.ID), token,
		map[string]any{"status": refundStatusReceived}, http.StatusOK,
	)
	if updated.RefundStatus == nil || *updated.RefundStatus != refundStatusReceived {
		t.Fatalf("settled refundable has status %#v", updated.RefundStatus)
	}
	pending = performJSONRequest[[]models.Entry](
		t, router, http.MethodGet, "/v1/refundables", token, nil, http.StatusOK,
	)
	if len(pending) != 0 {
		t.Fatalf("received refund still appears pending: %#v", pending)
	}
}

func TestRefundableEntryValidation(t *testing.T) {
	input := validEntryInput()
	input.Amount = rupees(10000)
	input.Tag = "Refundable"
	refundable := rupees(5000)
	expected := "2026-12-11"
	input.RefundableAmount = &refundable
	input.RefundExpectedOn = &expected

	if fields := input.validate(); len(fields) != 0 {
		t.Fatalf("valid refundable fields rejected: %#v", fields)
	}

	tooMuch := input
	over := input.Amount + 1
	tooMuch.RefundableAmount = &over
	if fields := tooMuch.validate(); fields["refundable_amount"] == "" {
		t.Fatalf("refundable amount above total was accepted: %#v", fields)
	}
}

func TestRefundReminderIsCreatedOnce(t *testing.T) {
	useSmokeDatabase(t)
	user := createBudgetTestUser(t)
	amount := rupees(5000)
	expected := "2026-09-11"
	status := refundStatusPending
	reminder := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)
	entry := models.Entry{
		UserID: user.ID, Title: "Security deposit", Type: "expense",
		Amount: rupees(10000), Currency: "INR", Source: "manual", Mode: "Cash",
		Category: defaultCategory, Tag: "Refundable", Date: "2026-06-11",
		RefundableAmount: &amount, RefundExpectedOn: &expected,
		RefundReminderAt: &reminder, RefundStatus: &status,
	}
	if err := database.DB.Create(&entry).Error; err != nil {
		t.Fatal(err)
	}

	for run := 0; run < 2; run++ {
		created, err := syncRefundReminders(reminder.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if created != 1-run {
			t.Fatalf("run %d created %d reminders", run+1, created)
		}
	}
	var count int64
	database.DB.Model(&models.Notification{}).
		Where("user_id = ? AND type = ?", user.ID, refundReminderType).Count(&count)
	if count != 1 {
		t.Fatalf("notifications = %d, want 1", count)
	}
}
