package billing

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"finnri/internal/ai"
	"finnri/internal/models"
)

const (
	LoggedInFreeTrialCredits = 1000
	GuestTrialCredits        = 300
	LoggedInFreeDailyLimit   = 50
	GuestDailyLimit          = 15
	TrialDuration            = 30 * 24 * time.Hour
	AnonymousGuestRetention  = 90 * 24 * time.Hour
)

const (
	GrantSourceFreeTrial          = "free_trial"
	GrantSourceSubscriptionPeriod = "subscription_period"
	LedgerDirectionGrant          = "grant"
	LedgerDirectionDebit          = "debit"
	LedgerDirectionRefund         = "refund"
	LedgerDirectionExpiry         = "expiry"
	ReasonFreeTrial               = "free_trial_grant"
	ReasonGuestTrial              = "guest_trial_grant"
	ReasonSubscriptionPeriod      = "subscription_period_grant"
	ReasonGrantExpiry             = "credit_grant_expiry"
	ReasonReservationDebit        = "ai_credit_reservation"
	ReasonReservationRefund       = "ai_credit_refund"
	ReasonManualAdjustment        = "manual_credit_adjustment"
)

const DefaultCreditCostUSDMicros int64 = 100

const (
	UsageStatusReserved             = "reserved"
	UsageStatusSucceeded            = "succeeded"
	UsageStatusFailedBeforeProvider = "failed_before_provider"
	UsageStatusFailedAfterProvider  = "failed_after_provider"
	UsageStatusCancelled            = "cancelled"
)

const (
	AllowanceAllowed             = "allowed"
	AllowanceSubjectRequired     = "subject_required"
	AllowanceFeatureLocked       = "feature_locked"
	AllowanceGuestNotAllowed     = "guest_not_allowed"
	AllowanceInsufficientCredits = "insufficient_ai_credits"
	AllowanceDailyLimitReached   = "daily_ai_limit_reached"
)

var ErrAllowanceDenied = errors.New("ai credit allowance denied")
var ErrInvalidCreditSubject = errors.New("invalid credit subject")

type CreditService struct {
	db       *gorm.DB
	registry ai.ActionRegistry
	now      func() time.Time
}

func NewCreditService(db *gorm.DB) *CreditService {
	return &CreditService{db: db, registry: ai.DefaultActionRegistry(), now: func() time.Time { return time.Now().UTC() }}
}

func NewCreditServiceWithClock(db *gorm.DB, now func() time.Time) *CreditService {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &CreditService{db: db, registry: ai.DefaultActionRegistry(), now: now}
}

type CreditSubject struct {
	UserID            *uint
	GuestDeviceIDHash string
}

func SubjectForUser(userID uint) CreditSubject {
	return CreditSubject{UserID: &userID}
}

func SubjectForGuestDeviceID(deviceID string) CreditSubject {
	return CreditSubject{GuestDeviceIDHash: HashUsageKey(deviceID)}
}

func SubjectForGuestHash(hash string) CreditSubject {
	return CreditSubject{GuestDeviceIDHash: strings.TrimSpace(hash)}
}

func (s CreditSubject) valid() bool {
	return (s.UserID != nil && *s.UserID != 0) || strings.TrimSpace(s.GuestDeviceIDHash) != ""
}

func (s CreditSubject) isGuest() bool {
	return s.UserID == nil && strings.TrimSpace(s.GuestDeviceIDHash) != ""
}

type AllowanceResult struct {
	Allowed               bool
	Reason                string
	Action                ai.Action
	RequiredCredits       int
	AvailableCredits      int
	DailyLimit            int
	UsedToday             int
	DailyRemaining        int
	PlanCode              string
	PaidPlanActive        bool
	CurrentSubscriptionID *uint
}

type ProviderUsage struct {
	Status                 string
	Provider               string
	Model                  string
	SecondaryProvider      string
	SecondaryModel         string
	FinalCredits           *int
	EstimatedCostUSDMicros *int64
	ActualCostUSDMicros    *int64
	PromptTokens           *int
	CompletionTokens       *int
	TotalTokens            *int
	AudioDurationMs        *int
	AudioBytes             *int64
	InputChars             *int
	ResponseBytes          *int
	ErrorCode              string
	ProviderStartedAt      *time.Time
	FinishedAt             *time.Time
}

type AnonymousGuestRetentionResult struct {
	CreditLedger       int64
	AIUsageEvents      int64
	AIUsageLimitEvents int64
	DailyCreditUsage   int64
	GuestUsageKeys     int64
	CreditGrants       int64
}

func HashUsageKey(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (s *CreditService) CheckAllowance(subject CreditSubject, actionCode ai.ActionCode) (AllowanceResult, error) {
	var result AllowanceResult
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var err error
		result, err = s.checkAllowanceTx(tx, subject, actionCode)
		return err
	})
	return result, err
}

func (s *CreditService) ReserveCredits(subject CreditSubject, actionCode ai.ActionCode, idempotencyKey string) (models.AIUsageEvent, AllowanceResult, error) {
	var event models.AIUsageEvent
	var allowance AllowanceResult
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if !subject.valid() {
			allowance = AllowanceResult{Allowed: false, Reason: AllowanceSubjectRequired}
			return ErrInvalidCreditSubject
		}

		idempotencyKey = strings.TrimSpace(idempotencyKey)
		if idempotencyKey != "" {
			existing, found, err := findUsageEventByIdempotency(tx, subject, idempotencyKey)
			if err != nil {
				return err
			}
			if found {
				event = existing
				action, err := s.registry.Require(ai.ActionCode(existing.ActionCode))
				if err != nil {
					return err
				}
				allowance = AllowanceResult{
					Allowed:          true,
					Reason:           AllowanceAllowed,
					Action:           action,
					RequiredCredits:  existing.ReservedCredits,
					AvailableCredits: existing.ReservedCredits,
					DailyRemaining:   existing.ReservedCredits,
				}
				return nil
			}
		}

		var err error
		allowance, err = s.checkAllowanceTx(tx, subject, actionCode)
		if err != nil {
			return err
		}
		if !allowance.Allowed {
			return ErrAllowanceDenied
		}

		now := s.now()
		requestID, err := generateRequestID()
		if err != nil {
			return err
		}
		action := allowance.Action
		event = models.AIUsageEvent{
			UserID:                 subject.UserID,
			GuestDeviceIDHash:      strings.TrimSpace(subject.GuestDeviceIDHash),
			RequestID:              requestID,
			IdempotencyKey:         idempotencyKey,
			ActionCode:             string(action.Code),
			InputKind:              string(action.InputKind),
			Status:                 UsageStatusReserved,
			EstimatedCredits:       allowance.RequiredCredits,
			ReservedCredits:        allowance.RequiredCredits,
			EstimatedCostUSDMicros: 0,
			StartedAt:              now,
		}
		if err := tx.Create(&event).Error; err != nil {
			return err
		}
		if err := debitAvailableGrants(tx, subject, allowance.RequiredCredits, ReasonReservationDebit, reservationLedgerKey(event.RequestID), &event.ID, now); err != nil {
			return err
		}
		return adjustDailyUsage(tx, subject, now, allowance.RequiredCredits)
	})
	if err != nil {
		if errors.Is(err, ErrAllowanceDenied) {
			s.recordAllowanceDenial(subject, allowance)
		}
		return models.AIUsageEvent{}, allowance, err
	}
	return event, allowance, nil
}

func (s *CreditService) FinalizeUsage(eventID uint, usage ProviderUsage) (models.AIUsageEvent, error) {
	var event models.AIUsageEvent
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&event, eventID).Error; err != nil {
			return err
		}
		if event.Status != UsageStatusReserved {
			return nil
		}

		action, err := s.registry.Require(ai.ActionCode(event.ActionCode))
		if err != nil {
			return err
		}
		subject := subjectFromEvent(event)
		finalCredits := event.ReservedCredits
		if usage.FinalCredits != nil {
			finalCredits = *usage.FinalCredits
		}
		if finalCredits < 0 {
			finalCredits = 0
		}
		if finalCredits > action.MaxCredits {
			finalCredits = action.MaxCredits
		}

		if finalCredits < event.ReservedCredits {
			refund := event.ReservedCredits - finalCredits
			if err := refundEventCredits(tx, event, refund, ReasonReservationRefund); err != nil {
				return err
			}
			if err := adjustDailyUsage(tx, subject, event.StartedAt, -refund); err != nil {
				return err
			}
		} else if finalCredits > event.ReservedCredits {
			extra := finalCredits - event.ReservedCredits
			allowance, err := s.checkAllowanceTx(tx, subject, action.Code)
			if err != nil {
				return err
			}
			if allowance.AvailableCredits >= extra && allowance.DailyRemaining >= extra {
				if err := debitAvailableGrants(tx, subject, extra, ReasonReservationDebit, reservationLedgerKey(event.RequestID)+":final", &event.ID, s.now()); err != nil {
					return err
				}
				if err := adjustDailyUsage(tx, subject, event.StartedAt, extra); err != nil {
					return err
				}
			} else {
				finalCredits = event.ReservedCredits
			}
		}

		status := strings.TrimSpace(usage.Status)
		if status == "" {
			status = UsageStatusSucceeded
		}
		if status != UsageStatusSucceeded && status != UsageStatusFailedAfterProvider {
			return fmt.Errorf("invalid final usage status: %s", status)
		}

		finishedAt := s.now()
		if usage.FinishedAt != nil {
			finishedAt = usage.FinishedAt.UTC()
		}
		updates := map[string]any{
			"status":        status,
			"final_credits": finalCredits,
			"finished_at":   finishedAt,
		}
		copyUsageUpdates(updates, usage)
		if _, ok := updates["estimated_cost_usd_micros"]; !ok {
			estimated, err := estimateUsageCostUSDMicros(tx, event, usage, finalCredits)
			if err != nil {
				return err
			}
			updates["estimated_cost_usd_micros"] = estimated
		}
		if err := tx.Model(&event).Updates(updates).Error; err != nil {
			return err
		}
		return tx.First(&event, eventID).Error
	})
	if err == nil {
		logAIUsageEvent(event)
	}
	return event, err
}

func (s *CreditService) CancelReservation(eventID uint) (models.AIUsageEvent, error) {
	return s.CancelReservationWithStatus(eventID, UsageStatusCancelled)
}

func (s *CreditService) CancelReservationWithStatus(eventID uint, status string) (models.AIUsageEvent, error) {
	var event models.AIUsageEvent
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if status != UsageStatusCancelled && status != UsageStatusFailedBeforeProvider {
			return fmt.Errorf("invalid cancellation status: %s", status)
		}
		if err := tx.First(&event, eventID).Error; err != nil {
			return err
		}
		if event.Status != UsageStatusReserved {
			return nil
		}
		if err := refundEventCredits(tx, event, event.ReservedCredits, ReasonReservationRefund); err != nil {
			return err
		}
		if err := adjustDailyUsage(tx, subjectFromEvent(event), event.StartedAt, -event.ReservedCredits); err != nil {
			return err
		}
		finishedAt := s.now()
		if err := tx.Model(&event).Updates(map[string]any{
			"status":        status,
			"final_credits": 0,
			"finished_at":   finishedAt,
		}).Error; err != nil {
			return err
		}
		return tx.First(&event, eventID).Error
	})
	if err == nil {
		logAIUsageEvent(event)
	}
	return event, err
}

func (s *CreditService) GrantManualAdjustment(subject CreditSubject, credits int, reasonCode, idempotencyKey string, expiresAt *time.Time) (models.CreditGrant, bool, error) {
	if !subject.valid() {
		return models.CreditGrant{}, false, ErrInvalidCreditSubject
	}
	if credits <= 0 {
		return models.CreditGrant{}, false, fmt.Errorf("credits must be positive")
	}
	reasonCode = strings.TrimSpace(reasonCode)
	if reasonCode == "" {
		reasonCode = ReasonManualAdjustment
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" {
		idempotencyKey = fmt.Sprintf("manual_adjustment:%d:%s", s.now().UnixNano(), reasonCode)
	}

	var grant models.CreditGrant
	created := false
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var ledger models.CreditLedger
		if err := tx.Where("idempotency_key = ? AND direction = ?", idempotencyKey, LedgerDirectionGrant).
			First(&ledger).Error; err == nil {
			if ledger.GrantID != nil {
				return tx.First(&grant, *ledger.GrantID).Error
			}
			return nil
		} else if err != gorm.ErrRecordNotFound {
			return err
		}

		now := s.now()
		grant = models.CreditGrant{
			UserID:            subject.UserID,
			GuestDeviceIDHash: strings.TrimSpace(subject.GuestDeviceIDHash),
			Source:            "manual_adjustment",
			CreditsGranted:    credits,
			CreditsRemaining:  credits,
			ValidFrom:         now,
			ExpiresAt:         expiresAt,
		}
		if err := tx.Create(&grant).Error; err != nil {
			return err
		}
		created = true
		return createGrantLedger(tx, grant, LedgerDirectionGrant, credits, reasonCode, idempotencyKey)
	})
	return grant, created, err
}

func (s *CreditService) EnsureLoggedInFreeTrialGrant(userID uint) (models.CreditGrant, bool, error) {
	if userID == 0 {
		return models.CreditGrant{}, false, fmt.Errorf("user id is required")
	}

	var grant models.CreditGrant
	created := false
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var err error
		grant, created, err = s.ensureLoggedInFreeTrialGrantTx(tx, userID)
		return err
	})
	if err != nil {
		if lookupErr := s.db.Where("user_id = ? AND source = ?", userID, GrantSourceFreeTrial).First(&grant).Error; lookupErr == nil {
			return grant, false, nil
		}
		return models.CreditGrant{}, false, err
	}

	return grant, created, nil
}

func (s *CreditService) EnsureLoggedInFreeTrialGrantTx(tx *gorm.DB, userID uint) (models.CreditGrant, bool, error) {
	if userID == 0 {
		return models.CreditGrant{}, false, fmt.Errorf("user id is required")
	}
	return s.ensureLoggedInFreeTrialGrantTx(tx, userID)
}

func (s *CreditService) ensureLoggedInFreeTrialGrantTx(tx *gorm.DB, userID uint) (models.CreditGrant, bool, error) {
	var grant models.CreditGrant
	if err := tx.Where("user_id = ? AND source = ?", userID, GrantSourceFreeTrial).
		First(&grant).Error; err == nil {
		return grant, false, nil
	} else if err != gorm.ErrRecordNotFound {
		return models.CreditGrant{}, false, err
	}

	now := s.now()
	expiresAt := now.Add(TrialDuration)
	grant = models.CreditGrant{
		UserID:           &userID,
		Source:           GrantSourceFreeTrial,
		CreditsGranted:   LoggedInFreeTrialCredits,
		CreditsRemaining: LoggedInFreeTrialCredits,
		ValidFrom:        now,
		ExpiresAt:        &expiresAt,
	}
	if err := tx.Create(&grant).Error; err != nil {
		return models.CreditGrant{}, false, err
	}
	if err := createGrantLedger(tx, grant, LedgerDirectionGrant, LoggedInFreeTrialCredits, ReasonFreeTrial, fmt.Sprintf("free_trial:user:%d", userID)); err != nil {
		return models.CreditGrant{}, false, err
	}
	return grant, true, nil
}

func (s *CreditService) EnsureGuestTrialGrant(deviceID, ip string) (models.CreditGrant, bool, error) {
	deviceHash := HashUsageKey(deviceID)
	if deviceHash == "" {
		return models.CreditGrant{}, false, nil
	}
	ipHash := HashUsageKey(ip)

	var grant models.CreditGrant
	created := false
	err := s.db.Transaction(func(tx *gorm.DB) error {
		now := s.now()
		var guestKey models.GuestUsageKey
		if err := tx.Where("guest_device_id_hash = ?", deviceHash).First(&guestKey).Error; err == nil {
			updates := map[string]any{"last_seen_at": now}
			if ipHash != "" {
				updates["ip_hash"] = ipHash
			}
			if err := tx.Model(&guestKey).Updates(updates).Error; err != nil {
				return err
			}
		} else if err == gorm.ErrRecordNotFound {
			guestKey = models.GuestUsageKey{
				GuestDeviceIDHash: deviceHash,
				IPHash:            ipHash,
				FirstSeenAt:       now,
				LastSeenAt:        now,
			}
			if err := tx.Create(&guestKey).Error; err != nil {
				return err
			}
		} else {
			return err
		}

		if err := tx.Where("guest_device_id_hash = ? AND source = ?", deviceHash, GrantSourceFreeTrial).
			First(&grant).Error; err == nil {
			if guestKey.TrialGrantID == nil {
				if err := tx.Model(&guestKey).Update("trial_grant_id", grant.ID).Error; err != nil {
					return err
				}
			}
			return nil
		} else if err != gorm.ErrRecordNotFound {
			return err
		}

		expiresAt := now.Add(TrialDuration)
		grant = models.CreditGrant{
			GuestDeviceIDHash: deviceHash,
			Source:            GrantSourceFreeTrial,
			CreditsGranted:    GuestTrialCredits,
			CreditsRemaining:  GuestTrialCredits,
			ValidFrom:         now,
			ExpiresAt:         &expiresAt,
		}
		if err := tx.Create(&grant).Error; err != nil {
			return err
		}
		if err := tx.Model(&guestKey).Update("trial_grant_id", grant.ID).Error; err != nil {
			return err
		}
		created = true
		return createGrantLedger(tx, grant, LedgerDirectionGrant, GuestTrialCredits, ReasonGuestTrial, "free_trial:guest:"+deviceHash)
	})
	if err != nil {
		if lookupErr := s.db.Where("guest_device_id_hash = ? AND source = ?", deviceHash, GrantSourceFreeTrial).First(&grant).Error; lookupErr == nil {
			return grant, false, nil
		}
		return models.CreditGrant{}, false, err
	}

	return grant, created, nil
}

func (s *CreditService) PromoteGuestDeviceToUser(tx *gorm.DB, userID uint, deviceID string) error {
	if userID == 0 {
		return fmt.Errorf("user id is required")
	}
	deviceHash := HashUsageKey(deviceID)
	if deviceHash == "" {
		return nil
	}

	var guestGrant models.CreditGrant
	if err := tx.Where("guest_device_id_hash = ? AND source = ?", deviceHash, GrantSourceFreeTrial).
		First(&guestGrant).Error; err == nil {
		usedCredits := guestGrant.CreditsGranted - guestGrant.CreditsRemaining
		if usedCredits < 0 {
			usedCredits = 0
		}
		targetGranted := guestGrant.CreditsGranted
		if targetGranted < LoggedInFreeTrialCredits {
			targetGranted = LoggedInFreeTrialCredits
		}
		targetRemaining := targetGranted - usedCredits
		if targetRemaining < 0 {
			targetRemaining = 0
		}
		topUpCredits := targetRemaining - guestGrant.CreditsRemaining
		expiresAt := s.now().Add(TrialDuration)
		updates := map[string]any{
			"user_id":              userID,
			"guest_device_id_hash": "",
			"credits_granted":      targetGranted,
			"credits_remaining":    targetRemaining,
			"expires_at":           expiresAt,
		}
		if err := tx.Model(&guestGrant).Updates(updates).Error; err != nil {
			return err
		}
		guestGrant.UserID = &userID
		guestGrant.GuestDeviceIDHash = ""
		guestGrant.CreditsGranted = targetGranted
		guestGrant.CreditsRemaining = targetRemaining
		guestGrant.ExpiresAt = &expiresAt
		if topUpCredits > 0 {
			if err := createGrantLedger(
				tx,
				guestGrant,
				LedgerDirectionGrant,
				topUpCredits,
				ReasonFreeTrial,
				fmt.Sprintf("free_trial:upgrade:user:%d:guest:%s", userID, deviceHash),
			); err != nil {
				return err
			}
		}
	} else if err != gorm.ErrRecordNotFound {
		return err
	}

	if err := tx.Model(&models.CreditGrant{}).
		Where("guest_device_id_hash = ?", deviceHash).
		Updates(map[string]any{"user_id": userID, "guest_device_id_hash": ""}).Error; err != nil {
		return err
	}
	if err := tx.Model(&models.CreditLedger{}).
		Where("guest_device_id_hash = ?", deviceHash).
		Updates(map[string]any{"user_id": userID, "guest_device_id_hash": ""}).Error; err != nil {
		return err
	}
	if err := tx.Model(&models.AIUsageEvent{}).
		Where("guest_device_id_hash = ?", deviceHash).
		Updates(map[string]any{"user_id": userID, "guest_device_id_hash": ""}).Error; err != nil {
		return err
	}
	if err := promoteGuestDailyUsageToUser(tx, userID, deviceHash); err != nil {
		return err
	}
	if err := tx.Model(&models.AIUsageLimitEvent{}).
		Where("guest_device_id_hash = ?", deviceHash).
		Updates(map[string]any{"user_id": userID, "guest_device_id_hash": ""}).Error; err != nil {
		return err
	}
	if err := tx.Model(&models.AIAbuseBlock{}).
		Where("guest_device_id_hash = ?", deviceHash).
		Updates(map[string]any{"user_id": userID, "guest_device_id_hash": ""}).Error; err != nil {
		return err
	}
	return nil
}

func promoteGuestDailyUsageToUser(tx *gorm.DB, userID uint, deviceHash string) error {
	var guestRows []models.DailyCreditUsage
	if err := tx.Where("guest_device_id_hash = ?", deviceHash).Find(&guestRows).Error; err != nil {
		return err
	}
	for _, guestRow := range guestRows {
		var userRow models.DailyCreditUsage
		if err := tx.Where("user_id = ? AND usage_date = ?", userID, guestRow.UsageDate).First(&userRow).Error; err == nil {
			if err := tx.Model(&userRow).Update("credits_used", userRow.CreditsUsed+guestRow.CreditsUsed).Error; err != nil {
				return err
			}
			if err := tx.Delete(&guestRow).Error; err != nil {
				return err
			}
		} else if err == gorm.ErrRecordNotFound {
			if err := tx.Model(&guestRow).
				Updates(map[string]any{"user_id": userID, "guest_device_id_hash": ""}).Error; err != nil {
				return err
			}
		} else {
			return err
		}
	}
	return nil
}

func (s *CreditService) GrantSubscriptionPeriod(subscription models.UserSubscription, credits int, validFrom, expiresAt time.Time) (models.CreditGrant, bool, error) {
	if subscription.ID == 0 {
		return models.CreditGrant{}, false, fmt.Errorf("subscription id is required")
	}
	if subscription.UserID == 0 {
		return models.CreditGrant{}, false, fmt.Errorf("subscription user id is required")
	}
	if credits <= 0 {
		return models.CreditGrant{}, false, fmt.Errorf("credits must be positive")
	}
	if !expiresAt.After(validFrom) {
		return models.CreditGrant{}, false, fmt.Errorf("subscription grant expiry must be after valid_from")
	}

	var grant models.CreditGrant
	created := false
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("subscription_id = ? AND valid_from = ? AND source = ?", subscription.ID, validFrom, GrantSourceSubscriptionPeriod).
			First(&grant).Error; err == nil {
			return nil
		} else if err != gorm.ErrRecordNotFound {
			return err
		}

		expiresAtCopy := expiresAt
		grant = models.CreditGrant{
			UserID:           &subscription.UserID,
			Source:           GrantSourceSubscriptionPeriod,
			CreditsGranted:   credits,
			CreditsRemaining: credits,
			ValidFrom:        validFrom,
			ExpiresAt:        &expiresAtCopy,
			SubscriptionID:   &subscription.ID,
		}
		if err := tx.Create(&grant).Error; err != nil {
			return err
		}
		created = true
		return createGrantLedger(tx, grant, LedgerDirectionGrant, credits, ReasonSubscriptionPeriod, fmt.Sprintf("subscription_period:%d:%s", subscription.ID, validFrom.UTC().Format(time.RFC3339)))
	})
	if err != nil {
		if lookupErr := s.db.Where("subscription_id = ? AND valid_from = ? AND source = ?", subscription.ID, validFrom, GrantSourceSubscriptionPeriod).
			First(&grant).Error; lookupErr == nil {
			return grant, false, nil
		}
		return models.CreditGrant{}, false, err
	}

	return grant, created, nil
}

func (s *CreditService) ExpireCredits() (int, error) {
	now := s.now()
	expiredCount := 0
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var grants []models.CreditGrant
		if err := tx.Where("expires_at IS NOT NULL AND expires_at <= ? AND credits_remaining > 0", now).Find(&grants).Error; err != nil {
			return err
		}
		for _, grant := range grants {
			remaining := grant.CreditsRemaining
			if remaining <= 0 {
				continue
			}
			result := tx.Model(&models.CreditGrant{}).Where("id = ? AND credits_remaining = ?", grant.ID, remaining).
				Update("credits_remaining", 0)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 0 {
				continue
			}
			grant.CreditsRemaining = 0
			if err := createGrantLedger(tx, grant, LedgerDirectionExpiry, remaining, ReasonGrantExpiry, fmt.Sprintf("expiry:grant:%d", grant.ID)); err != nil {
				return err
			}
			expiredCount++
		}
		return nil
	})
	return expiredCount, err
}

func (s *CreditService) PurgeAnonymousGuestUsage(retention time.Duration) (AnonymousGuestRetentionResult, error) {
	if retention <= 0 {
		return AnonymousGuestRetentionResult{}, fmt.Errorf("retention must be positive")
	}
	return s.PurgeAnonymousGuestUsageBefore(s.now().Add(-retention))
}

func (s *CreditService) PurgeAnonymousGuestUsageBefore(cutoff time.Time) (AnonymousGuestRetentionResult, error) {
	cutoff = cutoff.UTC()
	cutoffDate := cutoff.Format("2006-01-02")
	result := AnonymousGuestRetentionResult{}

	err := s.db.Transaction(func(tx *gorm.DB) error {
		var oldGuestHashes []string
		if err := tx.Model(&models.GuestUsageKey{}).
			Where("last_seen_at < ?", cutoff).
			Pluck("guest_device_id_hash", &oldGuestHashes).Error; err != nil {
			return err
		}

		guestLedger := tx.Where("user_id IS NULL AND guest_device_id_hash <> '' AND created_at < ?", cutoff)
		if len(oldGuestHashes) > 0 {
			guestLedger = guestLedger.Or("user_id IS NULL AND guest_device_id_hash IN ?", oldGuestHashes)
		}
		deleted, err := deleteRows(guestLedger, &models.CreditLedger{})
		if err != nil {
			return fmt.Errorf("purge anonymous guest credit ledger: %w", err)
		}
		result.CreditLedger = deleted

		guestUsage := tx.Where("user_id IS NULL AND guest_device_id_hash <> '' AND started_at < ?", cutoff)
		if len(oldGuestHashes) > 0 {
			guestUsage = guestUsage.Or("user_id IS NULL AND guest_device_id_hash IN ?", oldGuestHashes)
		}
		deleted, err = deleteRows(guestUsage, &models.AIUsageEvent{})
		if err != nil {
			return fmt.Errorf("purge anonymous guest ai usage events: %w", err)
		}
		result.AIUsageEvents = deleted

		guestLimitEvents := tx.Where("user_id IS NULL AND guest_device_id_hash <> '' AND created_at < ?", cutoff)
		if len(oldGuestHashes) > 0 {
			guestLimitEvents = guestLimitEvents.Or("user_id IS NULL AND guest_device_id_hash IN ?", oldGuestHashes)
		}
		deleted, err = deleteRows(guestLimitEvents, &models.AIUsageLimitEvent{})
		if err != nil {
			return fmt.Errorf("purge anonymous guest ai usage limit events: %w", err)
		}
		result.AIUsageLimitEvents = deleted

		guestDailyUsage := tx.Where("user_id IS NULL AND guest_device_id_hash <> '' AND usage_date < ?", cutoffDate)
		if len(oldGuestHashes) > 0 {
			guestDailyUsage = guestDailyUsage.Or("user_id IS NULL AND guest_device_id_hash IN ?", oldGuestHashes)
		}
		deleted, err = deleteRows(guestDailyUsage, &models.DailyCreditUsage{})
		if err != nil {
			return fmt.Errorf("purge anonymous guest daily usage: %w", err)
		}
		result.DailyCreditUsage = deleted

		guestKeys := tx.Where("last_seen_at < ?", cutoff)
		deleted, err = deleteRows(guestKeys, &models.GuestUsageKey{})
		if err != nil {
			return fmt.Errorf("purge anonymous guest usage keys: %w", err)
		}
		result.GuestUsageKeys = deleted

		guestGrants := tx.Where("user_id IS NULL AND guest_device_id_hash <> '' AND expires_at IS NOT NULL AND expires_at < ?", cutoff)
		if len(oldGuestHashes) > 0 {
			guestGrants = guestGrants.Or("user_id IS NULL AND guest_device_id_hash IN ?", oldGuestHashes)
		}
		deleted, err = deleteRows(guestGrants, &models.CreditGrant{})
		if err != nil {
			return fmt.Errorf("purge anonymous guest credit grants: %w", err)
		}
		result.CreditGrants = deleted

		return nil
	})
	return result, err
}

func (s *CreditService) checkAllowanceTx(tx *gorm.DB, subject CreditSubject, actionCode ai.ActionCode) (AllowanceResult, error) {
	if !subject.valid() {
		return AllowanceResult{Allowed: false, Reason: AllowanceSubjectRequired}, nil
	}
	action, err := s.registry.RequireImplemented(actionCode)
	if err != nil {
		return AllowanceResult{}, err
	}

	now := s.now()
	result := AllowanceResult{
		Action:          action,
		RequiredCredits: action.DefaultCredits,
		Reason:          AllowanceAllowed,
	}

	if subject.isGuest() && !action.GuestAllowed {
		result.Reason = AllowanceGuestNotAllowed
		return result, nil
	}

	plan, subscription, paidActive, err := activePlanForSubject(tx, subject, now)
	if err != nil {
		return AllowanceResult{}, err
	}
	result.PaidPlanActive = paidActive
	if paidActive {
		result.PlanCode = plan.Code
		result.CurrentSubscriptionID = &subscription.ID
	}
	if action.PaidPlanRequired && !paidActive {
		result.Reason = AllowanceFeatureLocked
		return result, nil
	}

	dailyLimit := dailyLimitForSubject(subject, plan, paidActive)
	usedToday, err := dailyCreditsUsed(tx, subject, now)
	if err != nil {
		return AllowanceResult{}, err
	}
	available, err := availableCredits(tx, subject, now)
	if err != nil {
		return AllowanceResult{}, err
	}

	result.DailyLimit = dailyLimit
	result.UsedToday = usedToday
	result.AvailableCredits = available
	result.DailyRemaining = dailyLimit - usedToday
	if result.DailyRemaining < 0 {
		result.DailyRemaining = 0
	}

	if dailyLimit > 0 && result.DailyRemaining < result.RequiredCredits {
		result.Reason = AllowanceDailyLimitReached
		return result, nil
	}
	if available < result.RequiredCredits {
		result.Reason = AllowanceInsufficientCredits
		return result, nil
	}

	result.Allowed = true
	return result, nil
}

func activePlanForSubject(tx *gorm.DB, subject CreditSubject, now time.Time) (models.Plan, models.UserSubscription, bool, error) {
	if subject.UserID == nil {
		return models.Plan{}, models.UserSubscription{}, false, nil
	}
	var subscription models.UserSubscription
	err := tx.Preload("Plan").
		Where(
			"user_id = ? AND status IN ? AND current_period_start <= ? AND current_period_end > ?",
			*subject.UserID,
			[]string{"trialing", "active"},
			now,
			now,
		).
		Order("current_period_end DESC").
		First(&subscription).Error
	if err == gorm.ErrRecordNotFound {
		return models.Plan{}, models.UserSubscription{}, false, nil
	}
	if err != nil {
		return models.Plan{}, models.UserSubscription{}, false, err
	}
	return subscription.Plan, subscription, true, nil
}

func dailyLimitForSubject(subject CreditSubject, plan models.Plan, paidActive bool) int {
	if paidActive && plan.DailyCreditLimit > 0 {
		return plan.DailyCreditLimit
	}
	if subject.isGuest() {
		return GuestDailyLimit
	}
	return LoggedInFreeDailyLimit
}

func availableCredits(tx *gorm.DB, subject CreditSubject, now time.Time) (int, error) {
	query := tx.Model(&models.CreditGrant{}).
		Where("valid_from <= ? AND (expires_at IS NULL OR expires_at > ?) AND credits_remaining > 0", now, now)
	query = scopeGrants(query, subject)

	var total int
	if err := query.Select("COALESCE(SUM(credits_remaining), 0)").Scan(&total).Error; err != nil {
		return 0, err
	}
	return total, nil
}

func dailyCreditsUsed(tx *gorm.DB, subject CreditSubject, now time.Time) (int, error) {
	var usage models.DailyCreditUsage
	query := tx.Where("usage_date = ?", now.Format("2006-01-02"))
	query = scopeDailyUsage(query, subject)
	if err := query.First(&usage).Error; err == nil {
		return usage.CreditsUsed, nil
	} else if err != gorm.ErrRecordNotFound {
		return 0, err
	}
	return 0, nil
}

func debitAvailableGrants(tx *gorm.DB, subject CreditSubject, credits int, reasonCode, idempotencyKey string, eventID *uint, now time.Time) error {
	if credits <= 0 {
		return nil
	}
	var grants []models.CreditGrant
	query := tx.Where("valid_from <= ? AND (expires_at IS NULL OR expires_at > ?) AND credits_remaining > 0", now, now)
	query = scopeGrants(query, subject)
	if tx.Dialector.Name() != "sqlite" {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	if err := query.
		Order("CASE WHEN expires_at IS NULL THEN 1 ELSE 0 END").
		Order("expires_at ASC").
		Order("valid_from ASC").
		Order("id ASC").
		Find(&grants).Error; err != nil {
		return err
	}

	remaining := credits
	for _, grant := range grants {
		if remaining <= 0 {
			break
		}
		take := grant.CreditsRemaining
		if take > remaining {
			take = remaining
		}
		result := tx.Model(&models.CreditGrant{}).
			Where("id = ? AND credits_remaining >= ?", grant.ID, take).
			Update("credits_remaining", gorm.Expr("credits_remaining - ?", take))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return fmt.Errorf("credit grant balance changed during reservation")
		}
		balanceAfter := grant.CreditsRemaining - take
		if err := createLedger(tx, models.CreditLedger{
			UserID:            grant.UserID,
			GuestDeviceIDHash: grant.GuestDeviceIDHash,
			GrantID:           &grant.ID,
			Direction:         LedgerDirectionDebit,
			Credits:           take,
			BalanceAfter:      balanceAfter,
			ReasonCode:        reasonCode,
			IdempotencyKey:    idempotencyKey,
			AIUsageEventID:    eventID,
		}); err != nil {
			return err
		}
		remaining -= take
	}
	if remaining > 0 {
		return fmt.Errorf("insufficient credits while reserving")
	}
	return nil
}

func refundEventCredits(tx *gorm.DB, event models.AIUsageEvent, credits int, reasonCode string) error {
	if credits <= 0 {
		return nil
	}
	var debits []models.CreditLedger
	if err := tx.Where("ai_usage_event_id = ? AND direction = ?", event.ID, LedgerDirectionDebit).
		Order("id DESC").
		Find(&debits).Error; err != nil {
		return err
	}
	remaining := credits
	for _, debit := range debits {
		if remaining <= 0 {
			break
		}
		refunded, err := refundedCreditsForDebit(tx, debit)
		if err != nil {
			return err
		}
		refundable := debit.Credits - refunded
		if refundable <= 0 {
			continue
		}
		refund := refundable
		if refund > remaining {
			refund = remaining
		}

		var grant models.CreditGrant
		if debit.GrantID == nil {
			return fmt.Errorf("debit ledger missing grant id")
		}
		if err := tx.First(&grant, *debit.GrantID).Error; err != nil {
			return err
		}
		newBalance := grant.CreditsRemaining + refund
		if newBalance > grant.CreditsGranted {
			newBalance = grant.CreditsGranted
			refund = newBalance - grant.CreditsRemaining
		}
		if refund <= 0 {
			continue
		}
		if err := tx.Model(&models.CreditGrant{}).Where("id = ?", grant.ID).
			Update("credits_remaining", newBalance).Error; err != nil {
			return err
		}
		if err := createLedger(tx, models.CreditLedger{
			UserID:            grant.UserID,
			GuestDeviceIDHash: grant.GuestDeviceIDHash,
			GrantID:           &grant.ID,
			Direction:         LedgerDirectionRefund,
			Credits:           refund,
			BalanceAfter:      newBalance,
			ReasonCode:        reasonCode,
			IdempotencyKey:    debit.IdempotencyKey + ":refund",
			AIUsageEventID:    &event.ID,
		}); err != nil {
			return err
		}
		remaining -= refund
	}
	if remaining > 0 {
		return fmt.Errorf("unable to refund all reserved credits")
	}
	return nil
}

func refundedCreditsForDebit(tx *gorm.DB, debit models.CreditLedger) (int, error) {
	if debit.GrantID == nil {
		return 0, nil
	}
	var total int
	err := tx.Model(&models.CreditLedger{}).
		Where(
			"ai_usage_event_id = ? AND grant_id = ? AND direction = ? AND idempotency_key = ?",
			debit.AIUsageEventID,
			*debit.GrantID,
			LedgerDirectionRefund,
			debit.IdempotencyKey+":refund",
		).
		Select("COALESCE(SUM(credits), 0)").
		Scan(&total).Error
	return total, err
}

func adjustDailyUsage(tx *gorm.DB, subject CreditSubject, at time.Time, delta int) error {
	if delta == 0 {
		return nil
	}
	usageDate := at.UTC().Format("2006-01-02")
	var usage models.DailyCreditUsage
	query := tx.Where("usage_date = ?", usageDate)
	query = scopeDailyUsage(query, subject)
	if err := query.First(&usage).Error; err == nil {
		next := usage.CreditsUsed + delta
		if next < 0 {
			next = 0
		}
		return tx.Model(&usage).Update("credits_used", next).Error
	} else if err != gorm.ErrRecordNotFound {
		return err
	}
	if delta < 0 {
		delta = 0
	}
	usage = models.DailyCreditUsage{
		UserID:            subject.UserID,
		GuestDeviceIDHash: strings.TrimSpace(subject.GuestDeviceIDHash),
		UsageDate:         usageDate,
		CreditsUsed:       delta,
	}
	return tx.Create(&usage).Error
}

func findUsageEventByIdempotency(tx *gorm.DB, subject CreditSubject, idempotencyKey string) (models.AIUsageEvent, bool, error) {
	var event models.AIUsageEvent
	query := tx.Where("idempotency_key = ?", idempotencyKey)
	query = scopeUsageEvents(query, subject)
	if err := query.First(&event).Error; err == nil {
		return event, true, nil
	} else if err != gorm.ErrRecordNotFound {
		return models.AIUsageEvent{}, false, err
	}
	return models.AIUsageEvent{}, false, nil
}

func scopeGrants(query *gorm.DB, subject CreditSubject) *gorm.DB {
	if subject.UserID != nil {
		return query.Where("user_id = ?", *subject.UserID)
	}
	return query.Where("guest_device_id_hash = ?", strings.TrimSpace(subject.GuestDeviceIDHash))
}

func scopeDailyUsage(query *gorm.DB, subject CreditSubject) *gorm.DB {
	if subject.UserID != nil {
		return query.Where("user_id = ?", *subject.UserID)
	}
	return query.Where("guest_device_id_hash = ?", strings.TrimSpace(subject.GuestDeviceIDHash))
}

func scopeUsageEvents(query *gorm.DB, subject CreditSubject) *gorm.DB {
	if subject.UserID != nil {
		return query.Where("user_id = ?", *subject.UserID)
	}
	return query.Where("guest_device_id_hash = ?", strings.TrimSpace(subject.GuestDeviceIDHash))
}

func subjectFromEvent(event models.AIUsageEvent) CreditSubject {
	return CreditSubject{UserID: event.UserID, GuestDeviceIDHash: event.GuestDeviceIDHash}
}

func generateRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "ai_" + hex.EncodeToString(b[:]), nil
}

func reservationLedgerKey(requestID string) string {
	return "reservation:" + requestID
}

func copyUsageUpdates(updates map[string]any, usage ProviderUsage) {
	stringUpdates := map[string]string{
		"provider":           usage.Provider,
		"model":              usage.Model,
		"secondary_provider": usage.SecondaryProvider,
		"secondary_model":    usage.SecondaryModel,
		"error_code":         usage.ErrorCode,
	}
	for key, value := range stringUpdates {
		if strings.TrimSpace(value) != "" {
			updates[key] = strings.TrimSpace(value)
		}
	}
	if usage.EstimatedCostUSDMicros != nil {
		updates["estimated_cost_usd_micros"] = maxInt64(0, *usage.EstimatedCostUSDMicros)
	}
	if usage.ActualCostUSDMicros != nil {
		updates["actual_cost_usd_micros"] = maxInt64(0, *usage.ActualCostUSDMicros)
	}
	if usage.PromptTokens != nil {
		updates["prompt_tokens"] = maxInt(0, *usage.PromptTokens)
	}
	if usage.CompletionTokens != nil {
		updates["completion_tokens"] = maxInt(0, *usage.CompletionTokens)
	}
	if usage.TotalTokens != nil {
		updates["total_tokens"] = maxInt(0, *usage.TotalTokens)
	}
	if usage.AudioDurationMs != nil {
		updates["audio_duration_ms"] = maxInt(0, *usage.AudioDurationMs)
	}
	if usage.AudioBytes != nil {
		updates["audio_bytes"] = maxInt64(0, *usage.AudioBytes)
	}
	if usage.InputChars != nil {
		updates["input_chars"] = maxInt(0, *usage.InputChars)
	}
	if usage.ResponseBytes != nil {
		updates["response_bytes"] = maxInt(0, *usage.ResponseBytes)
	}
	if usage.ProviderStartedAt != nil {
		updates["provider_started_at"] = usage.ProviderStartedAt.UTC()
	}
}

func maxInt(minimum, value int) int {
	if value < minimum {
		return minimum
	}
	return value
}

func maxInt64(minimum, value int64) int64 {
	if value < minimum {
		return minimum
	}
	return value
}

func createGrantLedger(tx *gorm.DB, grant models.CreditGrant, direction string, credits int, reasonCode, idempotencyKey string) error {
	ledger := models.CreditLedger{
		UserID:            grant.UserID,
		GuestDeviceIDHash: grant.GuestDeviceIDHash,
		GrantID:           &grant.ID,
		Direction:         direction,
		Credits:           credits,
		BalanceAfter:      grant.CreditsRemaining,
		ReasonCode:        reasonCode,
		IdempotencyKey:    idempotencyKey,
	}
	if direction == LedgerDirectionExpiry {
		ledger.BalanceAfter = 0
	}
	return createLedger(tx, ledger)
}

func createLedger(tx *gorm.DB, ledger models.CreditLedger) error {
	if ledger.Credits <= 0 {
		return fmt.Errorf("ledger credits must be positive")
	}
	if ledger.BalanceAfter < 0 {
		return fmt.Errorf("ledger balance cannot be negative")
	}
	return tx.Create(&ledger).Error
}

func deleteRows(tx *gorm.DB, model any) (int64, error) {
	result := tx.Delete(model)
	if result.Error != nil {
		return 0, result.Error
	}
	return result.RowsAffected, nil
}

func (s *CreditService) recordAllowanceDenial(subject CreditSubject, allowance AllowanceResult) {
	if !subject.valid() {
		return
	}
	event := models.AIUsageLimitEvent{
		UserID:            subject.UserID,
		GuestDeviceIDHash: strings.TrimSpace(subject.GuestDeviceIDHash),
		ActionCode:        string(allowance.Action.Code),
		Reason:            allowance.Reason,
		RequiredCredits:   allowance.RequiredCredits,
		AvailableCredits:  allowance.AvailableCredits,
		DailyLimit:        allowance.DailyLimit,
		UsedToday:         allowance.UsedToday,
		DailyRemaining:    allowance.DailyRemaining,
		PlanCode:          allowance.PlanCode,
		CreatedAt:         s.now(),
	}
	if strings.TrimSpace(event.Reason) == "" {
		event.Reason = AllowanceSubjectRequired
	}
	if err := s.db.Create(&event).Error; err != nil {
		log.Printf("ai_usage_limit_record_failed reason=%s err=%v", event.Reason, err)
		return
	}
	logStructured("ai_usage_limit", map[string]any{
		"id":                   event.ID,
		"user_id":              event.UserID,
		"guest_device_id_hash": redactHash(event.GuestDeviceIDHash),
		"action_code":          event.ActionCode,
		"reason":               event.Reason,
		"required_credits":     event.RequiredCredits,
		"available_credits":    event.AvailableCredits,
		"daily_limit":          event.DailyLimit,
		"used_today":           event.UsedToday,
		"daily_remaining":      event.DailyRemaining,
		"plan_code":            event.PlanCode,
	})
}

func estimateUsageCostUSDMicros(tx *gorm.DB, event models.AIUsageEvent, usage ProviderUsage, finalCredits int) (int64, error) {
	total := int64(0)
	if usage.PromptTokens != nil || usage.CompletionTokens != nil {
		pricing, found, err := findModelPricing(tx, usage.Provider, usage.Model, "llm")
		if err != nil {
			return 0, err
		}
		if found {
			if usage.PromptTokens != nil {
				total += int64(maxInt(0, *usage.PromptTokens)) * pricing.InputTokenUSDMicros
			}
			if usage.CompletionTokens != nil {
				total += int64(maxInt(0, *usage.CompletionTokens)) * pricing.OutputTokenUSDMicros
			}
			total += pricing.RequestUSDMicros
		}
	}
	if usage.AudioDurationMs != nil {
		pricing, found, err := findModelPricing(tx, usage.SecondaryProvider, usage.SecondaryModel, "transcription")
		if err != nil {
			return 0, err
		}
		if found {
			minutes := float64(maxInt(0, *usage.AudioDurationMs)) / 60000
			total += int64(minutes * float64(pricing.AudioMinuteUSDMicros))
			total += pricing.RequestUSDMicros
		}
	}
	if total > 0 {
		return total, nil
	}

	creditCost := DefaultCreditCostUSDMicros
	pricing, found, err := findModelPricing(tx, "", "", "credit_fallback")
	if err != nil {
		return 0, err
	}
	if found && pricing.CreditUSDMicros > 0 {
		creditCost = pricing.CreditUSDMicros
	}
	if finalCredits <= 0 {
		finalCredits = event.ReservedCredits
	}
	return int64(maxInt(0, finalCredits)) * creditCost, nil
}

func findModelPricing(tx *gorm.DB, provider, model, operation string) (models.AIModelPricing, bool, error) {
	var pricing models.AIModelPricing
	query := tx.Where("operation = ? AND active = ?", operation, true)
	if strings.TrimSpace(provider) != "" {
		query = query.Where("provider = ?", strings.TrimSpace(provider))
	} else {
		query = query.Where("provider = ''")
	}
	if strings.TrimSpace(model) != "" {
		query = query.Where("model = ?", strings.TrimSpace(model))
	} else {
		query = query.Where("model = ''")
	}
	err := query.Order("updated_at DESC").First(&pricing).Error
	if err == nil {
		return pricing, true, nil
	}
	if err == gorm.ErrRecordNotFound {
		return models.AIModelPricing{}, false, nil
	}
	return models.AIModelPricing{}, false, err
}

func logAIUsageEvent(event models.AIUsageEvent) {
	logStructured("ai_usage_event", map[string]any{
		"id":                        event.ID,
		"request_id":                event.RequestID,
		"user_id":                   event.UserID,
		"guest_device_id_hash":      redactHash(event.GuestDeviceIDHash),
		"action_code":               event.ActionCode,
		"input_kind":                event.InputKind,
		"status":                    event.Status,
		"provider":                  event.Provider,
		"model":                     event.Model,
		"secondary_provider":        event.SecondaryProvider,
		"secondary_model":           event.SecondaryModel,
		"reserved_credits":          event.ReservedCredits,
		"final_credits":             event.FinalCredits,
		"estimated_cost_usd_micros": event.EstimatedCostUSDMicros,
		"actual_cost_usd_micros":    event.ActualCostUSDMicros,
		"prompt_tokens":             event.PromptTokens,
		"completion_tokens":         event.CompletionTokens,
		"total_tokens":              event.TotalTokens,
		"audio_duration_ms":         event.AudioDurationMs,
		"audio_bytes":               event.AudioBytes,
		"input_chars":               event.InputChars,
		"response_bytes":            event.ResponseBytes,
		"error_code":                event.ErrorCode,
		"started_at":                event.StartedAt,
		"finished_at":               event.FinishedAt,
	})
}

func logStructured(event string, fields map[string]any) {
	fields["event"] = event
	encoded, err := json.Marshal(fields)
	if err != nil {
		log.Printf("%s %+v", event, fields)
		return
	}
	log.Print(string(encoded))
}

func redactHash(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}
