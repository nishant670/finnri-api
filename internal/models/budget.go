package models

import (
	"time"

	"gorm.io/gorm"
)

type Budget struct {
	ID                    uint      `gorm:"primaryKey" json:"id"`
	UserID                uint      `gorm:"index;not null" json:"user_id"`
	User                  User      `json:"-" gorm:"foreignKey:UserID"`
	Name                  string    `gorm:"type:varchar(120);not null" json:"name"`
	Period                string    `gorm:"type:varchar(16);not null;default:monthly" json:"period"`
	Category              string    `gorm:"type:varchar(80);index" json:"category"`
	LimitAmount           Money     `gorm:"type:numeric(19,2);not null" json:"limit_amount"`
	Currency              string    `gorm:"type:char(3);not null;default:INR" json:"currency"`
	AlertThresholdPercent int       `gorm:"not null;default:80" json:"alert_threshold_percent"`
	Active                bool      `gorm:"not null;default:true" json:"active"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

type BudgetAlert struct {
	ID             uint         `gorm:"primaryKey" json:"id"`
	UserID         uint         `gorm:"uniqueIndex:idx_budget_alert_once_period;index;not null" json:"user_id"`
	User           User         `json:"-" gorm:"foreignKey:UserID"`
	BudgetID       uint         `gorm:"uniqueIndex:idx_budget_alert_once_period;index;not null" json:"budget_id"`
	Budget         Budget       `json:"-" gorm:"foreignKey:BudgetID"`
	PeriodStart    string       `gorm:"type:date;uniqueIndex:idx_budget_alert_once_period;not null" json:"period_start"`
	Kind           string       `gorm:"type:varchar(24);uniqueIndex:idx_budget_alert_once_period;not null" json:"kind"`
	SpendAmount    Money        `gorm:"type:numeric(19,2);not null" json:"spend_amount"`
	LimitAmount    Money        `gorm:"type:numeric(19,2);not null" json:"limit_amount"`
	NotificationID *uint        `gorm:"index" json:"notification_id"`
	Notification   Notification `json:"-" gorm:"foreignKey:NotificationID"`
	CreatedAt      time.Time    `json:"created_at"`
}

// See CalendarDay: a DATE column comes back as an RFC3339 timestamp, and
// these fields are published as calendar days.
func (alert *BudgetAlert) AfterFind(_ *gorm.DB) error {
	alert.PeriodStart = CalendarDay(alert.PeriodStart)
	return nil
}
