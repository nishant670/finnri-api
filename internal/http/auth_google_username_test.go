package http

import (
	"net/http"
	"testing"

	"finnri/internal/database"
	"finnri/internal/models"
)

// Every Google sign-in used to be named `User_2c9f4a1b`, and the app printed
// that on Home and Profile. The ID token carries a display name whenever the
// `profile` scope is granted, which the app always asks for.
func TestAuthGoogleNamesUserFromGoogleProfile(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)
	stubGoogleVerifier(t, googleIdentity{
		Subject:       "google-sub-named",
		Email:         "named@example.com",
		EmailVerified: true,
		Name:          "Nishant Munjal",
	})

	response := performJSONRequest[AuthResponse](
		t,
		router,
		http.MethodPost,
		"/v1/auth/google",
		"",
		map[string]string{"id_token": "valid-google-token"},
		http.StatusOK,
	)

	if response.User == nil || response.User.Username != "Nishant Munjal" {
		t.Fatalf("expected the Google display name as username, got %#v", response.User)
	}
}

// A Workspace account can withhold the name. The address is still a better
// greeting than a UUID.
func TestAuthGoogleFallsBackToEmailLocalPart(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)
	stubGoogleVerifier(t, googleIdentity{
		Subject:       "google-sub-nameless",
		Email:         "nishant.munjal@example.com",
		EmailVerified: true,
	})

	response := performJSONRequest[AuthResponse](
		t,
		router,
		http.MethodPost,
		"/v1/auth/google",
		"",
		map[string]string{"id_token": "valid-google-token"},
		http.StatusOK,
	)

	if response.User == nil || response.User.Username != "nishant munjal" {
		t.Fatalf("expected the address local part as username, got %#v", response.User)
	}
}

// Two people can share a name; the column cannot.
func TestAuthGoogleDisambiguatesATakenName(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)
	taken := "taken@example.com"
	if err := database.DB.Create(&models.User{
		UUID:     generateUUID(),
		Email:    &taken,
		Username: "Nishant Munjal",
	}).Error; err != nil {
		t.Fatalf("failed to seed the existing name: %v", err)
	}
	stubGoogleVerifier(t, googleIdentity{
		Subject:       "google-sub-collision",
		Email:         "other@example.com",
		EmailVerified: true,
		Name:          "Nishant Munjal",
	})

	response := performJSONRequest[AuthResponse](
		t,
		router,
		http.MethodPost,
		"/v1/auth/google",
		"",
		map[string]string{"id_token": "valid-google-token"},
		http.StatusOK,
	)

	if response.User == nil || response.User.Username != "Nishant Munjal 2" {
		t.Fatalf("expected a disambiguated username, got %#v", response.User)
	}
}

// The accounts already created as `User_…` are the reason this backfills rather
// than only naming new ones: the person who reported this is signed in as one.
func TestAuthGoogleBackfillsAGeneratedUsername(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)
	email := "backfill@example.com"
	subject := "google-sub-backfill"
	existing := models.User{
		UUID:          generateUUID(),
		Email:         &email,
		GoogleSubject: &subject,
		Username:      "User_2c9f4a1b",
	}
	if err := database.DB.Create(&existing).Error; err != nil {
		t.Fatalf("failed to seed the generated username: %v", err)
	}
	stubGoogleVerifier(t, googleIdentity{
		Subject:       subject,
		Email:         email,
		EmailVerified: true,
		Name:          "Nishant Munjal",
	})

	response := performJSONRequest[AuthResponse](
		t,
		router,
		http.MethodPost,
		"/v1/auth/google",
		"",
		map[string]string{"id_token": "valid-google-token"},
		http.StatusOK,
	)

	if response.User == nil || response.User.ID != existing.ID {
		t.Fatalf("expected the same account back, got %#v", response.User)
	}
	if response.User.Username != "Nishant Munjal" {
		t.Fatalf("expected the generated username to be backfilled, got %q", response.User.Username)
	}
}

// A name someone typed into Edit Profile is theirs. Signing in again must not
// quietly replace it with whatever Google currently holds.
func TestAuthGoogleKeepsAChosenUsername(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)
	email := "chosen@example.com"
	subject := "google-sub-chosen"
	existing := models.User{
		UUID:          generateUUID(),
		Email:         &email,
		GoogleSubject: &subject,
		Username:      "moneybags",
	}
	if err := database.DB.Create(&existing).Error; err != nil {
		t.Fatalf("failed to seed the chosen username: %v", err)
	}
	stubGoogleVerifier(t, googleIdentity{
		Subject:       subject,
		Email:         email,
		EmailVerified: true,
		Name:          "Nishant Munjal",
	})

	response := performJSONRequest[AuthResponse](
		t,
		router,
		http.MethodPost,
		"/v1/auth/google",
		"",
		map[string]string{"id_token": "valid-google-token"},
		http.StatusOK,
	)

	if response.User == nil || response.User.Username != "moneybags" {
		t.Fatalf("expected the chosen username to survive sign-in, got %#v", response.User)
	}
}
