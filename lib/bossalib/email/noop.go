package email

import (
	"context"

	"github.com/rs/zerolog/log"
)

// NoopMailer satisfies Mailer by logging and discarding the message.
// Used when no email provider is configured (e.g. dev without a Resend key).
type NoopMailer struct{}

// NewNoopMailer returns a Mailer that does nothing but log the call.
func NewNoopMailer() *NoopMailer {
	return &NoopMailer{}
}

// Send logs the fact that an email would have been sent and returns nil.
func (*NoopMailer) Send(_ context.Context, to, subject, _ string) error {
	log.Warn().
		Str("to", to).
		Str("subject", subject).
		Msg("email: noop mailer — not delivered (no provider configured)")
	return nil
}
