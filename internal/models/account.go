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
	Balance        Money `gorm:"type:numeric(19,2);not null;default:0" json:"balance"`
	// BalanceTracked says the user gave this account a balance, so its running
	// balance is shown even when the opening worked out to exactly ₹0 — a meal
	// wallet logged from its first load does. Before it, a zero opening read
	// as "never entered". See migrations/0066_add_wallet_balance_and_pay_later.sql.
	BalanceTracked bool `gorm:"not null;default:false" json:"balance_tracked"`
	// CreditKind tells a pay-later line (CRED flash, LazyPay, Simpl) from a
	// card. Both are credit_card accounts and share the limit, statement and
	// due-date machinery; only the wording differs. Empty is a card.
	CreditKind string `gorm:"type:varchar(16);not null;default:''" json:"credit_kind,omitempty"`
	// CardProductID is the exact card from the catalogue (card_products), set
	// from Add card's product picker. Nil when the user's card is not listed or
	// they have not said; the card then draws from its bank alone. Credit cards
	// only. See migrations/0076_create_card_products.sql.
	CardProductID *string             `gorm:"type:varchar(64);index" json:"card_product_id"`
	CardProduct   *CardProductSummary `gorm:"-" json:"card_product,omitempty"`
	// CardNetwork is printed on the card as plain text, never as a logo:
	// visa, mastercard, rupay, amex or diners; empty when unknown.
	CardNetwork string `gorm:"type:varchar(16);not null;default:''" json:"card_network"`
	IsDefault   bool   `json:"is_default"`
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
	// CreditKind is "pay_later" for a pay-later lender, so picking one makes
	// the new credit_card account a pay-later line without another question.
	CreditKind string `json:"credit_kind,omitempty"`
}
