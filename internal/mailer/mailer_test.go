package mailer

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"finnri/internal/config"
)

func TestNotConfiguredNeverReportsSuccess(t *testing.T) {
	err := NotConfigured{}.Send(context.Background(), OTPEmail("a@example.com", "123456", 10))
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("expected ErrNotConfigured, got %v", err)
	}
}

func TestFromConfigSelectsDriver(t *testing.T) {
	for _, testCase := range []struct{ provider, want string }{
		{"smtp", "smtp"},
		{"resend", "resend"},
		{"log", "log"},
		{"", "none"},
		{"carrier-pigeon", "none"},
	} {
		got := FromConfig(&config.Config{EmailProvider: testCase.provider, EmailFromAddress: "no-reply@finnri.com"}).Name()
		if got != testCase.want {
			t.Fatalf("provider %q selected driver %q, want %q", testCase.provider, got, testCase.want)
		}
	}
}

func TestMessageValidateRejectsHeaderInjection(t *testing.T) {
	msg := Message{To: "a@example.com", Subject: "hi\r\nBcc: attacker@example.com", Text: "body"}
	if err := msg.Validate(); err == nil {
		t.Fatal("a subject containing CRLF must be rejected")
	}
}

func TestMessageValidateRejectsBadRecipient(t *testing.T) {
	for _, to := range []string{"", "not-an-address", "a@@example.com"} {
		msg := Message{To: to, Subject: "s", Text: "b"}
		if err := msg.Validate(); err == nil {
			t.Fatalf("recipient %q should have been rejected", to)
		}
	}
}

func TestBuildMIMEProducesMultipartWithBothBodies(t *testing.T) {
	msg := OTPEmail("user@example.com", "654321", 10)
	raw, err := buildMIME(Address{Name: "Finnri", Email: "no-reply@finnri.com"}, msg, time.Now())
	if err != nil {
		t.Fatalf("buildMIME failed: %v", err)
	}
	body := string(raw)
	for _, want := range []string{
		"From: Finnri <no-reply@finnri.com>",
		"To: user@example.com",
		"MIME-Version: 1.0",
		"multipart/alternative",
		"text/plain; charset=UTF-8",
		"text/html; charset=UTF-8",
		"Auto-Submitted: auto-generated",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("MIME output missing %q:\n%s", want, body)
		}
	}
	// The plain-text alternative must come first so a text-only client shows
	// the code rather than an empty message.
	if strings.Index(body, "text/plain") > strings.Index(body, "text/html") {
		t.Fatal("text/plain part must precede text/html")
	}
	if !strings.Contains(body, "654321") {
		t.Fatal("the code did not survive quoted-printable encoding")
	}
}

func TestOTPEmailAgreesWithItself(t *testing.T) {
	msg := OTPEmail("user@example.com", "042042", 7)
	if !strings.Contains(msg.Subject, "042042") {
		t.Fatalf("subject should carry the code for notification autofill: %q", msg.Subject)
	}
	if !strings.Contains(msg.Text, "042042") || !strings.Contains(msg.HTML, "042042") {
		t.Fatal("both bodies must carry the code")
	}
	if !strings.Contains(msg.Text, "7 minutes") {
		t.Fatalf("expiry should be stated in the body: %q", msg.Text)
	}
}

func TestResendSenderPostsExpectedPayload(t *testing.T) {
	var captured map[string]any
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"re_123"}`))
	}))
	defer server.Close()

	sender := NewResendSender(ResendOptions{
		APIKey:  "re_test_key",
		BaseURL: server.URL,
		From:    Address{Name: "Finnri", Email: "no-reply@finnri.com"},
	})
	if err := sender.Send(context.Background(), OTPEmail("user@example.com", "111222", 10)); err != nil {
		t.Fatalf("send failed: %v", err)
	}
	if authorization != "Bearer re_test_key" {
		t.Fatalf("unexpected authorization header %q", authorization)
	}
	if captured["subject"] != "111222 is your Finnri code" {
		t.Fatalf("unexpected subject %#v", captured["subject"])
	}
	recipients, _ := captured["to"].([]any)
	if len(recipients) != 1 || recipients[0] != "user@example.com" {
		t.Fatalf("unexpected recipients %#v", captured["to"])
	}
}

func TestResendSenderReportsProviderRejection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"domain is not verified"}`))
	}))
	defer server.Close()

	sender := NewResendSender(ResendOptions{APIKey: "k", BaseURL: server.URL, From: Address{Email: "no-reply@finnri.com"}})
	err := sender.Send(context.Background(), OTPEmail("user@example.com", "111222", 10))
	if err == nil {
		t.Fatal("a 422 from the provider must be an error")
	}
	// The reason has to survive into the log, or a domain-verification failure
	// looks identical to a network blip.
	if !strings.Contains(err.Error(), "domain is not verified") {
		t.Fatalf("provider reason lost: %v", err)
	}
}

func TestResendSenderRequiresAPIKey(t *testing.T) {
	sender := NewResendSender(ResendOptions{From: Address{Email: "no-reply@finnri.com"}})
	if err := sender.Send(context.Background(), OTPEmail("u@example.com", "1", 1)); err == nil {
		t.Fatal("a missing API key must fail")
	}
}

func TestSMTPSenderRefusesPasswordWithoutEncryption(t *testing.T) {
	sender := NewSMTPSender(SMTPOptions{
		Host: "localhost", Port: 1025, TLSMode: "none", Password: "hunter2",
		From: Address{Email: "no-reply@finnri.com"},
	})
	err := sender.Send(context.Background(), OTPEmail("u@example.com", "123456", 10))
	if err == nil || !strings.Contains(err.Error(), "unencrypted") {
		t.Fatalf("expected refusal to send a password in clear, got %v", err)
	}
}

func TestSMTPSenderDeliversToAServer(t *testing.T) {
	server := newFakeSMTPServer(t, false)
	defer server.close()

	sender := NewSMTPSender(SMTPOptions{
		Host: server.host, Port: server.port, TLSMode: "none",
		From: Address{Name: "Finnri", Email: "no-reply@finnri.com"},
	})
	if err := sender.Send(context.Background(), OTPEmail("user@example.com", "998877", 10)); err != nil {
		t.Fatalf("send failed: %v", err)
	}

	delivered := server.lastMessage()
	if !strings.Contains(delivered, "998877") {
		t.Fatalf("the server did not receive the code:\n%s", delivered)
	}
	if !strings.Contains(delivered, "To: user@example.com") {
		t.Fatalf("missing To header:\n%s", delivered)
	}
}

// TestSMTPSenderSurfacesRejectionAtDataClose is the subtle one: an SMTP server
// rejects a message in its reply to end-of-DATA, which Go reports from
// writer.Close, not from Write. A driver that ignored Close would report a
// delivery that never happened.
func TestSMTPSenderSurfacesRejectionAtDataClose(t *testing.T) {
	server := newFakeSMTPServer(t, true)
	defer server.close()

	sender := NewSMTPSender(SMTPOptions{
		Host: server.host, Port: server.port, TLSMode: "none",
		From: Address{Email: "no-reply@finnri.com"},
	})
	err := sender.Send(context.Background(), OTPEmail("user@example.com", "123456", 10))
	if err == nil {
		t.Fatal("a server rejecting the message must produce an error")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// fakeSMTPServer speaks just enough SMTP to accept one message.
type fakeSMTPServer struct {
	listener net.Listener
	host     string
	port     int
	received chan string
}

func newFakeSMTPServer(t *testing.T, rejectData bool) *fakeSMTPServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	addr := listener.Addr().(*net.TCPAddr)
	server := &fakeSMTPServer{listener: listener, host: "127.0.0.1", port: addr.Port, received: make(chan string, 1)}

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		write := func(line string) { _, _ = conn.Write([]byte(line + "\r\n")) }

		write("220 fake.finnri.test ESMTP")
		var body strings.Builder
		inData := false
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if err == io.EOF {
					return
				}
				return
			}
			trimmed := strings.TrimRight(line, "\r\n")

			if inData {
				if trimmed == "." {
					inData = false
					if rejectData {
						write("554 5.7.1 message rejected")
					} else {
						write("250 2.0.0 Ok: queued")
						server.received <- body.String()
					}
					continue
				}
				body.WriteString(trimmed + "\n")
				continue
			}

			switch {
			case strings.HasPrefix(trimmed, "EHLO"), strings.HasPrefix(trimmed, "HELO"):
				write("250-fake.finnri.test")
				write("250 8BITMIME")
			case strings.HasPrefix(trimmed, "MAIL FROM"), strings.HasPrefix(trimmed, "RCPT TO"):
				write("250 2.1.0 Ok")
			case trimmed == "DATA":
				inData = true
				write("354 End data with <CR><LF>.<CR><LF>")
			case trimmed == "QUIT":
				write("221 2.0.0 Bye")
				return
			default:
				write("250 2.0.0 Ok")
			}
		}
	}()
	return server
}

func (f *fakeSMTPServer) lastMessage() string {
	select {
	case msg := <-f.received:
		return msg
	case <-time.After(3 * time.Second):
		return ""
	}
}

func (f *fakeSMTPServer) close() { _ = f.listener.Close() }
