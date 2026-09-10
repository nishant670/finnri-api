package http

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"finnri/internal/config"
	"finnri/internal/database"
	"finnri/internal/models"
)

// lastActiveWriteInterval bounds how often a request refreshes users.last_active_at.
const lastActiveWriteInterval = 5 * time.Minute

func AuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.AbortWithStatusJSON(401, gin.H{"error": "authorization_header_missing"})
			return
		}

		parts := strings.Split(authHeader, " ")
		if len(parts) != 2 || parts[0] != "Bearer" {
			c.AbortWithStatusJSON(401, gin.H{"error": "authorization_header_invalid"})
			return
		}

		token := strings.TrimSpace(parts[1])
		if token == "" {
			c.AbortWithStatusJSON(401, gin.H{"error": "invalid_token"})
			return
		}

		var session models.AuthSession
		if err := database.DB.Preload("User").
			Where("token_hash = ? AND revoked_at IS NULL AND expires_at > ?", hashSessionToken(token), time.Now().UTC()).
			Limit(1).
			Find(&session).Error; err != nil {
			c.AbortWithStatusJSON(500, gin.H{"error": "auth_session_lookup_failed"})
			return
		}
		if session.ID == 0 {
			c.AbortWithStatusJSON(401, gin.H{"error": "invalid_or_expired_session"})
			return
		}
		user := session.User
		if user.ID == 0 {
			c.AbortWithStatusJSON(401, gin.H{"error": "invalid_session_user"})
			return
		}
		// Store user in context
		c.Set("user", &user)
		c.Set("userID", user.ID)
		c.Set("authSessionID", session.ID)
		// Coarse enough for "last active" on the admin console, and it keeps the
		// hottest path in the product from issuing a row update on every single
		// authenticated request.
		now := time.Now().UTC()
		if user.LastActiveAt == nil || now.Sub(*user.LastActiveAt) >= lastActiveWriteInterval {
			_ = database.DB.Model(&models.User{}).Where("id = ?", user.ID).UpdateColumn("last_active_at", now).Error
			user.LastActiveAt = &now
		}

		c.Next()
	}
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

type memoryRateLimiter struct {
	mu         sync.Mutex
	rps        float64
	burst      int
	buckets    map[string]*tokenBucket
	now        func() time.Time
	maxBuckets int
	staleAfter time.Duration
	lastSweep  time.Time
}

func newMemoryRateLimiter(rps float64, burst int) *memoryRateLimiter {
	refillWindow := time.Duration(float64(time.Second) * float64(burst) / math.Max(rps, 0.001))
	staleAfter := 10 * refillWindow
	if staleAfter < 10*time.Minute {
		staleAfter = 10 * time.Minute
	}
	return &memoryRateLimiter{
		rps:        rps,
		burst:      burst,
		buckets:    make(map[string]*tokenBucket),
		now:        time.Now,
		maxBuckets: 10_000,
		staleAfter: staleAfter,
	}
}

func (l *memoryRateLimiter) allow(key string) (bool, int) {
	if l.rps <= 0 || l.burst <= 0 {
		return true, 0
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.evictStale(now)
	bucket, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= l.maxBuckets {
			l.evictOldest()
		}
		l.buckets[key] = &tokenBucket{tokens: float64(l.burst - 1), last: now}
		return true, 0
	}

	elapsed := now.Sub(bucket.last).Seconds()
	if elapsed > 0 {
		bucket.tokens = math.Min(float64(l.burst), bucket.tokens+elapsed*l.rps)
		bucket.last = now
	}

	if bucket.tokens >= 1 {
		bucket.tokens--
		return true, 0
	}

	retryAfter := int(math.Ceil((1 - bucket.tokens) / l.rps))
	if retryAfter < 1 {
		retryAfter = 1
	}
	return false, retryAfter
}

func (l *memoryRateLimiter) evictStale(now time.Time) {
	if !l.lastSweep.IsZero() && now.Sub(l.lastSweep) < time.Minute {
		return
	}
	for key, bucket := range l.buckets {
		if now.Sub(bucket.last) > l.staleAfter {
			delete(l.buckets, key)
		}
	}
	l.lastSweep = now
}

func (l *memoryRateLimiter) evictOldest() {
	var oldestKey string
	var oldestTime time.Time
	for key, bucket := range l.buckets {
		if oldestKey == "" || bucket.last.Before(oldestTime) {
			oldestKey = key
			oldestTime = bucket.last
		}
	}
	if oldestKey != "" {
		delete(l.buckets, oldestKey)
	}
}

// clientIPContextKey holds the address every per-client decision keys on: the
// rate-limit buckets, the guest trial grant, and the hashed IP in the admin
// audit log.
const clientIPContextKey = "clientIP"

// resolveClientIP settles the caller's address once per request.
//
// gin.Context.ClientIP() answers this correctly only where it can recognise the
// proxy in front of it, which means knowing that proxy's address range.
// Railway's edge does not connect from the private ranges TRUSTED_PROXIES
// defaults to, and its address is neither stable nor documented, so ClientIP()
// resolved to a value that differed from one request to the next. Every request
// therefore opened its own rate-limit bucket and nothing was ever limited: 120
// concurrent requests against production on 2026-09-10 drew zero 429s with the
// limits set to 5 rps and a burst of 10.
//
// Counting hops from the right of X-Forwarded-For needs no knowledge of the
// proxy at all. The last entry is the one the immediate proxy appended, and it
// is the only entry a caller cannot choose - anything a caller sends itself
// arrives to the left of it. TRUSTED_PROXY_HOPS is how many proxies stand
// between the client and this process: 1 behind Railway, and 0 for a process
// exposed directly to the internet, where the header must not be believed at
// all and Gin's own answer is the honest one.
func resolveClientIP(cfg *config.Config) gin.HandlerFunc {
	hops := cfg.TrustedProxyHops
	return func(c *gin.Context) {
		c.Set(clientIPContextKey, clientIPFromHops(c, hops))
		c.Next()
	}
}

// clientIPFromHops takes the hops'th entry counting back from the end of
// X-Forwarded-For, and falls back to Gin whenever the header cannot supply one:
// no header at all, a chain shorter than hops claims, or an empty entry. A
// wrong hops count fails loudly rather than silently - too many and every
// caller shares one bucket, which shows up immediately as blanket 429s.
func clientIPFromHops(c *gin.Context, hops int) string {
	if hops <= 0 {
		return c.ClientIP()
	}
	forwarded := c.GetHeader("X-Forwarded-For")
	if forwarded == "" {
		return c.ClientIP()
	}
	parts := strings.Split(forwarded, ",")
	index := len(parts) - hops
	if index < 0 || index >= len(parts) {
		return c.ClientIP()
	}
	if ip := strings.TrimSpace(parts[index]); ip != "" {
		return ip
	}
	return c.ClientIP()
}

// requestClientIP reads what resolveClientIP settled. Handlers reached without
// that middleware - every test that builds a bare context - still get Gin's
// answer rather than an empty key that would pool them all together.
func requestClientIP(c *gin.Context) string {
	if value, ok := c.Get(clientIPContextKey); ok {
		if ip, ok := value.(string); ok && ip != "" {
			return ip
		}
	}
	return c.ClientIP()
}

func rateLimit(cfg *config.Config, scope string) gin.HandlerFunc {
	return rateLimitAt(cfg.RateLimitRPS, cfg.RateLimitBurst, scope)
}

// The admin console fetches five or six endpoints per page, and behind the web
// BFF every admin shares one source IP. The product-wide 5 rps / burst 10 turned
// an ordinary page load into partial failures with a "rate_limited" banner.
func adminRateLimit(cfg *config.Config) gin.HandlerFunc {
	return rateLimitAt(cfg.AdminRateLimitRPS, cfg.AdminRateLimitBurst, "admin")
}

func webhookRateLimit(cfg *config.Config) gin.HandlerFunc {
	return rateLimitAt(cfg.WebhookRateLimitRPS, cfg.WebhookRateLimitBurst, "billing-webhook")
}

func rateLimitAt(rps float64, burst int, scope string) gin.HandlerFunc {
	limiter := newMemoryRateLimiter(rps, burst)
	return func(c *gin.Context) {
		key := scope + ":" + requestClientIP(c)
		ok, retryAfter := limiter.allow(key)
		if !ok {
			c.Header("Retry-After", strconv.Itoa(retryAfter))
			c.AbortWithStatusJSON(429, gin.H{"error": "rate_limited"})
			return
		}
		c.Next()
	}
}

func requestLimits(maxBytes int64, timeoutSec int) gin.HandlerFunc {
	return func(c *gin.Context) {
		if maxBytes > 0 && c.Request.ContentLength > maxBytes {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request_body_too_large"})
			return
		}
		if maxBytes > 0 && c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		}

		if timeoutSec > 0 {
			ctx, cancel := context.WithTimeout(c.Request.Context(), time.Duration(timeoutSec)*time.Second)
			defer cancel()
			c.Request = c.Request.WithContext(ctx)
		}

		c.Next()
	}
}

func jsonRequestLimits(cfg *config.Config) gin.HandlerFunc {
	return requestLimits(cfg.MaxJSONKB*1024, cfg.ReqTimeoutSec)
}

func uploadRequestLimits(cfg *config.Config) gin.HandlerFunc {
	return requestLimits(cfg.MaxUploadMB*1024*1024, cfg.ReqTimeoutSec)
}

func requestBodyTooLarge(err error) bool {
	var maxBytesErr *http.MaxBytesError
	return errors.As(err, &maxBytesErr)
}
