package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/recurser/bossalib/safego"
	zlog "github.com/rs/zerolog/log"
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
	// lastE2ETokens is what the e2e login seam last persisted from inside its
	// own callback. The seam saves without handing the Manager the record, so
	// without this the seam branch could not run the equality leg of
	// verification and a silently no-oped save over an unflagged record would
	// render as a successful login — precisely the class this verification
	// exists to catch. It is always nil in production builds, where
	// SetE2ELogin is a no-op.
	lastE2ETokens *Tokens
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

// Login performs the WorkOS device code flow and stores the resulting tokens,
// then proves the write against the store before returning. A successful login
// always writes a clean record, clearing any retained re-login marker.
//
// The returned LoginVerification is only meaningful when err is nil; on an
// error it is the zero value, whose Outcome is the invalid "never ran"
// sentinel. A non-verified verdict with a nil error is not a failed command —
// it is a command that finished and found the credentials are not what the
// caller was about to announce.
//
// The clearReloginMarker call below is DEFENSIVE, not load-bearing: the
// device-code exchange constructs a fresh Tokens, so there is never a marker
// on it today. What actually drops a retained marker is that Save REPLACES
// the whole record — which is the property the tests assert.
func (m *Manager) Login(ctx context.Context) (LoginVerification, error) {
	result, err := Login(ctx, m.config)
	if err != nil {
		return LoginVerification{}, err
	}
	result.Tokens.clearReloginMarker()
	return m.commitLogin(ctx, result.Tokens, func() error {
		return m.store.Save(result.Tokens)
	})
}

// commitLogin performs a login's keychain write and its read-back proof under a
// SINGLE acquisition of the cross-process credential lock. Verifying inside the
// same critical section is the point: releasing and re-acquiring would let a
// concurrent daemon refresh land between the write and the read, and the check
// would then be reporting on somebody else's record.
//
// The re-read runs only when save() reported success — a failed save has
// nothing to prove and its error is returned unchanged.
//
// expect is the record the caller believes it just wrote, used for the equality
// leg. Callers that persist through a seam rather than through the Manager pass
// nil and let the seam's recorded tokens stand in.
func (m *Manager) commitLogin(ctx context.Context, expect *Tokens, save func() error) (LoginVerification, error) {
	var verdict LoginVerification
	err := m.withCredentialLock(ctx, func() error {
		// Never let a previous run's seam record satisfy this run's equality
		// leg.
		m.lastE2ETokens = nil
		if saveErr := save(); saveErr != nil {
			return saveErr
		}
		want := expect
		if want == nil {
			want = m.lastE2ETokens
		}
		logCredentialSave(want)
		verdict = m.verifyPersistedLocked(ctx, want)
		logCredentialVerdict(verdict)
		return nil
	})
	if err != nil {
		return LoginVerification{}, err
	}
	return verdict, nil
}

// logCredentialSave records that a login write reported success, and what it
// claimed to have written. Pair it with logCredentialVerdict: together they say
// whether the boss side believed it saved credentials and whether the store
// agreed, which is the first fork the next occurrence of this incident has to
// take.
//
// The record's email is deliberately on this line — it is what correlates the
// boss line with bossd's, and it is already the retained identity the re-login
// UI displays. Nothing else about the tokens may join it: no access token, no
// refresh token, and no substring of either.
func logCredentialSave(saved *Tokens) {
	event := zlog.Info().Str("component", "auth-store")
	if saved == nil {
		event.Msg("credential save reported success with no record to describe")
		return
	}
	event.
		Str("email", saved.Email).
		Time("expires_at", saved.ExpiresAt).
		Bool("needs_relogin", saved.NeedsRelogin).
		Str("relogin_reason", saved.ReloginReasonOrEmpty()).
		Msg("credential save reported success")
}

// logCredentialVerdict records what the store actually held afterwards. Only
// the enumerated outcome and reason are logged; LoginVerification.Err is
// deliberately absent, because a keyring error can embed record bytes.
func logCredentialVerdict(verdict LoginVerification) {
	zlog.Info().
		Str("component", "auth-store").
		Str("email", verdict.Email).
		Str("outcome", verdict.Outcome.String()).
		Str("reason", verdict.Reason).
		Msg("credential save verified")
}

// verifyPersistedLocked reads the credential record back and judges whether it
// reflects the login that just happened. It MUST be called with the credential
// lock held.
//
// It checks persistence and usability, never freshness: an access token that is
// already expired is still a correctly persisted login, because the daemon can
// refresh it. Deliberately absent is any call to Tokens.Valid(), which folds
// expiry into its answer and would report a perfectly good save as a failure.
//
// The read carries its own bound — the same refreshLockTimeout budget the lock
// acquisition uses — so a keychain that hangs cannot pin the credential lock
// against every other process. Blowing the bound is inconclusive, not a
// failure: the record may be fine, we simply could not look.
func (m *Manager) verifyPersistedLocked(ctx context.Context, expect *Tokens) LoginVerification {
	readCtx, cancel := context.WithTimeout(ctx, refreshLockTimeout)
	defer cancel()

	type loadResult struct {
		tokens *Tokens
		err    error
	}
	// store.Load() cannot be cancelled, so run it alongside the bound and
	// abandon it on timeout. The channel is buffered: a late send must not
	// block the goroutine forever.
	done := make(chan loadResult, 1)
	safego.Go(zlog.Logger, func() {
		tokens, err := m.store.Load()
		done <- loadResult{tokens: tokens, err: err}
	})

	var res loadResult
	select {
	case res = <-done:
	case <-readCtx.Done():
		return LoginVerification{
			Outcome: LoginVerifyInconclusive,
			Reason:  LoginVerifyReasonLockTimeout,
			Err:     readCtx.Err(),
		}
	}

	if res.err != nil {
		// A record that is simply absent is a verdict, not a mystery: the save
		// reported success and left nothing behind. Anything else — an
		// undecryptable record, a backend that errored — leaves the question
		// open.
		if tokenKeyMissing(res.err) {
			return LoginVerification{
				Outcome: LoginVerifyRecordNotUpdated,
				Reason:  LoginVerifyReasonRecordAbsent,
			}
		}
		return LoginVerification{
			Outcome: LoginVerifyInconclusive,
			Reason:  LoginVerifyReasonReadFailed,
			Err:     res.err,
		}
	}

	tokens := res.tokens
	if tokens == nil {
		return LoginVerification{
			Outcome: LoginVerifyRecordNotUpdated,
			Reason:  LoginVerifyReasonRecordAbsent,
		}
	}
	if reason := tokens.ReloginReasonOrEmpty(); reason != "" {
		return LoginVerification{
			Outcome: LoginVerifyRecordNotUpdated,
			Reason:  reason,
			Email:   tokens.Email,
		}
	}
	if tokens.AccessToken == "" {
		return LoginVerification{
			Outcome: LoginVerifyRecordNotUpdated,
			Reason:  LoginVerifyReasonNoAccessToken,
			Email:   tokens.Email,
		}
	}
	if tokens.RefreshToken == "" {
		return LoginVerification{
			Outcome: LoginVerifyRecordNotUpdated,
			Reason:  LoginVerifyReasonNoRefreshToken,
			Email:   tokens.Email,
		}
	}
	if expect != nil && tokens.AccessToken != expect.AccessToken {
		return LoginVerification{
			Outcome: LoginVerifyRecordNotUpdated,
			Reason:  LoginVerifyReasonAccessTokenMismatch,
			Email:   tokens.Email,
		}
	}
	return LoginVerification{Outcome: LoginVerified, Email: tokens.Email}
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

// PollLogin polls for token completion, saves the resulting tokens, and proves
// the write against the store before returning.
//
// As with Login, the returned LoginVerification is only meaningful when err is
// nil; on an error it is the zero value and must not be read.
func (m *Manager) PollLogin(ctx context.Context, deviceCode string, interval int) (LoginVerification, error) {
	if m.pollLogin != nil {
		// The e2e login seam persists from inside its callback. Keep that
		// mutation serialized with refresh marker writes too — and verify it
		// on exactly the same terms as the production branch, using the record
		// the seam recorded on the Manager as the expectation.
		return m.commitLogin(ctx, nil, func() error {
			return m.pollLogin(ctx, deviceCode, interval)
		})
	}
	result, err := PollForToken(ctx, m.config, deviceCode, interval)
	if err != nil {
		return LoginVerification{}, err
	}
	result.Tokens.clearReloginMarker()
	return m.commitLogin(ctx, result.Tokens, func() error {
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
