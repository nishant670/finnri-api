package models

import "time"

// AccountRule ties a phrase to one of the user's accounts: "fuel" → the SBI
// BPCL card. A new expense whose title, merchant or tag mentions the phrase is
// pre-filled with that account when the bill or sentence did not name one.
//
// The table is created in schema.go (migration 0071), not by AutoMigrate, so
// both foreign keys cascade: a rule goes with its account and its user.
type AccountRule struct {
	ID        uint   `gorm:"primaryKey" json:"id"`
	UserID    uint   `gorm:"uniqueIndex:idx_account_rules_user_phrase;not null" json:"-"`
	AccountID uint   `gorm:"index:idx_account_rules_account;not null" json:"account_id"`
	Phrase    string `gorm:"type:varchar(40);not null" json:"phrase"`
	// PhraseKey is Phrase lowercased with its spacing collapsed: "Fuel" and
	// " fuel " are one rule.
	PhraseKey string    `gorm:"type:varchar(40);uniqueIndex:idx_account_rules_user_phrase;not null" json:"-"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
