package http

import (
	"strings"
	"time"

	"finnri/internal/models"
)

const (
	subscriptionIntervalDaily         = "daily"
	subscriptionIntervalBusinessDaily = "business_daily"
	subscriptionIntervalWeekly        = "weekly"
	subscriptionIntervalBiweekly      = "biweekly"
	subscriptionIntervalMonthly       = "monthly"
	subscriptionIntervalQuarterly     = "quarterly"
	subscriptionIntervalYearly        = "yearly"

	subscriptionStatusActive    = "active"
	subscriptionStatusPaused    = "paused"
	subscriptionStatusCancelled = "cancelled"

	defaultSubscriptionReminderDays = 3
	maxSubscriptionReminderDays     = 30
	maxSubscriptionNameLength       = 120
	maxSubscriptionInstalments      = 600

	recurringKindSubscription = "subscription"
	recurringKindLoan         = "loan"
	recurringKindInvestment   = "investment"
	recurringKindBill         = "bill"

	maxRecurringRatePct = 60.0
)

var recurringLoanTypes = map[string]bool{
	"personal": true, "car": true, "two_wheeler": true, "home": true, "education": true,
	"gold": true, "consumer": true, "business": true, "other": true,
}

type subscriptionInput struct {
	AccountID       *uint        `json:"account_id"`
	Name            string       `json:"name"`
	Merchant        string       `json:"merchant"`
	Category        string       `json:"category"`
	Amount          models.Money `json:"amount"`
	Currency        string       `json:"currency"`
	BillingInterval string       `json:"billing_interval"`
	NextDueDate     string       `json:"next_due_date"`
	LastChargedDate string       `json:"last_charged_date"`
	Status          string       `json:"status"`
	ReminderDays    *int         `json:"reminder_days"`
	CancelBeforeDue bool         `json:"cancel_before_due"`
	CancelOnDate    string       `json:"cancel_on_date"`
	AutoPay         bool         `json:"autopay"`
	PaymentMode     string       `json:"payment_mode"`
	TransactionTag  string       `json:"transaction_tag"`
	PurposeType     string       `json:"purpose_type"`
	Notes           string       `json:"notes"`
	// Both optional. Left out, an update keeps what is stored.
	TotalInstalments *int `json:"total_instalments"`
	InstalmentsPaid  *int `json:"instalments_paid"`

	// Everything below is optional and, like the instalment counts, leaves
	// the stored value alone when omitted — an older client that knows only
	// subscriptions must not wipe a loan's details by editing its name.
	// Kind is inferred on create when absent; see inferRecurringKind.
	Kind                 *string       `json:"kind"`
	LoanType             *string       `json:"loan_type"`
	Lender               *string       `json:"lender"`
	Principal            *models.Money `json:"principal"`
	AnnualRatePct        *float64      `json:"annual_rate_pct"`
	ProcessingFee        *models.Money `json:"processing_fee"`
	ForeclosureChargePct *float64      `json:"foreclosure_charge_pct"`
	StartDate            *string       `json:"start_date"`
	Platform             *string       `json:"platform"`
	StepUpPct            *float64      `json:"step_up_pct"`
}

type markSubscriptionPaidInput struct {
	PaidDate string `json:"paid_date"`
}

func (input subscriptionInput) validate() map[string]string {
	fields := map[string]string{}
	if strings.TrimSpace(input.Name) == "" {
		fields["name"] = "is required"
	} else if len([]rune(strings.TrimSpace(input.Name))) > maxSubscriptionNameLength {
		fields["name"] = "must not exceed 120 characters"
	}
	if !input.Amount.IsPositive() {
		fields["amount"] = "must be greater than zero"
	}
	if currency := normalizeSubscriptionCurrency(input.Currency); currency != "" && currency != "INR" {
		fields["currency"] = "must be INR"
	}
	if normalizeSubscriptionInterval(input.BillingInterval) == "" {
		fields["billing_interval"] = "must be daily, business_daily, weekly, biweekly, monthly, quarterly, or yearly"
	}
	if _, err := parseStrictAPIDate(input.NextDueDate); err != nil {
		fields["next_due_date"] = "must use YYYY-MM-DD"
	}
	if strings.TrimSpace(input.LastChargedDate) != "" {
		if _, err := parseStrictAPIDate(input.LastChargedDate); err != nil {
			fields["last_charged_date"] = "must use YYYY-MM-DD"
		}
	}
	if normalizeSubscriptionStatus(input.Status) == "" {
		fields["status"] = "must be active, paused, or cancelled"
	}
	if input.ReminderDays != nil && (*input.ReminderDays < 0 || *input.ReminderDays > maxSubscriptionReminderDays) {
		fields["reminder_days"] = "must be between 0 and 30"
	}
	interval := normalizeSubscriptionInterval(input.BillingInterval)
	if (interval == subscriptionIntervalDaily || interval == subscriptionIntervalBusinessDaily) && !input.AutoPay {
		fields["autopay"] = "must be enabled for daily and market-day schedules"
	}
	if input.AutoPay && input.AccountID == nil {
		fields["account_id"] = "is required when autopay is enabled"
	}
	if strings.TrimSpace(input.PaymentMode) != "" && normalizeSubscriptionPaymentMode(input.PaymentMode) == "" {
		fields["payment_mode"] = "must be Cash, Bank Account, UPI, Credit Card, or Wallets"
	}
	if input.CancelBeforeDue {
		if _, err := parseStrictAPIDate(input.CancelOnDate); err != nil {
			fields["cancel_on_date"] = "is required and must use YYYY-MM-DD"
		}
	} else if strings.TrimSpace(input.CancelOnDate) != "" {
		fields["cancel_on_date"] = "must be blank when cancellation reminder is disabled"
	}
	if input.AccountID != nil && *input.AccountID == 0 {
		fields["account_id"] = "must be a positive integer"
	}
	if input.TotalInstalments != nil && (*input.TotalInstalments < 0 || *input.TotalInstalments > maxSubscriptionInstalments) {
		fields["total_instalments"] = "must be between 0 and 600"
	}
	if input.Kind != nil && normalizeRecurringKind(*input.Kind) == "" {
		fields["kind"] = "must be subscription, loan, investment, or bill"
	}
	if input.LoanType != nil && strings.TrimSpace(*input.LoanType) != "" &&
		!recurringLoanTypes[strings.ToLower(strings.TrimSpace(*input.LoanType))] {
		fields["loan_type"] = "must be personal, car, two_wheeler, home, education, gold, consumer, business, or other"
	}
	if input.Principal != nil && *input.Principal < 0 {
		fields["principal"] = "must not be negative"
	}
	if input.ProcessingFee != nil && *input.ProcessingFee < 0 {
		fields["processing_fee"] = "must not be negative"
	}
	if input.AnnualRatePct != nil && (*input.AnnualRatePct < 0 || *input.AnnualRatePct > maxRecurringRatePct) {
		fields["annual_rate_pct"] = "must be between 0 and 60"
	}
	if input.ForeclosureChargePct != nil && (*input.ForeclosureChargePct < 0 || *input.ForeclosureChargePct > 20) {
		fields["foreclosure_charge_pct"] = "must be between 0 and 20"
	}
	if input.StepUpPct != nil && (*input.StepUpPct < 0 || *input.StepUpPct > 100) {
		fields["step_up_pct"] = "must be between 0 and 100"
	}
	if input.StartDate != nil && strings.TrimSpace(*input.StartDate) != "" {
		if _, err := parseStrictAPIDate(*input.StartDate); err != nil {
			fields["start_date"] = "must use YYYY-MM-DD"
		}
	}
	if input.InstalmentsPaid != nil {
		if *input.InstalmentsPaid < 0 {
			fields["instalments_paid"] = "must not be negative"
		} else if input.TotalInstalments != nil && *input.TotalInstalments > 0 && *input.InstalmentsPaid > *input.TotalInstalments {
			fields["instalments_paid"] = "must not exceed total_instalments"
		}
	}
	return fields
}

func (input subscriptionInput) apply(subscription *models.Subscription) {
	subscription.AccountID = input.AccountID
	subscription.Name = strings.TrimSpace(input.Name)
	subscription.Merchant = strings.TrimSpace(input.Merchant)
	subscription.Category = strings.TrimSpace(input.Category)
	subscription.Amount = input.Amount
	subscription.Currency = normalizeSubscriptionCurrency(input.Currency)
	if subscription.Currency == "" {
		subscription.Currency = "INR"
	}
	subscription.BillingInterval = normalizeSubscriptionInterval(input.BillingInterval)
	if subscription.BillingInterval == "" {
		subscription.BillingInterval = subscriptionIntervalMonthly
	}
	subscription.NextDueDate = strings.TrimSpace(input.NextDueDate)
	subscription.LastChargedDate = strings.TrimSpace(input.LastChargedDate)
	subscription.Status = normalizeSubscriptionStatus(input.Status)
	if subscription.Status == "" {
		subscription.Status = subscriptionStatusActive
	}
	subscription.ReminderDays = defaultSubscriptionReminderDays
	if input.ReminderDays != nil {
		subscription.ReminderDays = *input.ReminderDays
	}
	subscription.CancelBeforeDue = input.CancelBeforeDue
	subscription.CancelOnDate = strings.TrimSpace(input.CancelOnDate)
	subscription.AutoPay = input.AutoPay
	subscription.PaymentMode = normalizeSubscriptionPaymentMode(input.PaymentMode)
	if subscription.PaymentMode == "" {
		subscription.PaymentMode = "Cash"
	}
	if input.TotalInstalments != nil {
		subscription.TotalInstalments = *input.TotalInstalments
	}
	if input.InstalmentsPaid != nil {
		subscription.InstalmentsPaid = *input.InstalmentsPaid
	}
	input.applyRecurringDetails(subscription)

	// Tag and purpose follow the kind when the client leaves them out, so the
	// entries a loan or SIP writes are filed as EMI or Investment.
	subscription.TransactionTag = strings.TrimSpace(input.TransactionTag)
	if subscription.TransactionTag == "" {
		subscription.TransactionTag = defaultRecurringTag(subscription.Kind)
	}
	subscription.PurposeType = strings.ToLower(strings.TrimSpace(input.PurposeType))
	if subscription.PurposeType == "" {
		subscription.PurposeType = "normal_spend"
		if subscription.Kind == recurringKindInvestment {
			subscription.PurposeType = "investment"
		}
	}
	if subscription.BillingInterval == subscriptionIntervalDaily || subscription.BillingInterval == subscriptionIntervalBusinessDaily {
		subscription.ReminderDays = 0
	}
	subscription.Notes = strings.TrimSpace(input.Notes)
}

// applyRecurringDetails sets the kind and the loan and investment details,
// touching only what the request names.
func (input subscriptionInput) applyRecurringDetails(subscription *models.Subscription) {
	switch {
	case input.Kind != nil:
		subscription.Kind = normalizeRecurringKind(*input.Kind)
	case subscription.Kind == "":
		subscription.Kind = inferRecurringKind(input.TransactionTag, input.PurposeType, subscription.TotalInstalments)
	}
	if input.LoanType != nil {
		subscription.LoanType = strings.ToLower(strings.TrimSpace(*input.LoanType))
	}
	if input.Lender != nil {
		subscription.Lender = strings.TrimSpace(*input.Lender)
	}
	if input.Principal != nil {
		subscription.Principal = *input.Principal
	}
	if input.AnnualRatePct != nil {
		subscription.AnnualRatePct = *input.AnnualRatePct
	}
	if input.ProcessingFee != nil {
		subscription.ProcessingFee = *input.ProcessingFee
	}
	if input.ForeclosureChargePct != nil {
		subscription.ForeclosureChargePct = *input.ForeclosureChargePct
	}
	if input.StartDate != nil {
		subscription.StartDate = strings.TrimSpace(*input.StartDate)
	}
	if input.Platform != nil {
		subscription.Platform = strings.TrimSpace(*input.Platform)
	}
	if input.StepUpPct != nil {
		subscription.StepUpPct = *input.StepUpPct
	}
}

func normalizeRecurringKind(value string) string {
	switch kind := strings.ToLower(strings.TrimSpace(value)); kind {
	case recurringKindSubscription, recurringKindLoan, recurringKindInvestment, recurringKindBill:
		return kind
	case "emi":
		return recurringKindLoan
	case "sip":
		return recurringKindInvestment
	default:
		return ""
	}
}

// inferRecurringKind files a payment made by a client that does not send a
// kind — the current app's EMI tag and its SIP-style investments — the same
// way the migration filed the existing rows.
func inferRecurringKind(tag, purpose string, totalInstalments int) string {
	switch {
	case strings.EqualFold(strings.TrimSpace(tag), "EMI") || totalInstalments > 0:
		return recurringKindLoan
	case strings.EqualFold(strings.TrimSpace(tag), "Investment") ||
		strings.EqualFold(strings.TrimSpace(purpose), "investment"):
		return recurringKindInvestment
	default:
		return recurringKindSubscription
	}
}

func defaultRecurringTag(kind string) string {
	switch kind {
	case recurringKindLoan:
		return "EMI"
	case recurringKindInvestment:
		return "Investment"
	case recurringKindBill:
		return "General"
	default:
		return "Subscription"
	}
}

func normalizeSubscriptionPaymentMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "cash":
		return "Cash"
	case "bank", "bank account", "savings account", "saving account":
		return "Bank Account"
	case "upi":
		return "UPI"
	case "credit card", "creditcard":
		return "Credit Card"
	case "wallet", "wallets":
		return "Wallets"
	default:
		return ""
	}
}

func normalizeSubscriptionInterval(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", subscriptionIntervalMonthly:
		return subscriptionIntervalMonthly
	case subscriptionIntervalDaily:
		return subscriptionIntervalDaily
	case subscriptionIntervalBusinessDaily:
		return subscriptionIntervalBusinessDaily
	case subscriptionIntervalWeekly:
		return subscriptionIntervalWeekly
	case subscriptionIntervalBiweekly:
		return subscriptionIntervalBiweekly
	case subscriptionIntervalQuarterly:
		return subscriptionIntervalQuarterly
	case subscriptionIntervalYearly:
		return subscriptionIntervalYearly
	default:
		return ""
	}
}

func normalizeSubscriptionStatus(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", subscriptionStatusActive:
		return subscriptionStatusActive
	case subscriptionStatusPaused:
		return subscriptionStatusPaused
	case subscriptionStatusCancelled:
		return subscriptionStatusCancelled
	default:
		return ""
	}
}

func normalizeSubscriptionCurrency(value string) string {
	return strings.ToUpper(strings.TrimSpace(value))
}

func parseAPIDate(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if len(value) >= len("2006-01-02") {
		value = value[:len("2006-01-02")]
	}
	return time.Parse("2006-01-02", value)
}

func parseStrictAPIDate(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil {
		return time.Time{}, err
	}
	return parsed, nil
}
