package models

import (
	"finnri/internal/identity"
	"time"

	"gorm.io/gorm"
)

type User struct {
	ID                  uint       `gorm:"primaryKey" json:"id"`
	UUID                string     `gorm:"uniqueIndex" json:"uuid"`                 // Public ID for API tokens check
	Email               *string    `gorm:"uniqueIndex" json:"email,omitempty"`      // Nullable unique email
	Phone               *string    `gorm:"uniqueIndex" json:"phone,omitempty"`      // Nullable unique phone
	PhoneNormalized     string     `gorm:"type:varchar(10);index" json:"-"`         // India-first identity comparison key
	GoogleSubject       *string    `gorm:"uniqueIndex" json:"-"`                    // Stable Google account subject
	DeviceID            *string    `gorm:"index" json:"device_id,omitempty"`        // For device-scoped guest reuse
	PinHash             string     `json:"-"`                                       // Bcrypt hash, hidden from JSON
	BiometricsEnabled   bool       `gorm:"default:false" json:"biometrics_enabled"` // User preference
	IsGuest             bool       `gorm:"default:false" json:"is_guest"`
	Username            string     `gorm:"uniqueIndex" json:"username"` // Unique username
	ProfileImage        string     `json:"profile_image"`
	FailedLoginAttempts int        `gorm:"default:0" json:"-"`
	LoginLockedUntil    *time.Time `gorm:"index" json:"-"`
	ConvertedAt         *time.Time `gorm:"index" json:"converted_at,omitempty"`
	LastActiveAt        *time.Time `gorm:"index" json:"last_active_at,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
	HasPin              bool       `gorm:"-" json:"has_pin"`
}

func (user *User) BeforeSave(_ *gorm.DB) error {
	if user.Phone == nil {
		user.PhoneNormalized = ""
	} else {
		user.PhoneNormalized = identity.NormalizePhone(*user.Phone)
	}
	return nil
}
