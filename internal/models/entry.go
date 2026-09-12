package models

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"
)

type Entry struct {
	ID          uint        `gorm:"primaryKey" json:"id"`
	Title       string      `json:"title"`
	Type        string      `json:"type"`
	Amount      Money       `gorm:"type:numeric(19,2);not null" json:"amount"`
	Currency    string      `gorm:"type:char(3);not null;default:INR" json:"currency"`
	Source      string      `gorm:"type:varchar(16);not null;default:manual" json:"source"`
	Mode        string      `json:"mode"`
	CardNetwork string      `json:"card_network"`
	Category    string      `json:"category"`
	Merchant    string      `json:"merchant"`
	PurposeType string      `json:"purpose_type"`
	Tag         string      `json:"tag"`
	Tags        StringArray `gorm:"type:jsonb" json:"tags"`
	Notes       string      `json:"notes"`
	// Date stays a YYYY-MM-DD string in JSON while PostgreSQL stores and
	// compares it as a real DATE. Writers validate the API form before save.
	Date             string     `gorm:"type:date;not null" json:"date"`
	Time             string     `json:"time"`
	SourceText       string     `json:"source_text"`
	Attachment       string     `json:"attachment"`
	RefundableAmount *Money     `gorm:"type:numeric(19,2)" json:"refundable_amount,omitempty"`
	RefundExpectedOn *string    `gorm:"type:text" json:"refund_expected_on,omitempty"`
	RefundReminderAt *time.Time `gorm:"index" json:"refund_reminder_at,omitempty"`
	RefundStatus     *string    `gorm:"type:varchar(16);index" json:"refund_status,omitempty"`
	IdempotencyKey   *string    `gorm:"type:varchar(128);uniqueIndex:idx_entries_user_idempotency" json:"-"`
	AccountID        *uint      `gorm:"index" json:"account_id"`
	Account          *Account   `json:"account,omitempty" gorm:"foreignKey:AccountID"`

	UserID uint `gorm:"uniqueIndex:idx_entries_user_idempotency" json:"user_id"`
	User   User `json:"-" gorm:"foreignKey:UserID"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SQLite (used by the test suite) exposes DATE values as RFC3339 timestamps
// when scanning into a string. PostgreSQL returns the canonical date directly.
// Normalize at the model boundary so the public JSON contract is identical on
// both dialects and remains YYYY-MM-DD after the storage migration.
func (entry *Entry) AfterFind(_ *gorm.DB) error {
	if len(entry.Date) >= len("2006-01-02") {
		entry.Date = entry.Date[:len("2006-01-02")]
	}
	return nil
}

type StringArray []string

func (sa StringArray) Value() (driver.Value, error) {
	if len(sa) == 0 {
		return []byte("[]"), nil
	}
	return json.Marshal(sa)
}

func (sa *StringArray) Scan(value interface{}) error {
	if value == nil {
		*sa = nil
		return nil
	}
	var data []byte
	switch v := value.(type) {
	case []byte:
		data = v
	case string:
		data = []byte(v)
	default:
		return fmt.Errorf("unsupported type for StringArray: %T", value)
	}
	if len(data) == 0 {
		*sa = nil
		return nil
	}
	return json.Unmarshal(data, sa)
}
