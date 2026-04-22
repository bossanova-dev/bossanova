// Package email provides a small Mailer abstraction for sending
// transactional email. Implementations are stdlib-only: no third-party SDK.
package email

import "context"

// Mailer sends a single email. Implementations must be safe for concurrent use.
type Mailer interface {
	Send(ctx context.Context, to, subject, htmlBody string) error
}
