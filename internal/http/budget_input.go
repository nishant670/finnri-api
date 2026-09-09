package http

import (
	"strings"

	"finnri/internal/models"
)

const (
	budgetPeriodMonthly         = "monthly"
	defaultBudgetAlertThreshold = 80
	maxBudgetNameLength         = 120
	maxBudgetCategoryNameLength = 80
	minBudgetAlertThreshold     = 1
	maxBudgetAlertThreshold     = 100
)

type budgetInput struct {
	Name                  string       `json:"name"`
	Period                string       `json:"period"`
	Category              string       `json:"category"`
	LimitAmount           models.Money `json:"limit_amount"`
	Currency              string       `json:"currency"`
	AlertThresholdPercent int          `json:"alert_threshold_percent"`
	Active                *bool        `json:"active"`
}

func (input budgetInput) validate() map[string]string {
	fields := map[string]string{}
	if strings.TrimSpace(input.Name) == "" {
		fields["name"] = "is required"
	} else if len([]rune(strings.TrimSpace(input.Name))) > maxBudgetNameLength {
		fields["name"] = "must not exceed 120 characters"
	}
	if category := strings.TrimSpace(input.Category); len([]rune(category)) > maxBudgetCategoryNameLength {
		fields["category"] = "must not exceed 80 characters"
	}
	if !input.LimitAmount.IsPositive() {
		fields["limit_amount"] = "must be greater than zero"
	}
	if normalizeBudgetPeriod(input.Period) != budgetPeriodMonthly {
		fields["period"] = "must be monthly"
	}
	if normalized := normalizeBudgetCurrency(input.Currency); normalized != "" && normalized != "INR" {
		fields["currency"] = "must be INR"
	}
	threshold := input.AlertThresholdPercent
	if threshold == 0 {
		threshold = defaultBudgetAlertThreshold
	}
	if threshold < minBudgetAlertThreshold || threshold > maxBudgetAlertThreshold {
		fields["alert_threshold_percent"] = "must be between 1 and 100"
	}
	return fields
}

func (input budgetInput) apply(budget *models.Budget) {
	budget.Name = strings.TrimSpace(input.Name)
	budget.Period = normalizeBudgetPeriod(input.Period)
	if budget.Period == "" {
		budget.Period = budgetPeriodMonthly
	}
	budget.Category = strings.TrimSpace(input.Category)
	budget.LimitAmount = input.LimitAmount
	budget.Currency = normalizeBudgetCurrency(input.Currency)
	if budget.Currency == "" {
		budget.Currency = "INR"
	}
	budget.AlertThresholdPercent = input.AlertThresholdPercent
	if budget.AlertThresholdPercent == 0 {
		budget.AlertThresholdPercent = defaultBudgetAlertThreshold
	}
	if input.Active == nil {
		budget.Active = true
	} else {
		budget.Active = *input.Active
	}
}

func normalizeBudgetPeriod(period string) string {
	period = strings.ToLower(strings.TrimSpace(period))
	if period == "" {
		return budgetPeriodMonthly
	}
	return period
}

func normalizeBudgetCurrency(currency string) string {
	return strings.ToUpper(strings.TrimSpace(currency))
}
