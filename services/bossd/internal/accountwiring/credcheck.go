package accountwiring

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/agenterr"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/safego"

	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/credmaterialize"
)

// --- Credential maintenance (BOS-1141) ------------------------------------
//
// CredentialMaintainer is the daemon-owned owner of live Codex credential
// verification. It exists so there is exactly ONE place that runs a live
// credential check, whatever triggered it:
//
//   - the boot sweep, shortly after daemon wiring completes,
//   - the jittered periodic sweep,
//   - an explicit TestAccount RPC (the server's AccountSmokeRunner hook).
//
// All three funnel through verify(), which is single-flight per account: a
// second trigger joins the live run instead of starting a parallel one.
//
// It never handles credential bytes itself. Materialization, PersistBack, and
// keyring access all stay inside SmokeRunner / credmaterialize / accountcred.
// What it persists is redacted metadata ONLY — a closed-set outcome token, a
// closed-set failure class, the check time, and the next retry instant. No
// provider log text, no environment, no blob, ever reaches the store or a log
// line here.

// Defaults for the maintenance schedule. They are deliberately conservative:
// a live check spawns a real agent process, so the cadence is hours, not
// minutes, and the boot sweep waits for the daemon to finish coming up.
const (
	defaultAuthCheckInterval    = 6 * time.Hour
	defaultAuthCheckStaleness   = 6 * time.Hour
	defaultAuthCheckBootDelay   = 30 * time.Second
	defaultAuthCheckBackoffBase = 5 * time.Minute
	defaultAuthCheckBackoffMax  = 4 * time.Hour
	defaultAuthCheckJitter      = 0.2
	defaultAuthCheckStopTimeout = 30 * time.Second
	authCheckWriteTimeout       = 5 * time.Second
)

// Redacted failure-class tokens. These are the ONLY failure strings that ever
// reach durable state: fixed identifiers chosen here, never provider text.
const (
	authFailureRunnerUnavailable = "runner_unavailable"
	authFailureAuthInvalidated   = "auth_invalidated"
	// authFailureSuspended is reserved for a confirmed provider suspension
	// signal on this path. classifyVerification does not currently emit it:
	// the only suspension contract in this package belongs to the claude usage
	// probe, not to the agent-runner smoke path (see classifyVerification).
	authFailureSuspended         = "suspended"
	authFailureRateLimited       = "rate_limited"
	authFailureUsageExhausted    = "usage_exhausted"
	authFailureTransientProvider = "transient_provider"
	authFailureUnclassified      = "unclassified"
	// authFailureCredentialSuperseded reports that the ambient codex login
	// carries the SAME provider account as this row but a DIFFERENT refresh
	// token: someone rotated the chain outside the daemon (a manual
	// `codex login`, most often) and the stored refresh token is no longer the
	// live one (BOS-1175).
	//
	// It is the one class that qualifies a HEALTHY outcome rather than a
	// failing one, and it deliberately does NOT change eligibility. The stored
	// access token still works — that is exactly why the account keeps
	// verifying clean — so this is a warning about the future, not a present
	// rejection, and the provider remains the only authority on the latter. The
	// remedy is `boss account reauth` (BOS-1142), which the operator can only
	// reach for if the state is visible, which is why both client mirrors
	// render it.
	authFailureCredentialSuperseded = "credential_superseded"
	// authFailureRefreshNotObserved qualifies AuthCheckOutcomeRefreshChainUnproven
	// (BOS-1174). It names what was NOT observed rather than what failed: the
	// verification succeeded, so there is no provider failure to classify — what
	// is missing is the credential rotation the access token's own expiry says
	// should already have happened.
	authFailureRefreshNotObserved = "refresh_not_observed"
)

// codexAuthRequiredMarkers are the runner-owned signal that a codex credential
// is expired or missing. The codex plugin surfaces a fixed typed sentinel
// (plugins/bossd-plugin-codex/auth.go: ErrAuthRequired, "codex auth required:
// run `codex login`"), which the runner reports verbatim as its ExitError and
// smoke.go wraps as "credential verification failed: <sentinel>".
//
// The string is DUPLICATED here rather than imported: a plugin binary and the
// host must not share internal packages (see CLAUDE.md, "Module boundaries"),
// so the coupling is a copied literal pinned by TestClassifyVerificationCodexAuthSentinel.
//
// Matching this is not the log-tail matching classifyVerification exists to
// avoid: smokeUnderlying has already stripped the agent-authored diagnostic,
// so this compares only against text the RUNNER itself produced.
var codexAuthRequiredMarkers = []string{
	"codex auth required",
	"run `codex login`",
}

// isRunnerAuthSentinel reports whether the runner's own error is a settled
// "this credential will not work until the user re-authenticates" signal.
func isRunnerAuthSentinel(msg string) bool {
	for _, marker := range codexAuthRequiredMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// runnerUnavailableMarkers are the runner-owned signal that the agent BINARY
// could not be executed at all: the login shell exited 127 or 126 without ever
// starting it (plugins/bossd-plugin-codex/unavailable.go: ErrRunnerUnavailable
// / ErrRunnerNotExecutable).
//
// Recognising the exit code has to happen in the plugin, because
// AgentRunnerService.ExitStatus carries only a string exit_error -- there is no
// numeric exit code on that RPC for the host to read. So the plugin reports a
// fixed sentinel and the host matches this literal, DUPLICATED here for exactly
// the same reason as codexAuthRequiredMarkers above: a plugin binary and the
// host must not share internal packages (CLAUDE.md, "Module boundaries"). The
// coupling is pinned from both sides -- the plugin's
// TestRunnerUnavailableSentinelMessages and this package's
// TestClassifyVerificationRunnerUnavailableSentinel.
//
// As with the auth markers, this is not the log-tail matching
// classifyVerification exists to avoid: smokeUnderlying has already stripped
// the agent-authored diagnostic, so it compares only against text the RUNNER
// itself produced.
var runnerUnavailableMarkers = []string{
	"could not run the codex binary",
}

// isRunnerUnavailableSentinel reports whether the runner's own error says the
// agent binary was never executed.
//
// This is the "we could not check" half of the invalid-versus-undetermined
// distinction: nothing reached the provider, so the result is not a verdict on
// the credential in either direction.
func isRunnerUnavailableSentinel(msg string) bool {
	for _, marker := range runnerUnavailableMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// errStaleAuthCheck is the internal sentinel for a result discarded by the
// credential-generation guard. It never leaves this package.
var errStaleAuthCheck = errors.New("credential verification superseded by a credential change")

// staleAuthCheckError reports a result the generation guard discarded WITHOUT
// losing the verification failure that was discarded. The durable write is
// skipped because we no longer know which credential the result describes.
//
// The caller of an explicit TestAccount RPC is never handed this outcome as a
// verdict: "we don't know" must never be reported as "it works", and it must
// not be reported as "it is broken" either, because the failure belongs to the
// credential that was replaced. Smoke retries such a result against what is
// actually stored and reports ErrVerificationInconclusive if that is
// superseded too. verifyErr is retained for diagnostics and for callers that
// need the discarded outcome, not as something to attribute to a replacement.
type staleAuthCheckError struct {
	verifyErr error
}

func (e *staleAuthCheckError) Error() string {
	if e.verifyErr == nil {
		return errStaleAuthCheck.Error()
	}
	return errStaleAuthCheck.Error() + ": " + e.verifyErr.Error()
}

// Is makes errors.Is(err, errStaleAuthCheck) hold for the wrapper.
func (e *staleAuthCheckError) Is(target error) bool { return target == errStaleAuthCheck }

// Unwrap exposes the discarded verification failure (nil when the discarded
// result was a clean one).
func (e *staleAuthCheckError) Unwrap() error { return e.verifyErr }

// ErrAuthCheckNotRecorded reports that a verification completed but its durable
// result could not be committed. It is exported because the outcome is only
// meaningful to an EXPLICIT caller (TestAccount, RefreshAccount with
// test_after_save): the periodic sweep can simply re-record on its next pass,
// but a caller who asked "does this credential work?" must not be told "yes"
// while the durable row still reads auth_invalid and the resolver keeps
// refusing the account.
var ErrAuthCheckNotRecorded = errors.New("credential verification result not recorded")

// authCheckNotRecordedError carries the durable-write failure without losing
// the verification outcome it failed to record. Both are preserved because
// they answer different questions: the store error says why eligibility state
// is now stale, and verifyErr (nil for a clean run) says what the credential
// actually did.
type authCheckNotRecordedError struct {
	storeErr  error
	verifyErr error
}

func (e *authCheckNotRecordedError) Error() string {
	msg := ErrAuthCheckNotRecorded.Error()
	if e.storeErr != nil {
		msg += ": " + e.storeErr.Error()
	}
	if e.verifyErr != nil {
		msg += " (verification: " + e.verifyErr.Error() + ")"
	}
	return msg
}

// Is makes errors.Is(err, ErrAuthCheckNotRecorded) hold for the wrapper.
func (e *authCheckNotRecordedError) Is(target error) bool {
	return target == ErrAuthCheckNotRecorded
}

// Unwrap exposes the store failure. The verification error is reachable via
// VerifyErr rather than Unwrap so that errors.Is against a provider error
// cannot make an unrecorded result look like a plain verification failure.
func (e *authCheckNotRecordedError) Unwrap() error { return e.storeErr }

// VerifyErr returns the verification outcome that went unrecorded (nil when
// the unrecorded run was clean).
func (e *authCheckNotRecordedError) VerifyErr() error { return e.verifyErr }

// ErrVerificationInconclusive reports that a live verification could not be
// attributed to the credential currently stored: the credential was replaced
// while the run was in flight, so the run's (clean) outcome describes bytes
// that are no longer there.
//
// It exists because nil is the one answer this case must never give. A clean
// result for a superseded credential, reported as success, stamps a passing
// live test on a replacement nothing has tested.
var ErrVerificationInconclusive = errors.New("credential verification inconclusive: credential changed during verification")

// CredentialVerifier runs one live credential verification. *SmokeRunner is
// the production implementation; tests substitute a fake.
type CredentialVerifier interface {
	Verify(ctx context.Context, accountID, provider string, blob []byte) (SmokeResult, error)
}

// AuthCheckStore is the narrow store slice the maintainer needs: enumerate
// accounts to find stale ones, and atomically record one redacted result.
// *db.SQLiteAccountStore satisfies it.
type AuthCheckStore interface {
	List(ctx context.Context) ([]*models.Account, error)
	// Get reads one account's current row. The maintainer needs the verdict
	// already recorded there before it overwrites it: an inconclusive result
	// must not clear a settled auth-invalid bench (preserveSettledOutcome).
	// It reports sql.ErrNoRows when the account does not exist.
	Get(ctx context.Context, id string) (*models.Account, error)
	RecordAuthCheck(ctx context.Context, id string, check models.AuthCheck) error
	// ClearAuthCheck withdraws a recorded result, leaving the row's auth-check
	// state unknown and immediately due. The maintainer needs it to roll back a
	// verdict it committed for a credential that was replaced mid-write.
	ClearAuthCheck(ctx context.Context, id string) error
}

// AuthCheckClock is the time seam. Production uses the wall clock; tests
// supply a controllable clock so schedule, jitter bounds, and backoff are
// asserted deterministically instead of slept through.
type AuthCheckClock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type wallAuthCheckClock struct{}

func (wallAuthCheckClock) Now() time.Time                         { return time.Now() }
func (wallAuthCheckClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// CredentialMaintainerOption configures a CredentialMaintainer.
type CredentialMaintainerOption func(*CredentialMaintainer)

// WithAuthCheckInterval sets the periodic sweep cadence (pre-jitter).
func WithAuthCheckInterval(d time.Duration) CredentialMaintainerOption {
	return func(m *CredentialMaintainer) {
		if d > 0 {
			m.interval = d
		}
	}
}

// WithAuthCheckStaleness sets how old a recorded check may be before the
// account is swept again.
func WithAuthCheckStaleness(d time.Duration) CredentialMaintainerOption {
	return func(m *CredentialMaintainer) {
		if d > 0 {
			m.staleness = d
		}
	}
}

// WithAuthCheckEnabled wires the live managed-accounts kill switch consulted
// before each scheduled sweep. Pass a loader that re-reads settings from disk,
// not a captured boot-time boolean; fail closed on a load error, matching
// Lifecycle.currentRotationConfig. It gates only the scheduled path — an
// operator-initiated TestAccount still runs through Smoke with rotation off.
func WithAuthCheckEnabled(fn func() bool) CredentialMaintainerOption {
	return func(m *CredentialMaintainer) {
		m.enabled = fn
	}
}

// WithAuthCheckBootDelay sets the delay before the stale-at-boot sweep.
func WithAuthCheckBootDelay(d time.Duration) CredentialMaintainerOption {
	return func(m *CredentialMaintainer) {
		if d >= 0 {
			m.bootDelay = d
		}
	}
}

// WithAuthCheckBackoff sets the exponential retry backoff base and ceiling
// used after a transient or unavailable verification.
func WithAuthCheckBackoff(base, maxWait time.Duration) CredentialMaintainerOption {
	return func(m *CredentialMaintainer) {
		if base > 0 {
			m.backoffBase = base
		}
		if maxWait > 0 {
			m.backoffMax = maxWait
		}
	}
}

// WithAuthCheckJitter sets the jitter fraction (0 disables jitter) and the
// [0,1) random source used to spread scheduled work.
func WithAuthCheckJitter(fraction float64, source func() float64) CredentialMaintainerOption {
	return func(m *CredentialMaintainer) {
		if fraction >= 0 && fraction < 1 {
			m.jitter = fraction
		}
		if source != nil {
			m.rand = source
		}
	}
}

// WithAuthCheckClock substitutes the time seam.
func WithAuthCheckClock(c AuthCheckClock) CredentialMaintainerOption {
	return func(m *CredentialMaintainer) {
		if c != nil {
			m.clock = c
		}
	}
}

type authFlight struct {
	done chan struct{}
	err  error
	// generation is the credential generation the result DESCRIBES — the
	// generation of the bytes that were actually verified, not whatever is
	// stored when a joiner reads it. A joiner compares the two to tell a result
	// about its own credential from one about a predecessor.
	//
	// It is needed because the flight stays joinable after runVerification's
	// last revalidation has released the credential lock: a replacement landing
	// in that window would otherwise have its TestAccount join the still-published
	// flight and receive the old credential's verdict.
	generation uint64
	// hasGeneration is false when the credential store exposes no generation
	// seam, in which case a joiner cannot validate and takes the result as-is.
	hasGeneration bool
}

// CredentialMaintainer schedules and de-duplicates live credential
// verification for managed Codex accounts, and records the redacted result.
type CredentialMaintainer struct {
	verifier CredentialVerifier
	store    AuthCheckStore
	creds    CredentialStore
	logger   zerolog.Logger
	clock    AuthCheckClock

	interval    time.Duration
	staleness   time.Duration
	bootDelay   time.Duration
	backoffBase time.Duration
	backoffMax  time.Duration
	stopTimeout time.Duration
	jitter      float64
	rand        func() float64
	// enabled re-reads the managed-accounts kill switch per sweep. It is the
	// scheduled path's only gate: the daemon starts this maintainer whenever it
	// exists, so a flip of `boss settings --no-managed-accounts` (a plain
	// settings.json write with no restart, services/boss/cmd/handlers.go) takes
	// effect on the next sweep in both directions. A boot-time snapshot could
	// not do that — it would keep spawning real Codex verification subprocesses
	// after the operator turned rotation off, and would never start maintenance
	// after they turned it on. Same live-reload idiom as ChatRotator.SweepProactive
	// and Lifecycle.currentRotationConfig. nil means unconditionally enabled.
	enabled func() bool

	mu       sync.Mutex
	flights  map[string]*authFlight
	failures map[string]int
	started  bool
	stopped  bool
	cancel   context.CancelFunc
	done     <-chan struct{}
}

// NewCredentialMaintainer builds the coordinator. verifier and store are
// required; creds is the credential store whose per-account lock and
// generation counter guard result commits (nil, or a store without those
// seams, simply skips the guard). A nil verifier or store yields a nil
// maintainer so callers can degrade the same way they do for a missing smoke
// runner.
func NewCredentialMaintainer(
	verifier CredentialVerifier,
	store AuthCheckStore,
	creds CredentialStore,
	logger zerolog.Logger,
	opts ...CredentialMaintainerOption,
) (*CredentialMaintainer, error) {
	if verifier == nil {
		return nil, fmt.Errorf("credential maintenance requires a verifier")
	}
	if store == nil {
		return nil, fmt.Errorf("credential maintenance requires an account store")
	}
	m := &CredentialMaintainer{
		verifier:    verifier,
		store:       store,
		creds:       creds,
		logger:      logger,
		clock:       wallAuthCheckClock{},
		interval:    defaultAuthCheckInterval,
		staleness:   defaultAuthCheckStaleness,
		bootDelay:   defaultAuthCheckBootDelay,
		backoffBase: defaultAuthCheckBackoffBase,
		backoffMax:  defaultAuthCheckBackoffMax,
		stopTimeout: defaultAuthCheckStopTimeout,
		jitter:      defaultAuthCheckJitter,
		rand:        rand.Float64,
		flights:     make(map[string]*authFlight),
		failures:    make(map[string]int),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m, nil
}

// Smoke satisfies the server's AccountSmokeRunner hook so an explicit
// TestAccount RPC shares this coordinator's single-flight and durable
// recording instead of opening a second, unguarded verification path.
func (m *CredentialMaintainer) Smoke(ctx context.Context, accountID, provider string, _ []byte) error {
	if m == nil {
		return fmt.Errorf("credential verification runner not configured")
	}
	err := m.verify(ctx, accountID, provider)
	var stale *staleAuthCheckError
	if !errors.As(err, &stale) {
		return err
	}
	// EITHER direction of a discarded result describes credential bytes that
	// are no longer stored, so neither may be attributed to the replacement.
	// Reporting a discarded pass would stamp last_test_ok_at and health=OK on
	// something nothing verified; reporting a discarded failure would bench a
	// replacement on its predecessor's failure, which for
	// RefreshAccount(test_after_save=true) means marking the credential it just
	// saved as failed without ever testing it.
	//
	// Retry once against the credential that is actually stored. The prior
	// flight is already retired by verify, so this starts a fresh run whose
	// generation baseline is read after the replacement landed.
	err = m.verify(ctx, accountID, provider)
	if !errors.As(err, &stale) {
		return err
	}
	// Superseded twice: the credential is being rewritten faster than it can be
	// verified. Report inconclusive rather than attributing either an obsolete
	// pass or an obsolete failure to whatever is stored now.
	return ErrVerificationInconclusive
}

// Start launches the maintenance loop: one stale-at-boot sweep after a short
// jittered delay, then a jittered periodic sweep. It is safe to call once;
// later calls are no-ops. Stop must be called to release the goroutine.
func (m *CredentialMaintainer) Start(ctx context.Context) {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.started || m.stopped {
		m.mu.Unlock()
		return
	}
	m.started = true
	runCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	// m.done is published under m.mu, BEFORE the loop goroutine exists: a Stop
	// racing Start must either see "not started" or see a done channel it can
	// wait on. Assigning it after unlocking both raced with Stop's read and let
	// a Stop landing in the window skip its bounded wait entirely. safego.Go
	// only launches the goroutine; it does not block on m.mu.
	m.done = safego.Go(m.logger, func() { m.loop(runCtx) })
	m.mu.Unlock()
}

// Stop cancels future scheduling and waits, bounded, for owned work to
// finish. The wait uses the real clock deliberately: it is a shutdown safety
// net, not simulated schedule time, so a test clock cannot wedge shutdown.
func (m *CredentialMaintainer) Stop() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	m.stopped = true
	cancel := m.cancel
	done := m.done
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(m.stopTimeout):
		m.logger.Warn().Dur("timeout", m.stopTimeout).
			Msg("account: credential maintenance did not stop in time; abandoning")
	}
}

func (m *CredentialMaintainer) loop(ctx context.Context) {
	wait := m.jittered(m.bootDelay)
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.clock.After(wait):
		}
		m.sweep(ctx)
		if ctx.Err() != nil {
			return
		}
		wait = m.jittered(m.interval)
	}
}

// sweep verifies every managed Codex account whose recorded state is due.
// Accounts are handled sequentially so a sweep never fans out provider calls.
func (m *CredentialMaintainer) sweep(ctx context.Context) {
	// Checked per sweep, not at startup: the kill switch is a live settings.json
	// value and the loop keeps running while it flips.
	if m.enabled != nil && !m.enabled() {
		return
	}
	accounts, err := m.store.List(ctx)
	if err != nil {
		m.logger.Warn().Err(err).Msg("account: credential maintenance could not list accounts")
		return
	}
	now := m.clock.Now()
	for _, a := range accounts {
		if ctx.Err() != nil {
			return
		}
		if a == nil || !m.due(a, now) {
			continue
		}
		if err := m.verify(ctx, a.ID, string(a.Provider)); err != nil {
			// Only the account id is logged. The verification error can carry a
			// redacted provider log tail, so it is deliberately NOT attached.
			m.logger.Debug().Str("account_id", a.ID).
				Msg("account: scheduled credential verification did not pass")
		}
	}
}

// due reports whether a scheduled sweep should verify this account now.
func (m *CredentialMaintainer) due(a *models.Account, now time.Time) bool {
	if a.Provider != models.AccountProviderCodex || a.Status != models.AccountStatusActive {
		return false
	}
	if next := a.AuthCheck.NextRetryAt; next != nil && now.Before(*next) {
		return false
	}
	checked := a.AuthCheck.CheckedAt
	if checked == nil {
		return true
	}
	// A check stamped in the future (clock skew, restored data) is not
	// trustworthy as "recent"; treat it as stale rather than never re-checking.
	if checked.After(now) {
		return true
	}
	return now.Sub(*checked) >= m.staleness
}

// verify is the single-flight entry point. A caller arriving while a
// verification for the same account is live joins it and receives that
// result rather than starting a second live run.
func (m *CredentialMaintainer) verify(ctx context.Context, accountID, provider string) error {
	m.mu.Lock()
	if existing := m.flights[accountID]; existing != nil {
		m.mu.Unlock()
		select {
		case <-existing.done:
			return m.joinedResult(accountID, existing)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	flight := &authFlight{done: make(chan struct{})}
	m.flights[accountID] = flight
	m.mu.Unlock()

	flight.err = m.runVerification(ctx, accountID, provider, flight)

	m.mu.Lock()
	delete(m.flights, accountID)
	m.mu.Unlock()
	close(flight.done)

	return flight.err
}

// joinedResult attributes a shared flight's result to the credential the
// JOINER cares about. The flight was computed for a specific generation, and it
// stays joinable after that generation was last checked, so a replacement can
// land between the run's final revalidation and a joiner reading the result.
//
// Without this, RefreshAccount(test_after_save=true) could join a flight for
// the credential it just replaced and be handed that verdict: a clean old run
// would mark an unverified replacement tested and restore its health, and a
// failed old run would bench a replacement that may be fine.
func (m *CredentialMaintainer) joinedResult(accountID string, flight *authFlight) error {
	err := flight.err
	// Already known superseded — Smoke retries this against what is stored now,
	// so re-wrapping it would say nothing new.
	var stale *staleAuthCheckError
	if errors.As(err, &stale) {
		return err
	}
	if !flight.hasGeneration {
		return err
	}
	if m.credentialGenerationMoved(accountID, flight.generation) {
		return &staleAuthCheckError{verifyErr: err}
	}
	return err
}

// credentialGenerationMoved reports whether the stored credential differs from
// the generation a result was computed for. An unreadable lock answers "moved":
// a result that cannot be attributed must not be accepted as though it were
// about the current credential.
func (m *CredentialMaintainer) credentialGenerationMoved(accountID string, expected uint64) bool {
	locker, hasLock := m.creds.(credentialStoreLocker)
	generations, hasGeneration := m.creds.(credentialGenerationStore)
	if !hasLock || !hasGeneration {
		return false
	}
	moved := false
	if err := locker.WithCredentialLock(accountID, func() error {
		moved = generations.CredentialGeneration(accountID) != expected
		return nil
	}); err != nil {
		m.logger.Warn().Err(err).Str("account_id", accountID).
			Msg("account: could not attribute a joined verification result; treating it as superseded")
		return true
	}
	return moved
}

// runVerification performs the provider I/O OUTSIDE the credential lock, then
// re-reads the credential generation under the lock and discards the result if
// it moved underneath the run. The durable write happens after the lock is
// released; only the generation compare is inside the critical section.
func (m *CredentialMaintainer) runVerification(ctx context.Context, accountID, provider string, flight *authFlight) error {
	locker, hasLock := m.creds.(credentialStoreLocker)
	generations, hasGeneration := m.creds.(credentialGenerationStore)
	guarded := hasLock && hasGeneration

	var before uint64
	if guarded {
		if err := locker.WithCredentialLock(accountID, func() error {
			before = generations.CredentialGeneration(accountID)
			return nil
		}); err != nil {
			return err
		}
	}

	// Provider I/O. Unlocked on purpose: it spawns a real agent process, and
	// SmokeRunner's own materialize/persist path takes the same per-account
	// lock internally.
	res, verifyErr := m.verifier.Verify(ctx, accountID, provider, nil)

	// A cancelled run (daemon shutdown) says nothing about the credential.
	// Report it, record nothing.
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// The credential lock covers the generation read/compare ONLY. The durable
	// write below is up to authCheckWriteTimeout of SQLite I/O and the same
	// per-account lock is taken by materialization on every spawn, so holding
	// it across the write would block spawns for the duration of a slow write.
	stale := false
	if guarded {
		if err := locker.WithCredentialLock(accountID, func() error {
			after := generations.CredentialGeneration(accountID)
			// A run may write the credential itself: MaterializeCodex folds a
			// refreshed auth.json back into the store, and PersistBack can save
			// again afterwards. SmokeResult reports how many such writes
			// actually committed, counted at the store seam, so exactly those
			// bumps are expected here. Any other movement means a concurrent
			// writer replaced the credential and this result describes bytes
			// that are no longer stored.
			//
			// The count is authoritative precisely because it measures
			// mutations rather than intent: a persist closure that no-ops under
			// credmaterialize's unchanged gate contributes nothing, so this run
			// can no longer absorb someone else's replacement.
			stale = after != before+res.SelfCredentialWrites
			return nil
		}); err != nil {
			m.logger.Warn().Err(err).Str("account_id", accountID).
				Msg("account: could not check credential generation; verification result not recorded")
			// Same class as a failed durable write below: classify and the
			// write are both skipped, so eligibility state still describes an
			// older run. An explicit caller must not read that as a pass.
			return &authCheckNotRecordedError{storeErr: err, verifyErr: verifyErr}
		}
	}
	if stale {
		m.logger.Debug().Str("account_id", accountID).
			Msg("account: discarded credential verification result; credential changed mid-check")
		// The result is not recorded, but the failure is not invented away.
		return &staleAuthCheckError{verifyErr: verifyErr}
	}

	// classify advances the retry backoff, so it runs only once the result is
	// known to be committable: a discarded result must not consume an
	// escalation step it never recorded.
	check := m.classify(accountID, verifyErr, res, m.clock.Now())

	writeCtx, cancelWrite := context.WithTimeout(context.WithoutCancel(ctx), authCheckWriteTimeout)
	defer cancelWrite()
	check, err := m.preserveSettledOutcome(writeCtx, accountID, check)
	if err != nil {
		m.logger.Warn().Err(err).Str("account_id", accountID).
			Msg("account: could not read recorded verification state; result not recorded")
		// Same class as the failed durable write below: nothing was committed,
		// so eligibility state still describes an older run and an explicit
		// caller must not read this as a pass.
		return &authCheckNotRecordedError{storeErr: err, verifyErr: verifyErr}
	}
	if err := m.store.RecordAuthCheck(writeCtx, accountID, check); err != nil {
		// Only the account id and the store error are logged; the verification
		// error can carry a redacted provider log tail and never reaches a log.
		m.logger.Warn().Err(err).Str("account_id", accountID).
			Msg("account: could not record credential verification result")
		// The verification happened, but durable eligibility state did not
		// move. Returning verifyErr alone would report a clean run as an
		// unqualified pass while the row may still read auth_invalid, leaving
		// the resolver refusing an account the caller was just told is fine.
		// The sweep caller discards this (it re-records next pass); the
		// explicit callers map it to an infrastructure failure.
		return &authCheckNotRecordedError{storeErr: err, verifyErr: verifyErr}
	}
	// The credential lock was released before the write above, so a
	// replacement can land between the generation compare and the commit.
	// Nothing so far notices: the verdict just written describes bytes that are
	// no longer stored, and because RefreshAccount clears this row as part of
	// replacing a credential, the write can silently restore state the clear
	// had withdrawn — re-benching a valid replacement, or worse, marking an
	// unverified one healthy and suppressing re-verification for a full
	// interval.
	if guarded {
		// The generation the result DESCRIBES: the baseline plus the writes
		// this run itself performed. Recorded before the flight is published so
		// a joiner can tell whether the credential has moved since.
		flight.generation = before + res.SelfCredentialWrites
		flight.hasGeneration = true
		withdrawn, err := m.withdrawIfCredentialChanged(ctx, accountID, locker, generations, before+res.SelfCredentialWrites)
		switch {
		case err != nil:
			// Either we could not revalidate, or we could not withdraw. Both
			// leave durable eligibility state that is not known to describe
			// the stored credential, and unlike the ordinary path there is no
			// later sweep guarantee that it is about the right bytes.
			return &authCheckNotRecordedError{storeErr: err, verifyErr: verifyErr}
		case withdrawn:
			// The verdict was real, but about a credential that no longer
			// exists. Returning verifyErr here would hand a joined
			// RefreshAccount the OLD credential's outcome — reporting a clean
			// old run as a pass for the replacement, or benching a valid
			// replacement on an old failure. Report it as superseded so Smoke
			// retries against what is actually stored.
			return &staleAuthCheckError{verifyErr: verifyErr}
		}
	}
	return verifyErr
}

// inconclusiveOutcome reports whether an outcome says nothing about the stored
// credential. transient and unavailable both mean "we could not tell": the
// provider was throttling, the runner was not loaded, the failure text was
// unrecognized. Only healthy, auth_invalid and refresh_chain_unproven are
// answers.
//
// refresh_chain_unproven is deliberately on the ANSWER side (BOS-1174). It is
// only ever produced by a run that completed cleanly, so it carries everything
// a healthy verdict carries — the provider answered, the credential works —
// plus one extra observation about the refresh chain. Treating it as
// inconclusive would preserve a settled auth_invalid bench across a run that
// actually proved the credential usable, which is the opposite of what the
// evidence says.
func inconclusiveOutcome(o models.AuthCheckOutcome) bool {
	return !settledOutcome(o)
}

// settledOutcome reports whether an outcome is an ANSWER about the stored
// credential rather than a failure to reach one. It is the single closed
// switch both classify and inconclusiveOutcome read, so a new outcome value
// cannot be added to one and forgotten in the other — the failure mode
// BOS-1174 had to design around, because a value in neither of classify's arms
// was silently dropped and broke backoff with no other symptom.
//
// The default is UNSETTLED, matching the package's standing bias: an outcome
// this build cannot classify must not be allowed to clear a bench or to skip
// the retry escalation.
func settledOutcome(o models.AuthCheckOutcome) bool {
	switch o {
	case models.AuthCheckOutcomeHealthy,
		models.AuthCheckOutcomeAuthInvalid,
		models.AuthCheckOutcomeRefreshChainUnproven:
		return true
	default:
		return false
	}
}

// preserveSettledOutcome keeps a confirmed auth-invalid verdict in place when
// the verification that just ran was inconclusive.
//
// This is load-bearing because durable eligibility is LATEST-OUTCOME-ONLY.
// RecordAuthCheck overwrites auth_check_outcome unconditionally (there is no
// sticky column, trigger, or status flag behind it — recording a verdict
// deliberately leaves Health and last_test_* alone), and every predicate reads
// only that one value: models.Account.IsAuthInvalid, rotation.isSelectable,
// the resolver's pre-materialization refusal, and the bind-time eligibility
// check. So a transient/unavailable write over an auth_invalid row does not
// merely lose information — it silently returns a credential the provider
// already rejected to BOTH selection tiers, with no re-authentication and no
// passing verification in between. A missing Codex runner or one throttled
// response would be enough to un-bench every benched account.
//
// The documented rule is the opposite one: "A later healthy verification
// clears it" (account.AccountMeta.AuthInvalid), which is also why classify keeps
// an auth-invalid account on the ordinary cadence instead of backing it off —
// it is re-checked so a re-authentication can clear it on its own. Preserving
// here is what makes that sentence true: the bench survives every answer that
// is not an answer, and only a healthy run (or the explicit ClearAuthCheck
// that RefreshAccount performs when the credential is REPLACED) lifts it.
//
// The read is a TOCTOU only in appearance. The maintainer is single-flight per
// account, so no second verification can interleave a fresh verdict; and if an
// operator re-authenticates in the window, the credential generation moves and
// withdrawIfCredentialChanged rolls this write back to unknown, which is
// exactly the case that must not inherit the old bench.
//
// A read failure is fail-closed: the caller records nothing rather than risk
// clearing a bench it could not confirm. sql.ErrNoRows is not that case — the
// row is gone, so there is no verdict to preserve and the write below reports
// the missing row itself.
func (m *CredentialMaintainer) preserveSettledOutcome(
	ctx context.Context,
	accountID string,
	check models.AuthCheck,
) (models.AuthCheck, error) {
	if !inconclusiveOutcome(check.Outcome) {
		return check, nil
	}
	prior, err := m.store.Get(ctx, accountID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return check, nil
	case err != nil:
		return check, err
	case prior == nil || !prior.IsAuthInvalid():
		return check, nil
	}
	// Keep the settled verdict and its failure class; take the fresh check time
	// and the escalated retry instant from the run that just happened. The
	// account stays benched, and it stays scheduled for the re-check that can
	// clear it.
	m.logger.Debug().Str("account_id", accountID).Str("outcome", string(check.Outcome)).
		Msg("account: inconclusive verification did not clear the recorded auth-invalid verdict")
	check.Outcome = prior.AuthCheck.Outcome
	check.FailureClass = prior.AuthCheck.FailureClass
	return check, nil
}

// withdrawIfCredentialChanged rolls the durable auth-check row back to unknown
// when the credential moved while the result was being written.
//
// Rolling back to unknown rather than to the previous verdict is deliberate:
// NULL auth_checked_at is immediately due, so the replacement is verified on
// the next tick instead of inheriting a verdict earned by different bytes.
//
// This is a compensating write rather than a wider lock. Holding the credential
// lock across the durable write would serialize it with materialization, which
// takes the same per-account lock on every spawn, so a slow SQLite write would
// stall spawns for up to authCheckWriteTimeout. It is safe without the lock
// because the maintainer is single-flight per account: no second verification
// can interleave a fresh verdict between the write and this withdrawal.
//
// It returns whether the verdict was withdrawn, and any error that left the
// question unsettled, because the CALLER's return value depends on it: a
// withdrawn verdict must not be reported to a joined explicit caller as though
// it described the credential that is stored now.
func (m *CredentialMaintainer) withdrawIfCredentialChanged(
	ctx context.Context,
	accountID string,
	locker credentialStoreLocker,
	generations credentialGenerationStore,
	expected uint64,
) (bool, error) {
	changed := false
	if err := locker.WithCredentialLock(accountID, func() error {
		changed = generations.CredentialGeneration(accountID) != expected
		return nil
	}); err != nil {
		m.logger.Warn().Err(err).Str("account_id", accountID).
			Msg("account: could not revalidate credential generation after recording verification result")
		return false, err
	}
	if !changed {
		return false, nil
	}
	clearCtx, cancelClear := context.WithTimeout(context.WithoutCancel(ctx), authCheckWriteTimeout)
	defer cancelClear()
	if err := m.store.ClearAuthCheck(clearCtx, accountID); err != nil {
		m.logger.Warn().Err(err).Str("account_id", accountID).
			Msg("account: could not withdraw verification result recorded for a replaced credential")
		return true, err
	}
	m.logger.Debug().Str("account_id", accountID).
		Msg("account: withdrew verification result; credential was replaced during the durable write")
	return true, nil
}

// classify turns a verification error into the durable, redacted record and
// advances (or clears) the retry backoff for the account.
//
// Unrecognized failures are TRANSIENT by construction: provider text and
// transport codes drift, and the cost of a wrong "auth invalid" (benching a
// working account) is far higher than the cost of a wrong "transient"
// (re-checking later).
func (m *CredentialMaintainer) classify(accountID string, verifyErr error, res SmokeResult, now time.Time) models.AuthCheck {
	outcome, failureClass := classifyVerification(verifyErr, res, now)
	failureClass = withSupersededClass(outcome, failureClass, res.AmbientAuth)
	check := models.AuthCheck{
		CheckedAt:    &now,
		Outcome:      outcome,
		FailureClass: failureClass,
	}

	switch {
	case settledOutcome(outcome):
		// A settled answer: clear the retry escalation and fall back to the
		// ordinary periodic cadence. An auth-invalid account keeps being
		// re-checked so a re-authentication clears the state on its own, and a
		// refresh-chain-unproven one keeps being re-checked because the very
		// next run may observe the refresh that clears it.
		m.resetFailures(accountID)
	case outcome == models.AuthCheckOutcomeUnavailable:
		// Verification could not be performed, so nothing was learned about
		// this credential. Advancing the failure level here let an
		// unresolvable PATH fault inflate a count that a later genuine
		// transient failure then inherited, on an account that stayed
		// perfectly valid (BOS-1172).
		//
		// The level itself is NOT durable: m.failures is an in-memory
		// map[string]int on the maintainer, never persisted and never logged,
		// so it dies with the process. What it feeds is -- backoff() turns it
		// into NextRetryAt, which account_store writes to the accounts table
		// and convert.go surfaces on the API as next_retry_at. The harm is
		// therefore an inflated NextRetryAt recorded for a failure that never
		// happened. Under production defaults that rarely changes when the
		// next sweep actually fires, because due() gates on m.staleness as
		// well as NextRetryAt and the 6h default staleness dominates the 4h
		// backoffMax -- but the durable value operators read is still wrong,
		// and a tuned-down staleness would make it bind.
		//
		// The level is left exactly where it was, neither advanced nor
		// cleared. Clearing it would let an unrelated unavailability forgive
		// an escalation a genuine transient failure earned, which is the same
		// error in the opposite direction: "we learned nothing" cuts both ways.
		//
		// currentFailure returns 0 when this is the first recorded failure;
		// backoff clamps attempt < 1 to 1, so that is the base delay and not
		// a zero wait.
		wait := m.backoff(m.currentFailure(accountID))
		// No ProbeRetryAfter here, deliberately: it reports ok only when the
		// error carries a gRPC ResourceExhausted status (IsProbeThrottled),
		// and neither Unavailable source can. The runner-not-loaded arm
		// returns on agent.ErrAgentRunnerNotLoaded, a host-side errors.New
		// sentinel that carries no gRPC status at all; the runner-unavailable
		// sentinel arm is reached only after IsProbeThrottled has already
		// routed throttles to Transient. Either way the call would report
		// (0, false).
		//
		// Honouring a provider hint would also read backwards for an outcome
		// whose premise is that nothing reached the provider.
		next := now.Add(wait)
		check.NextRetryAt = &next
	default:
		// Unknown and Transient — and, deliberately, any outcome a future change
		// adds without touching this switch. The enumerated form this replaces
		// dropped an unlisted value silently, which cost it its backoff with no
		// other symptom; settledOutcome above and this default between them leave
		// no value unhandled.
		attempt := m.nextFailure(accountID)
		wait := m.backoff(attempt)
		// Honour an explicit provider RetryInfo when it asks for longer.
		if retryAfter, ok := ProbeRetryAfter(verifyErr); ok && retryAfter > wait {
			wait = retryAfter
		}
		next := now.Add(wait)
		check.NextRetryAt = &next
	}
	return check
}

// smokeUnderlying strips the operator-facing smoke diagnostic wrapper so
// classification sees only the failure the runner reported, never the
// agent-authored log tail appended for humans (smoke.go
// appendSmokeDiagnostic). The tail is arbitrary agent prose: a task that
// merely printed "please sign in again" would otherwise be enough to mark a
// working credential auth-invalid, since agenterr gives AUTH top precedence.
func smokeUnderlying(err error) error {
	for err != nil {
		var diag *smokeDiagnosticError
		if !errors.As(err, &diag) {
			return err
		}
		err = diag.Unwrap()
	}
	return nil
}

// classifyVerification maps a verification error onto the closed outcome and
// failure-class sets.
//
// auth-invalid is the single outcome that removes an account from BOTH
// selection tiers, so it is only ever produced from a signal the runner itself
// reported: the agent taxonomy applied to the runner's own error (its start
// error, its status error, or the exit error it reported), with the
// human-facing smoke diagnostic stripped first. Everything unrecognized falls
// through to transient.
func classifyVerification(err error, res SmokeResult, now time.Time) (models.AuthCheckOutcome, string) {
	if err == nil {
		return classifyCleanVerification(res)
	}
	// No runner loaded: verification could not happen at all, so it says
	// nothing about the credential.
	if errors.Is(err, agent.ErrAgentRunnerNotLoaded) {
		return models.AuthCheckOutcomeUnavailable, authFailureRunnerUnavailable
	}
	if IsProbeThrottled(err) {
		return models.AuthCheckOutcomeTransient, authFailureRateLimited
	}
	// NOTE: codes.PermissionDenied is deliberately NOT read as a settled auth
	// failure here. That contract (IsSuspension / MarkSuspendedIfConfirmed) is
	// the claude plugin's usage-probe endpoint contract; the errors reaching
	// this function come from the agent-runner smoke path, where no plugin
	// reserves that code for a suspension. A transport-level PermissionDenied
	// from StartRun/ExitStatus therefore stays transient.

	signal := smokeUnderlying(err)
	if signal == nil {
		return models.AuthCheckOutcomeTransient, authFailureUnclassified
	}

	// The runner could not execute the agent binary at all, so nothing ever
	// reached the provider. This is the same "verification could not be
	// performed" case as a missing runner above, one layer down: the credential
	// is untested, not rejected, and the cause will never clear itself without
	// operator action -- so it must not be recorded as transient (BOS-1172).
	//
	// Checked ahead of the auth sentinel because a binary that never ran cannot
	// have been refused by a provider; the two marker sets are disjoint, so
	// neither shadows the other for any message a runner actually emits.
	if isRunnerUnavailableSentinel(signal.Error()) {
		return models.AuthCheckOutcomeUnavailable, authFailureRunnerUnavailable
	}

	// The runner's own auth sentinel is the strongest signal available: the
	// plugin emits it only after the provider refused the stored credential,
	// so it is a settled auth failure and not an inference from prose.
	// agenterr's patterns are written for claude's wording and do not cover it.
	if isRunnerAuthSentinel(signal.Error()) {
		return models.AuthCheckOutcomeAuthInvalid, authFailureAuthInvalidated
	}

	switch agenterr.Classify(signal.Error(), now).Kind {
	case agenterr.KindAuthInvalidated:
		return models.AuthCheckOutcomeAuthInvalid, authFailureAuthInvalidated
	case agenterr.KindRateLimited:
		return models.AuthCheckOutcomeTransient, authFailureRateLimited
	case agenterr.KindUsageExhausted:
		return models.AuthCheckOutcomeTransient, authFailureUsageExhausted
	case agenterr.KindTransientProvider:
		return models.AuthCheckOutcomeTransient, authFailureTransientProvider
	case agenterr.KindNone:
		return models.AuthCheckOutcomeTransient, authFailureUnclassified
	default:
		return models.AuthCheckOutcomeTransient, authFailureUnclassified
	}
}

// withSupersededClass qualifies a HEALTHY verdict with the superseded class
// when the ambient codex login has replaced the stored refresh chain.
//
// The outcome is left alone on purpose. auth_invalid is the single outcome that
// removes an account from both selection tiers, and a superseded refresh token
// is not a present rejection: the provider just accepted the credential. Writing
// anything but healthy here would bench a working account on an inference this
// package is not entitled to make, which is the inverse of the bug the class
// exists to surface.
//
// It only qualifies a verdict that has nothing else to say. A run that FAILED
// already carries the provider's own class, and that class is strictly more
// actionable — "the provider refused this credential" tells an operator to
// reauthenticate now, where "the refresh chain moved" tells them to do it
// eventually. The provider's answer must never be overwritten by ours.
//
// Every non-superseded ambient state — in sync, and every not-evaluable case
// including an ambient login belonging to a DIFFERENT account — contributes no
// class at all, so the ordinary two-account setup produces no signal rather than
// a benign-looking one.
func withSupersededClass(
	outcome models.AuthCheckOutcome,
	failureClass string,
	ambient credmaterialize.AmbientAuthState,
) string {
	if outcome != models.AuthCheckOutcomeHealthy || failureClass != "" {
		return failureClass
	}
	if ambient != credmaterialize.AmbientAuthSuperseded {
		return failureClass
	}
	return authFailureCredentialSuperseded
}

// classifyCleanVerification refines a verification that raised no error at all.
//
// The clean run stays the baseline and the expiry assertion can only REFINE it
// — it never substitutes for it. Every error path above is untouched, because a
// classification that read expiry instead of the provider's answer would just
// rebuild the same misdiagnosis pointed the other way.
//
// The refinement fires on a conjunction of three independent facts, and each
// one is load-bearing:
//
//   - the run was clean, so the credential demonstrably still works;
//   - the credential's own access token says a refresh should ALREADY have
//     happened (credmaterialize decides this; an expiry it could not read is
//     Unknown and lands here as ordinary health, never as an accusation); and
//   - this run observed no credential write, so the refresh did not happen
//     during it either.
//
// SelfCredentialWrites alone cannot carry this: a zero count means either "no
// refresh was needed" or "one was attempted and never persisted", which is
// ambiguous in both directions. The expiry assertion is precisely what
// separates them, which is why neither signal is consulted without the other.
//
// The conjunction is a REPORT, not a proof, and it has a known false-positive
// window: a healthy client that refreshes later than credmaterialize's
// refreshDueFraction is reported as unproven for the stretch in between, and
// the smoke prompt this check runs is not itself an event that obliges a client
// to rotate, so a zero write count there is expected rather than suspicious.
// That is why the outcome is non-condemning — IsAuthInvalid stays false and the
// account stays selectable — and why closing the window would need a streak of
// observations across checks, i.e. persisted refresh history the AuthCheck
// record does not carry. See the refreshDueFraction doc comment in
// credmaterialize for the measurement and the tuning consequences.
//
// PRECEDENCE against the BOS-1175 superseded class. Both signals say "the
// refresh chain is suspect", and they can hold at once. The superseded class
// wins: it is a DIRECT observation that the stored refresh token is not the
// live one, while this outcome is an INFERENCE from the access token's expiry,
// and it carries the more urgent remedy (`boss account reauth` now, rather
// than eventually). So a superseded run returns ordinary health here and lets
// withSupersededClass qualify it, instead of reporting the weaker inference and
// losing the stronger observation.
func classifyCleanVerification(res SmokeResult) (models.AuthCheckOutcome, string) {
	if res.AmbientAuth == credmaterialize.AmbientAuthSuperseded {
		return models.AuthCheckOutcomeHealthy, ""
	}
	if res.RefreshAssertion == credmaterialize.RefreshAssertionOverdue && res.SelfCredentialWrites == 0 {
		return models.AuthCheckOutcomeRefreshChainUnproven, authFailureRefreshNotObserved
	}
	return models.AuthCheckOutcomeHealthy, ""
}

func (m *CredentialMaintainer) resetFailures(accountID string) {
	m.mu.Lock()
	delete(m.failures, accountID)
	m.mu.Unlock()
}

// currentFailure reports the account's consecutive-failure count WITHOUT
// advancing it, for outcomes that must schedule a retry without treating the
// result as evidence about the credential.
func (m *CredentialMaintainer) currentFailure(accountID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.failures[accountID]
}

func (m *CredentialMaintainer) nextFailure(accountID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failures[accountID]++
	return m.failures[accountID]
}

// backoff returns the jittered wait for the nth consecutive failed
// verification: base, 2*base, 4*base … capped at backoffMax.
func (m *CredentialMaintainer) backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	wait := m.backoffBase
	for i := 1; i < attempt; i++ {
		if wait >= m.backoffMax {
			break
		}
		wait *= 2
	}
	if wait > m.backoffMax {
		wait = m.backoffMax
	}
	return m.jittered(wait)
}

// jittered spreads d by ±m.jitter so restarts and multi-account sweeps do not
// synchronise into a thundering herd against the provider.
func (m *CredentialMaintainer) jittered(d time.Duration) time.Duration {
	if d <= 0 || m.jitter <= 0 || m.rand == nil {
		return d
	}
	factor := 1 + m.jitter*(2*m.rand()-1)
	out := time.Duration(float64(d) * factor)
	if out < 0 {
		return 0
	}
	return out
}
