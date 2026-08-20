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
	// Carry the refresher's logger into Refresh so the provider's in-window
	// replay warning lands in the daemon log with the same component field as
	// the sign-out line it is meant to be read against (see logRefreshReplay).
	ctx = c.logger.WithContext(ctx)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-c.clock.After(c.refreshInterval):
		}

		expiresAt := c.tokenProvider.ExpiresAt()
		if expiresAt.IsZero() {
			// Two very different daemons arrive at this same `continue`, and
			// before BOS-944 both left the log identical — empty.
			//
			//  1. A dev/static-token daemon that simply has no expiry to
			//     reason about. Nothing is wrong; stay silent forever.
			//  2. A daemon whose provider was reloaded from a re-login-marked
			//     record. applyTokensLocked suppresses the access token in
			//     that case, so ExpiresAt() is zero for the opposite reason:
			//     there are no usable credentials at all, and the refresher
			//     will now skip every tick for the life of the process
			//     without ever saying so.
			//
			// A sentinel rendered for two opposite causes is not a
			// diagnostic, so split them on the enumerated relogin reason and
			// say so once for case 2. Once per transition, not per tick: the
			// refresher wakes every refreshInterval and this condition is
			// permanent until `boss login`.
			if reason := providerReloginReason(c.tokenProvider); reason != "" && c.noteZeroExpiry() {
				c.logger.Warn().
					Str("relogin_reason", reason).
					Msg("upstream token has no expiry and the stored credentials are marked for re-login; refresh is idle until `boss login`")
			}
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
			//
			// This branch is final by the time it is reached, but NOT because
			// replaying an unconfirmed exchange is unsafe. Re-presenting the
			// same refresh token inside WorkOS's 30s replay grace window is the
			// documented recovery — see workOSReplayGraceWindow and
			// https://workos.com/docs/authkit/session-resilience — and refresh()
			// has already spent that budget before it composes ErrAuthExpired.
			// So an ErrRefreshOutcomeUnknown arriving here means
			// maxAmbiguousDispatches dispatches all went unconfirmed; another
			// retry at this level could only replay outside the window, where
			// WorkOS answers authoritatively anyway. The retry that still belongs
			// at this level is the transient one below (BOS-941, BOS-659).
			if errors.Is(err, ErrAuthExpired) {
				if c.authState != nil && c.authState.MarkNeedsLogin() {
					logReloginPause(&c.logger, "", err)
				}
				return fmt.Errorf("token refresh: %w", err)
			}

			// Everything else leaves the stored refresh token usable: either
			// nothing was dispatched, or WorkOS answered without exchanging
			// it (a 5xx, say). Retrying is safe — this is NOT the ambiguous
			// case, which the terminal branch above already claimed.
			//
			// Tearing the stream down here (the previous behaviour) forced
			// the retry to travel through a full reconnect, and the refresh
			// threshold used to leave ~1s of validity, so in practice there
			// was room for one attempt. Staying on the stream and retrying on
			// the next tick turns the threshold into a real budget: several
			// attempts before the token expires, with no reconnect churn.
			// Once the token really is expired there is nothing left to
			// protect — bosso will reject it — so fall through and close.
			//
			// One exception: Refresh can hand back a rotated access token
			// ALONGSIDE an error when the exchange succeeded but persisting
			// it failed. Retrying would strand that token — the emit below is
			// what tells bosso about it — so keep the old close-and-reconnect
			// behaviour, which re-registers with the new token from cache.
			if remaining := expiresAt.Sub(c.clock.Now()); remaining > 0 && newTok == "" {
				event := c.logger.Warn().Err(err).Dur("remaining", remaining)
				if failure, ok := refreshFailureOf(err); ok {
					event = event.Str("refresh_failure_class", string(failure.class)).
						Bool("conn_reused", failure.connReused)
				}
				event.Msg("token refresh failed; retrying before expiry")
				continue
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
