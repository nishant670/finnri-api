package http

import (
	"regexp"
	"strings"

	"finnri/internal/models"
)

var canonicalAccountTypes = map[string]string{
	"cash":        "cash",
	"upi":         "upi",
	"bank":        "bank",
	"credit_card": "credit_card",
	"credit":      "credit_card",
	"debit_card":  "debit_card",
	"debit":       "debit_card",
	"wallet":      "wallet",
	"wallets":     "wallet",
	"other":       "other",
}

type accountInput struct {
	Type            string       `json:"type"`
	Name            string       `json:"name"`
	Color           string       `json:"color"`
	Provider        string       `json:"provider"`
	Identifier      string       `json:"identifier"`
	ProviderID      *string      `json:"provider_id"`
	Last4           *string      `json:"last4"`
	UPIHandle       *string      `json:"upi_handle"`
	WalletNickname  *string      `json:"wallet_nickname"`
	AccountNickname *string      `json:"account_nickname"`
	CreditLimit     models.Money `json:"credit_limit"`
	DueDay          int          `json:"due_day"`
	FeeMonth        string       `json:"fee_month"`
	Balance         models.Money `json:"balance"`
	IsDefault       bool         `json:"is_default"`
	AutoCreated     bool         `json:"auto_created"`

	// Card statement settings are pointers so that omitting them means "leave
	// as is" rather than "reset to zero". The app sends the whole account back
	// on every edit, and StatementDay in particular is inferred once from the
	// user's first bill — a rename must not silently wipe it and take the
	// card's billing cycle with it.
	StatementDay       *int  `json:"statement_day"`
	ReminderDaysBefore *int  `json:"reminder_days_before"`
	ReminderEnabled    *bool `json:"reminder_enabled"`
	AutopayEnabled     *bool `json:"autopay_enabled"`
}

var fourDigits = regexp.MustCompile(`^[0-9]{4}$`)

func (input accountInput) validate() map[string]string {
	fields := map[string]string{}
	if strings.TrimSpace(input.Name) == "" {
		fields["name"] = "is required"
	}
	if normalizeAccountType(input.Type) == "" {
		fields["type"] = "is invalid"
	}
	accountType := normalizeAccountType(input.Type)
	if input.ProviderID != nil && strings.TrimSpace(*input.ProviderID) != "" {
		provider, ok := accountProviderByID(*input.ProviderID)
		if !ok {
			fields["provider_id"] = "is not in the provider catalogue"
		} else if !providerSupportsType(provider, accountType) {
			fields["provider_id"] = "does not support this account type"
		}
	}
	if input.Last4 != nil && strings.TrimSpace(*input.Last4) != "" {
		if !fourDigits.MatchString(strings.TrimSpace(*input.Last4)) {
			fields["last4"] = "must be exactly 4 digits"
		} else if accountType != "bank" && accountType != "credit_card" && accountType != "debit_card" {
			fields["last4"] = "is only valid for bank and card accounts"
		}
	}
	if input.UPIHandle != nil && strings.TrimSpace(*input.UPIHandle) != "" {
		handle := strings.TrimSpace(*input.UPIHandle)
		if accountType != "upi" {
			fields["upi_handle"] = "is only valid for UPI accounts"
		} else if len(handle) > 120 || !strings.Contains(handle, "@") {
			fields["upi_handle"] = "must be a valid UPI handle"
		}
	}
	if input.WalletNickname != nil && strings.TrimSpace(*input.WalletNickname) != "" && accountType != "wallet" {
		fields["wallet_nickname"] = "is only valid for wallet accounts"
	}
	if input.WalletNickname != nil && len(strings.TrimSpace(*input.WalletNickname)) > 120 {
		fields["wallet_nickname"] = "must be at most 120 characters"
	}
	if input.AccountNickname != nil && len(strings.TrimSpace(*input.AccountNickname)) > 120 {
		fields["account_nickname"] = "must be at most 120 characters"
	}
	if input.CreditLimit < 0 {
		fields["credit_limit"] = "must not be negative"
	}
	if input.DueDay < 0 || input.DueDay > 31 {
		fields["due_day"] = "must be between 1 and 31"
	}
	if input.StatementDay != nil && (*input.StatementDay < 0 || *input.StatementDay > 31) {
		fields["statement_day"] = "must be between 1 and 31"
	}
	if input.ReminderDaysBefore != nil && (*input.ReminderDaysBefore < 0 || *input.ReminderDaysBefore > 30) {
		fields["reminder_days_before"] = "must be between 0 and 30"
	}
	return fields
}

func (input accountInput) apply(account *models.Account) {
	account.Type = normalizeAccountType(input.Type)
	account.Name = strings.TrimSpace(input.Name)
	account.Color = strings.TrimSpace(input.Color)
	account.Provider = strings.TrimSpace(input.Provider)
	account.Identifier = strings.TrimSpace(input.Identifier)
	if input.ProviderID != nil {
		account.ProviderID = strings.TrimSpace(*input.ProviderID)
	} else if account.Provider != "" {
		// Older clients do not send ProviderID. Resolve their current text each
		// time so changing a catalogued provider to a custom one also clears the
		// stale structured reference.
		account.ProviderID = ""
	}
	if account.ProviderID == "" {
		if provider, ok := matchAccountProvider(account.Provider); ok && providerSupportsType(provider, account.Type) {
			account.ProviderID = provider.ID
		}
	}
	if provider, ok := accountProviderByID(account.ProviderID); ok && providerSupportsType(provider, account.Type) {
		account.Provider = provider.DisplayName
	}

	if input.Last4 != nil {
		account.Last4 = strings.TrimSpace(*input.Last4)
	}
	if input.UPIHandle != nil {
		account.UPIHandle = strings.TrimSpace(*input.UPIHandle)
	}
	if input.WalletNickname != nil {
		account.WalletNickname = strings.TrimSpace(*input.WalletNickname)
	}
	if input.AccountNickname != nil {
		account.AccountNickname = strings.TrimSpace(*input.AccountNickname)
	}
	// An old client sends only Identifier. Infer the structured field so its
	// edits remain forward-compatible. A new client also writes Identifier as a
	// display fallback for older app versions.
	if account.Identifier != "" {
		switch account.Type {
		case "bank", "credit_card", "debit_card":
			if input.Last4 == nil && fourDigits.MatchString(account.Identifier) {
				account.Last4 = account.Identifier
			}
		case "upi":
			if input.UPIHandle == nil {
				account.UPIHandle = account.Identifier
			}
		case "wallet":
			if input.WalletNickname == nil {
				account.WalletNickname = account.Identifier
			}
		}
	}
	if account.Identifier == "" {
		account.Identifier = accountDisplayIdentifier(*account)
	}
	account.CreditLimit = input.CreditLimit
	account.DueDay = input.DueDay
	account.FeeMonth = strings.TrimSpace(input.FeeMonth)
	account.Balance = input.Balance
	account.IsDefault = input.IsDefault
	account.AutoCreated = input.AutoCreated

	if input.StatementDay != nil {
		account.StatementDay = *input.StatementDay
	}
	if input.ReminderDaysBefore != nil {
		account.ReminderDaysBefore = *input.ReminderDaysBefore
	}
	if input.ReminderEnabled != nil {
		account.ReminderEnabled = *input.ReminderEnabled
	}
	if input.AutopayEnabled != nil {
		account.AutopayEnabled = *input.AutopayEnabled
	}
}

func accountDisplayIdentifier(account models.Account) string {
	switch normalizeAccountType(account.Type) {
	case "bank", "credit_card", "debit_card":
		return strings.TrimSpace(account.Last4)
	case "upi":
		return strings.TrimSpace(account.UPIHandle)
	case "wallet":
		return strings.TrimSpace(account.WalletNickname)
	default:
		return strings.TrimSpace(account.Identifier)
	}
}

func normalizeAccountType(accountType string) string {
	return canonicalAccountTypes[strings.ToLower(strings.TrimSpace(accountType))]
}
