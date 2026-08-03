// Package upstream — auth_state.go houses the shared "user must re-login"
// signal between the openers (which discover the unusable refresh token
// when WorkOS returns invalid_grant, or when a dispatched exchange never
// confirms its outcome — see BOS-659) and the Run loops (which need to stop
// dialling instead of tight-looping on a credential that will never work
// again).
//
// Both DaemonStream and TerminalStream share a single AuthState so a
// rejection on one bidi pauses the other too — they authenticate with the
// same JWT, so if one is dead the other is.
package upstream

import (
	"context"
	"sync"
)

// AuthState is the tiny synchronisation primitive that flips between
// "auth OK" (default) and "needs login" when an opener observes
// ErrAuthExpired or the user explicitly logs out. Run loops poll
// NeedsLogin and block on Wait until the next MarkOK, which the auth
// notifier fires after `boss login` rewrites the keychain. Active streams
// watch NeedsLoginSignal so a logout cancels them immediately instead of
// lingering until the next reconnect.
type AuthState struct {
	mu         sync.Mutex
	needsLogin bool
	// waitCh is closed on every needsLogin → !needsLogin transition so
	// blocked waiters wake up. A fresh channel is allocated for the next
	// round so a later MarkNeedsLogin can re-arm it. nil-safe receive on
	// the closed channel returns immediately, which is exactly the
	// "already cleared" semantics we want for late callers of Wait.
	waitCh       chan struct{}
	needsLoginCh chan struct{}
}

// NewAuthState returns an AuthState in the "auth OK" state with a fresh
// wait channel ready for the first MarkNeedsLogin.
func NewAuthState() *AuthState {
	return &AuthState{
		waitCh:       make(chan struct{}),
		needsLoginCh: make(chan struct{}),
	}
}

// MarkNeedsLogin transitions to "needs login". Returns true iff this was
// a real state change (so the caller can log once instead of on every
// reconnect attempt). Idempotent — calling on an already-flagged state
// is a no-op that returns false.
func (s *AuthState) MarkNeedsLogin() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.needsLogin {
		return false
	}
	s.needsLogin = true
	close(s.needsLoginCh)
	return true
}

// MarkOK transitions back to "auth OK" and unblocks any goroutine
// currently in Wait. Returns true iff this was a real state change.
// Safe to call when already OK — that's the steady state during normal
// operation, and NotifyLogin fires on every login regardless of prior
// state.
func (s *AuthState) MarkOK() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.needsLogin {
		return false
	}
	s.needsLogin = false
	close(s.waitCh)
	s.waitCh = make(chan struct{})
	s.needsLoginCh = make(chan struct{})
	return true
}

// NeedsLogin reports whether the daemon is paused waiting for re-login.
// Read by the Run loops at the top of each iteration to decide whether
// to dial or to block on Wait.
func (s *AuthState) NeedsLogin() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.needsLogin
}

// Wait returns a channel that closes on the next MarkOK. The channel is
// snapshotted at call time, so a caller that calls Wait, then sees
// NeedsLogin go false before blocking, may briefly select on an
// already-closed channel — that's fine, it just unblocks immediately.
//
// Callers MUST re-check NeedsLogin after wake-up: NotifyLogin fires
// MarkOK whenever the keychain is reloaded, but the new credentials
// might still be expired, in which case the next dial flips the state
// back to NeedsLogin.
func (s *AuthState) Wait() <-chan struct{} {
	if s == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.waitCh
}

// NeedsLoginSignal returns a channel that closes on the next transition to
// needs-login. If the state already needs login, the returned channel is
// already closed.
func (s *AuthState) NeedsLoginSignal() <-chan struct{} {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.needsLoginCh
}

// watchNeedsLogin runs onSignal exactly once if the AuthState transitions
// to needs-login before ctx is cancelled. The returned channel closes when
// the watcher goroutine exits — on ctx cancel, or after onSignal runs — so
// callers can join it during teardown. A nil state (or one whose signal is
// already disarmed) yields an immediately-closed channel and spawns no
// goroutine. onSignal carries whatever teardown the caller needs: a bare
// cancel for the daemon stream, or close-attaches-then-cancel for the
// terminal stream.
func watchNeedsLogin(ctx context.Context, state *AuthState, onSignal func()) <-chan struct{} {
	done := make(chan struct{})
	if state == nil {
		close(done)
		return done
	}
	signal := state.NeedsLoginSignal()
	if signal == nil {
		close(done)
		return done
	}

	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
		case <-signal:
			onSignal()
		}
	}()

	return done
}

func cancelOnNeedsLogin(ctx context.Context, state *AuthState, cancel context.CancelFunc) <-chan struct{} {
	return watchNeedsLogin(ctx, state, func() { cancel() })
}
