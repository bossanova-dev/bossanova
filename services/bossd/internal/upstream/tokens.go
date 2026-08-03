// Package upstream — tokens.go owns the keychain-backed WorkOS token
// loader used by both the legacy heartbeat Manager (behind the
// legacy_upstream build tag) and the new StreamClient. T3.7 lifted this
// out from upstream.go so the default build can compile without the
// legacy RPCs present. Phase 8 deletes the legacy copy; this file
// survives as the single source of truth.
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/99designs/keyring"
	"github.com/rs/zerolog"
	"golang.org/x/sync/singleflight"

	"github.com/recurser/bossalib/authlock"
	"github.com/recurser/bossalib/authtoken"
	"github.com/recurser/bossalib/keyringutil"
)

// ErrAuthExpired is the umbrella sentinel for every state that can only be
// resolved by a new `boss login`. It is returned (wrapped) by Refresh when
// the stored refresh token can no longer be used — either because WorkOS
// authoritatively rejected it (ErrRefreshTokenRejected) or because a refresh
// exchange was dispatched and its outcome could never be confirmed
// (ErrRefreshOutcomeUnknown), which means WorkOS may already have consumed
// and rotated the token. Callers detect this with errors.Is and treat it as
// "stop retrying, wait for the user to log in again" rather than continuing
// the normal reconnect/backoff loop. It is deliberately broad: use the
// specific sentinels below when the distinction matters (diagnostics, the
// persisted re-login reason), and ErrAuthExpired when it does not.
var ErrAuthExpired = errors.New("auth expired: re-login required")

// ErrRefreshTokenRejected is an authoritative terminal rejection: WorkOS
// answered the exchange and said the refresh token is not usable. Credentials
// are retained on disk (flagged for re-login) rather than deleted.
var ErrRefreshTokenRejected = fmt.Errorf("refresh token rejected by WorkOS: %w", ErrAuthExpired)

// ErrRefreshTokenAlreadyExchanged is the documented WorkOS
// "Refresh token already exchanged." variant of ErrRefreshTokenRejected. It
// is kept separate for diagnostics and for the pending-rotation case where an
// earlier ambiguous attempt is confirmed to have consumed the token.
var ErrRefreshTokenAlreadyExchanged = fmt.Errorf("refresh token already exchanged: %w", ErrRefreshTokenRejected)

// ErrRefreshOutcomeUnknown means the refresh request was dispatched but its
// result never came back (transport failure, or a response body that could not
// be read). WorkOS may or may not have consumed and rotated the one-shot
// refresh token, so replaying it is unsafe — the provider fails closed,
// persists the reason, and waits for a new login instead. See BOS-659.
var ErrRefreshOutcomeUnknown = fmt.Errorf("refresh outcome could not be confirmed: %w", ErrAuthExpired)

// Enumerated, non-secret re-login reasons persisted on the shared
// "workos-tokens" keychain record. These strings are part of the on-disk
// contract with services/boss/internal/auth (auth.ReloginReason*); keep the
// two lists in step.
const (
	reloginReasonRefreshOutcomeUnknown = "refresh_outcome_unknown"
	reloginReasonRefreshTokenRejected  = "refresh_token_rejected"
)

// keychainTokens mirrors the boss CLI token structure for keychain reading.
// NeedsRelogin/ReloginReason are the BOS-659 re-login marker: both are
// omitempty so a record written here still round-trips through an older boss
// CLI, and a record written by the CLI with no marker keeps behaving exactly
// as it always has.
type keychainTokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	Email        string    `json:"email,omitempty"`
	// NeedsRelogin marks credentials that are retained for their identity
	// (email) but must not be used for another WorkOS exchange.
	NeedsRelogin bool `json:"needs_relogin,omitempty"`
	// ReloginReason is one of the enumerated reloginReason* values. It never
	// carries token material or upstream response bodies.
	ReloginReason string `json:"relogin_reason,omitempty"`
}

func (t *keychainTokens) valid() bool {
	return t != nil && !t.NeedsRelogin && t.AccessToken != "" && time.Now().Before(t.ExpiresAt)
}

// reloginReason normalizes the persisted marker: an entry flagged without a
// recognized reason is treated as an authoritative rejection, which is the
// conservative reading (it never invites a retry).
func (t *keychainTokens) reloginReason() string {
	if t == nil || !t.NeedsRelogin {
		return ""
	}
	if t.ReloginReason == reloginReasonRefreshOutcomeUnknown {
		return reloginReasonRefreshOutcomeUnknown
	}
	return reloginReasonRefreshTokenRejected
}

// reloginError maps a persisted reason back to the sentinel callers match on.
func reloginError(reason string) error {
	if reason == reloginReasonRefreshOutcomeUnknown {
		return ErrRefreshOutcomeUnknown
	}
	return ErrRefreshTokenRejected
}

// reloginReasonForError is reloginError's inverse: it maps a refresh failure
// onto the enumerated reason persisted next to the retained record, or "" when
// the failure is not one of the two terminal re-login states. A bare
// ErrAuthExpired deliberately maps to "" — it says "re-login required" without
// saying which of the two happened, and guessing would be exactly the
// mislabelling BOS-659 is about.
func reloginReasonForError(err error) string {
	switch {
	case errors.Is(err, ErrRefreshOutcomeUnknown):
		return reloginReasonRefreshOutcomeUnknown
	case errors.Is(err, ErrRefreshTokenRejected):
		return reloginReasonRefreshTokenRejected
	default:
		return ""
	}
}

// reloginPauseMessage renders the one sanitized warning the daemon and
// terminal openers (and the periodic refresher) emit when they pause for
// re-login. It carries no token material, no upstream response body, and no
// transport detail — an ambiguous timeout must never be reported as
// invalid_grant, which is what the pre-BOS-659 wording did for every failure.
// Every variant states that the stored credentials were RETAINED, because the
// daemon no longer deletes them.
func reloginPauseMessage(reason string) string {
	const retained = "credentials retained; run 'boss login' to sign in again"
	switch reason {
	case reloginReasonRefreshOutcomeUnknown:
		return "upstream token refresh outcome could not be confirmed; " + retained
	case reloginReasonRefreshTokenRejected:
		return "upstream refresh token was rejected; " + retained
	default:
		return "upstream authentication needs a new sign-in; " + retained
	}
}

// errReloginMarkerNotPersisted means the re-login marker could NOT be written
// to the shared keychain record. It is worth surfacing on its own: the
// in-memory cache is disabled either way, but a daemon restart reloads an
// unflagged record and will replay the very refresh token this run decided was
// unsafe. Callers match it with errors.Is to log the distinction.
var errReloginMarkerNotPersisted = errors.New("persist re-login marker")

// logReloginPause emits the single sanitized warning the daemon and terminal
// openers and the periodic refresher share when they pause for re-login.
// Only enumerated, non-secret fields are attached: never the wrapped error,
// which carries transport detail, and never an upstream response body. The
// reason field is omitted rather than logged empty when the failure is a bare
// ErrAuthExpired that does not say which of the two states occurred.
func logReloginPause(logger *zerolog.Logger, prefix string, err error) {
	if errors.Is(err, errCredentialsRemoved) {
		// Every reloginPauseMessage variant states that the credentials were
		// RETAINED, which is true of a flagged record and false of a deleted
		// one. Say what actually happened instead of routing through it.
		logger.Warn().Msg(prefix + "upstream credentials were removed; run 'boss login' to sign in again")
		return
	}
	reason := reloginReasonForError(err)
	event := logger.Warn()
	if reason != "" {
		event = event.Str("relogin_reason", reason)
	}
	if errors.Is(err, errReloginMarkerNotPersisted) {
		// The pause is correct, but it is not durable: a restart reloads an
		// unflagged record and would replay the token.
		event = event.Bool("relogin_marker_persisted", false)
	}
	event.Msg(prefix + reloginPauseMessage(reason))
}

// reloginReporter is an optional capability on TokenProvider implementations:
// report the persisted re-login reason observed by the last durable-store read.
// KeychainTokenProvider satisfies it; static/test providers need not.
type reloginReporter interface {
	ReloginReason() string
}

// providerReloginReason reports the provider's persisted re-login marker, or
// "" when the provider is nil or cannot carry one.
func providerReloginReason(tp TokenProvider) string {
	reporter, ok := tp.(reloginReporter)
	if !ok || reporter == nil {
		return ""
	}
	return reporter.ReloginReason()
}

// noCredentialsPauseMessage explains a dial with no bearer token to send.
// A provider reloaded at daemon startup from a marked record is exactly this
// case — applyTokensLocked suppresses its access token — so the retained
// re-login wording must win over the generic "no credentials" line, which
// would otherwise read as "you never logged in".
func noCredentialsPauseMessage(tp TokenProvider) string {
	if reason := providerReloginReason(tp); reason != "" {
		return reloginPauseMessage(reason)
	}
	return "no upstream credentials available; pausing stream until login"
}

// defaultWorkOSClientID is the production WorkOS client used when
// BOSS_WORKOS_CLIENT_ID is unset. Mirrors services/boss/cmd/auth.go so
// `boss login` and the bossd refresh path agree on the same client out of
// the box. Override for staging / self-host.
const defaultWorkOSClientID = "client_01KP805YXXAMZSN2YB4NGXS9XB"

// openKeyring opens the shared bossanova keyring. bossd runs as a daemon
// with no flag plumbing, so allowInsecure is hard-wired to false here — a
// broken environment should surface a real error rather than silently
// reverting to the hardcoded passphrase.
var (
	workOSRefreshHTTPClient = &http.Client{Timeout: 10 * time.Second}
	workOSAuthenticateURL   = "https://api.workos.com/user_management/authenticate"
	openKeyring             = func() (keyring.Keyring, error) {
		return keyring.Open(keyring.Config{
			ServiceName:              "bossanova",
			KeychainTrustApplication: true,
			FileDir:                  "~/.config/bossanova/keyring",
			FilePasswordFunc:         keyring.PromptFunc(keyringutil.New(false)),
			// Optional override via BOSS_KEYRING_BACKEND. Stays in lock-step
			// with the boss CLI so a developer who exports the env var sees
			// the same backend on both processes.
			AllowedBackends: keyringutil.Backends(),
		})
	}
	refreshWorkOSTokenFn = refreshWorkOSToken
	acquireRefreshLock   = func(ctx context.Context) (refreshUnlocker, error) {
		return authlock.AcquireWorkOSRefreshLock(ctx)
	}
	loadKeychainTokensFn = loadKeychainTokens
	saveKeychainTokensFn = saveKeychainTokens
)

type refreshUnlocker interface {
	Unlock() error
}

func loadKeychainTokens() (*keychainTokens, error) {
	ring, err := openKeyring()
	if err != nil {
		return nil, err
	}
	item, err := ring.Get("workos-tokens")
	if err != nil {
		return nil, err
	}
	var tokens keychainTokens
	if err := json.Unmarshal(item.Data, &tokens); err != nil {
		return nil, err
	}
	return &tokens, nil
}

func saveKeychainTokens(tokens *keychainTokens) error {
	ring, err := openKeyring()
	if err != nil {
		return err
	}
	data, err := json.Marshal(tokens)
	if err != nil {
		return err
	}
	return ring.Set(keyring.Item{
		Key:         "workos-tokens",
		Data:        data,
		Label:       "Bossanova",
		Description: "WorkOS authentication tokens",
	})
}

// NOTE (BOS-659): there is deliberately no removeKeychainTokens here. No
// automatic daemon path may delete the shared "workos-tokens" item — an
// ambiguous or rejected refresh flags the record instead, so the user's
// identity survives and only an explicit `boss logout` removes it.

// keychainRecordDeleted discriminates the two ways a keychain read fails, which
// need OPPOSITE responses:
//
//   - keyring.ErrKeyNotFound means the shared "workos-tokens" item is gone.
//     Since BOS-659 nothing in the daemon deletes it, so the only thing that
//     can have is an explicit `boss logout` — an authoritative answer. Drop the
//     cached credentials and stop; recreating the item would sign the user back
//     in behind their back, and `boss logout`'s NotifyAuthChange to the daemon
//     is best-effort (services/boss/cmd/auth.go), so the daemon cannot rely on
//     being told.
//   - Any other error means the record could not be READ right now (keychain
//     locked, backend unavailable, a transient outage). It may be perfectly
//     intact, so the cached credentials must be preserved — surviving exactly
//     that outage is what this branch exists for.
//
// Conflating the two is what let a successful refresh resurrect a deliberately
// deleted record.
func keychainRecordDeleted(err error) bool {
	return errors.Is(err, keyring.ErrKeyNotFound)
}

// errCredentialsRemoved reports that the shared "workos-tokens" record is no
// longer in the keychain. It composes with ErrAuthExpired so the stream openers
// and the periodic refresher take the EXISTING pause-until-login path rather
// than reconnecting in a loop: a logged-out daemon has nothing left to retry
// with, and the `boss login` that follows clears the AuthState mark through
// NotifyLogin exactly as it does for the two terminal refresh states. It is
// deliberately not one of the enumerated relogin reasons — those are persisted
// onto a RETAINED record, and this record is gone.
var errCredentialsRemoved = fmt.Errorf("stored credentials were removed: %w", ErrAuthExpired)

// refreshWorkOSToken exchanges a refresh token for a new access token.
func refreshWorkOSToken(ctx context.Context, clientID, refreshToken string) (*keychainTokens, error) {
	data := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {refreshToken},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, workOSAuthenticateURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	// gotConn records whether this exchange ever reached a live connection to
	// WorkOS. A Do error is only AMBIGUOUS once it did: before that — DNS
	// failure, connection refused, TLS handshake failure, a context cancelled
	// while dialling — WorkOS provably never saw the exchange, the one-shot
	// refresh token provably was not consumed, and the correct behaviour is
	// the pre-BOS-659 one: a plain retryable error the next tick retries.
	// Treating an offline laptop as an ambiguous exchange would log the user
	// out for exactly the outage this ticket exists to survive.
	//
	// Why GotConn rather than the more precise-sounding WroteRequest:
	//
	//   - WroteRequest fires on the transport's writeLoop GOROUTINE, and
	//     persistConn.roundTrip can return through its response or
	//     connection-closed cases without ever selecting on the write result.
	//     A connection dying mid-write can therefore let Do return before the
	//     callback runs, leaving the flag false for a request whose bytes DID
	//     go out — the unsafe direction, which replays a token WorkOS may have
	//     consumed.
	//   - GotConn is delivered synchronously on the RoundTrip goroutine before
	//     the request is handed to the connection — from Transport.getConn for
	//     HTTP/1, and from the h2 transport itself for HTTP/2 (which
	//     api.workos.com negotiates via ALPN). Its answer is therefore always
	//     observed, and it errs the safe way: "we had a connection and then
	//     something failed" is treated as ambiguous even in the cases where
	//     nothing was actually written.
	//
	// It fires for a REUSED pooled connection as well as a fresh dial, which
	// is what makes the reset below necessary rather than merely tidy.
	//
	// GetConn fires once per outer connection ATTEMPT, so resetting there
	// scopes the flag to the attempt that produced the error Do returns. That
	// matters for the VPN-drop/sleep case: net/http hands out a pooled
	// connection (GotConn fires), the write fails with nothing written, and it
	// retries on a fresh dial that is refused — no byte ever left the host, and
	// without the reset the retry would inherit the pooled attempt's flag.
	//
	// The reset can never discard a legitimate "we did write": this POST
	// carries no Idempotency-Key, so Request.isReplayable is false, leaving
	// nothing-written-on-a-reused-conn and no-cached-h2-conn as the only retry
	// classes net/http can take — both of which guarantee nothing was sent.
	var gotConn atomic.Bool
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GetConn: func(string) { gotConn.Store(false) },
		GotConn: func(httptrace.GotConnInfo) { gotConn.Store(true) },
	}))

	// From the moment a connection is in hand, a failure leaves the refresh
	// token's fate unknown: WorkOS may already have consumed and rotated it.
	// Fail closed rather than replaying it.
	resp, err := workOSRefreshHTTPClient.Do(req)
	if err != nil {
		if !gotConn.Load() {
			return nil, fmt.Errorf("refresh request never dispatched: %w", err)
		}
		return nil, fmt.Errorf("%w: %w", ErrRefreshOutcomeUnknown, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, readErr := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		// A response arrived, so the outcome is authoritative — never
		// ambiguous. WorkOS returns HTTP 400 with {"error":"invalid_grant"}
		// when the refresh token has been revoked, already exchanged, or its
		// session ended. That's terminal: wrap with ErrRefreshTokenRejected
		// (which composes with ErrAuthExpired) so the stream client can pause
		// instead of tight-looping on a credential that will never work again.
		//
		// Response bodies are never interpolated into the returned error:
		// these strings reach logs and the user, and the body can echo token
		// material back at us.
		if resp.StatusCode == http.StatusBadRequest && readErr == nil {
			var errBody struct {
				Error            string `json:"error"`
				ErrorDescription string `json:"error_description"`
			}
			if json.Unmarshal(body, &errBody) == nil && errBody.Error == "invalid_grant" {
				if strings.Contains(strings.ToLower(errBody.ErrorDescription), "already exchanged") {
					return nil, fmt.Errorf("refresh failed (HTTP %d): %w", resp.StatusCode, ErrRefreshTokenAlreadyExchanged)
				}
				return nil, fmt.Errorf("refresh failed (HTTP %d): %w", resp.StatusCode, ErrRefreshTokenRejected)
			}
		}
		return nil, fmt.Errorf("refresh failed (HTTP %d)", resp.StatusCode)
	}
	if readErr != nil {
		return nil, fmt.Errorf("%w: %w", ErrRefreshOutcomeUnknown, readErr)
	}

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		User         struct {
			Email string `json:"email"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		// HTTP 200 means WorkOS answered successfully and therefore rotated
		// the one-shot refresh token — and we just failed to read the
		// replacement out of the response. That is strictly less recoverable
		// than the read failure above, so it is ambiguous too: retrying the
		// old token here is exactly the replay this ticket forbids. The
		// unmarshal error is not wrapped, because it embeds the response
		// bytes it choked on and those can echo token material.
		return nil, fmt.Errorf("%w: refresh response could not be decoded", ErrRefreshOutcomeUnknown)
	}
	if result.AccessToken == "" {
		// Same hazard, well-formed JSON: a 200 rotated the token, but the
		// response carries no usable replacement. Without this the caller
		// would happily save a token-less record over the good one (and
		// AccessTokenExpiry would stamp it with a default TTL), destroying
		// the credentials this ticket exists to retain.
		return nil, fmt.Errorf("%w: refresh response carried no access token", ErrRefreshOutcomeUnknown)
	}

	return &keychainTokens{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		ExpiresAt:    authtoken.AccessTokenExpiry(result.AccessToken, result.ExpiresIn, time.Now()),
		Email:        result.User.Email,
	}, nil
}

// KeychainTokenProvider is a TokenProvider backed by the "boss login"
// keychain entry. It caches the last-known access token in memory so the
// StreamClient's refresher can observe Token()/ExpiresAt() without a
// keychain read on every tick, then falls back to the keychain on Refresh.
type KeychainTokenProvider struct {
	mu           sync.RWMutex
	accessToken  string
	refreshToken string
	expiresAt    time.Time
	// reloginReason mirrors the persisted re-login marker. Non-empty means the
	// cached credentials are disabled: no bearer token is exposed and no WorkOS
	// exchange may be attempted until a fresh login clears the record.
	reloginReason string

	// clientIDEnv is the env var that holds the WorkOS client ID. Split
	// out so tests can point at a fake without touching the real env.
	clientIDEnv string

	// refreshGroup coalesces concurrent Refresh calls into a single WorkOS
	// exchange. The StreamClient opens two streams (DaemonStream and
	// TerminalStream) that each carry their own refresher; when an access
	// token expires both can call Refresh at the same instant. WorkOS
	// rotates the refresh token on every exchange, so two un-coalesced
	// exchanges race: the first consumes the stored token and rotates it,
	// then a gateway 502 can lose that rotated token in flight, and the
	// sibling replays the now-consumed token → invalid_grant → terminal
	// pause-until-relogin. Single-flighting guarantees exactly one exchange
	// per rotation, so the siblings share one result instead of double-
	// spending the token. See BOS-44 Strand B.
	refreshGroup singleflight.Group
}

// refreshSingleflightKey is the constant single-flight key for Refresh. All
// concurrent refreshes share one in-flight exchange, so a fixed key is
// correct — the provider already scopes to a single keychain entry.
const refreshSingleflightKey = "workos-refresh"

// NewKeychainTokenProvider constructs a provider and populates it from
// the keychain at construction time. A missing keychain entry is not an
// error — Token() simply returns "". Callers can still run the stream
// without auth (local-only mode) or let bosso reject the handshake.
func NewKeychainTokenProvider() *KeychainTokenProvider {
	p := &KeychainTokenProvider{clientIDEnv: "BOSS_WORKOS_CLIENT_ID"}
	p.Reload()
	return p
}

// Reload re-reads the keychain entry into the in-memory cache. Used by
// the auth-change notifier so a fresh `boss login` (which writes new
// tokens to the keychain) is observable to the running daemon without a
// restart — calling Refresh here would fail because the cached refresh
// token has been superseded by the new login.
func (p *KeychainTokenProvider) Reload() {
	p.loadFromKeychain()
}

// loadFromKeychain snapshots the keychain entry into the in-memory cache.
// Safe to call repeatedly. A DELETED entry clears the cache, so an explicit
// `boss logout` de-authorises a daemon that is already running; any other read
// failure leaves the cache alone, because the record may still be there.
func (p *KeychainTokenProvider) loadFromKeychain() {
	tokens, err := loadKeychainTokensFn()
	if err != nil {
		if keychainRecordDeleted(err) {
			p.clearCache()
		}
		return
	}
	p.mu.Lock()
	p.applyTokensLocked(tokens)
	p.mu.Unlock()
}

// Token implements TokenProvider.Token.
func (p *KeychainTokenProvider) Token() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.accessToken
}

// ExpiresAt implements TokenProvider.ExpiresAt.
func (p *KeychainTokenProvider) ExpiresAt() time.Time {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.expiresAt
}

// Refresh implements TokenProvider.Refresh. Concurrent calls are coalesced via
// refreshGroup so the two stream openers can never drive two WorkOS exchanges
// for the same token rotation (see refreshGroup's doc). The single in-flight
// call performs the exchange; coalesced callers receive its result. The key is
// forgotten when the call returns, so a later Refresh starts a fresh exchange.
func (p *KeychainTokenProvider) Refresh(ctx context.Context) (string, error) {
	v, err, _ := p.refreshGroup.Do(refreshSingleflightKey, func() (any, error) {
		return p.refresh(ctx)
	})
	// refresh may return a non-empty token alongside an error (the
	// save-succeeded-locally-but-keychain-write-failed path), so propagate
	// both rather than dropping the token on any error.
	tok, _ := v.(string)
	return tok, err
}

// refresh runs the WorkOS refresh flow under the shared cross-process refresh
// lock and persists the new tokens back to keychain. Always invoked through
// Refresh's single-flight wrapper.
func (p *KeychainTokenProvider) refresh(ctx context.Context) (tok string, retErr error) {
	p.mu.RLock()
	originalAccess := p.accessToken
	refreshTok := p.refreshToken
	pendingReason := p.reloginReason
	p.mu.RUnlock()
	originalRefreshTok := refreshTok

	// A durable re-login marker is final until the user logs in again: return
	// without dispatching anything, so a possibly-consumed refresh token is
	// never replayed. `boss login` clears the marker and Reload() picks that up.
	if pendingReason != "" {
		return "", reloginError(pendingReason)
	}

	clientID := os.Getenv(p.clientIDEnv)
	if clientID == "" {
		clientID = defaultWorkOSClientID
	}
	if refreshTok == "" || clientID == "" {
		return "", fmt.Errorf("refresh not configured (empty refresh token or %s)", p.clientIDEnv)
	}

	lockCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lock, err := acquireRefreshLock(lockCtx)
	if err != nil {
		return "", fmt.Errorf("acquire refresh lock: %w", err)
	}
	defer func() {
		if unlockErr := lock.Unlock(); unlockErr != nil {
			retErr = errors.Join(retErr, unlockErr)
		}
	}()

	latest, err := loadKeychainTokensFn()
	if keychainRecordDeleted(err) {
		// `boss logout` deleted the record while this refresh waited for the
		// cross-process lock, and its notification to the daemon never landed.
		// Logout performs no WorkOS revoke, so the cached refresh token would
		// still exchange cleanly — dispatching it and saving the result is
		// precisely how a logged-out user gets silently signed back in.
		p.clearCache()
		return "", errCredentialsRemoved
	}
	if err != nil {
		// The record may have been marked for re-login by another daemon while
		// this process waited for the lock. Do not exchange an unverifiable
		// cached refresh token; preserve it only for a later retry.
		return "", fmt.Errorf("load tokens before refresh: %w", err)
	}
	p.mu.Lock()
	p.applyTokensLocked(latest)
	p.mu.Unlock()
	// Another process may have flagged the shared record while we waited
	// for the cross-process lock. Same rule: no exchange from a marked
	// record.
	if reason := latest.reloginReason(); reason != "" {
		return "", reloginError(reason)
	}
	if latest.valid() && (latest.RefreshToken != originalRefreshTok || latest.AccessToken != originalAccess) {
		return latest.AccessToken, nil
	}
	if latest.RefreshToken != "" {
		refreshTok = latest.RefreshToken
	}

	for attempts := 0; attempts < 2; attempts++ {
		requestCtx, requestCancel := context.WithTimeout(ctx, 10*time.Second)
		refreshed, err := refreshWorkOSTokenFn(requestCtx, clientID, refreshTok)
		requestCancel()
		if err != nil {
			// Ambiguous outcome: the exchange was dispatched but never
			// confirmed. Keep the credentials exactly as stored, record why
			// re-login is needed, and do not retry — a second attempt could
			// replay a token WorkOS already consumed. Checked before the
			// terminal branch because both compose with ErrAuthExpired.
			if errors.Is(err, ErrRefreshOutcomeUnknown) {
				return p.flagRetainedRecord(refreshTok, reloginReasonRefreshOutcomeUnknown, err)
			}
			if errors.Is(err, ErrAuthExpired) {
				reloaded, loadErr := loadKeychainTokensFn()
				if loadErr == nil && reloaded.RefreshToken != "" && reloaded.RefreshToken != refreshTok {
					p.mu.Lock()
					p.applyTokensLocked(reloaded)
					p.mu.Unlock()
					if reloaded.valid() {
						return reloaded.AccessToken, nil
					}
					if reason := reloaded.reloginReason(); reason != "" {
						return "", reloginError(reason)
					}
					// Advance `latest` alongside `refreshTok` (as the
					// success-path race below does): it is the record a
					// second attempt would write back, and leaving it on the
					// pre-recovery snapshot would persist the older token
					// over the newer one this branch just adopted.
					latest = reloaded
					refreshTok = reloaded.RefreshToken
					continue
				}
				// Authoritative rejection with nothing newer to adopt: retain
				// the entry (email included) behind the re-login marker.
				preserved := latest
				if loadErr == nil {
					preserved = reloaded
				}
				reason := preservedReloginReason(preserved, refreshTok, err)
				return p.flagRetainedRecord(refreshTok, reason, err)
			}
			return "", err
		}
		if refreshed.RefreshToken == "" {
			refreshed.RefreshToken = refreshTok
		}
		// A successful exchange always writes a clean record: any stale
		// re-login marker is resolved by this rotation.
		refreshed.NeedsRelogin = false
		refreshed.ReloginReason = ""
		if latest != nil && refreshed.Email == "" {
			refreshed.Email = latest.Email
		}
		current, loadErr := loadKeychainTokensFn()
		if keychainRecordDeleted(loadErr) {
			// The item was deleted while the exchange was in flight (the
			// pre-loop load can miss that when the keychain is momentarily
			// unreadable). The rotated tokens are perfectly good, and saving
			// them would RECREATE the record the user deleted. Drop them.
			p.clearCache()
			return "", errCredentialsRemoved
		}
		if loadErr == nil && current.RefreshToken != "" && current.RefreshToken != refreshTok {
			p.mu.Lock()
			p.applyTokensLocked(current)
			p.mu.Unlock()
			if current.valid() {
				return current.AccessToken, nil
			}
			// Same rule as the pre-loop load and the invalid_grant recovery:
			// a flagged record never drives another exchange. Without this
			// the loop would dispatch attempt 2 with a refresh token the
			// shared record explicitly marks as unusable.
			if reason := current.reloginReason(); reason != "" {
				return "", reloginError(reason)
			}
			latest = current
			refreshTok = current.RefreshToken
			continue
		}
		p.mu.Lock()
		p.applyTokensLocked(refreshed)
		p.mu.Unlock()
		if err := saveKeychainTokensFn(refreshed); err != nil {
			return refreshed.AccessToken, fmt.Errorf("save refreshed tokens: %w", err)
		}
		return refreshed.AccessToken, nil
	}

	// Both `continue` paths above adopted newer, UNFLAGGED credentials, so
	// exhausting the loop means we kept being superseded — not that the user
	// must sign in again. Returning ErrAuthExpired here would park both stream
	// loops behind AuthState with a clean record on disk and nothing left to
	// call MarkOK, the same stranding flagRetainedRecord exists to avoid.
	return "", errRefreshSuperseded
}

func (p *KeychainTokenProvider) applyTokensLocked(tokens *keychainTokens) {
	if reason := tokens.reloginReason(); reason != "" {
		// The record is retained on disk for its identity, but a provider
		// loaded from it must expose no bearer token and must never replay the
		// refresh token it still carries.
		p.accessToken = ""
		p.refreshToken = ""
		p.expiresAt = time.Time{}
		p.reloginReason = reason
		return
	}
	p.accessToken = tokens.AccessToken
	p.refreshToken = tokens.RefreshToken
	p.expiresAt = tokens.ExpiresAt
	p.reloginReason = ""
}

// ReloginReason reports the persisted re-login reason observed by the last
// keychain read, or "" when the cached credentials are usable.
func (p *KeychainTokenProvider) ReloginReason() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.reloginReason
}

// markNeedsRelogin persists the non-secret re-login marker and retained
// identity, clears token material, and disables the in-memory cache. It never
// deletes the keychain item. Clearing the tokens keeps older binaries that
// ignore the marker from replaying a refresh token whose outcome is unknown.
//
// It re-reads the durable record immediately before writing rather than
// trusting the pre-exchange snapshot. Current `boss login` and `boss logout`
// take this same cross-process lock around their credential mutations, so
// their read-modify-write operations cannot interleave with this marker save.
// The re-read still protects against a second loop attempt and external or
// older writers that did not take the lock.
//
// When the re-read finds credentials that are not ours to flag, it ADOPTS
// them and returns them instead of writing anything; callers must treat a
// non-nil adopted record as "this attempt is obsolete", never as a terminal
// re-login state (see flagRetainedRecord).
func (p *KeychainTokenProvider) markNeedsRelogin(refreshTok, reason string) (adopted *keychainTokens, err error) {
	current, loadErr := loadKeychainTokensFn()
	if loadErr != nil {
		// The durable record cannot be verified. This includes an explicit
		// logout, but a transient keychain failure could also hide newer
		// credentials; do not write a marker from our stale snapshot.
		p.disableCacheForRelogin(reason)
		return nil, fmt.Errorf("%w: %w", errReloginMarkerNotPersisted, loadErr)
	}
	if current.reloginReason() == "" && current.RefreshToken != "" && current.RefreshToken != refreshTok {
		// Someone else stored newer credentials while our exchange was
		// failing. Flagging them would be precisely the "overwritten or
		// marked failed by a stale caller" this ticket forbids: adopt
		// them and leave the durable record alone.
		p.mu.Lock()
		p.applyTokensLocked(current)
		p.mu.Unlock()
		return current, nil
	}
	// current is non-nil here: loadErr == nil (the unreadable case returned
	// above) and every load returns a record alongside a nil error — the
	// adoption check above already dereferences it, so a (nil, nil) load would
	// panic there rather than reach this point.
	marked := *current
	marked.AccessToken = ""
	marked.RefreshToken = ""
	marked.NeedsRelogin = true
	marked.ReloginReason = reason
	saveErr := saveKeychainTokensFn(&marked)

	p.disableCacheForRelogin(reason)

	if saveErr != nil {
		return nil, fmt.Errorf("%w: %w", errReloginMarkerNotPersisted, saveErr)
	}
	return nil, nil
}

// errRefreshSuperseded reports that this exchange attempt is obsolete because
// another process stored fresh credentials while it was failing. It is a
// PLAIN error on purpose: it must not compose with ErrAuthExpired, because the
// openers and the periodic refresher answer ErrAuthExpired by marking the
// shared AuthState, and the only thing that ever clears that mark is a
// login's NotifyLogin. A login that already fired its notification before our
// stale attempt failed would leave both stream loops parked on AuthState.Wait
// forever with perfectly good credentials in hand.
var errRefreshSuperseded = errors.New("refresh superseded: newer credentials were stored by another process")

// flagRetainedRecord persists the re-login marker for a terminal refresh
// failure and converts the outcome into refresh()'s return values. It is the
// single place that decides whether a failure is really terminal: when
// markNeedsRelogin adopted fresher credentials instead of flagging them, the
// failure is not terminal at all.
func (p *KeychainTokenProvider) flagRetainedRecord(refreshTok, reason string, err error) (string, error) {
	adopted, markErr := p.markNeedsRelogin(refreshTok, reason)
	if markErr != nil {
		return "", errors.Join(err, markErr)
	}
	if adopted == nil {
		return "", err
	}
	if adopted.valid() {
		return adopted.AccessToken, nil
	}
	// Adopted but not yet usable (an expired access token with a newer refresh
	// token). The next Refresh exchanges the adopted token; reporting the
	// terminal error here would pause the daemon instead.
	return "", errRefreshSuperseded
}

// disableCacheForRelogin drops the in-memory credentials and records the
// re-login reason, so the provider exposes no bearer token and refuses to
// dispatch another exchange until a fresh login clears the record.
func (p *KeychainTokenProvider) disableCacheForRelogin(reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearCacheLocked()
	p.reloginReason = reason
}

// clearCache drops the in-memory credentials WITHOUT a re-login reason. That
// distinction is the point: a flagged record is retained on disk and explains
// itself through reloginPauseMessage, whereas a deleted one is simply gone, so
// the openers' generic "no upstream credentials" pause is the accurate wording.
func (p *KeychainTokenProvider) clearCache() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearCacheLocked()
}

func (p *KeychainTokenProvider) clearCacheLocked() {
	p.accessToken = ""
	p.refreshToken = ""
	p.expiresAt = time.Time{}
	p.reloginReason = ""
}

// preservedReloginReason picks the reason to persist for an authoritative
// rejection. When the retained record already carries a pending
// unknown-outcome marker for this very refresh token and WorkOS now confirms
// the token was already exchanged, that earlier reason is the accurate
// description of what happened, so it is kept rather than downgraded.
//
// Reviewers keep reading that branch as dead, on the grounds that refresh()
// returns early for a marked record before it can ever dispatch. It is not.
// The two early returns (the in-memory pendingReason, and the marker on the
// record loaded under the authlock) BOTH require a successful read. When the
// pre-exchange load ERRORS, refresh() falls through with only its stale
// in-memory token, dispatches, and the post-failure reload can then succeed
// and hand back a record a PEER daemon flagged before we took the lock —
// carrying the same refresh token, hence the "already exchanged" answer. That
// is the interleaving TestKeychainTokenProviderRefreshKeepsPendingUnknownReason
// stages with its load counter: unreadable-then-flagged, not clean-then-flagged.
func preservedReloginReason(preserved *keychainTokens, refreshTok string, err error) string {
	if preserved != nil &&
		preserved.reloginReason() == reloginReasonRefreshOutcomeUnknown &&
		preserved.RefreshToken == refreshTok &&
		errors.Is(err, ErrRefreshTokenAlreadyExchanged) {
		return reloginReasonRefreshOutcomeUnknown
	}
	return reloginReasonRefreshTokenRejected
}
