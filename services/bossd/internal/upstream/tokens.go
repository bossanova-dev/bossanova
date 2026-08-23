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
	"io/fs"
	"net"
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
// refresh token — but that ambiguity does not have to be resolved to act on
// it. WorkOS keeps a 30-second replay grace window (workOSReplayGraceWindow)
// in which re-presenting the SAME refresh token returns the tokens it already
// rotated, idempotently, rather than an error; its session-resilience guidance
// is explicitly "retry the same refresh token" and "never destroy a session on
// a transient failure" (https://workos.com/docs/authkit/session-resilience).
// So refresh() replays this error immediately, inside that window, and only
// fails closed — persisting the reason and waiting for a new login — once the
// replay budget (maxAmbiguousDispatches) is spent. BOS-941 reversed BOS-659's
// fail-on-the-first-dispatch behaviour on the strength of that documented
// window: a replay that lands after it is answered authoritatively, which is
// the same terminal state failing closed produced immediately, only reached
// after the recoverable case has been given its chance.
var ErrRefreshOutcomeUnknown = fmt.Errorf("refresh outcome could not be confirmed: %w", ErrAuthExpired)

// refreshFailureClass is the enumerated, non-secret shape of a failed refresh
// exchange. The sanitized pause warning deliberately drops the wrapped error
// (it carries transport detail and can echo response bytes), which left the
// logs unable to distinguish "the laptop's network stalled" from "WorkOS
// answered something we could not read" — the two have completely different
// fixes, and the sign-out they produce looks identical. These values carry the
// distinction without carrying any of the detail that made the error unsafe to
// log: they are a closed set of constants chosen here, never upstream strings.
type refreshFailureClass string

const (
	// refreshFailureDial: no connection was ever established, so WorkOS
	// provably never saw the exchange. Safe, retryable.
	refreshFailureDial refreshFailureClass = "dial"
	// refreshFailureTimeout: a connection was in hand and the deadline expired
	// before a response arrived. Ambiguous.
	refreshFailureTimeout refreshFailureClass = "timeout"
	// refreshFailureTransport: a connection was in hand and the transport
	// failed for a non-timeout reason. Ambiguous.
	refreshFailureTransport refreshFailureClass = "transport"
	// refreshFailureResponseRead: WorkOS answered and the body could not be
	// read. Ambiguous.
	refreshFailureResponseRead refreshFailureClass = "response_read"
	// refreshFailureResponseDecode: HTTP 200 that would not decode. Ambiguous,
	// and the token was certainly rotated.
	refreshFailureResponseDecode refreshFailureClass = "response_decode"
	// refreshFailureNoAccessToken: HTTP 200, well-formed, no usable token.
	refreshFailureNoAccessToken refreshFailureClass = "no_access_token"
)

// refreshFailure annotates a refresh error with its non-secret class and
// whether the underlying connection came from the pool. connReused is the
// single most useful field when triaging an overnight sign-out: a reused
// connection that then timed out is the half-open-pool signature, whereas a
// fresh connection that timed out points at genuine upstream latency.
type refreshFailure struct {
	class      refreshFailureClass
	connReused bool
	err        error
}

func (f *refreshFailure) Error() string { return f.err.Error() }
func (f *refreshFailure) Unwrap() error { return f.err }

// withRefreshFailure tags err with its class. connReused is meaningless for
// classes that never held a connection; callers pass false there.
func withRefreshFailure(class refreshFailureClass, connReused bool, err error) error {
	return &refreshFailure{class: class, connReused: connReused, err: err}
}

// refreshFailureOf recovers the annotation from anywhere in err's chain.
func refreshFailureOf(err error) (*refreshFailure, bool) {
	var f *refreshFailure
	if errors.As(err, &f) {
		return f, true
	}
	return nil, false
}

// classifyDoError separates a transport failure that timed out from one that
// did not. Both are ambiguous once a connection existed; the distinction is
// purely diagnostic.
func classifyDoError(err error) refreshFailureClass {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return refreshFailureTimeout
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return refreshFailureTimeout
	}
	return refreshFailureTransport
}

// Enumerated, non-secret re-login reasons persisted on the shared
// "workos-tokens" keychain record. These strings are part of the on-disk
// contract with services/boss/internal/auth (auth.ReloginReason*); keep the
// two lists in step.
const (
	reloginReasonRefreshOutcomeUnknown = "refresh_outcome_unknown"
	reloginReasonRefreshTokenRejected  = "refresh_token_rejected"
	legacyTokenKey                     = "workos-tokens"
	versionedTokenKey                  = "workos-tokens-v1"
	tokenRecordV1                      = 1
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

// keychainTokenRecord is the authoritative, versioned WorkOS payload shared
// with the CLI. Current readers only consult the legacy key when this key is
// absent, so an older writer cannot erase a re-login marker by rewriting the
// old record.
type keychainTokenRecord struct {
	Version int `json:"version"`
	keychainTokens
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
// unflagged record and will re-present the very refresh token whose replay
// budget this run had already spent — long outside the grace window that made
// those replays idempotent. Callers match it with errors.Is to log the
// distinction.
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
	// Enumerated diagnostics only — see refreshFailureClass. Without these the
	// pause line says a sign-in is needed but nothing about which failure
	// produced it, which is the difference between a five-minute diagnosis and
	// a log archaeology session.
	if failure, ok := refreshFailureOf(err); ok {
		event = event.Str("refresh_failure_class", string(failure.class)).
			Bool("conn_reused", failure.connReused)
	}
	if errors.Is(err, errReloginMarkerNotPersisted) {
		// The pause is correct, but it is not durable: a restart reloads an
		// unflagged record and would replay the token.
		event = event.Bool("relogin_marker_persisted", false)
	}
	event.Msg(prefix + reloginPauseMessage(reason))
}

// logAuthWedge announces the moment a provider's in-memory credentials go from
// usable to disabled. It is deliberately separate from logReloginPause: that
// line is emitted by whoever *decided* to pause and carries the failure that
// caused it, whereas this one fires inside the provider on the state
// transition itself and carries only the enumerated reason.
//
// It exists because the BOS-942 incident had a daemon whose provider had been
// reloaded from an already-marked record at startup. Nothing decided to pause
// in that process — there was no failing exchange to report — so no
// logReloginPause line was ever written, and the log showed a healthy daemon
// while every registration attempt failed. Only enumerated, non-secret fields
// are attached; never a token, an Authorization header, or a response body.
func logAuthWedge(logger *zerolog.Logger, reason string) {
	event := logger.Warn()
	if reason != "" {
		event = event.Str("relogin_reason", reason)
	}
	event.Msg("upstream credentials are marked for re-login; this daemon cannot authenticate until `boss login` runs")
}

// replayWarnFallback is where logRefreshReplay writes when the caller's context
// carries no logger. It is os.Stderr because that is where bossalog.Setup points
// the daemon's global logger, so a dropped-context warning lands in the same
// bossd.stderr.log as every other line — just without the caller's component
// field. A var, not a const, only so the test can capture it.
var replayWarnFallback io.Writer = os.Stderr

// logRefreshReplay emits the one warning per in-window replay of an exchange
// whose outcome could not be confirmed. Same sanitization rule as
// logReloginPause: the enumerated class and the pooled-connection flag, never
// the wrapped error, which carries transport detail. The replay index is what
// makes the two outcomes legible in the log after the fact — a sign-out
// preceded by replay 1 and replay 2 is an exhausted budget, whereas one with no
// replay lines at all is an authoritative rejection that never had a budget.
//
// The logger rides on ctx (Logger.WithContext at each Refresh call site) rather
// than on the provider. Not because no logger exists yet — bossalog.Setup runs
// long before NewKeychainTokenProvider — but because the CALLER's logger is the
// right one: this line is meant to be read against the sign-out line that
// logReloginPause emits, and that sibling takes the caller's *zerolog.Logger as
// a parameter for exactly that reason, so both carry the same component field.
// refresh() is several frames below Refresh, and ctx is the only thing already
// threaded that far; a provider field would need one logger for callers that
// have different ones, and a parameter would change four internal signatures.
//
// Read off the context that seam WOULD fail silently — zerolog.Ctx returns a
// disabled logger when nothing was attached, so a Refresh call site that
// forgets WithContext drops every replay line with no error and no panic, which
// already happened once on this branch. replayWarnFallback closes that: a
// missing context logger costs the caller's component field, not the line. The
// degradation is therefore visible in the log rather than invisible, and
// TestLogRefreshReplayFallsBackWhenContextCarriesNoLogger pins it; the
// structural guarantee is what makes the four call sites safe to add to, since
// no test can reach a call site that does not exist yet.
func logRefreshReplay(ctx context.Context, replay int, err error) {
	logger := zerolog.Ctx(ctx)
	if logger.GetLevel() == zerolog.Disabled {
		fallback := zerolog.New(replayWarnFallback).With().Timestamp().Logger()
		logger = &fallback
	}
	event := logger.Warn().Int("refresh_replay", replay)
	if failure, ok := refreshFailureOf(err); ok {
		event = event.Str("refresh_failure_class", string(failure.class)).
			Bool("conn_reused", failure.connReused)
	}
	event.Msg("upstream token refresh outcome unconfirmed; replaying the refresh token inside WorkOS's grace window")
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

// workOSRefreshTimeout bounds one WorkOS refresh exchange end to end (dial,
// TLS handshake, request, response).
//
// It is deliberately SHORT, and that is a reversal of the reasoning that once
// raised it from 10s to 30s (buy enough budget for a cold DNS lookup, TCP
// connect, TLS handshake and round trip on a just-woken laptop). Under WorkOS's
// replay grace window that reasoning inverts. The window
// (workOSReplayGraceWindow) opens when WorkOS processes the exchange — near
// enough to when we dispatch it — so a timeout as long as the window guarantees
// the replay lands OUTSIDE the only interval in which recovery is possible, and
// rescues only the case where WorkOS never saw the request at all. With a replay
// budget in place the asymmetry runs the other way: a too-short timeout costs
// one extra dispatch, while a too-long timeout costs a sign-out. 8s leaves room
// for maxAmbiguousDispatches whole dispatches inside the window.
const workOSRefreshTimeout = 8 * time.Second

// maxAmbiguousDispatches and workOSReplayGraceWindow are a PAIR, and so is
// workOSRefreshTimeout above: they are only correct together, and changing any
// one of them without re-checking the invariant below silently removes the
// recovery this budget exists to provide.
//
// workOSReplayGraceWindow is WorkOS's documented refresh-token replay grace
// period: for 30 seconds after a refresh token is exchanged, replaying that
// same token returns the same rotated tokens instead of an error
// (https://workos.com/docs/authkit/session-resilience). It is WorkOS's number,
// not a knob — it is declared here so the arithmetic can be checked against it,
// and nothing reads it at run time.
//
// maxAmbiguousDispatches is how many times refresh() may dispatch the SAME
// refresh token while the outcome stays unconfirmed (the original plus two
// replays). The bound is a count, with no wall-clock check and no backoff, on
// purpose: KeychainTokenProvider has no injectable clock, and every second of
// backoff would spend the very window that makes the replay idempotent. A count
// is sufficient because the worst case is a static arithmetic property of these
// three constants rather than a runtime measurement:
//
//	maxAmbiguousDispatches * workOSRefreshTimeout < workOSReplayGraceWindow
//	                    3 *                  8s  <                     30s
//
// TestWorkOSReplayBudgetFitsGraceWindow asserts exactly that, so raising the
// timeout back toward 30s — or raising the dispatch count — fails the build
// instead of quietly pushing replays outside the window.
const (
	maxAmbiguousDispatches  = 3
	workOSReplayGraceWindow = 30 * time.Second
)

// maxSupersedeAttempts bounds the exchange loop's OTHER budget: the two
// supersede-adoption paths, which retry with NEWER credentials another writer
// stored while our exchange was failing. It stays at the 2 dispatches it has
// always been. It is named, and counted separately from the replay budget,
// because the two must not share a counter — see the loop in refresh().
const maxSupersedeAttempts = 2

// newWorkOSRefreshHTTPClient builds the client used for the refresh exchange.
//
// It shares the daemon's HTTP/2 keepalive settings (BuildUpstreamHTTPClient's
// transport) rather than using http.DefaultTransport. Without the keepalive
// PINGs, a pooled HTTP/2 connection to api.workos.com that has gone half-open
// — no FIN/RST, exactly what a sleep/wake or network-path change produces — is
// handed straight back out of the pool. httptrace's GotConn then fires, the
// request is written into a dead connection, and the failure is classified as
// a dispatched-but-unconfirmed exchange: the unrecoverable case. With
// ReadIdleTimeout/PingTimeout the dead connection is detected and evicted in
// the background, so the next refresh dials a fresh one instead.
func newWorkOSRefreshHTTPClient() *http.Client {
	tr, _ := buildHTTPSUpstreamTransport()
	return &http.Client{Transport: tr, Timeout: workOSRefreshTimeout}
}

// Package-level seams the tests swap out. openKeyring opens the shared
// bossanova keyring: bossd runs as a daemon with no flag plumbing, so
// allowInsecure is hard-wired to false here — a broken environment should
// surface a real error rather than silently reverting to the hardcoded
// passphrase.
var (
	workOSRefreshHTTPClient = newWorkOSRefreshHTTPClient()
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
	item, err := ring.Get(versionedTokenKey)
	if err != nil {
		if !tokenKeyMissing(err) {
			return nil, err
		}
		return loadLegacyKeychainTokens(ring)
	}
	var record keychainTokenRecord
	if err := json.Unmarshal(item.Data, &record); err != nil {
		return nil, err
	}
	if record.Version != tokenRecordV1 {
		return nil, errors.New("unsupported WorkOS token record version")
	}
	return &record.keychainTokens, nil
}

func loadLegacyKeychainTokens(ring keyring.Keyring) (*keychainTokens, error) {
	item, err := ring.Get(legacyTokenKey)
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
	data, err := json.Marshal(keychainTokenRecord{Version: tokenRecordV1, keychainTokens: *tokens})
	if err != nil {
		return err
	}
	if err := ring.Set(keyring.Item{
		Key:         versionedTokenKey,
		Data:        data,
		Label:       "Bossanova",
		Description: "WorkOS authentication tokens",
	}); err != nil {
		return err
	}
	if err := ring.Remove(legacyTokenKey); err != nil && !tokenKeyMissing(err) {
		return err
	}
	return nil
}

func tokenKeyMissing(err error) bool {
	return errors.Is(err, keyring.ErrKeyNotFound) || errors.Is(err, fs.ErrNotExist)
}

// NOTE (BOS-659): there is deliberately no removeKeychainTokens here. No
// automatic daemon path may delete the shared "workos-tokens" item — an
// ambiguous or rejected refresh flags the record instead, so the user's
// identity survives and only an explicit `boss logout` removes it.

// keychainRecordDeleted discriminates the two ways a keychain read fails, which
// need OPPOSITE responses:
//
//   - keyring.ErrKeyNotFound or fs.ErrNotExist means the shared
//     "workos-tokens" item is gone.
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
	return tokenKeyMissing(err)
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
	var gotConn, connReused atomic.Bool
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GetConn: func(string) { gotConn.Store(false); connReused.Store(false) },
		GotConn: func(info httptrace.GotConnInfo) {
			gotConn.Store(true)
			connReused.Store(info.Reused)
		},
	}))

	// From the moment a connection is in hand, a failure leaves the refresh
	// token's fate unknown: WorkOS may already have consumed and rotated it.
	// Report that ambiguity as ErrRefreshOutcomeUnknown and let refresh()
	// decide — it replays the same token inside WorkOS's grace window
	// (maxAmbiguousDispatches) and only fails closed once that budget is
	// spent. This function stays a pure classifier and holds no policy.
	resp, err := workOSRefreshHTTPClient.Do(req)
	if err != nil {
		if !gotConn.Load() {
			return nil, withRefreshFailure(refreshFailureDial, false,
				fmt.Errorf("refresh request never dispatched: %w", err))
		}
		return nil, withRefreshFailure(classifyDoError(err), connReused.Load(),
			fmt.Errorf("%w: %w", ErrRefreshOutcomeUnknown, err))
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
		return nil, withRefreshFailure(refreshFailureResponseRead, connReused.Load(),
			fmt.Errorf("%w: %w", ErrRefreshOutcomeUnknown, readErr))
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
		// than the read failure above, so it is ambiguous too — and BOS-941
		// replays it like any other unconfirmed outcome. Only one of the two
		// sub-cases can actually be recovered that way: a body corrupted in
		// transit decodes on the replay, while a deterministically malformed
		// 200 fails identically every time and simply spends the budget before
		// reaching the terminal state it would have reached at once. Spending
		// it is the right trade — the two are indistinguishable from here, and
		// only the recoverable one is worth being wrong about. The unmarshal
		// error is not wrapped, because it embeds the response bytes it choked
		// on and those can echo token material.
		return nil, withRefreshFailure(refreshFailureResponseDecode, connReused.Load(),
			fmt.Errorf("%w: refresh response could not be decoded", ErrRefreshOutcomeUnknown))
	}
	if result.AccessToken == "" {
		// Same hazard, well-formed JSON: a 200 rotated the token, but the
		// response carries no usable replacement. Without this the caller
		// would happily save a token-less record over the good one (and
		// AccessTokenExpiry would stamp it with a default TTL), destroying
		// the credentials this ticket exists to retain.
		return nil, withRefreshFailure(refreshFailureNoAccessToken, connReused.Load(),
			fmt.Errorf("%w: refresh response carried no access token", ErrRefreshOutcomeUnknown))
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

	// logger is where the credential-state transition warning goes. A pointer
	// (not a value) so a provider built as a bare struct literal — which
	// several tests do — is safely silent instead of panicking on zerolog's
	// nil writer. Nil means "no logger set"; see logOrNop.
	logger *zerolog.Logger

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

// nopLogger backs logOrNop. Package-level so every unlogged provider shares
// one instead of allocating per call.
var nopLogger = zerolog.Nop()

// SetLogger points the provider's credential-state warnings at a real logger.
// Separate from the constructor so NewKeychainTokenProvider keeps its existing
// zero-argument signature and every current caller compiles unchanged; a
// provider that is never given one stays silent.
func (p *KeychainTokenProvider) SetLogger(logger zerolog.Logger) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.logger = &logger
	// NewKeychainTokenProvider reads the keychain inside the constructor, so
	// the startup load — the one that discovers an already-marked record and
	// the single most important one to hear about — has always already
	// happened by the time a logger can be attached. Replay it here rather
	// than losing exactly the case this warning exists for. Still one line:
	// applyTokensLocked will not re-announce a reason that is already set.
	if p.reloginReason != "" {
		logAuthWedge(p.logOrNop(), p.reloginReason)
	}
}

// logOrNop returns the provider's logger, or a no-op one when unset. Caller
// holds p.mu (read or write).
func (p *KeychainTokenProvider) logOrNop() *zerolog.Logger {
	if p.logger == nil {
		return &nopLogger
	}
	return p.logger
}

// Reload re-reads the keychain entry into the in-memory cache. Used by
// the auth-change notifier so a fresh `boss login` (which writes new
// tokens to the keychain) is observable to the running daemon without a
// restart — calling Refresh here would fail because the cached refresh
// token has been superseded by the new login.
func (p *KeychainTokenProvider) Reload() {
	_ = p.ReloadResult()
}

// Enumerated, non-secret classes for a reload that did not read the record.
// They never carry the underlying error text, which can embed record bytes.
const (
	// ReloadErrorRecordDeleted means the shared record is gone — an
	// authoritative answer, and the only one that clears the cache.
	ReloadErrorRecordDeleted = "record_deleted"
	// ReloadErrorReadFailed means the record could not be read right now. It
	// may be perfectly intact; the cache is deliberately left alone.
	ReloadErrorReadFailed = "read_failed"
)

// ReloadOutcome reports what a reload actually observed, as opposed to what
// the cache happens to hold afterwards.
//
// The distinction is the whole point. loadFromKeychain preserves the cache on
// any read failure that is not "record deleted", so reading Token() or
// ReloginReason() after a Reload describes the CACHE. Without this, an empty
// token after a login would mean either "the record really is still flagged"
// or "the reload could not read the record at all" — the exact ambiguity that
// made the original incident undiagnosable.
type ReloadOutcome struct {
	// ReadOK reports whether the keychain read itself succeeded.
	ReadOK bool
	// ErrorClass is one of the ReloadError* constants, empty when ReadOK.
	ErrorClass string
}

// ReloadResult re-reads the keychain entry like Reload and reports whether the
// read succeeded. Reload keeps its bare signature because callers reach it
// through a `reloader` interface.
func (p *KeychainTokenProvider) ReloadResult() ReloadOutcome {
	return p.loadFromKeychain()
}

// loadFromKeychain snapshots the keychain entry into the in-memory cache.
// Safe to call repeatedly. A DELETED entry clears the cache, so an explicit
// `boss logout` de-authorises a daemon that is already running; any other read
// failure leaves the cache alone, because the record may still be there.
func (p *KeychainTokenProvider) loadFromKeychain() ReloadOutcome {
	tokens, err := loadKeychainTokensFn()
	if err != nil {
		if keychainRecordDeleted(err) {
			p.clearCache()
			return ReloadOutcome{ErrorClass: ReloadErrorRecordDeleted}
		}
		return ReloadOutcome{ErrorClass: ReloadErrorReadFailed}
	}
	p.mu.Lock()
	p.applyTokensLocked(tokens)
	p.mu.Unlock()
	return ReloadOutcome{ReadOK: true}
}

// KeyringBackend names the credential backend this process resolved, so a
// reload that read nothing can be told apart from one that read a different
// store than the CLI wrote to. It is configuration, never credentials.
func KeyringBackend() string {
	backends := keyringutil.Backends()
	if len(backends) == 0 {
		return "platform-default"
	}
	return string(backends[0])
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

	// Two budgets, deliberately on two counters. `attempts` bounds the
	// supersede-adoption paths below, whose `continue`s retry with NEWER
	// credentials; `replays` bounds the ambiguous-outcome path, whose `continue`
	// retries the SAME token inside WorkOS's grace window. Sharing one counter
	// — as this loop did when supersede was its only retry — would let a
	// supersede race spend the replay budget and vice versa.
	//
	// Only the supersede budget appears in the loop condition, and that is the
	// point: each budget is enforced in exactly ONE place. Replay exhaustion is
	// enforced inside the ErrRefreshOutcomeUnknown branch below, which returns
	// flagRetainedRecord rather than falling out of the loop — so a
	// `replays < maxAmbiguousDispatches` conjunct here could never fire, and
	// stating the same invariant twice invites an editor to trust the condition
	// and drop the branch's `return`, which would silently turn a spent replay
	// budget into the non-terminal errRefreshSuperseded below. The overall
	// bound is still at most maxSupersedeAttempts + maxAmbiguousDispatches - 1
	// dispatches, which
	// TestKeychainTokenProviderRefreshAmbiguousAfterStaleRecoveryClearsNewerToken
	// pins by scripting every dispatch of a mixed supersede-plus-replay run.
	//
	// The separation is of the BUDGETS, not of the tokens: `replays` is not
	// reset when a supersede adopts a newer token, so a token adopted after the
	// replay budget is partly spent inherits what is left of it rather than a
	// fresh three. That is deliberate. It keeps the window invariant trivially
	// true (per-token dispatches stay at or below maxAmbiguousDispatches, and
	// the overall bound stays legible above), and it is strictly better than
	// the pre-BOS-941 behaviour, where an adopted token got no replay at all.
	// Resetting would give each token its own budget at the cost of 6
	// dispatches and 48s of single-flight blocking, which is the trade this
	// declines.
	//
	// unconfirmedTok is the identity half that `replays` cannot carry: the
	// token whose dispatch actually went unconfirmed. `replays` survives a
	// supersede adoption but `refreshTok` does not, so the count alone cannot
	// answer "was THIS token the one we replayed?" — which is exactly what the
	// re-login reason override below has to know.
	attempts, replays := 0, 0
	var unconfirmedTok string
	for attempts < maxSupersedeAttempts {
		requestCtx, requestCancel := context.WithTimeout(ctx, workOSRefreshTimeout)
		refreshed, err := refreshWorkOSTokenFn(requestCtx, clientID, refreshTok)
		requestCancel()
		if err != nil {
			// Ambiguous outcome: the exchange was dispatched but never
			// confirmed. WorkOS's replay grace window makes re-presenting the
			// SAME token the documented recovery — inside the window it returns
			// the tokens already rotated for the lost response — so replay
			// immediately: no backoff and no keychain re-read, both of which
			// spend the window this depends on. Only an exhausted budget keeps
			// the credentials as stored and records why re-login is needed,
			// exactly as before. Checked before the terminal branch because both
			// compose with ErrAuthExpired, and the replay lives lexically INSIDE
			// this branch so an authoritative ErrRefreshTokenRejected below
			// cannot inherit the budget — replaying a token WorkOS has
			// definitively refused is waste, not safety.
			if errors.Is(err, ErrRefreshOutcomeUnknown) {
				// A dead parent context makes both halves of this branch
				// wrong. The budget is sized in wall-clock terms
				// (maxAmbiguousDispatches whole workOSRefreshTimeouts inside
				// the grace window), but a cancelled or nearly-expired ctx
				// truncates every requestCtx below it, so the remaining
				// dispatches fail in microseconds — and they fail AMBIGUOUSLY,
				// because net/http reports GotConn from the idle pool before
				// roundTrip observes the dead context. Spending the budget that
				// way and then writing the durable sign-out marker would turn a
				// caller-side deadline (a shutdown, or the startup refresh's
				// 10s cap in cmd/main.go) into a permanent sign-out that no
				// WorkOS exchange ever justified. Return a plain error instead:
				// it does not compose with ErrAuthExpired, so the tick-level
				// retry in runTokenRefresher picks the exchange back up with a
				// live context.
				if ctxErr := ctx.Err(); ctxErr != nil {
					return "", fmt.Errorf("refresh outcome unconfirmed and the caller's context ended before a replay could be spent: %w", ctxErr)
				}
				replays++
				if replays >= maxAmbiguousDispatches {
					return p.flagRetainedRecord(refreshTok, reloginReasonRefreshOutcomeUnknown, err)
				}
				unconfirmedTok = refreshTok
				logRefreshReplay(ctx, replays, err)
				continue
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
					attempts++
					continue
				}
				// Authoritative rejection with nothing newer to adopt: retain
				// the entry (email included) behind the re-login marker.
				preserved := latest
				if loadErr == nil {
					preserved = reloaded
				}
				reason := preservedReloginReason(preserved, refreshTok, err)
				if unconfirmedTok != "" && refreshTok == unconfirmedTok &&
					errors.Is(err, ErrRefreshTokenAlreadyExchanged) {
					// Our own replay of THIS token produced this answer, so the
					// token was consumed by the dispatch whose response we lost:
					// the unconfirmed exchange is why re-login is needed, not a
					// bad credential. Same precision rule as
					// preservedReloginReason's cross-daemon case, sourced from
					// this loop's own history instead of from a marker another
					// daemon left on the record.
					//
					// The token-identity conjunct is load-bearing, not
					// belt-and-braces: `replays` is loop-global and survives a
					// supersede adoption, so `replays > 0` alone would persist
					// refresh_outcome_unknown for the ordering "unknown on token
					// A, adopt token B, already-exchanged on token B" — a token
					// this loop never replayed, and one for which
					// refresh_token_rejected is the accurate reason.
					// preservedReloginReason makes the same identity check for
					// the same reason.
					reason = reloginReasonRefreshOutcomeUnknown
				}
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
			attempts++
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

	// Only the supersede budget can bring us here: the replay branch flags from
	// inside itself. Both supersede `continue` paths adopted newer, UNFLAGGED
	// credentials, so exhausting that budget means we kept being superseded —
	// not that the user must sign in again. Returning ErrAuthExpired here would park both stream
	// loops behind AuthState with a clean record on disk and nothing left to
	// call MarkOK, the same stranding flagRetainedRecord exists to avoid.
	return "", errRefreshSuperseded
}

func (p *KeychainTokenProvider) applyTokensLocked(tokens *keychainTokens) {
	previous := p.reloginReason
	if reason := tokens.reloginReason(); reason != "" {
		// The record is retained on disk for its identity, but a provider
		// loaded from it must expose no bearer token and must never replay the
		// refresh token it still carries.
		p.accessToken = ""
		p.refreshToken = ""
		p.expiresAt = time.Time{}
		p.reloginReason = reason
		// Announce the TRANSITION only. applyTokensLocked runs on every
		// keychain read — startup, Reload, and each refresh — so warning
		// unconditionally would emit the same line on a cadence for a
		// condition that is permanent until `boss login`. Gating on a CHANGE
		// makes it one line per time the diagnosis actually moves, including
		// the startup read that loads an already-marked record (previous is ""
		// on a fresh provider), which is exactly the BOS-942 case that
		// otherwise logged nothing at all.
		//
		// It gates on previous != reason rather than previous == "" because
		// the reason is the operator's only cause line, and it does change —
		// refresh_outcome_unknown hardening into refresh_token_rejected, say.
		// Announcing only the first one left that line naming a superseded
		// diagnosis for the rest of the process lifetime. The anti-spam
		// property is unchanged: an unchanged reason still logs exactly once.
		//
		// "Once per change" is bounded rather than merely finite only because
		// the marker does not oscillate: it is written by the refresh paths in
		// this process, and each transition is a hardening (unset ->
		// refresh_outcome_unknown -> refresh_token_rejected) that no path
		// walks back without a `boss login` clearing the record outright. If a
		// second writer against the same record is ever introduced, alternating
		// markers would announce at the refresh cadence — gate on a monotone
		// severity order at that point, not on inequality.
		if previous != reason {
			logAuthWedge(p.logOrNop(), reason)
		}
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

// CredentialVerdict reports whether the cached credentials are usable, and the
// persisted re-login reason when they are not.
//
// It exists so callers can ask that question under a SINGLE lock. Composing it
// from separate ReloginReason() and Token() calls takes two RLocks, and a
// concurrent Refresh/applyTokensLocked can commit between them — the composed
// answer would then describe a state that never existed (a token from before
// the flag, or a flag from after the token). applyTokensLocked already writes
// both fields together under the write lock; reading them together is the
// matching half.
//
// usable is deliberately the conjunction of both fields rather than either
// alone. A non-empty reloginReason means the record is disabled even if a
// stale accessToken were still cached, and an empty accessToken means there is
// nothing to dial with even when no marker was ever written — the two failures
// have different causes and must stay distinguishable to the caller, which is
// why the reason is returned alongside rather than folded into a single string.
func (p *KeychainTokenProvider) CredentialVerdict() (usable bool, reloginReason string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.reloginReason == "" && p.accessToken != "", p.reloginReason
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
// The early returns (the in-memory pendingReason, and the marker on the record
// loaded under the authlock) both depend on the state visible before dispatch.
// A writer that does not hold the credential lock, such as an older boss binary
// or a foreign shared-keychain process, can flag the same refresh token after
// our under-lock pre-exchange read but before the post-failure reload. That is
// the interleaving TestKeychainTokenProviderRefreshKeepsPendingUnknownReason
// stages with its load counter: clean-then-flagged, not unreadable-then-flagged.
func preservedReloginReason(preserved *keychainTokens, refreshTok string, err error) string {
	if preserved != nil &&
		preserved.reloginReason() == reloginReasonRefreshOutcomeUnknown &&
		preserved.RefreshToken == refreshTok &&
		errors.Is(err, ErrRefreshTokenAlreadyExchanged) {
		return reloginReasonRefreshOutcomeUnknown
	}
	return reloginReasonRefreshTokenRejected
}
