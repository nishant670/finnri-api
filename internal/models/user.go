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
	// IsTest marks an account that is not a real customer: Google Play's
	// pre-launch robots, the team's own accounts, or one an admin flagged. It
	// only changes what the admin console counts; the account works as usual.
	IsTest       bool       `gorm:"not null;default:false;index" json:"-"`
	TestReason   string     `gorm:"type:varchar(16);not null;default:''" json:"-"`
	TestMarkedAt *time.Time `json:"-"` // when the flag was last set or cleared
	// SignupPlatform is where the account was made: android, ios or web.
	// Empty for accounts made before it was recorded (2026-11).
	SignupPlatform string    `gorm:"type:varchar(16);not null;default:''" json:"-"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	HasPin         bool      `gorm:"-" json:"has_pin"`
}

func (user *User) BeforeSave(_ *gorm.DB) error {
	if user.Phone == nil {
		user.PhoneNormalized = ""
	} else {
		user.PhoneNormalized = identity.NormalizePhone(*user.Phone)
	}
	return nil
}

// Why an account is left out of the admin console's real-user numbers.
const (
	UserTestReasonTestLab  = "test_lab" // Google Play pre-launch report devices
	UserTestReasonInternal = "internal" // the team, family, and their own testing
	UserTestReasonManual   = "manual"   // flagged by an admin for another reason
)

// UserTestReasons is the allowed list; schema.go builds its CHECK from it.
var UserTestReasons = []string{UserTestReasonTestLab, UserTestReasonInternal, UserTestReasonManual}
