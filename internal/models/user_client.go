package models

import "time"

// UserClient is one app install or browser a user has used, as the client
// describes itself (app headers) or as its User-Agent shows (website). One row
// per user per client; last_seen_at moves forward as they keep using it.
// Deleted with the user.
type UserClient struct {
	ID           uint      `gorm:"primaryKey" json:"id"`
	UserID       uint      `gorm:"not null;uniqueIndex:idx_user_clients_key,priority:1;index" json:"user_id"`
	ClientKey    string    `gorm:"type:varchar(64);not null;uniqueIndex:idx_user_clients_key,priority:2" json:"-"`
	Platform     string    `gorm:"type:varchar(16);not null;default:'';index" json:"platform"` // android, ios, web, unknown
	FormFactor   string    `gorm:"type:varchar(16);not null;default:''" json:"form_factor"`    // phone, tablet, desktop, unknown
	Device       string    `gorm:"type:varchar(80);not null;default:''" json:"device"`         // "Samsung SM-S918B", "Chrome on Windows"
	OS           string    `gorm:"type:varchar(40);not null;default:''" json:"os"`             // "Android 15", "Windows"
	AppVersion   string    `gorm:"type:varchar(24);not null;default:''" json:"app_version"`    // "1.0.0"
	AppBuild     string    `gorm:"type:varchar(16);not null;default:''" json:"app_build"`      // "14"
	Variant      string    `gorm:"type:varchar(16);not null;default:''" json:"variant"`        // play, preview, debug, web
	IsTestDevice bool      `gorm:"not null;default:false" json:"is_test_device"`               // Google Play's pre-launch devices
	FirstSeenAt  time.Time `gorm:"not null" json:"first_seen_at"`
	LastSeenAt   time.Time `gorm:"not null;index" json:"last_seen_at"`
}
