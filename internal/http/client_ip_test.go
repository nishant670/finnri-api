package http

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// request builds a context whose peer is a proxy Gin trusts, which is the
// shape every request arrives in behind a managed platform.
func requestFromProxy(t *testing.T, forwarded string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, engine := gin.CreateTestContext(httptest.NewRecorder())
	if err := engine.SetTrustedProxies(nil); err != nil {
		t.Fatal(err)
	}
	c.Request = httptest.NewRequest("GET", "/v1/tools", nil)
	c.Request.RemoteAddr = "10.1.2.3:54321"
	if forwarded != "" {
		c.Request.Header.Set("X-Forwarded-For", forwarded)
	}
	return c
}

func TestClientIPTakesTheEntryTheImmediateProxyAppended(t *testing.T) {
	// One proxy: the last entry is the client, and it is the only one the
	// caller could not have written itself.
	c := requestFromProxy(t, "203.0.113.7")
	if got := clientIPFromHops(c, 1); got != "203.0.113.7" {
		t.Fatalf("single-entry chain: got %q", got)
	}

	// Two proxies: the client sits one further left.
	c = requestFromProxy(t, "203.0.113.7, 198.51.100.2")
	if got := clientIPFromHops(c, 2); got != "203.0.113.7" {
		t.Fatalf("two hops: got %q", got)
	}
}

func TestClientIPIgnoresAForgedForwardedForPrefix(t *testing.T) {
	// A caller inventing its own X-Forwarded-For to mint a fresh rate-limit
	// bucket: whatever it sends lands to the LEFT of the entry the proxy
	// appends, so it never becomes the key.
	first := requestFromProxy(t, "1.1.1.1, 203.0.113.7")
	second := requestFromProxy(t, "2.2.2.2, 203.0.113.7")

	if got := clientIPFromHops(first, 1); got != "203.0.113.7" {
		t.Fatalf("forged prefix leaked into the key: %q", got)
	}
	if clientIPFromHops(first, 1) != clientIPFromHops(second, 1) {
		t.Fatal("rotating the forged prefix produced a different key, which is the bypass this exists to close")
	}
}

func TestClientIPFallsBackWhenTheHeaderCannotAnswer(t *testing.T) {
	// No header: a direct connection, or local development.
	if got := clientIPFromHops(requestFromProxy(t, ""), 1); got != "10.1.2.3" {
		t.Fatalf("missing header: got %q", got)
	}
	// A chain shorter than hops claims. Reading past the front would take a
	// caller-supplied entry, so this falls back instead.
	if got := clientIPFromHops(requestFromProxy(t, "203.0.113.7"), 3); got != "10.1.2.3" {
		t.Fatalf("short chain: got %q", got)
	}
	// An empty entry where the client should be.
	if got := clientIPFromHops(requestFromProxy(t, "203.0.113.7, "), 1); got != "10.1.2.3" {
		t.Fatalf("empty entry: got %q", got)
	}
	// Hops disabled: the header is not believed at all.
	if got := clientIPFromHops(requestFromProxy(t, "203.0.113.7"), 0); got != "10.1.2.3" {
		t.Fatalf("hops disabled: got %q", got)
	}
}

func TestRequestClientIPFallsBackWithoutTheMiddleware(t *testing.T) {
	// Handlers exercised directly in tests never run resolveClientIP; they
	// must not all collapse onto one empty key.
	c := requestFromProxy(t, "203.0.113.7")
	if got := requestClientIP(c); got != "10.1.2.3" {
		t.Fatalf("bare context: got %q", got)
	}
	c.Set(clientIPContextKey, "203.0.113.7")
	if got := requestClientIP(c); got != "203.0.113.7" {
		t.Fatalf("after resolveClientIP: got %q", got)
	}
}
