package http

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"finnri/internal/config"
	"finnri/internal/database"
	"finnri/internal/mailer"
	"finnri/internal/models"
)

// stubMailer records what would have been sent, and can be told to fail, so
// the tests below can assert on the code that actually reaches an inbox rather
// than on the response body alone.
type stubMailer struct {
	mu   sync.Mutex
	sent []mailer.Message
	err  error
}

func (s *stubMailer) Name() string { return "stub" }

func (s *stubMailer) Send(_ context.Context, msg mailer.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.sent = append(s.sent, msg)
	return nil
}

func (s *stubMailer) messages() []mailer.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]mailer.Message(nil), s.sent...)
}

// withMailer configures a deployment that has OTP sign-in switched on and a
// working mail sender. OTP is off by default now — Google and guest are the
// launch doors in — so the tests covering the delivery machinery have to ask
// for it. They still earn their place: the machinery is what comes back when
// email and SMS OTP ship together.
func withMailer(sender mailer.Sender) func(*Server, *config.Config) {
	return func(s *Server, cfg *config.Config) {
		s.mailer = sender
		cfg.OTPExpiresMinutes = 10
		cfg.AuthOTPEnabled = true
	}
}

// TestAuthOtpSendDeliversTheCodeThatVerifies is the regression guard for the
// defect this endpoint shipped with: it answered "otp_sent" while no mail
// existed. Asserting a 200 is not enough — the test pulls the code out of the
// delivered message and spends it on /otp/verify, which is the only way to
// prove the thing in the inbox is the thing the server will accept.
func TestAuthOtpSendDeliversTheCodeThatVerifies(t *testing.T) {
	useSmokeDatabase(t)
	sender := &stubMailer{}
	router := smokeRouter(t, withMailer(sender))

	performJSONRequest[map[string]any](
		t, router, http.MethodPost, "/v1/auth/otp/send", "",
		map[string]string{"identifier": "Signup@Example.com "}, http.StatusOK,
	)

	messages := sender.messages()
	if len(messages) != 1 {
		t.Fatalf("expected exactly one email, sent %d", len(messages))
	}
	// The identifier is normalised before it is used as a recipient.
	if messages[0].To != "signup@example.com" {
		t.Fatalf("expected normalised recipient, got %q", messages[0].To)
	}

	code := extractOTPCode(t, messages[0])
	claim := performJSONRequest[map[string]any](
		t, router, http.MethodPost, "/v1/auth/otp/verify", "",
		map[string]string{"identifier": "signup@example.com", "otp": code}, http.StatusOK,
	)
	token, _ := claim["claim_token"].(string)
	if !strings.HasPrefix(token, claimTokenPrefix) {
		t.Fatalf("emailed code did not verify into a claim token: %#v", claim)
	}
}

// TestAuthOtpSendSurfacesDeliveryFailure covers the second half of the defect:
// a failure must reach the caller, and must not leave a live code behind that
// nobody received but the cooldown still counts against.
func TestAuthOtpSendSurfacesDeliveryFailure(t *testing.T) {
	useSmokeDatabase(t)
	sender := &stubMailer{err: context.DeadlineExceeded}
	router := smokeRouter(t, withMailer(sender))

	body := performJSONRequest[map[string]any](
		t, router, http.MethodPost, "/v1/auth/otp/send", "",
		map[string]string{"identifier": "down@example.com"}, http.StatusBadGateway,
	)
	if body["error"] != "otp_send_failed" {
		t.Fatalf("expected otp_send_failed, got %#v", body)
	}

	var remaining int64
	if err := database.DB.Model(&models.AuthVerification{}).
		Where("identifier = ?", "down@example.com").Count(&remaining).Error; err != nil {
		t.Fatalf("failed to count verifications: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("a failed send left %d verification rows behind", remaining)
	}
}

// TestAuthOtpSendWithNoProviderConfiguredFails pins the default. A deployment
// that forgets EMAIL_PROVIDER must break loudly at the first signup, not go on
// claiming to have sent codes.
func TestAuthOtpSendWithNoProviderConfiguredFails(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t, func(_ *Server, cfg *config.Config) { cfg.AuthOTPEnabled = true })

	body := performJSONRequest[map[string]any](
		t, router, http.MethodPost, "/v1/auth/otp/send", "",
		map[string]string{"identifier": "nobody@example.com"}, http.StatusBadGateway,
	)
	if body["error"] != "otp_send_failed" {
		t.Fatalf("expected otp_send_failed with no provider, got %#v", body)
	}
}

func TestAuthOtpSendRejectsPhoneWhileSMSIsUnwired(t *testing.T) {
	useSmokeDatabase(t)
	sender := &stubMailer{}
	router := smokeRouter(t, withMailer(sender))

	body := performJSONRequest[map[string]any](
		t, router, http.MethodPost, "/v1/auth/otp/send", "",
		map[string]string{"identifier": "9876543210"}, http.StatusUnprocessableEntity,
	)
	if body["error"] != "otp_channel_unavailable" {
		t.Fatalf("expected otp_channel_unavailable, got %#v", body)
	}
	if len(sender.messages()) != 0 {
		t.Fatalf("a phone identifier must not send email")
	}
	var stored int64
	database.DB.Model(&models.AuthVerification{}).Where("identifier_type = ?", "phone").Count(&stored)
	if stored != 0 {
		t.Fatalf("a rejected phone request stored %d codes", stored)
	}
}

func TestAuthOtpSendThrottlesRepeatSendsToOneAddress(t *testing.T) {
	useSmokeDatabase(t)
	sender := &stubMailer{}
	router := smokeRouter(t, func(s *Server, cfg *config.Config) {
		withMailer(sender)(s, cfg)
		cfg.OTPResendCooldownSeconds = 60
	})

	performJSONRequest[map[string]any](
		t, router, http.MethodPost, "/v1/auth/otp/send", "",
		map[string]string{"identifier": "repeat@example.com"}, http.StatusOK,
	)

	response := performRawRequest(t, router, http.MethodPost, "/v1/auth/otp/send", "",
		strings.NewReader(`{"identifier":"repeat@example.com"}`))
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 on immediate resend, got %d: %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Retry-After") == "" {
		t.Fatalf("a throttled resend must tell the app when to retry")
	}
	if len(sender.messages()) != 1 {
		t.Fatalf("the throttled resend still sent mail: %d messages", len(sender.messages()))
	}
}

// TestOtpResendCooldownExpires proves the throttle is a window, not a lock.
func TestOtpResendCooldownExpires(t *testing.T) {
	useSmokeDatabase(t)
	created := time.Now().UTC().Add(-90 * time.Second)
	verification := models.AuthVerification{
		IdentifierType: "email",
		Identifier:     "cooled@example.com",
		OTPHash:        "irrelevant",
		OTPExpiresAt:   created.Add(10 * time.Minute),
		CreatedAt:      created,
	}
	if err := database.DB.Create(&verification).Error; err != nil {
		t.Fatalf("failed to seed verification: %v", err)
	}

	if _, blocked := otpResendCooldownRemaining("email", "cooled@example.com", 60, time.Now().UTC()); blocked {
		t.Fatalf("a 90-second-old code must not block a 60-second cooldown")
	}
	if remaining, blocked := otpResendCooldownRemaining("email", "cooled@example.com", 300, time.Now().UTC()); !blocked || remaining <= 0 {
		t.Fatalf("a 90-second-old code must block a 300-second cooldown, got remaining=%d blocked=%v", remaining, blocked)
	}
}

// extractOTPCode pulls the six digits out of a delivered message and checks
// that every part of the email agrees on them — a subject advertising one code
// and a body carrying another is the kind of defect that only shows up on a
// real phone.
func extractOTPCode(t *testing.T, msg mailer.Message) string {
	t.Helper()
	code := ""
	for _, field := range strings.Fields(msg.Subject) {
		if validOTPCode(field) {
			code = field
			break
		}
	}
	if code == "" {
		t.Fatalf("no six-digit code in subject %q", msg.Subject)
	}
	if !strings.Contains(msg.Text, code) {
		t.Fatalf("text body does not carry the subject's code %q: %s", code, msg.Text)
	}
	if msg.HTML != "" && !strings.Contains(msg.HTML, code) {
		t.Fatalf("html body does not carry the subject's code %q", code)
	}
	return code
}

// ---------------------------------------------------------------------------
// The OTP sign-in gate
// ---------------------------------------------------------------------------

// TestOtpSignInIsOffByDefault is the structural half of the production
// incident. A deployed environment was serving a static dev_otp, which turned
// an email address into an account takeover. With the flow off by default that
// is unreachable no matter what OTP_DEBUG_RESPONSE says, so the hole cannot be
// reopened by an environment variable alone.
func TestOtpSignInIsOffByDefault(t *testing.T) {
	useSmokeDatabase(t)
	sender := &stubMailer{}
	// Every ingredient of the old exploit, deliberately: a working mailer, and
	// debug responses on with a static code.
	router := smokeRouter(t, func(s *Server, cfg *config.Config) {
		s.mailer = sender
		cfg.OTPDebugResponse = true
		cfg.OTPDevCode = "123456"
	})

	for _, target := range []string{"/v1/auth/otp/send", "/v1/auth/otp/verify"} {
		response := performRawRequest(t, router, http.MethodPost, target, "",
			strings.NewReader(`{"identifier":"someone@example.com","otp":"123456"}`))
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s answered %d with OTP disabled: %s", target, response.Code, response.Body.String())
		}
		if !strings.Contains(response.Body.String(), "otp_sign_in_disabled") {
			t.Fatalf("%s should name the reason: %s", target, response.Body.String())
		}
		// The whole point: no code reaches the caller, and none is stored.
		if strings.Contains(response.Body.String(), "123456") {
			t.Fatalf("%s leaked a code while disabled: %s", target, response.Body.String())
		}
	}

	if len(sender.messages()) != 0 {
		t.Fatal("a disabled flow sent mail")
	}
	var stored int64
	database.DB.Model(&models.AuthVerification{}).Count(&stored)
	if stored != 0 {
		t.Fatalf("a disabled flow stored %d verification rows", stored)
	}
}

// TestOtpSignInGateNamesTheWorkingDoors keeps the client honest: a 503 with no
// guidance would leave the app showing "something went wrong" on the one screen
// a person cannot get past.
func TestOtpSignInGateNamesTheWorkingDoors(t *testing.T) {
	useSmokeDatabase(t)
	router := smokeRouter(t)

	body := performJSONRequest[map[string]any](
		t, router, http.MethodPost, "/v1/auth/otp/send", "",
		map[string]string{"identifier": "someone@example.com"}, http.StatusServiceUnavailable,
	)
	channels, _ := body["available_channels"].([]any)
	if len(channels) != 2 || channels[0] != "google" || channels[1] != "guest" {
		t.Fatalf("expected google and guest to be named, got %#v", body["available_channels"])
	}
}
