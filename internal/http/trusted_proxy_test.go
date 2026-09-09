package http

import (
	nethttp "net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"finnri/internal/config"
)

// rateLimit keys its buckets on ClientIP, so if Gin trusts every proxy a caller
// can hand itself a fresh bucket on each request by varying X-Forwarded-For.
// These tests pin the behaviour that closes that off.

func newRateLimitedRouter(t *testing.T, trusted []string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	if err := router.SetTrustedProxies(trusted); err != nil {
		t.Fatalf("SetTrustedProxies: %v", err)
	}
	cfg := &config.Config{RateLimitRPS: 1, RateLimitBurst: 2}
	router.Use(rateLimit(cfg, "test"))
	router.GET("/probe", func(c *gin.Context) { c.JSON(nethttp.StatusOK, gin.H{"ok": true}) })
	return router
}

func probe(router *gin.Engine, remoteAddr, forwardedFor string) int {
	request := httptest.NewRequest(nethttp.MethodGet, "/probe", nil)
	request.RemoteAddr = remoteAddr
	if forwardedFor != "" {
		request.Header.Set("X-Forwarded-For", forwardedFor)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response.Code
}

func TestRateLimitIgnoresSpoofedForwardedForBehindTrustedProxy(t *testing.T) {
	router := newRateLimitedRouter(t, config.DefaultTrustedProxies())

	// Arrives from the platform's edge proxy on the private network, which
	// appended the real caller 203.0.113.9. The leading entry is attacker
	// supplied and must not influence the bucket key.
	const proxy = "10.0.0.5:44321"
	spoofs := []string{
		"1.1.1.1, 203.0.113.9",
		"2.2.2.2, 203.0.113.9",
		"3.3.3.3, 203.0.113.9",
	}

	codes := make([]int, 0, len(spoofs))
	for _, forwarded := range spoofs {
		codes = append(codes, probe(router, proxy, forwarded))
	}

	// Burst is 2, so the third request from the same real caller must be
	// rejected even though it claimed a different X-Forwarded-For.
	if codes[0] != nethttp.StatusOK || codes[1] != nethttp.StatusOK {
		t.Fatalf("expected first two requests to pass, got %v", codes)
	}
	if codes[2] != nethttp.StatusTooManyRequests {
		t.Fatalf("spoofed X-Forwarded-For bypassed the rate limiter: got %d, want 429", codes[2])
	}
}

func TestRateLimitSeparatesDistinctCallersBehindTrustedProxy(t *testing.T) {
	router := newRateLimitedRouter(t, config.DefaultTrustedProxies())
	const proxy = "10.0.0.5:44321"

	if code := probe(router, proxy, "203.0.113.9"); code != nethttp.StatusOK {
		t.Fatalf("first caller rejected: %d", code)
	}
	if code := probe(router, proxy, "203.0.113.9"); code != nethttp.StatusOK {
		t.Fatalf("first caller rejected on second request: %d", code)
	}
	// A genuinely different caller behind the same proxy keeps its own bucket,
	// so testers do not share one another's limit.
	if code := probe(router, proxy, "198.51.100.4"); code != nethttp.StatusOK {
		t.Fatalf("second caller wrongly shares the first caller's bucket: %d", code)
	}
}

func TestRateLimitIgnoresForwardedForFromUntrustedPeer(t *testing.T) {
	router := newRateLimitedRouter(t, config.DefaultTrustedProxies())

	// Direct public connection, no proxy in front: X-Forwarded-For is entirely
	// caller controlled and must be discarded.
	const direct = "203.0.113.77:51000"
	codes := []int{
		probe(router, direct, "1.1.1.1"),
		probe(router, direct, "2.2.2.2"),
		probe(router, direct, "3.3.3.3"),
	}

	if codes[2] != nethttp.StatusTooManyRequests {
		t.Fatalf("untrusted peer bypassed the rate limiter: got %v, want third request 429", codes)
	}
}
