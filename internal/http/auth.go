package http

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"finnri/internal/billing"
	"finnri/internal/config"
	"finnri/internal/database"
	"finnri/internal/identity"
	"finnri/internal/mailer"
	"finnri/internal/models"
)

// Auth Response Wrapper
type AuthResponse struct {
	Token     string       `json:"token"`
	ExpiresAt time.Time    `json:"expires_at"`
	User      *models.User `json:"user"`
}

const defaultSessionTTL = 7 * 24 * time.Hour

func sessionTTLForConfig(cfg *config.Config) time.Duration {
	if cfg == nil || cfg.AuthSessionTTLDays < 1 || cfg.AuthSessionTTLDays > 30 {
		return defaultSessionTTL
	}
	return time.Duration(cfg.AuthSessionTTLDays) * 24 * time.Hour
}

func (s *Server) authLogout(c *gin.Context) {
	sessionID, ok := c.Get("authSessionID")
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_session"})
		return
	}
	now := time.Now().UTC()
	result := database.DB.Model(&models.AuthSession{}).
		Where("id = ? AND revoked_at IS NULL", sessionID).
		Update("revoked_at", now)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_revoke_session"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "signed_out"})
}

func (s *Server) authRevokeAllSessions(c *gin.Context) {
	userID := c.MustGet("userID").(uint)
	now := time.Now().UTC()
	result := database.DB.Model(&models.AuthSession{}).
		Where("user_id = ? AND revoked_at IS NULL", userID).
		Update("revoked_at", now)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_revoke_sessions"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "all_sessions_revoked", "revoked": result.RowsAffected})
}

const maxOTPAttempts = 5
const maxPINAttempts = 5
const loginLockDuration = 15 * time.Minute
const claimTokenPrefix = "fnrct_"

type verifiedClaim struct {
	IdentifierType string
	Identifier     string
}

// Generate a random UUID-like string
func generateUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func generateSessionToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("secure random token generation failed: " + err.Error())
	}
	return "fnr_" + hex.EncodeToString(b)
}

func hashSessionToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func generateOTPCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

func validOTPCode(otp string) bool {
	if len(otp) != 6 {
		return false
	}
	for _, char := range otp {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func validPINFormat(pin string) bool {
	if len(pin) != 4 {
		return false
	}
	for _, char := range pin {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func validPIN(pin string) bool {
	if !validPINFormat(pin) {
		return false
	}
	allSame := true
	for i, char := range pin {
		if i > 0 && byte(char) != pin[0] {
			allSame = false
		}
	}
	return !allSame
}

var errWeakPIN = errors.New("weak_pin")

// hashOptionalPIN turns a PIN the caller may have omitted into the value that
// belongs in users.pin_hash. An empty PIN is not an error — it is an account
// that deliberately has none — and returns an empty hash, which is exactly what
// bcrypt.CompareHashAndPassword rejects on every PIN login attempt.
func hashOptionalPIN(pin string) (string, error) {
	if pin == "" {
		return "", nil
	}
	if !validPIN(pin) {
		return "", errWeakPIN
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pin), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

func hashOTP(otp string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(otp), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

func verifyOTPHash(hash, otp string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(otp)) == nil
}

func generateClaimToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("secure random claim token generation failed: " + err.Error())
	}
	return claimTokenPrefix + hex.EncodeToString(b)
}

func validClaimTokenFormat(token string) bool {
	if !strings.HasPrefix(token, claimTokenPrefix) {
		return false
	}
	raw := strings.TrimPrefix(token, claimTokenPrefix)
	if len(raw) != 64 {
		return false
	}
	_, err := hex.DecodeString(raw)
	return err == nil
}

func hashClaimToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func normalizeIdentifier(identifier string) (string, string, error) {
	normalized := strings.TrimSpace(identifier)
	if normalized == "" {
		return "", "", errors.New("identifier_required")
	}
	if strings.Contains(normalized, "@") {
		return "email", strings.ToLower(normalized), nil
	}
	normalizedPhone := identity.NormalizePhone(normalized)
	if normalizedPhone == "" {
		return "", "", errors.New("invalid_phone")
	}
	return "phone", normalizedPhone, nil
}

func consumeClaimToken(rawToken string) (verifiedClaim, error) {
	return consumeClaimTokenTx(database.DB, rawToken)
}

func consumeClaimTokenTx(tx *gorm.DB, rawToken string) (verifiedClaim, error) {
	if !validClaimTokenFormat(rawToken) {
		return verifiedClaim{}, errors.New("invalid_claim_token")
	}

	var verification models.AuthVerification
	now := time.Now().UTC()
	if err := tx.
		Where("claim_token_hash = ? AND claim_used_at IS NULL AND claim_expires_at > ?", hashClaimToken(rawToken), now).
		First(&verification).Error; err != nil {
		return verifiedClaim{}, errors.New("invalid_or_expired_claim_token")
	}

	verification.ClaimUsedAt = &now
	if err := tx.Save(&verification).Error; err != nil {
		return verifiedClaim{}, err
	}

	return verifiedClaim{IdentifierType: verification.IdentifierType, Identifier: verification.Identifier}, nil
}

func issueSession(userID uint) (string, time.Time, error) {
	return issueSessionWithTTL(userID, defaultSessionTTL)
}

func issueSessionWithTTL(userID uint, ttl time.Duration) (string, time.Time, error) {
	token := generateSessionToken()
	expiresAt := time.Now().UTC().Add(ttl)
	session := models.AuthSession{
		UserID: userID, TokenHash: hashSessionToken(token), ExpiresAt: expiresAt,
	}
	if err := database.DB.Create(&session).Error; err != nil {
		return "", time.Time{}, err
	}
	return token, expiresAt, nil
}

func (s *Server) authResponse(user *models.User) (AuthResponse, error) {
	if err := reconcileSplitIdentities(database.DB, user.ID); err != nil {
		return AuthResponse{}, err
	}
	token, expiresAt, err := issueSessionWithTTL(user.ID, sessionTTLForConfig(s.cfg))
	if err != nil {
		return AuthResponse{}, err
	}
	return AuthResponse{Token: token, ExpiresAt: expiresAt, User: user}, nil
}

func findUserByVerifiedIdentifier(identifierType, identifier string) (models.User, error) {
	var user models.User
	query := "email = ?"
	if identifierType != "email" {
		query = "phone_normalized = ?"
	}
	err := database.DB.Where(query, identifier).First(&user).Error
	return user, err
}

func findUserByLoginIdentifier(identifier string) (models.User, error) {
	identifierType, normalized, err := normalizeIdentifier(identifier)
	if err != nil {
		return models.User{}, err
	}
	return findUserByVerifiedIdentifier(identifierType, normalized)
}

func resetLoginLock(user *models.User) error {
	user.FailedLoginAttempts = 0
	user.LoginLockedUntil = nil
	return database.DB.Model(user).Updates(map[string]interface{}{
		"failed_login_attempts": 0,
		"login_locked_until":    nil,
	}).Error
}

func clearExpiredLoginLock(user *models.User, now time.Time) error {
	if user.LoginLockedUntil == nil || user.LoginLockedUntil.After(now) {
		return nil
	}
	return resetLoginLock(user)
}

func recordFailedLoginAttempt(user *models.User, now time.Time) (int, *time.Time, error) {
	attempts := user.FailedLoginAttempts + 1
	updates := map[string]interface{}{
		"failed_login_attempts": attempts,
	}

	var lockedUntil *time.Time
	if attempts >= maxPINAttempts {
		lockUntil := now.Add(loginLockDuration)
		lockedUntil = &lockUntil
		updates["login_locked_until"] = lockedUntil
	}

	if err := database.DB.Model(user).Updates(updates).Error; err != nil {
		return 0, nil, err
	}

	user.FailedLoginAttempts = attempts
	user.LoginLockedUntil = lockedUntil
	remaining := maxPINAttempts - attempts
	if remaining < 0 {
		remaining = 0
	}
	return remaining, lockedUntil, nil
}

// POST /v1/auth/guest
func (s *Server) authGuest(c *gin.Context) {
	var input struct {
		DeviceID string `json:"device_id"`
	}
	if err := c.ShouldBindJSON(&input); err != nil && err != io.EOF {
		if requestBodyTooLarge(err) {
			c.JSON(413, gin.H{"error": "request_body_too_large"})
			return
		}
		c.JSON(400, gin.H{"error": "invalid_request"})
		return
	}

	deviceID := strings.TrimSpace(input.DeviceID)
	var user models.User
	if deviceID != "" {
		if err := database.DB.
			Where("device_id = ? AND is_guest = ?", deviceID, true).
			Limit(1).
			Find(&user).Error; err != nil {
			c.JSON(500, gin.H{"error": "failed_lookup_guest"})
			return
		}
		if user.ID != 0 {
			// Found existing guest session
			if err := ensureDefaultCashAccount(user.ID); err != nil {
				c.JSON(500, gin.H{"error": "failed_ensure_default_account"})
				return
			}
			if _, _, err := billing.NewCreditService(database.DB).EnsureGuestTrialGrant(deviceID, c.ClientIP()); err != nil {
				c.JSON(500, gin.H{"error": "failed_ensure_guest_credits"})
				return
			}
			user.HasPin = user.PinHash != ""
			response, err := s.authResponse(&user)
			if err != nil {
				c.JSON(500, gin.H{"error": "failed_create_session"})
				return
			}
			c.JSON(200, response)
			return
		}
	}

	var deviceIDPtr *string
	if deviceID != "" {
		deviceIDPtr = &deviceID
	}

	// Generate unique username
	// In production, you might want a retry loop here to ensure uniqueness
	username := "Guest_" + generateUUID()[:8]

	user = models.User{
		UUID:     generateUUID(),
		IsGuest:  true,
		DeviceID: deviceIDPtr,
		Username: username,
	}

	if err := database.DB.Create(&user).Error; err != nil {
		if deviceID != "" {
			var existingGuest models.User
			lookupErr := database.DB.
				Where("device_id = ? AND is_guest = ?", deviceID, true).
				Limit(1).
				Find(&existingGuest).Error
			if lookupErr == nil && existingGuest.ID != 0 {
				if err := ensureDefaultCashAccount(existingGuest.ID); err != nil {
					c.JSON(500, gin.H{"error": "failed_ensure_default_account"})
					return
				}
				if _, _, err := billing.NewCreditService(database.DB).EnsureGuestTrialGrant(deviceID, c.ClientIP()); err != nil {
					c.JSON(500, gin.H{"error": "failed_ensure_guest_credits"})
					return
				}
				existingGuest.HasPin = existingGuest.PinHash != ""
				response, err := s.authResponse(&existingGuest)
				if err != nil {
					c.JSON(500, gin.H{"error": "failed_create_session"})
					return
				}
				c.JSON(200, response)
				return
			}
		}
		c.JSON(500, gin.H{"error": "failed_create_guest"})
		return
	}
	if err := ensureDefaultCashAccount(user.ID); err != nil {
		c.JSON(500, gin.H{"error": "failed_create_default_account"})
		return
	}
	if _, _, err := billing.NewCreditService(database.DB).EnsureGuestTrialGrant(deviceID, c.ClientIP()); err != nil {
		c.JSON(500, gin.H{"error": "failed_ensure_guest_credits"})
		return
	}

	user.HasPin = user.PinHash != ""
	response, err := s.authResponse(&user)
	if err != nil {
		c.JSON(500, gin.H{"error": "failed_create_session"})
		return
	}
	c.JSON(200, response)
}

func ensureDefaultCashAccount(userID uint) error {
	return ensureDefaultCashAccountTx(database.DB, userID)
}

func ensureDefaultCashAccountTx(tx *gorm.DB, userID uint) error {
	var count int64
	if err := tx.Model(&models.Account{}).Where("user_id = ?", userID).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	return tx.Create(&models.Account{
		UserID: userID, Type: "cash", Name: "Cash", IsDefault: true, Color: "#2ECC71",
	}).Error
}

// POST /v1/auth/identify
func (s *Server) authIdentify(c *gin.Context) {
	var input struct {
		Identifier string `json:"identifier" binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		if requestBodyTooLarge(err) {
			c.JSON(413, gin.H{"error": "request_body_too_large"})
			return
		}
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	identifierType, identifier, err := normalizeIdentifier(input.Identifier)
	if err != nil {
		c.JSON(422, gin.H{"error": err.Error()})
		return
	}

	var user models.User
	query := "email = ?"
	if identifierType == "phone" {
		query = "phone_normalized = ?"
	}
	err = database.DB.Where(query, identifier).First(&user).Error
	if err == gorm.ErrRecordNotFound {
		c.JSON(200, gin.H{"exists": false})
		return
	} else if err != nil {
		c.JSON(500, gin.H{"error": "db_error"})
		return
	}

	// has_pin tells the app which second step to show: the PIN keypad, or the
	// OTP it would otherwise only reach through "Forgot PIN". An account that
	// skipped PIN setup has no keypad to offer.
	c.JSON(200, gin.H{"exists": true, "is_guest": user.IsGuest, "has_pin": user.PinHash != ""})
}

// otpSignInDisabled answers the request and reports true when OTP sign-in is
// switched off.
//
// The gate sits in front of both OTP endpoints rather than only in front of
// the client's UI, for two reasons. A disabled flow that still answers is a
// flow someone can still drive with curl. And with `send` refusing outright,
// the debug-code path is unreachable — so OTP_DEBUG_RESPONSE left on in a
// deployed environment is inert instead of handing out working sign-in codes,
// which is exactly how production came to be exploitable.
func (s *Server) otpSignInDisabled(c *gin.Context) bool {
	if s.cfg != nil && s.cfg.AuthOTPEnabled {
		return false
	}
	c.JSON(503, gin.H{
		"error":              "otp_sign_in_disabled",
		"available_channels": []string{"google", "guest"},
	})
	return true
}

// POST /v1/auth/otp/send
func (s *Server) authOtpSend(c *gin.Context) {
	if s.otpSignInDisabled(c) {
		return
	}
	var input struct {
		Identifier string `json:"identifier" binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		if requestBodyTooLarge(err) {
			c.JSON(413, gin.H{"error": "request_body_too_large"})
			return
		}
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	identifierType, identifier, err := normalizeIdentifier(input.Identifier)
	if err != nil {
		c.JSON(422, gin.H{"error": err.Error()})
		return
	}

	// Phone codes have no carrier behind them. India SMS needs DLT template
	// registration before a provider will accept traffic, so rather than store
	// a code nothing can deliver, say so and let the app offer email instead.
	if identifierType == "phone" && !s.cfg.OTPPhoneChannelEnabled {
		c.JSON(422, gin.H{
			"error":              "otp_channel_unavailable",
			"channel":            "phone",
			"available_channels": []string{"email"},
		})
		return
	}

	// One live code per identifier per cooldown. The auth rate limiter buckets
	// by IP, which does nothing to stop one address being mailed repeatedly
	// from a rotating set of them.
	if retryAfter, blocked := otpResendCooldownRemaining(identifierType, identifier, s.cfg.OTPResendCooldownSeconds, time.Now().UTC()); blocked {
		c.Header("Retry-After", strconv.Itoa(retryAfter))
		c.JSON(429, gin.H{"error": "otp_resend_too_soon", "retry_after_seconds": retryAfter})
		return
	}

	otp := s.cfg.OTPDevCode
	if !s.cfg.OTPDebugResponse || !validOTPCode(otp) {
		generatedOTP, err := generateOTPCode()
		if err != nil {
			c.JSON(500, gin.H{"error": "otp_generation_failed"})
			return
		}
		otp = generatedOTP
	}
	otpHash, err := hashOTP(otp)
	if err != nil {
		c.JSON(500, gin.H{"error": "otp_hash_failed"})
		return
	}

	expiresAt := time.Now().UTC().Add(time.Duration(s.cfg.OTPExpiresMinutes) * time.Minute)
	verification := models.AuthVerification{
		IdentifierType: identifierType,
		Identifier:     identifier,
		OTPHash:        otpHash,
		OTPExpiresAt:   expiresAt,
	}
	if err := database.DB.Create(&verification).Error; err != nil {
		c.JSON(500, gin.H{"error": "failed_create_otp"})
		return
	}

	// Deliver before answering. The row is written first so a code can never
	// arrive in an inbox without a hash to verify it against; if the send then
	// fails, the row is removed so the cooldown does not lock the person out
	// of retrying with a provider that has recovered.
	if err := s.deliverOTP(c, identifierType, identifier, otp); err != nil {
		database.DB.Delete(&verification)
		log.Printf("[ERROR] otp delivery failed via %s: %v", s.emailSender().Name(), err)
		if !s.cfg.OTPDebugResponse {
			c.JSON(502, gin.H{"error": "otp_send_failed", "channel": identifierType})
			return
		}
		// Debug mode is the local-development path: there is usually no mail
		// provider at all, and dev_otp below is the delivery channel.
	}

	response := gin.H{"message": "otp_sent", "expires_at": expiresAt, "channel": identifierType}
	if s.cfg.OTPDebugResponse {
		response["dev_otp"] = otp
	}
	c.JSON(200, response)
}

// deliverOTP carries the code to the person. Only email is wired; phone is
// rejected earlier, and this returns an explicit error rather than nil if that
// guard is ever removed without a provider being added behind it.
func (s *Server) deliverOTP(c *gin.Context, identifierType, identifier, otp string) error {
	if identifierType != "email" {
		return fmt.Errorf("no delivery channel for identifier type %q", identifierType)
	}
	timeout := time.Duration(s.cfg.EmailSendTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	defer cancel()
	return s.emailSender().Send(ctx, mailer.OTPEmail(identifier, otp, s.cfg.OTPExpiresMinutes))
}

// otpResendCooldownRemaining reports how long the caller must wait before a
// new code may be issued for this identifier, based on the newest unconsumed
// verification row.
func otpResendCooldownRemaining(identifierType, identifier string, cooldownSeconds int, now time.Time) (int, bool) {
	if cooldownSeconds <= 0 {
		return 0, false
	}
	var latest models.AuthVerification
	err := database.DB.
		Where("identifier_type = ? AND identifier = ? AND verified_at IS NULL", identifierType, identifier).
		Order("created_at DESC").
		First(&latest).Error
	if err != nil {
		return 0, false
	}
	elapsed := now.Sub(latest.CreatedAt.UTC())
	cooldown := time.Duration(cooldownSeconds) * time.Second
	if elapsed >= cooldown {
		return 0, false
	}
	remaining := int((cooldown - elapsed).Seconds())
	if remaining < 1 {
		remaining = 1
	}
	return remaining, true
}

// POST /v1/auth/otp/verify
func (s *Server) authOtpVerify(c *gin.Context) {
	if s.otpSignInDisabled(c) {
		return
	}
	var input struct {
		Identifier string `json:"identifier" binding:"required"`
		OTP        string `json:"otp" binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		if requestBodyTooLarge(err) {
			c.JSON(413, gin.H{"error": "request_body_too_large"})
			return
		}
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	identifierType, identifier, err := normalizeIdentifier(input.Identifier)
	if err != nil {
		c.JSON(422, gin.H{"error": err.Error()})
		return
	}

	var verification models.AuthVerification
	now := time.Now().UTC()
	err = database.DB.
		Where("identifier_type = ? AND identifier = ? AND verified_at IS NULL AND otp_expires_at > ?", identifierType, identifier, now).
		Order("created_at DESC").
		First(&verification).Error
	if err != nil {
		c.JSON(401, gin.H{"error": "invalid_or_expired_otp"})
		return
	}

	if verification.Attempts >= maxOTPAttempts {
		c.JSON(429, gin.H{"error": "too_many_otp_attempts"})
		return
	}

	if !verifyOTPHash(verification.OTPHash, input.OTP) {
		database.DB.Model(&verification).Update("attempts", verification.Attempts+1)
		c.JSON(401, gin.H{"error": "invalid_otp"})
		return
	}

	claimToken := generateClaimToken()
	claimTokenHash := hashClaimToken(claimToken)
	claimExpiresAt := now.Add(time.Duration(s.cfg.ClaimTokenMinutes) * time.Minute)
	verification.VerifiedAt = &now
	verification.ClaimTokenHash = &claimTokenHash
	verification.ClaimExpiresAt = &claimExpiresAt
	if err := database.DB.Save(&verification).Error; err != nil {
		c.JSON(500, gin.H{"error": "failed_create_claim"})
		return
	}

	c.JSON(200, gin.H{"claim_token": claimToken, "expires_at": claimExpiresAt})
}

// POST /v1/auth/register
func (s *Server) authRegister(c *gin.Context) {
	var input struct {
		ClaimToken        string `json:"claim_token" binding:"required"`
		PIN               string `json:"pin" binding:"omitempty,len=4"`
		GuestUUID         string `json:"guest_uuid"`
		DeviceID          string `json:"device_id"`
		BiometricsEnabled bool   `json:"biometrics_enabled"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		if requestBodyTooLarge(err) {
			c.JSON(413, gin.H{"error": "request_body_too_large"})
			return
		}
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	// A PIN is optional at signup: the account exists to back the data up, and
	// the app screen that asks for one has a "Set up later". An omitted PIN
	// leaves pin_hash empty, which is what has_pin=false is derived from — the
	// account then signs in on a new device by OTP instead of by keypad.
	hash, err := hashOptionalPIN(input.PIN)
	if err != nil {
		if errors.Is(err, errWeakPIN) {
			c.JSON(400, gin.H{"error": "weak_pin"})
			return
		}
		c.JSON(500, gin.H{"error": "encryption_failed"})
		return
	}

	var user models.User
	if err := database.DB.Transaction(func(tx *gorm.DB) error {
		claim, err := consumeClaimTokenTx(tx, input.ClaimToken)
		if err != nil {
			return err
		}

		var email *string
		var phone *string
		if claim.IdentifierType == "email" {
			email = &claim.Identifier
		} else {
			phone = &claim.Identifier
		}

		var existing models.User
		query := "email = ?"
		if claim.IdentifierType != "email" {
			query = "phone_normalized = ?"
		}
		if err := tx.Where(query, claim.Identifier).First(&existing).Error; err == nil {
			return gorm.ErrDuplicatedKey
		} else if err != gorm.ErrRecordNotFound {
			return err
		}

		deviceID := strings.TrimSpace(input.DeviceID)
		var deviceIDPtr *string
		if deviceID != "" {
			deviceIDPtr = &deviceID
		}

		userFound := false
		guestUUID := strings.TrimSpace(input.GuestUUID)
		if guestUUID != "" {
			if err := tx.Where("uuid = ? AND is_guest = ?", guestUUID, true).First(&user).Error; err == nil {
				userFound = true
			} else if err != gorm.ErrRecordNotFound {
				return err
			}
		}
		if !userFound && deviceID != "" {
			if err := tx.Where("device_id = ? AND is_guest = ?", deviceID, true).First(&user).Error; err == nil {
				userFound = true
			} else if err != gorm.ErrRecordNotFound {
				return err
			}
		}

		if userFound {
			convertedAt := time.Now().UTC()
			if email != nil {
				user.Email = email
			}
			if phone != nil {
				user.Phone = phone
			}
			if hash != "" {
				user.PinHash = hash
			}
			user.IsGuest = false
			user.ConvertedAt = &convertedAt
			user.BiometricsEnabled = input.BiometricsEnabled
			user.Username = "User_" + generateUUID()[:8]
			if deviceIDPtr != nil {
				user.DeviceID = deviceIDPtr
			}
			if err := tx.Save(&user).Error; err != nil {
				return err
			}
		} else {
			user = models.User{
				UUID:              generateUUID(),
				Email:             email,
				Phone:             phone,
				PinHash:           hash,
				IsGuest:           false,
				BiometricsEnabled: input.BiometricsEnabled,
				DeviceID:          deviceIDPtr,
				Username:          "User_" + generateUUID()[:8],
			}
			if err := tx.Create(&user).Error; err != nil {
				return err
			}
		}

		if err := ensureDefaultCashAccountTx(tx, user.ID); err != nil {
			return err
		}
		creditService := billing.NewCreditService(database.DB)
		if err := creditService.PromoteGuestDeviceToUser(tx, user.ID, deviceID); err != nil {
			return err
		}
		if _, _, err := creditService.EnsureLoggedInFreeTrialGrantTx(tx, user.ID); err != nil {
			return err
		}
		return nil
	}); err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			c.JSON(409, gin.H{"error": "user_already_exists"})
			return
		}
		if err.Error() == "invalid_claim_token" || err.Error() == "invalid_or_expired_claim_token" {
			c.JSON(401, gin.H{"error": "invalid_claim_token"})
			return
		}
		c.JSON(500, gin.H{"error": "failed_register_user"})
		return
	}

	_ = database.DB.Model(&models.AuthSession{}).
		Where("user_id = ? AND revoked_at IS NULL", user.ID).
		Update("revoked_at", time.Now().UTC()).Error
	user.HasPin = user.PinHash != ""
	response, err := s.authResponse(&user)
	if err != nil {
		c.JSON(500, gin.H{"error": "failed_create_session"})
		return
	}
	c.JSON(201, response)
}

// POST /v1/auth/google
func (s *Server) authGoogle(c *gin.Context) {
	var input struct {
		IDToken           string `json:"id_token" binding:"required"`
		Nonce             string `json:"nonce"`
		GuestUUID         string `json:"guest_uuid"`
		DeviceID          string `json:"device_id"`
		BiometricsEnabled bool   `json:"biometrics_enabled"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		if requestBodyTooLarge(err) {
			c.JSON(413, gin.H{"error": "request_body_too_large"})
			return
		}
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if len(s.cfg.GoogleClientIDs) == 0 {
		c.JSON(503, gin.H{"error": "google_login_not_configured"})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	identity, err := verifyGoogleIDToken(ctx, strings.TrimSpace(input.IDToken), s.cfg.GoogleClientIDs)
	if err != nil {
		c.JSON(401, gin.H{"error": err.Error()})
		return
	}
	if input.Nonce != "" && identity.Nonce != input.Nonce {
		c.JSON(401, gin.H{"error": "invalid_google_nonce"})
		return
	}

	var user models.User
	googleSubject := identity.Subject
	email := identity.Email
	deviceID := strings.TrimSpace(input.DeviceID)
	deviceIDPtr := (*string)(nil)
	if deviceID != "" {
		deviceIDPtr = &deviceID
	}

	err = database.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("google_subject = ?", googleSubject).First(&user).Error; err == nil {
			updates := map[string]interface{}{
				"failed_login_attempts": 0,
				"login_locked_until":    nil,
			}
			if user.Email == nil || *user.Email == "" {
				updates["email"] = email
			}
			if deviceID != "" {
				updates["device_id"] = deviceID
			}
			// Accounts created before Google's name was read are still carrying
			// `User_2c9f4a1b`; this is where they get their name. A username the
			// person chose is left exactly as it is.
			if isGeneratedUsername(user.Username) {
				if named := googleUsername(tx, identity); !isGeneratedUsername(named) {
					updates["username"] = named
				}
			}
			if user.ProfileImage == "" && identity.Picture != "" {
				updates["profile_image"] = identity.Picture
			}
			if err := tx.Model(&user).Updates(updates).Error; err != nil {
				return err
			}
			return tx.First(&user, user.ID).Error
		} else if err != gorm.ErrRecordNotFound {
			return err
		}

		var existing models.User
		if err := tx.Where("LOWER(email) = LOWER(?)", email).First(&existing).Error; err == nil {
			if existing.GoogleSubject != nil && *existing.GoogleSubject != googleSubject {
				return errors.New("email_linked_to_different_google_account")
			}
			updates := map[string]interface{}{
				"google_subject":        googleSubject,
				"failed_login_attempts": 0,
				"login_locked_until":    nil,
			}
			if deviceID != "" {
				updates["device_id"] = deviceID
			}
			if isGeneratedUsername(existing.Username) {
				if named := googleUsername(tx, identity); !isGeneratedUsername(named) {
					updates["username"] = named
				}
			}
			if existing.ProfileImage == "" && identity.Picture != "" {
				updates["profile_image"] = identity.Picture
			}
			if err := tx.Model(&existing).Updates(updates).Error; err != nil {
				return err
			}
			user = existing
			return tx.First(&user, existing.ID).Error
		} else if err != gorm.ErrRecordNotFound {
			return err
		}

		if strings.TrimSpace(input.GuestUUID) != "" {
			if err := tx.Where("uuid = ? AND is_guest = ?", strings.TrimSpace(input.GuestUUID), true).First(&user).Error; err == nil {
				convertedAt := time.Now().UTC()
				user.Email = &email
				user.GoogleSubject = &googleSubject
				user.IsGuest = false
				user.ConvertedAt = &convertedAt
				user.BiometricsEnabled = input.BiometricsEnabled
				// A guest is named `Guest_59d8f84f`, so there is nothing here
				// worth keeping either way.
				user.Username = googleUsername(tx, identity)
				if identity.Picture != "" {
					user.ProfileImage = identity.Picture
				}
				if deviceIDPtr != nil {
					user.DeviceID = deviceIDPtr
				}
				return tx.Save(&user).Error
			} else if err != gorm.ErrRecordNotFound {
				return err
			}
		}

		user = models.User{
			UUID:              generateUUID(),
			Email:             &email,
			GoogleSubject:     &googleSubject,
			IsGuest:           false,
			BiometricsEnabled: input.BiometricsEnabled,
			DeviceID:          deviceIDPtr,
			Username:          googleUsername(tx, identity),
			ProfileImage:      identity.Picture,
		}
		return tx.Create(&user).Error
	})
	if err != nil {
		if err.Error() == "email_linked_to_different_google_account" {
			c.JSON(409, gin.H{"error": "email_linked_to_different_google_account"})
			return
		}
		c.JSON(500, gin.H{"error": "google_login_failed"})
		return
	}

	if err := ensureDefaultCashAccount(user.ID); err != nil {
		c.JSON(500, gin.H{"error": "failed_ensure_default_account"})
		return
	}
	if _, _, err := billing.NewCreditService(database.DB).EnsureLoggedInFreeTrialGrant(user.ID); err != nil {
		c.JSON(500, gin.H{"error": "failed_ensure_trial_credits"})
		return
	}

	user.HasPin = user.PinHash != ""
	response, err := s.authResponse(&user)
	if err != nil {
		c.JSON(500, gin.H{"error": "failed_create_session"})
		return
	}
	c.JSON(200, response)
}

// POST /v1/auth/pin/reset
func (s *Server) authPinReset(c *gin.Context) {
	var input struct {
		ClaimToken        string `json:"claim_token" binding:"required"`
		PIN               string `json:"pin" binding:"omitempty,len=4"`
		DeviceID          string `json:"device_id"`
		BiometricsEnabled *bool  `json:"biometrics_enabled"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		if requestBodyTooLarge(err) {
			c.JSON(413, gin.H{"error": "request_body_too_large"})
			return
		}
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	// An omitted PIN signs the account in on the strength of the OTP alone and
	// leaves whatever pin_hash it already had untouched. That is the path for an
	// account that never set one — it has no keypad to be sent to — and it is no
	// weaker than the reset it shares an endpoint with, which already hands out
	// a session to whoever proved control of the identifier.
	hash, err := hashOptionalPIN(input.PIN)
	if err != nil {
		if errors.Is(err, errWeakPIN) {
			c.JSON(400, gin.H{"error": "weak_pin"})
			return
		}
		c.JSON(500, gin.H{"error": "encryption_failed"})
		return
	}

	claim, err := consumeClaimToken(input.ClaimToken)
	if err != nil {
		c.JSON(401, gin.H{"error": "invalid_claim_token"})
		return
	}

	user, err := findUserByVerifiedIdentifier(claim.IdentifierType, claim.Identifier)
	if err != nil {
		c.JSON(404, gin.H{"error": "user_not_found"})
		return
	}

	updates := map[string]interface{}{
		"failed_login_attempts": 0,
		"login_locked_until":    nil,
	}
	if hash != "" {
		updates["pin_hash"] = hash
	}
	deviceID := strings.TrimSpace(input.DeviceID)
	if deviceID != "" {
		updates["device_id"] = deviceID
	}
	if input.BiometricsEnabled != nil {
		updates["biometrics_enabled"] = *input.BiometricsEnabled
	}

	if err := database.DB.Model(&user).Updates(updates).Error; err != nil {
		c.JSON(500, gin.H{"error": "failed_reset_pin"})
		return
	}
	if err := database.DB.First(&user, user.ID).Error; err != nil {
		c.JSON(500, gin.H{"error": "failed_load_user"})
		return
	}

	user.HasPin = user.PinHash != ""
	response, err := s.authResponse(&user)
	if err != nil {
		c.JSON(500, gin.H{"error": "failed_create_session"})
		return
	}
	c.JSON(200, response)
}

// POST /v1/auth/login
func (s *Server) authLogin(c *gin.Context) {
	var input struct {
		Identifier string `json:"identifier" binding:"required"`
		PIN        string `json:"pin" binding:"required"`
		DeviceID   string `json:"device_id"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		if requestBodyTooLarge(err) {
			c.JSON(413, gin.H{"error": "request_body_too_large"})
			return
		}
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if !validPINFormat(input.PIN) {
		c.JSON(401, gin.H{"error": "invalid_credentials"})
		return
	}

	user, err := findUserByLoginIdentifier(input.Identifier)
	if err != nil {
		c.JSON(401, gin.H{"error": "invalid_credentials"})
		return
	}

	now := time.Now().UTC()
	if err := clearExpiredLoginLock(&user, now); err != nil {
		c.JSON(500, gin.H{"error": "failed_update_login_lock"})
		return
	}
	if user.LoginLockedUntil != nil && user.LoginLockedUntil.After(now) {
		c.JSON(429, gin.H{
			"error":        "login_locked",
			"locked_until": user.LoginLockedUntil,
		})
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PinHash), []byte(input.PIN)); err != nil {
		attemptsRemaining, lockedUntil, updateErr := recordFailedLoginAttempt(&user, now)
		if updateErr != nil {
			c.JSON(500, gin.H{"error": "failed_update_login_attempts"})
			return
		}
		if lockedUntil != nil {
			c.JSON(429, gin.H{
				"error":        "login_locked",
				"locked_until": lockedUntil,
			})
			return
		}
		c.JSON(401, gin.H{
			"error":              "invalid_credentials",
			"attempts_remaining": attemptsRemaining,
		})
		return
	}

	// Update Device ID if provided and different
	shouldSave := false
	if input.DeviceID != "" {
		if user.DeviceID == nil || *user.DeviceID != input.DeviceID {
			user.DeviceID = &input.DeviceID
			shouldSave = true
		}
	}

	if shouldSave {
		database.DB.Save(&user)
	}
	if user.FailedLoginAttempts != 0 || user.LoginLockedUntil != nil {
		if err := resetLoginLock(&user); err != nil {
			c.JSON(500, gin.H{"error": "failed_update_login_lock"})
			return
		}
	}

	user.HasPin = user.PinHash != ""
	response, err := s.authResponse(&user)
	if err != nil {
		c.JSON(500, gin.H{"error": "failed_create_session"})
		return
	}
	c.JSON(200, response)
}
