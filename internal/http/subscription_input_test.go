package http

import (
	"testing"

	"finnri/internal/models"
)

func TestSubscriptionInputValidation(t *testing.T) {
	reminderDays := 31
	accountID := uint(1)
	tests := []struct {
		name  string
		input subscriptionInput
		valid bool
	}{
		{"valid", subscriptionInput{Name: "Streamly", Amount: testMoney("499"), NextDueDate: "2026-07-20"}, true},
		{"daily", subscriptionInput{Name: "Cloud", Amount: testMoney("49"), BillingInterval: "daily", NextDueDate: "2026-07-20", AutoPay: true, AccountID: &accountID}, true},
		{"business daily", subscriptionInput{Name: "Fund SIP", Amount: testMoney("100"), BillingInterval: "business_daily", NextDueDate: "2026-07-20", AutoPay: true, AccountID: &accountID}, true},
		{"daily requires autopay", subscriptionInput{Name: "Cloud", Amount: testMoney("49"), BillingInterval: "daily", NextDueDate: "2026-07-20"}, false},
		{"autopay requires account", subscriptionInput{Name: "Cloud", Amount: testMoney("49"), NextDueDate: "2026-07-20", AutoPay: true}, false},
		{"cancel reminder requires date", subscriptionInput{Name: "Cloud", Amount: testMoney("49"), NextDueDate: "2026-07-20", CancelBeforeDue: true}, false},
		{"weekly", subscriptionInput{Name: "Cloud", Amount: testMoney("199"), BillingInterval: "weekly", NextDueDate: "2026-07-20"}, true},
		{"biweekly", subscriptionInput{Name: "Cloud", Amount: testMoney("199"), BillingInterval: "biweekly", NextDueDate: "2026-07-20"}, true},
		{"quarterly", subscriptionInput{Name: "Cloud", Amount: testMoney("999"), BillingInterval: "quarterly", NextDueDate: "2026-07-20"}, true},
		{"missing name", subscriptionInput{Amount: testMoney("499"), NextDueDate: "2026-07-20"}, false},
		{"bad amount", subscriptionInput{Name: "Streamly", Amount: 0, NextDueDate: "2026-07-20"}, false},
		{"bad interval", subscriptionInput{Name: "Streamly", Amount: testMoney("499"), BillingInterval: "hourly", NextDueDate: "2026-07-20"}, false},
		{"bad status", subscriptionInput{Name: "Streamly", Amount: testMoney("499"), Status: "lost", NextDueDate: "2026-07-20"}, false},
		{"bad due date", subscriptionInput{Name: "Streamly", Amount: testMoney("499"), NextDueDate: "20-07-2026"}, false},
		{"bad reminder", subscriptionInput{Name: "Streamly", Amount: testMoney("499"), NextDueDate: "2026-07-20", ReminderDays: &reminderDays}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := len(test.input.validate()) == 0; got != test.valid {
				t.Fatalf("valid = %v, fields = %v", got, test.input.validate())
			}
		})
	}
}

func TestSubscriptionInputApplyDefaults(t *testing.T) {
	var subscription models.Subscription
	input := subscriptionInput{
		Name: " Streamly ", Merchant: " Streamly India ", Category: " Entertainment ",
		Amount: testMoney("499"), NextDueDate: "2026-07-20",
	}
	input.apply(&subscription)
	if subscription.Name != "Streamly" || subscription.Merchant != "Streamly India" ||
		subscription.Category != "Entertainment" || subscription.Currency != "INR" ||
		subscription.BillingInterval != "monthly" || subscription.Status != "active" ||
		subscription.ReminderDays != 3 || subscription.CancelBeforeDue {
		t.Fatalf("subscription defaults were not applied: %#v", subscription)
	}
}
