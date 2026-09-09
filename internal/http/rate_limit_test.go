package http

import (
	"fmt"
	nethttp "net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"finnri/internal/config"
)

func TestRateLimitRejectsRequestsAfterBurst(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(rateLimit(&config.Config{RateLimitRPS: 1, RateLimitBurst: 1}, "auth"))
	router.POST("/v1/auth/login", func(c *gin.Context) { c.Status(nethttp.StatusOK) })

	first := httptest.NewRequest(nethttp.MethodPost, "/v1/auth/login", nil)
	first.RemoteAddr = "203.0.113.10:1234"
	firstResponse := httptest.NewRecorder()
	router.ServeHTTP(firstResponse, first)

	if firstResponse.Code != nethttp.StatusOK {
		t.Fatalf("first status = %d, body = %s", firstResponse.Code, firstResponse.Body.String())
	}

	second := httptest.NewRequest(nethttp.MethodPost, "/v1/auth/login", nil)
	second.RemoteAddr = "203.0.113.10:1234"
	secondResponse := httptest.NewRecorder()
	router.ServeHTTP(secondResponse, second)

	if secondResponse.Code != nethttp.StatusTooManyRequests {
		t.Fatalf("second status = %d, body = %s", secondResponse.Code, secondResponse.Body.String())
	}
	if got := secondResponse.Header().Get("Retry-After"); got == "" {
		t.Fatal("expected Retry-After header")
	}
}

func TestMemoryRateLimiterCapsDistinctClientBuckets(t *testing.T) {
	limiter := newMemoryRateLimiter(1, 1)
	limiter.maxBuckets = 3

	for i := 0; i < 10; i++ {
		allowed, _ := limiter.allow(fmt.Sprintf("client-%d", i))
		if !allowed {
			t.Fatalf("new client %d should receive its first token", i)
		}
	}

	if got := len(limiter.buckets); got != limiter.maxBuckets {
		t.Fatalf("bucket count = %d, want %d", got, limiter.maxBuckets)
	}
}

func TestMemoryRateLimiterEvictsStaleBuckets(t *testing.T) {
	limiter := newMemoryRateLimiter(1, 1)
	current := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	limiter.now = func() time.Time { return current }
	limiter.staleAfter = time.Minute

	limiter.allow("stale-client")
	current = current.Add(2 * time.Minute)
	limiter.allow("current-client")

	if _, exists := limiter.buckets["stale-client"]; exists {
		t.Fatal("stale bucket was not evicted")
	}
	if _, exists := limiter.buckets["current-client"]; !exists {
		t.Fatal("current bucket is missing")
	}
}

func TestRateLimitScopesAuthAndAISeparately(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{RateLimitRPS: 1, RateLimitBurst: 1}
	router := gin.New()
	router.POST("/v1/auth/login", rateLimit(cfg, "auth"), func(c *gin.Context) { c.Status(nethttp.StatusOK) })
	router.POST("/v1/parse", rateLimit(cfg, "ai"), func(c *gin.Context) { c.Status(nethttp.StatusOK) })

	for i := 0; i < 2; i++ {
		request := httptest.NewRequest(nethttp.MethodPost, "/v1/auth/login", nil)
		request.RemoteAddr = "203.0.113.11:1234"
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
	}

	parseRequest := httptest.NewRequest(nethttp.MethodPost, "/v1/parse", nil)
	parseRequest.RemoteAddr = "203.0.113.11:1234"
	parseResponse := httptest.NewRecorder()
	router.ServeHTTP(parseResponse, parseRequest)

	if parseResponse.Code != nethttp.StatusOK {
		t.Fatalf("parse status = %d, body = %s", parseResponse.Code, parseResponse.Body.String())
	}
}
