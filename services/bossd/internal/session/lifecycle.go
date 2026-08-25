// Package session provides the SessionLifecycle orchestrator that wires
// together worktree management, Claude process management, and the state
// machine for a complete session lifecycle.
package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"golang.org/x/sync/singleflight"

	"github.com/recurser/bossalib/config"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gitremote"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossalib/sessionreason"
	libtelemetry "github.com/recurser/bossalib/telemetry"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/dotenv"
	gitpkg "github.com/recurser/bossd/internal/git"
	"github.com/recurser/bossd/internal/proofenv"
	"github.com/recurser/bossd/internal/rotation"
	"github.com/recurser/bossd/internal/tmux"
)

var (
	errListOpenPRs             = errors.New("list open PRs")
	conventionalCommitPrefixRE = regexp.MustCompile(`^[[:alpha:]][[:alnum:]-]*(\([^)]*\))?!?:[[:space:]]+`)
	prNumberPrefixRE           = regexp.MustCompile(`^\[#[0-9]+\][[:space:]]+`)
)

// preflightRollbackTimeout bounds cleanup after a post-setup capability check
// fails. The request may already be canceled, but its unrecorded worktree and
// branch still need to be removed before a retry can safely create them again.
const preflightRollbackTimeout = 30 * time.Second

// settingUpTracker is the narrow slice of *status.DisplayTracker the
// lifecycle needs to drive the transient "initializing" and "archiving"
// display statuses. A local interface keeps the session package free of a
// status-package import dependency. *status.DisplayTracker satisfies it.
type settingUpTracker interface {
	SetSettingUp(sessionID string, on bool)
	SetArchiving(sessionID string, on bool)
}

// PollArmer arms a poll-fallback goroutine that drives an agent run to
// completion when the agent doesn't support a finalize hook. Implemented
// by *agent.PollFallback (which accepts a wider AgentRunnerClient via
// interface satisfaction). Defined here so tests can fake it.
//
// We pass the full agent.AgentRunnerClient — even though the real
// PollFallback.Arm only needs ExitStatus — because Go's structural typing
// would otherwise create a name-only mismatch between session's narrower
// interface and agent's narrower interface.
type PollArmer interface {
	Arm(ctx context.Context, sessionID, agentSessionID string, client agent.AgentRunnerClient)
}

type pollRunCompleter interface {
	SignalRunComplete(sessionID, agentSessionID, exitError string)
}

type cronCompletionNotifier interface {
	NotifyCronAgentStopped(sessionID string)
}

type sessionDeletedNotifier func(context.Context, string)

// chatStatusReporter records process status for a chat's agent_session_id.
// Satisfied by *status.Tracker. Kept as an interface so the session package
// doesn't depend on the status package and tests can inject a recorder.
type chatStatusReporter interface {
	Update(agentSessionID string, status bossanovav1.ChatStatus, lastOutputAt time.Time)
}

// questionSignalReader reports whether a structured pending-question signal
// is live for a headless agent session. Kept narrow so session does not depend
// on the status package and tests can inject a reader.
type questionSignalReader interface {
	HasPending(agentSessionID string) bool
	Clear(agentSessionID string)
}

// Lifecycle orchestrates worktree creation, Claude process management,
// and state machine transitions for coding sessions.
type Lifecycle struct {
	sessions    db.SessionStore
	repos       db.RepoStore
	agentChats  db.AgentChatStore
	cronJobs    db.CronJobStore
	worktrees   gitpkg.WorktreeManager
	agentRunner agent.AgentDispatcher
	tmux        *tmux.Client
	provider    vcs.Provider
	logger      zerolog.Logger
	telemetry   libtelemetry.Client

	// branchResolver, when set, lets the finalize attach path match PRs
	// against the worktree's live branch (the agent may have switched
	// branches). Nil means stored-branch matching only.
	branchResolver LiveBranchResolver

	// pollArmer is retained for compatibility with older wiring, but tmux-
	// hosted lifecycle paths no longer arm plugin ExitStatus polling.
	pollArmer PollArmer

	// pollCompleter preserves HostServiceServer's StartChatRun waiter path
	// for hookless tmux chats. Lifecycle-owned cron runs are not registered
	// in HostServiceServer, so cron finalization is driven by
	// cronCompletionNotifier below instead of host-service maps.
	pollCompleter pollRunCompleter

	// questionHooks records the per-run question-hook token for headless runs
	// (BOS-486). Nil means the loopback question context is simply not handed
	// to headless agents — they run exactly as they did before, with no
	// question signal. Wired post-construction, like pollCompleter, because
	// cmd/main.go builds the lifecycle before the host service.
	questionHooks questionHookRegistrar

	// cronCompletionNotifier receives hookless poll completion candidates
	// for lifecycle-owned cron sessions.
	cronCompletionNotifier cronCompletionNotifier

	// reapFinalizer is the finalize entry the stranded-cron sweep
	// (recoverStrandedCronSessions) routes dead strands through. nil means
	// "use the real finalizeSessionFrom"; tests inject a fake to record the
	// (sessionID, expectedStates) it is called with without exercising the
	// full finalize pipeline.
	reapFinalizer func(ctx context.Context, sessionID string, expectedStates []int) (*FinalizeResult, error)

	// daemonCtx is retained for compatibility with older SetDaemonCtx wiring.
	daemonCtx context.Context //nolint:containedctx // retained compatibility field

	// agentClientHookSupport caches the per-agent-name IsSupported result
	// from ConfigureFinalizeHook. Populated lazily during Bootstrap (and,
	// later, SetAgents). It is used only to skip the RPC for known-hookless
	// agents — hook-supporting agents still call ConfigureFinalizeHook per
	// surviving chat because the hook config is written per-worktree and must
	// carry the current daemon port. nil-safe: a missing entry triggers a
	// fresh probe.
	agentClientHookSupport map[string]bool

	// hookPort is the loopback TCP port of the daemon's Stop-hook server.
	// Stamped via SetHookPort once the hook server has bound, before any
	// session that needs a HookToken is started. Zero means "not yet set"
	// and StartSession will error out rather than write a config that
	// points at no listener.
	hookPort int

	// proxyPort is the loopback TCP port of the S7 failover reverse proxy
	// (BOS-320), stamped via SetProxyPort once that server has bound. Zero
	// means "not bound" — the ANTHROPIC_BASE_URL overlay is then never injected
	// and sessions talk to api.anthropic.com directly (the liveness gate).
	proxyPort int
	// proxyRegistrar mints/registers the per-session proxy path token used in
	// the injected ANTHROPIC_BASE_URL. Nil (default) disables injection, so the
	// proxy is strictly additive and opt-in. Never logs the token.
	proxyRegistrar proxyTokenRegistrar

	// paneRepair dispatches a probe-skipping respawn-in-place for one chat whose
	// pane the failover proxy has just rejected with its OWN 401 (BOS-982).
	// Wired in production to ChatRotator.OnProxyTokenUnresolved. Nil (default)
	// makes RepairProxyPane inert, so the attribution path stays strictly
	// additive: without a dispatcher the proxy's 401 behaves exactly as before.
	paneRepair func(agentSessionID string)

	// defaultAccountResolver resolves a provider's managed default account id at
	// startup-adoption time (BOS-481), mirroring the spawn's DefaultAccountID
	// resolution for a surviving pane whose chat AND session both lack a
	// persisted account binding. Nil (default) ⇒ such both-nil rows resolve to ""
	// and are skipped. Wired in production to account.Resolver.DefaultAccountID.
	defaultAccountResolver func(ctx context.Context, provider string, now time.Time) (string, error)

	// agents maps an agent plugin name (matching session.AgentName) to its
	// AgentRunnerClient — used for ConfigureFinalizeHook,
	// BuildInteractiveCommand, and other RPCs that aren't on AgentRunner.
	// Populated via SetAgents during daemon startup. An empty map is valid
	// (sessions without HookToken still work); the map is read-only after
	// SetAgents and lookups must not mutate it.
	agents map[string]agent.AgentRunnerClient

	// agentLogsDir is the bossd-owned directory where agent plugins tee
	// their interactive (tmux-hosted) output. Stamped via SetAgentLogsDir
	// during daemon startup. StartTmuxChat passes a per-agent-session log
	// path under this directory into BuildInteractiveCommand so the chat
	// list view's "tail my chat log" affordance has a stable location to
	// read from. An empty string disables interactive launch — StartTmuxChat
	// will reject the call with FailedPrecondition rather than write to an
	// unconfigured path.
	agentLogsDir string

	// newTmuxChatAgentSessionID mints daemon-local IDs for tmux-hosted
	// agent chats. Kept on the Lifecycle instance so tests can make one
	// fixture deterministic without changing package-global state.
	newTmuxChatAgentSessionID func() string

	// tmuxCompletionPollInterval controls how often lifecycle-owned hookless
	// cron runs check whether their tmux session has exited.
	tmuxCompletionPollInterval time.Duration

	// startedAt is when this Lifecycle (and so, in production, this daemon
	// process) was constructed. The startup stranded-bootstrap reaper uses it to
	// reclaim only rows that predate the process, which is what makes that pass
	// unable to race a create the running daemon started (BOS-717/BOS-426).
	startedAt time.Time

	// bootstrapTimeout overrides BootstrapTimeout, the overall deadline
	// StartSession runs its worktree/agent bootstrap under (BOS-717). Zero means
	// "use the constant"; tests shorten it so the deadline path is exercisable in
	// milliseconds. The stranded-bootstrap reaper derives its age threshold from
	// the same value (bootstrapReapThreshold) so the two cannot drift.
	bootstrapTimeout time.Duration

	// settingUpTracker drives the transient "initializing" display status
	// while a setup script runs. nil in tests that don't exercise it; all
	// uses are nil-guarded.
	settingUpTracker settingUpTracker

	// sessionDeletedNotifier publishes session deletions triggered inside the
	// lifecycle, such as no-change cron finalization cleanup.
	sessionDeletedNotifier sessionDeletedNotifier

	// liveness reports whether a boss session's agent is still running, across
	// both headless subprocesses and tmux-hosted chats. The stranded-cron sweep
	// (cronRunIsOver) consults it as a fallback when the durable agent log can't
	// answer — e.g. a logless Codex run or a failed tmux pipe-pane — so such runs
	// can still be reaped once liveness confirms the agent is gone. nil leaves
	// the conservative "can't tell => still running" default in place.
	liveness sessionLiveness

	// chatStatus records WORKING/STOPPED for headless runs (codex exec /
	// claude --print), which have no tmux pane for TmuxStatusPoller and no
	// finalize hook. nil leaves headless chats statusless (the pre-fix
	// behaviour); all uses are nil-guarded.
	chatStatus chatStatusReporter

	// questionSignals is the shared structured-question store. Headless runs
	// have no tmux pane, so their lifecycle watcher is the reader that turns a
	// live signal into CHAT_STATUS_QUESTION. Nil preserves the prior behavior.
	questionSignals questionSignalReader

	// headlessStatusPollInterval controls how often watchHeadlessRunStatus
	// polls the agent plugin for run liveness. Defaults to 2s in NewLifecycle.
	headlessStatusPollInterval time.Duration

	// proofEnv resolves the allowlisted proof env overlay (proof credentials
	// + non-secret proof constants) injected into every managed agent spawn
	// so the agentic proof pipeline can run — including in unattended cron
	// worktrees where nothing else provides these values. Defaulted in
	// NewLifecycle to a real keyring-backed resolver; SetProofEnvResolver
	// overrides it for tests. Resolved fresh per spawn; never logged.
	proofEnv proofEnvResolver

	// accountEnv resolves the selected account's env overlay (e.g.
	// CLAUDE_CODE_OAUTH_TOKEN / CODEX_HOME) for account rotation, keyed off the
	// session's AccountID binding. Nil in older/test wiring degrades to no
	// overlay (account 0). Resolved fresh per spawn; never logged.
	accountEnv accountEnvResolver

	// rotationConfig holds the auto-rotation policy knobs (kill switch, max
	// rotations). The zero value enables rotation with default caps, so an
	// unset config is a safe production default. Injected via SetRotationConfig.
	rotationConfig config.ManagedAccountsConfig

	// rotationDecider, accountMaterializer, and rotationBinding are the narrow
	// injected seams the usage-limit rotation intercept consumes. Any nil seam
	// degrades rotation to today's Block path (fail-safe). Production wires the
	// rotation decide function / MaterializeAccount RPC / BOS-170 binding via the
	// setters in rotation.go; tests inject fakes.
	rotationDecider     rotationDecider
	accountMaterializer accountMaterializer
	rotationBinding     rotationBinding
	rateLimitProbe      rateLimitProbe

	// proxyProbeGroup coalesces the failover proxy's inline confirm-before-cool
	// probe by account id. A rate-limit episode 429s several in-flight requests
	// bound to the SAME account at once, and each one used to pay its own
	// MaterializeAccount + ProbeRateLimit round trip plus a usage-cache write —
	// fanning probes out at the provider that is already unhappy. They are all
	// asking one question ("is this account limited right now?"), so one answer
	// serves them all. The engine's own single-flight is downstream of this call
	// and cannot cover it. (BOS-584)
	proxyProbeGroup singleflight.Group

	// proxyProbeTimeout bounds that inline probe. Zero means
	// defaultProxyUsageProbeTimeout; tests shorten it so the deadline path does
	// not cost real wall-clock seconds.
	proxyProbeTimeout time.Duration

	// accountGetter resolves a *models.Account by id, so CurrentBearer can turn
	// the binding's CappedAccountID into an account the materializer accepts
	// (CurrentBinding returns only the id). Nil ⇒ CurrentBearer degrades to ""
	// (fail-safe: the proxy forwards the client's own header). Production wires
	// the db account store's Get; tests inject a fake.
	accountGetter func(ctx context.Context, id string) (*models.Account, error)

	// rotationRecorder audits every rotation decision outcome (BOS-176). Nil is
	// safe: the Recorder's methods no-op on a nil receiver, so an unwired daemon
	// simply records nothing. Injected via SetRotationRecorder.
	rotationRecorder *rotation.Recorder

	// proxyAuditMu serializes the failover proxy path's non-rotate decline audit
	// writes (all-exhausted / no-eligible). Those recorders dedup per episode by
	// reading the session's newest audit row, so concurrent replays for the same
	// session must not interleave the read-then-write or both would slip the
	// dedup and double-record. Scoped to the proxy decline path (BOS-484).
	proxyAuditMu sync.Mutex

	// rotationConfigLoader re-reads rotation policy for decisions and parked
	// sweeps so policy changes take effect without a daemon restart (BOS-176).
	// Nil ⇒ fall back to the config injected via SetRotationConfig (the cached
	// value used by unit tests). Production wires a config.Load-backed adapter.
	rotationConfigLoader func() (config.ManagedAccountsConfig, error)

	// clock is the time source for the resume-at-T parked-rotation sweep. It is
	// nil in production (now() falls back to time.Now); tests inject a fake via
	// SetClockForTest so the "past resume_at" boundary is deterministic. Kept
	// off the hot classify path (classifyUsageLimit still uses time.Now).
	clock func() time.Time

	// bearerRetry* bounds how long currentBearerForAccount retries a transient
	// Materialize failure (typically the claude plugin subprocess restarting)
	// before surfacing ErrBearerUnavailable. Zero/unset ⇒ production defaults;
	// tests set them via SetBearerRetryForTest to keep retry/exhaustion instant.
	bearerRetryConfigured bool
	bearerRetryAttempts   int
	bearerRetryBackoff    time.Duration
	bearerRetryBudget     time.Duration

	// Account-switch seams (BOS-171 manual switch). All are nil-safe:
	// accountSwitchBinding defaults lazily from `sessions`; a nil registry
	// makes SwitchAccount error (it can't validate the target); a nil working
	// reader treats the chat as not mid-turn; a nil transcript probe assumes a
	// present resume id is resumable. Wired in production via
	// SetAccountSwitchDeps; tests set the fields directly.
	accountSwitchBinding     accountBinding
	accountSwitchRegistry    accountRegistry
	accountSwitchWorking     chatStatusReader
	accountSwitchTranscripts transcriptProbe

	// backgroundDraftPRs tracks the in-flight background draft-PR creates
	// StartSession spawns (BOS-540), keyed by session id. The value channel is
	// closed when that session's create finishes. The registry is what makes
	// the step tracked rather than fire-and-forget: daemon shutdown joins every
	// entry via WaitForBackgroundDraftPRs, and callers that need one session's
	// create to have landed use WaitForBackgroundDraftPR. Lazily created on
	// first use, so the zero Lifecycle is usable.
	backgroundDraftPRMu sync.Mutex
	backgroundDraftPRs  map[string]*backgroundDraftPRHandle

	// draftPRRetries rate-limits RetryFailedDraftPRsPeriodic (BOS-875), keyed
	// by session id. Deliberately in memory and NOT a column: this is a rate
	// limiter, not a fact about the session, and it must not outlive the
	// process — a daemon restart legitimately re-arms every retry, which is
	// exactly when a fresh attempt is most likely to succeed. Lazily created on
	// first use, so the zero Lifecycle is usable.
	draftPRRetryMu sync.Mutex
	draftPRRetries map[string]draftPRRetryState

	// draftPREnsurer is the PR-open entry the draft-PR retry sweep calls, held
	// as a seam for the same reason as reapFinalizer above: nil means the real
	// EnsurePR, and tests inject a recorder so the sweep's selection, cooldown
	// and log-and-continue behaviour can be driven without a git remote or a
	// GitHub provider. EnsurePR itself is untouched by this indirection.
	draftPREnsurer func(ctx context.Context, sessionID string) error
}

// proofEnvResolver resolves the allowlisted proof environment overlay. The
// concrete implementation is proofenv.Resolver; the interface keeps the
// lifecycle testable without a real keyring.
type proofEnvResolver interface {
	Resolve() map[string]string
}

// SetProofEnvResolver overrides the proof env resolver (tests inject a fake
// so no real keyring is touched). Safe to leave unset in production —
// NewLifecycle installs a real resolver.
func (l *Lifecycle) SetProofEnvResolver(r proofEnvResolver) { l.proofEnv = r }

// resolveProofEnv returns the proof env overlay, or nil when no resolver is
// wired (older/test wiring). Never logs values.
func (l *Lifecycle) resolveProofEnv() map[string]string {
	if l.proofEnv == nil {
		return nil
	}
	return l.proofEnv.Resolve()
}

// accountEnvResolver resolves the per-account environment overlay for
// rotation, keyed off the session's AccountID binding. Kept as an interface so
// the real resolver (accountwiring.SpawnEnvResolver, wrapping account.Resolver)
// can be injected without the session package depending on db/plugin plumbing.
type accountEnvResolver interface {
	Resolve(ctx context.Context, sess *models.Session) map[string]string
}

// SetAccountEnvResolver overrides the account env resolver (tests inject a
// synthetic map; the daemon installs the real one). Safe to leave unset.
func (l *Lifecycle) SetAccountEnvResolver(r accountEnvResolver) { l.accountEnv = r }

// resolveAccountEnv returns the account env overlay for sess, or nil when no
// resolver is wired (older/test wiring) or the session is unbound. When the S7
// failover proxy is enabled and live, it also folds in the ANTHROPIC_BASE_URL
// overlay that points the Claude subprocess at the loopback proxy (BOS-320),
// but only when the bound account resolver produced a Claude OAuth bearer the
// proxy can substitute for the BOS-326 sentinel. Never logs values (the proxy
// token is a secret).
func (l *Lifecycle) resolveAccountEnv(ctx context.Context, sess *models.Session) map[string]string {
	var base map[string]string
	if l.accountEnv != nil {
		base = l.accountEnv.Resolve(ctx, sess)
	}
	return l.ApplyFailoverProxyEnv(sess, base)
}

// ApplyFailoverProxyEnv folds the BOS-320/326 Claude proxy overlay into an
// already-materialized account env. It is shared by Lifecycle-owned spawns and
// server-driven attach/wake spawns. A nil/empty bearer means the proxy could not
// translate the sentinel, so the overlay is skipped and the caller keeps the
// direct/ambient auth behavior.
func (l *Lifecycle) ApplyFailoverProxyEnv(sess *models.Session, base map[string]string) map[string]string {
	if l == nil {
		return base
	}
	proxyURL := l.proxyBaseURL(sess)
	if proxyURL == "" {
		return base
	}
	return applyFailoverProxyOverlay(proxyURL, base)
}

// ApplyFailoverProxyEnvForChat folds the proxy overlay into a cross-agent Claude
// chat spawn whose account binding lives on agent_chats instead of sessions.
func (l *Lifecycle) ApplyFailoverProxyEnvForChat(sess *models.Session, chat *models.AgentChat, accountID string, base map[string]string) map[string]string {
	if l == nil {
		return base
	}
	proxyURL := l.proxyBaseURLForChat(sess, chat, accountID)
	if proxyURL == "" {
		return base
	}
	return applyFailoverProxyOverlay(proxyURL, base)
}

func applyFailoverProxyOverlay(proxyURL string, base map[string]string) map[string]string {
	if base[claudeOAuthTokenEnvKey] == "" {
		return base
	}
	// Fold ANTHROPIC_BASE_URL into the account overlay so it inherits the same
	// merge precedence (account > proof, below managed BOSS_*) at every spawn
	// site without touching them individually.
	overlay := make(map[string]string, len(base)+2)
	maps.Copy(overlay, base)
	overlay["ANTHROPIC_BASE_URL"] = proxyURL
	// The interactive Claude REPL ignores the injected CLAUDE_CODE_OAUTH_TOKEN and
	// authenticates from the keychain; a sentinel ANTHROPIC_API_KEY overrides that
	// and routes requests through the proxy as x-api-key (BOS-326). The proxy
	// strips the sentinel and substitutes the bound account's OAuth bearer.
	overlay["ANTHROPIC_API_KEY"] = SentinelAPIKey
	// Drop the account's OAuth bearer from the SUBPROCESS env once the sentinel is
	// in place: the proxy resolves the bound account's bearer server-side
	// (Failover.CurrentBearer, keyed on session/chat id — never read back from
	// this env), so the token here is dead weight. Worse, leaving it alongside the
	// sentinel trips Claude Code's "both CLAUDE_CODE_OAUTH_TOKEN and
	// ANTHROPIC_API_KEY set · auth may not work as expected" warning and mislabels
	// the session as "API Usage Billing" even though billing stays on the
	// subscription via the proxy swap. When the proxy is DOWN this overlay is never
	// applied (proxyURL == "" upstream), so base — token present, no sentinel — is
	// used for direct auth; the token is only ever redundant here, never load-
	// bearing. See the proxy first-leg auth in server.ProxyServer.handleProxy.
	delete(overlay, claudeOAuthTokenEnvKey)
	return overlay
}

// mergeEnv overlays extra onto base, returning a new map. base wins on key
// conflict: the managed BOSS_* environment is authoritative and must never
// be shadowed by the proof overlay. A nil/empty extra returns base
// unchanged (a fresh copy is still cheap and avoids aliasing surprises).
func mergeEnv(base, extra map[string]string) map[string]string {
	merged := make(map[string]string, len(base)+len(extra))
	for k, v := range extra {
		merged[k] = v
	}
	for k, v := range base {
		merged[k] = v // base wins on conflict
	}
	return merged
}

// mergeSessionEnv builds the tmux session environment with precedence
// managed (BOSS_*) > account > proof. Managed keys are authoritative and are
// NEVER shadowed; the account overlay overrides proof values but not managed
// ones. A nil/empty account map yields a result byte-identical to the prior
// mergeEnv(managed, proof) — the account-rotation no-op path (D9). Values are
// never logged.
func mergeSessionEnv(managed, account, proof map[string]string) map[string]string {
	return mergeEnv(managed, mergeEnv(account, proof))
}

// configHomeEnv selects the only environment values an interactive runner may
// need before the tmux child starts: CODEX_HOME takes precedence, and HOME is
// the fallback Codex uses when CODEX_HOME is absent. Keep session credentials
// out of the runner request; tmux still receives the complete environment.
func configHomeEnv(env map[string]string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	selected := make(map[string]string, 2)
	for _, name := range []string{"CODEX_HOME", "HOME"} {
		if value, ok := env[name]; ok {
			selected[name] = value
		}
	}
	return selected
}

// SetHookPort records the hook server's bound loopback port so
// StartSession can stamp it into a worktree's settings.local.json when
// installing the Stop-hook config. Called from the daemon entrypoint
// after hookSrv.Listen() succeeds.
func (l *Lifecycle) SetHookPort(port int) {
	l.hookPort = port
}

// SetDisplayTracker wires the display tracker used to flip the transient
// "initializing" status while a setup script runs. Safe to leave unset.
func (l *Lifecycle) SetDisplayTracker(t settingUpTracker) {
	l.settingUpTracker = t
}

// SetSessionDeletedNotifier wires deletion notifications for lifecycle-owned
// cleanup paths that delete sessions outside the server RPC handlers.
func (l *Lifecycle) SetSessionDeletedNotifier(n func(context.Context, string)) {
	l.sessionDeletedNotifier = n
}

// SetSessionLiveness wires the liveness checker used by the stranded-cron sweep
// to reap logless runs once their agent is confirmed gone. Safe to leave unset:
// without it the sweep keeps its conservative "no durable evidence => still
// running" behavior.
func (l *Lifecycle) SetSessionLiveness(c sessionLiveness) {
	l.liveness = c
}

// SetChatStatus wires the daemon's chat status tracker so headless runs can
// report WORKING/STOPPED. Called once during daemon startup.
func (l *Lifecycle) SetChatStatus(r chatStatusReporter) { l.chatStatus = r }

// SetQuestionSignals wires the shared structured question-signal store used by
// the headless status watcher. Safe to leave unset for the historical
// WORKING/STOPPED-only behavior.
func (l *Lifecycle) SetQuestionSignals(r questionSignalReader) { l.questionSignals = r }

func (l *Lifecycle) headlessLiveStatus(agentSessionID string) bossanovav1.ChatStatus {
	if l.questionSignals != nil && l.questionSignals.HasPending(agentSessionID) {
		return bossanovav1.ChatStatus_CHAT_STATUS_QUESTION
	}
	return bossanovav1.ChatStatus_CHAT_STATUS_WORKING
}

// watchHeadlessRunStatus marks a headless agent run WORKING, then polls the
// agent plugin until the run exits and marks the chat STOPPED. Headless runs
// have no tmux pane (TmuxStatusPoller skips them) and no finalize hook, so
// without this a headless chat never gets a status and `boss chat wait` /
// get_chat_statuses / the orchestrator status stream can't tell it is done.
// Intended to run on a safego goroutine; returns once the run finishes.
func (l *Lifecycle) watchHeadlessRunStatus(agentName, agentSessionID string) {
	if l.chatStatus == nil || agentSessionID == "" {
		return
	}
	l.chatStatus.Update(agentSessionID, l.headlessLiveStatus(agentSessionID), time.Now())
	interval := l.headlessStatusPollInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	for {
		time.Sleep(interval)
		if !l.agentRunner.IsRunningByAgent(agentName, agentSessionID) {
			l.chatStatus.Update(agentSessionID, bossanovav1.ChatStatus_CHAT_STATUS_STOPPED, time.Now())
			if l.questionSignals != nil {
				l.questionSignals.Clear(agentSessionID)
			}
			return
		}
		// Refresh WORKING on every positive poll. The status tracker ages
		// entries out after status.StaleThreshold (~15s) and GetBatch then
		// surfaces them as STOPPED, so a single WORKING update at start would
		// make boss chat wait / get_chat_statuses / the orchestrator status
		// stream observe STOPPED while the headless agent is still running.
		// The poll interval (2s default) stays well under the threshold.
		l.chatStatus.Update(agentSessionID, l.headlessLiveStatus(agentSessionID), time.Now())
	}
}

// SetAgents installs the per-name AgentRunnerClient registry used to call
// ConfigureFinalizeHook (and other plugin RPCs) during StartSession. The
// map is keyed by agent plugin name (matching session.AgentName). Must be
// called before any session with a HookToken is started — sessions
// without a HookToken don't need this dep.
//
// An empty (or nil) map is valid: it just means no agent plugins are
// loaded, and StartSession will error with a clear message when a session
// that requires a hook tries to start. The map is treated as read-only
// after this call; callers must not mutate it.
//
// Concurrency: called exactly once during daemon startup, before serving
// begins. Not safe for concurrent re-injection alongside in-flight RPCs.
func (l *Lifecycle) SetAgents(m map[string]agent.AgentRunnerClient) { l.agents = m }

// SetTelemetry installs the optional daemon telemetry sink.
func (l *Lifecycle) SetTelemetry(client libtelemetry.Client) { l.telemetry = client }

// SetBranchResolver attaches a live-branch resolver used when matching an
// existing PR to a session whose stored branch may have drifted.
func (l *Lifecycle) SetBranchResolver(branches LiveBranchResolver) {
	l.branchResolver = branches
}

// Bootstrap performs one-time restart recovery for session-keyed runs. It first
// re-arms finalize hooks / completion polls for tmux-hosted chats that survived
// the daemon restart, then sweeps headless (boss new --detach) runs that the
// restart killed mid-flight into the terminal Orphaned state. Call once during
// daemon startup, after SetAgents and SetSessionLiveness and before serving.
func (l *Lifecycle) Bootstrap(ctx context.Context) {
	l.reArmSurvivingTmuxChats(ctx)
	l.sweepOrphanedHeadlessRuns(ctx)
	// Restore the failover-proxy path-token registry from its durable rows
	// (BOS-979). MUST run before adoptSurvivingPaneProxyTokens: a persisted row
	// is authoritative — it was written by the daemon that actually minted the
	// token — whereas the sweep RECONSTRUCTS a token from tmux env and can only
	// cover panes that are still alive and still on the matching port. Running
	// the rebuild first means the sweep degrades to a fallback for pre-migration
	// panes instead of being the sole recovery path.
	l.rebuildProxyTokenRegistry(ctx)
	// Re-adopt each surviving tmux pane's baked failover-proxy path token onto
	// the fresh ProxyServer (BOS-481), so a pane whose port still matches (the
	// fixed port of BOS-409) reconnects in place instead of wedging on a 401.
	// MUST run before detectStalePaneProxyPorts: adoption handles the
	// same-port panes, and the stale-port sweep then only surfaces the residual
	// panes whose baked port genuinely no longer matches.
	l.adoptSurvivingPaneProxyTokens(ctx)
	// Flag live tmux panes whose baked ANTHROPIC_BASE_URL points at a stale
	// failover-proxy port after this restart (BOS-409). Runs after SetProxyPort
	// has stamped the live port (Bootstrap is called from main.go after the proxy
	// block binds), so the comparison is against the real port.
	l.detectStalePaneProxyPorts(ctx)
	// Best-effort: retry any local base fast-forwards that a prior merge had to
	// defer because the operator's base checkout was dirty at merge time.
	l.worktrees.RetryDeferredBaseSyncs(ctx)
}

// reArmSurvivingTmuxChats restores supported session-keyed finalize hooks for
// tmux chats that were alive when the daemon last shut down.
//
// For each agent_chats row whose tmux session is still alive, it looks up the
// parent session's HookToken and calls the agent plugin's ConfigureFinalizeHook
// so that session's worktree gets its hook config rewritten with this
// (post-restart) daemon's port. The call runs per surviving chat because the
// config is per-worktree; the per-agent-name support result is cached only to
// skip the RPC for known-hookless agents, which are re-armed onto the completion
// poll instead. Hookless tmux-hosted runs are intentionally not wired to
// PollFallback: plugin ExitStatus only observes StartAgentRun processes, not
// tmux-spawned processes.
//
// Failures (DB error, missing session, RPC error) are logged and skipped;
// a single bad row mustn't block the rest from re-arming.
func (l *Lifecycle) reArmSurvivingTmuxChats(ctx context.Context) {
	if l.agentChats == nil {
		return
	}
	chats, err := l.agentChats.ListWithTmuxSession(ctx)
	if err != nil {
		l.logger.Warn().Err(err).Msg("bootstrap re-arm: failed to list chats with tmux session")
		return
	}
	for _, chat := range chats {
		if chat == nil || chat.AgentSessionID == "" {
			continue
		}
		// HookToken lives on the parent session record (the per-run hook
		// token used by /boss-repair lives only in HostServiceServer's
		// in-memory map — that flavour of run is gone after a daemon
		// restart anyway). If the session has no HookToken there's
		// nothing the Stop hook would have authenticated against, so
		// nothing to fall back from.
		sess, err := l.sessions.Get(ctx, chat.SessionID)
		if err != nil {
			l.logger.Warn().Err(err).
				Str("agent_session", chat.AgentSessionID).
				Str("session", chat.SessionID).
				Msg("bootstrap re-arm: failed to load session; skipping")
			continue
		}
		if sess.HookToken == nil || *sess.HookToken == "" {
			continue
		}
		client, ok := l.agents[chat.AgentName]
		if !ok || client == nil {
			l.logger.Warn().
				Str("agent", chat.AgentName).
				Str("agent_session", chat.AgentSessionID).
				Msg("bootstrap re-arm: agent client missing; skipping")
			continue
		}
		tmuxName := tmux.ChatSessionName(sess.RepoID, chat.AgentSessionID)
		if chat.TmuxSessionName != nil && *chat.TmuxSessionName != "" {
			tmuxName = *chat.TmuxSessionName
		}

		// A known-hookless agent has no finalize hook to rewrite — re-arm the
		// completion poll directly and skip the RPC.
		if supported, known := l.agentClientHookSupport[chat.AgentName]; known && !supported {
			l.armTmuxCompletionForHooklessTmux(chat.SessionID, chat.AgentSessionID, tmuxName)
			continue
		}

		// Call ConfigureFinalizeHook for every surviving hook-capable chat:
		// the hook config is written into THIS session's worktree and carries
		// this daemon's (post-restart) hook port. The write is per-worktree,
		// so it must run for each chat — caching the support result across
		// sessions (the previous behavior) skipped the RPC for every chat
		// after the first, leaving those worktrees pointing at the dead
		// previous daemon's port so their Stop hook never reached the
		// restarted daemon. We still cache the support result to skip the RPC
		// for known-hookless agents (above).
		hookResp, err := client.ConfigureFinalizeHook(ctx, &bossanovav1.ConfigureFinalizeHookRequest{
			WorkDir:   sess.WorktreePath,
			SessionId: chat.SessionID,
			HookToken: *sess.HookToken,
			HookPort:  clampInt32(l.hookPort),
		})
		if err != nil {
			l.logger.Warn().Err(err).
				Str("agent_session", chat.AgentSessionID).
				Msg("bootstrap re-arm: ConfigureFinalizeHook probe failed; skipping")
			continue
		}
		supported := hookResp.GetIsSupported()
		if l.agentClientHookSupport == nil {
			l.agentClientHookSupport = map[string]bool{}
		}
		l.agentClientHookSupport[chat.AgentName] = supported
		if supported {
			// Hook config (re)written for this worktree with the current port.
			continue
		}
		l.armTmuxCompletionForHooklessTmux(chat.SessionID, chat.AgentSessionID, tmuxName)
	}

	// NOTE: headless (boss new, non-cron, non-tmux_unattended, paneless) runs in
	// ImplementingPlan are deliberately NOT re-armed here. A headless run's exit
	// status lives only in the agent plugin's in-memory process map, which is
	// empty after a daemon restart (the plugin subprocess restarts too).
	// PollFallback.ExitStatus would then report a phantom clean exit
	// (IsComplete=true, ExitError="") for every such row, so re-arming would
	// clean-finalize runs that were actually interrupted by the restart —
	// prematurely marking partial PRs ready, losing real failures, and in the
	// no-PR/no-commits case hard-deleting the session. Cron and tmux_unattended
	// runs don't have this problem: both are tmux-hosted with a HookToken, so
	// they're re-armed in the loop above via ConfigureFinalizeHook / the
	// completion poll, keyed on liveness of the tmux pane rather than the
	// plugin's in-memory map. Instead of re-arming, sweepOrphanedHeadlessRuns
	// (called next in Bootstrap) marks these restart-killed headless runs
	// ORPHANED — loud and never green — keyed on liveness, not the lost exit map.
}

// sweepOrphanedHeadlessRuns marks headless (boss new --detach) runs that a
// daemon restart killed mid-flight. On restart the agent plugin's in-memory
// process map is empty, so a headless run's exit is unrecoverable — which is
// exactly why reArmSurvivingTmuxChats does NOT re-arm completion for them.
// Rather than leave such a run stranded-but-silent in ImplementingPlan, where
// its bootstrap-only draft PR can read as green, move it to the distinct
// terminal Orphaned state with a short reason so it is loudly visible and never
// masquerades as done. Deliberately no auto-re-dispatch: a one-shot's prompt may
// have side effects; the human decides.
//
// Conservative by construction — every ambiguous case fails toward NOT orphaning
// (a false Orphaned on a live run misleads more than a delayed one):
//   - unattended sessions (cron / tmux_unattended) are tmux-hosted and owned by
//     the re-arm loop + completion gate, so they are skipped here.
//   - a session with no agent run id has no headless run to orphan — skipped.
//   - only a session whose liveness DEFINITIVELY reports dead is swept. Unwired
//     liveness, or a session still alive (an interactive run whose tmux pane
//     survived the restart, or — defensively — a live process), is left untouched.
//
// The ImplementingPlan→Orphaned move uses the atomic CAS as its idempotency
// gate, mirroring the failed-headless-run block path, so a late completion
// signal racing the sweep is a safe no-op.
func (l *Lifecycle) sweepOrphanedHeadlessRuns(ctx context.Context) {
	if l.sessions == nil {
		return
	}
	orphanStore, ok := l.sessions.(db.OrphanHeadlessRunStore)
	if !ok {
		l.logger.Error().Msg("orphan sweep: session store does not support atomic orphan marker")
		return
	}
	stranded, err := l.sessions.ListByState(ctx, int(machine.ImplementingPlan))
	if err != nil {
		l.logger.Warn().Err(err).Msg("orphan sweep: failed to list implementing_plan sessions")
		return
	}
	for _, sess := range stranded {
		if sess == nil {
			continue
		}
		// Unattended runs are tmux-hosted and recovered by the re-arm loop and the
		// stranded-cron sweep; never treat them as headless orphans.
		if isUnattendedSession(sess) {
			continue
		}
		// No run id means no headless run to orphan (e.g. an idle interactive
		// session that never started an agent).
		if sess.AgentSessionID == nil || *sess.AgentSessionID == "" {
			continue
		}
		// Fail toward NOT orphaning: only sweep when liveness definitively reports
		// the session dead. Unwired liveness, a session still alive (an
		// interactive tmux pane that survived the restart), or a parked session
		// (its pane deliberately reaped, chat row kept) leaves the row alone.
		if l.liveness == nil || l.liveness.SessionLiveness(ctx, sess.ID) != LivenessDead {
			continue
		}
		advanced, err := orphanStore.OrphanHeadlessRun(ctx, sess.ID, OrphanedHeadlessRunReason)
		if err != nil {
			l.logger.Error().Err(err).Str("session", sess.ID).Msg("orphan sweep: transition to orphaned failed")
			continue
		}
		if !advanced {
			// A concurrent signal already advanced the session out of
			// ImplementingPlan; nothing to orphan.
			continue
		}
		l.logger.Warn().
			Str("session", sess.ID).
			Str("agent_session", *sess.AgentSessionID).
			Msg("orphaned headless run: killed by daemon restart, marked ORPHANED (not re-dispatched)")
	}
}

// SetPollArmer retains compatibility with older daemon wiring. Tmux-hosted
// lifecycle paths intentionally do not arm plugin ExitStatus polling.
func (l *Lifecycle) SetPollArmer(p PollArmer) { l.pollArmer = p }

// SetPollCompleter wires the host-service waiter completion path for
// hookless StartChatRun calls.
func (l *Lifecycle) SetPollCompleter(c pollRunCompleter) { l.pollCompleter = c }

// SetCronCompletionNotifier wires the cron completion gate for hookless
// lifecycle-owned cron runs.
func (l *Lifecycle) SetCronCompletionNotifier(n cronCompletionNotifier) {
	l.cronCompletionNotifier = n
}

// SignalSessionRunComplete is called by PollFallback when a hookless agent
// run completes. It preserves host-service waiter completion for StartChatRun
// and independently routes lifecycle-owned unattended sessions to the cron gate.
func (l *Lifecycle) SignalSessionRunComplete(sessionID, agentSessionID, exitError string) {
	// This agent session is finished either way, so drop its question-hook
	// token FIRST — ahead of the rotation intercept's early return, which would
	// otherwise strand the token of every rotated-away run. Keeps the registry
	// from growing for the daemon's lifetime and stops a stale hook POST from
	// authenticating. A no-op for ids that were never armed (tmux chats keep
	// their tokens in a separate registry, untouched by this call).
	l.releaseQuestionHookToken(agentSessionID)
	// Rotation intercept: a usage-limited exit on a live, rotatable plan run is
	// rotated-and-restarted (or parked) here. When handled, skip the normal
	// finalize/block fan-out below; the restarted run re-enters via its own
	// completion signal.
	if l.attemptUsageLimitRotation(context.Background(), sessionID, agentSessionID, exitError) {
		return
	}
	if l.pollCompleter != nil {
		l.pollCompleter.SignalRunComplete(sessionID, agentSessionID, exitError)
	}
	l.notifyCronCompletionIfUnattended(context.Background(), sessionID)
	l.finalizeHeadlessRunIfApplicable(context.Background(), sessionID, agentSessionID, exitError)
}

// headlessRunBlockedReasonMaxLen caps the length of the blocked reason derived
// from a failed run's exit error so a multi-kilobyte agent dump can't bloat the
// session row or the UI.
const headlessRunBlockedReasonMaxLen = 200

// headlessRunBlockedReason builds a short, non-secret summary of a failed
// headless run's exit error. It keeps only the first line (so stack traces /
// multi-line agent output don't leak) and truncates on a rune boundary.
func headlessRunBlockedReason(exitError string) string {
	summary := strings.TrimSpace(exitError)
	if summary == "" {
		summary = "agent run failed"
	}
	return "headless agent run failed: " + truncateBlockedReason(summary)
}

// truncateBlockedReason keeps only the first line of a blocked-reason summary
// (so stack traces / multi-line agent output don't leak) and truncates it on a
// rune boundary to headlessRunBlockedReasonMaxLen. Shared by the headless-run
// block path and the rotation park path.
func truncateBlockedReason(summary string) string {
	summary = strings.TrimSpace(summary)
	if idx := strings.IndexByte(summary, '\n'); idx >= 0 {
		summary = strings.TrimSpace(summary[:idx])
	}
	if utf8.RuneCountInString(summary) > headlessRunBlockedReasonMaxLen {
		summary = string([]rune(summary)[:headlessRunBlockedReasonMaxLen]) + "…"
	}
	return summary
}

// finalizeHeadlessRunIfApplicable advances a non-unattended headless session
// out of ImplementingPlan when its hookless run completes: a clean exit runs the
// same idempotent FinalizeSession the completion gate uses; a failed exit blocks
// the session with a short reason. It is a no-op for a missing session, an
// unattended session — cron OR tmux_unattended, which are tmux-hosted and owned
// by the completion gate — or a session no longer in ImplementingPlan (a
// duplicate signal or an already-advanced run).
//
// The state == ImplementingPlan gate is also what keeps this off other run
// flavors that call SignalSessionRunComplete: StartChatRun / repair runs and
// tmux-hosted runs are excluded by isUnattendedSession (cron and tmux_unattended
// runs are tmux-hosted, not headless-paneless); and once any such run has
// advanced past ImplementingPlan a late completion signal here is a no-op.
func (l *Lifecycle) finalizeHeadlessRunIfApplicable(ctx context.Context, sessionID, agentSessionID, exitError string) {
	if sessionID == "" || l.sessions == nil {
		return
	}
	session, err := l.sessions.Get(ctx, sessionID)
	if err != nil || session == nil {
		return
	}
	if isUnattendedSession(session) || session.State != machine.ImplementingPlan {
		return
	}

	if exitError == "" {
		res, err := l.FinalizeSession(ctx, sessionID)
		if err != nil {
			l.logger.Error().Err(err).Str("session", sessionID).Msg("headless finalize: FinalizeSession failed")
			return
		}
		l.logger.Info().
			Str("session", sessionID).
			Str("agent_session", agentSessionID).
			Str("outcome", string(res.Outcome)).
			Bool("noop", res.NoOp).
			Msg("headless finalize: completed clean run")
		return
	}

	// Failed run: transition to Blocked first (the atomic CAS is the
	// idempotency gate), then stamp the non-secret reason ONLY if this signal
	// actually won the transition. Writing the reason unconditionally first
	// would stamp a stale "run failed" reason onto a session a concurrent signal
	// had already advanced out of ImplementingPlan.
	advanced, err := l.sessions.UpdateStateConditional(ctx, sessionID, int(machine.Blocked), int(machine.ImplementingPlan))
	if err != nil {
		l.logger.Error().Err(err).Str("session", sessionID).Msg("headless finalize: transition to blocked failed")
		return
	}
	if !advanced {
		return
	}
	reason := headlessRunBlockedReason(exitError)
	reasonPtr := &reason
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{BlockedReason: &reasonPtr}); err != nil {
		l.logger.Error().Err(err).Str("session", sessionID).Msg("headless finalize: persist blocked reason failed")
	}
}

// notifyCronCompletionIfUnattended routes an unattended session — cron-scheduled
// or tmux_unattended (e.g. /boss-epic) — to the cron completion gate. The gate
// re-checks isUnattendedSession and runIsOver, so this pre-filter only avoids a
// pointless dispatch for interactive sessions; it must admit the same set the
// gate does or tmux_unattended sessions never finalize.
func (l *Lifecycle) notifyCronCompletionIfUnattended(ctx context.Context, sessionID string) {
	if sessionID == "" || l.cronCompletionNotifier == nil || l.sessions == nil {
		return
	}

	session, err := l.sessions.Get(ctx, sessionID)
	if err != nil {
		l.logger.Warn().Err(err).Str("session", sessionID).Msg("poll fallback completion: failed to load session")
		return
	}
	if !isUnattendedSession(session) {
		return
	}

	l.cronCompletionNotifier.NotifyCronAgentStopped(sessionID)
}

// SetDaemonCtx retains compatibility with older daemon wiring.
func (l *Lifecycle) SetDaemonCtx(ctx context.Context) { l.daemonCtx = ctx }

func (l *Lifecycle) armTmuxCompletionForHooklessRun(sessionID, agentSessionID, repoID string) {
	l.armTmuxCompletionForHooklessTmux(sessionID, agentSessionID, tmux.ChatSessionName(repoID, agentSessionID))
}

// chatPanePointerCleared reports whether the chat identified by agentSessionID
// currently has no persisted tmux pane pointer — the durable signal that its
// pane was torn down on purpose rather than dying with its agent.
//
// A non-nil error means the answer is UNKNOWN and the caller must defer rather
// than pick a default. The error is returned rather than folded into a second
// bool so the caller can report WHY it deferred: this poller is the only reader,
// it re-checks every tick, and a row that stays unreadable defers forever — a
// state nobody can diagnose from a bare "unknown".
//
// A chat row that is genuinely gone (ErrAgentChatNotFound) is known-not-parked:
// there is no row left to park, so the historical "pane gone means run over"
// inference is all that remains. The same applies when no chat store is wired.
func (l *Lifecycle) chatPanePointerCleared(ctx context.Context, agentSessionID string) (bool, error) {
	if l.agentChats == nil || agentSessionID == "" {
		return false, nil
	}
	chat, err := l.agentChats.GetByAgentSessionID(ctx, agentSessionID)
	if errors.Is(err, db.ErrAgentChatNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read chat for agent session %s: %w", agentSessionID, err)
	}
	if chat == nil {
		return false, nil
	}
	return chat.TmuxSessionName == nil || *chat.TmuxSessionName == "", nil
}

func (l *Lifecycle) armTmuxCompletionForHooklessTmux(sessionID, agentSessionID, tmuxName string) {
	if l.tmux == nil || (l.pollCompleter == nil && l.cronCompletionNotifier == nil) {
		return
	}
	ctx := l.daemonCtx
	if ctx == nil {
		ctx = context.Background()
	}
	interval := l.tmuxCompletionPollInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}

	safego.Go(l.logger, func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			alive, err := l.tmux.HasSessionStatus(ctx, tmuxName)
			if err != nil {
				l.logger.Warn().Err(err).
					Str("session", sessionID).
					Str("agent_session", agentSessionID).
					Str("tmux_session", tmuxName).
					Msg("hookless unattended tmux completion poll failed")
			} else if !alive {
				// A missing pane only means "the agent exited" while the chat
				// still carries its pane pointer. Every deliberate teardown
				// (KillChatTmuxSession) kills the pane and then clears
				// agent_chats.tmux_session_name, so a cleared pointer is a
				// reap, not a death, and must not finalize the run. tmuxName
				// was captured when this poll was armed and cannot observe a
				// later clear, so re-read the row before concluding (BOS-884).
				// The read costs nothing per tick: it happens once, only at the
				// moment the poller would otherwise signal.
				cleared, readErr := l.chatPanePointerCleared(ctx, agentSessionID)
				switch {
				case readErr != nil:
					// Unreadable row: ambiguous, so defer to the next tick
					// rather than guess, matching loglessTmuxCompletionEvidence
					// where a probe error also defers finalization.
					l.logger.Warn().Err(readErr).
						Str("session", sessionID).
						Str("agent_session", agentSessionID).
						Str("tmux_session", tmuxName).
						Msg("hookless unattended tmux completion poll: pane gone but chat row unreadable; deferring")
				case cleared:
					l.logger.Info().
						Str("session", sessionID).
						Str("agent_session", agentSessionID).
						Str("tmux_session", tmuxName).
						Msg("hookless unattended tmux completion poll: pane deliberately reaped (pointer cleared); not signalling run complete")
					return
				default:
					l.SignalSessionRunComplete(sessionID, agentSessionID, "")
					return
				}
			}

			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})
}

// shouldArmHeadlessPollFallback reports whether a non-cron headless run needs
// the ExitStatus poll fallback armed. It is armed exactly when the run took the
// headless StartByAgent branch, is not cron-spawned (the cron gate owns those),
// produced an agent session id, and the agent is NOT driven by a live Stop hook.
// A nil hookResp means the ConfigureFinalizeHook probe never ran (no HookToken),
// so no Stop hook was installed — treat that as "no competing hook" and arm.
// A non-nil hookResp reporting IsSupported means a hook will drive completion,
// so the poll fallback must NOT arm (it would double-finalize).
func (l *Lifecycle) shouldArmHeadlessPollFallback(headlessRun bool, cronJobID, agentSessionID string, hookResp *bossanovav1.ConfigureFinalizeHookResponse) bool {
	if !headlessRun || cronJobID != "" || agentSessionID == "" {
		return false
	}
	return hookResp == nil || !hookResp.GetIsSupported()
}

// armHeadlessPollFallback wires PollFallback for a non-cron headless run so its
// completion drives the session out of ImplementingPlan. The poller outlives
// the StartSession request, so it uses a non-cancelable base context (the
// daemon ctx when set). nil-guards mirror the tmux arm guards: a missing
// pollArmer or agent client is logged at debug and skipped.
// Reports whether the arm actually happened. That matters beyond logging: the
// armed poller is what eventually calls SignalSessionRunComplete, which is the
// only thing that releases a headless run's question-hook token (BOS-486). A
// caller that minted one uses this to decide whether completion will ever be
// signalled, rather than assuming it from reaching this line.
func (l *Lifecycle) armHeadlessPollFallback(sessionID, agentSessionID string, session *models.Session) bool {
	if l.pollArmer == nil {
		l.logger.Debug().Str("session", sessionID).Msg("headless poll fallback: no pollArmer configured; skipping")
		return false
	}
	client, err := l.agentClientFor(session)
	if err != nil || client == nil {
		l.logger.Debug().Err(err).Str("session", sessionID).Msg("headless poll fallback: no agent client; skipping")
		return false
	}
	ctx := l.daemonCtx
	if ctx == nil {
		ctx = context.Background()
	}
	l.pollArmer.Arm(ctx, sessionID, agentSessionID, client)
	return true
}

// SetAgentLogsDir records the bossd-owned directory where agent plugins
// write their tmux-hosted interactive run logs. Mirrors the same field
// on HostServiceServer so StartTmuxChat can pass a deterministic
// log_path into BuildInteractiveCommand. Called from the daemon
// entrypoint after MkdirAll succeeds, before any session that spawns a
// tmux chat is started. An empty string leaves StartTmuxChat in a
// fail-closed state.
//
// Concurrency: called exactly once during daemon startup, before serving
// begins. Not safe for concurrent re-injection alongside in-flight RPCs.
func (l *Lifecycle) SetAgentLogsDir(dir string) { l.agentLogsDir = dir }

// agentClientFor returns the registered AgentRunnerClient for sess.AgentName.
// Returns an error wrapping agent.ErrAgentNotLoaded when no client matches —
// defense in depth against an AgentName the daemon was never configured for.
// CreateSession is expected to resolve AgentName before persistence, so an
// empty AgentName here indicates a stale row from before the multi-agent
// migration; the error names that case explicitly so operators can fix the
// data, and callers can use errors.Is to distinguish this from real RPC
// failures.
func (l *Lifecycle) agentClientFor(sess *models.Session) (agent.AgentRunnerClient, error) {
	if c, ok := l.agents[sess.AgentName]; ok && c != nil {
		return c, nil
	}
	return nil, fmt.Errorf("agent %q not loaded for session %s: %w", sess.AgentName, sess.ID, agent.ErrAgentNotLoaded)
}

// clientForAgentName returns the registered AgentRunnerClient for a bare agent
// name (BOS-381: a chat can run a different provider than its parent session, so
// spawn paths that resolve the chat's provider dispatch by the chat's agent, not
// the session's). Mirrors agentClientFor's not-loaded error.
func (l *Lifecycle) clientForAgentName(agentName string) (agent.AgentRunnerClient, error) {
	if c, ok := l.agents[agentName]; ok && c != nil {
		return c, nil
	}
	return nil, fmt.Errorf("agent %q not loaded: %w", agentName, agent.ErrAgentNotLoaded)
}

// primaryChatForSession returns the session's primary agent chat — the chat
// whose agent_session_id matches the session's AgentSessionID (the first chat,
// created from the create-session seed). Returns nil (never an error) when the
// session has no primary chat yet (first-start, before the row is persisted) or
// the lookup fails, so callers fall back to the session's own mirrored
// provider/account/model fields. BOS-381: provider/account/model authority lives
// on the chat; restart/rotation/resume paths that hold only a session resolve it
// here.
func (l *Lifecycle) primaryChatForSession(ctx context.Context, sess *models.Session) *models.AgentChat {
	if sess == nil || l.agentChats == nil || sess.AgentSessionID == nil || *sess.AgentSessionID == "" {
		return nil
	}
	chat, err := l.agentChats.GetByAgentSessionID(ctx, *sess.AgentSessionID)
	if err != nil || chat == nil {
		return nil
	}
	return chat
}

// effectiveSpawnSession returns a shallow copy of sess with AgentName, Model, and
// AccountID overridden from its primary chat (BOS-381), for restart/rotation/
// resume spawn paths that hold only a session. Returns sess unchanged when no
// primary chat exists yet (first-start seed) so the seed on the session still
// governs the very first spawn. Only the three authority fields are copied; a
// chat that never bound its own account (nil AccountID) or model ("") inherits
// the session's mirrored value, preserving the same-provider common case.
func (l *Lifecycle) effectiveSpawnSession(ctx context.Context, sess *models.Session) *models.Session {
	chat := l.primaryChatForSession(ctx, sess)
	if chat == nil {
		return sess
	}
	eff := *sess
	if chat.AgentName != "" {
		eff.AgentName = chat.AgentName
	}
	if chat.Model != "" {
		eff.Model = chat.Model
	}
	if chat.AccountID != nil {
		eff.AccountID = chat.AccountID
	}
	return &eff
}

// NewLifecycle creates a new session lifecycle orchestrator. cronJobs may be
// nil for callers that never spawn cron-linked sessions (tests, legacy flows);
// FinalizeSession requires it and will error if it's absent.
func NewLifecycle(
	sessions db.SessionStore,
	repos db.RepoStore,
	agentChats db.AgentChatStore,
	cronJobs db.CronJobStore,
	worktrees gitpkg.WorktreeManager,
	agentRunner agent.AgentDispatcher,
	tmux *tmux.Client,
	provider vcs.Provider,
	logger zerolog.Logger,
) *Lifecycle {
	return &Lifecycle{
		sessions:                   sessions,
		repos:                      repos,
		agentChats:                 agentChats,
		cronJobs:                   cronJobs,
		worktrees:                  worktrees,
		agentRunner:                agentRunner,
		tmux:                       tmux,
		provider:                   provider,
		logger:                     logger,
		newTmuxChatAgentSessionID:  uuid.NewString,
		startedAt:                  time.Now(),
		tmuxCompletionPollInterval: 2 * time.Second,
		headlessStatusPollInterval: 2 * time.Second,
		// Default to a hermetic no-op resolver so unit tests never open the real
		// OS keyring (the Linux SecretService/dbus backend leaks a connection
		// goroutine per open). Production wires the real keyring resolver via
		// SetProofEnvResolver in cmd/bossd's daemon startup.
		proofEnv: proofenv.NewNoop(),
	}
}

// StartSessionOpts bundles the optional inputs to StartSession. Each field
// has a zero-value default that preserves the historical behavior, so
// callers only need to populate the fields they care about.
type StartSessionOpts struct {
	// ExistingBranch, when non-empty, makes the worktree check out that
	// branch instead of creating a fresh one (used for existing PR sessions).
	// Every caller today sets it only alongside a PR number; this path fetches
	// the head branch but not the base, and createDraftPR relies on that
	// pairing without enforcing it. Read the SkipFetch rationale in
	// createDraftPR before setting this from a new call site.
	ExistingBranch string

	// ForceBranch removes any pre-existing branch with the derived name
	// before creating the new worktree.
	ForceBranch bool

	// SkipSetupScript bypasses the repo's configured setup script
	// (e.g. for dependabot PRs that should not run user code).
	SkipSetupScript bool

	// SetupOutput receives streamed setup-script output, when non-nil.
	SetupOutput io.Writer

	// DeferPR skips the immediate draft-PR creation that StartSession
	// otherwise performs for sessions without a PR. The Stop-hook
	// finalize path is responsible for calling EnsurePR later.
	DeferPR bool

	// CronJobID, when non-empty, marks this session as cron-spawned
	// (persisted on the session record once the schema/store land).
	CronJobID string

	// HookToken, when non-empty, is the secret written into the
	// worktree's settings.local.json so the Stop hook can authenticate
	// to the bossd hook server. Plumbed through in flight leg 5.
	HookToken string

	// BranchName, when non-empty, overrides the default title-derived
	// branch name. Used by the cron path so each fire gets a unique
	// branch (e.g. cron-<slug>-<unix>) and a previous run's orphaned
	// branch can't trip ErrBranchExists on the next fire. Ignored when
	// ExistingBranch is set.
	BranchName string

	// Detach runs the session's initial agent pass headlessly (codex exec /
	// claude --print). Set by `boss new --detach`. The zero value (false)
	// leaves an interactive session idle: no headless run fires and the
	// agent starts on first attach, exactly like a tracker-sourced session.
	// This is what distinguishes a `--detach` run (which carries a prompt
	// and wants an autonomous pass) from a plain interactive `boss new`.
	Detach bool

	// HeadlessCapabilityProfile is an explicit operation-surface requirement
	// for this initial panel-less launch. UNSPECIFIED preserves the historical
	// StartByAgent call exactly; callers must opt in and must not infer it from
	// the plan text or a command name.
	HeadlessCapabilityProfile bossanovav1.HeadlessCapabilityProfile

	// IsTmuxUnattended routes this session through the durable tmux-hosted path
	// (like a cron session) instead of the headless detach path, and is
	// persisted so the completion gate and restart re-adoption recognise it.
	IsTmuxUnattended bool

	// ZeroOutput runs a cron job from the repository checkout without a
	// worktree, branch, draft PR, setup script, or Stop-hook file.
	//
	// ZeroOutput is the JOB-LEVEL declaration and models.Session.IsQuickChat is
	// its SESSION-LEVEL realization — they are two layers of one concept, not two
	// spellings of it, so do not converge them and do not add a third. ZeroOutput
	// says "run StartSession, but skip its repo-mutating steps": it is a persisted
	// cron_jobs column (CronJob.IsZeroOutput) surfaced through the TUI cron form,
	// MCP create_cron_job, the web UI and the cron proto messages, and it is read
	// INSIDE StartSession (below) to short-circuit worktree, setup script, branch
	// and PR. IsQuickChat says "skip StartSession entirely; there is no worktree"
	// — the daemon routes such a create to StartQuickChatSession instead.
	//
	// The two already converge in the one direction that matters: setting
	// ZeroOutput sets the session's IsQuickChat (see below), so finalize reads
	// exactly one session-level truth (isPlanningOnlyNoChangeSession) and a
	// zero-output cron run finalizes benignly rather than as a failed run.
	ZeroOutput bool
}

func writeSetupProgress(w io.Writer, text string) {
	if w == nil {
		return
	}
	_, _ = fmt.Fprintln(w, text)
}

// BootstrapTimeout is the overall deadline for a session bootstrap — everything
// StartSession does between the session row existing and the agent running:
// fetch, branch probe, worktree add, setup script, agent start, hook config.
//
// It is deliberately double gitpkg.SetupScriptTimeout (5 minutes), the largest
// single step inside it, leaving five further minutes for steps that each take
// seconds in normal operation. The value is not meant to be tight: a bootstrap
// still running after ten minutes is not slow, it is stuck. What matters is that
// the bootstrap has *a* bound at all — before BOS-717 it had none, so one that
// blocked inside Create() ran forever, held the process-global start lock, and
// wedged every subsequent CreateSession in the daemon.
//
// It is also what bounds how long a create for the same target can be made to
// wait (TargetStartLockTimeout), and what makes reaping a pre-agent row safe
// (bootstrapReapThreshold): past this deadline a live create path would already
// have failed itself.
//
// DERIVED from gitpkg.SetupScriptTimeout rather than restating the ten minutes,
// so "double the largest single step" stays true if that step's budget ever
// moves — the same reason TargetStartLockTimeout and bootstrapReapThreshold are
// computed from this constant rather than written out.
const BootstrapTimeout = 2 * gitpkg.SetupScriptTimeout

// SetBootstrapTimeout overrides BootstrapTimeout for this lifecycle. Zero
// restores the constant. Tests use it to exercise the deadline path in
// milliseconds; production leaves it unset.
func (l *Lifecycle) SetBootstrapTimeout(d time.Duration) { l.bootstrapTimeout = d }

// bootstrapDeadline is the effective bootstrap budget: the override when set,
// otherwise BootstrapTimeout.
func (l *Lifecycle) bootstrapDeadline() time.Duration {
	if l.bootstrapTimeout > 0 {
		return l.bootstrapTimeout
	}
	return BootstrapTimeout
}

// recordCreatedWorktree returns the gitpkg.CreateOpts.OnWorktreeReady hook that
// persists what a fresh-branch create just made, as soon as `git worktree add`
// has landed and BEFORE the setup script runs (BOS-717).
//
// Two things go wrong without it, both on the shape the 2026-08-06 incident took
// (a plain `boss new` / TUI new session, killed while the setup script hung):
//
//   - The row has no branch to clean up. A title-only create's branch is derived
//     inside Create (sanitized title, plus a uniquifying suffix), so branch_name
//     stays empty until StartSession returns. Neither StreamCreateSession's
//     failure cleanup nor the stranded-bootstrap reaper could name the worktree
//     and branch left on disk — both key on branch_name.
//   - The row has no PROOF the branch is ours to delete. branch_name alone is not
//     that proof: it is written at insert time from the request, before Create
//     has checked whether such a branch already exists. worktree_path is, because
//     it is only ever written after the add succeeded — which is exactly the
//     rule cleanupFailedCreateSession applies.
//
// Wired on every StartSession arm that *creates* a branch rather than checking
// one out: the fresh-branch arm chosen by `existingBranch == ""`, and the
// fallback Create the PR/existing-branch arm runs when its checkout fails. Both
// go through gitpkg.Create, whose `worktree add -b` either makes the branch or
// fails with ErrBranchExists, so the hook only ever fires for a branch this
// attempt made — which is what makes it ownership proof rather than a name.
//
// CreateFromExistingBranch itself is never handed the hook: it keeps the head
// branch it was given, so a dependabot head branch can never be made to look
// like something this attempt created. Leaving the fallback unhooked was not
// part of that protection but a gap in it — the fallback's branch *is* ours,
// and without the recording a failed bootstrap purges the worktree, refuses to
// reap the branch for want of proof, deletes the row, and strands the branch
// where the next retry collides with it (ErrBranchExists).
//
// The write runs on a detached, short-deadline context: the caller's bootstrap
// context may be a moment from expiring, and recording what to clean up must not
// be the thing that expires with it.
func (l *Lifecycle) recordCreatedWorktree(sessionID string) func(context.Context, string, string) {
	return func(ctx context.Context, branch, worktreePath string) {
		if branch == "" || worktreePath == "" {
			return
		}
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), worktreeRecordTimeout)
		defer cancel()
		if _, err := l.sessions.Update(writeCtx, sessionID, db.UpdateSessionParams{
			BranchName:   &branch,
			WorktreePath: &worktreePath,
		}); err != nil {
			// Non-fatal: the bootstrap continues and StartSession's own update
			// records both on the success path. What is lost is only the early
			// cleanup handle, so say so rather than failing the create.
			l.logger.Warn().Err(err).
				Str("session", sessionID).
				Str("branch", branch).
				Msg("could not record the created worktree early; a failed bootstrap may leave it orphaned")
		}
	}
}

// worktreeRecordTimeout bounds recordCreatedWorktree's single local DB write.
const worktreeRecordTimeout = 10 * time.Second

// spawnedRunStopTimeout bounds the stop of a run abandoned by a failed
// bootstrap. Generous relative to the work (a signal and a tmux kill) because
// the alternative to waiting is the orphan this exists to prevent.
const spawnedRunStopTimeout = 30 * time.Second

// stopRunAbandonedByFailedBootstrap stops an agent run this bootstrap already
// spawned, when a LATER step of the same bootstrap failed (BOS-717).
//
// The bootstrap deadline makes this reachable rather than theoretical. Once
// StartSession has spawned an agent, several writes still follow — the
// AgentSessionID update, the agent_chats insert, the AgentStarted transition,
// the ImplementingPlan write — and the deadline can expire across any of them.
// A spawn that succeeded a moment before the deadline therefore returns an error
// from a run that is already alive.
//
// Both create callers treat that error as a failed bootstrap: they purge the
// worktree and delete the row. The spawned process does NOT go with it. The
// claude and codex plugins deliberately detach it from the RPC context so a run
// survives its create call, so nothing cancels it — leaving a coding agent
// running against a worktree that is being deleted underneath it, with no row
// left to find it by.
//
// The stop runs on a context DETACHED from the caller's, for the obvious reason:
// the context that just expired is the one that made this necessary.
//
// stopCtx has to reach the plugin RPC, not merely exist. This runs from a defer
// on StartSession's exit, so a stop that blocks blocks StartSession — which
// holds the per-target lock across its whole failure cleanup. An unresponsive
// plugin would therefore wedge the create path for that target indefinitely and
// leave the half-started row and worktree uncleaned: the daemon-wedge shape this
// ticket exists to remove, reintroduced by the cleanup meant to prevent an
// orphan. AgentRunner.Stop takes no context and PluginRunner issued StopRun on
// context.Background(), so the bound is carried by ContextualStopper, which
// Dispatcher.StopByAgent prefers for every production runner.
//
// The stop is attempted rather than gated on IsRunningByAgent, unlike
// StopSession. A runner that under-reports a just-spawned detached process would
// turn that guard into the leak this function exists to close, and the cost of
// being wrong the other way is one error log for a run that had already exited.
func (l *Lifecycle) stopRunAbandonedByFailedBootstrap(ctx context.Context, sessionID, agentName, runID string, tmuxHosted bool) {
	if runID == "" || l.agentRunner == nil {
		return
	}
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), spawnedRunStopTimeout)
	defer cancel()

	l.logger.Warn().
		Str("session", sessionID).
		Str("claudeSession", runID).
		Msg("bootstrap failed after the agent started; stopping the spawned run so it cannot outlive the session")

	if err := l.agentRunner.StopByAgent(stopCtx, agentName, runID); err != nil {
		l.logger.Warn().Err(err).
			Str("session", sessionID).
			Str("claudeSession", runID).
			Msg("could not stop the run abandoned by a failed bootstrap; it may still be running against a deleted worktree")
	}

	// A tmux-hosted run is owned by the tmux server, not by this process, so
	// stopping the agent does not take its pane with it (StopSession kills the
	// two separately for the same reason).
	if tmuxHosted && l.tmux != nil && l.agentChats != nil {
		l.killAllChatTmuxSessions(stopCtx, sessionID)
	}
}

// CleanUpFailedBootstrapArtifacts reclaims the worktree and branch a failed
// bootstrap left on disk, for a caller that is about to discard the session row
// (BOS-717).
//
// StreamCreateSession has always done this; the task-orchestrator create path
// (cron, dependabot, /boss-epic) never did — it deleted the row and left
// whatever was on disk. That was survivable while a wedged bootstrap simply
// hung: it never reached the failure path at all. The bootstrap deadline makes
// it routine, so AC 2's "cleans up its worktree and branch" now has to hold on
// this path too.
//
// The ownership rule it applies lives in CleanUpBootstrapArtifacts, which the
// server's own failed-create cleanup calls too — one copy of a policy that must
// not diverge between the two entry points onto session creation.
//
// Best-effort throughout: a failure to clean up is logged, never returned. The
// caller's job is to report why the bootstrap failed, not why the mop-up did.
func (l *Lifecycle) CleanUpFailedBootstrapArtifacts(ctx context.Context, sessionID string) {
	// A background draft-PR step may still be pushing this branch (BOS-540);
	// stop it before deleting the branch out from under it. Ahead of the
	// wiring guard below, because cancelling an in-flight push is worth doing
	// even on a lifecycle with no worktree manager to clean up with.
	l.StopBackgroundDraftPR(ctx, sessionID)
	CleanUpBootstrapArtifacts(ctx, l.logger, l.sessions, l.repos, l.worktrees, sessionID)
}

// CleanUpBootstrapArtifacts reclaims the worktree and branch a failed bootstrap
// left on disk. It is the ONE implementation of that policy: the daemon has two
// entry points onto session creation (the interactive StreamCreateSession and
// the task orchestrator), both must mop up identically, and the rule below is
// subtle enough that two copies would drift into one of them force-deleting a
// user's branch.
//
// The branch is deleted ONLY when the row carries a worktree path, which is
// written only after `git worktree add` succeeded. branch_name alone proves
// nothing — it comes from the request, before Create ever checked whether such a
// branch already existed, so reaping on it would `git branch -D` a pre-existing
// branch (a Linear suggested branch, a dependabot PR head) and its unpushed
// commits when a create failed early. The worktree DIRECTORY is removed either
// way, so a stale directory cannot wedge the next attempt.
//
// Purge before reaping, never the other way round: `git branch -D` refuses a
// branch still checked out in a registered worktree, and ReapLocalBranches'
// `worktree prune` does not help, because prune only unregisters worktrees whose
// directory is already gone — exactly not the case here.
//
// Which is also why a purge that did not RUN suppresses the reap rather than
// letting it proceed. A purge skipped for a contended clone leaves the worktree
// registered, so the reap that follows is refused — and reaping anyway buys
// nothing but a generic delete failure in the log. Half-applying the ordering is
// worse than not applying it.
//
// Neither half is deferred, and the log says so rather than promising a retry.
// Both callers delete the session row immediately after this returns, so nothing
// names the branch or the worktree afterwards.
//
// It is tempting to believe the next create for this repo and branch re-clears
// the directory. It does not, on the path that matters: a suppressed reap leaves
// the BRANCH alive, so gitpkg.Manager.Create's uniquifier settles the next
// same-titled create on `<branch>-2` and derives its worktree path from THAT —
// clearStaleWorktree then clears the -2 path and never touches this one. (An
// explicit branch name gets ErrBranchExists before a path is computed at all.)
// Only a Force create or CreateFromExistingBranch, neither of which uniquifies,
// returns to this exact path. So both halves are abandoned here, and reclaiming
// them is a manual act — which is why this is logged at Error and names them.
//
// It does NOT stop a background draft-PR step; that is the caller's to do
// (l.StopBackgroundDraftPR / s.lifecycle), and doing it twice is pointless.
// Best-effort: every failure is logged, none is returned.
func CleanUpBootstrapArtifacts(
	ctx context.Context,
	logger zerolog.Logger,
	sessions db.SessionStore,
	repos db.RepoStore,
	worktrees gitpkg.WorktreeManager,
	sessionID string,
) {
	if worktrees == nil || repos == nil || sessions == nil {
		return
	}

	sess, err := sessions.Get(ctx, sessionID)
	if err != nil {
		// Distinct from the nothing-to-clean-up cases below: a store read that
		// FAILED leaves artifacts on disk that nobody will come back for, and
		// swallowing it into the same bare return is how that goes unnoticed.
		logger.Warn().Err(err).Str("session", sessionID).
			Msg("failed-bootstrap cleanup: get session failed; any worktree or branch it left is orphaned")
		return
	}
	if sess == nil || sess.RepoID == "" || sess.BranchName == "" {
		return
	}
	repo, err := repos.Get(ctx, sess.RepoID)
	if err != nil || repo == nil {
		logger.Warn().Err(err).Str("session", sessionID).
			Msg("failed-bootstrap cleanup: get repo failed")
		return
	}
	if err := worktrees.PurgeWorktree(ctx, repo.LocalPath, repo.DisplayName, repo.WorktreeBaseDir, sess.BranchName); err != nil {
		// The purge did not run, so the worktree is still registered and
		// `git branch -D` would refuse the branch. Skip both halves, and say
		// they are abandoned rather than deferred (see the ordering note above).
		//
		// What is named depends on what this cleanup was entitled to touch. With
		// no recorded worktree path the branch was never ours to reap — it is a
		// name from the request, possibly a pre-existing branch carrying unpushed
		// work, which the ownership rule above deliberately protects. Naming it
		// as abandoned would invite an operator to delete exactly that.
		event := logger.Error().Err(err).
			Str("session", sessionID).
			Str("repo", repo.LocalPath)
		if sess.WorktreePath != "" {
			event.Str("branch", sess.BranchName).
				Str("worktree", sess.WorktreePath).
				Msg("failed-bootstrap cleanup: worktree purge did not run; abandoning this session's worktree and branch — the row is about to be deleted, so nothing names them again")
		} else {
			event.Str("worktree_branch", sess.BranchName).
				Msg("failed-bootstrap cleanup: worktree purge did not run; abandoning this session's worktree directory — the row is about to be deleted. The branch is left alone: this create never recorded a worktree, so it never owned it")
		}
		return
	}
	if sess.WorktreePath != "" {
		if err := worktrees.ReapLocalBranches(ctx, repo.LocalPath, []string{sess.BranchName}); err != nil {
			// Logged rather than discarded: a silently refused delete is how an
			// orphaned branch goes unnoticed.
			logger.Warn().Err(err).Str("session", sessionID).Str("branch", sess.BranchName).
				Msg("failed-bootstrap cleanup: delete branch failed")
		}
	}
}

// StartSession creates a worktree, starts a Claude process, and fires
// state machine events. It updates the session record with the worktree
// path, branch name, and Claude session ID.
//
// See StartSessionOpts for how to customize behavior. The zero-value opts
// preserve historical defaults: a fresh branch, setup script enabled,
// and an immediate draft PR for sessions without one.
//
// The whole bootstrap runs under a bootstrapDeadline context (BOS-717). Work
// that must outlive the call already detaches from this context explicitly:
// the background draft-PR step runs on context.WithoutCancel, the hookless tmux
// completion poller on the daemon context, and the headless status watcher on
// none at all.
func (l *Lifecycle) StartSession(ctx context.Context, sessionID string, opts StartSessionOpts) (retErr error) {
	ctx, cancel := context.WithTimeout(ctx, l.bootstrapDeadline())
	defer cancel()

	existingBranch := opts.ExistingBranch
	forceBranch := opts.ForceBranch
	skipSetupScript := opts.SkipSetupScript
	setupOutput := opts.SetupOutput
	session, err := l.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}

	repo, err := l.repos.Get(ctx, session.RepoID)
	if err != nil {
		return fmt.Errorf("get repo: %w", err)
	}

	// Derive the headless operation surface this launch requires, unless the
	// caller demanded one explicitly (an explicit value always wins untouched).
	// Applying the policy here — the single seam every launch passes through —
	// covers both production callers at once and keeps a future third caller
	// correct by construction, without adding a client-settable RPC field.
	// opts is a by-value copy, so this mutation is local to this launch.
	resolvedAgentName := l.resolveAgentName(session.AgentName)
	if opts.HeadlessCapabilityProfile == bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_UNSPECIFIED {
		opts.HeadlessCapabilityProfile = headlessCapabilityProfileFor(resolvedAgentName, opts)
	}

	// A `boss new --detach` run is hosted in a durable tmux pane when tmux is
	// available, so it survives a daemon restart and is re-monitored on boot
	// (exactly like cron / tmux_unattended) instead of being orphaned. When tmux
	// is unavailable it falls back to the paneless headless path, which still
	// orphans on restart — so detachViaTmux (and thus the persisted Detach flag)
	// is set ONLY on the successful tmux-hosted branch. tmuxHosted unifies every
	// durable-tmux provenance for the routing/hook/liveness decisions below.
	detachViaTmux := opts.Detach && l.tmux != nil && l.tmux.Available(ctx)
	tmuxHosted := opts.CronJobID != "" || opts.IsTmuxUnattended || detachViaTmux

	// Initialize state machine at CreatingWorktree.
	sm := machine.New(machine.CreatingWorktree)

	// Update session state to CreatingWorktree and stamp the cron_job_id
	// when the cron scheduler spawned us. The cron linkage is set here
	// (rather than by the task orchestrator) so it's guaranteed to land
	// before any finalize path observes the row.
	creatingState := int(machine.CreatingWorktree)
	updateParams := db.UpdateSessionParams{
		State: &creatingState,
	}
	if opts.CronJobID != "" {
		cronJobID := &opts.CronJobID
		updateParams.CronJobID = &cronJobID
	}
	if opts.HookToken != "" && tmuxHosted && !opts.ZeroOutput {
		hookToken := &opts.HookToken
		updateParams.HookToken = &hookToken
	}
	if opts.IsTmuxUnattended {
		tmuxUnattended := true
		updateParams.IsTmuxUnattended = &tmuxUnattended
	}
	if opts.ZeroOutput {
		quickChat := true
		updateParams.IsQuickChat = &quickChat
	}
	// Persist Detach only on the durable tmux-hosted branch: a `--detach` run
	// that fell back to the paneless headless path (tmux unavailable) must stay
	// in the headless class so it still orphans on restart (there is genuinely no
	// live process to recover), so it leaves Detach unset (nil → stays false).
	if detachViaTmux {
		detach := true
		updateParams.Detach = &detach
	}
	if _, err := l.sessions.Update(ctx, sessionID, updateParams); err != nil {
		return fmt.Errorf("set creating_worktree state: %w", err)
	}

	if opts.ZeroOutput {
		l.logger.Info().Str("session", sessionID).Str("repo", repo.LocalPath).
			Msg("starting zero-output cron session without worktree")
	} else {
		l.logger.Info().Str("session", sessionID).Str("repo", repo.LocalPath).Msg("creating worktree")
		writeSetupProgress(setupOutput, "creating worktree")
	}

	// Determine setup script — skip it when the flag is set (e.g. dependabot PRs).
	setupScript := repo.SetupScript
	if skipSetupScript || opts.ZeroOutput {
		setupScript = nil
	}

	// Surface the "initializing" display status for the lifetime of the
	// pre-chat creation window when a setup script will actually run. The
	// flag is transient/in-memory; defer guarantees it clears on every
	// return path (incl. a failed Create) so it can never be stranded.
	if l.settingUpTracker != nil && setupScript != nil && strings.TrimSpace(*setupScript) != "" {
		l.settingUpTracker.SetSettingUp(sessionID, true)
		defer l.settingUpTracker.SetSettingUp(sessionID, false)
	}

	// Create worktree: existing branch (PR) or new branch.
	worktreeStarted := time.Now()
	worktreeResult := "created_fresh"
	createdFreshBranch := !opts.ZeroOutput && existingBranch == ""
	var result *gitpkg.CreateResult
	if opts.ZeroOutput {
		worktreeResult = "zero_output"
		result = &gitpkg.CreateResult{WorktreePath: repo.LocalPath}
	} else if existingBranch != "" {
		worktreeResult = "checked_out_existing"
		result, err = l.worktrees.CreateFromExistingBranch(ctx, gitpkg.CreateFromExistingBranchOpts{
			RepoPath:          repo.LocalPath,
			BranchName:        existingBranch,
			WorktreeBaseDir:   repo.WorktreeBaseDir,
			RepoName:          repo.DisplayName,
			SetupScript:       setupScript,
			SetupScriptOutput: setupOutput,
		})
		if err != nil {
			// Preserve the existing fallback for any checkout failure. The
			// worktree manager does not expose a typed missing-remote error, so
			// report the failure neutrally instead of guessing its cause.
			l.logger.Info().
				Str("branch", existingBranch).
				Str("result", "existing_checkout_failed").
				Dur("worktree_duration", time.Since(worktreeStarted)).
				Err(err).
				Msg("existing branch checkout failed; creating fresh branch")
			worktreeResult = "created_fresh_after_existing_checkout_error"
			createdFreshBranch = true
			result, err = l.worktrees.Create(ctx, gitpkg.CreateOpts{
				RepoPath:          repo.LocalPath,
				BaseBranch:        session.BaseBranch,
				WorktreeBaseDir:   repo.WorktreeBaseDir,
				RepoName:          repo.DisplayName,
				Title:             session.Title,
				BranchName:        existingBranch,
				SetupScript:       setupScript,
				SetupScriptOutput: setupOutput,
				Force:             forceBranch,
				OnWorktreeReady:   l.recordCreatedWorktree(sessionID),
			})
		}
	} else {
		result, err = l.worktrees.Create(ctx, gitpkg.CreateOpts{
			RepoPath:          repo.LocalPath,
			BaseBranch:        session.BaseBranch,
			WorktreeBaseDir:   repo.WorktreeBaseDir,
			RepoName:          repo.DisplayName,
			Title:             session.Title,
			BranchName:        opts.BranchName,
			SetupScript:       setupScript,
			SetupScriptOutput: setupOutput,
			Force:             forceBranch,
			OnWorktreeReady:   l.recordCreatedWorktree(sessionID),
		})
	}
	if err != nil {
		return fmt.Errorf("create worktree: %w", err)
	}
	if !opts.ZeroOutput {
		l.logger.Info().
			Str("session", sessionID).
			Str("result", worktreeResult).
			Dur("worktree_duration", time.Since(worktreeStarted)).
			Dur("fetch_ms", result.FetchDuration).
			Dur("branch_probe_ms", result.BranchProbeDuration).
			Dur("worktree_add_ms", result.WorktreeAddDuration).
			Dur("setup_script_ms", result.SetupScriptDuration).
			Msg("worktree startup complete")
		writeSetupProgress(setupOutput, "worktree startup complete")
	}

	// A setup-script failure is non-fatal: the worktree is valid, so the
	// session still starts. Flag the degraded state — log it, stream the
	// reason to any connected client, and persist it for the TUI / `boss show`
	// — rather than aborting and tearing the worktree down.
	var setupErrStr string
	if result.SetupErr != nil {
		setupErrStr = result.SetupErr.Error()
		l.logger.Warn().
			Str("session", sessionID).
			Str("worktree", result.WorktreePath).
			Err(result.SetupErr).
			Msg("setup script failed; starting session in a degraded state")
		if setupOutput != nil {
			_, _ = fmt.Fprintf(setupOutput, "\n⚠ setup script failed; the session was created anyway:\n%s\n", setupErrStr)
		}
	}

	// Undo the artifacts this call created when a capability preflight rejects
	// the launch, so a rejected session leaves no worktree or branch behind for
	// the next attempt to collide with. Used by the post-setup probe below and
	// again for the authoritative probe inside startTmuxChat, which runs after
	// the worktree and branch have been persisted.
	rollbackPreflightFailure := func(preflightErr error) error {
		if opts.ZeroOutput {
			return preflightErr
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), preflightRollbackTimeout)
		defer cleanupCancel()
		if archiveErr := l.worktrees.Archive(cleanupCtx, result.WorktreePath); archiveErr != nil {
			return fmt.Errorf("%w; rollback worktree %q: %v", preflightErr, result.WorktreePath, archiveErr)
		}
		if createdFreshBranch {
			if branchErr := l.worktrees.DeleteLocalBranch(cleanupCtx, repo.LocalPath, result.BranchName); branchErr != nil {
				return fmt.Errorf("%w; rollback local branch %q: %v", preflightErr, result.BranchName, branchErr)
			}
		}
		return preflightErr
	}

	// Validate after setup against the environment this launch will inherit. A
	// target branch or setup script may create or replace .env (including
	// CODEX_HOME / HOME), so the registered checkout is not authoritative.
	// Roll the worktree back if preflight fails. Since BOS-717 a fresh-branch
	// create has already recorded it (recordCreatedWorktree), so the caller's
	// failure cleanup would reach it too — this rollback still runs first and
	// keeps the error message specific about what it undid.
	if opts.HeadlessCapabilityProfile != bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_UNSPECIFIED {
		dispatcher, ok := l.agentRunner.(agent.HeadlessCapabilityProfilePreflightDispatcher)
		if !ok {
			return rollbackPreflightFailure(fmt.Errorf("headless capability profile %s for agent %q requires profile-aware agent dispatcher", opts.HeadlessCapabilityProfile, resolvedAgentName))
		}
		preflightEnv := resolveWorktreeRelativeHomes(dotenv.OverlayWithRepo(mergeEnv(l.resolveAccountEnv(ctx, session), l.resolveProofEnv()), result.WorktreePath, repo), result.WorktreePath)
		if err := dispatcher.PreflightByAgentWithHeadlessCapabilityProfile(
			ctx, session.AgentName, result.WorktreePath, session.Model, preflightEnv, opts.HeadlessCapabilityProfile,
		); err != nil {
			return rollbackPreflightFailure(fmt.Errorf("preflight headless capabilities for agent %q with profile %s: %w", resolvedAgentName, opts.HeadlessCapabilityProfile, err))
		}
	}

	// Update session with worktree info (and any setup-script failure flag).
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		WorktreePath: &result.WorktreePath,
		BranchName:   &result.BranchName,
		SetupError:   &setupErrStr,
	}); err != nil {
		return fmt.Errorf("update worktree path: %w", err)
	}

	// Install the Stop-hook config for cron-spawned sessions. This must
	// happen after the setup script ran (otherwise a script-written
	// settings.local.json would be clobbered by a non-merge write
	// elsewhere) and before claude.Start (so Claude reads the config on
	// startup). Non-cron sessions have an empty HookToken and skip this
	// path entirely, preserving historical behaviour.
	var hookResp *bossanovav1.ConfigureFinalizeHookResponse
	if opts.HookToken != "" && tmuxHosted && !opts.ZeroOutput {
		if l.hookPort == 0 {
			return fmt.Errorf("hook port not configured: SetHookPort must be called before starting sessions with a HookToken")
		}
		client, err := l.agentClientFor(session)
		if err != nil {
			return fmt.Errorf("agent client not configured: %w; SetAgents must be called before starting sessions with a HookToken", err)
		}
		hookResp, err = client.ConfigureFinalizeHook(ctx, &bossanovav1.ConfigureFinalizeHookRequest{
			WorkDir:   result.WorktreePath,
			SessionId: sessionID,
			HookToken: opts.HookToken,
			HookPort:  clampInt32(l.hookPort),
		})
		if err != nil {
			return fmt.Errorf("configure finalize hook: %w", err)
		}
		if !hookResp.GetIsSupported() {
			l.logger.Info().Str("session", sessionID).Msg("agent does not support finalize hook")
		} else {
			l.logger.Info().
				Str("session", sessionID).
				Int("hookPort", l.hookPort).
				Msg("installed Stop-hook config in worktree")
		}
	}

	// Fire WorktreeCreated → StartingAgent.
	if err := sm.FireCtx(ctx, machine.WorktreeCreated); err != nil {
		return fmt.Errorf("fire worktree_created: %w", err)
	}

	startingState := int(machine.StartingAgent)
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		State: &startingState,
	}); err != nil {
		return fmt.Errorf("set starting_claude state: %w", err)
	}

	l.logger.Info().
		Str("session", sessionID).
		Str("worktree", result.WorktreePath).
		Msg("starting claude")

	// Start the agent. Cron-spawned sessions, tmux_unattended sessions (e.g.
	// /boss-epic's durable tmux-hosted runs), and `boss new --detach` runs with
	// tmux available (detachViaTmux) run in a tmux-hosted Claude UI so the user
	// can attach and — crucially — the pane is owned by the independent tmux
	// server, surviving a daemon restart to be re-monitored on boot instead of
	// orphaned (tmuxHosted). Interactive sessions (the TUI's new-PR / existing-PR
	// flows, and tracker-sourced Linear/Sentry sessions created WITHOUT detach)
	// are created idle: no agent runs yet, and it starts interactively on first
	// attach (RecordChat → tmux). A detached run whose tmux is UNAVAILABLE falls
	// back to the headless `claude --print` / `codex exec` path, because that
	// caller still wants an autonomous pass with a prompt (it orphans on restart,
	// with no live process to recover).
	//
	// Detach — NOT tracker-sourcing — is the sole signal that governs
	// headless-vs-idle here. A tracker id only links the Linear/Sentry issue; it
	// must not force the idle branch, or an unattended orchestrator that sets
	// {detach:true, tracker_id} (which /boss-epic always does) would sit idle
	// forever waiting for a client attach that never comes (the BOS-179
	// blocker). Interactive TUI creation still leaves Detach=false → idle, so
	// that path is unchanged. Firing a headless run for an interactive,
	// plan-less session would launch `claude --print` with an empty prompt,
	// which exits non-zero and (via finalizeHeadlessRunIfApplicable) blocks the
	// session — Detach=false keeps such sessions off this branch.
	var claudeSessionID string
	headlessRun := false
	// Set by the headless branch when it mints a question-hook token (BOS-486);
	// flipped true once the poll fallback that will eventually release the token
	// is genuinely armed. See the defer in that branch.
	var questionHookRelease *bool
	switch {
	case tmuxHosted:
		claudeSessionID, err = l.startTmuxChat(ctx, sessionID, opts, session, result)
		if err != nil {
			// startTmuxChat runs the authoritative capability preflight, which
			// can reject a launch the post-setup probe accepted. That happens
			// after the worktree and branch are persisted, and the cron caller
			// only drops the session row on error, so roll them back here as
			// the post-setup probe does. Scoped to preflight rejections: every
			// other failure below keeps its existing no-rollback behavior.
			var preflightErr capabilityPreflightError
			if errors.As(err, &preflightErr) {
				err = rollbackPreflightFailure(err)
			}
			return fmt.Errorf("start tmux chat: %w", err)
		}
	case !opts.Detach:
		// Interactive (non-detach) session: left unstarted. For tracker sessions
		// the plan is pre-filled into the agent's input on first attach (see the
		// boss attach prefill path); for plain/existing-PR sessions there is no
		// plan to prefill. Either way the interactive run started on attach arms
		// its own finalize hook, so nothing here depends on a headless run
		// firing. claudeSessionID stays empty (no agent yet), mirroring the idle
		// quick-chat path.
		l.logger.Info().
			Str("session", sessionID).
			Msg("interactive session created idle; awaiting manual start on first attach")
	default:
		headlessRun = true
		// Account env sits above proof (disjoint keys today; account wins by
		// convention, mirroring the interactive tmux precedence account > proof).
		// There is no managed BOSS_* layer on the headless path. The repo's
		// stored LINEAR_API_KEY / SENTRY_* secrets are filled beneath the
		// worktree .env (OverlayWithRepo) so the run authenticates to its own
		// repo's Linear workspace, not the daemon's ambient one.
		headlessEnv := resolveWorktreeRelativeHomes(dotenv.OverlayWithRepo(mergeEnv(l.resolveAccountEnv(ctx, session), l.resolveProofEnv()), result.WorktreePath, repo), result.WorktreePath)
		// Hand the run the loopback question-signal context (BOS-486). The
		// token is minted BEFORE the spawn — it has to be in the child's env —
		// and bound to the agent session id the plugin resolves AFTER it, which
		// is the same id the injected hook reports itself under and the same id
		// the question store is keyed by. Agents known not to consume the keys
		// (claude, codex) are skipped entirely, so their env is byte-identical
		// to what it was before BOS-486.
		headlessEnv = l.withQuestionHookEnv(session.AgentName, headlessEnv)
		launchRunner, ok := l.agentRunner.(agent.HeadlessLaunchOptionsDispatcher)
		if !ok {
			return fmt.Errorf("headless launch for agent %q requires a launch-options-aware agent dispatcher", resolvedAgentName)
		}
		claudeSessionID, err = launchRunner.StartByAgentWithHeadlessLaunchOptions(ctx, session.AgentName, result.WorktreePath, session.Plan, nil, "", session.Model, headlessEnv, agent.HeadlessLaunchOptions{
			HeadlessCapabilityProfile: opts.HeadlessCapabilityProfile,
		})
		if err != nil {
			return fmt.Errorf("start claude: %w", err)
		}
		l.registerQuestionHookToken(claudeSessionID, headlessEnv[questionHookTokenEnv])

		// Bind the token's release to this function's EXIT rather than to a
		// position in it. The only production releaser is
		// SignalSessionRunComplete, reached via the poll fallback armed ~60
		// lines below — and several error returns sit in between (the
		// AgentSessionID update, the agent_chats insert, the state writes), as
		// does the possibility that the arm declines or is never reached at all.
		// On any of those the entry would stay in headlessHookTokens for the
		// daemon's lifetime while the spawned child still holds the matching
		// credential in its env — both failure modes
		// ReleaseHeadlessRunHookToken exists to prevent. Releasing costs only
		// this run's question signal, which nothing consumes for a headless chat
		// yet; leaking costs a valid token.
		//
		// For whoever eventually gives the headless path a consumer: a CRON run
		// never reaches this branch at all, so it can never carry a question
		// signal. tmuxHosted is true for any opts.CronJobID, so cron takes the
		// tmux case above and no token is ever minted for it. (The cronJobID
		// clause in shouldArmHeadlessPollFallback is therefore belt-and-braces
		// — its !headlessRun clause already excludes cron.)
		//
		// Guarded on a token having actually been minted, so an opt-out agent
		// (claude, codex) is not even passed to the registry: their behaviour
		// stays untouched because the call does not happen, not because the
		// callee is a no-op for them.
		if questionHookToken := headlessEnv[questionHookTokenEnv]; questionHookToken != "" {
			questionHookHandedOff := false
			armedQuestionHookID := claudeSessionID
			defer func() {
				if !questionHookHandedOff {
					l.releaseQuestionHookToken(armedQuestionHookID)
				}
			}()
			questionHookRelease = &questionHookHandedOff
		}
	}
	// An agent is now running for this session, and everything below can still
	// fail — most of it on the bootstrap deadline this function runs under. Both
	// create callers answer a StartSession error by purging the worktree and
	// deleting the row, neither of which stops a detached run, so bind the stop
	// to this function's EXIT rather than adding it to each of the error returns
	// between here and the end. See stopRunAbandonedByFailedBootstrap.
	if claudeSessionID != "" {
		spawnedRunID := claudeSessionID
		spawnedTmuxHosted := tmuxHosted
		spawnedAgentName := session.AgentName
		defer func() {
			if retErr != nil {
				l.stopRunAbandonedByFailedBootstrap(ctx, sessionID, spawnedAgentName, spawnedRunID, spawnedTmuxHosted)
			}
		}()
	}

	if hookResp != nil && !hookResp.GetIsSupported() {
		l.logger.Info().Str("session", sessionID).Msg("agent does not support finalize hook")
	}
	hooklessTmux := tmuxHosted && (opts.ZeroOutput || hookResp != nil && !hookResp.GetIsSupported())

	// Update session with Claude session ID. Idle tracker sessions have no
	// agent session yet, so leave AgentSessionID nil (matching quick chat)
	// rather than persisting an empty value.
	if claudeSessionID != "" {
		if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
			AgentSessionID: strPtr(claudeSessionID),
		}); err != nil {
			return fmt.Errorf("update claude session id: %w", err)
		}
		// The headless path (codex exec / claude --print) never passes through
		// StartTmuxChat, which is the only other place an agent_chats row is
		// created. Without a row, GetChatTranscript / SendChatMessage /
		// GetChatStatuses — and the orchestrator's FindDaemonForChat, fed by the
		// agent_chats OnChange delta — cannot resolve the chat, so `boss chat
		// show/wait/send` and the remote MCP tools 404 for a session that is
		// otherwise running fine. Persist the primary chat row here so the chat
		// is followable the moment the session exists. The cron/tmux path
		// already created its row, so only the headless branch does this.
		// (l.agentChats is nil in some unit tests; production always sets it.)
		if headlessRun && l.agentChats != nil {
			// BOS-381: seed the primary chat with the session's resolved
			// provider/account/model so the chat is the runtime authority from the
			// moment it exists (convert/rotation/resume all read the chat).
			if _, err := l.agentChats.Create(ctx, db.CreateAgentChatParams{
				SessionID:      sessionID,
				AgentSessionID: claudeSessionID,
				AgentName:      session.AgentName,
				Title:          session.Title,
				AccountID:      session.AccountID,
				Model:          session.Model,
			}); err != nil {
				return fmt.Errorf("create agent_chats row for headless run of session %s: %w", sessionID, err)
			}
		}
		if headlessRun && claudeSessionID != "" && l.chatStatus != nil {
			agentName := session.AgentName
			runID := claudeSessionID
			safego.Go(l.logger, func() {
				l.watchHeadlessRunStatus(agentName, runID)
			})
		}
	}
	if hooklessTmux {
		l.armTmuxCompletionForHooklessRun(sessionID, claudeSessionID, session.RepoID)
	} else if l.shouldArmHeadlessPollFallback(headlessRun, opts.CronJobID, claudeSessionID, hookResp) {
		// A detached (`boss new --detach`) session took the headless StartByAgent
		// branch, running codex exec / claude --print. The only production caller
		// of StartSession passes no HookToken, so the ConfigureFinalizeHook probe
		// never runs and hookResp is nil — meaning NO Stop hook is installed for
		// ANY agent here. Without arming, the run is stranded in ImplementingPlan
		// forever. Arm PollFallback (which captures the run's exit status) to
		// drive finalize/block on completion. A hook-supporting agent that DID
		// receive a HookToken (hookResp != nil && IsSupported) is excluded: its
		// Stop hook drives completion, so arming would double-finalize.
		if l.armHeadlessPollFallback(sessionID, claudeSessionID, session) && questionHookRelease != nil {
			// Completion will now route through SignalSessionRunComplete, which
			// releases the question-hook token — so hand ownership off from the
			// defer armed above and let the run keep its signal.
			*questionHookRelease = true
		}
	}

	// Fire AgentStarted → ImplementingPlan.
	if err := sm.FireCtx(ctx, machine.AgentStarted); err != nil {
		return fmt.Errorf("fire agent_started: %w", err)
	}

	implementingState := int(machine.ImplementingPlan)
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		State: &implementingState,
	}); err != nil {
		return fmt.Errorf("set implementing_plan state: %w", err)
	}

	// For sessions without an existing PR, push the branch and create a
	// draft PR so the user gets a PR for the session. This covers both plain
	// "new PR" sessions and tracker-sourced sessions (e.g. Linear tickets) —
	// the latter carry a Plan but still need a PR for visibility.
	//
	// The create runs as a TRACKED BACKGROUND STEP (BOS-540): it costs a push,
	// a fetch and a `gh pr create` — 64s of a measured 135s session start —
	// and nothing the agent does needs the PR to exist, so blocking the return
	// on it only delayed the moment the session became attachable. The session
	// row remains the single source of truth: the PR number is persisted
	// through the usual sessions.Update once the create lands, and the TUI/web
	// pick it up on their normal refresh. See startBackgroundDraftPR for the
	// context and lifetime contract.
	//
	// Cron-spawned sessions opt out entirely via opts.DeferPR — the Stop-hook
	// finalize path calls EnsurePR once the run actually produces commits. That
	// is unchanged: a DeferPR session starts no background step and creates no
	// PR here.
	if session.PRNumber == nil && !opts.DeferPR {
		l.startBackgroundDraftPR(ctx, sessionID, result.WorktreePath, result.BranchName)
	}

	l.logger.Info().
		Str("session", sessionID).
		Str("claudeSession", claudeSessionID).
		Msg("session started, implementing plan")

	return nil
}

// resolveWorktreeRelativeHomes makes preflight observe HOME and CODEX_HOME as
// the launched agent will: relative values are interpreted from its worktree.
func resolveWorktreeRelativeHomes(env map[string]string, worktreePath string) map[string]string {
	for _, key := range []string{"CODEX_HOME", "HOME"} {
		if value := env[key]; value != "" && !filepath.IsAbs(value) {
			env[key] = filepath.Join(worktreePath, value)
		}
	}
	return env
}

// StartQuickChatSession starts a Claude process directly in the repo's base
// directory. No worktree, branch, or PR is created.
func (l *Lifecycle) StartQuickChatSession(ctx context.Context, sessionID string) error {
	session, err := l.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}

	repo, err := l.repos.Get(ctx, session.RepoID)
	if err != nil {
		return fmt.Errorf("get repo: %w", err)
	}

	// Set WorktreePath to repo's base directory (no worktree created).
	worktreePath := repo.LocalPath
	emptyBranch := ""
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		WorktreePath: &worktreePath,
		BranchName:   &emptyBranch,
	}); err != nil {
		return fmt.Errorf("update worktree path: %w", err)
	}

	// Skip CreatingWorktree, go straight to StartingAgent.
	startingState := int(machine.StartingAgent)
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		State: &startingState,
	}); err != nil {
		return fmt.Errorf("set starting_claude state: %w", err)
	}

	// Quick chat has no plan — Claude starts on-demand when user attaches.
	// Transition directly to ImplementingPlan so the session is ready.
	implementingState := int(machine.ImplementingPlan)
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		State: &implementingState,
	}); err != nil {
		return fmt.Errorf("set implementing_plan state: %w", err)
	}

	l.logger.Info().
		Str("session", sessionID).
		Msg("quick chat session started (Claude on-demand)")

	return nil
}

// SubmitPR transitions the session from ImplementingPlan through to
// AwaitingChecks. If the PR was already created (draft-PR-up-front sessions),
// it pushes any pending commits and goes directly to AwaitingChecks. Otherwise
// it pushes the branch, creates a draft PR, and transitions through
// PushingBranch → OpeningDraftPR → AwaitingChecks.
func (l *Lifecycle) SubmitPR(ctx context.Context, sessionID string) error {
	session, err := l.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}

	repo, err := l.repos.Get(ctx, session.RepoID)
	if err != nil {
		return fmt.Errorf("get repo: %w", err)
	}

	// Ensure origin URL is available before any VCS operations.
	if err := l.resolveOriginURL(ctx, repo); err != nil {
		return fmt.Errorf("resolve origin URL: %w", err)
	}

	hasPR := session.PRNumber != nil

	// Initialize state machine at the session's current state.
	sm := machine.NewWithContext(session.State, &machine.SessionContext{
		AttemptCount: session.AttemptCount,
		MaxAttempts:  machine.MaxAttempts,
		HasPR:        hasPR,
	})

	// Fire PlanComplete.
	// If HasPR: → AwaitingChecks (PR already exists).
	// Otherwise: → PushingBranch (need to push and create PR).
	if err := sm.FireCtx(ctx, machine.PlanComplete); err != nil {
		return fmt.Errorf("fire plan_complete: %w", err)
	}

	if hasPR {
		// PR already exists — skip PR creation, but still push so that any
		// commits made since createDraftPR (e.g. Claude's implementation
		// commits on top of the empty placeholder commit) reach the remote.
		if err := l.worktrees.Push(ctx, session.WorktreePath, session.BranchName); err != nil {
			return fmt.Errorf("push branch: %w", err)
		}

		awaitingState := int(machine.AwaitingChecks)
		if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
			State: &awaitingState,
		}); err != nil {
			return fmt.Errorf("set awaiting_checks state: %w", err)
		}

		l.logger.Info().
			Str("session", sessionID).
			Msg("plan complete, PR exists, awaiting checks")

		return nil
	}

	// No PR yet — push branch and create draft PR.
	pushingState := int(machine.PushingBranch)
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		State: &pushingState,
	}); err != nil {
		return fmt.Errorf("set pushing_branch state: %w", err)
	}

	l.logger.Info().
		Str("session", sessionID).
		Str("branch", session.BranchName).
		Msg("pushing branch")

	// Push the branch to remote.
	if err := l.worktrees.Push(ctx, session.WorktreePath, session.BranchName); err != nil {
		return fmt.Errorf("push branch: %w", err)
	}

	// Fire BranchPushed → OpeningDraftPR.
	if err := sm.FireCtx(ctx, machine.BranchPushed); err != nil {
		return fmt.Errorf("fire branch_pushed: %w", err)
	}

	openingState := int(machine.OpeningDraftPR)
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		State: &openingState,
	}); err != nil {
		return fmt.Errorf("set opening_draft_pr state: %w", err)
	}

	l.logger.Info().
		Str("session", sessionID).
		Msg("creating draft PR")

	if err := l.openDraftPRForBranch(ctx, sessionID, session, repo); err != nil {
		l.logDraftPRBranchDebugSnapshot(ctx, sessionID, session.WorktreePath, session.BranchName, session.BaseBranch, err)
		return err
	}

	// Fire PROpened → AwaitingChecks.
	if err := sm.FireCtx(ctx, machine.PROpened); err != nil {
		return fmt.Errorf("fire pr_opened: %w", err)
	}

	awaitingState := int(machine.AwaitingChecks)
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		State: &awaitingState,
	}); err != nil {
		return fmt.Errorf("set awaiting_checks state: %w", err)
	}

	prNumber := 0
	if session.PRNumber != nil {
		prNumber = *session.PRNumber
	}
	prURL := ""
	if session.PRURL != nil {
		prURL = *session.PRURL
	}
	l.logger.Info().
		Str("session", sessionID).
		Int("prNumber", prNumber).
		Str("prURL", prURL).
		Msg("draft PR created, awaiting checks")

	return nil
}

// draftPRBlockedReason renders the blocked_reason recorded when a draft PR
// create fails.
//
// This is the SINGLE place the transient/terminal form is chosen (BOS-877). Both
// writers reach it through setDraftPRBlockedReason — the background create via
// failInFlightDraftPR, and EnsurePR — so putting the decision here is what stops
// the two paths from ever disagreeing about the same error.
//
// gitremote.IsTransient reads err.Error(), so it sees wrapped causes: an
// exhausted retry ladder returns an *AttemptsError wrapping the transient error
// and still classifies transient. That is deliberate. The ladder having run does
// not turn GitHub's outage into the operator's key problem; the reason then
// carries both the transient marker and the "after N attempts" suffix, which is
// exactly the pair a reader needs.
func draftPRBlockedReason(err error) string {
	if gitremote.IsTransient(err) {
		return sessionreason.DraftPRCreationTransientFailure(err)
	}
	return sessionreason.DraftPRCreationFailure(err)
}

func isDraftPRBlockedReason(reason *string) bool {
	return sessionreason.IsDraftPRCreationFailure(reason)
}

// isClearableDraftPRReason reports whether reason is one of the two
// lifecycle-owned draft-PR markers a successful create or attach should clear:
// the failure reason from a previous attempt, or the in-flight marker the
// background step (BOS-540) wrote when it started. Anything else — a fix-loop
// block, a finalize failure — belongs to another owner and is left alone.
func isClearableDraftPRReason(reason *string) bool {
	return isDraftPRBlockedReason(reason) || sessionreason.IsDraftPRCreationInFlight(reason)
}

func (l *Lifecycle) setDraftPRBlockedReason(ctx context.Context, sessionID string, err error) {
	if err == nil {
		return
	}
	reason := draftPRBlockedReason(err)
	reasonPtr := &reason
	if _, updateErr := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		BlockedReason: &reasonPtr,
	}); updateErr != nil {
		l.logger.Warn().Err(updateErr).
			Str("session", sessionID).
			Msg("failed to persist draft PR creation error")
	}
}

// clearDraftPRBlockedReason drops a lifecycle-owned draft-PR marker (a previous
// attempt's failure, or the background step's in-flight marker) once a PR has
// been created or attached.
//
// The clear is an unconditional `blocked_reason = NULL`, so it re-reads the row
// before writing. The caller's *models.Session is a snapshot, and on the BOS-540
// background path that snapshot is taken before a push + fetch + `gh pr create`
// — tens of seconds during which the session's own headless run can fail and
// write a real block. Deciding from the stale copy would erase that diagnostic
// and leave the session Blocked with no reason. Re-reading narrows the window to
// the store round-trip, matching what this helper's synchronous callers have
// always had.
func (l *Lifecycle) clearDraftPRBlockedReason(ctx context.Context, sessionID string, session *models.Session) error {
	if !isClearableDraftPRReason(session.BlockedReason) {
		return nil
	}
	// A failed re-read falls through to the clear rather than erroring. Two of
	// this helper's callers (attachPRMetadata, LinkPR) propagate the error, and
	// they reach here AFTER the PR metadata is persisted — so failing on a
	// transient read would invent a partial-success shape the row does not have.
	// Falling through is exactly the behaviour every caller had before the
	// re-read existed.
	if current, err := l.sessions.Get(ctx, sessionID); err != nil {
		l.logger.Warn().Err(err).
			Str("session", sessionID).
			Msg("could not re-read session before clearing draft PR blocked reason; clearing from the caller's copy")
	} else if !isClearableDraftPRReason(current.BlockedReason) {
		// Someone else owns the reason now. Adopt the stored value so the
		// caller's copy stops disagreeing with the row.
		session.BlockedReason = current.BlockedReason
		return nil
	}
	var cleared *string
	updated, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		BlockedReason: &cleared,
	})
	if err != nil {
		return fmt.Errorf("clear draft PR blocked reason: %w", err)
	}
	session.BlockedReason = updated.BlockedReason
	return nil
}

// draftPRPlaceholderCommitSubject is the message of the empty commit used to
// give a branch a diff so GitHub will open a PR for it (session-start draft
// PRs and dirty-only cron finalize both use it). draftPRTitle recognizes it
// and falls back to the session/cron title rather than deriving a user-facing
// PR title from this scaffolding commit.
//
// The literal now lives in package git as gitpkg.DraftPRPlaceholderCommitSubject
// — this is an alias, not a second source of truth — because InjectPRNumbers'
// rebase --exec (owned by git) must also recognize the placeholder so it
// never rewrites its subject (BOS-591), and git cannot import session (this
// package already imports git).
const draftPRPlaceholderCommitSubject = gitpkg.DraftPRPlaceholderCommitSubject

// createDraftPR pushes the branch and creates a draft PR on GitHub, storing the
// PR number and URL on the session. StartSession runs it for any session
// without an existing PR — since BOS-540 from a tracked background step rather
// than on its synchronous return path, so the PR lands after the session is
// already usable.
func (l *Lifecycle) createDraftPR(ctx context.Context, sessionID, worktreePath, branchName string, session *models.Session, repo *models.Repo) error {
	// Ensure origin URL is available before any VCS operations.
	if err := l.resolveOriginURL(ctx, repo); err != nil {
		return fmt.Errorf("resolve origin URL: %w", err)
	}

	l.logger.Info().
		Str("session", sessionID).
		Str("branch", branchName).
		Msg("pushing branch for immediate PR")

	if err := l.worktrees.VerifyCurrentBranch(ctx, worktreePath, branchName); err != nil {
		return fmt.Errorf("verify worktree branch before draft PR: %w", err)
	}

	// Create an empty commit so the branch diverges from base; GitHub
	// rejects PRs with "No commits between" otherwise.
	if err := l.worktrees.EmptyCommit(ctx, worktreePath, draftPRPlaceholderCommitSubject); err != nil {
		return fmt.Errorf("empty commit: %w", err)
	}

	if err := l.worktrees.Push(ctx, worktreePath, branchName); err != nil {
		return fmt.Errorf("push branch: %w", err)
	}

	// SkipFetch: refs/remotes/origin/<base> is already present in the repo's
	// shared common git dir — Manager.Create fetched this very base earlier in
	// this same StartSession call — so fetching it again here buys nothing but
	// a second GitHub round-trip. The base may have advanced since, and that
	// staleness is accepted: this is a local sanity check whose only job is a
	// better error message than GitHub's bare "No commits between" (see the
	// wrapping below). GitHub remains the authority, comparing against the
	// real base when the PR is actually opened. A stale ref cannot flip the
	// ahead-count gate on its own, because the placeholder commit above was
	// created here moments ago and so is in no origin/<base> this repo could
	// be holding locally.
	//
	// The other worktree path, CreateFromExistingBranch, fetches the head
	// branch but not the base, so it does not populate that ref itself. It
	// does not reach this code today — createDraftPR runs only when
	// session.PRNumber is nil, and ExistingBranch is only ever set alongside a
	// PR number — but nothing enforces that pairing, so treat it as a
	// convention. A caller that broke it would read whatever an earlier
	// session left behind, or nothing at all if this clone never fetched that
	// base; the latter fails to resolve origin/<base> and surfaces as a
	// retryable blocked draft PR. Neither yields a wrong PR.
	verification, err := l.worktrees.VerifyPushedBranchAheadOfBase(ctx, worktreePath, branchName, session.BaseBranch, gitpkg.VerifyPushedBranchAheadOfBaseOpts{SkipFetch: true})
	if err != nil {
		return fmt.Errorf("verify PR branch before draft PR: %w", err)
	}

	session.WorktreePath = worktreePath
	session.BranchName = branchName
	if err := l.openDraftPRForBranch(ctx, sessionID, session, repo); err != nil {
		if strings.Contains(err.Error(), "No commits between") && verification != nil {
			err = fmt.Errorf(
				"head branch %s has no commits over base origin/%s (base SHA %s, head SHA %s): %w",
				branchName,
				session.BaseBranch,
				verification.BaseSHA,
				verification.HeadSHA,
				err,
			)
		}
		l.logDraftPRBranchDebugSnapshot(ctx, sessionID, worktreePath, branchName, session.BaseBranch, err)
		return err
	}

	return nil
}

// logDraftPRBranchDebugSnapshot logs the worktree's current branch/HEAD/remote
// state alongside a failed draft PR error. The snapshot reflects state at call
// time, which is after the branch has been pushed — so remote_head_sha shows
// the just-pushed HEAD. That's intentional: it confirms whether the PR branch
// actually received the commit when CreateDraftPR still failed.
func (l *Lifecycle) logDraftPRBranchDebugSnapshot(ctx context.Context, sessionID, worktreePath, branchName, baseBranch string, draftPRErr error) {
	if snapshot, snapshotErr := l.worktrees.BranchDebugSnapshot(ctx, worktreePath, branchName, baseBranch); snapshotErr == nil {
		l.logger.Warn().Err(draftPRErr).
			Str("session", sessionID).
			Str("branch", branchName).
			Str("base", baseBranch).
			Str("current_branch", snapshot.CurrentBranch).
			Str("head_sha", snapshot.HeadSHA).
			Str("remote_head_sha", snapshot.RemoteHeadSHA).
			Str("ahead_behind", snapshot.AheadBehind).
			Msg("draft PR creation failed with branch debug snapshot")
	} else {
		l.logger.Warn().Err(snapshotErr).
			Str("session", sessionID).
			Str("branch", branchName).
			Str("draft_pr_error", draftPRErr.Error()).
			Msg("failed to collect branch debug snapshot after draft PR failure")
	}
}

func (l *Lifecycle) openDraftPRForBranch(ctx context.Context, sessionID string, session *models.Session, repo *models.Repo) error {
	// Refresh the two row fields this function decides from and then writes back.
	// Every caller but one fetched the session moments ago, so this is a
	// confirming read; the exception is the BOS-540 background step, whose copy
	// is a push and a fetch old by the time it gets here — long enough for both
	// fields to have moved under it, and the write below is unconditional.
	//
	//   - Title: a rename issued after CreateSession returned. Its own PR-title
	//     sync is skipped (UpdateSession gates that on pr_number, still nil), so
	//     deciding from the stale copy would open the PR with the pre-rename
	//     title AND revert the rename on the row, with nothing left to re-sync.
	//   - PRNumber: finalize's EnsurePR, SubmitPR, the reconciler, or LinkPR
	//     attaching a PR while we were pushing. Creating anyway would open a
	//     second PR (LinkPR's may be on another branch, so GitHub would not even
	//     refuse it with ErrPRAlreadyExists) and overwrite the attachment.
	//     Converging here reaches the same end state as the ErrPRAlreadyExists
	//     attach below, one round-trip earlier.
	//
	// A failed read falls through to the pre-existing behaviour rather than
	// erroring: callers propagate this error, and the create itself is fine.
	if current, err := l.sessions.Get(ctx, sessionID); err != nil {
		l.logger.Warn().Err(err).
			Str("session", sessionID).
			Msg("could not re-read session before opening draft PR; using the caller's copy")
	} else {
		session.Title = current.Title
		if current.PRNumber != nil {
			session.PRNumber = current.PRNumber
			session.PRURL = current.PRURL
			// Decide the clear below from the ROW, not the caller's copy: the
			// background step's copy predates the in-flight marker on some
			// paths, and a stale nil there would leave the marker set on a
			// session that now has a PR.
			session.BlockedReason = current.BlockedReason
			l.logger.Info().
				Str("session", sessionID).
				Int("prNumber", *current.PRNumber).
				Msg("draft PR open skipped: another writer already attached a PR")
			if clearErr := l.clearDraftPRBlockedReason(ctx, sessionID, session); clearErr != nil {
				l.logger.Warn().Err(clearErr).
					Str("session", sessionID).
					Msg("clear draft PR blocked reason failed after concurrent attach")
			}
			return nil
		}
	}

	title := l.draftPRTitle(ctx, session)
	prInfo, err := l.provider.CreateDraftPR(ctx, vcs.CreatePROpts{
		RepoPath:   repo.OriginURL,
		HeadBranch: session.BranchName,
		BaseBranch: session.BaseBranch,
		Title:      title,
		Body:       session.Plan,
		Draft:      true,
	})
	if err != nil {
		if errors.Is(err, vcs.ErrPRAlreadyExists) {
			if attachErr := l.attachExistingPRForBranch(ctx, sessionID, session, repo); attachErr != nil {
				return fmt.Errorf("attach existing PR after duplicate create: %w", attachErr)
			}
			return nil
		}
		return fmt.Errorf("create draft PR: %w", err)
	}

	prNumber := &prInfo.Number
	prURL := &prInfo.URL
	updateParams := db.UpdateSessionParams{
		PRNumber: &prNumber,
		PRURL:    &prURL,
	}
	// Persist the session title to match the PR we just created. Without this
	// the row keeps the cron job name ("Bossanova auto-implement") even though
	// the GitHub PR has a real title. Safe one-shot: every caller reaches here
	// only when PRNumber was nil (SubmitPR, StartSession→createDraftPR, EnsurePR
	// all guard on it), so this cannot overwrite a later user-edited title —
	// the same invariant attachPRMetadata relies on.
	if t := strings.TrimSpace(title); t != "" {
		updateParams.Title = &t
	}
	updated, err := l.sessions.Update(ctx, sessionID, updateParams)
	if err != nil {
		return fmt.Errorf("update PR info: %w", err)
	}

	session.PRNumber = updated.PRNumber
	session.PRURL = updated.PRURL
	session.Title = updated.Title
	if err := l.clearDraftPRBlockedReason(ctx, sessionID, session); err != nil {
		l.logger.Warn().Err(err).
			Str("session", sessionID).
			Msg("clear draft PR blocked reason failed after PR creation")
	}

	return nil
}

func (l *Lifecycle) draftPRTitle(ctx context.Context, session *models.Session) string {
	if session.CronJobID == nil || *session.CronJobID == "" {
		return session.Title
	}

	fallback := normalizeCronPRTitle(session.Title)
	subject, err := l.worktrees.LatestCommitSubject(ctx, session.WorktreePath)
	if err != nil {
		l.logger.Warn().Err(err).
			Str("session", session.ID).
			Str("worktree", session.WorktreePath).
			Msg("failed to read latest commit subject for cron PR title")
		return fallback
	}
	// A dirty-only cron run gets an empty placeholder commit so a PR can be
	// opened; its subject is scaffolding, not the user's work, so fall back to
	// the cron/session title instead of deriving the PR title from it.
	if trimmed := strings.TrimSpace(subject); trimmed == "" || trimmed == draftPRPlaceholderCommitSubject {
		return fallback
	}
	return normalizeCronPRTitle(subject)
}

// finalizeTitle decides the PR/session title for a session at cron finalize.
//
// The historical behavior derived the title from the LAST commit subject. That
// is fine when the branch is a single commit (the subject IS the PR's intent),
// but wrong once the branch carries several commits — the last one is usually a
// narrow fix, so the derived title is irrelevant and, worse, it CLOBBERS a good
// title the agent set at PR creation (the WON-1280 bug: "[WON-1280] Don't allow …"
// became "Fail closed when clean-tree baseline is missing").
//
// So the policy is:
//   - ≥2 real commits: ask the agent (which keeps a good existing title and only
//     improves a weak one). If it has no suggestion, PRESERVE the existing PR
//     title rather than rewriting it to the last commit subject.
//   - 0–1 real commits: derive deterministically by normalizing the commit
//     subject (the old, well-tested behavior — cleans a messy "type(scope): …"
//     PR title into a readable one).
//   - commit history unreadable: preserve the existing title (never risk a
//     clobber on incomplete information).
//
// currentPRTitle is the PR's current title (set earlier, e.g. by the agent at PR
// creation). Non-cron sessions keep their own title unchanged.
func (l *Lifecycle) finalizeTitle(ctx context.Context, session *models.Session, currentPRTitle string) string {
	if session.CronJobID == nil || *session.CronJobID == "" {
		return session.Title
	}

	subjects, err := l.worktrees.CommitSubjects(ctx, session.WorktreePath, session.BaseBranch)
	if err != nil {
		l.logger.Warn().Err(err).
			Str("session", session.ID).
			Str("base", session.BaseBranch).
			Msg("failed to read commit subjects for cron PR title; preserving existing title")
		if t := strings.TrimSpace(currentPRTitle); t != "" {
			return t
		}
		return l.draftPRTitle(ctx, session)
	}

	if real := realCommitSubjects(subjects); len(real) >= 2 {
		if title, ok := l.agentSuggestedTitle(ctx, session, currentPRTitle, real); ok {
			return title
		}
		// Multi-commit with no agent suggestion: never rewrite to the last commit
		// subject. Preserve a meaningful existing title; derive only if none.
		if t := strings.TrimSpace(currentPRTitle); t != "" {
			return t
		}
	}
	return l.draftPRTitle(ctx, session)
}

// realCommitSubjects drops the empty draft-PR placeholder commit so it never
// counts toward the multi-commit threshold or reaches the title suggester.
//
// Classification is delegated to gitpkg.IsDraftPRPlaceholderSubject: package
// git owns both the placeholder literal and the tolerance for the "[#NNN] "
// tag inject-pr-tag can leave in it (BOS-591). Keeping one implementation
// means the finalize guard and the PR-tag injector can never disagree about
// what counts as real work.
func realCommitSubjects(raw []string) []string {
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		if t := strings.TrimSpace(s); t == "" || gitpkg.IsDraftPRPlaceholderSubject(t) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// agentSuggestedTitle asks the session's agent plugin to suggest a PR title.
// Returns ok=false on any error / unsupported / empty result, so the caller
// falls back deterministically — a title suggestion must never block or fail
// finalize.
func (l *Lifecycle) agentSuggestedTitle(ctx context.Context, session *models.Session, currentPRTitle string, subjects []string) (string, bool) {
	client, err := l.agentClientFor(session)
	if err != nil || client == nil {
		return "", false
	}
	resp, err := client.SuggestPRTitle(ctx, &bossanovav1.SuggestPRTitleRequest{
		WorkDir:      session.WorktreePath,
		CurrentTitle: currentPRTitle,
		GitLog:       strings.Join(subjects, "\n"),
		BaseBranch:   session.BaseBranch,
	})
	if err != nil {
		l.logger.Warn().Err(err).
			Str("session", session.ID).
			Msg("agent SuggestPRTitle failed; falling back to deterministic title")
		return "", false
	}
	if !resp.GetSupported() {
		return "", false
	}
	title := strings.TrimSpace(resp.GetTitle())
	if title == "" {
		return "", false
	}
	return title, true
}

func normalizeCronPRTitle(subject string) string {
	title := strings.TrimSpace(subject)
	if title == "" {
		return title
	}

	for {
		next := conventionalCommitPrefixRE.ReplaceAllString(title, "")
		next = prNumberPrefixRE.ReplaceAllString(strings.TrimSpace(next), "")
		next = strings.TrimSpace(next)
		if next == title {
			break
		}
		title = next
	}
	if title == "" {
		return strings.TrimSpace(subject)
	}

	first, size := utf8.DecodeRuneInString(title)
	if first == utf8.RuneError && size == 0 {
		return title
	}
	return string(unicode.ToUpper(first)) + title[size:]
}

func (l *Lifecycle) attachExistingPRForBranch(ctx context.Context, sessionID string, session *models.Session, repo *models.Repo) error {
	found, err := l.attachOpenPRForBranch(ctx, sessionID, session, repo)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("existing PR for branch %q not found", session.BranchName)
	}
	return nil
}

// attachOpenPRForBranch finds an open PR whose head matches one of the
// session's candidate branches and persists its metadata (number, URL, and —
// via attachPRMetadata — the corrected branch and title).
//
// Every caller invokes this only for a session whose PRNumber is still unset
// (the first-time duplicate-create / safety-net paths in lifecycle.go and
// finalize.go). That precondition is what makes the title persist in
// attachPRMetadata safe: the stored title is still the freshly-generated one,
// so it cannot clobber a user-edited title. Do not reuse this as a generic
// re-attach for sessions that already have a PR without revisiting that
// guarantee.
func (l *Lifecycle) attachOpenPRForBranch(ctx context.Context, sessionID string, session *models.Session, repo *models.Repo) (bool, error) {
	prs, err := l.provider.ListOpenPRs(ctx, repo.OriginURL)
	if err != nil {
		return false, fmt.Errorf("%w: %w", errListOpenPRs, err)
	}

	candidates := l.candidateBranches(ctx, session)
	for _, branch := range candidates {
		for _, pr := range prs {
			if pr.State != vcs.PRStateOpen || pr.HeadBranch != branch {
				continue
			}

			prTitle := pr.Title
			if session.CronJobID != nil && *session.CronJobID != "" {
				// Decide the title from the agent (keep-if-valid) or, failing
				// that, preserve the existing PR title — never blindly rewrite it
				// to the last commit subject. Only call GitHub when it changes.
				title := l.finalizeTitle(ctx, session, pr.Title)
				if title != pr.Title {
					if err := l.provider.UpdatePRTitle(ctx, repo.OriginURL, pr.Number, title); err != nil {
						return false, fmt.Errorf("update attached PR title: %w", err)
					}
				}
				prTitle = title
			}

			if err := l.attachPRMetadata(ctx, sessionID, session, repo, pr.Number, pr.HeadBranch, prTitle); err != nil {
				return false, err
			}
			return true, nil
		}
	}

	return false, nil
}

// candidateBranches returns the branches a session's PR might live on: the
// stored branch, plus the worktree's live branch when it differs and is
// resolvable. Live-resolution failures are swallowed (stored branch only).
func (l *Lifecycle) candidateBranches(ctx context.Context, session *models.Session) []string {
	out := []string{}
	if session.BranchName != "" {
		out = append(out, session.BranchName)
	}
	if l.branchResolver == nil || session.WorktreePath == "" {
		return out
	}
	live, err := l.branchResolver.CurrentBranch(ctx, session.WorktreePath)
	if err != nil {
		l.logger.Debug().Err(err).
			Str("session", session.ID).
			Str("worktree", session.WorktreePath).
			Msg("attach PR: resolve live worktree branch")
		return out
	}
	if live = strings.TrimSpace(live); live != "" && live != session.BranchName {
		out = append(out, live)
	}
	return out
}

func (l *Lifecycle) attachPRMetadata(ctx context.Context, sessionID string, session *models.Session, repo *models.Repo, prNumber int, matchedBranch, prTitle string) error {
	prURL, err := prURLForRepo(repo.OriginURL, prNumber)
	if err != nil {
		return err
	}
	prNumberPtr := &prNumber
	prURLPtr := &prURL
	updateParams := db.UpdateSessionParams{
		PRNumber: &prNumberPtr,
		PRURL:    &prURLPtr,
	}
	if matchedBranch != "" && matchedBranch != session.BranchName {
		branch := matchedBranch
		updateParams.BranchName = &branch
	}
	// Persisting the title is safe because every caller reaches this only when
	// the session has no PR yet (see attachOpenPRForBranch), so the stored
	// title is still the generated one and cannot overwrite a user edit.
	if title := strings.TrimSpace(prTitle); title != "" {
		updateParams.Title = &title
	}
	updated, err := l.sessions.Update(ctx, sessionID, updateParams)
	if err != nil {
		return fmt.Errorf("update PR info: %w", err)
	}

	session.PRNumber = updated.PRNumber
	session.PRURL = updated.PRURL
	if updateParams.BranchName != nil {
		session.BranchName = *updateParams.BranchName
	}
	if updateParams.Title != nil {
		session.Title = *updateParams.Title
	}
	if err := l.clearDraftPRBlockedReason(ctx, sessionID, session); err != nil {
		return err
	}
	return nil
}

func prURLForRepo(originURL string, prNumber int) (string, error) {
	nwo := vcs.GitHubNWO(originURL)
	if nwo == "" && strings.Count(originURL, "/") == 1 && !strings.Contains(originURL, "://") {
		nwo = strings.TrimSuffix(strings.TrimSpace(originURL), ".git")
	}
	if nwo == "" {
		return "", fmt.Errorf("construct PR URL: unsupported origin URL %q", originURL)
	}
	return fmt.Sprintf("https://github.com/%s/pull/%d", nwo, prNumber), nil
}

// LinkPR attaches an existing pull request to a session. It is intended as a
// manual repair path for cron sessions whose agent already created a PR before
// the Stop-hook finalize path ran.
//
// It assumes the session row still exists and does not guard against a
// concurrent finalize that deletes a no-changes session: the two are not
// serialized, so a link issued at the exact moment such a finalize deletes the
// row will fail benignly on the row lookup or update rather than corrupt state.
// In practice this path is used precisely because finalize already ran or
// failed, so that window is negligible.
func (l *Lifecycle) LinkPR(ctx context.Context, sessionID, prRef string) (*models.Session, error) {
	prRef = strings.TrimSpace(prRef)
	if sessionID == "" {
		return nil, fmt.Errorf("session ID is required")
	}
	if prRef == "" {
		return nil, fmt.Errorf("PR number or URL is required")
	}

	sess, err := l.sessions.Get(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	repo, err := l.repos.Get(ctx, sess.RepoID)
	if err != nil {
		return nil, fmt.Errorf("get repo: %w", err)
	}
	if err := l.resolveOriginURL(ctx, repo); err != nil {
		return nil, fmt.Errorf("resolve origin URL: %w", err)
	}

	prNumber, prURL, err := resolvePRReference(repo.OriginURL, prRef)
	if err != nil {
		return nil, err
	}
	var prTitle string
	if l.provider != nil {
		status, err := l.provider.GetPRStatus(ctx, repo.OriginURL, prNumber)
		if err != nil {
			return nil, fmt.Errorf("get PR status: %w", err)
		}
		if status != nil {
			prTitle = strings.TrimSpace(status.Title)
		}
	}

	prNumberPtr := &prNumber
	prURLPtr := &prURL
	updateParams := db.UpdateSessionParams{
		PRNumber: &prNumberPtr,
		PRURL:    &prURLPtr,
	}
	// Adopt the linked PR's title, but only on first association (no PR yet) so
	// re-linking can't clobber a title the user deliberately set. Mirrors the
	// one-shot rename invariant in reconcile.go / attachPRMetadata.
	if prTitle != "" && sess.PRNumber == nil {
		updateParams.Title = &prTitle
	}
	if sess.State == machine.Finalizing || sess.State == machine.Blocked {
		awaitingState := int(machine.AwaitingChecks)
		updateParams.State = &awaitingState
	}
	if sess.State == machine.Blocked {
		var cleared *string
		updateParams.BlockedReason = &cleared
	}
	updated, err := l.sessions.Update(ctx, sessionID, updateParams)
	if err != nil {
		return nil, fmt.Errorf("update PR info: %w", err)
	}
	if err := l.clearDraftPRBlockedReason(ctx, sessionID, updated); err != nil {
		return nil, err
	}

	if updated.CronJobID != nil && *updated.CronJobID != "" && l.cronJobs != nil {
		recordedID := updated.ID
		if err := l.cronJobs.UpdateLastRun(ctx, *updated.CronJobID, db.UpdateCronJobLastRunParams{
			SessionID: &recordedID,
			RanAt:     time.Now(),
			Outcome:   models.CronJobOutcomePRCreated,
		}); err != nil {
			return nil, fmt.Errorf("update cron last run: %w", err)
		}
	}

	l.logger.Info().
		Str("session", sessionID).
		Int("pr", prNumber).
		Msg("linked existing PR to session")

	return updated, nil
}

func resolvePRReference(originURL, prRef string) (int, string, error) {
	if prNumber, err := strconv.Atoi(prRef); err == nil {
		if prNumber <= 0 {
			return 0, "", fmt.Errorf("PR number must be positive")
		}
		prURL, err := prURLForRepo(originURL, prNumber)
		if err != nil {
			return 0, "", err
		}
		return prNumber, prURL, nil
	}

	u, err := url.Parse(prRef)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return 0, "", fmt.Errorf("PR reference must be a PR number or URL")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != "pull" {
		return 0, "", fmt.Errorf("PR URL must look like https://github.com/owner/repo/pull/123")
	}
	prNumber, err := strconv.Atoi(parts[3])
	if err != nil || prNumber <= 0 {
		return 0, "", fmt.Errorf("PR URL has invalid number %q", parts[3])
	}

	expectedRepoURL := vcs.NormalizeRepoURL(originURL)
	actualRepoURL := fmt.Sprintf("https://%s/%s/%s", u.Hostname(), parts[0], parts[1])
	if expectedRepoURL == "" || !strings.EqualFold(expectedRepoURL, actualRepoURL) {
		return 0, "", fmt.Errorf("PR URL repo %s/%s does not match session repo", parts[0], parts[1])
	}

	return prNumber, fmt.Sprintf("https://%s/%s/%s/pull/%d", u.Host, parts[0], parts[1], prNumber), nil
}

// EnsurePR pushes the session's branch and creates a draft PR if one does
// not already exist. It is idempotent: if session.PRNumber is already set,
// the call is a no-op. Used by the cron-finalize path (FL4) once the
// session has produced real commits, where DeferPR=true skipped the
// up-front PR creation.
//
// Unlike createDraftPR, EnsurePR does NOT make an empty placeholder commit:
// callers invoke it after Claude has produced its own commits.
func (l *Lifecycle) EnsurePR(ctx context.Context, sessionID string) error {
	session, err := l.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}

	if session.PRNumber != nil {
		return nil
	}

	repo, err := l.repos.Get(ctx, session.RepoID)
	if err != nil {
		return fmt.Errorf("get repo: %w", err)
	}

	if err := l.resolveOriginURL(ctx, repo); err != nil {
		return fmt.Errorf("resolve origin URL: %w", err)
	}

	l.logger.Info().
		Str("session", sessionID).
		Str("branch", session.BranchName).
		Msg("ensuring PR: pushing branch")

	if err := l.worktrees.Push(ctx, session.WorktreePath, session.BranchName); err != nil {
		return fmt.Errorf("push branch: %w", err)
	}

	if err := l.openDraftPRForBranch(ctx, sessionID, session, repo); err != nil {
		if errors.Is(err, vcs.ErrPRAlreadyExists) {
			if attachErr := l.attachExistingPRForBranch(ctx, sessionID, session, repo); attachErr == nil {
				return nil
			} else {
				l.setDraftPRBlockedReason(ctx, sessionID, attachErr)
				return fmt.Errorf("attach existing PR after duplicate create: %w", attachErr)
			}
		}
		l.logDraftPRBranchDebugSnapshot(ctx, sessionID, session.WorktreePath, session.BranchName, session.BaseBranch, err)
		l.setDraftPRBlockedReason(ctx, sessionID, err)
		return err
	}

	l.logger.Info().
		Str("session", sessionID).
		Str("branch", session.BranchName).
		Msg("ensured PR")

	return nil
}

// StopSession stops the Claude process for a session.
func (l *Lifecycle) StopSession(ctx context.Context, sessionID string) error {
	session, err := l.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}

	// Stop Claude process if running. The primary chat's provider (BOS-381) keys
	// the runner: a primary chat switched to another agent runs under the chat's
	// plugin, so stopping by the stale session agent would miss it.
	stopSess := l.effectiveSpawnSession(ctx, session)
	if session.AgentSessionID != nil && l.agentRunner.IsRunningByAgent(stopSess.AgentName, *session.AgentSessionID) {
		if err := l.agentRunner.StopByAgent(ctx, stopSess.AgentName, *session.AgentSessionID); err != nil {
			l.logger.Warn().Err(err).
				Str("session", sessionID).
				Msg("failed to stop claude process")
		}
	}

	// Kill all per-chat tmux sessions.
	l.killAllChatTmuxSessions(ctx, sessionID)

	// Also kill the legacy per-session tmux session if it exists.
	if session.TmuxSessionName != nil {
		l.KillTmuxByName(ctx, sessionID, *session.TmuxSessionName)
	}

	// Update state to Closed.
	closedState := int(machine.Closed)
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		State: &closedState,
	}); err != nil {
		return fmt.Errorf("set closed state: %w", err)
	}

	l.logger.Info().Str("session", sessionID).Msg("session stopped")
	return nil
}

// ArchiveSession stops the Claude process and removes the worktree,
// but keeps the branch alive for later resurrection.
func (l *Lifecycle) ArchiveSession(ctx context.Context, sessionID string) error {
	// Surface an explicit "archive in flight" signal on the session
	// (Session.archive_pending, hydrated from the DisplayTracker) so the TUI
	// renders "Archiving…" only while an archive is actually running here —
	// rather than inferring it from steady-state MERGED + repo flag, which stays
	// true forever for a resurrected merged session (BOS-425). Set on entry,
	// clear on exit via defer so it clears on both the success and error paths.
	// This is the single archive chokepoint: the ArchiveSession RPC, the
	// dispatcher's auto-archive-after-merge, and the dependabot auto-archive all
	// funnel through here via ArchiveSessionAndNotify.
	if l.settingUpTracker != nil {
		l.settingUpTracker.SetArchiving(sessionID, true)
		defer l.settingUpTracker.SetArchiving(sessionID, false)
	}

	session, err := l.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}

	// Stop Claude process if running. Route by the primary chat's provider
	// (BOS-381) so a chat switched to another agent is still stopped.
	stopSess := l.effectiveSpawnSession(ctx, session)
	if session.AgentSessionID != nil && l.agentRunner.IsRunningByAgent(stopSess.AgentName, *session.AgentSessionID) {
		if err := l.agentRunner.StopByAgent(ctx, stopSess.AgentName, *session.AgentSessionID); err != nil {
			l.logger.Warn().Err(err).
				Str("session", sessionID).
				Msg("failed to stop claude process")
		}
	}

	// Kill all per-chat tmux sessions.
	l.killAllChatTmuxSessions(ctx, sessionID)

	// Also kill the legacy per-session tmux session if it exists.
	if session.TmuxSessionName != nil {
		l.KillTmuxByName(ctx, sessionID, *session.TmuxSessionName)
	}

	// Archive worktree (removes directory, keeps branch).
	// Skip for quick chat sessions where WorktreePath is the base repo.
	repo, err := l.repos.Get(ctx, session.RepoID)
	if err != nil {
		return fmt.Errorf("get repo: %w", err)
	}
	// Stop any in-flight background draft-PR create before the worktree goes
	// (BOS-540). It runs `git commit` and `git push` INSIDE that directory, so
	// removing it underneath the goroutine corrupts the create; and the local
	// branch reap below could drop a branch whose remote copy and PR the create
	// had just published.
	l.StopBackgroundDraftPR(ctx, sessionID)
	// Re-read after the join, as RemoveSession does: the step can correct a
	// drifted BranchName on its way out (attachPRMetadata does), and the branch
	// reap below would then target a name that no longer exists while the real
	// branch survives.
	if fresh, freshErr := l.sessions.Get(ctx, sessionID); freshErr == nil {
		session = fresh
	}

	if session.WorktreePath != "" && session.WorktreePath != repo.LocalPath {
		if err := l.worktrees.Archive(ctx, session.WorktreePath); err != nil {
			return fmt.Errorf("archive worktree: %w", err)
		}

		// BOS-180: reclaim the session's LOCAL branch on archive when the repo
		// opts in (CanAutoDeleteBranches) and the branch is safe to drop.
		// Guarded by the same non-quick-chat condition as the worktree removal.
		// Best-effort: the worktree is already archived, so neither a
		// "not safe, keep it" outcome nor any git failure may fail the archive.
		// LOCAL ONLY — never deletes or pushes the remote branch. Safety is
		// routed through the worktree-manager interface (BranchSafeToDelete ==
		// merge-base --is-ancestor) so this path stays testable via the mock.
		if repo.CanAutoDeleteBranches {
			l.reapSafeLocalBranch(ctx, sessionID, repo, session)
		}
	}

	// Mark session as archived in DB.
	if err := l.sessions.Archive(ctx, sessionID); err != nil {
		return fmt.Errorf("archive session: %w", err)
	}

	l.logger.Info().Str("session", sessionID).Msg("session archived")
	return nil
}

// reapSafeLocalBranch best-effort deletes the session's LOCAL branch after its
// worktree has been removed, when the branch is safe to drop
// (merged/fast-forwarded/NO_CHANGE per BranchSafeToDelete) and is not the
// base/default branch. It never returns an error: callers invoke it after the
// worktree is already gone, so a predicate failure, an "unsafe, keep it"
// outcome, or a delete failure is only logged. LOCAL ONLY — it never touches
// the remote branch.
//
// Callers decide whether to gate on repo.CanAutoDeleteBranches: ArchiveSession
// (BOS-180) reaps only when the repo opts in, while the no-change cron
// hard-delete (BOS-424) reaps unconditionally — a throwaway no-change branch
// has nothing to resurrect, and the shared commits-no-origin caller is
// protected by the BranchSafeToDelete guard (unmerged commits ⇒ not safe ⇒
// kept).
func (l *Lifecycle) reapSafeLocalBranch(ctx context.Context, sessionID string, repo *models.Repo, session *models.Session) {
	if session.BranchName == "" {
		return
	}
	// Defensive guard: never force-delete the base branch. A ref is trivially
	// its own ancestor, so if a session's BranchName ever equals its BaseBranch
	// (or the repo default), BranchSafeToDelete returns true and the destructive
	// `git branch -D` would remove the base branch locally whenever it is not the
	// currently checked-out branch. Normal generated-branch sessions never hit
	// this, but a force-delete's safety must not depend on that invariant.
	if session.BranchName == session.BaseBranch || session.BranchName == repo.DefaultBaseBranch {
		l.logger.Info().
			Str("session", sessionID).
			Str("branch", session.BranchName).
			Msg("reap: branch is the base branch; keeping branch")
		return
	}
	safe, err := l.worktrees.BranchSafeToDelete(ctx, repo.LocalPath, session.BranchName, session.BaseBranch)
	if err != nil {
		l.logger.Warn().Err(err).
			Str("session", sessionID).
			Str("branch", session.BranchName).
			Msg("reap: branch safe-to-delete check failed; keeping branch")
		return
	}
	if !safe {
		l.logger.Info().
			Str("session", sessionID).
			Str("branch", session.BranchName).
			Msg("reap: branch not safe to auto-delete; keeping branch")
		return
	}
	if err := l.worktrees.DeleteLocalBranch(ctx, repo.LocalPath, session.BranchName); err != nil {
		l.logger.Warn().Err(err).
			Str("session", sessionID).
			Str("branch", session.BranchName).
			Msg("reap: failed to delete local branch; keeping branch")
		return
	}
	l.logger.Info().
		Str("session", sessionID).
		Str("branch", session.BranchName).
		Msg("reap: deleted safe local branch")
}

// ResurrectSessionOpts are the per-call knobs of Lifecycle.ResurrectSession.
type ResurrectSessionOpts struct {
	// SetupOutput, when non-nil, receives the repo setup script's live output
	// plus the coarse phase lines this method writes around it. It mirrors
	// StartSessionOpts.SetupOutput and exists for the same reason: the setup
	// script is the slowest step by an order of magnitude, and without a live
	// sink the caller sees nothing at all until it finishes (BOS-984).
	SetupOutput io.Writer
}

// ResurrectSessionResult reports the non-fatal outcomes of a resurrect that
// otherwise succeeded.
type ResurrectSessionResult struct {
	// SetupErr is the repo setup script's failure, nil when it succeeded, when
	// there is no script, or when this session has no worktree of its own.
	// Non-fatal by design — see gitpkg.ResurrectResult.SetupErr.
	SetupErr error
}

// ResurrectSession re-creates a worktree from an existing branch and
// starts a new Claude process (with --resume if a previous Claude session exists).
//
// A non-nil error means the session is NOT back. A non-nil result with a
// non-nil SetupErr means it IS back but its dependencies were not installed;
// the two are deliberately different outcomes (BOS-984).
func (l *Lifecycle) ResurrectSession(ctx context.Context, sessionID string, opts ResurrectSessionOpts) (*ResurrectSessionResult, error) {
	// Bound the whole resurrect, exactly as StartSession bounds a bootstrap.
	//
	// This is load-bearing after BOS-984. Until then the RPC inherited two
	// accidental ceilings — the client's 120s unary deadline and the daemon's
	// 120s http.Server WriteTimeout — and the bug was that both were far too
	// SHORT for the setup script. Streaming removes them, and removing them with
	// nothing in their place would leave the one step that can genuinely hang
	// forever (a wedged agent start) unbounded. BootstrapTimeout is double
	// SetupScriptTimeout, so an honest slow setup finishes comfortably inside it
	// while a stuck resurrect still ends.
	//
	// The rollback below deliberately detaches from this context, so a resurrect
	// killed here still un-archives the row rather than stranding it live and
	// agent-less.
	ctx, cancel := context.WithTimeout(ctx, l.bootstrapDeadline())
	defer cancel()

	setupOutput := opts.SetupOutput
	session, err := l.sessions.Get(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}

	if session.ArchivedAt == nil {
		return nil, fmt.Errorf("session %s is not archived", sessionID)
	}

	repo, err := l.repos.Get(ctx, session.RepoID)
	if err != nil {
		return nil, fmt.Errorf("get repo: %w", err)
	}

	l.logger.Info().
		Str("session", sessionID).
		Str("branch", session.BranchName).
		Msg("resurrecting session")

	result := &ResurrectSessionResult{}

	// Resurrect worktree from existing branch.
	// Skip for quick chat sessions where WorktreePath is the base repo.
	if session.WorktreePath != repo.LocalPath {
		writeSetupProgress(setupOutput, "recreating worktree")
		wt, err := l.worktrees.Resurrect(ctx, gitpkg.ResurrectOpts{
			RepoPath:     repo.LocalPath,
			WorktreePath: session.WorktreePath,
			BranchName:   session.BranchName,
			// Base branch lets Resurrect recreate the branch when it was
			// safe-deleted on archive (BOS-180). See BOS-421.
			BaseBranch:        session.BaseBranch,
			SetupScript:       repo.SetupScript,
			SetupScriptOutput: setupOutput,
		})
		if err != nil {
			return nil, fmt.Errorf("resurrect worktree: %w", err)
		}
		if wt != nil {
			result.SetupErr = wt.SetupErr
			if wt.SetupErr != nil {
				writeSetupProgress(setupOutput, "setup script failed: "+wt.SetupErr.Error())
			}
		}
		writeSetupProgress(setupOutput, "worktree restored")
	}

	// Clear archived status AND leave the terminal state in one conditional
	// write, BEFORE the slow agent start below (BOS-697, BOS-924).
	//
	// It has to be one statement. A row wearing {archived_at NULL, state
	// Merged} is byte-for-byte what archiveMergedButUnarchived (reconcile.go)
	// heals, so clearing archived_at and writing the state as two writes opens
	// a window in which a reconcile tick can dispatch a detached archive that
	// deletes the worktree this call just recreated. Writing the live state
	// also un-wedges the row for its own sake: a terminal state permits no
	// lifecycle event, so nothing else could ever move it.
	//
	// preResurrectState and preResurrectArchivedAt are captured from the row we
	// read above, so a failed start can put it back exactly where it was rather
	// than resetting its trash age.
	preResurrectState := int(session.State)
	preResurrectArchivedAt := *session.ArchivedAt
	implementingState := int(machine.ImplementingPlan)
	resurrected, err := l.sessions.ResurrectToState(ctx, sessionID, implementingState)
	if err != nil {
		return nil, fmt.Errorf("resurrect session: %w", err)
	}
	if !resurrected {
		// The row stopped being archived between the read above and this write.
		// Another writer owns it now; proceeding would start an agent against a
		// session this call does not control.
		return nil, fmt.Errorf("resurrect session: %s was no longer archived", sessionID)
	}

	writeSetupProgress(setupOutput, "restarting agent")

	// Start Claude process, resuming previous session if available.
	var resume *string
	if session.AgentSessionID != nil {
		resume = session.AgentSessionID
	}

	// Account env sits above proof (disjoint keys today; account wins by
	// convention). No managed BOSS_* layer on the resume path. The repo's
	// stored LINEAR_API_KEY / SENTRY_* secrets are filled beneath the worktree
	// .env (OverlayWithRepo) so the resumed run keeps its own repo's Linear
	// workspace, not the daemon's ambient one.
	// BOS-381: the primary chat carries the authoritative provider/account/model.
	// Resolve it so a chat whose agent/account/model diverged from the session's
	// seed resumes under the right runner, credentials, and model.
	spawnSess := l.effectiveSpawnSession(ctx, session)
	resumeEnv := dotenv.OverlayWithRepo(mergeEnv(l.resolveAccountEnv(ctx, spawnSess), l.resolveProofEnv()), session.WorktreePath, repo)
	claudeSessionID, err := l.agentRunner.StartByAgent(ctx, spawnSess.AgentName, session.WorktreePath, session.Plan, resume, "", spawnSess.Model, resumeEnv)
	if err != nil {
		// Undo the un-archive (BOS-924). Without this the session is left live,
		// agent-less and un-retryable: the guard at the top of this function
		// rejects a row that is no longer archived, and a terminal state permits
		// no lifecycle event, so nothing would ever move it again.
		//
		// The compensating write runs on a detached, bounded context because ctx
		// may already be cancelled by whatever failed the start — the same reason
		// rollbackPreflightFailure detaches. Its own failure is logged, never
		// folded into the returned error: masking the start error would hide why
		// the resurrect failed in the first place.
		func() {
			rollbackCtx, cancelRollback := context.WithTimeout(context.WithoutCancel(ctx), preflightRollbackTimeout)
			defer cancelRollback()
			restored, rollbackErr := l.sessions.RollbackFailedResurrect(
				rollbackCtx, sessionID, preResurrectArchivedAt, preResurrectState, implementingState)
			switch {
			case rollbackErr != nil:
				l.logger.Error().
					Err(rollbackErr).
					Str("session", sessionID).
					Msg("resurrect rollback failed; session left live without an agent")
			case !restored:
				// A concurrent writer moved the row off the shape this call
				// wrote. Re-archiving anyway would stomp whatever it is doing,
				// so stop and name what we actually see instead of retrying
				// blind.
				observed := "unknown"
				if current, getErr := l.sessions.Get(rollbackCtx, sessionID); getErr == nil && current != nil {
					observed = current.State.String()
				}
				l.logger.Warn().
					Str("session", sessionID).
					Str("observedState", observed).
					Str("expectedState", machine.ImplementingPlan.String()).
					Msg("resurrect rollback skipped: session moved on before it could be re-archived")
			}
		}()
		return nil, fmt.Errorf("start claude: %w", err)
	}

	// Update Claude session ID. The state was already written above, with the
	// un-archive, so this write no longer carries it.
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		AgentSessionID: strPtr(claudeSessionID),
	}); err != nil {
		return nil, fmt.Errorf("update session: %w", err)
	}

	l.logger.Info().
		Str("session", sessionID).
		Str("claudeSession", claudeSessionID).
		Msg("session resurrected")

	return result, nil
}

// resolveOriginURL ensures the repo has a non-empty OriginURL. If it's
// empty (e.g. git remote get-url failed during initial registration), it
// re-detects the URL from the repo's local path and persists it.
func (l *Lifecycle) resolveOriginURL(ctx context.Context, repo *models.Repo) error {
	if repo.OriginURL != "" {
		return nil
	}

	url, err := l.worktrees.DetectOriginURL(ctx, repo.LocalPath)
	if err != nil {
		return fmt.Errorf("detect origin URL: %w", err)
	}
	if url == "" {
		return fmt.Errorf("repo %q has no origin remote configured", repo.DisplayName)
	}

	if _, err := l.repos.Update(ctx, repo.ID, db.UpdateRepoParams{
		OriginURL: &url,
	}); err != nil {
		return fmt.Errorf("persist origin URL: %w", err)
	}

	l.logger.Info().
		Str("repo", repo.ID).
		Str("originURL", url).
		Msg("re-detected and persisted origin URL")

	repo.OriginURL = url
	return nil
}

// killAllChatTmuxSessions kills the tmux session for every chat in the given
// boss session and clears the tmux_session_name on each chat record.
func (l *Lifecycle) killAllChatTmuxSessions(ctx context.Context, sessionID string) {
	if l.tmux == nil {
		return
	}
	chats, err := l.agentChats.ListBySession(ctx, sessionID)
	if err != nil {
		l.logger.Warn().Err(err).Str("session", sessionID).Msg("failed to list chats for tmux cleanup")
		return
	}
	for _, chat := range chats {
		if chat.TmuxSessionName == nil || *chat.TmuxSessionName == "" {
			continue
		}
		if err := l.tmux.KillSession(ctx, *chat.TmuxSessionName); err != nil {
			l.logger.Warn().Err(err).
				Str("session", sessionID).
				Str("agentSessionID", chat.AgentSessionID).
				Str("tmuxSession", *chat.TmuxSessionName).
				Msg("failed to kill chat tmux session during cleanup")
		} else {
			l.logger.Info().
				Str("session", sessionID).
				Str("agentSessionID", chat.AgentSessionID).
				Str("tmuxSession", *chat.TmuxSessionName).
				Msg("killed chat tmux session")
		}
		if err := l.agentChats.UpdateTmuxSessionName(ctx, chat.AgentSessionID, nil); err != nil {
			l.logger.Warn().Err(err).Str("agentSessionID", chat.AgentSessionID).Msg("failed to clear tmux name during cleanup")
		}
	}
}

// KillTmuxByName kills a tmux session by name and clears the
// TmuxSessionName field on the associated boss session record.
func (l *Lifecycle) KillTmuxByName(ctx context.Context, sessionID, tmuxName string) {
	if tmuxName == "" || l.tmux == nil || !l.tmux.Available(ctx) {
		return
	}
	if err := l.tmux.KillSession(ctx, tmuxName); err != nil {
		l.logger.Warn().Err(err).
			Str("session", sessionID).
			Str("tmuxSession", tmuxName).
			Msg("failed to kill tmux session during cleanup")
	} else {
		l.logger.Info().
			Str("session", sessionID).
			Str("tmuxSession", tmuxName).
			Msg("tmux session killed")
	}
	var nilName *string
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		TmuxSessionName: &nilName,
	}); err != nil {
		l.logger.Warn().Err(err).Str("session", sessionID).Msg("failed to clear tmux name during cleanup")
	}
}

// IsTmuxSessionAlive reports whether the given tmux session name is still
// running. Returns false when tmux is unavailable or the name is empty.
func (l *Lifecycle) IsTmuxSessionAlive(ctx context.Context, name string) bool {
	if name == "" || l.tmux == nil {
		return false
	}
	return l.tmux.HasSession(ctx, name)
}

// strPtr returns a double pointer to a string (for UpdateSessionParams).
func strPtr(s string) **string {
	p := &s
	return &p
}
