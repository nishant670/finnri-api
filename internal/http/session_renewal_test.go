package http

import (
	"bytes"
	nethttp "net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"finnri/internal/database"
	"finnri/internal/models"
)

func TestRenewedSessionExpiry(t *testing.T) {
	const day = 24 * time.Hour
	ttl, maxLifetime := 7*day, 180*day
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)

	// More than half its life left: nothing to write.
	if _, ok := renewedSessionExpiry(now.Add(-day), now.Add(6*day), now, ttl, maxLifetime); ok {
		t.Fatal("a session with six of seven days left must not renew")
	}

	// Past halfway: a full TTL from now.
	renewed, ok := renewedSessionExpiry(now.Add(-5*day), now.Add(2*day), now, ttl, maxLifetime)
	if !ok || !renewed.Equal(now.Add(ttl)) {
		t.Fatalf("renewed = %v ok=%v, want %v", renewed, ok, now.Add(ttl))
	}

	// Close to the wall: only as far as the wall.
	created := now.Add(-178 * day)
	renewed, ok = renewedSessionExpiry(created, now.Add(day), now, ttl, maxLifetime)
	if !ok || !renewed.Equal(created.Add(maxLifetime)) {
		t.Fatalf("renewed = %v ok=%v, want the %v cap", renewed, ok, created.Add(maxLifetime))
	}

	// At the wall: never pulled earlier, never extended.
	created = now.Add(-180*day + time.Hour)
	if got, ok := renewedSessionExpiry(created, created.Add(maxLifetime), now, ttl, maxLifetime); ok {
		t.Fatalf("a session at its maximum lifetime renewed to %v", got)
	}
}

func sessionRenewalRouter() *gin.Engine {
	router := gin.New()
	router.GET("/v1/ping", AuthMiddleware(), func(c *gin.Context) { c.Status(nethttp.StatusNoContent) })
	return router
}

func sessionExpiry(t *testing.T, token string) time.Time {
	t.Helper()
	var session models.AuthSession
	if err := database.DB.Where("token_hash = ?", hashSessionToken(token)).First(&session).Error; err != nil {
		t.Fatal(err)
	}
	return session.ExpiresAt
}

func pingWithToken(router *gin.Engine, token string) int {
	request := httptest.NewRequest(nethttp.MethodGet, "/v1/ping", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response.Code
}

func TestUsingASessionPastHalfwayRenewsIt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	_, token := createBillingTestUserSession(t)
	router := sessionRenewalRouter()

	// Fresh: used, not rewritten.
	before := sessionExpiry(t, token)
	if code := pingWithToken(router, token); code != nethttp.StatusNoContent {
		t.Fatalf("status = %d", code)
	}
	if after := sessionExpiry(t, token); !after.Equal(before) {
		t.Fatalf("a fresh session was rewritten: %v -> %v", before, after)
	}

	// Two days left of seven: pushed out to about a week from now.
	soon := time.Now().UTC().Add(48 * time.Hour)
	if err := database.DB.Model(&models.AuthSession{}).Where("token_hash = ?", hashSessionToken(token)).
		UpdateColumn("expires_at", soon).Error; err != nil {
		t.Fatal(err)
	}
	if code := pingWithToken(router, token); code != nethttp.StatusNoContent {
		t.Fatalf("status = %d", code)
	}
	if left := time.Until(sessionExpiry(t, token)); left < 6*24*time.Hour {
		t.Fatalf("session was not renewed: %s left", left)
	}
}

func TestRevokedAndAdminSessionsDoNotRenew(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useSmokeDatabase(t)
	user, token := createBillingTestUserSession(t)
	router := sessionRenewalRouter()
	soon := time.Now().UTC().Add(time.Hour)

	// An admin-console session keeps its short life.
	adminToken, _, err := issueAdminSession(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.DB.Model(&models.AuthSession{}).Where("token_hash = ?", hashSessionToken(adminToken)).
		UpdateColumn("expires_at", soon).Error; err != nil {
		t.Fatal(err)
	}
	pingWithToken(router, adminToken)
	if left := time.Until(sessionExpiry(t, adminToken)); left > 2*time.Hour {
		t.Fatalf("an admin session was renewed: %s left", left)
	}

	// A revoked session is refused, and stays as it was.
	now := time.Now().UTC()
	if err := database.DB.Model(&models.AuthSession{}).Where("token_hash = ?", hashSessionToken(token)).
		UpdateColumns(map[string]any{"expires_at": soon, "revoked_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	if code := pingWithToken(router, token); code != nethttp.StatusUnauthorized {
		t.Fatalf("revoked session status = %d, want 401", code)
	}
	if left := time.Until(sessionExpiry(t, token)); left > 2*time.Hour {
		t.Fatalf("a revoked session was renewed: %s left", left)
	}
}

func TestUploadBodyRoutesAreRegistered(t *testing.T) {
	routes := registeredRouteSet(t)
	for path := range uploadBodyRoutes {
		if !routes["POST "+path] {
			t.Errorf("uploadBodyRoutes names %q, which is not a registered POST route; "+
				"a renamed upload route would silently fall under the 1 MB JSON limit", path)
		}
	}
}

func TestSignedInJSONBodiesAreBounded(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	group := router.Group("/v1", authenticatedBodyLimit())
	echo := func(c *gin.Context) { c.Status(nethttp.StatusNoContent) }
	group.POST("/entries", echo)
	group.POST("/upload", echo)

	oversized := bytes.Repeat([]byte("a"), maxAuthenticatedJSONBytes+1)
	for path, want := range map[string]int{
		"/v1/entries": nethttp.StatusRequestEntityTooLarge,
		// Upload routes set their own limit further down the chain.
		"/v1/upload": nethttp.StatusNoContent,
	} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(nethttp.MethodPost, path, bytes.NewReader(oversized)))
		if response.Code != want {
			t.Errorf("%s with %d bytes: status = %d, want %d", path, len(oversized), response.Code, want)
		}
	}
}

func TestUserRateLimitCountsPerAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/v1/ping", func(c *gin.Context) {
		if c.GetHeader("X-User") == "2" {
			c.Set("userID", uint(2))
		} else {
			c.Set("userID", uint(1))
		}
	}, userRateLimit(0.001, 2, "user"), func(c *gin.Context) { c.Status(nethttp.StatusNoContent) })

	call := func(user string) int {
		request := httptest.NewRequest(nethttp.MethodGet, "/v1/ping", nil)
		request.Header.Set("X-User", user)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response.Code
	}
	if a, b, c := call("1"), call("1"), call("1"); a != 204 || b != 204 || c != 429 {
		t.Fatalf("user 1 statuses = %d %d %d, want 204 204 429", a, b, c)
	}
	// Same address, different account: its own allowance.
	if code := call("2"); code != 204 {
		t.Fatalf("user 2 status = %d, want 204", code)
	}
}
