package models

import (
	"time"
)

type Account struct {
	ID         uint   `gorm:"primaryKey" json:"id"`
	UserID     uint   `json:"user_id"`
	Type       string `json:"type"` // cash, upi, bank, credit_card, debit_card, wallet, other
	Name       string `json:"name"`
	Color      string `json:"color"`
	Provider   string `json:"provider"`   // bank name or issuer
	Identifier string `json:"identifier"` // last 4 digits or upi id
	// Structured provider and identifiers coexist with the legacy strings.
	// Old clients keep reading Provider/Identifier; new clients prefer these.
	ProviderID      string                  `gorm:"type:varchar(64);index" json:"provider_id,omitempty"`
	ProviderDetails *AccountProviderDetails `gorm:"-" json:"provider_details,omitempty"`
	Last4           string                  `gorm:"type:varchar(4)" json:"last4,omitempty"`
	UPIHandle       string                  `gorm:"type:varchar(120)" json:"upi_handle,omitempty"`
	WalletNickname  string                  `gorm:"type:varchar(120)" json:"wallet_nickname,omitempty"`
	AccountNickname string                  `gorm:"type:varchar(120)" json:"account_nickname,omitempty"`
	CreditLimit     Money                   `gorm:"type:numeric(19,2);not null;default:0" json:"credit_limit"`
	DueDay          int                     `json:"due_day"`
	// StatementDay is the day of month a card bills on. Zero means the user
	// has not told us; the first statement they enter infers it.
	StatementDay int `gorm:"not null;default:0" json:"statement_day"`
	// ReminderDaysBefore is the lead time on the "bill due soon" reminder.
	ReminderDaysBefore int `gorm:"not null;default:3" json:"reminder_days_before"`
	// ReminderEnabled lets each card opt out without overloading a lead time of
	// zero, which means "remind me on the due date".
	ReminderEnabled bool `gorm:"not null;default:true" json:"reminder_enabled"`
	// AutopayEnabled only changes how the due-date reminder is worded. A
	// payment is never recorded without the user confirming it.
	AutopayEnabled bool `gorm:"not null;default:false" json:"autopay_enabled"`
	// CardLedgerResetAt starts a statement-less card's fallback outstanding at
	// zero. Statement-backed cards ignore it and keep using bank bill data.
	CardLedgerResetAt *time.Time `json:"card_ledger_reset_at,omitempty"`
	// EntryID is the deterministic boundary used for arithmetic. Timestamps can
	// have database-specific precision, while entry ids preserve insertion order.
	CardLedgerResetEntryID *uint  `json:"-"`
	FeeMonth               string `json:"fee_month"`
	// AnnualFee is the card's yearly membership fee before GST, and
	// FeeWaiverSpend the yearly spend that waives it. Both are zero when
	// unknown; a statement that prints them can fill them in.
	AnnualFee      Money `gorm:"type:numeric(19,2);not null;default:0" json:"annual_fee"`
	FeeWaiverSpend Money `gorm:"type:numeric(19,2);not null;default:0" json:"fee_waiver_spend"`
	Balance                Money  `gorm:"type:numeric(19,2);not null;default:0" json:"balance"`
	IsDefault              bool   `json:"is_default"`
	// AutoCreated distinguishes a capture shortcut from an intentionally sparse
	// account. The first explicit account edit clears it.
	AutoCreated bool      `gorm:"not null;default:false" json:"auto_created"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type AccountProviderDetails struct {
	ID          string   `json:"id"`
	DisplayName string   `json:"display_name"`
	TypeSupport []string `json:"type_support"`
	AssetKey    string   `json:"asset_key"`
	Aliases     []string `json:"aliases,omitempty"`
}
