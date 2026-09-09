package http

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"finnri/internal/ai"
	"finnri/internal/billing"
	"finnri/internal/config"
	"finnri/internal/database"
	"finnri/internal/models"
)

type providerCircuitBreaker struct {
	mu                  sync.Mutex
	consecutiveFailures int
	openUntil           time.Time
}

func (s *Server) circuitBreaker() *providerCircuitBreaker {
	s.providerCircuitMu.Lock()
	defer s.providerCircuitMu.Unlock()
	if s.providerCircuit == nil {
		s.providerCircuit = &providerCircuitBreaker{}
	}
	return s.providerCircuit
}

func (b *providerCircuitBreaker) allow(now time.Time) (bool, time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openUntil.After(now) {
		return false, b.openUntil
	}
	if !b.openUntil.IsZero() {
		b.openUntil = time.Time{}
		b.consecutiveFailures = 0
	}
	return true, time.Time{}
}

func (b *providerCircuitBreaker) recordFailure(now time.Time, threshold int, openFor time.Duration) {
	if threshold <= 0 || openFor <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutiveFailures++
	if b.consecutiveFailures >= threshold {
		b.openUntil = now.Add(openFor)
	}
}

func (b *providerCircuitBreaker) recordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutiveFailures = 0
	b.openUntil = time.Time{}
}

func (s *Server) rejectIfAIParseDisabled(c *gin.Context) bool {
	if s.cfg.AIParseDisabled {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "ai_parse_disabled"})
		return true
	}
	return false
}

func (s *Server) rejectIfProviderCircuitOpen(c *gin.Context) bool {
	if s.cfg.AIProviderFailureThreshold <= 0 || s.cfg.AIProviderCircuitBreakerSeconds <= 0 {
		return false
	}
	now := time.Now().UTC()
	allowed, retryAt := s.circuitBreaker().allow(now)
	if allowed {
		return false
	}
	retryAfter := int(time.Until(retryAt).Seconds())
	if retryAfter < 1 {
		retryAfter = 1
	}
	c.Header("Retry-After", stringInt(retryAfter))
	c.JSON(http.StatusServiceUnavailable, gin.H{"error": "ai_provider_circuit_open", "retry_at": retryAt})
	return true
}

func (s *Server) recordAIProviderFailure() {
	if s.cfg.AIProviderFailureThreshold <= 0 || s.cfg.AIProviderCircuitBreakerSeconds <= 0 {
		return
	}
	s.circuitBreaker().recordFailure(
		time.Now().UTC(),
		s.cfg.AIProviderFailureThreshold,
		time.Duration(s.cfg.AIProviderCircuitBreakerSeconds)*time.Second,
	)
}

func (s *Server) recordAIProviderSuccess() {
	if s.providerCircuit == nil {
		return
	}
	s.providerCircuit.recordSuccess()
}

func (s *Server) rejectIfAIAbuseBlocked(c *gin.Context, subject billing.CreditSubject) bool {
	block, found, err := activeAIAbuseBlock(subject)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "ai_abuse_block_lookup_failed"})
		return true
	}
	if !found {
		return false
	}
	c.JSON(http.StatusForbidden, gin.H{
		"error":       "ai_access_blocked",
		"reason_code": block.ReasonCode,
		"expires_at":  block.ExpiresAt,
	})
	return true
}

func activeAIAbuseBlock(subject billing.CreditSubject) (models.AIAbuseBlock, bool, error) {
	var block models.AIAbuseBlock
	now := time.Now().UTC()
	query := database.DB.
		Where("active = ? AND scope IN ? AND (expires_at IS NULL OR expires_at > ?)", true, []string{"ai_parse", "all_ai"}, now)
	if subject.UserID != nil {
		query = query.Where("user_id = ?", *subject.UserID)
	} else {
		query = query.Where("guest_device_id_hash = ?", strings.TrimSpace(subject.GuestDeviceIDHash))
	}
	err := query.Order("created_at DESC").First(&block).Error
	if err == nil {
		return block, true, nil
	}
	if err == gorm.ErrRecordNotFound {
		return models.AIAbuseBlock{}, false, nil
	}
	return models.AIAbuseBlock{}, false, err
}

func (s *Server) rejectIfAIFailureCooldown(c *gin.Context, subject billing.CreditSubject) bool {
	active, retryAt, failures, err := activeAIFailureCooldown(subject, s.cfg)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "ai_cooldown_lookup_failed"})
		return true
	}
	if !active {
		return false
	}
	retryAfter := int(time.Until(retryAt).Seconds())
	if retryAfter < 1 {
		retryAfter = 1
	}
	c.Header("Retry-After", stringInt(retryAfter))
	c.JSON(http.StatusTooManyRequests, gin.H{
		"error":       "ai_parse_cooldown_active",
		"failures":    failures,
		"retry_at":    retryAt,
		"retry_after": retryAfter,
	})
	return true
}

func activeAIFailureCooldown(subject billing.CreditSubject, cfg *config.Config) (bool, time.Time, int64, error) {
	if cfg.AIFailedParseCooldownThreshold <= 0 || cfg.AIFailedParseCooldownWindowMin <= 0 || cfg.AIFailedParseCooldownMinutes <= 0 {
		return false, time.Time{}, 0, nil
	}
	now := time.Now().UTC()
	windowStart := now.Add(-time.Duration(cfg.AIFailedParseCooldownWindowMin) * time.Minute)
	query := database.DB.Model(&models.AIUsageEvent{}).
		Where("started_at >= ? AND status = ?", windowStart, billing.UsageStatusFailedAfterProvider)
	if subject.UserID != nil {
		query = query.Where("user_id = ?", *subject.UserID)
	} else {
		query = query.Where("guest_device_id_hash = ?", strings.TrimSpace(subject.GuestDeviceIDHash))
	}
	var failures int64
	if err := query.Count(&failures).Error; err != nil {
		return false, time.Time{}, 0, err
	}
	if failures < int64(cfg.AIFailedParseCooldownThreshold) {
		return false, time.Time{}, failures, nil
	}

	var latest models.AIUsageEvent
	latestQuery := database.DB.
		Where("started_at >= ? AND status = ?", windowStart, billing.UsageStatusFailedAfterProvider)
	if subject.UserID != nil {
		latestQuery = latestQuery.Where("user_id = ?", *subject.UserID)
	} else {
		latestQuery = latestQuery.Where("guest_device_id_hash = ?", strings.TrimSpace(subject.GuestDeviceIDHash))
	}
	if err := latestQuery.Order("started_at DESC").First(&latest).Error; err != nil {
		return false, time.Time{}, failures, err
	}
	retryAt := latest.StartedAt.UTC().Add(time.Duration(cfg.AIFailedParseCooldownMinutes) * time.Minute)
	if !retryAt.After(now) {
		return false, time.Time{}, failures, nil
	}
	return true, retryAt, failures, nil
}

func (s *Server) rejectIfUnpaidVoiceTooLong(c *gin.Context, creditService *billing.CreditService, subject billing.CreditSubject, actionCode ai.ActionCode, audioSize int64) bool {
	if s.cfg.AIUnpaidMaxVoiceBytes <= 0 || audioSize <= s.cfg.AIUnpaidMaxVoiceBytes {
		return false
	}
	allowance, err := creditService.CheckAllowance(subject, actionCode)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "credit_allowance_failed"})
		return true
	}
	if allowance.PaidPlanActive {
		return false
	}
	c.JSON(http.StatusPaymentRequired, gin.H{
		"error":                 billing.AllowanceFeatureLocked,
		"required_credits":      allowance.RequiredCredits,
		"available_credits":     allowance.AvailableCredits,
		"daily_limit_remaining": allowance.DailyRemaining,
		"upgrade_required":      true,
		"max_voice_bytes":       s.cfg.AIUnpaidMaxVoiceBytes,
	})
	return true
}

func validGuestDeviceFingerprint(deviceID string) bool {
	deviceID = strings.TrimSpace(deviceID)
	if len(deviceID) < 12 || len(deviceID) > 256 {
		return false
	}
	allSame := true
	var first rune
	for i, r := range deviceID {
		if r <= 32 || r == 127 {
			return false
		}
		if i == 0 {
			first = r
		} else if r != first {
			allSame = false
		}
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && !strings.ContainsRune("-_:.=", r) {
			return false
		}
	}
	return !allSame
}

func stringInt(value int) string {
	return strconv.Itoa(value)
}
