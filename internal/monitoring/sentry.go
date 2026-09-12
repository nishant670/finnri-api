package monitoring

import (
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/getsentry/sentry-go"
	sentrygin "github.com/getsentry/sentry-go/gin"
	"github.com/gin-gonic/gin"
)

var configured atomic.Bool

var sensitiveKeys = []string{
	"amount",
	"title",
	"note",
	"notes",
	"merchant",
	"identifier",
	"phone",
	"email",
	"token",
	"claim_token",
	"otp",
	"pin",
	"transcript",
}

// Init configures crash reporting only when SENTRY_DSN is present. A deploy
// without the variable keeps the SDK and its Gin middleware completely inert.
func Init() error {
	configured.Store(false)
	dsn := strings.TrimSpace(os.Getenv("SENTRY_DSN"))
	if dsn == "" {
		return nil
	}

	environment := strings.TrimSpace(os.Getenv("SENTRY_ENVIRONMENT"))
	if environment == "" {
		environment = "production"
	}

	if err := sentry.Init(sentry.ClientOptions{
		Dsn:              dsn,
		Environment:      environment,
		EnableTracing:    false,
		TracesSampleRate: 0,
		SendDefaultPII:   false,
		BeforeSend:       sanitizeEvent,
		BeforeBreadcrumb: sanitizeBreadcrumb,
	}); err != nil {
		return err
	}
	configured.Store(true)
	return nil
}

// Configured reports whether Init installed a usable client.
func Configured() bool {
	return configured.Load()
}

// GinMiddleware captures request panics and repanics so Gin's existing
// Recovery middleware remains responsible for the response.
func GinMiddleware() gin.HandlerFunc {
	return sentrygin.New(sentrygin.Options{Repanic: true, WaitForDelivery: false})
}

// Flush gives queued events a bounded chance to leave during graceful exit.
func Flush(timeout time.Duration) bool {
	if !Configured() {
		return true
	}
	return sentry.Flush(timeout)
}

func sanitizeEvent(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
	if event.Request != nil {
		// Request metadata is useful; a personal-finance payload, query filter,
		// session cookie, or invite token is not.
		event.Request.URL = ""
		event.Request.Data = ""
		event.Request.QueryString = ""
		event.Request.Cookies = ""
	}
	event.Extra = redactMap(event.Extra)
	for name, context := range event.Contexts {
		event.Contexts[name] = redactMap(context)
	}
	for index, breadcrumb := range event.Breadcrumbs {
		event.Breadcrumbs[index] = sanitizeBreadcrumb(breadcrumb, nil)
	}
	return event
}

func sanitizeBreadcrumb(breadcrumb *sentry.Breadcrumb, _ *sentry.BreadcrumbHint) *sentry.Breadcrumb {
	if breadcrumb == nil || breadcrumb.Category == "console" {
		return nil
	}
	breadcrumb.Data = redactMap(breadcrumb.Data)
	return breadcrumb
}

func redactMap(values map[string]interface{}) map[string]interface{} {
	if values == nil {
		return nil
	}
	redacted := make(map[string]interface{}, len(values))
	for key, value := range values {
		if isSensitiveKey(key) {
			redacted[key] = "[redacted]"
			continue
		}
		redacted[key] = redactValue(value)
	}
	return redacted
}

func redactValue(value interface{}) interface{} {
	switch typed := value.(type) {
	case map[string]interface{}:
		return redactMap(typed)
	case []interface{}:
		redacted := make([]interface{}, len(typed))
		for index, item := range typed {
			redacted[index] = redactValue(item)
		}
		return redacted
	default:
		return value
	}
}

func isSensitiveKey(key string) bool {
	lower := strings.ToLower(key)
	for _, sensitive := range sensitiveKeys {
		if strings.Contains(lower, sensitive) {
			return true
		}
	}
	return false
}
