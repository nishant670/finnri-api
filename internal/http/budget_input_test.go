package http

import (
	"testing"

	"finnri/internal/models"
)

func TestBudgetInputValidationDefaultsAndRejectsInvalidFields(t *testing.T) {
	active := false
	tests := []struct {
		name  string
		input budgetInput
		valid bool
	}{
		{"valid minimal monthly budget", budgetInput{Name: "Food", LimitAmount: testMoney("1000")}, true},
		{"valid inactive budget", budgetInput{Name: "Pause", LimitAmount: testMoney("1000"), Active: &active}, true},
		{"missing name", budgetInput{LimitAmount: testMoney("1000")}, false},
		{"non-positive limit", budgetInput{Name: "Food", LimitAmount: 0}, false},
		{"non monthly period", budgetInput{Name: "Food", Period: "weekly", LimitAmount: testMoney("1000")}, false},
		{"non INR currency", budgetInput{Name: "Food", LimitAmount: testMoney("1000"), Currency: "USD"}, false},
		{"invalid threshold", budgetInput{Name: "Food", LimitAmount: testMoney("1000"), AlertThresholdPercent: 101}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := len(test.input.validate()) == 0; got != test.valid {
				t.Fatalf("valid = %v, fields = %v", got, test.input.validate())
			}
		})
	}
}

func TestBudgetInputApplyDefaults(t *testing.T) {
	var budget models.Budget
	input := budgetInput{Name: " Food ", Category: " Dining ", LimitAmount: testMoney("2500")}
	input.apply(&budget)
	if budget.Name != "Food" || budget.Category != "Dining" || budget.Period != "monthly" ||
		budget.Currency != "INR" || budget.AlertThresholdPercent != 80 || !budget.Active {
		t.Fatalf("budget defaults were not applied: %#v", budget)
	}
}
