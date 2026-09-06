package accountwiring

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/agenterr"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/credmaterialize"
)

const (
	defaultSmokeTimeout      = 2 * time.Minute
	defaultSmokePollInterval = 500 * time.Millisecond
	smokeDiagnosticTailBytes = 8 * 1024
	smokeDiagnosticMaxLines  = 6
	smokeDiagnosticMaxChars  = 1200
	smokePrompt              = "Reply with exactly: OK"
	// ambientAuthCompareTimeout bounds the ambient-login comparison once its
	// context has had cancellation stripped, so a stripped context cannot leave
	// the diagnostic waiting indefinitely on a context-aware seam.
	//
	// It does NOT bound every seam, and the difference matters. The store read
	// underneath is contextCredentialStore.LoadCredential (adapters.go), which
	// checks ctx.Err() ONCE on entry and then calls accountcred.Store.Load ->
	// ring.Get — a synchronous, context-free OS keyring call. A deadline cannot
	// interrupt that call once it has been entered; bounding it for real would
	// mean parking a goroutine on the keyring and abandoning it, which trades a
	// hang for a leak. What this deadline does deliver is that the comparison
	// cannot wait on the cancellation-stripped context itself, and that expiry
	// lands on not-evaluable, which is already the honest answer.
	ambientAuthCompareTimeout = 5 * time.Second

	// providerCodex names the one provider that has an ambient CLI login to
	// compare a stored credential against. The literal is duplicated from
	// credmaterialize (where it is unexported) rather than exported across the
	// package boundary for one string.
	providerCodex = "codex"
)

// SmokeCredentialStore is the credential store surface needed to materialize
// account credentials for provider verification and persist Codex refreshes back.
type SmokeCredentialStore interface {
	Load(accountID string) ([]byte, error)
	Save(accountID string, blob []byte) error
}

// SmokeRunnerOption configures a SmokeRunner.
type SmokeRunnerOption func(*smokeRunnerConfig)

type smokeRunnerConfig struct {
	baseDir      string
	timeout      time.Duration
	pollInterval time.Duration
}

// WithSmokeBaseDir overrides the credential-materialization base dir.
// Production leaves it unset; tests use a temp dir.
func WithSmokeBaseDir(dir string) SmokeRunnerOption {
	return func(c *smokeRunnerConfig) { c.baseDir = dir }
}

// WithSmokeTimeout overrides the provider verification timeout.
func WithSmokeTimeout(d time.Duration) SmokeRunnerOption {
	return func(c *smokeRunnerConfig) { c.timeout = d }
}

// WithSmokePollInterval overrides the ExitStatus polling interval.
func WithSmokePollInterval(d time.Duration) SmokeRunnerOption {
	return func(c *smokeRunnerConfig) { c.pollInterval = d }
}

// SmokeRunner runs a short live provider invocation using a registered account's
// materialized credentials.
type SmokeRunner struct {
	clients      map[string]agent.AgentRunnerClient
	materializer *credmaterialize.Materializer
	creds        credentialStoreAdapter
	saves        *credentialSaveCounter
	timeout      time.Duration
	pollInterval time.Duration
	logger       zerolog.Logger
}

// NewSmokeRunner builds an account provider verification runner. It materializes credentials
// using the same on-disk executor that spawn paths use for Codex CODEX_HOME.
func NewSmokeRunner(
	clients map[string]agent.AgentRunnerClient,
	store SmokeCredentialStore,
	logger zerolog.Logger,
	opts ...SmokeRunnerOption,
) (*SmokeRunner, error) {
	cfg := smokeRunnerConfig{timeout: defaultSmokeTimeout, pollInterval: defaultSmokePollInterval}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.timeout <= 0 {
		cfg.timeout = defaultSmokeTimeout
	}
	if cfg.pollInterval <= 0 {
		cfg.pollInterval = defaultSmokePollInterval
	}

	cmOpts := []credmaterialize.Option{}
	if cfg.baseDir != "" {
		cmOpts = append(cmOpts, credmaterialize.WithBaseDir(cfg.baseDir))
	}
	saves := newCredentialSaveCounter()
	creds := credentialStoreAdapter{store: store, saves: saves}
	m, err := credmaterialize.New(creds, logger, cmOpts...)
	if err != nil {
		return nil, err
	}
	return &SmokeRunner{
		clients:      clients,
		materializer: m,
		creds:        creds,
		saves:        saves,
		timeout:      cfg.timeout,
		pollInterval: cfg.pollInterval,
		logger:       logger,
	}, nil
}

// SmokeResult reports what a verification run did besides succeed or fail.
// It carries NO credential material and no provider text — only a count of the
// credential writes the run itself performed. The credential-maintenance
// coordinator (credcheck.go) uses that count to tell generation bumps this very
// run caused from ones a concurrent writer caused.
type SmokeResult struct {
	// SelfCredentialWrites is how many times this run actually mutated the
	// credential store, counted at the store seam on success only.
	//
	// It deliberately counts writes rather than reporting that a persist
	// closure was invoked. Invocation is not mutation: PersistBack no-ops
	// under credmaterialize's unchanged gate when the agent did not rewrite
	// auth.json, so treating "closure ran" as "one bump is mine" lets a run
	// absorb a concurrent external replacement and record a result about
	// credential bytes that are no longer stored.
	//
	// It is a count and not a bool because a single run can legitimately write
	// more than once: MaterializeCodex folds a refreshed auth.json back into
	// the store during materialization, and PersistBack may save again
	// afterwards.
	SelfCredentialWrites uint64

	// AmbientAuth reports how the ambient codex login compares to the
	// credential this account has stored: superseded, in sync, or not
	// evaluable (BOS-1175).
	//
	// It is a closed-set enum and nothing else. NO TOKEN BYTES AND NO
	// account_id reach this struct: the comparison happens inside
	// credmaterialize, where both values already are, and only its verdict
	// comes back. That is what makes the field safe to carry into the durable
	// auth-check row and into a log line.
	//
	// A non-codex provider, and every path that returns before the comparison
	// runs, leaves the zero value — credmaterialize.AmbientAuthNotEvaluable,
	// which is the honest answer for "this was never evaluated".
	AmbientAuth credmaterialize.AmbientAuthState

	// RefreshAssertion is the redacted BOS-1174 verdict credmaterialize reached
	// about the credential this run materialized: whether the credential's own
	// access token says a token refresh should already have happened.
	//
	// It exists because SelfCredentialWrites cannot answer the question on its
	// own. A zero count means EITHER no refresh was needed OR one was attempted
	// and never persisted — ambiguous in both directions — and the difference
	// between them is the difference between a healthy quiet account and one
	// living on a still-valid access token above a dead refresh chain.
	//
	// CREDENTIAL SAFETY: this is a classification and nothing else. No claim
	// value, no expiry timestamp and no token byte is carried here, so the
	// whole struct remains what its doc comment above promises — a redacted
	// report about a run, never about credential contents.
	RefreshAssertion credmaterialize.RefreshAssertion
}

// Smoke materializes accountID, starts a tiny headless run under provider, and
// waits for a clean exit. blob is accepted to satisfy the server hook; the
// materializer reloads the current keyring value by account id.
func (r *SmokeRunner) Smoke(ctx context.Context, accountID, provider string, blob []byte) error {
	_, err := r.Verify(ctx, accountID, provider, blob)
	return err
}

// Verify is Smoke with the extra SmokeResult detail. Smoke remains the
// narrow hook the account RPC surface depends on.
func (r *SmokeRunner) Verify(ctx context.Context, accountID, provider string, _ []byte) (SmokeResult, error) {
	if r == nil {
		return SmokeResult{}, fmt.Errorf("credential verification runner not configured")
	}
	client := r.clients[provider]
	if client == nil {
		return SmokeResult{}, fmt.Errorf("credential verification unavailable: %w", agent.AgentRunnerNotLoaded(provider, r.clients))
	}

	// Baseline for this run's own credential writes. Taken before materialize
	// because MaterializeCodex can itself fold a refreshed auth.json back into
	// the store, and that write is as self-caused as the persist-back below.
	// Every return past this point reports the delta, including the failure
	// paths: a run that wrote and then failed still owns those writes, and
	// attributing them to a concurrent writer would discard a verdict that
	// correctly describes the stored credential.
	beforeSaves := r.saves.count(accountID)
	// The refresh assertion is only known once the credential has been
	// materialized. Every return before that point reports Unknown, which is
	// the correct answer: nothing has read the token yet, and "cannot
	// evaluate" must never be reported as evidence of a dead refresh chain.
	refresh := credmaterialize.RefreshAssertionUnknown
	selfWrites := func() SmokeResult {
		return SmokeResult{
			SelfCredentialWrites: r.saves.count(accountID) - beforeSaves,
			AmbientAuth:          r.ambientAuthState(ctx, accountID, provider),
			RefreshAssertion:     refresh,
		}
	}
	ctx = r.withCredentialWriteGuard(ctx, accountID, beforeSaves)

	mat, persist, err := r.materialize(ctx, accountID, provider)
	if err != nil {
		return selfWrites(), err
	}
	refresh = mat.RefreshAssertion
	env := mat.Env

	workDir, err := os.MkdirTemp("", "boss-account-smoke-work-*")
	if err != nil {
		return selfWrites(), fmt.Errorf("create smoke workdir: %w", err)
	}
	defer func() { _ = os.RemoveAll(workDir) }()
	logDir, err := os.MkdirTemp("", "boss-account-smoke-log-*")
	if err != nil {
		return selfWrites(), fmt.Errorf("create smoke log dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(logDir) }()

	sessionID := smokeSessionID()
	logPath := filepath.Join(logDir, sessionID+".log")
	if _, err := client.StartRun(ctx, &bossanovav1.StartAgentRunRequest{
		WorkDir:   workDir,
		Plan:      smokePrompt,
		SessionId: sessionID,
		LogPath:   logPath,
		ExtraEnv:  env,
	}); err != nil {
		return selfWrites(), fmt.Errorf("credential verification start: %w", err)
	}

	if err := r.waitClean(ctx, client, sessionID); err != nil {
		_, _ = client.StopRun(context.Background(), &bossanovav1.StopAgentRunRequest{SessionId: sessionID})
		return selfWrites(), appendSmokeDiagnostic(err, logPath)
	}
	if persist != nil {
		if err := persist(ctx); err != nil {
			return selfWrites(), fmt.Errorf("credential verification persist refreshed credential: %w", err)
		}
		return selfWrites(), nil
	}
	return selfWrites(), nil
}

// ambientAuthState compares the ambient codex login against the stored
// credential for accountID. It is evaluated at RETURN time, on every exit path,
// so the comparison sees the freshest stored bytes — MaterializeCodex can fold
// an agent-side refresh into the store mid-run, and a comparison taken before
// that would describe a credential the run itself replaced.
//
// It is deliberately unconditional about failure: the helper cannot error, and
// every unreadable or unresolvable case is not-evaluable. A diagnostic that
// fails the verification it was added to observe is worse than no diagnostic
// (docs/solutions/design-patterns/a-safety-refusal-must-not-become-a-fatal-error-when-the-fallback-is-worse-than-what-was-refused.md).
//
// The context is stripped of cancellation for the same reason that note gives
// about diagnostic writes: a timed-out or cancelled run is EXACTLY the case
// where an operator most needs to know the stored refresh token was superseded,
// and inheriting the cancellation would reproduce silence for that failure
// class. Stripping it alone would be unbounded, though: the comparison's first
// act is a keyring-backed store.LoadCredential, and once the smoke timeout can
// no longer interrupt it nothing else would. A replacement deadline restores a
// bound on the context-aware seams — the same shape runVerification uses for its
// own post-cancellation write — and expiry lands on not-evaluable, which is
// already the honest answer. See ambientAuthCompareTimeout for the seam this
// deadline cannot reach.
//
// Only codex has an ambient login to compare against; every other provider is
// not-evaluable by construction.
func (r *SmokeRunner) ambientAuthState(ctx context.Context, accountID, provider string) credmaterialize.AmbientAuthState {
	if provider != providerCodex {
		return credmaterialize.AmbientAuthNotEvaluable
	}
	cmpCtx, cancelCmp := context.WithTimeout(context.WithoutCancel(ctx), ambientAuthCompareTimeout)
	defer cancelCmp()
	return r.materializer.CompareAmbientCodexAuth(cmpCtx, accountID)
}

// materialize returns the whole Materialized value rather than just its Env
// overlay: BOS-1174 added a second field the caller needs (the redacted
// refresh assertion), and projecting one field here would have meant the
// verdict silently stopped at this seam.
func (r *SmokeRunner) materialize(ctx context.Context, accountID, provider string) (credmaterialize.Materialized, credmaterialize.PersistBack, error) {
	switch provider {
	case "claude":
		mat, err := r.materializer.MaterializeClaude(ctx, accountID)
		if err != nil {
			return credmaterialize.Materialized{}, nil, fmt.Errorf("materialize claude account: %w", err)
		}
		return mat, nil, nil
	case providerCodex:
		mat, persist, err := r.materializer.MaterializeCodex(ctx, accountID)
		if err != nil {
			return credmaterialize.Materialized{}, nil, fmt.Errorf("materialize codex account: %w", err)
		}
		return mat, persist, nil
	default:
		return credmaterialize.Materialized{}, nil, fmt.Errorf("credential verification unavailable: unknown provider %q", provider)
	}
}

// errCredentialReplacedMidRun reports that the stored credential moved under a
// verification run that was about to fold its own materialized copy back in.
var errCredentialReplacedMidRun = errors.New("credential replaced during verification")

// credentialWriteGuardKey scopes a per-run credential-write guard onto the
// context that already flows from Verify into MaterializeCodex and the
// PersistBack closure it returns.
type credentialWriteGuardKey struct{}

// credentialWriteGuard is one run's claim about the stored credential: at
// baselineSaves writes of its own, the store's generation was baselineGen.
type credentialWriteGuard struct {
	accountID    string
	baselineGen  uint64
	baselineSave uint64
}

// withCredentialWriteGuard records the run's starting generation alongside its
// starting write count, so a later save can tell "the only writes since I
// started are mine" from "someone replaced this credential".
//
// The two readings must be taken together under the credential lock: a
// generation sampled apart from the write count describes a different instant
// than the count does, and the guard's whole arithmetic is the difference
// between them. Nothing holds the lock at this point in Verify — materialize
// and persist both take it later, and both release it before returning.
//
// A store with no generation surface gets no guard: there is nothing to
// compare, and refusing writes on an unavailable reading would break every
// custom store and test fake that only satisfies SmokeCredentialStore.
func (r *SmokeRunner) withCredentialWriteGuard(ctx context.Context, accountID string, beforeSaves uint64) context.Context {
	generations, ok := r.creds.store.(credentialGenerationStore)
	if !ok {
		return ctx
	}
	var baseline uint64
	if err := r.creds.WithCredentialLock(accountID, func() error {
		baseline = generations.CredentialGeneration(accountID)
		return nil
	}); err != nil {
		// An unreadable baseline is not evidence of a replacement. Leave the
		// guard off rather than refusing this run's own legitimate persist on
		// a reading we never obtained; credcheck's generation compare still
		// discards any verdict the run produces about a credential that moved.
		r.logger.Warn().Err(err).Str("account_id", accountID).
			Msg("account: could not baseline the credential generation; verification writes run unguarded")
		return ctx
	}
	return context.WithValue(ctx, credentialWriteGuardKey{}, credentialWriteGuard{
		accountID:    accountID,
		baselineGen:  baseline,
		baselineSave: beforeSaves,
	})
}

func (r *SmokeRunner) waitClean(ctx context.Context, client agent.AgentRunnerClient, sessionID string) error {
	wctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()

	for {
		resp, err := client.ExitStatus(wctx, &bossanovav1.AgentExitStatusRequest{SessionId: sessionID})
		if err != nil {
			return fmt.Errorf("credential verification status: %w", err)
		}
		if resp.GetIsComplete() {
			if exitError := resp.GetExitError(); exitError != "" {
				// A runner that never executed the agent binary reached no
				// provider, so "credential verification failed" names the
				// wrong thing entirely -- that sentence is what sent an
				// operator down the auth path for a PATH fault (BOS-1172).
				// The sentinel already names the binary it could not run, so
				// surface it as the leading sentence rather than prefixing it
				// with a second, redundant clause. appendSmokeDiagnostic still
				// adds the log tail, which carried the real cause all along.
				if isRunnerUnavailableSentinel(exitError) {
					return errors.New(exitError)
				}
				return fmt.Errorf("credential verification failed: %s", exitError)
			}
			return nil
		}
		select {
		case <-wctx.Done():
			return fmt.Errorf("credential verification timed out: %w", wctx.Err())
		case <-ticker.C:
		}
	}
}

// smokeDiagnosticError carries an operator-facing tail of the agent's own log
// alongside the failure it explains.
//
// The diagnostic is agent-authored prose. It is deliberately NOT part of any
// classification surface: a sign-in-shaped line in the tail of an otherwise
// transient failure must never be able to bench a working account. Unwrap
// therefore exposes the underlying failure, and credential maintenance
// classifies THAT (see smokeUnderlying in credcheck.go) rather than Error().
type smokeDiagnosticError struct {
	err  error
	diag string
}

func (e *smokeDiagnosticError) Error() string {
	return fmt.Sprintf("%s; diagnostic: %s", e.err.Error(), e.diag)
}

func (e *smokeDiagnosticError) Unwrap() error { return e.err }

func appendSmokeDiagnostic(err error, logPath string) error {
	if err == nil {
		return nil
	}
	diag := smokeDiagnostic(logPath)
	if diag == "" {
		return err
	}
	return &smokeDiagnosticError{err: err, diag: diag}
}

func smokeDiagnostic(logPath string) string {
	// #nosec G304 -- reads a daemon-internal smoke-test log path; output is agenterr.Redact'ed before use
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
	data, err := os.ReadFile(logPath)
	if err != nil || len(data) == 0 {
		return ""
	}
	data = agenterr.Redact(data)
	if len(data) > smokeDiagnosticTailBytes {
		data = data[len(data)-smokeDiagnosticTailBytes:]
		if i := strings.IndexByte(string(data), '\n'); i >= 0 && i+1 < len(data) {
			data = data[i+1:]
		}
	}
	lines := strings.Split(string(data), "\n")
	texts := make([]string, 0, smokeDiagnosticMaxLines)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry struct {
			Text string `json:"text"`
		}
		text := line
		if err := json.Unmarshal([]byte(line), &entry); err == nil && strings.TrimSpace(entry.Text) != "" {
			text = entry.Text
		}
		text = strings.TrimSpace(string(agenterr.Redact([]byte(text))))
		if text == "" {
			continue
		}
		texts = append(texts, text)
		if len(texts) > smokeDiagnosticMaxLines {
			texts = texts[1:]
		}
	}
	diag := strings.Join(texts, " | ")
	if len(diag) > smokeDiagnosticMaxChars {
		diag = diag[len(diag)-smokeDiagnosticMaxChars:]
		if i := strings.IndexByte(diag, ' '); i >= 0 && i+1 < len(diag) {
			diag = diag[i+1:]
		}
	}
	return diag
}

func smokeSessionID() string {
	return uuid.NewString()
}

// credentialSaveCounter tallies successful credential writes per account. It
// is shared between a SmokeRunner and the store adapter handed to its
// materializer, which is the single seam every write in a run flows through,
// so a run can measure its own writes by taking a delta around itself.
type credentialSaveCounter struct {
	mu sync.Mutex
	n  map[string]uint64
}

func newCredentialSaveCounter() *credentialSaveCounter {
	return &credentialSaveCounter{n: make(map[string]uint64)}
}

func (c *credentialSaveCounter) inc(accountID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n[accountID]++
}

func (c *credentialSaveCounter) count(accountID string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[accountID]
}

type credentialStoreAdapter struct {
	store SmokeCredentialStore
	saves *credentialSaveCounter
}

func (a credentialStoreAdapter) LoadCredential(_ context.Context, accountID string) ([]byte, error) {
	return a.store.Load(accountID)
}

// SaveCredential refuses a write that would land on a credential someone else
// replaced mid-run, then counts only writes that actually committed: a failed
// save leaves the stored credential — and therefore its generation —
// untouched, so counting an attempt would let the run absorb someone else's
// bump.
func (a credentialStoreAdapter) SaveCredential(ctx context.Context, accountID string, blob []byte) error {
	if err := a.refuseIfReplaced(ctx, accountID); err != nil {
		return err
	}
	if err := a.store.Save(accountID, blob); err != nil {
		return err
	}
	if a.saves != nil {
		a.saves.inc(accountID)
	}
	return nil
}

// refuseIfReplaced fails the save when the store's generation has moved by more
// than this run's own committed writes — i.e. when an operator refresh or a
// peer session replaced the credential after the run took its baseline.
//
// This is a data-loss guard, not an attribution one. credcheck's generation
// compare already discards a *verdict* computed against a superseded
// credential, but it runs after the fact and cannot undo a keyring write:
// persist-back reloads the store as its merge baseline and mergePreservingIDToken
// lets the on-disk tokens win, so an unguarded fold silently overwrites the
// replacement with the tokens the run materialized before it existed.
//
// Refusing returns an error rather than skipping quietly on purpose: a silent
// skip would let persistBack advance its recorded hash and durable write record
// as though the store had absorbed these bytes, and the next materialization
// would then decline to fold a genuine agent-side refresh.
//
// The caller already holds the store's credential lock — it is non-reentrant
// and every SaveCredential path runs inside it — so read the generation
// directly and do NOT re-acquire it here.
func (a credentialStoreAdapter) refuseIfReplaced(ctx context.Context, accountID string) error {
	guard, ok := ctx.Value(credentialWriteGuardKey{}).(credentialWriteGuard)
	if !ok || guard.accountID != accountID {
		return nil
	}
	generations, ok := a.store.(credentialGenerationStore)
	if !ok {
		return nil
	}
	var mine uint64
	if a.saves != nil {
		mine = a.saves.count(accountID) - guard.baselineSave
	}
	if got := generations.CredentialGeneration(accountID); got != guard.baselineGen+mine {
		return fmt.Errorf("%w: account %s", errCredentialReplacedMidRun, accountID)
	}
	return nil
}

// WithCredentialLock forwards the concrete store's optional per-account lock
// to credmaterialize. This keeps Codex smoke-token persistence serialized with
// account refreshes that use the same store.
func (a credentialStoreAdapter) WithCredentialLock(accountID string, fn func() error) error {
	if locker, ok := a.store.(credentialStoreLocker); ok {
		return locker.WithCredentialLock(accountID, fn)
	}
	return fn()
}
