package models

import (
	"time"

	"gorm.io/gorm"
)

type Subscription struct {
	ID              uint      `gorm:"primaryKey" json:"id"`
	UserID          uint      `gorm:"index;not null" json:"user_id"`
	User            User      `json:"-" gorm:"foreignKey:UserID"`
	AccountID       *uint     `gorm:"index" json:"account_id"`
	Account         *Account  `json:"account,omitempty" gorm:"foreignKey:AccountID"`
	Name            string    `gorm:"type:varchar(120);not null" json:"name"`
	Merchant        string    `gorm:"type:varchar(120);index" json:"merchant"`
	Category        string    `gorm:"type:varchar(80);index" json:"category"`
	Amount          Money     `gorm:"type:numeric(19,2);not null" json:"amount"`
	Currency        string    `gorm:"type:char(3);not null;default:INR" json:"currency"`
	BillingInterval string    `gorm:"type:varchar(16);not null;default:monthly" json:"billing_interval"`
	NextDueDate     string    `gorm:"type:date;index;not null" json:"next_due_date"`
	LastChargedDate string    `gorm:"type:varchar(10)" json:"last_charged_date"`
	Status          string    `gorm:"type:varchar(16);not null;default:active;index" json:"status"`
	ReminderDays    int       `gorm:"not null;default:3" json:"reminder_days"`
	CancelBeforeDue bool      `gorm:"not null;default:false" json:"cancel_before_due"`
	CancelOnDate    string    `gorm:"type:varchar(10)" json:"cancel_on_date"`
	AutoPay         bool      `gorm:"column:autopay;not null;default:false;index" json:"autopay"`
	PaymentMode     string    `gorm:"type:varchar(24);not null;default:Cash" json:"payment_mode"`
	TransactionTag  string    `gorm:"type:varchar(40);not null;default:Subscription" json:"transaction_tag"`
	PurposeType     string    `gorm:"type:varchar(40);not null;default:normal_spend" json:"purpose_type"`
	Notes           string    `gorm:"type:text" json:"notes"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type SubscriptionOccurrence struct {
	ID             uint         `gorm:"primaryKey" json:"id"`
	UserID         uint         `gorm:"uniqueIndex:idx_subscription_occurrence_once;index;not null" json:"user_id"`
	SubscriptionID uint         `gorm:"uniqueIndex:idx_subscription_occurrence_once;index;not null" json:"subscription_id"`
	Subscription   Subscription `json:"subscription,omitempty" gorm:"foreignKey:SubscriptionID"`
	EntryID        uint         `gorm:"index;not null" json:"entry_id"`
	Entry          Entry        `json:"entry,omitempty" gorm:"foreignKey:EntryID"`
	DueDate        string       `gorm:"type:date;uniqueIndex:idx_subscription_occurrence_once;not null" json:"due_date"`
	Status         string       `gorm:"type:varchar(16);not null;default:pending;index" json:"status"`
	ConfirmedAt    *time.Time   `json:"confirmed_at,omitempty"`
	RevertedAt     *time.Time   `json:"reverted_at,omitempty"`
	NotificationID *uint        `gorm:"index" json:"notification_id,omitempty"`
	CreatedAt      time.Time    `json:"created_at"`
	UpdatedAt      time.Time    `json:"updated_at"`
}

type SubscriptionReminder struct {
	ID             uint         `gorm:"primaryKey" json:"id"`
	UserID         uint         `gorm:"uniqueIndex:idx_subscription_reminder_once_due;index;not null" json:"user_id"`
	User           User         `json:"-" gorm:"foreignKey:UserID"`
	SubscriptionID uint         `gorm:"uniqueIndex:idx_subscription_reminder_once_due;index;not null" json:"subscription_id"`
	Subscription   Subscription `json:"-" gorm:"foreignKey:SubscriptionID"`
	DueDate        string       `gorm:"type:date;uniqueIndex:idx_subscription_reminder_once_due;not null" json:"due_date"`
	Kind           string       `gorm:"type:varchar(24);uniqueIndex:idx_subscription_reminder_once_due;not null" json:"kind"`
	NotificationID *uint        `gorm:"index" json:"notification_id"`
	Notification   Notification `json:"-" gorm:"foreignKey:NotificationID"`
	CreatedAt      time.Time    `json:"created_at"`
}

// See CalendarDay: a DATE column comes back as an RFC3339 timestamp, and
// these fields are published as calendar days.
func (subscription *Subscription) AfterFind(_ *gorm.DB) error {
	subscription.NextDueDate = CalendarDay(subscription.NextDueDate)
	return nil
}

func (occurrence *SubscriptionOccurrence) AfterFind(_ *gorm.DB) error {
	occurrence.DueDate = CalendarDay(occurrence.DueDate)
	return nil
}

func (reminder *SubscriptionReminder) AfterFind(_ *gorm.DB) error {
	reminder.DueDate = CalendarDay(reminder.DueDate)
	return nil
}
