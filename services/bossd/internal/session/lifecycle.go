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
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/config"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossalib/sessionreason"
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

// settingUpTracker is the narrow slice of *status.DisplayTracker the
// lifecycle needs to drive the "initializing" display status. A local
// interface keeps the session package free of a status-package import
// dependency. *status.DisplayTracker satisfies it.
type settingUpTracker interface {
	SetSettingUp(sessionID string, on bool)
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

	// cronCompletionNotifier receives hookless poll completion candidates
	// for lifecycle-owned cron sessions.
	cronCompletionNotifier cronCompletionNotifier

	// reapFinalizer is the finalize entry the stranded-cron sweep
	// (RecoverStrandedCronSessions) routes dead strands through. nil means
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
	l.chatStatus.Update(agentSessionID, bossanovav1.ChatStatus_CHAT_STATUS_WORKING, time.Now())
	interval := l.headlessStatusPollInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	for {
		time.Sleep(interval)
		if !l.agentRunner.IsRunningByAgent(agentName, agentSessionID) {
			l.chatStatus.Update(agentSessionID, bossanovav1.ChatStatus_CHAT_STATUS_STOPPED, time.Now())
			return
		}
		// Refresh WORKING on every positive poll. The status tracker ages
		// entries out after status.StaleThreshold (~15s) and GetBatch then
		// surfaces them as STOPPED, so a single WORKING update at start would
		// make boss chat wait / get_chat_statuses / the orchestrator status
		// stream observe STOPPED while the headless agent is still running.
		// The poll interval (2s default) stays well under the threshold.
		l.chatStatus.Update(agentSessionID, bossanovav1.ChatStatus_CHAT_STATUS_WORKING, time.Now())
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
			HookPort:  int32(l.hookPort),
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
		// the session dead. Unwired liveness, or a session still alive (an
		// interactive tmux pane that survived the restart), leaves the row alone.
		if l.liveness == nil || l.liveness.IsSessionAlive(ctx, sess.ID) {
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
				l.SignalSessionRunComplete(sessionID, agentSessionID, "")
				return
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
func (l *Lifecycle) armHeadlessPollFallback(sessionID, agentSessionID string, session *models.Session) {
	if l.pollArmer == nil {
		l.logger.Debug().Str("session", sessionID).Msg("headless poll fallback: no pollArmer configured; skipping")
		return
	}
	client, err := l.agentClientFor(session)
	if err != nil || client == nil {
		l.logger.Debug().Err(err).Str("session", sessionID).Msg("headless poll fallback: no agent client; skipping")
		return
	}
	ctx := l.daemonCtx
	if ctx == nil {
		ctx = context.Background()
	}
	l.pollArmer.Arm(ctx, sessionID, agentSessionID, client)
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

	// TmuxUnattended routes this session through the durable tmux-hosted path
	// (like a cron session) instead of the headless detach path, and is
	// persisted so the completion gate and restart re-adoption recognise it.
	TmuxUnattended bool
}

// StartSession creates a worktree, starts a Claude process, and fires
// state machine events. It updates the session record with the worktree
// path, branch name, and Claude session ID.
//
// See StartSessionOpts for how to customize behavior. The zero-value opts
// preserve historical defaults: a fresh branch, setup script enabled,
// and an immediate draft PR for sessions without one.
func (l *Lifecycle) StartSession(ctx context.Context, sessionID string, opts StartSessionOpts) error {
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
	if opts.HookToken != "" {
		hookToken := &opts.HookToken
		updateParams.HookToken = &hookToken
	}
	if opts.TmuxUnattended {
		tmuxUnattended := true
		updateParams.TmuxUnattended = &tmuxUnattended
	}
	if _, err := l.sessions.Update(ctx, sessionID, updateParams); err != nil {
		return fmt.Errorf("set creating_worktree state: %w", err)
	}

	l.logger.Info().
		Str("session", sessionID).
		Str("repo", repo.LocalPath).
		Msg("creating worktree")

	// Determine setup script — skip it when the flag is set (e.g. dependabot PRs).
	setupScript := repo.SetupScript
	if skipSetupScript {
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
	var result *gitpkg.CreateResult
	if existingBranch != "" {
		result, err = l.worktrees.CreateFromExistingBranch(ctx, gitpkg.CreateFromExistingBranchOpts{
			RepoPath:          repo.LocalPath,
			BranchName:        existingBranch,
			WorktreeBaseDir:   repo.WorktreeBaseDir,
			RepoName:          repo.DisplayName,
			SetupScript:       setupScript,
			SetupScriptOutput: setupOutput,
		})
		if err != nil {
			// The branch may not exist on the remote yet (e.g. a Linear issue
			// with no PR). Fall back to creating a new branch with that name.
			l.logger.Info().
				Str("branch", existingBranch).
				Err(err).
				Msg("existing branch not found on remote, creating new branch")
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
		})
	}
	if err != nil {
		return fmt.Errorf("create worktree: %w", err)
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
	if opts.HookToken != "" {
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
			HookPort:  int32(l.hookPort),
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

	// Start the agent. Cron-spawned sessions and tmux_unattended sessions (e.g.
	// /boss-epic's durable tmux-hosted runs) run in a tmux-hosted Claude UI so
	// the user can attach to the live session. Interactive sessions (the TUI's
	// new-PR / existing-PR flows, and tracker-sourced Linear/Sentry sessions
	// created WITHOUT detach) are created idle: no agent runs yet, and it starts
	// interactively on first attach (RecordChat → tmux). A detached run
	// (opts.Detach) — `boss new --detach`, or `/boss-epic`'s headless
	// create_session fan-out — takes the headless `claude --print` / `codex
	// exec` path, because that caller wants an autonomous pass with a prompt.
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
	switch {
	case opts.CronJobID != "" || opts.TmuxUnattended:
		claudeSessionID, err = l.startTmuxChat(ctx, sessionID, opts, session, result)
		if err != nil {
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
		headlessEnv := dotenv.OverlayWithRepo(mergeEnv(l.resolveAccountEnv(ctx, session), l.resolveProofEnv()), result.WorktreePath, repo)
		claudeSessionID, err = l.agentRunner.StartByAgent(ctx, session.AgentName, result.WorktreePath, session.Plan, nil, "", session.Model, headlessEnv)
		if err != nil {
			return fmt.Errorf("start claude: %w", err)
		}
	}
	if hookResp != nil && !hookResp.GetIsSupported() {
		l.logger.Info().Str("session", sessionID).Msg("agent does not support finalize hook")
	}
	hooklessTmux := (opts.CronJobID != "" || opts.TmuxUnattended) && hookResp != nil && !hookResp.GetIsSupported()

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
		l.armHeadlessPollFallback(sessionID, claudeSessionID, session)
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
	// draft PR immediately so the user gets a PR right away. This covers
	// both plain "new PR" sessions and tracker-sourced sessions (e.g.
	// Linear tickets) — the latter carry a Plan but still need a PR up
	// front for visibility.
	//
	// Cron-spawned sessions opt out via opts.DeferPR — the Stop-hook
	// finalize path calls EnsurePR once the run actually produces commits.
	if session.PRNumber == nil && !opts.DeferPR {
		if err := l.createDraftPR(ctx, sessionID, result.WorktreePath, result.BranchName, session, repo); err != nil {
			l.setDraftPRBlockedReason(ctx, sessionID, err)
			l.logger.Warn().Err(err).
				Str("session", sessionID).
				Str("branch", result.BranchName).
				Msg("draft PR creation failed during session start; branch is preserved and retryable")
		}
	}

	l.logger.Info().
		Str("session", sessionID).
		Str("claudeSession", claudeSessionID).
		Msg("session started, implementing plan")

	return nil
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

// createDraftPR pushes the branch and creates a draft PR on GitHub,
// storing the PR number and URL on the session. Used during StartSession
// to create the PR immediately for any session without an existing one.
func draftPRBlockedReason(err error) string {
	return sessionreason.DraftPRCreationFailure(err)
}

func isDraftPRBlockedReason(reason *string) bool {
	return sessionreason.IsDraftPRCreationFailure(reason)
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

func (l *Lifecycle) clearDraftPRBlockedReason(ctx context.Context, sessionID string, session *models.Session) error {
	if !isDraftPRBlockedReason(session.BlockedReason) {
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
const draftPRPlaceholderCommitSubject = "chore: [skip ci] create pull request"

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

	verification, err := l.worktrees.VerifyPushedBranchAheadOfBase(ctx, worktreePath, branchName, session.BaseBranch)
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
func realCommitSubjects(raw []string) []string {
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		if t := strings.TrimSpace(s); t == "" || t == draftPRPlaceholderCommitSubject {
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
		if err := l.agentRunner.StopByAgent(stopSess.AgentName, *session.AgentSessionID); err != nil {
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
	session, err := l.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}

	// Stop Claude process if running. Route by the primary chat's provider
	// (BOS-381) so a chat switched to another agent is still stopped.
	stopSess := l.effectiveSpawnSession(ctx, session)
	if session.AgentSessionID != nil && l.agentRunner.IsRunningByAgent(stopSess.AgentName, *session.AgentSessionID) {
		if err := l.agentRunner.StopByAgent(stopSess.AgentName, *session.AgentSessionID); err != nil {
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
	if session.WorktreePath != "" && session.WorktreePath != repo.LocalPath {
		if err := l.worktrees.Archive(ctx, session.WorktreePath); err != nil {
			return fmt.Errorf("archive worktree: %w", err)
		}
	}

	// Mark session as archived in DB.
	if err := l.sessions.Archive(ctx, sessionID); err != nil {
		return fmt.Errorf("archive session: %w", err)
	}

	l.logger.Info().Str("session", sessionID).Msg("session archived")
	return nil
}

// ResurrectSession re-creates a worktree from an existing branch and
// starts a new Claude process (with --resume if a previous Claude session exists).
func (l *Lifecycle) ResurrectSession(ctx context.Context, sessionID string) error {
	session, err := l.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}

	if session.ArchivedAt == nil {
		return fmt.Errorf("session %s is not archived", sessionID)
	}

	repo, err := l.repos.Get(ctx, session.RepoID)
	if err != nil {
		return fmt.Errorf("get repo: %w", err)
	}

	l.logger.Info().
		Str("session", sessionID).
		Str("branch", session.BranchName).
		Msg("resurrecting session")

	// Resurrect worktree from existing branch.
	// Skip for quick chat sessions where WorktreePath is the base repo.
	if session.WorktreePath != repo.LocalPath {
		if err := l.worktrees.Resurrect(ctx, gitpkg.ResurrectOpts{
			RepoPath:     repo.LocalPath,
			WorktreePath: session.WorktreePath,
			BranchName:   session.BranchName,
			SetupScript:  repo.SetupScript,
		}); err != nil {
			return fmt.Errorf("resurrect worktree: %w", err)
		}
	}

	// Clear archived status.
	if err := l.sessions.Resurrect(ctx, sessionID); err != nil {
		return fmt.Errorf("resurrect session: %w", err)
	}

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
		return fmt.Errorf("start claude: %w", err)
	}

	// Update Claude session ID.
	implementingState := int(machine.ImplementingPlan)
	if _, err := l.sessions.Update(ctx, sessionID, db.UpdateSessionParams{
		AgentSessionID: strPtr(claudeSessionID),
		State:          &implementingState,
	}); err != nil {
		return fmt.Errorf("update session: %w", err)
	}

	l.logger.Info().
		Str("session", sessionID).
		Str("claudeSession", claudeSessionID).
		Msg("session resurrected")

	return nil
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
