package models

import "time"

// Payment is one attempt to buy one plan. A row is written when the order is
// created, before the person has paid anything, and only ever moves forward:
// created -> captured -> refunded, or created -> failed.
//
// The row is the join between a Razorpay order and the subscription it paid
// for, which is what makes a duplicate webhook harmless: the second delivery
// finds the payment already captured and stops.
type Payment struct {
	ID     uint  `gorm:"primaryKey" json:"id"`
	UserID uint  `gorm:"index;not null" json:"user_id"`
	User   *User `json:"-" gorm:"foreignKey:UserID"`
	PlanID uint  `gorm:"index;not null" json:"plan_id"`
	Plan   *Plan `json:"plan,omitempty" gorm:"foreignKey:PlanID"`

	Provider string `gorm:"type:varchar(40);index;not null" json:"provider"`
	// ProviderOrderID is unique: it is the idempotency key for the whole
	// purchase, and two subscriptions must never be granted for one order.
	ProviderOrderID   string `gorm:"type:varchar(120);uniqueIndex;not null" json:"provider_order_id"`
	ProviderPaymentID string `gorm:"type:varchar(120);index;not null;default:''" json:"provider_payment_id"`

	Status              string `gorm:"type:varchar(24);index;not null" json:"status"`
	AmountMinor         int64  `gorm:"not null" json:"amount_minor"`
	AmountRefundedMinor int64  `gorm:"not null;default:0" json:"amount_refunded_minor"`
	Currency            string `gorm:"type:char(3);not null;default:INR" json:"currency"`
	Method              string `gorm:"type:varchar(40);not null;default:''" json:"method,omitempty"`
	Receipt             string `gorm:"type:varchar(64);not null;default:''" json:"receipt,omitempty"`
	FailureReason       string `gorm:"type:varchar(255);not null;default:''" json:"failure_reason,omitempty"`

	// SubscriptionID is set when the capture actually granted a period, which
	// is what a refund needs in order to know what to take back.
	SubscriptionID *uint             `gorm:"index" json:"subscription_id,omitempty"`
	Subscription   *UserSubscription `json:"-" gorm:"foreignKey:SubscriptionID"`

	// PromotionCode names the offer this order was priced under ("" for full
	// price), and OriginalAmountMinor is what it would have cost without it.
	// The launch offer's cap and once-per-person rule are counted from these.
	PromotionCode       string `gorm:"type:varchar(40);index;not null;default:''" json:"promotion_code,omitempty"`
	OriginalAmountMinor int64  `gorm:"not null;default:0" json:"original_amount_minor,omitempty"`

	CapturedAt *time.Time `gorm:"index" json:"captured_at,omitempty"`
	RefundedAt *time.Time `gorm:"index" json:"refunded_at,omitempty"`
	CreatedAt  time.Time  `gorm:"index" json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// Payment statuses.
const (
	PaymentStatusCreated  = "created"
	PaymentStatusCaptured = "captured"
	PaymentStatusFailed   = "failed"
	PaymentStatusRefunded = "refunded"
)

// PaymentWebhookEvent records every webhook the provider delivers, verified or
// not. It is the idempotency ledger: the unique index on (provider, event_id)
// is what stops a retried delivery — which providers do routinely, and which
// an attacker can also force by replaying a body whose signature still checks
// out — from granting a second set of credits.
//
// Rejected deliveries are recorded too. A run of signature failures is the
// only visible sign of either a rotated secret or someone probing the endpoint.
type PaymentWebhookEvent struct {
	ID        uint   `gorm:"primaryKey" json:"id"`
	Provider  string `gorm:"type:varchar(40);not null;uniqueIndex:idx_payment_webhook_events_identity" json:"provider"`
	EventID   string `gorm:"type:varchar(160);not null;uniqueIndex:idx_payment_webhook_events_identity" json:"event_id"`
	EventType string `gorm:"type:varchar(80);index;not null;default:''" json:"event_type"`
	// PayloadHash lets a replay with a different body under a reused event id
	// be spotted, without keeping the payload itself.
	PayloadHash string     `gorm:"type:char(64);not null;default:''" json:"payload_hash"`
	Status      string     `gorm:"type:varchar(24);index;not null" json:"status"`
	Detail      string     `gorm:"type:varchar(255);not null;default:''" json:"detail,omitempty"`
	ProcessedAt *time.Time `json:"processed_at,omitempty"`
	CreatedAt   time.Time  `gorm:"index" json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// Webhook processing outcomes.
const (
	WebhookStatusProcessed = "processed"
	WebhookStatusIgnored   = "ignored"
	WebhookStatusRejected  = "rejected"
	WebhookStatusFailed    = "failed"
)
