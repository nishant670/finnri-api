package models

import (
	"time"

	"gorm.io/gorm"
)

type Plan struct {
	ID                      uint      `gorm:"primaryKey" json:"id"`
	Code                    string    `gorm:"type:varchar(64);uniqueIndex;not null" json:"code"`
	Name                    string    `gorm:"type:varchar(120);not null" json:"name"`
	BillingInterval         string    `gorm:"type:varchar(24);not null" json:"billing_interval"`
	PriceMinor              int64     `gorm:"not null;default:0" json:"price_minor"`
	ListPriceMinor          int64     `gorm:"not null;default:0" json:"list_price_minor"`
	Currency                string    `gorm:"type:char(3);not null;default:INR" json:"currency"`
	IncludedCredits         int       `gorm:"not null;default:0" json:"included_credits"`
	DailyCreditLimit        int       `gorm:"not null;default:0" json:"daily_credit_limit"`
	IsPublic                bool      `gorm:"not null;default:false" json:"is_public"`
	RequiresLogin           bool      `gorm:"not null;default:true" json:"requires_login"`
	RequiresPriorPaidMonths int       `gorm:"not null;default:0" json:"requires_prior_paid_months"`
	CreatedAt               time.Time `json:"created_at"`
	UpdatedAt               time.Time `json:"updated_at"`
}

type UserSubscription struct {
	ID                     uint      `gorm:"primaryKey" json:"id"`
	UserID                 uint      `gorm:"index;not null" json:"user_id"`
	User                   User      `json:"-" gorm:"foreignKey:UserID"`
	PlanID                 uint      `gorm:"index;not null" json:"plan_id"`
	Plan                   Plan      `json:"plan,omitempty" gorm:"foreignKey:PlanID"`
	Status                 string    `gorm:"type:varchar(24);index;not null" json:"status"`
	CurrentPeriodStart     time.Time `gorm:"index;not null" json:"current_period_start"`
	CurrentPeriodEnd       time.Time `gorm:"index;not null" json:"current_period_end"`
	Provider               string    `gorm:"type:varchar(40);not null;default:''" json:"provider"`
	ProviderCustomerID     string    `gorm:"type:varchar(120);index;not null;default:''" json:"provider_customer_id"`
	ProviderSubscriptionID string    `gorm:"type:varchar(120);index;not null;default:''" json:"provider_subscription_id"`
	CancelAtPeriodEnd      bool      `gorm:"not null;default:false" json:"cancel_at_period_end"`
	CreatedAt              time.Time `json:"created_at"`
	UpdatedAt              time.Time `json:"updated_at"`
}

type CreditGrant struct {
	ID                uint              `gorm:"primaryKey" json:"id"`
	UserID            *uint             `gorm:"index" json:"user_id,omitempty"`
	User              *User             `json:"-" gorm:"foreignKey:UserID"`
	GuestDeviceIDHash string            `gorm:"type:char(64);index;not null;default:''" json:"guest_device_id_hash,omitempty"`
	Source            string            `gorm:"type:varchar(40);index;not null" json:"source"`
	CreditsGranted    int               `gorm:"not null" json:"credits_granted"`
	CreditsRemaining  int               `gorm:"not null" json:"credits_remaining"`
	ValidFrom         time.Time         `gorm:"index;not null" json:"valid_from"`
	ExpiresAt         *time.Time        `gorm:"index" json:"expires_at,omitempty"`
	SubscriptionID    *uint             `gorm:"index" json:"subscription_id,omitempty"`
	Subscription      *UserSubscription `json:"-" gorm:"foreignKey:SubscriptionID"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
}

type CreditLedger struct {
	ID                uint          `gorm:"primaryKey" json:"id"`
	UserID            *uint         `gorm:"index" json:"user_id,omitempty"`
	User              *User         `json:"-" gorm:"foreignKey:UserID"`
	GuestDeviceIDHash string        `gorm:"type:char(64);index;not null;default:''" json:"guest_device_id_hash,omitempty"`
	GrantID           *uint         `gorm:"index" json:"grant_id,omitempty"`
	Grant             *CreditGrant  `json:"-" gorm:"foreignKey:GrantID"`
	Direction         string        `gorm:"type:varchar(20);index;not null" json:"direction"`
	Credits           int           `gorm:"not null" json:"credits"`
	BalanceAfter      int           `gorm:"not null" json:"balance_after"`
	ReasonCode        string        `gorm:"type:varchar(64);index;not null" json:"reason_code"`
	IdempotencyKey    string        `gorm:"type:varchar(120);not null;default:''" json:"idempotency_key,omitempty"`
	AIUsageEventID    *uint         `gorm:"index" json:"ai_usage_event_id,omitempty"`
	AIUsageEvent      *AIUsageEvent `json:"-" gorm:"foreignKey:AIUsageEventID"`
	CreatedAt         time.Time     `gorm:"index" json:"created_at"`
}

func (CreditLedger) TableName() string {
	return "credit_ledger"
}

type AIUsageEvent struct {
	ID                     uint       `gorm:"primaryKey" json:"id"`
	UserID                 *uint      `gorm:"index" json:"user_id,omitempty"`
	User                   *User      `json:"-" gorm:"foreignKey:UserID"`
	GuestDeviceIDHash      string     `gorm:"type:char(64);index;not null;default:''" json:"guest_device_id_hash,omitempty"`
	SessionID              string     `gorm:"type:varchar(120);index;not null;default:''" json:"session_id,omitempty"`
	RequestID              string     `gorm:"type:varchar(120);uniqueIndex;not null" json:"request_id"`
	IdempotencyKey         string     `gorm:"type:varchar(120);not null;default:''" json:"idempotency_key,omitempty"`
	ActionCode             string     `gorm:"type:varchar(80);index;not null" json:"action_code"`
	InputKind              string     `gorm:"type:varchar(20);index;not null" json:"input_kind"`
	Status                 string     `gorm:"type:varchar(32);index;not null" json:"status"`
	Provider               string     `gorm:"type:varchar(40);not null;default:''" json:"provider"`
	Model                  string     `gorm:"type:varchar(80);not null;default:''" json:"model"`
	SecondaryProvider      string     `gorm:"type:varchar(40);not null;default:''" json:"secondary_provider"`
	SecondaryModel         string     `gorm:"type:varchar(80);not null;default:''" json:"secondary_model"`
	EstimatedCredits       int        `gorm:"not null" json:"estimated_credits"`
	ReservedCredits        int        `gorm:"not null;default:0" json:"reserved_credits"`
	FinalCredits           int        `gorm:"not null;default:0" json:"final_credits"`
	EstimatedCostUSDMicros int64      `gorm:"not null;default:0" json:"estimated_cost_usd_micros"`
	ActualCostUSDMicros    *int64     `json:"actual_cost_usd_micros,omitempty"`
	PromptTokens           *int       `json:"prompt_tokens,omitempty"`
	CompletionTokens       *int       `json:"completion_tokens,omitempty"`
	TotalTokens            *int       `json:"total_tokens,omitempty"`
	AudioDurationMs        *int       `json:"audio_duration_ms,omitempty"`
	AudioBytes             *int64     `json:"audio_bytes,omitempty"`
	InputChars             *int       `json:"input_chars,omitempty"`
	ResponseBytes          *int       `json:"response_bytes,omitempty"`
	ErrorCode              string     `gorm:"type:varchar(80);index;not null;default:''" json:"error_code,omitempty"`
	StartedAt              time.Time  `gorm:"index;not null" json:"started_at"`
	ProviderStartedAt      *time.Time `json:"provider_started_at,omitempty"`
	FinishedAt             *time.Time `gorm:"index" json:"finished_at,omitempty"`
	CreatedAt              time.Time  `json:"created_at"`
	UpdatedAt              time.Time  `json:"updated_at"`
}

type AIUsageLimitEvent struct {
	ID                uint      `gorm:"primaryKey" json:"id"`
	UserID            *uint     `gorm:"index" json:"user_id,omitempty"`
	User              *User     `json:"-" gorm:"foreignKey:UserID"`
	GuestDeviceIDHash string    `gorm:"type:char(64);index;not null;default:''" json:"guest_device_id_hash,omitempty"`
	ActionCode        string    `gorm:"type:varchar(80);index;not null" json:"action_code"`
	Reason            string    `gorm:"type:varchar(64);index;not null" json:"reason"`
	RequiredCredits   int       `gorm:"not null;default:0" json:"required_credits"`
	AvailableCredits  int       `gorm:"not null;default:0" json:"available_credits"`
	DailyLimit        int       `gorm:"not null;default:0" json:"daily_limit"`
	UsedToday         int       `gorm:"not null;default:0" json:"used_today"`
	DailyRemaining    int       `gorm:"not null;default:0" json:"daily_remaining"`
	PlanCode          string    `gorm:"type:varchar(64);not null;default:''" json:"plan_code"`
	CreatedAt         time.Time `gorm:"index" json:"created_at"`
}

type AIModelPricing struct {
	ID                   uint      `gorm:"primaryKey" json:"id"`
	Provider             string    `gorm:"type:varchar(40);index;uniqueIndex:idx_ai_model_pricing_key;not null" json:"provider"`
	Model                string    `gorm:"type:varchar(80);index;uniqueIndex:idx_ai_model_pricing_key;not null" json:"model"`
	Operation            string    `gorm:"type:varchar(32);index;uniqueIndex:idx_ai_model_pricing_key;not null" json:"operation"`
	InputTokenUSDMicros  int64     `gorm:"not null;default:0" json:"input_token_usd_micros"`
	OutputTokenUSDMicros int64     `gorm:"not null;default:0" json:"output_token_usd_micros"`
	AudioMinuteUSDMicros int64     `gorm:"not null;default:0" json:"audio_minute_usd_micros"`
	RequestUSDMicros     int64     `gorm:"not null;default:0" json:"request_usd_micros"`
	CreditUSDMicros      int64     `gorm:"not null;default:100" json:"credit_usd_micros"`
	Active               bool      `gorm:"not null;default:true" json:"active"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

type AIAbuseBlock struct {
	ID                uint       `gorm:"primaryKey" json:"id"`
	UserID            *uint      `gorm:"index" json:"user_id,omitempty"`
	User              *User      `json:"-" gorm:"foreignKey:UserID"`
	GuestDeviceIDHash string     `gorm:"type:char(64);index;not null;default:''" json:"guest_device_id_hash,omitempty"`
	Scope             string     `gorm:"type:varchar(32);index;not null;default:'ai_parse'" json:"scope"`
	ReasonCode        string     `gorm:"type:varchar(80);index;not null" json:"reason_code"`
	Notes             string     `gorm:"type:text;not null;default:''" json:"notes"`
	Active            bool       `gorm:"index;not null;default:true" json:"active"`
	ExpiresAt         *time.Time `gorm:"index" json:"expires_at,omitempty"`
	CreatedBy         string     `gorm:"type:varchar(120);not null;default:''" json:"created_by,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

type DailyCreditUsage struct {
	ID                uint      `gorm:"primaryKey" json:"id"`
	UserID            *uint     `gorm:"index" json:"user_id,omitempty"`
	User              *User     `json:"-" gorm:"foreignKey:UserID"`
	GuestDeviceIDHash string    `gorm:"type:char(64);index;not null;default:''" json:"guest_device_id_hash,omitempty"`
	UsageDate         string    `gorm:"type:date;index;not null" json:"usage_date"`
	CreditsUsed       int       `gorm:"not null;default:0" json:"credits_used"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

func (DailyCreditUsage) TableName() string {
	return "daily_credit_usage"
}

type GuestUsageKey struct {
	ID                uint         `gorm:"primaryKey" json:"id"`
	GuestDeviceIDHash string       `gorm:"type:char(64);uniqueIndex;not null" json:"guest_device_id_hash"`
	IPHash            string       `gorm:"type:char(64);index;not null" json:"ip_hash"`
	FirstSeenAt       time.Time    `gorm:"not null" json:"first_seen_at"`
	LastSeenAt        time.Time    `gorm:"index;not null" json:"last_seen_at"`
	TrialGrantID      *uint        `gorm:"index" json:"trial_grant_id,omitempty"`
	TrialGrant        *CreditGrant `json:"-" gorm:"foreignKey:TrialGrantID"`
	AbuseScore        int          `gorm:"not null;default:0" json:"abuse_score"`
	CreatedAt         time.Time    `json:"created_at"`
	UpdatedAt         time.Time    `json:"updated_at"`
}

type LifetimeQuoteRequest struct {
	ID                          uint      `gorm:"primaryKey" json:"id"`
	UserID                      uint      `gorm:"index;not null" json:"user_id"`
	User                        User      `json:"-" gorm:"foreignKey:UserID"`
	Status                      string    `gorm:"type:varchar(24);index;not null;default:requested" json:"status"`
	PaidMonthsCompleted         int       `gorm:"not null;default:0" json:"paid_months_completed"`
	UsageWindowStart            time.Time `gorm:"index;not null" json:"usage_window_start"`
	UsageWindowEnd              time.Time `gorm:"index;not null" json:"usage_window_end"`
	UsageEventCount             int       `gorm:"not null;default:0" json:"usage_event_count"`
	CreditsUsed                 int       `gorm:"not null;default:0" json:"credits_used"`
	AverageMonthlyCredits       int       `gorm:"not null;default:0" json:"average_monthly_credits"`
	EstimatedCostUSDMicros      int64     `gorm:"not null;default:0" json:"estimated_cost_usd_micros"`
	AverageMonthlyCostUSDMicros int64     `gorm:"not null;default:0" json:"average_monthly_cost_usd_micros"`
	Notes                       string    `gorm:"type:text;not null;default:''" json:"notes"`
	CreatedAt                   time.Time `json:"created_at"`
	UpdatedAt                   time.Time `json:"updated_at"`
}

// See CalendarDay: a DATE column comes back as an RFC3339 timestamp, and
// these fields are published as calendar days.
func (usage *DailyCreditUsage) AfterFind(_ *gorm.DB) error {
	usage.UsageDate = CalendarDay(usage.UsageDate)
	return nil
}
