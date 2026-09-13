package models

import (
	"time"

	"gorm.io/gorm"
)

const (
	AdminRoleViewer  = "viewer"
	AdminRoleSupport = "support"
	AdminRoleOwner   = "owner"
)

type AdminUser struct {
	ID         uint       `gorm:"primaryKey" json:"id"`
	UserID     uint       `gorm:"uniqueIndex;not null" json:"user_id"`
	User       User       `json:"user,omitempty" gorm:"foreignKey:UserID;constraint:OnDelete:CASCADE"`
	Role       string     `gorm:"type:varchar(24);index;not null;default:viewer" json:"role"`
	DisabledAt *time.Time `gorm:"index" json:"disabled_at,omitempty"`
	CreatedBy  string     `gorm:"type:varchar(120);not null;default:''" json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

type AdminAuditLog struct {
	ID uint `gorm:"primaryKey" json:"id"`
	// Null when the action came from the configured machine token. The previous
	// code substituted "first owner in the table", which recorded a named human
	// as the author of something they did not do.
	AdminUserID *uint     `gorm:"index" json:"admin_user_id,omitempty"`
	Actor       string    `gorm:"type:varchar(40);index;not null;default:admin_user" json:"actor"`
	Action      string    `gorm:"type:varchar(80);index;not null" json:"action"`
	SubjectType string    `gorm:"type:varchar(40);not null;default:''" json:"subject_type"`
	SubjectID   string    `gorm:"type:varchar(120);not null;default:''" json:"subject_id"`
	Payload     string    `gorm:"type:jsonb;not null;default:'{}'" json:"payload"`
	IPHash      string    `gorm:"type:char(64);not null;default:''" json:"-"`
	CreatedAt   time.Time `gorm:"index" json:"created_at"`
}

func (AdminAuditLog) TableName() string { return "admin_audit_log" }

// AdminDailyMetric is the bounded rollup used by the admin chart endpoints once
// raw event volume makes interactive grouping expensive.
type AdminDailyMetric struct {
	ID                 uint      `gorm:"primaryKey" json:"id"`
	MetricDate         string    `gorm:"type:date;uniqueIndex;not null" json:"metric_date"`
	ActiveUsers        int       `gorm:"not null;default:0" json:"active_users"`
	AIEvents           int       `gorm:"not null;default:0" json:"ai_events"`
	AICredits          int       `gorm:"not null;default:0" json:"ai_credits"`
	AICostUSDMicros    int64     `gorm:"not null;default:0" json:"ai_cost_usd_micros"`
	SuccessfulAIEvents int       `gorm:"not null;default:0" json:"successful_ai_events"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// See CalendarDay: a DATE column comes back as an RFC3339 timestamp, and
// these fields are published as calendar days.
func (metric *AdminDailyMetric) AfterFind(_ *gorm.DB) error {
	metric.MetricDate = CalendarDay(metric.MetricDate)
	return nil
}
