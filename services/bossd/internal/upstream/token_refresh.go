package upstream

import (
	"context"
	"errors"
	"fmt"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// runTokenRefresher polls the TokenProvider on refreshInterval and calls
// Refresh whenever the cached token is within refreshThreshold of expiry.
// On successful refresh it emits a DaemonEvent_TokenRefresh on outbound
// so bosso re-verifies the JWT against WorkOS JWKS and updates its
// auth context for subsequent commands (decision #2).
//
// Returns an error when Refresh itself fails. Per the design, a transient
// refresh failure closes the stream — the outer Run loop reconnects, which
// forces a fresh register/handshake with whatever token is available. A
// terminal re-login failure (either BOS-659 state: an unconfirmed exchange
// outcome or an authoritative rejection) is different: it marks the shared
// AuthState first, so the loop pauses for `boss login` instead of reconnecting.
// When the TokenProvider is nil, the function blocks on ctx only (used
// by tests that don't exercise the refresh path).
func (c *StreamClient) runTokenRefresher(ctx context.Context, outbound chan<- *pb.DaemonEvent) error {
	if c.tokenProvider == nil {
		<-ctx.Done()
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-c.clock.After(c.refreshInterval):
		}

		expiresAt := c.tokenProvider.ExpiresAt()
		if expiresAt.IsZero() {
			// Unknown expiry — skip. Don't treat as error; the caller
			// may simply be using a static token for dev.
			continue
		}
		remaining := expiresAt.Sub(c.clock.Now())
		if remaining > c.refreshThreshold {
			continue
		}

		c.logger.Debug().
			Dur("remaining", remaining).
			Msg("refreshing upstream token")

		newTok, err := c.tokenProvider.Refresh(ctx)
		if err != nil {
			// BOS-659: both terminal re-login states (an ambiguous outcome
			// and an authoritative rejection) compose with ErrAuthExpired.
			// Mark the shared AuthState BEFORE returning: the outer Run loop
			// checks NeedsLogin ahead of its error branch, so marking first
			// is what makes this an intentional pause (no stream-error
			// metric, no backoff ramp, no re-dial) instead of a reconnect.
			// Marking also cancels the live stream through the needs-login
			// watcher, so the pause is immediate rather than one tick late.
			if errors.Is(err, ErrAuthExpired) && c.authState != nil {
				if c.authState.MarkNeedsLogin() {
					logReloginPause(&c.logger, "", err)
				}
			}
			return fmt.Errorf("token refresh: %w", err)
		}
		if newTok == "" {
			// Refresh returned an empty token with no error. Treat as
			// a soft failure — try again on the next tick rather than
			// closing the stream for no reason.
			c.logger.Warn().Msg("token refresh returned empty token")
			continue
		}

		// Guard against the zero-TTL refresh loop: a successful refresh that
		// still leaves the token at/under expiry means upstream handed back a
		// token we can't stay authenticated with (historically an expires_in:0
		// response). Surface it loudly — this previously spun silently every
		// refreshInterval and starved webhook delivery.
		if newExp := c.tokenProvider.ExpiresAt(); !newExp.IsZero() {
			if newRemaining := newExp.Sub(c.clock.Now()); newRemaining <= 0 {
				c.logger.Warn().
					Dur("remaining", newRemaining).
					Msg("upstream token still expired immediately after refresh; check token exp claim / expires_in")
			}
		}

		ev := &pb.DaemonEvent{
			Event: &pb.DaemonEvent_TokenRefresh{
				TokenRefresh: &pb.TokenRefresh{AccessToken: newTok},
			},
		}
		select {
		case outbound <- ev:
		case <-ctx.Done():
			return nil
		case <-c.clock.After(5 * time.Second):
			// Outbound should never block this long — if it does, the
			// stream is almost certainly dead already. Return so the
			// outer loop notices.
			return fmt.Errorf("token refresh: outbound stalled")
		}
	}
}
