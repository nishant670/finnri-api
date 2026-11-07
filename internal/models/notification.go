package models

import "time"

type Notification struct {
	ID        uint       `gorm:"primaryKey" json:"id"`
	UserID    uint       `gorm:"index;not null" json:"user_id"`
	User      User       `json:"-" gorm:"foreignKey:UserID"`
	Type      string     `gorm:"type:varchar(40);not null;index" json:"type"`
	Title     string     `gorm:"type:varchar(140);not null" json:"title"`
	Body      string     `gorm:"type:text;not null" json:"body"`
	ActionURL string     `gorm:"type:varchar(255)" json:"action_url"`
	ReadAt    *time.Time `gorm:"index" json:"read_at,omitempty"`
	// Amount and DueDate are what a reminder is for, when it is for one (P48).
	Amount  *Money  `gorm:"type:numeric(19,2)" json:"amount,omitempty"`
	DueDate *string `gorm:"type:varchar(10)" json:"due_date,omitempty"`
	// ResolvedAt is when the job behind it was done; the app clears it (P49).
	ResolvedAt *time.Time `gorm:"index" json:"resolved_at,omitempty"`
	CreatedAt  time.Time  `gorm:"index" json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}
