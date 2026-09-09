package billing

import (
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"finnri/internal/models"
)

type FeatureCode string

const (
	FeatureAITextCapture         FeatureCode = "ai_text_capture"
	FeatureAIVoiceCapture        FeatureCode = "ai_voice_capture"
	FeatureAdvancedInsights      FeatureCode = "advanced_insights"
	FeatureWeeklyReview          FeatureCode = "weekly_review"
	FeatureBudgets               FeatureCode = "budgets"
	FeatureSubscriptionReminders FeatureCode = "subscription_reminders"
	FeatureSplitLedger           FeatureCode = "split_ledger"
	FeatureWebDashboard          FeatureCode = "web_dashboard"
	FeatureExports               FeatureCode = "exports"
	FeatureBulkEdit              FeatureCode = "bulk_edit"
	FeatureFutureAIAdvisor       FeatureCode = "future_ai_advisor"
)

const (
	EntitlementAllowed         = "allowed"
	EntitlementPaymentRequired = "payment_required"
	EntitlementFeatureLocked   = "feature_locked"
)

var ErrUnknownFeature = errors.New("unknown feature")

type FeatureRule struct {
	Code             FeatureCode
	Label            string
	RequiresPaidPlan bool
	CreditBacked     bool
}

type EntitlementService struct {
	db  *gorm.DB
	now func() time.Time
}

type EntitlementResult struct {
	Allowed         bool        `json:"allowed"`
	Reason          string      `json:"reason"`
	FeatureCode     FeatureCode `json:"feature_code"`
	FeatureLabel    string      `json:"feature_label"`
	RequiredPlan    string      `json:"required_plan,omitempty"`
	PlanCode        string      `json:"plan_code,omitempty"`
	PaidPlanActive  bool        `json:"paid_plan_active"`
	UpgradeRequired bool        `json:"upgrade_required"`
}

func NewEntitlementService(db *gorm.DB) *EntitlementService {
	return &EntitlementService{db: db, now: func() time.Time { return time.Now().UTC() }}
}

func NewEntitlementServiceWithClock(db *gorm.DB, now func() time.Time) *EntitlementService {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &EntitlementService{db: db, now: now}
}

func DefaultFeatureRules() map[FeatureCode]FeatureRule {
	return map[FeatureCode]FeatureRule{
		FeatureAITextCapture: {
			Code:         FeatureAITextCapture,
			Label:        "AI text capture",
			CreditBacked: true,
		},
		FeatureAIVoiceCapture: {
			Code:         FeatureAIVoiceCapture,
			Label:        "AI voice capture",
			CreditBacked: true,
		},
		FeatureAdvancedInsights: {
			Code:             FeatureAdvancedInsights,
			Label:            "Advanced insights",
			RequiresPaidPlan: true,
		},
		FeatureWeeklyReview: {
			Code:             FeatureWeeklyReview,
			Label:            "Weekly review",
			RequiresPaidPlan: true,
		},
		FeatureBudgets: {
			Code:             FeatureBudgets,
			Label:            "Budgets",
			RequiresPaidPlan: true,
		},
		FeatureSubscriptionReminders: {
			Code:             FeatureSubscriptionReminders,
			Label:            "Subscription reminders",
			RequiresPaidPlan: true,
		},
		FeatureSplitLedger: {
			Code:  FeatureSplitLedger,
			Label: "Split ledger",
		},
		FeatureWebDashboard: {
			Code:             FeatureWebDashboard,
			Label:            "Web dashboard",
			RequiresPaidPlan: true,
		},
		FeatureExports: {
			Code:             FeatureExports,
			Label:            "Exports",
			RequiresPaidPlan: true,
		},
		FeatureBulkEdit: {
			Code:             FeatureBulkEdit,
			Label:            "Bulk edit",
			RequiresPaidPlan: true,
		},
		FeatureFutureAIAdvisor: {
			Code:             FeatureFutureAIAdvisor,
			Label:            "AI advisor",
			RequiresPaidPlan: true,
		},
	}
}

func PaidFeatureCodes() []string {
	return []string{
		string(FeatureAITextCapture),
		string(FeatureAIVoiceCapture),
		string(FeatureAdvancedInsights),
		string(FeatureWeeklyReview),
		string(FeatureBudgets),
		string(FeatureSubscriptionReminders),
		string(FeatureWebDashboard),
		string(FeatureExports),
		string(FeatureBulkEdit),
		string(FeatureFutureAIAdvisor),
	}
}

func (s *EntitlementService) Check(subject CreditSubject, featureCode FeatureCode) (EntitlementResult, error) {
	if !subject.valid() {
		return EntitlementResult{Allowed: false, Reason: EntitlementFeatureLocked, FeatureCode: featureCode, UpgradeRequired: true}, ErrInvalidCreditSubject
	}
	rule, ok := DefaultFeatureRules()[featureCode]
	if !ok {
		return EntitlementResult{}, fmt.Errorf("%w: %s", ErrUnknownFeature, featureCode)
	}
	result := EntitlementResult{
		Allowed:      true,
		Reason:       EntitlementAllowed,
		FeatureCode:  rule.Code,
		FeatureLabel: rule.Label,
	}
	if !rule.RequiresPaidPlan {
		return result, nil
	}

	plan, paidActive, err := s.activePaidPlan(subject)
	if err != nil {
		return EntitlementResult{}, err
	}
	result.PaidPlanActive = paidActive
	if paidActive {
		result.PlanCode = plan.Code
		return result, nil
	}

	result.Allowed = false
	result.Reason = EntitlementPaymentRequired
	result.RequiredPlan = "paid"
	result.UpgradeRequired = true
	return result, nil
}

func (s *EntitlementService) activePaidPlan(subject CreditSubject) (models.Plan, bool, error) {
	if subject.UserID == nil {
		return models.Plan{}, false, nil
	}
	now := s.now()
	var subscription models.UserSubscription
	err := s.db.Preload("Plan").
		Where(
			"user_id = ? AND status = ? AND current_period_start <= ? AND current_period_end > ?",
			*subject.UserID,
			"active",
			now,
			now,
		).
		Order("current_period_end DESC").
		First(&subscription).Error
	if err == nil {
		return subscription.Plan, true, nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return models.Plan{}, false, nil
	}
	return models.Plan{}, false, err
}
