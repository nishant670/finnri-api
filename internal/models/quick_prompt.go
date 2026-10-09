package models

import "time"

type QuickPrompt struct {
	ID     uint    `gorm:"primaryKey" json:"id"`
	UserID uint    `gorm:"index" json:"user_id"`
	Title  string  `json:"title"`
	Amount float64 `json:"amount"`
	// Type is "expense" or "income". Prompts saved before it existed are
	// expenses, which is what the app always used them as.
	Type     string `gorm:"type:varchar(10);not null;default:expense" json:"type"`
	Mode     string `json:"mode"`
	Category string `json:"category"`
	// AccountID is the account the shortcut pays from — which wallet, which
	// card — not just the payment mode. Nil means the default for the mode.
	// Deleting the account clears it rather than blocking the delete.
	AccountID *uint     `gorm:"index" json:"account_id"`
	Merchant  string    `gorm:"not null;default:''" json:"merchant"`
	Tag       string    `gorm:"not null;default:''" json:"tag"`
	Notes     string    `gorm:"not null;default:''" json:"notes"`
	Icon      string    `json:"icon"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
