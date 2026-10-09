package models

import "time"

type Feedback struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	UserID     uint      `gorm:"index;not null" json:"user_id"`
	User       User      `json:"-" gorm:"foreignKey:UserID"`
	Type       string    `gorm:"type:varchar(32);not null;index" json:"type"`
	Area       string    `gorm:"type:varchar(64);not null;default:''" json:"area"`
	Title      string    `gorm:"type:varchar(140);not null" json:"title"`
	Message    string    `gorm:"type:text;not null" json:"message"`
	Impact     string    `gorm:"type:varchar(32);not null;default:'nice_to_have'" json:"impact"`
	Status     string    `gorm:"type:varchar(32);not null;default:'new';index" json:"status"`
	AdminNotes string    `gorm:"type:text;not null;default:''" json:"admin_notes"`
	ResolvedBy *uint     `gorm:"index" json:"resolved_by,omitempty"`
	CreatedAt  time.Time `gorm:"index" json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`

	// Attachments are screenshots or files the user added to show what they
	// mean, as upload URLs from POST /v1/upload. At most three. They are read
	// back only by admins, through GET /v1/admin/feedback/:id/attachments/:i.
	Attachments StringArray `gorm:"type:jsonb;not null;default:'[]'" json:"attachments"`
}
