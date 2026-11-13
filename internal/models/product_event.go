package models

import "time"

// ProductEvent is one thing a person did in the app, kept so the funnel from
// first open to first saved entry to purchase can be counted.
//
// It is deliberately small. An event is a name from a fixed list and at most
// one label from a fixed alphabet — "voice", "google", "monthly". There is no
// free text and no props bag, so an amount, a merchant or something somebody
// typed has nowhere to go even by accident. Rows are deleted with the account
// and after the retention window.
type ProductEvent struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	UserID     uint      `gorm:"index;not null" json:"user_id"`
	Name       string    `gorm:"type:varchar(48);not null;index:idx_product_events_name_at,priority:1" json:"name"`
	Label      string    `gorm:"type:varchar(32);not null;default:''" json:"label"`
	Platform   string    `gorm:"type:varchar(16);not null;default:''" json:"platform"`
	AppVersion string    `gorm:"type:varchar(24);not null;default:''" json:"app_version"`
	OccurredAt time.Time `gorm:"not null;index:idx_product_events_name_at,priority:2" json:"occurred_at"`
	CreatedAt  time.Time `json:"created_at"`
}
