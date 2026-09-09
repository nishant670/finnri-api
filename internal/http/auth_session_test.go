package http

import (
	"strings"
	"testing"
	"time"

	"finnri/internal/config"
)

func TestSessionTTLUsesSafeConfigBound(t *testing.T) {
	if got := sessionTTLForConfig(nil); got != 7*24*time.Hour {
		t.Fatalf("expected nil config to use seven-day TTL, got %s", got)
	}
	if got := sessionTTLForConfig(&config.Config{AuthSessionTTLDays: 14}); got != 14*24*time.Hour {
		t.Fatalf("expected configured fourteen-day TTL, got %s", got)
	}
	for _, invalid := range []int{0, -1, 31} {
		if got := sessionTTLForConfig(&config.Config{AuthSessionTTLDays: invalid}); got != 7*24*time.Hour {
			t.Fatalf("expected invalid TTL %d to use safe default, got %s", invalid, got)
		}
	}
}

func TestGenerateSessionTokenIsOpaqueAndNotMockToken(t *testing.T) {
	token := generateSessionToken()
	if !strings.HasPrefix(token, "fnr_") {
		t.Fatalf("expected fnr_ token prefix, got %q", token)
	}
	if strings.HasPrefix(token, "mock_token_") {
		t.Fatal("session token must not use forgeable mock token format")
	}
	if len(token) != 68 {
		t.Fatalf("expected 68 character token, got %d", len(token))
	}
}

func TestHashSessionToken(t *testing.T) {
	hash := hashSessionToken("fnr_test")
	if len(hash) != 64 {
		t.Fatalf("expected 64 character sha256 hex hash, got %d", len(hash))
	}
	if hash == "fnr_test" {
		t.Fatal("token hash must not store the raw token")
	}
	if hash != hashSessionToken("fnr_test") {
		t.Fatal("token hash must be stable")
	}
}

func TestGenerateClaimTokenIsOpaque(t *testing.T) {
	token := generateClaimToken()
	if !validClaimTokenFormat(token) {
		t.Fatalf("expected valid opaque claim token, got %q", token)
	}
	if strings.HasPrefix(token, "claim_") {
		t.Fatalf("claim token must not use plaintext claim_ format: %q", token)
	}
	if strings.Contains(token, "email:") || strings.Contains(token, "phone:") {
		t.Fatalf("claim token must not embed identifier metadata: %q", token)
	}
}

func TestOldPlaintextClaimTokensAreRejected(t *testing.T) {
	for _, token := range []string{
		"claim_email:user@example.com",
		"claim_phone:+919876543210",
		"claim_+919876543210",
	} {
		if validClaimTokenFormat(token) {
			t.Fatalf("old plaintext token should be rejected: %q", token)
		}
	}
}

func TestHashClaimToken(t *testing.T) {
	token := generateClaimToken()
	hash := hashClaimToken(token)
	if len(hash) != 64 {
		t.Fatalf("expected 64 character sha256 hex hash, got %d", len(hash))
	}
	if hash == token {
		t.Fatal("claim token hash must not store the raw token")
	}
	if hash != hashClaimToken(token) {
		t.Fatal("claim token hash must be stable")
	}
}

func TestOTPHashVerification(t *testing.T) {
	hash, err := hashOTP("493820")
	if err != nil {
		t.Fatal(err)
	}
	if hash == "493820" {
		t.Fatal("OTP hash must not store the raw OTP")
	}
	if !verifyOTPHash(hash, "493820") {
		t.Fatal("expected OTP hash to verify the original code")
	}
	if verifyOTPHash(hash, "1234") || verifyOTPHash(hash, "123456") {
		t.Fatal("static legacy OTPs must not verify against a different generated OTP")
	}
}

func TestGenerateOTPCodeFormat(t *testing.T) {
	otp, err := generateOTPCode()
	if err != nil {
		t.Fatal(err)
	}
	if len(otp) != 6 {
		t.Fatalf("expected 6 digit OTP, got %q", otp)
	}
	for _, char := range otp {
		if char < '0' || char > '9' {
			t.Fatalf("expected numeric OTP, got %q", otp)
		}
	}
}

func TestValidOTPCode(t *testing.T) {
	validCodes := []string{"000000", "123456", "493820"}
	for _, code := range validCodes {
		if !validOTPCode(code) {
			t.Fatalf("expected %q to be valid", code)
		}
	}

	invalidCodes := []string{"", "12345", "1234567", "12345a", " 123456"}
	for _, code := range invalidCodes {
		if validOTPCode(code) {
			t.Fatalf("expected %q to be invalid", code)
		}
	}
}

func TestNormalizeIdentifierTrimsAndNormalizesEmail(t *testing.T) {
	identifierType, identifier, err := normalizeIdentifier("  User.Name@Example.COM  ")
	if err != nil {
		t.Fatal(err)
	}
	if identifierType != "email" {
		t.Fatalf("identifier type = %q, want email", identifierType)
	}
	if identifier != "user.name@example.com" {
		t.Fatalf("identifier = %q, want normalized email", identifier)
	}
}

func TestValidPINRejectsDefaultRepeatedDigits(t *testing.T) {
	validPINs := []string{"1234", "9081", "1200"}
	for _, pin := range validPINs {
		if !validPIN(pin) {
			t.Fatalf("expected %q to be valid", pin)
		}
	}

	invalidPINs := []string{"", "123", "12345", "12a4", "0000", "1111", "9999"}
	for _, pin := range invalidPINs {
		if validPIN(pin) {
			t.Fatalf("expected %q to be invalid", pin)
		}
	}
}
