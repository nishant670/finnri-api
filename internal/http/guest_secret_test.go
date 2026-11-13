package http

import (
	"net/http"
	"strings"
	"testing"

	"finnri/internal/config"
	"finnri/internal/database"
	"finnri/internal/models"
)

const (
	testGuestSecret      = "4f3c2b1a09887766554433221100ffeeddccbbaa99887766554433221100abcd"
	otherTestGuestSecret = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func TestGuestSecretOpensTheGuestAndIsStoredHashed(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)
	signIn := func(body map[string]string, status int) AuthResponse {
		return performJSONRequest[AuthResponse](t, router, http.MethodPost, "/v1/auth/guest", "", body, status)
	}

	first := signIn(map[string]string{"device_id": "secret-guest-device", "guest_secret": testGuestSecret}, http.StatusOK)
	again := signIn(map[string]string{"device_id": "secret-guest-device", "guest_secret": testGuestSecret}, http.StatusOK)
	if first.User == nil || again.User == nil || first.User.ID != again.User.ID {
		t.Fatalf("the same secret must open the same guest: %#v vs %#v", first.User, again.User)
	}

	var stored models.User
	if err := database.DB.First(&stored, first.User.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.GuestSecretHash != hashGuestSecret(testGuestSecret) || strings.Contains(stored.GuestSecretHash, testGuestSecret) {
		t.Fatalf("the secret must be stored as its hash, got %q", stored.GuestSecretHash)
	}

	// The device id alone, or with somebody else's secret, opens nothing.
	signIn(map[string]string{"device_id": "secret-guest-device"}, http.StatusUnauthorized)
	signIn(map[string]string{"device_id": "secret-guest-device", "guest_secret": otherTestGuestSecret}, http.StatusUnauthorized)

	// The secret is what counts, even if the device id sent with it changes.
	moved := signIn(map[string]string{"device_id": "another-device-entirely", "guest_secret": testGuestSecret}, http.StatusOK)
	if moved.User == nil || moved.User.ID != first.User.ID {
		t.Fatalf("a known secret must open its guest whatever device id rides along")
	}
}

func TestLegacyGuestIsClaimedByItsFirstSecret(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)
	signIn := func(body map[string]string, status int) AuthResponse {
		return performJSONRequest[AuthResponse](t, router, http.MethodPost, "/v1/auth/guest", "", body, status)
	}

	// An app from before guest secrets: device id only, and it keeps working.
	legacy := signIn(map[string]string{"device_id": "legacy-guest-device"}, http.StatusOK)
	stillLegacy := signIn(map[string]string{"device_id": "legacy-guest-device"}, http.StatusOK)
	if legacy.User == nil || stillLegacy.User == nil || legacy.User.ID != stillLegacy.User.ID {
		t.Fatal("an older app must keep reaching its guest by device id")
	}

	// The updated app arrives with a secret and keeps the same records.
	claimed := signIn(map[string]string{"device_id": "legacy-guest-device", "guest_secret": testGuestSecret}, http.StatusOK)
	if claimed.User == nil || claimed.User.ID != legacy.User.ID {
		t.Fatal("the first secret must claim the existing guest, not create another")
	}

	// From here the device id is no longer a way in.
	signIn(map[string]string{"device_id": "legacy-guest-device"}, http.StatusUnauthorized)
	signIn(map[string]string{"device_id": "legacy-guest-device", "guest_secret": otherTestGuestSecret}, http.StatusUnauthorized)
}

func TestGuestSecretMustLookLikeOne(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)
	for _, bad := range []string{"short", strings.Repeat("a", 200), strings.Repeat("a", 60) + " b!"} {
		performJSONRequest[map[string]any](t, router, http.MethodPost, "/v1/auth/guest", "",
			map[string]string{"device_id": "bad-secret-device", "guest_secret": bad}, http.StatusBadRequest)
	}
}

func TestGuestSecretRequiredRefusesOlderApps(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t, func(_ *Server, cfg *config.Config) { cfg.GuestSecretRequired = true })

	performJSONRequest[map[string]any](t, router, http.MethodPost, "/v1/auth/guest", "",
		map[string]string{"device_id": "required-guest-device"}, http.StatusUpgradeRequired)
	performJSONRequest[AuthResponse](t, router, http.MethodPost, "/v1/auth/guest", "",
		map[string]string{"device_id": "required-guest-device", "guest_secret": testGuestSecret}, http.StatusOK)
}

func TestUserPayloadDoesNotCarryTheDeviceID(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)
	payload := performJSONRequest[map[string]any](t, router, http.MethodPost, "/v1/auth/guest", "",
		map[string]string{"device_id": "hidden-guest-device", "guest_secret": testGuestSecret}, http.StatusOK)
	user, _ := payload["user"].(map[string]any)
	for _, key := range []string{"device_id", "guest_secret_hash"} {
		if _, present := user[key]; present {
			t.Fatalf("the user payload must not include %q: %#v", key, user)
		}
	}
}

func TestGuestUpgradeNeedsTheGuestsOwnSessionWhenOneIsSent(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)
	stubGoogleVerifier(t, googleIdentity{Subject: "google-sub-upgrade", Email: "upgrade@example.com", EmailVerified: true})

	guest := performJSONRequest[AuthResponse](t, router, http.MethodPost, "/v1/auth/guest", "",
		map[string]string{"device_id": "upgrade-guest-device", "guest_secret": testGuestSecret}, http.StatusOK)
	stranger := performJSONRequest[AuthResponse](t, router, http.MethodPost, "/v1/auth/guest", "",
		map[string]string{"device_id": "stranger-guest-device", "guest_secret": otherTestGuestSecret}, http.StatusOK)
	upgrade := map[string]string{"id_token": "valid-google-token", "guest_uuid": guest.User.UUID, "device_id": "upgrade-guest-device"}

	// Somebody else's session, naming this guest's uuid: refused.
	performJSONRequest[map[string]any](t, router, http.MethodPost, "/v1/auth/google", stranger.Token, upgrade, http.StatusUnauthorized)
	var untouched models.User
	if err := database.DB.First(&untouched, guest.User.ID).Error; err != nil || !untouched.IsGuest {
		t.Fatalf("a refused upgrade must leave the guest as it was: %#v err=%v", untouched, err)
	}

	// The guest's own session: upgraded in place, and the secret is retired.
	upgraded := performJSONRequest[AuthResponse](t, router, http.MethodPost, "/v1/auth/google", guest.Token, upgrade, http.StatusOK)
	if upgraded.User == nil || upgraded.User.ID != guest.User.ID || upgraded.User.IsGuest {
		t.Fatalf("expected the guest to become the account, got %#v", upgraded.User)
	}
	var account models.User
	if err := database.DB.First(&account, guest.User.ID).Error; err != nil || account.GuestSecretHash != "" {
		t.Fatalf("an account must not keep a guest secret: %q err=%v", account.GuestSecretHash, err)
	}
}

func TestGuestUpgradeWithoutASessionFollowsTheMigrationSwitch(t *testing.T) {
	useSmokeDatabase(t)
	stubGoogleVerifier(t, googleIdentity{Subject: "google-sub-legacy-upgrade", Email: "legacy-upgrade@example.com", EmailVerified: true})

	// Required: an upgrade that presents no session is refused.
	strict := smokeRouter(t, func(_ *Server, cfg *config.Config) { cfg.GuestSecretRequired = true })
	guest := performJSONRequest[AuthResponse](t, strict, http.MethodPost, "/v1/auth/guest", "",
		map[string]string{"device_id": "legacy-upgrade-device", "guest_secret": testGuestSecret}, http.StatusOK)
	upgrade := map[string]string{"id_token": "valid-google-token", "guest_uuid": guest.User.UUID}
	performJSONRequest[map[string]any](t, strict, http.MethodPost, "/v1/auth/google", "", upgrade, http.StatusUnauthorized)

	// Window open (the default): an app from before this change still upgrades.
	lenient := smokeRouter(t)
	upgraded := performJSONRequest[AuthResponse](t, lenient, http.MethodPost, "/v1/auth/google", "", upgrade, http.StatusOK)
	if upgraded.User == nil || upgraded.User.ID != guest.User.ID {
		t.Fatalf("expected the older app's upgrade to succeed while the window is open, got %#v", upgraded.User)
	}
}
