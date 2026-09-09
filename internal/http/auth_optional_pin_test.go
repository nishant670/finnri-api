package http

import (
	"net/http"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"finnri/internal/database"
	"finnri/internal/models"
)

// issueTestClaimToken plants a verified AuthVerification row and hands back the
// raw claim token, so a test can exercise register / pin-reset without needing
// the OTP config the smoke router deliberately leaves at zero.
func issueTestClaimToken(t *testing.T, identifierType, identifier string) string {
	t.Helper()

	token := generateClaimToken()
	tokenHash := hashClaimToken(token)
	now := time.Now().UTC()
	expiresAt := now.Add(10 * time.Minute)
	verification := models.AuthVerification{
		IdentifierType: identifierType,
		Identifier:     identifier,
		OTPHash:        "unused",
		OTPExpiresAt:   now.Add(10 * time.Minute),
		VerifiedAt:     &now,
		ClaimTokenHash: &tokenHash,
		ClaimExpiresAt: &expiresAt,
	}
	if err := database.DB.Create(&verification).Error; err != nil {
		t.Fatalf("failed to plant claim token: %v", err)
	}
	return token
}

func TestAuthRegisterWithoutPINCreatesPinlessAccount(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)

	email := "no-pin@example.com"
	claimToken := issueTestClaimToken(t, "email", email)

	response := performJSONRequest[AuthResponse](
		t,
		router,
		http.MethodPost,
		"/v1/auth/register",
		"",
		map[string]any{"claim_token": claimToken, "device_id": "device-no-pin"},
		http.StatusCreated,
	)
	if response.User == nil {
		t.Fatal("expected a user in the register response")
	}
	if response.User.HasPin {
		t.Fatal("expected has_pin=false for an account that skipped PIN setup")
	}
	if response.Token == "" {
		t.Fatal("expected a session token — skipping the PIN must still sign the user in")
	}

	var stored models.User
	if err := database.DB.First(&stored, response.User.ID).Error; err != nil {
		t.Fatalf("failed to reload registered user: %v", err)
	}
	if stored.PinHash != "" {
		t.Fatalf("expected an empty pin_hash, got %q", stored.PinHash)
	}
	if stored.IsGuest {
		t.Fatal("expected the account to be a real account, not a guest")
	}

	// identify has to report the missing PIN, or the app sends the user to a
	// keypad that can never accept anything.
	identified := performJSONRequest[map[string]any](
		t,
		router,
		http.MethodPost,
		"/v1/auth/identify",
		"",
		map[string]string{"identifier": email},
		http.StatusOK,
	)
	if identified["exists"] != true {
		t.Fatalf("expected the account to exist: %#v", identified)
	}
	if identified["has_pin"] != false {
		t.Fatalf("expected has_pin=false from identify: %#v", identified)
	}
}

func TestAuthRegisterStillRejectsAWeakPIN(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)

	claimToken := issueTestClaimToken(t, "email", "weak-pin@example.com")

	body := performJSONRequest[map[string]string](
		t,
		router,
		http.MethodPost,
		"/v1/auth/register",
		"",
		map[string]any{"claim_token": claimToken, "pin": "1111", "device_id": "device-weak"},
		http.StatusBadRequest,
	)
	if body["error"] != "weak_pin" {
		t.Fatalf("expected weak_pin, got %#v", body)
	}
}

func TestAuthRegisterUpgradesGuestWithoutRequiringAPIN(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)

	guest := performJSONRequest[AuthResponse](
		t,
		router,
		http.MethodPost,
		"/v1/auth/guest",
		"",
		map[string]string{"device_id": "device-upgrade"},
		http.StatusOK,
	)
	if guest.User == nil || !guest.User.IsGuest {
		t.Fatalf("expected a guest user, got %#v", guest.User)
	}
	guestID := guest.User.ID

	claimToken := issueTestClaimToken(t, "email", "upgrade@example.com")
	upgraded := performJSONRequest[AuthResponse](
		t,
		router,
		http.MethodPost,
		"/v1/auth/register",
		"",
		map[string]any{
			"claim_token": claimToken,
			"guest_uuid":  guest.User.UUID,
			"device_id":   "device-upgrade",
		},
		http.StatusCreated,
	)
	if upgraded.User == nil || upgraded.User.ID != guestID {
		t.Fatalf("expected the guest row to be upgraded in place, got %#v", upgraded.User)
	}
	if upgraded.User.IsGuest {
		t.Fatal("expected the upgraded account to no longer be a guest")
	}
	if upgraded.User.HasPin {
		t.Fatal("expected has_pin=false after upgrading without a PIN")
	}
}

func TestAuthPinResetWithoutPINSignsInAndLeavesTheExistingPIN(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)

	email := "keeps-pin@example.com"
	hash, err := bcrypt.GenerateFromPassword([]byte("1357"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("failed to hash PIN: %v", err)
	}
	user := models.User{
		UUID:                generateUUID(),
		Email:               &email,
		PinHash:             string(hash),
		Username:            "keeps_pin",
		FailedLoginAttempts: 3,
	}
	if err := database.DB.Create(&user).Error; err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	claimToken := issueTestClaimToken(t, "email", email)
	response := performJSONRequest[AuthResponse](
		t,
		router,
		http.MethodPost,
		"/v1/auth/pin/reset",
		"",
		map[string]any{"claim_token": claimToken, "device_id": "device-keeps-pin"},
		http.StatusOK,
	)
	if response.Token == "" {
		t.Fatal("expected a session token from an OTP-only sign-in")
	}
	if response.User == nil || !response.User.HasPin {
		t.Fatalf("expected the existing PIN to survive, got %#v", response.User)
	}

	var stored models.User
	if err := database.DB.First(&stored, user.ID).Error; err != nil {
		t.Fatalf("failed to reload user: %v", err)
	}
	if stored.PinHash != string(hash) {
		t.Fatal("expected pin_hash to be untouched when no PIN was supplied")
	}
	if stored.FailedLoginAttempts != 0 {
		t.Fatalf("expected the failed-attempt counter to be cleared, got %d", stored.FailedLoginAttempts)
	}
}

func TestAuthPinResetAddsAPINToAPinlessAccount(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)

	email := "adds-pin@example.com"
	user := models.User{UUID: generateUUID(), Email: &email, Username: "adds_pin"}
	if err := database.DB.Create(&user).Error; err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	claimToken := issueTestClaimToken(t, "email", email)
	response := performJSONRequest[AuthResponse](
		t,
		router,
		http.MethodPost,
		"/v1/auth/pin/reset",
		"",
		map[string]any{"claim_token": claimToken, "pin": "2468", "device_id": "device-adds-pin"},
		http.StatusOK,
	)
	if response.User == nil || !response.User.HasPin {
		t.Fatalf("expected has_pin=true after setting one, got %#v", response.User)
	}

	performJSONRequest[AuthResponse](
		t,
		router,
		http.MethodPost,
		"/v1/auth/login",
		"",
		map[string]string{"identifier": email, "pin": "2468", "device_id": "device-adds-pin"},
		http.StatusOK,
	)
}

func TestAuthLoginRejectsEveryPINForAPinlessAccount(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)

	email := "pinless-login@example.com"
	user := models.User{UUID: generateUUID(), Email: &email, Username: "pinless_login"}
	if err := database.DB.Create(&user).Error; err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	body := performJSONRequest[map[string]any](
		t,
		router,
		http.MethodPost,
		"/v1/auth/login",
		"",
		map[string]string{"identifier": email, "pin": "0000", "device_id": "device-pinless"},
		http.StatusUnauthorized,
	)
	if body["error"] != "invalid_credentials" {
		t.Fatalf("expected invalid_credentials, got %#v", body)
	}
}
