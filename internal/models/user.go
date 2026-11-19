package models

import (
	"finnri/internal/identity"
	"time"

	"gorm.io/gorm"
)

type User struct {
	ID              uint    `gorm:"primaryKey" json:"id"`
	UUID            string  `gorm:"uniqueIndex" json:"uuid"`            // Public ID for API tokens check
	Email           *string `gorm:"uniqueIndex" json:"email,omitempty"` // Nullable unique email
	Phone           *string `gorm:"uniqueIndex" json:"phone,omitempty"` // Nullable unique phone
	PhoneNormalized string  `gorm:"type:varchar(10);index" json:"-"`    // India-first identity comparison key
	GoogleSubject   *string `gorm:"uniqueIndex" json:"-"`               // Stable Google account subject
	// DeviceID names the install a guest lives on: credits and the free trial
	// are counted against it. It is an identifier, not a credential, and it is
	// no longer sent to clients — see GuestSecretHash.
	DeviceID *string `gorm:"index" json:"-"`
	// GuestSecretHash is the SHA-256 of the random secret a guest's install
	// proves itself with. The device id used to do this job by itself: anyone
	// who read it — from a backup, a dump, a support screenshot — could open
	// that guest's records at any later time, and it was stored here in the
	// clear. Empty for accounts, and for guests whose app has not sent a
	// secret yet.
	GuestSecretHash     string     `gorm:"type:char(64);not null;default:'';index" json:"-"`
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
	TestMarkedAt *time.Time `json:"-"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	HasPin       bool       `gorm:"-" json:"has_pin"`
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
