package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/contract/daemon/socketauth"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossalib/kit/errortrack"
	libtelemetry "github.com/recurser/bossalib/kit/telemetry"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/account"
	"github.com/recurser/bossd/internal/agent"
	bcastsvc "github.com/recurser/bossd/internal/broadcast"
	"github.com/recurser/bossd/internal/cron"
	"github.com/recurser/bossd/internal/detach"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/plugin"
	"github.com/recurser/bossd/internal/rotation"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/status"
	"github.com/rs/zerolog"
	"golang.org/x/sync/singleflight"
)

const maxSetupOutputLineBytes = 256 * 1024

// DefaultSocketPath returns the default Unix socket path for the daemon.
// On macOS: ~/Library/Application Support/bossanova/bossd.sock
// On Linux: ~/.config/bossanova/bossd.sock
func DefaultSocketPath() (string, error) {
	dir, err := config.DefaultAppDataDir()
	if err != nil {
		return "", fmt.Errorf("resolve app data dir: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create socket dir: %w", err)
	}
	return filepath.Join(dir, "bossd.sock"), nil
}

// DefaultSocketPathForSettings returns the daemon socket path for loaded
// settings. socket_path wins over app_data_dir. When both are unset, existing
// platform defaults are preserved.
func DefaultSocketPathForSettings(settings config.Settings) (string, error) {
	if p, ok, err := config.ConfiguredSocketPath(settings); err != nil {
		return "", err
	} else if ok {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return "", fmt.Errorf("create socket dir: %w", err)
		}
		return p, nil
	}
	return DefaultSocketPath()
}

// Server wraps the ConnectRPC DaemonService handler and a Unix socket listener.
type Server struct {
	evidenceBucketOpener evidenceBucketOpener
	validateBranchName   func(context.Context, string) error
	repos                repoStore
	sessions             sessionStore
	agentChats           agentChatStore
	taskMappings         taskMappingFailureReader
	cronJobs             cronJobStore
	resumeAttempts       resumeAttemptReader
	// pendingTrackerMerges queues post-merge tracker writes that did not land,
	// for retryPendingTrackerMerges. Nil means a failed one is only reported.
	pendingTrackerMerges pendingTrackerMergeStore
	attentionCache       attentionCache
	githubCallbacks      githubCallbackStore
	telemetry            libtelemetry.Client
	// broadcasts persists broadcasts and their per-target deliveries (BOS-556);
	// broadcastResolver turns a validated selector into the frozen audience.
	// Both are optional (nil in tests/wiring that does not exercise the
	// broadcast surface), and the handlers fail with CodeUnavailable rather
	// than panicking when they are absent.
	broadcasts        broadcastStore
	broadcastResolver *bcastsvc.Resolver
	// broadcastEgress publishes a broadcast whose audience reaches beyond this
	// daemon up the reverse stream for bosso to route (BOS-558); daemonID is
	// this daemon's PERSISTED id, stamped on those events as the origin. Both
	// are optional: an unset publisher means "no upstream wiring", and the send
	// path then stays purely local rather than failing.
	broadcastEgress bcastsvc.EgressPublisher
	daemonID        string
	// notes persists free-text notes recorded against a repo (BOS-550).
	// Optional (nil where the note surface is not exercised); the handlers
	// report CodeUnavailable rather than panicking when it is absent.
	notes noteStore
	// noteSyncStates and noteSyncWorker back the note cloud sync (BOS-1435):
	// the outbox the SyncNotesNow counts come from, and the worker every
	// successful note write nudges. Both optional; the worker is nil on a
	// daemon with no upstream.
	noteSyncStates NoteSyncCounter
	noteSyncWorker NoteSyncNudger
	// broadcastSubscriptions persists standing subscriptions (BOS-557): the
	// rule that fires a broadcast when its owning session settles. Optional in
	// the same way as broadcasts; the handlers report CodeUnavailable when it
	// is absent. Firing is the evaluator's job, not this surface's — the
	// handlers narrow it to create/read/cancel.
	broadcastSubscriptions broadcastSubscriptionStore
	accounts               agentAccountStore
	rotationEngine         *rotation.Engine
	resolver               *account.Resolver
	accountCreds           AgentAccountCredentialStore
	agentAccountsChanged   func()
	addAccountMu           sync.Mutex
	accountSmoke           AgentAccountSmokeRunner
	usageProbe             UsageProbeRecorder
	// usageProbeSlots bounds all concurrent live usage probes on this daemon,
	// including probes started by independent ListAgentAccounts RPCs.
	usageProbeSlots     chan struct{}
	usageProbeSlotsOnce sync.Once
	// accountMaterializations purges an account's on-disk credential
	// materialization. Optional, may be nil — RemoveAgentAccount then behaves as it
	// did before the capability existed.
	accountMaterializations AgentAccountMaterializations
	checkSnapshots          checkSnapshotReader
	agentRuns               agentRunStore
	rotationEvents          rotationEventReader
	evidence                EvidenceSource
	cronScheduler           *cron.Scheduler
	// cronActivity is the agent-liveness seam used to derive cron STATUS
	// (RUNNING only when the last-run agent is actively working). Shares the
	// same instance as the scheduler's overlap check (cmd/main.go). Optional,
	// may be nil — cronJobStatus then falls back to session-state heuristics.
	cronActivity     cron.ActivityChecker
	chatStatus       *status.Tracker
	statusRecomputer status.Recomputer
	displayTracker   *status.DisplayTracker
	// archiveDeferrer holds archives deferred until every chat is idle.
	// Both deferrers are wired by New before the server handles requests.
	archiveDeferrer *archiveDeferrer
	// finishDeferrer defers explicit unattended-run finishes until the session settles.
	finishDeferrer *archiveDeferrer
	// prRefresher re-polls a single PR's display status on demand. MergeSession
	// uses it to repopulate the display tracker after every merge attempt (see
	// the HoldMerging block there). Optional, may be nil in tests / older wiring.
	prRefresher PRRefresher
	repairLease *status.RepairLeaseManager
	// transcriptTally reads a chat transcript's telemetry for the
	// restart-proof agent activity (wedgeActivity); nil means
	// tallyInteractiveAgentRunForServer (tests substitute it).
	transcriptTally TranscriptTallier
	// wedgeCache memoises the restart-proof agent activity, shared with the
	// reverse-stream hydrator (Config.WedgeActivityCache).
	wedgeCache *WedgeActivityCache
	tmuxPoller *status.TmuxStatusPoller
	lifecycle  *session.Lifecycle
	// promptReceipts confirms a Claude send by its UserPromptSubmit receipt
	// (BOS-1506). New derives it from Lifecycle; nil disables the receipt, which
	// leaves the pane verifier as the only signal.
	promptReceipts promptReceiptBook
	// bootstrapRunner owns session bootstraps: it runs them on the daemon's
	// context rather than the RPC's, so a client disconnect cannot abort one.
	bootstrapRunner *session.BootstrapRunner
	// switchAccountFn is the account-switch seam shared by the SwitchSessionAgentAccount
	// RPC and the SendChatMessage "/boss switch" interception. It defaults to
	// lifecycle.SwitchAgentAccount (see executeAccountSwitch); tests inject a spy so the
	// interception can be exercised without a real Lifecycle.
	switchAccountFn func(context.Context, session.SwitchAgentAccountParams) (session.SwitchAgentAccountResult, error)
	// switchFlightAttached is test-only: nil in production, where
	// executeAccountSwitch passes detach.Flight no option at all. When set it
	// fires once per caller that ATTACHES to the chat's switch flight, which is
	// what four time.Sleep calls in switch_account_budget_test.go previously
	// stood in for — three waiting for a joiner to attach before the held
	// leader was released, one draining for a publish that could not arrive.
	//
	// The attach moment is not here. detach.Flight owns the DoChan call, and
	// attachment is complete only once DoChan has returned, so the hook is
	// installed through detach.WithAttachHook and fires inside Flight; this
	// field is just the seam that carries the test's channel down to it.
	//
	// A field rather than a package-level var because three of those tests call
	// t.Parallel() under -race, where a shared global would be a data race and
	// would force t.Parallel() off all three. It mirrors switchAccountFn, the
	// seam the same file already uses.
	switchFlightAttached func(agentSessionID string)
	// switchRespawnBudgetOverride is test-only. Production leaves it zero and
	// resolves the budget from the tmux readiness setting below.
	switchRespawnBudgetOverride time.Duration
	agent                       agent.Runner
	agentClients                map[string]agent.RunnerClient
	worktrees                   worktreeManager
	provider                    vcs.Provider
	prResolver                  PRAssociationResolver
	pluginHost                  *plugin.Host
	chatTmux                    ChatTmuxSpawner
	tmux                        sessionStartReadiness
	chatWakeGroup               singleflight.Group // per-chat idempotency for WakeChat
	// chatSwitchGroup serializes account switches per chat, and it is the
	// mechanism BOS-897's teardown fix actually rests on — not the budget
	// beside it. A relay-bounded switch that outlives its caller leaves a user
	// pressing the button again; a resumable switch reuses the same
	// agentSessionID, and tmux.ChatSessionName is pure over repoID plus that
	// id, so both attempts compute the same tmux name. With nothing
	// serializing them the first attempt's failStartBestEffort kills the pane
	// the SECOND one established and stamps the chat start-failed. Admitting
	// one flight per chat removes the loser entirely: only the leader ever
	// calls SwitchAgentAccount, so no attempt is left to tear down a pane it does
	// not own. This holds on every route regardless of deadline arithmetic.
	//
	// A detach.Group rather than a bare singleflight.Group: the flight is
	// joined with DoChan and a caller-owned select, never Do (Do ignores a
	// joiner's context and would pin a 30s-relay joiner behind a 90s leader,
	// re-creating on the joiner the very "outlives its caller" defect this
	// removes — see upstream/terminal_stream.go's warning, BOS-885), and
	// DoChan's flight body MUST recover its own panic or the daemon dies. That
	// obligation is no longer prose: detach.Group exposes neither Do nor
	// DoChan, so detach.Flight — which always recovers — is the only way in.
	chatSwitchGroup detach.Group
	// mergeMu guards repoMergeGates. repoMergeGates serializes user-initiated
	// merges per repo (BOS-439): concurrent MergeSession calls for the same repo
	// share one local clone (.git/index.lock, refs/heads/<base>), so they must
	// not run at once. Mirrors the ref-counted-map lifecycle of the session
	// package's start/target gates, but acquisition is context-aware (see
	// acquireRepoMerge) so a queued merge whose caller cancels can bail without
	// merging.
	mergeMu              sync.Mutex
	repoMergeGates       map[string]*repoMergeGate
	wakeHook             wakeHook             // test-only: zero in production; see wake_chat.go
	mcpSurfaceHook       mcpSurfaceProber     // test-only: nil in production; see describe_chat_mcp.go
	keepCurrentSweepHook keepCurrentSweepHook // test-only: zero in production; see keep_current.go
	completionNotifier   session.CompletionNotifier
	authNotifier         AuthNotifier
	authStateReporter    AuthStateReporter
	onSessionDeleted     func(context.Context, string)
	onSessionUpdated     func(context.Context, *pb.Session)
	logger               zerolog.Logger
	fileLimitSoft        uint64
	// endpointReader and endpointNamespaceGate back the BOS-473 opt-in local
	// endpoint hydration. endpointReader is nil-safe (no hydration when unset);
	// endpointNamespaceGate is always non-nil after New (platform default).
	endpointReader        SessionEndpointReader
	endpointNamespaceGate func(clientNS string) bool
	passthroughStats      passthroughStatsProvider // nil until SetPassthroughStatsProvider; BOS-483
	fixedPortStatus       fixedPortStatusProvider  // nil until SetFixedPortStatusProvider; BOS-1632
	webhookEvidence       WebhookEvidence
	listener              net.Listener
	srv                   *http.Server
	agentLogsDir          string
	// protectedRoots is the symlink-resolved protected-root list the startup
	// diagnostic derived (BOS-725). The doctor check unions it with its own
	// lexical derivation so the two surfaces cannot disagree — see
	// protectedRootsReadableCheck. Nil unless the macOS startup diagnostic ran.
	protectedRoots []string

	// trackerResolver confirms tracker ids for SetSessionTracker. New builds
	// it over pluginHost; tests substitute a fake.
	trackerResolver session.TrackerIssueResolver

	// newVerifyVerbs builds the boss-verify toolbox runner ApproveSessionPR and
	// OperatorMergeSession tests use; production uses the constructor-injected factory.
	newVerifyVerbs      func(env []string) verifyVerbs
	verifyVerbsFactory  func([]string) VerifyVerbs
	environmentOverlay  func(map[string]string, string, *domain.Repo) map[string]string
	verifyProofKeys     []string
	protectedRootsProbe domain.TCCProbe

	// operatorMergeBudget bounds OperatorMergeSession; the zero value means
	// defaultOperatorMergeBudget. Tests shrink it.
	operatorMergeBudget operatorMergeBudget

	bossanovav1connect.UnimplementedDaemonServiceHandler
}

// repoMergeGate is a per-repo, capacity-1 serialization gate for user-initiated
// merges (BOS-439). sem is a buffered channel of capacity 1 used as a
// context-aware mutex; refs counts live acquirers so the map entry can be reaped
// when the last releases (the same lifecycle bossalib/keyedgate implements for
// the session start/target locks and the per-clone git gate).
type repoMergeGate struct {
	sem  chan struct{}
	refs int
}

type listSessionsPRAssociationResult struct {
	updated int64
	err     error
}

var (
	providerSessionIDBackgroundDiscoveryTimeout      = time.Minute
	providerSessionIDBackgroundDiscoveryPollInterval = time.Second
	// providerSessionIDLegacyBackfillTimeout bounds the legacy backfill on both
	// attach paths — ensureChatTmuxSession (record) and WakeChatInternal (wake).
	// That resolution allows the legacy time-window scan as its fallback, and a
	// codex rollout scan's cost scales with the corpus, so the 2s the fd path
	// gets in spawn_chat_tmux.go is the wrong size for it. (Since BOS-1298 the
	// call also carries the chat's live pane pid where one exists, so the fast
	// fd path can answer first — but the budget must still cover the scan that
	// runs when it cannot.) It is also not the minute above: that budget bounds a
	// genuinely background poll loop, whereas this one has a caller waiting on
	// chat creation.
	//
	// It must stay strictly BELOW the daemon→plugin unary RPC ceiling
	// (plugin.defaultPluginRPCTimeout, 30s), which bounds the same call one layer
	// down. At equal values the two deadlines race, and which one fires first
	// decides the error text an operator has to diagnose from. Keeping ours
	// smaller makes this budget the authoritative one. 20s still clears a
	// cold-cache scan of the shard window the prefilters leave (see
	// shardDirStartsAfter in the codex plugin, which bounds that window by the
	// legacy notAfter rather than by the chat's age) while staying visibly
	// foreground. Package-level var (not const) so tests can shrink it to
	// milliseconds.
	providerSessionIDLegacyBackfillTimeout = 20 * time.Second
	// providerSessionIDBackfillPersistTimeout bounds the UpdateProviderSessionID
	// write that records a backfilled id. Like the persist in
	// plugin.providerSessionIDForAgentSession it is deliberately NOT the scan's
	// context: a scan that returns an id near the end of its budget would
	// otherwise hand the write an all-but-spent deadline and throw away the id it
	// just found, and the caller's warn-and-continue would swallow that loss
	// silently — leaving every later attach to repeat the whole scan.
	providerSessionIDBackfillPersistTimeout = 5 * time.Second
	listSessionsPRAssociationTimeout        = 10 * time.Second
	interactiveAgentRunRecordTimeout        = 10 * time.Second
)

// Config holds all dependencies for creating a new Server.
type Config struct {
	EvidenceBucketOpener evidenceBucketOpener
	ChatTmux             ChatTmuxSpawner
	ProtectedRootsProbe  domain.TCCProbe
	NewVerifyVerbs       func([]string) VerifyVerbs
	EnvironmentOverlay   func(map[string]string, string, *domain.Repo) map[string]string
	VerifyProofKeys      []string
	// ValidateBranchName checks git branch syntax before session creation.
	// Required for session creation; the composition root supplies the git adapter.
	ValidateBranchName func(context.Context, string) error

	// AgentLogsDir is the daemon-owned directory containing live agent logs.
	AgentLogsDir   string
	Repos          repoStore
	Sessions       sessionStore
	AgentChats     agentChatStore
	TaskMappings   taskMappingFailureReader
	CronJobs       cronJobStore
	ResumeAttempts resumeAttemptReader
	// PendingTrackerMerges queues post-merge tracker writes to retry. Optional.
	PendingTrackerMerges pendingTrackerMergeStore
	// GithubCallbacks persists one-shot GitHub callback registrations (BOS-467).
	// May be nil in tests/contexts that do not exercise the callback surface.
	GithubCallbacks githubCallbackStore
	// Telemetry records daemon-owned terminal callback outcomes. Optional so
	// tests and minimal server wiring retain their existing behavior.
	Telemetry libtelemetry.Client
	// Broadcasts persists broadcasts and their per-target deliveries (BOS-556).
	// May be nil in tests/contexts that do not exercise the broadcast RPCs; the
	// handlers then report CodeUnavailable.
	Broadcasts broadcastStore
	// BroadcastResolver resolves a validated selector to the audience frozen
	// into a broadcast's delivery rows. Shares the daemon's chat and session
	// stores. May be nil alongside Broadcasts.
	BroadcastResolver *bcastsvc.Resolver
	// BroadcastEgress publishes a broadcast whose selector reaches beyond this
	// daemon up the daemon->bosso reverse stream, so bosso can route it to the
	// daemons that can resolve targets for it (BOS-558). Interface-typed so a
	// test can script one; may be nil, which means the upstream stream is not
	// wired and a send stays local — a publish is never required for the RPC to
	// succeed.
	BroadcastEgress bcastsvc.EgressPublisher
	// DaemonID is this daemon's PERSISTED id (upstream.ResolveDaemonID), never
	// a per-process value. It is stamped on every egress event as the origin,
	// and the receiving side's loop guard drops a command whose origin daemon
	// id equals its own — an id that changed per process would defeat that
	// guard and let a broadcast echo back into a storm. May be empty when no
	// upstream is configured; the egress predicate then infers nothing from a
	// selector (see broadcast.ReachesBeyondDaemon).
	DaemonID string
	// Notes persists free-text notes recorded against a repo (BOS-550). May be
	// nil in tests/contexts that do not exercise the note RPCs; the handlers
	// then report CodeUnavailable.
	Notes noteStore
	// NoteSyncStates is the note sync outbox (BOS-1435) that SyncNotesNow
	// counts. May be nil; SyncNotesNow then reports CodeUnavailable.
	NoteSyncStates NoteSyncCounter
	// NoteSyncWorker is nudged after every successful note write and by
	// SyncNotesNow. Nil on a daemon not wired to Bosso, which runs no worker;
	// pass a nil interface, never a typed nil pointer.
	NoteSyncWorker NoteSyncNudger
	// BroadcastSubscriptions persists standing broadcast subscriptions
	// (BOS-557). May be nil in tests/contexts that do not exercise the
	// subscription RPCs; the handlers then report CodeUnavailable.
	BroadcastSubscriptions broadcastSubscriptionStore
	Accounts               agentAccountStore
	// RotationEngine is the account-rotation policy engine, held on the daemon
	// for future auto-rotate consumers (BOS-174/175). Optional, may be nil.
	RotationEngine *rotation.Engine
	// Resolver applies the creation-time default-account policy (which registry
	// account a new session binds to when the client omits account_id).
	// Optional, may be nil (older/test wiring then leaves sessions on account 0).
	Resolver *account.Resolver
	// AccountCredentials is the keyring-backed credential-blob plane for
	// accounts (satisfied by accountcred.Store on the daemon). Metadata lives
	// in Accounts; secrets never touch SQLite. Optional, may be nil (account
	// RPCs then fail closed with a clear error).
	AccountCredentials AgentAccountCredentialStore
	// AgentAccountsChanged invalidates the upstream metadata inventory after account changes.
	AgentAccountsChanged func()
	// AgentAccountSmokeRunner runs TestAgentAccount provider verification. Optional, may be nil;
	// when nil, TestAgentAccount records live_smoke_ran=false with an unavailable
	// detail.
	AccountSmokeRunner AgentAccountSmokeRunner
	// UsageProbe refreshes cached account usage metadata for list refreshes.
	// Optional, may be nil; refresh then falls back to cached values.
	UsageProbe UsageProbeRecorder
	// AgentAccountMaterializations purges an account's on-disk credential
	// materialization on removal. Optional, may be nil; RemoveAgentAccount then
	// leaves the materialized tree alone, exactly as it did before.
	AccountMaterializations AgentAccountMaterializations
	CheckSnapshots          checkSnapshotReader
	AgentRuns               agentRunStore
	// RotationEvents is the rotation audit store hydrated onto
	// Session.rotation_events for TUI/web history (BOS-176). Nil-safe.
	RotationEvents rotationEventReader
	// WebhookEvidence flags repos whose GitHub webhooks never reach the daemon
	// (upstream.WebhookHealth), surfaced as Repo.is_missing_webhooks by
	// ListRepos. Nil on a daemon with no upstream; pass a nil interface, never
	// a typed nil pointer.
	WebhookEvidence WebhookEvidence
	// Evidence serves each session's current-head verify evidence summary for
	// GetSessionTimeline (*evidence.Reader). Optional, may be nil; the
	// timeline then omits the evidence step.
	Evidence EvidenceSource
	// WedgeActivityCache memoises the restart-proof agent activity
	// (WedgeActivity). Pass the reverse-stream hydrator's cache so both hand
	// clients the same value. Optional; nil gives the server its own.
	WedgeActivityCache *WedgeActivityCache
	CronScheduler      *cron.Scheduler
	// CronActivity is the agent-liveness checker shared with the cron
	// scheduler's overlap suppression; cronJobStatus uses it so the TUI STATUS
	// column reads RUNNING only for an actively-working last run. Optional, may
	// be nil (STATUS then falls back to session-state heuristics).
	CronActivity cron.ActivityChecker
	ChatStatus   *status.Tracker
	// StatusRecomputer refreshes the display state after a callback changes
	// what an otherwise-idle chat is waiting for. Optional for minimal test
	// wiring; the periodic sweep remains the recovery path when it is absent.
	StatusRecomputer status.Recomputer
	DisplayTracker   *status.DisplayTracker
	// PRRefresher re-polls one PR's display status on demand, repopulating the
	// display tracker. Satisfied by *session.DisplayPoller. Optional, may be
	// nil; MergeSession then simply skips the post-merge re-poll and the next
	// scheduled poll repairs the label instead.
	PRRefresher PRRefresher
	RepairLease *status.RepairLeaseManager
	TmuxPoller  *status.TmuxStatusPoller
	Lifecycle   *session.Lifecycle
	// BootstrapRunner runs session bootstraps on the daemon's root context.
	// Optional: when nil and Lifecycle is set, New derives one from Lifecycle so
	// existing wiring (and tests) keep working — but real daemon wiring MUST
	// supply one built on the context Shutdown cancels, or bootstraps outlive it.
	BootstrapRunner    *session.BootstrapRunner
	Agent              agent.Runner
	AgentClients       map[string]agent.RunnerClient
	Worktrees          worktreeManager
	Provider           vcs.Provider
	PRResolver         PRAssociationResolver // optional, may be nil
	PluginHost         *plugin.Host
	Tmux               sessionStartReadiness
	CompletionNotifier session.CompletionNotifier // optional, may be nil
	AuthNotifier       AuthNotifier               // optional, nil in local-only mode
	// AuthStateReporter answers the GetAuthState diagnostic RPC. Optional and
	// nil in local-only mode, where the handler reports
	// upstream_configured=false rather than an error — "this daemon has no
	// upstream" is a real answer, not a failure to produce one.
	AuthStateReporter AuthStateReporter
	// OnSessionDeleted, when non-nil, is invoked after a session row is
	// removed from the local DB. cmd/main.go wires this to publish a
	// SessionDelta_KIND_DELETED on the reverse stream so bosso's in-memory
	// Registry stops surfacing the deleted session in the web UI. Without
	// this callback bosso only learns about deletions on daemon reconnect
	// (when it replaces its state with a fresh DaemonSnapshot).
	OnSessionDeleted func(context.Context, string)
	// OnSessionUpdated, when non-nil, is invoked after local session
	// mutations that should replace bosso's in-memory session projection.
	// cmd/main.go wires this to publish SessionDelta_KIND_UPDATED on the
	// reverse stream; archive/resurrect depend on it because they hide/show
	// rows in the default web session list without deleting the DB row.
	OnSessionUpdated func(context.Context, *pb.Session)
	Logger           zerolog.Logger
	// FileLimitSoft is the RLIMIT_NOFILE soft limit the daemon achieved at
	// startup (0 = unknown/non-unix). RepairDoctor surfaces a WARN when it is
	// below the safe floor (BOS-465).
	FileLimitSoft uint64
	// EndpointReader supplies verified machine-local HTTP endpoints for opted-in
	// local session reads (BOS-473), backed by the sessionports tracker in
	// cmd/main.go. Optional/nil-safe: when nil, GetSession/ListSessions never
	// hydrate Session.http_endpoints.
	EndpointReader SessionEndpointReader
	// EndpointNamespaceGate overrides the platform network-namespace check used
	// to gate endpoint hydration (a test seam). Nil selects the platform default
	// (platformEndpointNamespaceGate): fail-closed exact match on Linux, always
	// satisfied off Linux.
	EndpointNamespaceGate func(clientNS string) bool
	// ProtectedRoots is the symlink-resolved protected-root list the macOS
	// startup diagnostic derived. The doctor check probes these in addition to
	// the roots it derives lexically, so a working path that only reaches a
	// protected root through a symlink is still reported (BOS-725). Nil unless
	// the macOS startup diagnostic ran.
	ProtectedRoots []string
}

// AuthNotifier is the narrow interface the NotifyAuthChange RPC
// delegates to. Under the legacy heartbeat Manager this was the full
// upstream.Manager; under the new StreamClient wiring it's satisfied by
// a small adapter in cmd/main.go that toggles the stream's
// reconnect-with-new-token path. Defined here rather than in the
// upstream package so the server doesn't need to import upstream just
// to depend on this one interface.
type AuthNotifier interface {
	NotifyLogin(ctx context.Context, repoIDs []string) (LoginVerdict, error)
	NotifyLogout()
}

// LoginOutcome enumerates what the daemon actually observed when it reloaded
// credentials after a `boss login`. It is the non-secret half of LoginVerdict.
//
// It lives here, next to AuthNotifier and for the same reason in reverse: the
// adapter in cmd/main.go produces these values, and keeping them out of the
// generated proto package means cmd does not have to import pb just to answer
// this one question. NotifyAuthChange maps them onto the wire enum.
type LoginOutcome int

const (
	// LoginOutcomeUnspecified is the zero value: nothing was evaluated. It is
	// what a logout, a nil notifier, or a daemon too old to have a verdict
	// reports, and it must render as silence rather than as a false success.
	LoginOutcomeUnspecified LoginOutcome = iota
	// LoginOutcomeOK means the reloaded record was usable and the auth gate
	// was cleared.
	LoginOutcomeOK
	// LoginOutcomeCredentialsFlagged means the reloaded record still carried a
	// persisted re-login marker, so the auth gate was NOT cleared.
	LoginOutcomeCredentialsFlagged
	// LoginOutcomeCredentialsMissing means the reloaded record carried no
	// access token and no marker, so the auth gate was NOT cleared. Distinct
	// from Flagged because the two have opposite causes — a rejected refresh
	// token versus a login whose credentials never reached this process — and
	// therefore opposite remedies.
	LoginOutcomeCredentialsMissing
	// LoginOutcomeRegisterFailed means credentials were clean and the auth
	// gate WAS cleared, but the proactive re-register failed. The reactive
	// path remains the backstop, so this is reported, not fatal.
	LoginOutcomeRegisterFailed
)

// LoginVerdict is the daemon's answer to "did that login actually leave me
// able to talk upstream?". ReloginReason is one of the enumerated persisted
// re-login reasons and is set only for LoginOutcomeCredentialsFlagged; it is
// never token material.
type LoginVerdict struct {
	Outcome       LoginOutcome
	ReloginReason string
}

// loginVerdictToProto maps a LoginVerdict onto the wire response. An outcome
// this function does not recognise maps to OUTCOME_UNSPECIFIED rather than to
// a guess, keeping "we do not know" distinguishable from "OK".
func loginVerdictToProto(v LoginVerdict) *pb.NotifyAuthChangeResponse {
	resp := &pb.NotifyAuthChangeResponse{}
	switch v.Outcome {
	case LoginOutcomeOK:
		resp.Outcome = pb.NotifyAuthChangeResponse_OUTCOME_OK
	case LoginOutcomeCredentialsFlagged:
		resp.Outcome = pb.NotifyAuthChangeResponse_OUTCOME_CREDENTIALS_FLAGGED
		// relogin_reason is documented as empty for every other outcome, and
		// this mapper is the single wire boundary, so it is the only place
		// that invariant can actually be enforced rather than merely relied on.
		resp.ReloginReason = v.ReloginReason
	case LoginOutcomeCredentialsMissing:
		resp.Outcome = pb.NotifyAuthChangeResponse_OUTCOME_CREDENTIALS_MISSING
	case LoginOutcomeRegisterFailed:
		resp.Outcome = pb.NotifyAuthChangeResponse_OUTCOME_REGISTER_FAILED
	default:
		resp.Outcome = pb.NotifyAuthChangeResponse_OUTCOME_UNSPECIFIED
	}
	return resp
}

// AuthStateReporter is the read-only counterpart to AuthNotifier: it reports
// what the RUNNING daemon currently knows about its own upstream auth, which
// the GetAuthState RPC renders for `boss daemon doctor`.
//
// It is deliberately a second, separate interface rather than two more methods
// on AuthNotifier. AuthNotifier is a command surface — a caller that holds it
// can change the daemon's auth behaviour — and a diagnostic has no business
// requiring that authority. Keeping them apart also means a build with no
// reporter wired still compiles against every existing AuthNotifier
// implementation.
//
// Like AuthNotifier it is nil in local-only mode, where there is no upstream to
// report on. Implementations must be safe for concurrent use: the RPC handler
// calls this from a request goroutine while the stream loop mutates the same
// state.
type AuthStateReporter interface {
	// AuthState returns a snapshot of the daemon's live upstream auth state.
	// It must never return token material: the caller renders these fields
	// verbatim to an operator's terminal.
	AuthState(ctx context.Context) DaemonAuthState
}

// DaemonAuthState is the scalar, non-secret snapshot AuthStateReporter returns.
// It mirrors the wire message field for field, but is declared here so the
// upstream package's adapter does not have to import the generated protobuf
// types just to answer a diagnostic.
type DaemonAuthState struct {
	// NeedsLogin is the daemon's own "I have parked until someone runs
	// `boss login`" flag. It is NOT a fault signal on its own — it is the
	// correct, expected state right after a deliberate `boss logout`.
	NeedsLogin bool
	// ReloginReason is the enumerated, non-secret marker persisted alongside
	// the credentials ("refresh_outcome_unknown", "refresh_token_rejected"),
	// or "" when none. Treat an unrecognised value as forward-compatible:
	// render it rather than mapping it to "unknown".
	ReloginReason string
	// LastRegisteredAt is when this daemon process last completed a successful
	// upstream registration. Zero when it never has. It is context, not a
	// liveness floor: a long-lived healthy daemon legitimately registers once
	// and never again.
	LastRegisteredAt time.Time
	// Connected is the coarse "the last handshake reached snapshot-accepted"
	// signal. False during any ordinary reconnect gap, so it is never a
	// verdict by itself.
	Connected bool
	// AuthFailingSince is the discriminating BOS-942 fact: the start of the
	// current unbroken run of unrecoverable auth failures, zero when auth is
	// not currently failing. A daemon with NeedsLogin false and a non-zero
	// AuthFailingSince is wedged, and nothing else in the system says so.
	AuthFailingSince time.Time
}

// PRAssociationResolver refreshes local session-to-PR links.
type PRAssociationResolver interface {
	ReconcileSessions(context.Context, []*models.Session) (int64, error)
}

// PRRefresher re-polls a single PR's display status on demand, repopulating the
// display tracker with the live status. It is defined here (rather than
// importing session) so the server depends only on this narrow surface;
// *session.DisplayPoller satisfies it. MergeSession calls it after every merge
// attempt on a server wired with a display tracker, so the label reflects what
// the daemon just did (see the HoldMerging block).
//
// It deliberately names the WithoutWebhookCredit variant: a refresh the daemon
// triggered itself is not evidence that the session's webhook pipeline is
// healthy, and crediting one would back that session's scheduled poll interval
// off — leaving a still-active session staler after a blocked or failed merge
// than before it.
type PRRefresher interface {
	RefreshPRWithoutWebhookCredit(ctx context.Context, repoOriginURL string, prNumber int) error
}

// accountCredentialLocker is implemented by accountcred.Store. It is kept
// optional so custom credential stores and existing test fakes do not need to
// provide locking, while production refreshes share the materializer's
// per-account load-merge-save lock.
type accountCredentialLocker interface {
	WithCredentialLock(accountID string, fn func() error) error
}

// AgentAccountSmokeRunner runs a trivial live provider invocation to prove a
// credential works (claude -p "ok" / codex equivalent). It is injected and
// nil-safe: when absent, TestAgentAccount records an unavailable smoke result.
type AgentAccountSmokeRunner interface {
	Smoke(ctx context.Context, accountID, provider string, blob []byte) error
}

// UsageProbeRecorder probes and caches usage metadata for one account. It is
// fail-soft at call sites: refresh errors never make ListAgentAccounts fail.
type UsageProbeRecorder interface {
	RecordUsageProbe(ctx context.Context, accountID string) error
}

// AgentAccountMaterializations purges the on-disk credential materialization for one
// account (the plaintext tree an agent runner reads, e.g. codex's auth.json).
// Optional: a nil capability leaves RemoveAgentAccount behaving as before. The
// implementation is provider-aware — the RPC always calls it and lets the
// adapter no-op for providers that materialize nothing.
type AgentAccountMaterializations interface {
	RemoveMaterialization(ctx context.Context, provider, accountID string) error
}

// New creates a new Server wired to the given stores and lifecycle orchestrator.
func New(cfg Config) *Server {
	srv := &Server{
		chatTmux:            cfg.ChatTmux,
		protectedRootsProbe: cfg.ProtectedRootsProbe,
		verifyVerbsFactory:  cfg.NewVerifyVerbs,
		environmentOverlay:  cfg.EnvironmentOverlay,
		verifyProofKeys:     cfg.VerifyProofKeys,
		validateBranchName:  cfg.ValidateBranchName,
		repos:               cfg.Repos,
		sessions:            cfg.Sessions,
		agentChats:          cfg.AgentChats,
		taskMappings:        cfg.TaskMappings,
		cronJobs:            cfg.CronJobs,
		resumeAttempts:      cfg.ResumeAttempts,

		pendingTrackerMerges: cfg.PendingTrackerMerges,
		githubCallbacks:      cfg.GithubCallbacks,
		telemetry:            cfg.Telemetry,

		broadcasts:             cfg.Broadcasts,
		broadcastResolver:      cfg.BroadcastResolver,
		broadcastEgress:        cfg.BroadcastEgress,
		daemonID:               cfg.DaemonID,
		notes:                  cfg.Notes,
		evidenceBucketOpener:   cfg.EvidenceBucketOpener,
		noteSyncStates:         cfg.NoteSyncStates,
		noteSyncWorker:         cfg.NoteSyncWorker,
		broadcastSubscriptions: cfg.BroadcastSubscriptions,

		accounts:                cfg.Accounts,
		rotationEngine:          cfg.RotationEngine,
		resolver:                cfg.Resolver,
		accountCreds:            cfg.AccountCredentials,
		agentAccountsChanged:    cfg.AgentAccountsChanged,
		accountSmoke:            cfg.AccountSmokeRunner,
		usageProbe:              cfg.UsageProbe,
		accountMaterializations: cfg.AccountMaterializations,

		checkSnapshots:   cfg.CheckSnapshots,
		agentRuns:        cfg.AgentRuns,
		rotationEvents:   cfg.RotationEvents,
		evidence:         cfg.Evidence,
		webhookEvidence:  cfg.WebhookEvidence,
		wedgeCache:       cfg.WedgeActivityCache,
		cronScheduler:    cfg.CronScheduler,
		cronActivity:     cfg.CronActivity,
		chatStatus:       cfg.ChatStatus,
		statusRecomputer: cfg.StatusRecomputer,
		displayTracker:   cfg.DisplayTracker,
		prRefresher:      cfg.PRRefresher,
		repairLease:      cfg.RepairLease,
		tmuxPoller:       cfg.TmuxPoller,
		lifecycle:        cfg.Lifecycle,
		bootstrapRunner:  cfg.BootstrapRunner,

		agent:        cfg.Agent,
		agentClients: cfg.AgentClients,
		worktrees:    cfg.Worktrees,
		provider:     cfg.Provider,

		prResolver:         cfg.PRResolver,
		pluginHost:         cfg.PluginHost,
		trackerResolver:    NewTrackerResolver(cfg.PluginHost),
		tmux:               cfg.Tmux,
		repoMergeGates:     map[string]*repoMergeGate{},
		completionNotifier: cfg.CompletionNotifier,
		authNotifier:       cfg.AuthNotifier,
		authStateReporter:  cfg.AuthStateReporter,
		onSessionDeleted:   cfg.OnSessionDeleted,
		onSessionUpdated:   cfg.OnSessionUpdated,
		logger:             cfg.Logger,
		fileLimitSoft:      cfg.FileLimitSoft,
		endpointReader:     cfg.EndpointReader,
		agentLogsDir:       cfg.AgentLogsDir,
		protectedRoots:     cfg.ProtectedRoots,

		endpointNamespaceGate: cfg.EndpointNamespaceGate,
	}
	if srv.endpointNamespaceGate == nil {
		srv.endpointNamespaceGate = platformEndpointNamespaceGate
	}
	if cfg.Lifecycle != nil {
		srv.promptReceipts = cfg.Lifecycle
		cfg.Lifecycle.SetChatCreatedNotifier(srv.cancelPendingForNewChat)
	}
	if srv.wedgeCache == nil {
		srv.wedgeCache = NewWedgeActivityCache()
	}
	if srv.bootstrapRunner == nil && cfg.Lifecycle != nil {
		// Fallback for wiring that predates the runner (and for tests that build a
		// Server from a Lifecycle alone). context.Background() is deliberately the
		// WRONG context for the daemon — main.go passes the root context Shutdown
		// cancels — but it is the right one here: a nil runner would panic on the
		// first non-quick-chat create.
		srv.bootstrapRunner = session.NewBootstrapRunner(context.Background(), cfg.Lifecycle, cfg.Logger)
	}
	srv.initDeferrers()
	return srv
}

// passthroughStatsProvider exposes the failover proxy's bounded pass-through
// error tally to RepairDoctor. *ProxyServer satisfies it; the seam is a setter
// (SetPassthroughStatsProvider) because the ProxyServer is constructed AFTER
// server.New (main.go), so it cannot arrive through server.Config (BOS-483).
type passthroughStatsProvider interface {
	PassthroughStatsSnapshot() []domain.PassthroughSessionSnapshot
}

// fixedPortStatusProvider exposes the failover proxy's configured and live ports
// to RepairDoctor. *ProxyServer satisfies it; it is wired after server.New.
type fixedPortStatusProvider interface {
	FixedPortLost() bool
	ConfiguredPort() int
	Port() int
}

// SetFixedPortStatusProvider wires the failover proxy's port status into the
// daemon so RepairDoctor can surface a lost fixed port. Nil-safe on the read
// side; a daemon with no proxy has no fixed port to report.
func (s *Server) SetFixedPortStatusProvider(p fixedPortStatusProvider) {
	s.fixedPortStatus = p
}

// SetPassthroughStatsProvider wires the failover proxy's pass-through counters
// into the daemon so RepairDoctor can surface them as an informational check.
// Nil-safe on the read side; a daemon with no proxy simply omits the tally.
func (s *Server) SetPassthroughStatsProvider(p passthroughStatsProvider) {
	s.passthroughStats = p
}

// RotationEngine returns the account-rotation policy engine held on the
// daemon. It is wired but not yet invoked on any cap signal (BOS-174/175
// own the live rotation flow). May be nil if no account store was configured.
func (s *Server) RotationEngine() *rotation.Engine { return s.rotationEngine }

// Broadcasts returns the broadcast store held on the daemon (BOS-556), wired at
// startup and exposed through the Send/List/DeleteBroadcast RPC handlers. May
// be nil in contexts that did not configure one.
func (s *Server) Broadcasts() broadcastStore { return s.broadcasts }

// Notes returns the note store held on the daemon (BOS-550), wired at startup
// and exposed through the Create/Get/List/Update/DeleteNote RPC handlers. May
// be nil in contexts that did not configure one.
func (s *Server) Notes() noteStore { return s.notes }

// BroadcastSubscriptions returns the standing-subscription store held on the
// daemon (BOS-557), wired at startup and exposed through the
// Create/List/DeleteBroadcastSubscription RPC handlers. May be nil in contexts
// that did not configure one.
func (s *Server) BroadcastSubscriptions() broadcastSubscriptionStore {
	return s.broadcastSubscriptions
}

// Listen binds a Unix socket and initializes the underlying http.Server
// synchronously. After Listen returns the server is fully configured and
// Shutdown can be called safely; use Serve (typically in a goroutine) to
// accept requests.
//
// Splitting bind-and-configure from Serve eliminates a race where a caller
// firing Shutdown before the serving goroutine had written s.srv would
// observe a nil pointer (or worse, race the write under -race).
func (s *Server) Listen(socketPath string) error {
	// Defense in depth behind the singleton flock: if the socket is still being
	// served by a live daemon, refuse to start rather than os.Remove + rebind
	// (which would steal the socket from the running instance). The flock guard
	// in cmd/run should already prevent reaching here, but a socket left by a
	// process that crashed without releasing its lock would otherwise be stolen.
	if socketIsServing(socketPath) {
		return fmt.Errorf("socket %s is already served by a running daemon", socketPath)
	}

	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return fmt.Errorf("create socket dir: %w", err)
	}

	// Remove only stale socket files from previous runs. Never unlink an
	// arbitrary configured path.
	if info, err := os.Lstat(socketPath); err == nil {
		if info.Mode().Type() != os.ModeSocket {
			return fmt.Errorf("socket path exists and is not a socket: %s", socketPath)
		}
		if err := os.Remove(socketPath); err != nil {
			return fmt.Errorf("remove stale socket: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat socket path: %w", err)
	}

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen unix %s: %w", socketPath, err)
	}
	s.listener = ln

	// Make socket accessible only to the current user: owner read/write only.
	// Connecting to a Unix domain socket needs the write bit, not execute, so
	// 0o600 is functionally equivalent for an owner-only socket and strictly
	// more restrictive than 0o700 (drops the unused execute bit).
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = ln.Close()
		return fmt.Errorf("chmod socket: %w", err)
	}

	// Application-level auth (defence-in-depth behind the 0700 socket perms):
	// generate/load a stable Bearer token co-located with the socket and reject
	// any RPC lacking it. Never log the token itself.
	token, err := socketauth.LoadOrCreateToken(socketPath)
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("load socket auth token: %w", err)
	}

	mux := http.NewServeMux()
	path, handler := bossanovav1connect.NewDaemonServiceHandler(s, connect.WithInterceptors(
		socketauth.NewServerInterceptor(token),
		errortrack.Interceptor(),
	))
	mux.Handle(path, withStreamingWriteDeadlineOverride(handler))
	registerPprofHandlers(mux)

	s.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Keep this generic response ceiling equal to
		// config.SwitchResultCeiling; TestServerListenSetsWriteTimeout guards the
		// cross-service account-switch boundary.
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	return nil
}

// writeDeadlineExemptProcedures are the DaemonService procedures that
// legitimately outlive the daemon's generic non-streaming write timeout.
// CreateSession and ResurrectSession stream repo setup-script output. GetRunCost
// can backfill transcript archives inside one long unary response. The 120s
// WriteTimeout would otherwise cut each healthy response off before its
// operation-level deadline.
// UploadEvidence owns its ten-minute operation ceiling.
//
// Keying on the generated constants makes a rename a build failure. Adding a
// long-running response RPC without adding it here reproduces the same defect:
// a healthy daemon reporting deadline_exceeded because the transport write
// ceiling fired first. Extend this set for such procedures.
var writeDeadlineExemptProcedures = map[string]struct{}{
	bossanovav1connect.DaemonServiceUploadEvidenceProcedure:    {},
	bossanovav1connect.DaemonServiceWatchSessionSetupProcedure: {},
	bossanovav1connect.DaemonServiceCreateSessionProcedure:     {},
	bossanovav1connect.DaemonServiceResurrectSessionProcedure:  {},
	bossanovav1connect.DaemonServiceGetRunCostProcedure:        {},
	// OperatorMergeSession runs detached for up to config.OperatorMergeCeiling,
	// so a merge queued behind another still reports its outcome.
	bossanovav1connect.DaemonServiceOperatorMergeSessionProcedure: {},
}

func withStreamingWriteDeadlineOverride(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := writeDeadlineExemptProcedures[r.URL.Path]; ok {
			_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
		}
		next.ServeHTTP(w, r)
	})
}

// Serve blocks serving requests on the listener created by Listen. It
// returns http.ErrServerClosed when Shutdown completes successfully.
func (s *Server) Serve() error {
	return s.srv.Serve(s.listener)
}

// ListenAndServe is a convenience for Listen followed by Serve. Callers
// that need to invoke Shutdown concurrently with the serving goroutine
// should prefer the two-phase API (Listen sync, Serve in a goroutine).
func (s *Server) ListenAndServe(socketPath string) error {
	if err := s.Listen(socketPath); err != nil {
		return err
	}
	return s.Serve()
}

// Shutdown gracefully stops the server. Safe to call before or after
// Serve, but the caller must ensure Listen has returned first.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.srv != nil {
		return s.srv.Shutdown(ctx)
	}
	return nil
}

// --- Repo Validation ---

func (s *Server) ValidateRepoPath(ctx context.Context, req *connect.Request[pb.ValidateRepoPathRequest]) (*connect.Response[pb.ValidateRepoPathResponse], error) {
	localPath := req.Msg.LocalPath
	if localPath == "" {
		return connect.NewResponse(&pb.ValidateRepoPathResponse{
			IsValid:      false,
			ErrorMessage: "path is required",
		}), nil
	}

	// Check path exists and is a directory.
	info, err := os.Stat(localPath)
	if err != nil {
		return connect.NewResponse(&pb.ValidateRepoPathResponse{
			IsValid:      false,
			ErrorMessage: fmt.Sprintf("path does not exist: %s", localPath),
		}), nil
	}
	if !info.IsDir() {
		return connect.NewResponse(&pb.ValidateRepoPathResponse{
			IsValid:      false,
			ErrorMessage: fmt.Sprintf("path is not a directory: %s", localPath),
		}), nil
	}

	// Check it's a git repo.
	if !s.worktrees.IsGitRepo(ctx, localPath) {
		return connect.NewResponse(&pb.ValidateRepoPathResponse{
			IsValid:      false,
			ErrorMessage: fmt.Sprintf("not a git repository: %s", localPath),
		}), nil
	}

	// Detect metadata.
	originURL, _ := s.worktrees.DetectOriginURL(ctx, localPath)
	originURL = domain.CloneURLWithoutCredentials(originURL)
	defaultBranch, _ := s.worktrees.DetectDefaultBranch(ctx, localPath)

	return connect.NewResponse(&pb.ValidateRepoPathResponse{
		IsValid:       true,
		OriginUrl:     originURL,
		IsGithub:      vcs.IsGitHubURL(originURL),
		DefaultBranch: defaultBranch,
	}), nil
}

// ListAgents returns the names + GetInfo metadata of every AgentRunner
// plugin currently wired into the daemon. The boss TUI uses this to
// drive the agent picker in `boss session new` and to surface plugin
// versions in /diagnose. A plugin whose GetInfo errors is included with
// only Name populated rather than dropped silently — operators see the
// failure and can still pick the plugin.
//
// Sourced from s.agentClients (the same registry the orchestrator and
// status poller use) rather than the plugin host, so a future test can
// inject fakes without spinning up a real go-plugin subprocess. Returns
// agents in lexicographic order so callers don't see jitter run-to-run.
func (s *Server) ListAgents(ctx context.Context, _ *connect.Request[pb.ListAgentsRequest]) (*connect.Response[pb.ListAgentsResponse], error) {
	out := &pb.ListAgentsResponse{}
	if len(s.agentClients) == 0 {
		return connect.NewResponse(out), nil
	}
	names := make([]string, 0, len(s.agentClients))
	for name := range s.agentClients {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		client := s.agentClients[name]
		info, err := client.GetInfo(ctx)
		if err != nil || info == nil {
			s.logger.Warn().Err(err).Str("agent", name).Msg("ListAgents: GetInfo failed; including bare name only")
			out.Agents = append(out.Agents, &pb.AgentInfo{Name: name})
			continue
		}
		out.Agents = append(out.Agents, &pb.AgentInfo{
			Name:         info.GetName(),
			Version:      info.GetVersion(),
			UserSettings: info.GetUserSettings(),
		})
	}
	return connect.NewResponse(out), nil
}

// ListPlugins returns the runtime status of every plugin the daemon
// attempted to load this run. Loaded plugins are reported alongside
// disabled and failed entries so an operator with a typo'd path or a
// stale binary on disk can debug it without grepping the daemon log.
func (s *Server) ListPlugins(_ context.Context, _ *connect.Request[pb.ListPluginsRequest]) (*connect.Response[pb.ListPluginsResponse], error) {
	out := &pb.ListPluginsResponse{}
	if s.pluginHost == nil {
		return connect.NewResponse(out), nil
	}
	for _, p := range s.pluginHost.AllPlugins() {
		out.Plugins = append(out.Plugins, &pb.InstalledPlugin{
			Name:         p.Name,
			Path:         p.Path,
			Enabled:      p.Enabled,
			Status:       installedPluginStatus(p),
			Capabilities: append([]string(nil), p.Capabilities...),
			Error:        p.Error,
		})
	}
	return connect.NewResponse(out), nil
}

// installedPluginStatus collapses (Enabled, Loaded, Error) into the
// proto enum surfaced to the CLI.
func installedPluginStatus(p plugin.Status) pb.InstalledPlugin_Status {
	switch {
	case p.Loaded:
		return pb.InstalledPlugin_STATUS_LOADED
	case !p.Enabled:
		return pb.InstalledPlugin_STATUS_DISABLED
	default:
		return pb.InstalledPlugin_STATUS_LOAD_FAILED
	}
}

// isSubdirOf checks if child is the same as or a subdirectory of parent.
func isSubdirOf(child, parent string) bool {
	// Clean both paths for consistent comparison.
	child = filepath.Clean(child)
	parent = filepath.Clean(parent)

	if child == parent {
		return true
	}

	// Ensure parent ends with separator for prefix matching.
	parentPrefix := parent + string(filepath.Separator)
	return len(child) > len(parentPrefix) && child[:len(parentPrefix)] == parentPrefix
}

// checkBranchName also supports the zero-value Server used by focused handlers.
func (s *Server) checkBranchName(ctx context.Context, name string) error {
	if s.validateBranchName != nil {
		return s.validateBranchName(ctx, name)
	}
	return fmt.Errorf("branch name validator is not configured")
}
