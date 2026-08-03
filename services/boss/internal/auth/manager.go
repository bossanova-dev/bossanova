package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// migrationHintOnce ensures the "run 'boss login' to reset" hint is printed
// at most once per process, even across multiple Load() callers.
var (
	migrationHintOnce sync.Once
	migrationHintOut  io.Writer = os.Stderr
)

// credentialLockWarnOut receives the non-fatal warning withCredentialLock emits
// when releasing the cross-process credential lock fails. Package-level so
// tests can capture it, mirroring migrationHintOut.
var credentialLockWarnOut io.Writer = os.Stderr

// maybeWarnCredentialsUnreadable prints a one-shot hint when a Load() failure
// indicates the stored credentials can't be decrypted. Typical cause: the
// user upgraded past the keyringutil rollout and their on-disk keyring file
// was encrypted with the old hardcoded passphrase.
func maybeWarnCredentialsUnreadable(err error) {
	if !errors.Is(err, ErrCredentialsUnreadable) {
		return
	}
	migrationHintOnce.Do(func() {
		_, _ = fmt.Fprintln(migrationHintOut, "\nwarning: stored credentials can't be decrypted with the current keyring passphrase — run 'boss logout && boss login' to reset.")
	})
}

// Config holds WorkOS provider configuration.
type Config struct {
	ClientID string // WorkOS application client ID
}

// Manager coordinates token loading, refresh, and persistence.
type Manager struct {
	store      TokenStore
	config     Config
	startLogin func(context.Context) (*DeviceCodeResponse, error)
	pollLogin  func(context.Context, string, int) error
}

// NewManager creates a Manager with the given store and WorkOS config.
func NewManager(store TokenStore, cfg Config) *Manager {
	return &Manager{store: store, config: cfg}
}

// AccessToken returns a valid access token, refreshing if needed.
// Returns empty string (no error) if no tokens are stored — callers
// should treat this as unauthenticated (local mode).
func (m *Manager) AccessToken(ctx context.Context) (string, error) {
	tokens, err := m.store.Load()
	if err != nil {
		maybeWarnCredentialsUnreadable(err)
		// No stored tokens — not logged in.
		return "", nil
	}

	// A retained-but-flagged record must never drive another WorkOS exchange:
	// its refresh token may already have been consumed upstream.
	if reason := tokens.ReloginReasonOrEmpty(); reason != "" {
		return "", ReloginRequiredError(reason)
	}

	if tokens.Valid() {
		return tokens.AccessToken, nil
	}

	if tokens.RefreshToken == "" {
		return "", fmt.Errorf("access token expired and no refresh token available; run 'boss login'")
	}

	refreshed, err := m.refreshStoredTokens(ctx, false)
	if err != nil {
		return "", fmt.Errorf("refresh token: %w (run 'boss login' to re-authenticate)", err)
	}

	return refreshed.AccessToken, nil
}

// Login performs the WorkOS device code flow and stores the resulting tokens.
// A successful login always writes a clean record, clearing any retained
// re-login marker.
//
// The clearReloginMarker call below is DEFENSIVE, not load-bearing: the
// device-code exchange constructs a fresh Tokens, so there is never a marker
// on it today. What actually drops a retained marker is that Save REPLACES
// the whole record — which is the property the tests assert.
func (m *Manager) Login(ctx context.Context) error {
	result, err := Login(ctx, m.config)
	if err != nil {
		return err
	}
	result.Tokens.clearReloginMarker()
	return m.withCredentialLock(ctx, func() error {
		return m.store.Save(result.Tokens)
	})
}

// Refresh refreshes stored credentials using the saved refresh token.
func (m *Manager) Refresh(ctx context.Context) error {
	_, err := m.refreshStoredTokens(ctx, true)
	if err != nil {
		return fmt.Errorf("refresh token: %w", err)
	}
	return nil
}

// StartLogin initiates the device code flow and returns the device code
// response without printing to stdout (safe for TUI use).
func (m *Manager) StartLogin(ctx context.Context) (*DeviceCodeResponse, error) {
	if m.startLogin != nil {
		return m.startLogin(ctx)
	}
	return RequestDeviceCode(ctx, m.config)
}

// PollLogin polls for token completion and saves the resulting tokens.
func (m *Manager) PollLogin(ctx context.Context, deviceCode string, interval int) error {
	if m.pollLogin != nil {
		// The e2e login seam persists from inside its callback. Keep that
		// mutation serialized with refresh marker writes too.
		return m.withCredentialLock(ctx, func() error {
			return m.pollLogin(ctx, deviceCode, interval)
		})
	}
	result, err := PollForToken(ctx, m.config, deviceCode, interval)
	if err != nil {
		return err
	}
	result.Tokens.clearReloginMarker()
	return m.withCredentialLock(ctx, func() error {
		return m.store.Save(result.Tokens)
	})
}

// Logout removes stored tokens.
func (m *Manager) Logout(ctx context.Context) error {
	return m.withCredentialLock(ctx, m.store.Delete)
}

// withCredentialLock serializes every explicit replacement or deletion of the
// shared WorkOS credential record with refresh's durable read-modify-write.
// The lock is intentionally acquired only around keychain mutation: device
// login and polling must not block a daemon refresh while they use the network.
//
// Acquisition is bounded at refreshLockTimeout, the same budget its two
// siblings use (refreshStoredTokens here, and bossd's
// KeychainTokenProvider.refresh). The callers hand in unbounded contexts —
// `boss logout` passes cmd.Context() and the TUI passes an app-lifetime one —
// while bossd can legitimately hold the lock for two 10s exchange attempts plus
// keychain I/O, so without a bound a logout simply hangs. Only ACQUISITION is
// bounded: mutate keeps the caller's context, which matters for PollLogin's
// e2e seam (it polls from inside the lock).
func (m *Manager) withCredentialLock(ctx context.Context, mutate func() error) error {
	lockCtx, cancel := context.WithTimeout(ctx, refreshLockTimeout)
	defer cancel()
	lock, err := acquireRefreshLock(lockCtx)
	if err != nil {
		return fmt.Errorf("acquire credential lock: %w", err)
	}

	// A completed keychain mutation is authoritative: callers use a nil result
	// to decide whether to notify the daemon about new or removed credentials.
	// Unlock failures cannot undo that mutation, so do not turn a successful
	// login or logout into a failed operation after the fact.
	mutationErr := mutate()
	if unlockErr := lock.Unlock(); unlockErr != nil {
		if mutationErr != nil {
			return errors.Join(mutationErr, unlockErr)
		}
		// Non-fatal, but not silent: a failed flock Unlock leaves the
		// descriptor locked for the rest of the process, so in the long-lived
		// TUI every later login, logout, or refresh blocks on a lock nothing
		// will release. Say so here rather than only at the mystery timeout.
		_, _ = fmt.Fprintf(credentialLockWarnOut, "warning: releasing the credential lock failed: %v\n", unlockErr)
	}
	return mutationErr
}

// Status returns the current login status for display.
type Status struct {
	LoggedIn  bool
	ExpiresAt time.Time
	Email     string
	// NeedsRelogin reports credentials that are still stored — the Email
	// above is the retained identity — but can no longer be used, so the user
	// must sign in again. This is distinct from never having logged in.
	NeedsRelogin bool
	// ReloginReason is the enumerated, non-secret reason behind NeedsRelogin
	// (one of the ReloginReason* constants), or "" when it is false.
	ReloginReason string
}

// Status reports whether the user is logged in.
// A user is considered logged in if they have stored tokens — even if the
// access token has expired — as long as a refresh token is available and the
// record is not flagged for re-login. A flagged record is deliberately not
// logged in even though its email is still reported: callers must offer a
// sign-in rather than attempting another refresh.
func (m *Manager) Status() *Status {
	tokens, err := m.store.Load()
	if err != nil {
		maybeWarnCredentialsUnreadable(err)
		return &Status{LoggedIn: false}
	}

	if reason := tokens.ReloginReasonOrEmpty(); reason != "" {
		return &Status{
			LoggedIn:      false,
			ExpiresAt:     tokens.ExpiresAt,
			Email:         tokens.Email,
			NeedsRelogin:  true,
			ReloginReason: reason,
		}
	}

	loggedIn := tokens.Valid() || tokens.RefreshToken != ""

	return &Status{
		LoggedIn:  loggedIn,
		ExpiresAt: tokens.ExpiresAt,
		Email:     tokens.Email,
	}
}
