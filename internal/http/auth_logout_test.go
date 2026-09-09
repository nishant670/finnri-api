package http

import (
	"net/http"
	"testing"

	"finnri/internal/database"
	"finnri/internal/models"
)

func TestLogoutRevokesOnlyCallingSession(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)
	first := performJSONRequest[AuthResponse](t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{"device_id": "logout-one"}, http.StatusOK)
	second := performJSONRequest[AuthResponse](t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{"device_id": "logout-one"}, http.StatusOK)

	performJSONRequest[map[string]any](t, router, http.MethodPost, "/v1/auth/logout", first.Token, nil, http.StatusOK)
	performJSONRequest[map[string]any](t, router, http.MethodGet, "/v1/accounts", first.Token, nil, http.StatusUnauthorized)
	performJSONRequest[[]models.Account](t, router, http.MethodGet, "/v1/accounts", second.Token, nil, http.StatusOK)
}

func TestRevokeAllSessionsRevokesEveryUserSession(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)
	first := performJSONRequest[AuthResponse](t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{"device_id": "logout-all"}, http.StatusOK)
	second := performJSONRequest[AuthResponse](t, router, http.MethodPost, "/v1/auth/guest", "", map[string]string{"device_id": "logout-all"}, http.StatusOK)

	response := performJSONRequest[map[string]any](t, router, http.MethodPost, "/v1/auth/sessions/revoke-all", second.Token, nil, http.StatusOK)
	if response["revoked"].(float64) < 2 {
		t.Fatalf("expected both sessions revoked, got %#v", response)
	}
	performJSONRequest[map[string]any](t, router, http.MethodGet, "/v1/accounts", first.Token, nil, http.StatusUnauthorized)
	performJSONRequest[map[string]any](t, router, http.MethodGet, "/v1/accounts", second.Token, nil, http.StatusUnauthorized)

	var active int64
	if err := database.DB.Model(&models.AuthSession{}).Where("revoked_at IS NULL").Count(&active).Error; err != nil || active != 0 {
		t.Fatalf("active sessions = %d, err = %v", active, err)
	}
}
