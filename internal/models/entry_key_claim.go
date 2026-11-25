package models

import "time"

// EntryKeyClaim is where a create and a cancel of one Idempotency-Key meet, so
// that exactly one of them wins. Every keyed create takes the row in its own
// transaction; a cancel takes the same row and, finding no entry, marks it
// cancelled for good. Deleted with the user.
type EntryKeyClaim struct {
	UserID    uint      `gorm:"primaryKey;autoIncrement:false" json:"-"`
	Key       string    `gorm:"column:idempotency_key;primaryKey;type:varchar(128)" json:"-"`
	Cancelled bool      `gorm:"not null;default:false" json:"-"`
	CreatedAt time.Time `json:"-"`
}
