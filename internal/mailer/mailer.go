// Package mailer sends transactional email. It exists because OTP codes were
// generated, hashed and stored for months with nothing to carry them to the
// person signing up — authOtpSend returned {"message":"otp_sent"} and no mail
// ever left the process.
//
// The Sender interface is deliberately narrow: one method, one message. Adding
// SMS later means adding a sibling interface, not editing the call sites.
package mailer

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/mail"
	"strings"

	"finnri/internal/config"
)

// ErrNotConfigured is returned by the no-op sender. Callers must treat it as a
// send failure and surface it, never as a silent success — that silence is the
// exact defect this package was written to close.
var ErrNotConfigured = errors.New("mailer: no email provider configured")

// Message is one outbound email. Text is required; HTML is optional and, when
// present, is sent alongside Text as a multipart/alternative so that a client
// which cannot render HTML still shows the code.
type Message struct {
	To      string
	Subject string
	Text    string
	HTML    string
}

// Sender delivers a Message. An implementation must return a non-nil error
// when delivery was not accepted by the provider.
type Sender interface {
	// Send blocks until the provider has accepted the message or ctx expires.
	Send(ctx context.Context, msg Message) error
	// Name identifies the driver in logs and health output.
	Name() string
}

// Validate rejects a message the drivers cannot meaningfully send. It runs
// before any network call so a malformed address costs nothing.
func (m Message) Validate() error {
	if strings.TrimSpace(m.To) == "" {
		return errors.New("mailer: recipient is required")
	}
	if _, err := mail.ParseAddress(m.To); err != nil {
		return fmt.Errorf("mailer: invalid recipient %q: %w", m.To, err)
	}
	if strings.TrimSpace(m.Subject) == "" {
		return errors.New("mailer: subject is required")
	}
	if strings.TrimSpace(m.Text) == "" {
		return errors.New("mailer: text body is required")
	}
	// A header injected through the subject would let a caller add Bcc or
	// rewrite From. Nothing legitimate puts a newline in a subject line.
	if strings.ContainsAny(m.Subject, "\r\n") {
		return errors.New("mailer: subject must not contain newlines")
	}
	return nil
}

// FromConfig builds the sender named by EMAIL_PROVIDER. An unset or unknown
// provider yields the no-op sender, which fails every send with
// ErrNotConfigured rather than pretending to have delivered something.
func FromConfig(cfg *config.Config) Sender {
	from := Address{Name: cfg.EmailFromName, Email: cfg.EmailFromAddress}
	switch strings.ToLower(strings.TrimSpace(cfg.EmailProvider)) {
	case "smtp":
		return NewSMTPSender(SMTPOptions{
			Host:     cfg.SMTPHost,
			Port:     cfg.SMTPPort,
			Username: cfg.SMTPUsername,
			Password: cfg.SMTPPassword,
			TLSMode:  cfg.SMTPTLSMode,
			From:     from,
		})
	case "resend":
		return NewResendSender(ResendOptions{
			APIKey:  cfg.ResendAPIKey,
			BaseURL: cfg.ResendBaseURL,
			From:    from,
		})
	case "log":
		return NewLogSender(from)
	default:
		return NotConfigured{}
	}
}

// Address is a From identity. Name is optional.
type Address struct {
	Name  string
	Email string
}

// String renders the address for a From header, encoding the display name when
// it is not plain ASCII.
func (a Address) String() string {
	if strings.TrimSpace(a.Name) == "" {
		return a.Email
	}
	return fmt.Sprintf("%s <%s>", mime.QEncoding.Encode("utf-8", a.Name), a.Email)
}

func (a Address) valid() error {
	if strings.TrimSpace(a.Email) == "" {
		return errors.New("mailer: EMAIL_FROM_ADDRESS is required")
	}
	if _, err := mail.ParseAddress(a.Email); err != nil {
		return fmt.Errorf("mailer: invalid EMAIL_FROM_ADDRESS %q: %w", a.Email, err)
	}
	return nil
}

// NotConfigured is the zero-provider sender.
type NotConfigured struct{}

func (NotConfigured) Name() string { return "none" }

func (NotConfigured) Send(context.Context, Message) error { return ErrNotConfigured }
