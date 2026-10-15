package config

import (
	"os"
	"strings"
	"testing"
)

func TestLoadDoesNotDefaultCORSWildcard(t *testing.T) {
	t.Setenv("ALLOW_ORIGINS", "")

	cfg := Load()
	if cfg.AllowOrigins == "*" {
		t.Fatal("ALLOW_ORIGINS must not default to wildcard")
	}
	if cfg.AllowOrigins != "" {
		t.Fatalf("expected empty default ALLOW_ORIGINS, got %q", cfg.AllowOrigins)
	}
	if cfg.MaxJSONKB != 64 {
		t.Fatalf("expected default MAX_JSON_KB 64, got %d", cfg.MaxJSONKB)
	}
	if cfg.OpenAIStatementMaxTokens != 4000 {
		t.Fatalf("expected statement output limit 4000, got %d", cfg.OpenAIStatementMaxTokens)
	}
	if cfg.AIFreeCostPerUserAlertUSDMicros != 100000 {
		t.Fatalf("expected default free cost alert 100000 micros, got %d", cfg.AIFreeCostPerUserAlertUSDMicros)
	}
	if cfg.AIParseDisabled {
		t.Fatal("AI_PARSE_DISABLED must default to false")
	}
	if cfg.AIProviderFailureThreshold != 5 || cfg.AIProviderCircuitBreakerSeconds != 120 {
		t.Fatalf("unexpected provider circuit breaker defaults: %#v", cfg)
	}
	if cfg.AuthSessionTTLDays != 7 {
		t.Fatalf("expected default auth session TTL 7 days, got %d", cfg.AuthSessionTTLDays)
	}
	if !cfg.MaintenanceJobsEnabled || cfg.MaintenanceIntervalHours != 24 || cfg.AnonymousGuestRetentionDays != 90 {
		t.Fatalf("unexpected maintenance defaults: %#v", cfg)
	}
}

func TestLoadBoundsAuthSessionTTL(t *testing.T) {
	t.Setenv("AUTH_SESSION_TTL_DAYS", "14")
	if got := Load().AuthSessionTTLDays; got != 14 {
		t.Fatalf("expected configured auth session TTL 14 days, got %d", got)
	}

	for _, invalid := range []string{"0", "31", "invalid"} {
		t.Setenv("AUTH_SESSION_TTL_DAYS", invalid)
		if got := Load().AuthSessionTTLDays; got != 7 {
			t.Fatalf("expected invalid TTL %q to fall back to 7 days, got %d", invalid, got)
		}
	}
}

func TestLoadReadsAndNormalizesPublicWebOrigin(t *testing.T) {
	t.Setenv("WEB_BASE_URL", "  https://finnri.example/  ")
	cfg := Load()
	if cfg.WebBaseURL != "https://finnri.example" {
		t.Fatalf("expected normalized WEB_BASE_URL, got %q", cfg.WebBaseURL)
	}
	if cfg.WebhookRateLimitRPS != 50 || cfg.WebhookRateLimitBurst != 100 {
		t.Fatalf("unexpected webhook limiter defaults: %v/%d", cfg.WebhookRateLimitRPS, cfg.WebhookRateLimitBurst)
	}
}

func TestEnvExampleContainsRequiredRedactedVariables(t *testing.T) {
	contents, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	values := parseEnvExample(string(contents))

	required := []string{
		"PORT",
		"TZ_DEFAULT",
		"ALLOW_ORIGINS",
		"OPENAI_API_KEY",
		"OPENAI_BASE_URL",
		"OPENAI_LLM_MODEL",
		"OPENAI_WHISPER_MODEL",
		"OPENAI_MAX_OUTPUT_TOKENS",
		"OPENAI_STATEMENT_MAX_OUTPUT_TOKENS",
		"AI_PARSE_DISABLED",
		"AI_UNPAID_MAX_VOICE_BYTES",
		"AI_FAILED_PARSE_COOLDOWN_THRESHOLD",
		"AI_FAILED_PARSE_COOLDOWN_WINDOW_MINUTES",
		"AI_FAILED_PARSE_COOLDOWN_MINUTES",
		"AI_PROVIDER_FAILURE_THRESHOLD",
		"AI_PROVIDER_CIRCUIT_BREAKER_SECONDS",
		"AI_DAILY_COST_ALERT_USD_MICROS",
		"AI_ABUSE_DAILY_CREDITS_THRESHOLD",
		"AI_FREE_COST_PER_USER_ALERT_USD_MICROS",
		"MAINTENANCE_JOBS_ENABLED",
		"MAINTENANCE_INTERVAL_HOURS",
		"ANONYMOUS_GUEST_RETENTION_DAYS",
		"TRUSTED_PROXIES",
		"REQUEST_TIMEOUT_SECONDS",
		"RATE_LIMIT_RPS",
		"RATE_LIMIT_BURST",
		"WEBHOOK_RATE_LIMIT_RPS",
		"WEBHOOK_RATE_LIMIT_BURST",
		"MAX_JSON_KB",
		"MAX_UPLOAD_MB",
		"MAX_TRANSCRIPT_CHARS",
		"OTP_DEBUG_RESPONSE",
		"OTP_DEV_CODE",
		"OTP_EXPIRES_MINUTES",
		"AUTH_SESSION_TTL_DAYS",
		"CLAIM_TOKEN_EXPIRES_MINUTES",
		"DB_HOST",
		"DB_PORT",
		"DB_USER",
		"DB_PASSWORD",
		"DB_NAME",
		"DB_SSLMODE",
		"DATABASE_URL",
		"WEB_BASE_URL",
	}

	for _, key := range required {
		if _, ok := values[key]; !ok {
			t.Fatalf(".env.example missing %s", key)
		}
	}
	for _, secretKey := range []string{"OPENAI_API_KEY", "DB_PASSWORD", "DATABASE_URL"} {
		if strings.TrimSpace(values[secretKey]) != "" {
			t.Fatalf("%s must be redacted in .env.example", secretKey)
		}
	}
}

func parseEnvExample(contents string) map[string]string {
	values := map[string]string{}
	for _, line := range strings.Split(contents, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		values[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return values
}

// Staging's WEB_BASE_URL held a list, and every checkout opened a page on the
// host "finnri.app,https". Anything but one origin is refused.
func TestWebBaseURLMustBeOneOrigin(t *testing.T) {
	cases := map[string]string{
		"https://staging.finnri.app": "https://staging.finnri.app",
		" https://finnri.app/ ":      "https://finnri.app",
		"http://localhost:3000":      "http://localhost:3000",
		"":                           "",
		"https://finnri.app,https://staging.finnri.app": "",
		"https://finnri.app https://staging.finnri.app": "",
		"finnri.app":             "",
		"https://finnri.app/pay": "",
		"ftp://finnri.app":       "",
	}
	for raw, want := range cases {
		if got := webBaseURL(raw); got != want {
			t.Errorf("webBaseURL(%q) = %q, want %q", raw, got, want)
		}
	}
}
