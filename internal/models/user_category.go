package models

import "time"

// UserCategory is a category a user named themselves ("Dividend", "Pet Care"),
// kept so the picker offers it again on their next transaction.
//
// Categories the user's entries already carry are offered without a row here;
// a row exists when the user added one from the picker (before any entry used
// it) or removed one. Removed is a tombstone rather than a delete because the
// entries that use the name still exist, and without it the name would come
// straight back from them.
type UserCategory struct {
	ID     uint   `gorm:"primaryKey" json:"id"`
	UserID uint   `gorm:"uniqueIndex:idx_user_categories_user_type_key;not null" json:"user_id"`
	Type   string `gorm:"type:varchar(10);uniqueIndex:idx_user_categories_user_type_key;not null" json:"type"`
	Name   string `gorm:"type:varchar(40);not null" json:"name"`
	// NameKey is Name lowercased: "dividend" and "Dividend" are one category.
	NameKey   string    `gorm:"type:varchar(80);uniqueIndex:idx_user_categories_user_type_key;not null" json:"-"`
	Removed   bool      `gorm:"not null;default:false" json:"-"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
