// Package main is the entry point for the bossd daemon.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"

	"github.com/recurser/bossalib/apiversion"
	"github.com/recurser/bossalib/buildinfo"
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/daemonstate"
	"github.com/recurser/bossalib/errortrack"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	bossalog "github.com/recurser/bossalib/log"
	"github.com/recurser/bossalib/migrate"
	"github.com/recurser/bossalib/models"
	libtelemetry "github.com/recurser/bossalib/telemetry"
	"github.com/recurser/bossalib/vcs"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossd/internal/account"
	"github.com/recurser/bossd/internal/accountcred"
	"github.com/recurser/bossd/internal/accountwiring"
	"github.com/recurser/bossd/internal/agent"
	cronpkg "github.com/recurser/bossd/internal/cron"
	"github.com/recurser/bossd/internal/db"
	gitpkg "github.com/recurser/bossd/internal/git"
	"github.com/recurser/bossd/internal/plugin"
	"github.com/recurser/bossd/internal/plugin/eventbus"
	"github.com/recurser/bossd/internal/proofenvkeyring"
	"github.com/recurser/bossd/internal/rotation"
	"github.com/recurser/bossd/internal/server"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/status"
	"github.com/recurser/bossd/internal/taskorchestrator"
	daemontelemetry "github.com/recurser/bossd/internal/telemetry"
	"github.com/recurser/bossd/internal/tmux"
	"github.com/recurser/bossd/internal/upstream"
	"github.com/recurser/bossd/internal/vcs/github"
	"github.com/recurser/bossd/migrations"
)

// sessionListerAdapter adapts SessionStore to upstream.SessionLister.
type sessionListerAdapter struct {
	sessions db.SessionStore
}

// ListSessions returns every session (active and archived) as protobuf,
// populated with each session's repo display name via a single JOIN query.
// Archived sessions are included so the orchestrator sees the archive
// transition — filtering to active only would make an archived session
// look indistinguishable from a deleted one at the receiver.
func (a *sessionListerAdapter) ListSessions(ctx context.Context) ([]*bossanovav1.Session, error) {
	rows, err := a.sessions.ListWithRepo(ctx, "")
	if err != nil {
		return nil, err
	}
	pbSessions := make([]*bossanovav1.Session, 0, len(rows))
	for _, r := range rows {
		pbSess := server.SessionToProto(r.Session)
		pbSess.RepoDisplayName = r.RepoDisplayName
		pbSess.RepoOriginUrl = server.CanonicalRepoOriginURL(r.RepoOriginURL)
		pbSessions = append(pbSessions, pbSess)
	}
	return pbSessions, nil
}

// protoSessionListerFunc adapts a bare function to upstream.ProtoSessionLister
// (the snapshot reader's input) so the snapshot path can post-process the
// projected sessions — applying the reverse-stream observability overlay —
// without a dedicated struct.
type protoSessionListerFunc func(ctx context.Context) ([]*bossanovav1.Session, error)

func (f protoSessionListerFunc) ListSessions(ctx context.Context) ([]*bossanovav1.Session, error) {
	return f(ctx)
}

func publishAuthFailedSessionDelta(
	ctx context.Context,
	agentSessionID string,
	agentChats db.AgentChatStore,
	rawSessions db.SessionStore,
	repos db.RepoStore,
	chatStatusTracker *status.Tracker,
	streamBus *upstream.StreamBus,
	logger zerolog.Logger,
) {
	if agentChats == nil || rawSessions == nil || chatStatusTracker == nil || streamBus == nil {
		return
	}
	chat, err := agentChats.GetByAgentSessionID(ctx, agentSessionID)
	if err != nil || chat == nil {
		return
	}
	row, err := rawSessions.Get(ctx, chat.SessionID)
	if err != nil {
		logger.Debug().Err(err).Str("session_id", chat.SessionID).Msg("auth-change: session lookup failed")
		return
	}
	pbSess := server.SessionToProto(row)
	if row.RepoID != "" && repos != nil {
		if r, err := repos.Get(ctx, row.RepoID); err == nil && r != nil {
			pbSess.RepoDisplayName = r.DisplayName
			pbSess.RepoOriginUrl = server.CanonicalRepoOriginURL(r.OriginURL)
			// Compute the base attention before the auth overlay so a session that
			// is already Blocked/Orphaned keeps its real blocked_reason instead of
			// being clobbered by the low-priority AGENT_AUTH_FAILED overlay below.
			server.HydrateBaseAttention(pbSess, row, r)
		}
	}
	chats, err := agentChats.ListBySession(ctx, pbSess.Id)
	if err != nil {
		return
	}
	server.HydrateAgentObservability(chatStatusTracker, pbSess, chats)
	streamBus.Publish(upstream.StreamEvent{
		Session: &upstream.SessionEvent{
			Kind:    bossanovav1.SessionDelta_KIND_UPDATED,
			Session: pbSess,
		},
	})
}

type repoPRSessionStore interface {
	ListByRepoAndPR(ctx context.Context, repoID string, prNumber int) ([]*db.SessionWithRepo, error)
}

type displayPollerSessionLookup struct {
	sessions db.SessionStore
	repos    db.RepoStore
}

func (a *displayPollerSessionLookup) SessionsForPR(ctx context.Context, repoOriginURL string, prNumber int) ([]session.SessionForPR, error) {
	repo, err := a.repos.GetByOrigin(ctx, repoOriginURL)
	if err != nil || repo == nil {
		return nil, err
	}

	var rows []*db.SessionWithRepo
	if lister, ok := a.sessions.(repoPRSessionStore); ok {
		rows, err = lister.ListByRepoAndPR(ctx, repo.ID, prNumber)
	} else {
		rows, err = a.sessions.ListWithRepo(ctx, repo.ID)
		if err == nil {
			rows = filterSessionsByPR(rows, prNumber)
		}
	}
	if err != nil {
		return nil, err
	}

	out := make([]session.SessionForPR, 0, len(rows))
	for _, row := range rows {
		out = append(out, session.SessionForPR{ID: row.ID})
	}
	return out, nil
}

func filterSessionsByPR(rows []*db.SessionWithRepo, prNumber int) []*db.SessionWithRepo {
	out := make([]*db.SessionWithRepo, 0, len(rows))
	for _, row := range rows {
		if row.PRNumber == nil || *row.PRNumber != prNumber {
			continue
		}
		out = append(out, row)
	}
	return out
}

// snapshotFallbackEnabled reports whether the unary PublishDaemonSnapshot
// publisher should run in break-glass FULL-FALLBACK mode — i.e. as the sole
// feed for transports that cannot carry the DaemonStream bidi stream. In this
// mode it publishes aggressively and reclaims idle connections. It is opt-in.
func snapshotFallbackEnabled(getenv func(string) string) bool {
	return getenv("BOSSD_SNAPSHOT_FALLBACK") == "true"
}

// snapshotReconcileDisabled reports whether the steady-state read-model
// reconciliation publisher is turned off. It runs by default (alongside the
// stream) as a safety net against drift from lost/never-published deltas; set
// BOSSD_SNAPSHOT_RECONCILE=false to disable.
func snapshotReconcileDisabled(getenv func(string) string) bool {
	return getenv("BOSSD_SNAPSHOT_RECONCILE") == "false"
}

const (
	// steadyStateSnapshotInterval is how often the read model is reconciled
	// against the daemon's authoritative session set while the bidi stream is
	// the primary feed. Long enough to be cheap, short enough that phantom
	// rows self-heal promptly.
	steadyStateSnapshotInterval = 5 * time.Minute
	// snapshotFallbackInterval is the aggressive cadence used only in
	// break-glass full-fallback mode, where the publisher is the sole feed.
	snapshotFallbackInterval = 30 * time.Second
)

func main() {
	showVersion := flag.Bool("version", false, "Print version information and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("bossd " + buildinfo.String())
		return
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	if err := run(runOpts{stopSig: sigCh}); err != nil {
		fmt.Fprintf(os.Stderr, "bossd: %v\n", err)
		os.Exit(1)
	}
}

// runOpts carries optional overrides for run. All fields are optional;
// zero values produce the production daemon defaults. Tests use this to
// inject a synthetic stop signal, isolate paths, and observe readiness.
type runOpts struct {
	// stopSig triggers graceful shutdown. Required for non-test callers.
	stopSig <-chan os.Signal

	// dbPath overrides db.DefaultDBPath() when non-empty.
	dbPath string

	// socketPath overrides server.DefaultSocketPath() when non-empty.
	socketPath string

	// plugins overrides discovered/configured plugins when non-nil.
	// Pass a non-nil empty slice to disable plugin discovery entirely.
	plugins []config.PluginConfig

	// onReady, if non-nil, is invoked once the daemon's server is
	// listening and all startup goroutines have been launched. Runs on a
	// separate goroutine so it cannot block shutdown.
	onReady func()

	// onBootstrapComplete, if non-nil, fires synchronously immediately
	// after TmuxStatusPoller.Bootstrap returns and before srv.Serve is
	// scheduled. Used by tests to pin the Bootstrap-before-Serve init
	// ordering invariant: a regression that started Serve first would be
	// caught by an OnServeStart firing while OnBootstrapComplete has not.
	onBootstrapComplete func()

	// onServeStart, if non-nil, fires synchronously inside the Serve
	// goroutine just before srv.Serve is invoked. Pairs with
	// onBootstrapComplete to assert init ordering.
	onServeStart func()

	// onHookPortSet, if non-nil, fires synchronously immediately after
	// lifecycle.SetHookPort. onBootstrapStart fires just before
	// lifecycle.Bootstrap. Together they pin the invariant that the hook
	// port is bound before bootstrap re-arms surviving tmux chats — otherwise
	// ConfigureFinalizeHook would be re-issued with port 0 and the worktree
	// would keep the previous daemon's stale hook URL.
	onHookPortSet    func()
	onBootstrapStart func()
}

func run(opts runOpts) error {
	// Human-friendly console logging plus rotated file at $XDG_STATE_HOME/bossanova/logs/bossd.log.
	logCloser := bossalog.Setup("bossd")
	defer func() { _ = logCloser.Close() }()

	settings, err := config.Load()
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}

	// --- Database ---

	dbPath := opts.dbPath
	if dbPath == "" {
		p, err := db.DefaultDBPathForSettings(settings)
		if err != nil {
			return fmt.Errorf("db path: %w", err)
		}
		dbPath = p
	}

	// --- Singleton guard ---
	//
	// Acquire an exclusive flock before opening the DB or binding the socket so
	// only one bossd owns them at a time. Without this, a second bossd (e.g. a
	// stray `make dev` or a TUI auto-start after a transient blip) would steal
	// the socket and contend over the SQLite DB, which surfaces as the TUI
	// flashing "Cannot connect to daemon". The lock lives in the data dir
	// (alongside the DB/socket) and auto-releases on process exit.
	lockPath := filepath.Join(filepath.Dir(dbPath), server.LockFileName)
	lockFile, err := server.AcquireSingletonLock(lockPath)
	if err != nil {
		if errors.Is(err, server.ErrDaemonAlreadyRunning) {
			log.Info().Str("lock", lockPath).Msg("another bossd is already running; exiting")
			return nil
		}
		return fmt.Errorf("acquire singleton lock: %w", err)
	}
	defer func() { _ = lockFile.Close() }()

	socketPath := opts.socketPath
	if socketPath == "" {
		p, err := server.DefaultSocketPathForSettings(settings)
		if err != nil {
			return fmt.Errorf("socket path: %w", err)
		}
		socketPath = p
	}

	settingsPath, err := config.Path()
	if err != nil {
		return fmt.Errorf("settings path: %w", err)
	}
	executablePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}
	appDataDir := filepath.Dir(dbPath)
	if err := daemonstate.Write(appDataDir, daemonstate.Metadata{
		PID:            os.Getpid(),
		ExecutablePath: executablePath,
		SettingsPath:   settingsPath,
		SocketPath:     socketPath,
		StartedAt:      time.Now().UTC(),
	}); err != nil {
		return err
	}
	defer func() { _ = daemonstate.Remove(appDataDir) }()

	database, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = database.Close() }()

	log.Info().Str("path", dbPath).Msg("database opened")

	// --- Migrations ---

	if err := migrate.Run(database, migrations.FS); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	log.Info().Msg("migrations complete")

	// --- Stores ---

	repos := db.NewRepoStore(database)
	rawSessions := db.NewSessionStore(database)
	attempts := db.NewAttemptStore(database)
	// Wrap the raw chat store with a notifier so chat lifecycle events
	// reach the upstream stream bus. OnChange is wired below once
	// streamBus exists; calls before that point (none today, but the
	// store is referenced by other subsystems first) no-op safely.
	agentChats := db.NewNotifyingAgentChatStore(db.NewAgentChatStore(database))
	taskMappings := db.NewTaskMappingStore(database)
	rawWorkflows := db.NewWorkflowStore(database)
	cronJobs := db.NewCronJobStore(database)
	accounts := db.NewAccountStore(database)
	// Account-rotation policy engine (BOS-173). Held on the daemon for the
	// headless/interactive auto-rotate consumers (BOS-174/175) to call; no cap
	// signal invokes it yet.
	rotationEngine := rotation.NewEngine(accounts, rotation.WithDefaultCooldown(settings.Rotation.DefaultCooldown()))
	// Credential blobs for account rotation live in the OS keyring, never in
	// SQLite (decision D3). accountcred links keyring/dbus and is daemon-only.
	accountCreds := accountcred.New()

	// The display-status computer needs to read the bare stores; wrap them
	// after construction so the computer's own writes don't recurse through
	// the recompute hooks (the wrapper short-circuits on display-only writes,
	// but reading via the unwrapped store is also free of side effects).
	chatStatusTracker := status.NewTracker()
	displayTracker := status.NewDisplayTracker()
	// Single-repairer lease, shared between the plugin host (which takes/releases
	// it via SetRepairStatus) and the API server (which reads it for
	// Session.repair_active). See BOS-234.
	repairLease := status.NewRepairLeaseManager()
	displayComputer := status.NewDisplayStatusComputer(
		rawSessions, displayTracker, chatStatusTracker, agentChats, rawWorkflows, log.Logger,
	)
	var sessions db.SessionStore = db.NewRecomputingSessionStore(rawSessions, displayComputer)
	var workflows db.WorkflowStore = db.NewRecomputingWorkflowStore(rawWorkflows, displayComputer)

	// Wire the display tracker so its mutations recompute synchronously.
	displayTracker.SetRecomputer(displayComputer)

	// streamBus is the in-process pub/sub the upstream stream client
	// drains. Created here (rather than later, where the upstream
	// machinery is configured) so the chat-status tracker hook below can
	// publish per-chat status deltas onto it. Closed by the daemon's
	// shutdown path; see deferred Close() below.
	streamBus := upstream.NewStreamBus(log.Logger)
	defer streamBus.Close()

	// accountLabeler resolves the human-friendly account label for the reverse
	// stream. It needs only the registry (the full accountResolver below also
	// wires a credential materializer that depends on plugin clients not yet
	// constructed here), so it is a lightweight label-only Resolver. Label
	// degrades safely: "System default" for account 0, a short id on any miss —
	// so the stream overlay never hard-fails.
	accountLabeler := account.NewResolver(accountwiring.NewRegistry(accounts), nil, log.Logger)

	// hydrateSessionForStream applies the same last_agent_activity_at +
	// AGENT_AUTH_FAILED observability overlay that the local GetSession/
	// ListSessions RPCs add, to a Session proto bound for the reverse stream.
	// bosso applies session deltas as FULL replacements, so EVERY session
	// UPDATED delta (and the DaemonSnapshot) must carry this overlay
	// consistently — otherwise a display recompute that omits it would clobber a
	// live AGENT_AUTH_FAILED attention back off in the cloud/web read model.
	// Fails toward NOT flagging on any lookup error (the overlay is best-effort
	// enrichment, never a hard dependency of the delta).
	hydrateSessionForStream := func(ctx context.Context, pbSess *bossanovav1.Session) {
		if pbSess == nil || agentChats == nil {
			return
		}
		// Compute the base attention (blocked/orphaned/…) BEFORE the auth overlay,
		// exactly as the local GetSession/ListSessions RPCs do. Reverse-stream
		// protos come straight from SessionToProto, which carries blocked_reason
		// but no attention_status, and HydrateAgentObservability only overlays
		// AGENT_AUTH_FAILED where attention_status is nil — so without this a
		// blocked/orphaned session with an auth-failed chat would have its real
		// blocked_reason clobbered by the low-priority auth overlay. Best-effort:
		// skip on any lookup error.
		if row, err := rawSessions.Get(ctx, pbSess.Id); err == nil && row != nil {
			if repo, err := repos.Get(ctx, row.RepoID); err == nil && repo != nil {
				server.HydrateBaseAttention(pbSess, row, repo)
			}
			// Populate account_label so cloud/multi-instance web SessionDetail shows
			// the friendly account name instead of the raw account_id. The local
			// GetSession/ListSessions RPCs add this via the server's withAccountLabel;
			// the reverse-stream path must do the same on EVERY delta (bosso applies
			// deltas as full replacements, so a delta that omits it would blank a
			// previously-set label). Best-effort: Label never hard-fails.
			accountID := ""
			if row.AccountID != nil {
				accountID = *row.AccountID
			}
			label, _ := accountLabeler.Label(ctx, accountID)
			pbSess.AccountLabel = &label
		}
		chats, err := agentChats.ListBySession(ctx, pbSess.Id)
		if err != nil {
			return
		}
		server.HydrateAgentObservability(chatStatusTracker, pbSess, chats)
	}

	// Wire the chat-status tracker similarly. It is keyed by claude_id, so
	// resolve to a session before calling Recompute. In addition to the
	// display-side recompute (which fans out a SessionDelta UPDATE for the
	// owning session), publish a ChatStatusDelta to the stream bus so
	// bosso receives per-chat status updates with claude_id populated.
	// Without this publisher, ChatStatusDelta events never reach the wire
	// and the orchestrator's per-chat status map stays empty.
	// chatRotator auto-rotates interactive chats on CHAT_STATUS_LIMITED
	// transitions (BOS-175). Declared here so the tracker on-update closure can
	// reference it; constructed once the lifecycle + rotation engine adapters are
	// in scope (below). The nil-guard covers the brief startup window before it is
	// assigned.
	var chatRotator *rotation.ChatRotator

	chatStatusTracker.SetOnUpdate(func(agentSessionID string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		chat, err := agentChats.GetByAgentSessionID(ctx, agentSessionID)
		if err != nil || chat == nil {
			return
		}
		// Publish the per-chat status delta first so the stream sees a
		// fresh status alongside any session-level UPDATED delta the
		// recompute below emits via displayComputer.SetOnUpdate.
		if entry := chatStatusTracker.Get(agentSessionID); entry != nil {
			streamBus.Publish(upstream.StreamEvent{
				Status: &upstream.StatusEvent{
					Status: &bossanovav1.ChatStatusDelta{
						SessionId:      chat.SessionID,
						AgentSessionId: agentSessionID,
						Status:         entry.Status,
						LastOutputAt:   timestamppb.New(entry.LastOutputAt),
					},
				},
			})
		}
		_ = displayComputer.Recompute(ctx, chat.SessionID)

		// Auto-rotate interactive chats on LIMITED transitions (BOS-175). The
		// tracker only fires this hook on real transitions, and the rotator itself
		// is non-blocking + internally rate-limited, so a no-op call on every other
		// status transition is cheap.
		if chatRotator != nil {
			if entry := chatStatusTracker.Get(agentSessionID); entry != nil {
				chatRotator.OnChatStatus(agentSessionID, entry.Status, entry.ResetAt)
			}
		}
	})

	// Emit a structured audit log each time a chat enters or leaves the
	// usage-limited state. Fired exactly once per transition by the tracker;
	// the durable sink is Epic 4.4. Logging only — no behavior change.
	chatStatusTracker.SetOnLimitTransition(func(agentSessionID string, entered bool) {
		event := "limit-recovered"
		if entered {
			event = "limit-entered"
		}
		entry := log.Info().Str("agent_session_id", agentSessionID).Bool("entered", entered)
		if e := chatStatusTracker.Get(agentSessionID); e != nil && !e.ResetAt.IsZero() {
			entry = entry.Time("reset_at", e.ResetAt)
		}
		entry.Msg(event)
	})

	// Wire chat-store mutations onto the stream bus. Without this the
	// orchestrator only ever sees chats from the initial DaemonSnapshot,
	// so any chat created/renamed/deleted after the daemon connects is
	// invisible to the web UI's per-session chat list.
	agentChats.OnChange = func(kind db.ChatChangeKind, chat *models.AgentChat) {
		var pbKind bossanovav1.ChatDelta_Kind
		switch kind {
		case db.ChatChangeCreated:
			pbKind = bossanovav1.ChatDelta_KIND_CREATED
		case db.ChatChangeUpdated:
			pbKind = bossanovav1.ChatDelta_KIND_UPDATED
		case db.ChatChangeDeleted:
			pbKind = bossanovav1.ChatDelta_KIND_DELETED
		default:
			return
		}
		streamBus.Publish(upstream.StreamEvent{
			Chat: &upstream.ChatEvent{
				Kind: pbKind,
				Chat: &bossanovav1.ClaudeChatMetadata{
					Id:             chat.ID,
					SessionId:      chat.SessionID,
					AgentSessionId: chat.AgentSessionID,
					AgentName:      chat.AgentName,
					Title:          chat.Title,
					DaemonId:       chat.DaemonID,
					CreatedAt:      timestamppb.New(chat.CreatedAt),
				},
			},
		})
	}

	// Create session lister for upstream sync
	sessionLister := &sessionListerAdapter{sessions: sessions}

	// Fail any workflows left in running/pending state from a previous daemon
	// instance. Their driving goroutines no longer exist after a restart.
	if n, err := workflows.FailOrphaned(context.Background()); err != nil {
		log.Warn().Err(err).Msg("failed to clean up orphaned workflows")
	} else if n > 0 {
		log.Info().Int64("count", n).Msg("failed orphaned workflows from previous run")
	}

	// Fail any task mappings left in Pending/InProgress from a previous
	// daemon instance. Their driving goroutines no longer exist.
	if n, err := taskMappings.FailOrphanedMappings(context.Background()); err != nil {
		log.Warn().Err(err).Msg("failed to clean up orphaned task mappings")
	} else if n > 0 {
		log.Info().Int64("count", n).Msg("failed orphaned task mappings from previous run")
	}

	// NOTE: AdvanceOrphanedSessions and the display-status backfill were moved to
	// run *after* lifecycle.Bootstrap (see below). Bootstrap's headless orphan
	// sweep must mark restart-killed `boss new --detach` runs ORPHANED before the
	// AdvanceOrphanedSessions bulk-advance runs — otherwise those workflow-less
	// ImplementingPlan rows would be moved to AwaitingChecks first and their
	// bootstrap-only PR would read as a normal green checks session (BOS-229). The
	// backfill follows both so it recomputes labels from the final states.

	// --- Lifecycle ---

	worktrees := gitpkg.NewManager(log.Logger)
	// Run repo setup scripts through the user's login shell so per-project
	// version-manager shims (nodenv/asdf/…) are on PATH — otherwise the daemon's
	// restricted PATH can't find pnpm and worktree dependency/hook install
	// silently skips, leaving cron worktrees dependency-free.
	worktrees.LoginShell = settings.LoginShell
	tmuxClient := tmux.NewClient()
	ghProvider := github.New(log.Logger)
	prAssociationResolver := session.NewPRAssociationResolver(sessions, repos, ghProvider, log.Logger).
		WithBranchResolver(worktrees).
		WithCronJobs(cronJobs).
		WithUpdateNotifier(func(ctx context.Context, sess *models.Session) {
			// Reconcile renames the session to the PR title via a direct store
			// write, bypassing the UpdateSession RPC that would emit the event.
			// Publish it here so bosso/web don't show a stale title.
			pbSess := server.SessionToProto(sess)
			// bosso applies session deltas as full replacements
			// (state.go applySessionDelta), so populate the joined repo
			// display name or the web UI would lose the Repo column.
			if sess.RepoID != "" {
				if r, err := repos.Get(ctx, sess.RepoID); err == nil && r != nil {
					pbSess.RepoDisplayName = r.DisplayName
					pbSess.RepoOriginUrl = server.CanonicalRepoOriginURL(r.OriginURL)
				}
			}
			hydrateSessionForStream(ctx, pbSess)
			streamBus.Publish(upstream.StreamEvent{
				Session: &upstream.SessionEvent{
					Kind:    bossanovav1.SessionDelta_KIND_UPDATED,
					Session: pbSess,
				},
			})
		})

	// Reconcile sessions that were created before their PR existed (or
	// where PR creation happened out-of-band). Uses live branch state.
	if n, err := prAssociationResolver.Reconcile(context.Background()); err != nil {
		log.Warn().Err(err).Msg("failed to reconcile PR associations")
	} else if n > 0 {
		log.Info().Int64("count", n).Msg("reconciled sessions with existing PRs")
	}

	// --- Dispatcher + Poller ---
	// Note: FixLoop removed - repair functionality moved to plugin

	dispatcher := session.NewDispatcher(sessions, repos, ghProvider, log.Logger)
	// Let the dispatcher poke the display tracker the instant a PR merges/closes
	// so the STATUS column flips immediately instead of waiting for the display
	// poller's next cycle.
	dispatcher.SetDisplayStatusSetter(displayTracker)
	poller := session.NewPoller(sessions, repos, ghProvider, session.DefaultPollInterval, session.DefaultPollTimeout, log.Logger)

	// --- Settings + Display Poller ---

	bossEnv := config.EnvOr("BOSS_ENV", "local")
	var errortrackClose = func() {}
	if settings.ErrorTrackingEnabled {
		errortrackDSN := config.EnvOr("BOSSD_SENTRY_DSN", "https://f8081ecc39984438b534485cb56a7391@o4511396716871680.ingest.de.sentry.io/4511396747608144")
		close, err := errortrack.Init(errortrack.Opts{
			DSN:         errortrackDSN,
			App:         "bossd",
			Environment: bossEnv,
			Release:     buildinfo.Version + "-" + buildinfo.Commit,
		})
		if err != nil {
			log.Warn().Err(err).Msg("errortrack disabled")
		} else {
			errortrackClose = close
		}
	}
	defer errortrackClose()

	telemetryClient := libtelemetry.New(daemontelemetry.ConfigFromSettings(settings))
	defer telemetryClient.Close()

	// Bossd-owned log dir for agent runs. Lives outside the worktree so a
	// hostile/buggy plugin can't path-traverse via symlinks. Plugin opens
	// log files here with O_NOFOLLOW (Task 7).
	agentLogsDir := filepath.Join(filepath.Dir(settings.WorktreeBaseDir), "agent-logs")
	if err := os.MkdirAll(agentLogsDir, 0o700); err != nil {
		return fmt.Errorf("create agent-logs dir %s: %w", agentLogsDir, err)
	}

	displayPoller := session.NewDisplayPoller(
		sessions, repos, ghProvider, displayTracker,
		settings.DisplayPollInterval(), log.Logger,
	)
	// Persist every poll's check list so `boss session checks <id>` can show
	// the daemon's view of CI history. Disk volume is bounded by poll
	// interval × active sessions and is fine for ops debugging. The same
	// store is shared with the gRPC server below so reads and writes hit
	// one instance.
	checkSnapshots := db.NewCheckSnapshotStore(database)
	displayPoller.SetSnapshotStore(checkSnapshots)

	// Rotation audit trail (BOS-176): one store, shared by the Recorder (which
	// every rotation decision path writes through) and the gRPC server below
	// (which hydrates Session.rotation_events for the TUI/web history). Auditing
	// never fails a rotation — the Recorder swallows insert errors.
	rotationEvents := db.NewRotationEventStore(database)
	rotationRecorder := rotation.NewRecorder(db.NewRotationAuditStore(rotationEvents), log.Logger)

	// --- Plugin Host ---

	pluginBus := eventbus.New(log.Logger)
	pluginHost := plugin.New(pluginBus, ghProvider, log.Logger)
	pluginHost.SetSessionDeps(repos, sessions, agentChats, displayTracker, chatStatusTracker)
	pluginHost.SetRepairLease(repairLease)
	// The plugin host's HostServiceServer defaults to a hermetic no-op proof env
	// resolver (keeps unit tests off the OS keyring); the daemon injects the real
	// keyring-backed resolver so proof credentials reach plugin-side repair spawns.
	pluginHost.SetProofEnvResolver(proofenvkeyring.New(log.Logger))

	// Register DisplayTracker onChange callback to notify plugins of status changes
	displayTracker.SetOnChange(func(sessionID string, oldEntry, newEntry *status.DisplayEntry) {
		if newEntry != nil {
			pluginHost.NotifyStatusChange(context.Background(), sessionID, newEntry.Status, newEntry.HasFailures)
		}
	})

	pluginCfgs := settings.Plugins
	if opts.plugins != nil {
		pluginCfgs = opts.plugins
	} else {
		// Drop any non-discoverable plugin (e.g. the E2E-only stub-runner) that an
		// older daemon persisted before the binary was excluded from discovery.
		// The explicit --plugins path above intentionally bypasses this so the
		// E2E harness can still load the stub.
		if filtered, dropped := config.FilterNonDiscoverablePlugins(pluginCfgs); len(dropped) > 0 {
			log.Info().Strs("dropped", dropped).Msg("removing non-discoverable plugins from config")
			pluginCfgs = filtered
			settings.Plugins = filtered
			if err := config.Save(settings); err != nil {
				log.Warn().Err(err).Msg("failed to persist filtered plugin list to settings")
			} else {
				log.Info().Msg("persisted filtered plugin list to settings")
			}
		}
	}
	if opts.plugins == nil && len(pluginCfgs) == 0 {
		pluginCfgs = config.DiscoverPlugins()
		if len(pluginCfgs) > 0 {
			log.Info().Int("count", len(pluginCfgs)).Msg("auto-discovered plugins")
			settings.Plugins = pluginCfgs
			if err := config.Save(settings); err != nil {
				log.Warn().Err(err).Msg("failed to persist discovered plugins to settings")
			} else {
				log.Info().Msg("persisted discovered plugins to settings")
			}
		}
	} else if opts.plugins == nil {
		// The config already lists plugins, but a freshly-built plugin binary
		// (e.g. a new bossd-plugin-* that isn't in settings.json yet — or whose
		// entry a clobbering save stripped) wouldn't otherwise load. Merge any
		// discovered-but-unregistered plugins so new plugins appear without a
		// hand-edit. Existing entries are preserved untouched.
		if merged, added := config.MergeDiscoveredPlugins(pluginCfgs, config.DiscoverPlugins()); len(added) > 0 {
			log.Info().Strs("added", added).Msg("merged newly-discovered plugins into config")
			pluginCfgs = merged
			settings.Plugins = merged
			if err := config.Save(settings); err != nil {
				log.Warn().Err(err).Msg("failed to persist merged plugin list to settings")
			} else {
				log.Info().Msg("persisted merged plugin list to settings")
			}
		}
	}

	// Self-heal a settings file that accumulated duplicate plugin entries —
	// e.g. a user added a plugin the discovery loop also wrote. Duplicates
	// would otherwise spawn parallel plugin subprocesses with independent
	// in-memory dedup state (see bossd-plugin-repair).
	if deduped, dropped := config.DedupPluginConfigs(pluginCfgs); dropped {
		log.Warn().Int("before", len(pluginCfgs)).Int("after", len(deduped)).Msg("removing duplicate plugin entries")
		pluginCfgs = deduped
		if opts.plugins == nil {
			settings.Plugins = deduped
			if err := config.Save(settings); err != nil {
				log.Warn().Err(err).Msg("failed to persist deduped plugin list to settings")
			}
		}
	}

	// Configured plugins (settings.Plugins, e.g. persisted by `boss config init
	// --plugin-dir`, which the official installer runs) are exec'd by their
	// stored path. Auto-discovery already vets binaries it finds, but these
	// explicit entries bypass that scan, so on a release build a plugin binary
	// swapped after config init would run without a plugins.sum check. Re-verify
	// the final list against the manifest and drop any binary that fails (fail
	// closed). The explicit --plugins E2E override loads unverified test stubs by
	// design, so it is exempt.
	if opts.plugins == nil {
		verified, rejected := config.VerifyConfiguredPlugins(pluginCfgs)
		for _, r := range rejected {
			log.Error().Str("plugin", r.Name).Str("path", r.Path).Str("reason", r.Reason).
				Msg("rejected configured plugin failing checksum verification")
		}
		pluginCfgs = verified
	}

	if err := pluginHost.Start(context.Background(), pluginCfgs, settings); err != nil {
		pluginBus.Close()
		return fmt.Errorf("plugin host: %w", err)
	}

	loadedAgents := pluginHost.AgentRunners()
	agentClients := map[string]agent.AgentRunnerClient{}
	pluginRunners := map[string]agent.AgentRunner{}
	var agentRunner agent.AgentDispatcher
	if len(loadedAgents) == 0 {
		// No agent plugin loaded: daemon stays healthy but session creation
		// will fail. Operators install bossd-plugin-claude (or another
		// AgentRunner plugin) and restart.
		log.Warn().Msg("no AgentRunner plugin loaded; sessions cannot be started until an agent plugin is installed")
		agentRunner = agent.NoopRunner{}
	} else {
		// Build per-agent registries: agent.AgentRunnerClient (for
		// ConfigureFinalizeHook etc.) and agent.AgentRunner (for the
		// Dispatcher's per-session routing). The dispensed plugin client
		// satisfies both interfaces — plugin.AgentRunner is a superset of
		// agent.AgentRunnerClient (it adds GetInfo) — so a type assertion
		// is enough to bridge the package boundary.
		tailer := agent.NewTailer(log.Logger)
		for name, raw := range loadedAgents {
			client, ok := raw.(agent.AgentRunnerClient)
			if !ok {
				log.Warn().Str("plugin", name).Msg("agent plugin does not satisfy AgentRunnerClient; skipping")
				continue
			}
			agentClients[name] = client
			pluginRunners[name] = agent.NewPluginRunner(client, tailer, agentLogsDir, log.Logger)
		}
		pluginHost.SetAgentClients(agentClients)
		pluginHost.SetAgentLogsDir(agentLogsDir)

		// Dispatcher routes Start/Stop/IsRunning by reading AgentName from
		// SQLite via the lookup closure built below.
		lookup := newDispatcherLookup(sessions, agentChats)
		agentRunner = agent.NewDispatcher(pluginRunners, lookup, settings.DefaultAgent, log.Logger)
	}

	// Account binding (BOS-170): the resolver decides which registry account a
	// session runs under and materializes its spawn env via the provider plugin.
	// Degrade-safe — an empty registry, an unbound session, or a plugin without
	// rotation all collapse to account 0 (no per-account env). Wired into the
	// server (creation-time default policy) and both spawn seams below.
	accountMaterializer := accountwiring.NewMaterializer(agentClients, accounts, accountCreds, log.Logger)
	accountResolver := account.NewResolver(
		accountwiring.NewRegistry(accounts),
		accountMaterializer,
		log.Logger,
	)
	accountSpawnEnv := accountwiring.NewSpawnEnvResolver(accountResolver, log.Logger)
	pluginHost.SetAccountEnvResolver(accountSpawnEnv)
	accountSmoke, err := accountwiring.NewSmokeRunner(agentClients, accountCreds, log.Logger)
	if err != nil {
		log.Warn().Err(err).Msg("account smoke runner unavailable; account test will validate credential shape only")
	}

	lifecycle := session.NewLifecycle(sessions, repos, agentChats, cronJobs, worktrees, agentRunner, tmuxClient, ghProvider, log.Logger)
	// The lifecycle constructor defaults to a hermetic no-op proof env resolver
	// (keeps unit tests off the OS keyring); the daemon must inject the real
	// keyring-backed resolver so proof credentials reach managed session spawns.
	lifecycle.SetProofEnvResolver(proofenvkeyring.New(log.Logger))
	lifecycle.SetAccountEnvResolver(accountSpawnEnv)
	lifecycle.SetSessionDeletedNotifier(func(_ context.Context, sessionID string) {
		streamBus.Publish(upstream.StreamEvent{
			Session: &upstream.SessionEvent{
				Kind:    bossanovav1.SessionDelta_KIND_DELETED,
				Session: &bossanovav1.Session{Id: sessionID},
			},
		})
	})
	lifecycle.SetDisplayTracker(displayTracker)
	if len(agentClients) > 0 {
		lifecycle.SetAgents(agentClients)
	}
	lifecycle.SetBranchResolver(worktrees)
	// Mirror HostServiceServer.SetAgentLogsDir so Lifecycle.StartTmuxChat
	// can pass a deterministic log path to BuildInteractiveCommand. Without
	// this, the extracted method would fail-closed with FailedPrecondition.
	lifecycle.SetAgentLogsDir(agentLogsDir)
	lifecycle.SetChatStatus(chatStatusTracker)
	// Manual account switch (BOS-171): the registry validates the target
	// account, the transcript probe drives resume-vs-fresh via the agent
	// plugins, and the mid-turn reader adapts the server-held status.Tracker so
	// a WORKING chat is not interrupted without --force. The session→account
	// binding defaults from the lifecycle's own session store.
	lifecycle.SetAccountSwitchDeps(accounts, agentClients, func(agentSessionID string) bool {
		e := chatStatusTracker.Get(agentSessionID)
		return e != nil && e.Status == bossanovav1.ChatStatus_CHAT_STATUS_WORKING
	})

	// Auto-rotate interactive chats (BOS-175): on a CHAT_STATUS_LIMITED transition
	// the rotator asks the BOS-173 engine for the next account and executes the
	// swap through the BOS-171 SwitchAccount primitive (Auto path). All seams are
	// fail-safe — any error leaves the chat LIMITED. Config is re-read live so the
	// opt-out applies without a daemon restart.
	chatRotator = rotation.NewChatRotator(rotation.ChatRotatorDeps{
		Logger:   log.Logger,
		Recorder: rotationRecorder,
		LoadConfig: func() (config.RotationConfig, error) {
			loaded, err := config.Load()
			if err != nil {
				return config.RotationConfig{}, err
			}
			return loaded.Rotation, nil
		},
		ChatContext: func(ctx context.Context, agentSessionID string) (rotation.ChatContext, error) {
			chat, err := agentChats.GetByAgentSessionID(ctx, agentSessionID)
			if err != nil {
				return rotation.ChatContext{}, err
			}
			if chat == nil {
				return rotation.ChatContext{}, fmt.Errorf("agent chat not found for agent_session_id %q", agentSessionID)
			}
			sess, err := sessions.Get(ctx, chat.SessionID)
			if err != nil {
				return rotation.ChatContext{}, err
			}
			return rotation.ChatContext{SessionID: sess.ID, RepoID: sess.RepoID, Provider: sess.AgentName}, nil
		},
		CurrentStatus: func(agentSessionID string) bossanovav1.ChatStatus {
			if e := chatStatusTracker.Get(agentSessionID); e != nil {
				return e.Status
			}
			return bossanovav1.ChatStatus_CHAT_STATUS_UNSPECIFIED
		},
		// Decide adapter: build the BOS-173 engine's real Signal (the currently
		// bound account is the "capped" account; capability is probed live via the
		// provider plugin) and map its Outcome onto rotation.Decision.
		Decide: func(ctx context.Context, req rotation.DecideRequest) (rotation.Decision, error) {
			sess, err := sessions.Get(ctx, req.SessionID)
			if err != nil {
				return rotation.Decision{}, err
			}
			capped := ""
			if sess.AccountID != nil {
				capped = *sess.AccountID
			}
			capable, err := accountMaterializer.SupportsRotation(ctx, req.Provider)
			if err != nil {
				return rotation.Decision{}, err
			}
			var resetPtr *time.Time
			if !req.ResetAt.IsZero() {
				r := req.ResetAt
				resetPtr = &r
			}
			out, err := rotationEngine.Decide(ctx, rotation.Signal{
				Provider:        req.Provider,
				CappedAccountID: capped,
				Kind:            rotation.UsageLimited,
				ResetAt:         resetPtr,
				RotationCapable: capable,
			})
			if err != nil {
				return rotation.Decision{}, err
			}
			switch out.Kind {
			case rotation.OutcomeRotate:
				if out.NextAccount == nil {
					return rotation.Decision{Kind: rotation.DecisionStatusOnly}, nil
				}
				return rotation.Decision{
					Kind:      rotation.DecisionSwitch,
					AccountID: out.NextAccount.ID,
					Label:     out.NextAccount.Label,
				}, nil
			case rotation.OutcomeAllExhausted:
				return rotation.Decision{Kind: rotation.DecisionAllExhausted, ResumeAt: out.ResumeAt}, nil
			default:
				return rotation.Decision{Kind: rotation.DecisionStatusOnly}, nil
			}
		},
		// Switch adapter: the BOS-171 manual-switch primitive on its Auto path.
		Switch: func(ctx context.Context, req rotation.SwitchRequest) (rotation.SwitchResult, error) {
			res, err := lifecycle.SwitchAccount(ctx, session.SwitchAccountParams{
				SessionID:       req.SessionID,
				AgentSessionID:  req.AgentSessionID,
				TargetAccountID: req.AccountID,
				Auto:            true,
				PreviousResetAt: req.PreviousResetAt,
			})
			if err != nil {
				return rotation.SwitchResult{}, err
			}
			return rotation.SwitchResult{SwitchedToLabel: res.TargetLabel, Fresh: !res.Resumed}, nil
		},
	})
	cronGate := session.NewCronCompletionGate(session.CronCompletionGateDeps{
		Sessions:  sessions,
		Finalizer: lifecycle,
		Logger:    log.Logger,
		// Gate finalize on the run actually being over. The Stop hook fires every
		// turn (including mid-run pauses awaiting a background subagent), so without
		// this a paused run would be finalized — opening a junk PR and Blocking a
		// still-working session. Same criterion the stranded-cron sweep uses.
		RunIsOver: lifecycle.CronRunIsOver,
	})
	lifecycle.SetCronCompletionNotifier(cronGate)
	// Wire the lifecycle into HostServiceServer so plugin-side StartChatRun
	// (Task 4) can spawn tmux-hosted runs through the same path the cron
	// scheduler uses. SetLifecycle accepts the narrow ChatLifecycle
	// interface — *session.Lifecycle satisfies it — and is a no-op when
	// no plugins are loaded (HostService is nil in that branch).
	if hs := pluginHost.HostService(); hs != nil {
		hs.SetLifecycle(lifecycle)
		// Wire the poll-fallback armer so StartSession / StartTmuxChat
		// can drive completion for hookless agents (e.g. codex). Plugins
		// that own a finalize hook (claude) report IsSupported=true and
		// never reach the armer. Cadence + jitter chosen to balance
		// responsiveness with idle CPU cost on a daemon hosting many
		// concurrent runs.
		lifecycle.SetPollCompleter(hs)
		pollFallback := agent.NewPollFallback(log.Logger, 2*time.Second, 200*time.Millisecond, lifecycle)
		lifecycle.SetPollArmer(pollFallback)
	}

	// Liveness checker resolves whether a session's agent is still running
	// (headless subprocess or tmux chat). The Dispatcher already routes
	// per-session internally, so agentForSession can return it unconditionally;
	// the closure shape lets non-dispatcher wirings (e.g. unit tests) resolve
	// per session. Built here — before the startup cron-recovery pass below — so
	// the stranded-cron sweep can reap logless / agent-dead runs immediately on
	// boot instead of waiting out the durable log-idle threshold. Reused for the
	// session creator, orchestrator, and cron overlap checker.
	agentForSession := func(_ *models.Session) agent.AgentRunner {
		return agentRunner
	}
	livenessChecker := taskorchestrator.NewLivenessChecker(sessions, agentChats, agentForSession, tmuxClient)
	lifecycle.SetSessionLiveness(livenessChecker)

	// Auto-rotation wiring (BOS-174). Install the policy knobs (kill switch,
	// max rotations, sweep interval) and the real rotation.Engine as the
	// decision port. rotationBinding (session→account binding) and
	// accountMaterializer (keyring blob → MaterializeAccount RPC overlay) are
	// deliberately left UNSET here: BOS-170 supplies the live binding + a
	// materializer adapter, and rotation activates automatically once they are
	// wired. Until then every seam-gated path (attemptUsageLimitRotation and
	// SweepParkedRotations) degrades to today's Block on the nil binding, so the
	// feature is dormant and fail-safe in production.
	lifecycle.SetRotationConfig(settings.Rotation)
	lifecycle.SetRotationConfigLoader(config.Load)
	lifecycle.SetRotationDecider(rotationEngine)
	lifecycle.SetRotationRecorder(rotationRecorder)

	// Recover sessions left in Finalizing from a previous daemon crash.
	// They can't be safely re-driven (we don't know whether EnsurePR ran
	// or whether the finalize chat was spawned), so we record
	// failed_recovered on their cron_job and transition them to Blocked
	// for the operator to investigate. Worktrees are preserved.
	if n, err := lifecycle.RecoverFinalizingSessions(context.Background()); err != nil {
		log.Warn().Err(err).Msg("failed to recover Finalizing sessions")
	} else if n > 0 {
		log.Info().Int("count", n).Msg("recovered sessions stuck in Finalizing from previous run")
	}

	// Recover cron sessions whose run finished but whose Stop-hook finalize
	// signal never reached the daemon — e.g. the daemon restarted as the run
	// ended, so the hook's baked-in loopback port was stale and the curl got
	// connection-refused. Such sessions are stranded in ImplementingPlan with
	// no completion trigger (RecoverFinalizingSessions only rescues Finalizing).
	// Run once at startup (catches restart-stranded sessions) and periodically
	// below (catches a lost hook on a daemon that stayed up). Must run after
	// SetCronCompletionNotifier above so the gate is wired.
	if n, err := lifecycle.RecoverStrandedCronSessions(context.Background()); err != nil {
		log.Warn().Err(err).Msg("failed to recover stranded cron sessions")
	} else if n > 0 {
		log.Info().Int("count", n).Msg("recovered stranded cron sessions with lost finalize signal")
	}

	// shutdownWG tracks daemon goroutines so we can wait for them to exit cleanly.
	// Subsystems that manage their own goroutines (poller, dispatcher, orchestrator,
	// display poller, tmux poller) expose a Done() channel; goroutines spawned
	// directly below use wg.Add/wg.Done via trackedGo below.
	var shutdownWG sync.WaitGroup

	// trackedGo spawns fn via safego.Go and registers it with shutdownWG.
	trackedGo := func(fn func()) {
		shutdownWG.Add(1)
		safego.Go(log.Logger, func() {
			defer shutdownWG.Done()
			fn()
		})
	}

	// trackDone registers a subsystem's Done() channel with shutdownWG.
	trackDone := func(done <-chan struct{}) {
		shutdownWG.Add(1)
		go func() {
			defer shutdownWG.Done()
			<-done
		}()
	}

	// Auto-start the repair plugin synchronously. If the plugin is loaded
	// but its StartWorkflow fails, the daemon refuses to start: silently
	// continuing leaves auto-repair stopped (m.stopped=true) so every
	// subsequent NotifyStatusChange is dropped, which is exactly the
	// silent-fail mode that produced the empty repair-*.log files we found
	// on disk during the diagnose-first pass.
	//
	// GetInfo errors on individual plugins are tolerated (logged) so a
	// misbehaving non-repair workflow plugin can't gate the daemon.
	// Operators running without a repair plugin still get a healthy
	// daemon — auto-repair is opt-in by binary presence — but they get a
	// loud warning so the disabled state is visible.
	{
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		var repairFound bool
		for _, svc := range pluginHost.GetWorkflowServices() {
			infoResp, err := svc.GetInfo(ctx)
			if err != nil {
				log.Warn().Err(err).Msg("failed to get workflow plugin info; skipping for auto-start")
				continue
			}
			if infoResp == nil {
				log.Warn().Msg("workflow plugin returned nil info; skipping for auto-start")
				continue
			}
			if infoResp.Name != "repair" {
				continue
			}
			repairFound = true
			repairCfgJSON, err := json.Marshal(settings.Repair)
			if err != nil {
				cancel()
				return fmt.Errorf("marshal repair settings: %w", err)
			}
			log.Info().Str("plugin_name", infoResp.Name).Msg("auto-starting repair plugin")
			if _, err := svc.StartWorkflow(ctx, &bossanovav1.StartWorkflowRequest{
				ConfigJson: string(repairCfgJSON),
			}); err != nil {
				cancel()
				return fmt.Errorf("auto-start repair plugin: %w", err)
			}
			log.Info().
				Str("plugin_name", infoResp.Name).
				Int("cooldown_minutes", settings.Repair.CooldownMinutes).
				Int("sweep_interval_minutes", settings.Repair.SweepIntervalMinutes).
				Int("idle_repair_threshold_minutes", settings.Repair.IdleRepairThresholdMinutes).
				Msg("repair plugin running")
		}
		cancel()
		if !repairFound {
			log.Warn().Msg("no repair plugin loaded; auto-repair is disabled until a bossd-plugin-repair binary is installed")
		}
	}

	// --- Task Orchestrator ---

	// Warn if tmux is not available — interactive sessions will fail at attach
	// time, and cron fires will record fire_failed (cron-spawned sessions are
	// hosted in tmux with no headless fallback).
	if !tmuxClient.Available(context.Background()) {
		log.Warn().Msg("tmux is not installed or not in PATH; interactive sessions will not work, and cron fires will record fire_failed")
	}

	sessionCreator := taskorchestrator.NewSessionCreatorWithAccountResolver(sessions, lifecycle, func() string {
		loaded, err := config.Load()
		if err != nil {
			log.Warn().Err(err).Msg("load config for orchestrated session default agent")
			return settings.DefaultAgent
		}
		return loaded.DefaultAgent
	}, livenessChecker, func(_ context.Context, sessionID string) {
		// Propagate cleanup of a half-started session so it doesn't linger
		// as a phantom row in the web read model until the daemon reconnects.
		streamBus.Publish(upstream.StreamEvent{
			Session: &upstream.SessionEvent{
				Kind:    bossanovav1.SessionDelta_KIND_DELETED,
				Session: &bossanovav1.Session{Id: sessionID},
			},
		})
	}, accountResolver, log.Logger)
	orchestrator := taskorchestrator.New(
		pluginHost, repos, taskMappings, sessionCreator, ghProvider,
		worktrees, livenessChecker, taskorchestrator.DefaultPollInterval, log.Logger,
	)

	// --- Cron Scheduler ---

	cronScheduler := cronpkg.New(cronpkg.Config{
		Store:    cronJobs,
		Sessions: sessions,
		Repos:    repos,
		Creator:  sessionCreator,
		Activity: session.NewCronActivityChecker(agentLogsDir, livenessChecker),
		Logger:   log.Logger,
	})
	// NOTE: cronScheduler.Start is intentionally deferred until after the hook
	// server is bound and lifecycle.SetHookPort has run (below). A tick that
	// fires before the port is set would have its session rejected by
	// StartSession (hookPort == 0) and be recorded as fire_failed, which is most
	// likely right after a restart that lands near a scheduled tick.

	// Wire the orchestrator as the completion notifier for the dispatcher
	// and server so that terminal session states unblock the per-repo task queue.
	dispatcher.SetCompletionNotifier(orchestrator)

	// --- Tmux Status Poller ---

	tmuxStatusPoller := status.NewTmuxStatusPoller(chatStatusTracker, agentChats, sessions, tmuxClient, agentClients, log.Logger)

	// --- Server ---

	// --- Upstream (optional, cloud mode) ---
	//
	// The legacy upstream.Manager (heartbeat + SyncSessions loops) was
	// replaced in T3.7 by upstream.StreamClient, which opens a single
	// long-lived DaemonStream and receives commands the orchestrator
	// pushes. Bootstrap sequence:
	//   1. Build the Connect client against BOSSD_ORCHESTRATOR_URL.
	//   2. Call RegisterDaemon with the WorkOS JWT to obtain a
	//      session_token (bosso persists the daemon's identity).
	//   3. Construct StreamClient with adapters that wrap the existing
	//      stores/lifecycle/tmux reader — no new subsystems needed.
	//   4. Launch Run(ctx) in a tracked goroutine. It owns reconnects,
	//      token refresh, snapshot, delta forwarding, and command
	//      dispatch on its own.
	//
	// streamBus is created earlier (alongside the chat-status tracker
	// wiring) so the chat-status hook can publish per-chat ChatStatusDelta
	// events. We reuse that bus here.

	// Wire the display-status computer's post-write hook into the stream
	// bus so every Recompute that actually writes a new (label, intent,
	// spinner) trio fans out a SessionDelta{UPDATED} on the reverse
	// stream. Without this, bosso only ever sees the initial
	// DaemonSnapshot — labels recomputed after startup (PR check
	// results, chat status, workflow transitions) never reach the web UI
	// and every session shows whatever it computed to before the gh
	// poller had run, which is uniformly "idle" for sessions whose chat
	// status is IDLE and whose PR check state is UNSPECIFIED.
	displayComputer.SetOnUpdate(func(ctx context.Context, sessionID string) {
		row, err := rawSessions.Get(ctx, sessionID)
		if err != nil {
			log.Debug().Err(err).Str("session_id", sessionID).Msg("display update: session lookup failed")
			return
		}
		pbSess := server.SessionToProto(row)
		// Populate the joined repo display name. bosso applies session
		// deltas as full replacements (state.go applySessionDelta), so
		// omitting this would clobber the populated value the initial
		// DaemonSnapshot set and the web UI would lose the Repo column.
		if row.RepoID != "" {
			if r, err := repos.Get(ctx, row.RepoID); err == nil && r != nil {
				pbSess.RepoDisplayName = r.DisplayName
				pbSess.RepoOriginUrl = server.CanonicalRepoOriginURL(r.OriginURL)
			}
		}
		// Full-replacement semantics (above) also mean this recompute-driven
		// delta must re-assert the observability overlay, or it would clobber a
		// live AGENT_AUTH_FAILED that the auth-change hook published.
		hydrateSessionForStream(ctx, pbSess)
		streamBus.Publish(upstream.StreamEvent{
			Session: &upstream.SessionEvent{
				Kind:    bossanovav1.SessionDelta_KIND_UPDATED,
				Session: pbSess,
			},
		})
	})

	// Wire the auth-failed change hook: when a chat's login-required state flips
	// — which need not coincide with any chat STATUS change — resolve it to its
	// session and emit a hydrated SessionDelta{UPDATED}. Without this the
	// AGENT_AUTH_FAILED attention only ever appears on local daemon reads
	// (GetSession/ListSessions); the cloud/web read model, fed solely by the
	// reverse stream, would never see it for a session whose status stays
	// WORKING. SetAuthFailed gates the hook on an effective-state transition, so
	// the poller's per-tick calls don't storm the stream.
	chatStatusTracker.SetOnAuthChange(func(agentSessionID string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		publishAuthFailedSessionDelta(ctx, agentSessionID, agentChats, rawSessions, repos, chatStatusTracker, streamBus, log.Logger)
	})

	var streamClient *upstream.StreamClient
	var terminalStreamClient *upstream.TerminalStreamClient
	var snapshotPublisher func(context.Context)
	var authNotifier server.AuthNotifier
	var cmdHandlerStream *upstream.CommandHandlerAdapter
	var creatorAdapter *upstream.SessionCreatorAdapter
	webhookEventCh := make(chan session.SessionEvent, 64)
	emitter := session.NewSessionEventEmitter(&displayPollerSessionLookup{sessions: sessions, repos: repos}, webhookEventCh, log.Logger)
	_, upstreamURLExplicit := os.LookupEnv("BOSSD_ORCHESTRATOR_URL")
	if cfg := upstream.ConfigFromEnv(); cfg != nil {
		// Pin daemon_id to a UUID persisted under the data dir (not the
		// rotating hostname) so a hostname change doesn't orphan the old
		// id's rows in the orchestrator read model. BOSSD_DAEMON_ID still
		// wins when set; hostname remains the last-resort fallback.
		if daemonID, idErr := upstream.ResolveDaemonID(os.Getenv, appDataDir, cfg.Hostname); idErr != nil {
			log.Warn().Err(idErr).Str("daemon_id", daemonID).Msg("stable daemon id unavailable; using fallback")
			cfg.DaemonID = daemonID
		} else {
			cfg.DaemonID = daemonID
		}

		// ConnectRPC bidi streams (DaemonStream) require HTTP/2, and the
		// daemon needs HTTP/2 keepalive so a half-open stream (laptop
		// sleep, network change) is detected and reconnected instead of
		// blocking stream.Receive() forever. Both concerns live in
		// upstream.BuildUpstreamHTTPClient, which also documents why no
		// client-level Timeout is set on these long-lived streams.
		httpClient := upstream.BuildUpstreamHTTPClient(cfg.OrchestratorURL)
		client := bossanovav1connect.NewOrchestratorServiceClient(
			httpClient,
			cfg.OrchestratorURL,
			// Stamp the API version this daemon was built against so bosso
			// keeps us on compatible behavior after the API advances.
			connect.WithInterceptors(apiversion.ClientInterceptor(apiversion.DefaultRegistry().Current())),
		)

		// Gather repo IDs for registration.
		allRepos, err := repos.List(context.Background())
		if err != nil {
			log.Warn().Err(err).Msg("failed to list repos for upstream registration")
		}
		var repoIDs []string
		for _, r := range allRepos {
			repoIDs = append(repoIDs, r.ID)
		}

		// Prefer BOSSD_USER_JWT; fall back to whatever the keychain
		// holds. Empty is allowed — bosso will reject the handshake and
		// the outer Run loop will back off, but the daemon stays up in
		// local-only mode.
		tokenProvider := upstream.NewKeychainTokenProvider()
		authToken := cfg.UserJWT
		if authToken == "" {
			// If the cached keychain token is expired (or about to be),
			// proactively refresh via the WorkOS refresh_token before
			// RegisterDaemon. The periodic refresh loop only runs after
			// the stream is alive, which is too late — bosso rejects the
			// initial register with an expired JWT and the whole startup
			// falls back to local-only mode.
			exp := tokenProvider.ExpiresAt()
			if !exp.IsZero() && time.Until(exp) < 60*time.Second {
				refreshCtx, refreshCancel := context.WithTimeout(context.Background(), 10*time.Second)
				if _, err := tokenProvider.Refresh(refreshCtx); err != nil {
					log.Warn().Err(err).Msg("proactive token refresh before register failed")
				}
				refreshCancel()
			}
			authToken = tokenProvider.Token()
		}

		regCtx, regCancel := context.WithTimeout(context.Background(), 10*time.Second)
		sessionToken, err := upstream.Register(regCtx, client, cfg.DaemonID, cfg.Hostname, authToken, repoIDs)
		regCancel()
		if err != nil {
			// Non-fatal: the stream's outer Run loop sees CodeUnauthenticated
			// on its first attempt and calls reRegister, which retries with
			// whatever tokenProvider holds. After `boss login`, NotifyLogin
			// reloads the keychain so the next reRegister succeeds — no
			// daemon restart required.
			log.Warn().Err(err).Msg("upstream register failed; stream will retry via reRegister")

			// Diagnostic dump: print the register inputs and the JWT
			// claims (unverified) so it's obvious when the daemon is
			// sending an expired or wrong-client token. Access token
			// itself is not logged — just the claims.
			iss, sub, aud, expStr, jwtErr := decodeJWTClaimsForLog(authToken)
			log.Warn().
				Str("orchestrator_url", cfg.OrchestratorURL).
				Str("daemon_id", cfg.DaemonID).
				Str("hostname", cfg.Hostname).
				Bool("bossd_user_jwt_set", cfg.UserJWT != "").
				Int("token_len", len(authToken)).
				Str("boss_workos_client_id", os.Getenv("BOSS_WORKOS_CLIENT_ID")).
				Str("bosso_workos_client_id", os.Getenv("BOSSO_WORKOS_CLIENT_ID")).
				Str("jwt_iss", iss).
				Str("jwt_sub", sub).
				Str("jwt_aud", aud).
				Str("jwt_exp", expStr).
				AnErr("jwt_decode_err", jwtErr).
				Msg("upstream register diagnostic")
		} else {
			log.Info().Str("daemon_id", cfg.DaemonID).Msg("registered with orchestrator")
		}

		// Always wire up the stream pipeline, regardless of whether the
		// initial Register succeeded. When it failed, sessionToken is
		// empty and the stream's outer Run loop will see
		// CodeUnauthenticated on its first DaemonStream open, then call
		// reRegister to rotate in a fresh session_token. Combined with
		// streamAuthAdapter.NotifyLogin reloading the keychain, this
		// lets a fresh `boss login` recover bossd from a startup auth
		// failure without restarting the daemon.
		//
		// bosso expects BOTH credentials on the stream:
		//   Authorization: Bearer <WorkOS JWT>   — proves user identity
		//   X-Daemon-Token: <session_token>      — proves daemon identity
		// See services/bosso/internal/server/stream.go DaemonStream.

		// Snapshot readers pull from the bossd stores, projecting
		// to the slim pb types the snapshot expects.
		// Hydrate the observability overlay onto every snapshot session so the
		// cloud/web read model's initial state on (re)connect matches what the
		// local RPCs serve — a session already login-required at connect time
		// shows AGENT_AUTH_FAILED without waiting for the next auth transition.
		snapshotSessions := upstream.NewSessionSnapshotReader(protoSessionListerFunc(
			func(ctx context.Context) ([]*bossanovav1.Session, error) {
				pbSessions, err := sessionLister.ListSessions(ctx)
				if err != nil {
					return nil, err
				}
				for _, s := range pbSessions {
					hydrateSessionForStream(ctx, s)
				}
				return pbSessions, nil
			},
		))
		// Snapshot the canonical https://<host>/<owner>/<repo> form of each
		// repo's origin URL so it matches the identifier bosso's webhook
		// dispatcher routes by (the GitHub html_url / GitLab web_url, also
		// normalized on the receiving end). Internal DB IDs would never
		// match a webhook payload. Repos without a parseable origin
		// (local-only, malformed) drop out — they can't receive webhooks
		// anyway, so leaving them out of the snapshot's repo set is
		// strictly correct.
		snapshotRepos := upstream.NewRepoSnapshotReader(func(ctx context.Context) ([]string, error) {
			rs, err := repos.List(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]string, 0, len(rs))
			for _, r := range rs {
				canonical := vcs.NormalizeRepoURL(r.OriginURL)
				if canonical == "" {
					continue
				}
				out = append(out, canonical)
			}
			return out, nil
		})
		snapshotChats := upstream.NewChatSnapshotReader(func(ctx context.Context) ([]*bossanovav1.ClaudeChatMetadata, error) {
			// Routable (not tmux-only): headless runs have no tmux session
			// name, so the old ListWithTmuxSession snapshot dropped them and
			// bosso's FindDaemonForChat 404'd remote send/transcript calls
			// after a reconnect that missed the create delta.
			chats, err := agentChats.ListRoutableChats(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]*bossanovav1.ClaudeChatMetadata, 0, len(chats))
			for _, c := range chats {
				out = append(out, &bossanovav1.ClaudeChatMetadata{
					Id:             c.ID,
					SessionId:      c.SessionID,
					AgentSessionId: c.AgentSessionID,
					AgentName:      c.AgentName,
					Title:          c.Title,
					DaemonId:       c.DaemonID,
					CreatedAt:      timestamppb.New(c.CreatedAt),
				})
			}
			return out, nil
		})
		snapshotStatuses := upstream.NewStatusSnapshotReader(func(ctx context.Context) ([]*bossanovav1.ChatStatusEntry, error) {
			// Walk the tracker's current (non-stale) entries so a
			// freshly-connected orchestrator inherits per-chat
			// status without waiting for the next transition. The
			// Tracker.Update hook suppresses no-op heartbeats, so
			// long-lived "working" chats that haven't transitioned
			// since the daemon's last connect would otherwise be
			// invisible to bosso (and the web UI) until they next
			// change state.
			entries := chatStatusTracker.Snapshot()
			out := make([]*bossanovav1.ChatStatusEntry, 0, len(entries))
			for agentSessionID, e := range entries {
				out = append(out, &bossanovav1.ChatStatusEntry{
					AgentSessionId: agentSessionID,
					Status:         e.Status,
					LastOutputAt:   timestamppb.New(e.LastOutputAt),
				})
			}
			return out, nil
		})

		// Command adapters delegate back to the existing
		// lifecycle/store surfaces.
		cmdHandler := &upstream.CommandHandlerAdapter{
			Lifecycle:  lifecycle,
			Sessions:   sessionGetterAdapter{sessions: sessions},
			Automation: automationToggleAdapter{sessions: sessions},
			OnCompletion: func(ctx context.Context, sessionID string) {
				if orchestrator != nil {
					orchestrator.HandleSessionCompleted(ctx, sessionID, models.TaskMappingStatusFailed)
				}
			},
		}
		// Surface the adapter to the outer scope so we can attach
		// the chat-waker once srv exists. Keeping the assignment here
		// (rather than inside StreamClient construction) keeps the
		// happy path uncluttered when no orchestrator is configured.
		cmdHandlerStream = cmdHandler

		// Attacher bridges to claude.Runner's subscribe/history.
		attacher := &upstream.SessionAttacherAdapter{
			Sessions: attachLookupAdapter{sessions: sessions},
			Agent:    claudeAttachAdapter{runner: agentRunner},
			Logger:   log.Logger,
		}

		var sessionTokenHolder *upstream.SessionTokenHolder
		var reRegisterMu sync.Mutex
		// reRegister self-heals from a stale or missing session_token
		// (another bossd with the same daemon_id rotated it via UPSERT,
		// bosso's daemons row was cleared, OR the initial Register at
		// startup failed). The Run loop calls this after a
		// CodeUnauthenticated handshake; we re-use the fresh JWT path
		// from startup (tokenProvider auto-refreshes inside the opener)
		// and gather repoIDs each call so a repo set that changed
		// since startup is reflected. The mutex serializes token issuance
		// across the stream and snapshot-publisher recovery paths because
		// bosso keeps one current session_token per daemon_id.
		reRegister := func(ctx context.Context) (string, error) {
			reRegisterMu.Lock()
			defer reRegisterMu.Unlock()

			currentRepos, err := repos.List(ctx)
			if err != nil {
				log.Warn().Err(err).Msg("reRegister: repos.List failed; proceeding with empty set")
				currentRepos = nil
			}
			ids := make([]string, 0, len(currentRepos))
			for _, r := range currentRepos {
				ids = append(ids, r.ID)
			}
			jwt := tokenProvider.Token()
			if jwt == "" {
				jwt = cfg.UserJWT
			}
			regCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			tok, err := upstream.Register(regCtx, client, cfg.DaemonID, cfg.Hostname, jwt, ids)
			if err != nil {
				return "", err
			}
			if tok != "" && sessionTokenHolder != nil {
				sessionTokenHolder.Set(tok)
			}
			return tok, nil
		}

		// Shared session_token holder: every opener that sends
		// X-Daemon-Token reads from this so a re-register-driven
		// rotation fans out to all of them. When initial Register
		// failed, sessionToken is "" — the first stream open will be
		// rejected, reRegister fires, and the holder is populated.
		sessionTokenHolder = upstream.NewSessionTokenHolder(sessionToken)
		// Shared auth state: when WorkOS rejects our refresh token as
		// invalid_grant (the user's session ended) the opener flips this
		// to NeedsLogin and both Run loops pause until NotifyLogin
		// clears it after a fresh `boss login`. Without this, the
		// daemon tight-loops on a dead credential indefinitely.
		authState := upstream.NewAuthState()
		// creatorAdapter drives the daemon's StreamCreateSession core for
		// reverse-stream CreateSessionCommands. Server is wired post-hoc
		// (after srv.New, below) — same pattern as cmdHandlerStream.Waker.
		creatorAdapter = &upstream.SessionCreatorAdapter{Logger: log.Logger}
		streamClient = upstream.NewStreamClient(upstream.StreamClientConfig{
			Client:       client,
			AuthToken:    authToken,          // WorkOS JWT → Authorization header
			SessionToken: sessionTokenHolder, // daemon token → X-Daemon-Token header
			DaemonID:     cfg.DaemonID,
			Hostname:     cfg.Hostname,
			Stores: upstream.StreamStores{
				Sessions: snapshotSessions,
				Chats:    snapshotChats,
				Repos:    snapshotRepos,
				Statuses: snapshotStatuses,
			},
			Events:         streamBus,
			TokenProvider:  tokenProvider,
			CommandHandler: cmdHandler,
			Webhooks:       upstream.NewWebhookDispatcherWithEmitterAndReviewComments(displayPoller, emitter, ghProvider, log.Logger),
			Attacher:       attacher,
			Creator:        creatorAdapter,
			ReRegister:     reRegister,
			AuthState:      authState,
			Logger:         log.Logger,
		})
		// Periodic read-model reconciliation. The bidi DaemonStream is the
		// primary feed, but delta delivery is best-effort (forwardEvent drops
		// deltas on reconnect) and not every delete path publishes, so a
		// long-lived stream's read model drifts — deleted sessions linger as
		// phantom rows in the web until the daemon reconnects and re-snapshots.
		// A periodic full-snapshot re-publish reconciles the read model via
		// ReplaceDaemonSessions. It is unary (transport-agnostic), so it works
		// even where the bidi stream is half-duplexed by an intermediary.
		if !snapshotReconcileDisabled(os.Getenv) {
			// Steady state: gentle cadence, and MUST NOT close the live
			// stream's idle connections (they share the HTTP client).
			interval := steadyStateSnapshotInterval
			closeIdle := func() {}
			if upstreamURLExplicit && snapshotFallbackEnabled(os.Getenv) {
				// Break-glass: the bidi stream can't be carried at all, so the
				// publisher is the SOLE feed — publish aggressively and reclaim
				// idle connections (no long-lived stream to disrupt).
				interval = snapshotFallbackInterval
				closeIdle = httpClient.CloseIdleConnections
			}
			snapshotPublisher = func(ctx context.Context) {
				runSnapshotPublisher(ctx, client, sessionTokenHolder, upstream.StreamStores{
					Sessions: snapshotSessions,
					Chats:    snapshotChats,
					Repos:    snapshotRepos,
					Statuses: snapshotStatuses,
				}, cfg.DaemonID, cfg.Hostname, reRegister, closeIdle, interval, log.Logger)
			}
		}

		authNotifier = &streamAuthAdapter{
			streamClient:  streamClient,
			tokenProvider: tokenProvider,
			authState:     authState,
			logger:        log.Logger,
		}

		// TerminalStream is a sibling of DaemonStream — separate bidi
		// for keystroke / data-chunk traffic so it cannot starve
		// control-plane commands. Reuses the SAME orchestrator client,
		// AuthToken, sessionTokenHolder, TokenProvider, and AuthState so
		// a re-register-driven session_token rotation or an invalid_grant
		// pause fans out to both streams. Idle until bosso pushes the
		// first attach.
		terminalStreamClient = upstream.NewTerminalStreamClient(upstream.TerminalStreamClientConfig{
			Client:        client,
			AuthToken:     authToken,
			SessionToken:  sessionTokenHolder,
			TokenProvider: tokenProvider,
			AuthState:     authState,
			TmuxClient:    tmuxClient,
			Chats:         upstream.NewChatStoreLookup(agentChats),
			Logger:        log.Logger,
		})
	}

	srv := server.New(server.Config{
		Repos:              repos,
		Sessions:           sessions,
		Attempts:           attempts,
		AgentChats:         agentChats,
		Workflows:          workflows,
		TaskMappings:       taskMappings,
		CronJobs:           cronJobs,
		Accounts:           accounts,
		RotationEngine:     rotationEngine,
		Resolver:           accountResolver,
		AccountCredentials: accountCreds,
		AccountSmokeRunner: accountSmoke,
		CheckSnapshots:     checkSnapshots,
		RotationEvents:     rotationEvents,
		CronScheduler:      cronScheduler,
		ChatStatus:         chatStatusTracker,
		DisplayTracker:     displayTracker,
		RepairLease:        repairLease,
		TmuxPoller:         tmuxStatusPoller,
		Lifecycle:          lifecycle,
		Agent:              agentRunner,
		AgentClients:       agentClients,
		Worktrees:          worktrees,
		Provider:           ghProvider,

		PRResolver:         prAssociationResolver,
		PluginHost:         pluginHost,
		Tmux:               tmuxClient,
		CompletionNotifier: orchestrator,
		AuthNotifier:       authNotifier,
		// Publish a SessionDelta_KIND_DELETED on the reverse stream for
		// every session row removed from the DB (failed setup cleanup,
		// RemoveSession, EmptyTrash). Without this, bosso's in-memory
		// Registry retains the session until the daemon reconnects and
		// replaces its state from a fresh DaemonSnapshot — so failed
		// sessions linger as "stopped" rows in the web UI long after the
		// local TUI has lost sight of them.
		OnSessionDeleted: func(_ context.Context, sessionID string) {
			streamBus.Publish(upstream.StreamEvent{
				Session: &upstream.SessionEvent{
					Kind:    bossanovav1.SessionDelta_KIND_DELETED,
					Session: &bossanovav1.Session{Id: sessionID},
				},
			})
		},
		OnSessionUpdated: func(ctx context.Context, sess *bossanovav1.Session) {
			// Re-assert the observability overlay under bosso's full-replacement
			// delta semantics so an RPC-driven update (rename, PR link, …)
			// doesn't clobber a live AGENT_AUTH_FAILED. The RPC handlers that
			// serve GetSession/ListSessions already overlay it locally; this
			// keeps the reverse-stream copy consistent.
			hydrateSessionForStream(ctx, sess)
			streamBus.Publish(upstream.StreamEvent{
				Session: &upstream.SessionEvent{
					Kind:    bossanovav1.SessionDelta_KIND_UPDATED,
					Session: sess,
				},
			})
		},
		Logger: log.Logger,
	})

	// Auto-archive dependabot repair sessions when their PR merges (BOS-101).
	// The server's archive-and-notify path also emits the stream update so the
	// session leaves the TUI immediately.
	orchestrator.SetSessionArchiver(taskorchestrator.SessionArchiverFunc(srv.ArchiveSessionAndNotify))

	// Wire the chat-waker on the existing CommandHandlerAdapter now that
	// the server is constructed. Done post-hoc rather than at adapter
	// construction time because the adapter is built before srv.New
	// (it's passed into the StreamClient configuration above) — and
	// CommandHandlerAdapter is a pointer, so this mutates the same
	// instance the StreamClient's interface dispatches through.
	if cmdHandlerStream != nil {
		cmdHandlerStream.Waker = srv
		cmdHandlerStream.Commands = srv
	}
	// Wire the creator adapter's server now that srv exists (it was passed
	// into the StreamClient config above as a pointer). *server.Server
	// satisfies upstream.StreamCreateSessioner via StreamCreateSession.
	if creatorAdapter != nil {
		creatorAdapter.Server = srv
	}

	// Start poller and dispatcher.
	pollerCtx, pollerCancel := context.WithCancel(context.Background())
	defer pollerCancel()
	// Hand the daemon-scoped context to the lifecycle so PollArmer.Arm
	// runs goroutines that outlive any single RPC handler. pollerCancel is
	// invoked during shutdown, draining all armed polls.
	lifecycle.SetDaemonCtx(pollerCtx)
	pollerEvents := poller.Run(pollerCtx)
	merged := mergeSessionEvents(pollerCtx, pollerEvents, webhookEventCh)
	trackDone(poller.Done())
	dispatcherDone := safego.Go(log.Logger, func() { dispatcher.Run(pollerCtx, merged) })
	trackDone(dispatcherDone)

	// Start task orchestrator (polls plugin task sources).
	orchestrator.Start(pollerCtx)
	trackDone(orchestrator.Done())

	// Start display status poller.
	displayPoller.Run(pollerCtx)
	trackDone(displayPoller.Done())

	// --- Hook Server (loopback HTTP for Claude Stop-hook notifications) ---
	//
	// Created and bound BEFORE lifecycle.Bootstrap: Bootstrap re-issues
	// ConfigureFinalizeHook for surviving tmux chats with HookPort, and the
	// claude plugin's WriteHookConfig rejects port <= 0. Binding the port
	// first ensures the re-arm rewrites the worktree's hook config with this
	// daemon's port instead of leaving the previous (now-dead) port in place.
	hookCfg := server.HookServerConfig{
		Sessions:  sessions,
		Finalizer: lifecycle,
		Logger:    log.Logger,
	}
	// HostService is non-nil whenever any plugin is configured (it's
	// constructed alongside the plugin host). When no plugins are loaded
	// the completer stays nil-interface and the agent-run-complete
	// endpoint surfaces 500s — that's fine because no plugin is around
	// to register a run in the first place. Wrap the nil-pointer check
	// here so the HookServer can rely on `completer == nil` working.
	if hs := pluginHost.HostService(); hs != nil {
		hookCfg.Completer = hs
	}
	hookSrv := server.NewHookServer(hookCfg)
	hookSrv.SetCronCompletionNotifier(cronGate)
	if err := hookSrv.Listen(); err != nil {
		return fmt.Errorf("hook server listen: %w", err)
	}
	// Plumb the bound port into the lifecycle so cron-spawned sessions can
	// stamp it into settings.local.json without the lifecycle having to
	// read it back from a file written by the same process.
	lifecycle.SetHookPort(hookSrv.Port())
	log.Info().Int("port", hookSrv.Port()).Msg("hook server listening on 127.0.0.1")
	if opts.onHookPortSet != nil {
		opts.onHookPortSet()
	}

	// Start the cron scheduler now that the hook port is set, so cron-spawned
	// sessions (which carry a HookToken) pass StartSession's hookPort guard
	// instead of being recorded as fire_failed during the startup window.
	if err := cronScheduler.Start(context.Background()); err != nil {
		return fmt.Errorf("cron scheduler: %w", err)
	}

	// Re-arm the poll fallback for hookless agent runs that survived a
	// daemon restart. For agents with a finalize hook (claude), the poll
	// re-arm is skipped (cached IsSupported=true short-circuits the loop) but
	// ConfigureFinalizeHook still re-writes the worktree hook config with this
	// daemon's port — which is why SetHookPort must run first (above). For
	// codex (and future hookless agents), this re-attaches the daemon to
	// in-flight runs so their eventual completion still signals through.
	if opts.onBootstrapStart != nil {
		opts.onBootstrapStart()
	}
	lifecycle.Bootstrap(pollerCtx)

	// Advance sessions stuck in ImplementingPlan whose driving workflows are no
	// longer running. Must run after FailOrphaned (above) so the subquery sees
	// the updated workflow statuses, AND after lifecycle.Bootstrap's headless
	// orphan sweep so a restart-killed `boss new --detach` run has already been
	// marked ORPHANED and is no longer in ImplementingPlan — otherwise this bulk
	// advance (which matches any workflow-less ImplementingPlan row) would move it
	// to AwaitingChecks and its bootstrap-only PR would read as green (BOS-229).
	if n, err := sessions.AdvanceOrphanedSessions(context.Background()); err != nil {
		log.Warn().Err(err).Msg("failed to advance orphaned sessions")
	} else if n > 0 {
		log.Info().Int64("count", n).Msg("advanced orphaned sessions to awaiting_checks")
	}

	// Backfill the display-status composite for every active session. After
	// a daemon restart the in-memory inputs (chat, display tracker) are
	// empty, so the persisted display_label may not match the stored state.
	// Recomputing once at boot ensures the row matches what Compute would
	// produce given current inputs (typically "stopped" or PR-axis label),
	// so clients reading via the bosso DB-fallback path don't see stale
	// "running 2/4" labels from the previous daemon's last write. Runs after the
	// orphan sweep + AdvanceOrphanedSessions so it reflects the final states.
	{
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		all, err := rawSessions.ListActive(ctx, "")
		if err != nil {
			log.Warn().Err(err).Msg("display backfill: list active sessions failed")
		} else {
			var updated int
			for _, s := range all {
				if err := displayComputer.Recompute(ctx, s.ID); err != nil {
					log.Debug().Err(err).Str("session_id", s.ID).Msg("display backfill: recompute failed")
					continue
				}
				updated++
			}
			if updated > 0 {
				log.Info().Int("count", updated).Msg("display backfill: recomputed active sessions")
			}
		}
		cancel()
	}

	// Bootstrap tmux status poller with pre-existing sessions before starting
	// the polling loop, so sessions from before a daemon restart show correct
	// status (idle/question) instead of defaulting to unknown.
	tmuxStatusPoller.Bootstrap(context.Background())
	if opts.onBootstrapComplete != nil {
		opts.onBootstrapComplete()
	}

	// Start tmux status poller (captures pane content to detect question/idle/working).
	tmuxStatusPoller.Run(pollerCtx)
	trackDone(tmuxStatusPoller.Done())

	// Start chat status cleanup goroutine (GC stale entries every 30s).
	trackedGo(func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-pollerCtx.Done():
				return
			case <-ticker.C:
				chatStatusTracker.Cleanup()
			}
		}
	})

	// Periodically reconcile sessions that are missing a PR number against
	// existing PRs on the same branch. The same call also runs once at
	// startup (above), but a long-lived daemon needs to keep reconciling:
	// the cron-tmux finalize path can race or surface a PR via a path
	// that doesn't write back to the session row, leaving the UI showing
	// "no PR" for a session whose branch already has one. 60s is a
	// compromise — fast enough that the gap is barely visible, slow
	// enough that the GitHub list-PRs cost stays small (the inner branch
	// only fires when there ARE orphaned sessions).
	trackedGo(func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-pollerCtx.Done():
				return
			case <-ticker.C:
				if n, err := prAssociationResolver.Reconcile(pollerCtx); err != nil {
					log.Warn().Err(err).Msg("periodic reconcile: failed")
				} else if n > 0 {
					log.Info().Int64("count", n).Msg("periodic reconcile: linked sessions to existing PRs")
				}
			}
		}
	})

	// Periodically recover cron sessions whose Stop-hook finalize signal was
	// lost (see the startup call above). The startup pass only fires on a
	// restart; this sweep catches a hook that failed to deliver on a daemon
	// that stayed up. 2 min matches the orchestrator's reconcile cadence — the
	// session is already minutes-late, so sub-minute latency buys nothing, and
	// the inner agent-log idle checks only run when sessions are stuck.
	trackedGo(func() {
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-pollerCtx.Done():
				return
			case <-ticker.C:
				if n, err := lifecycle.RecoverStrandedCronSessions(pollerCtx); err != nil {
					log.Warn().Err(err).Msg("periodic stranded-cron recovery: failed")
				} else if n > 0 {
					log.Info().Int("count", n).Msg("periodic stranded-cron recovery: finalized stranded cron sessions")
				}
			}
		}
	})

	// Periodically re-dispatch headless runs parked mid-rotation once their
	// resume-at stamp comes due (BOS-174). Level-triggered off persisted
	// rotation_resume_at, so it re-arms every parked run across a daemon restart
	// (no in-memory timer). The kill switch (RotationEnabled) short-circuits the
	// sweep, and until BOS-170 wires the binding/materializer every candidate
	// stays parked. Cadence comes from the rotation config (defaulted when unset).
	trackedGo(func() {
		ticker := time.NewTicker(settings.Rotation.ParkSweepInterval())
		defer ticker.Stop()
		for {
			select {
			case <-pollerCtx.Done():
				return
			case <-ticker.C:
				if n := lifecycle.SweepParkedRotations(pollerCtx); n > 0 {
					log.Info().Int("count", n).Msg("parked-rotation sweep: redispatched parked headless runs")
				}
			}
		}
	})

	// Bind the socket and initialize the http.Server synchronously so
	// Shutdown below cannot race with the serving goroutine's write to
	// the internal server field.
	if err := srv.Listen(socketPath); err != nil {
		return fmt.Errorf("server listen: %w", err)
	}

	// Start serving in a goroutine.
	errCh := make(chan error, 1)
	trackedGo(func() {
		log.Info().Str("socket", socketPath).Msg("starting server")
		if opts.onServeStart != nil {
			opts.onServeStart()
		}
		errCh <- srv.Serve()
	})
	trackedGo(func() {
		if err := hookSrv.Serve(); err != nil && err != http.ErrServerClosed {
			log.Error().Err(err).Msg("hook server exited unexpectedly")
		}
	})

	// Start the upstream StreamClient (no-op in local-only mode).
	// streamCtx is separate from pollerCtx so we can stop the stream
	// before the plugin host is torn down, letting orchestrator commands
	// that ride on it drain cleanly.
	streamCtx, streamCancel := context.WithCancel(context.Background())
	defer streamCancel()
	if streamClient != nil {
		trackedGo(func() {
			streamClient.Run(streamCtx)
		})
	}
	if snapshotPublisher != nil {
		trackedGo(func() {
			snapshotPublisher(streamCtx)
		})
	}
	// Run the TerminalStream client alongside the DaemonStream client.
	// Each owns its own connect/reconnect loop so a transient bosso
	// outage on one bidi can't bring the other down — both sit in their
	// own backoff. Cancellation via streamCtx returns nil from Run on
	// graceful shutdown; a non-nil return on streamCtx-still-live is a
	// fatal opener misconfiguration (e.g. nil opener) and is logged
	// rather than restarted.
	if terminalStreamClient != nil {
		trackedGo(func() {
			if err := terminalStreamClient.Run(streamCtx); err != nil && streamCtx.Err() == nil {
				log.Error().Err(err).Msg("terminal stream client exited unexpectedly")
			}
		})
	}

	// --- Ready hook (tests) ---

	telemetryClient.Capture(context.Background(), libtelemetry.EventDaemonStarted, daemonDistinctID(), nil)

	if opts.onReady != nil {
		safego.Go(log.Logger, opts.onReady)
	}

	// --- Wait for shutdown trigger ---

	select {
	case sig := <-opts.stopSig:
		log.Info().Str("signal", sig.String()).Msg("shutting down")
	case err := <-errCh:
		// Server exited unexpectedly.
		return fmt.Errorf("server: %w", err)
	}

	// Stop poller, dispatcher, and task orchestrator (all use pollerCtx).
	// Must cancel before stopping plugin host, since the orchestrator
	// calls into plugins.
	pollerCancel()

	// Stop upstream StreamClient (if running).
	streamCancel()

	// Stop the cron scheduler and wait for in-flight fires to finish. Bound
	// the wait so a stuck CreateSession cannot delay overall shutdown.
	cronStopCtx, cronStopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := cronScheduler.Stop(cronStopCtx); err != nil {
		log.Warn().Err(err).Msg("cron scheduler stop timed out")
	}
	cronStopCancel()

	// Stop plugin host.
	if err := pluginHost.Stop(); err != nil {
		log.Warn().Err(err).Msg("plugin host stop error")
	}
	pluginBus.Close()

	// Graceful shutdown with 5-second timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	// Shut down the hook server and remove its port file.
	if err := hookSrv.Shutdown(ctx); err != nil {
		log.Warn().Err(err).Msg("hook server shutdown error")
	}

	// Clean up socket file.
	_ = os.Remove(socketPath)

	// Wait for all tracked daemon goroutines to exit, with a hard 10-second
	// upper bound. Logs a warning on timeout — we still exit cleanly but
	// some goroutines may have been abandoned (e.g. a plugin RPC hang).
	waitCh := make(chan struct{})
	go func() {
		shutdownWG.Wait()
		close(waitCh)
	}()
	select {
	case <-waitCh:
		log.Info().Msg("all daemon goroutines exited cleanly")
	case <-time.After(10 * time.Second):
		log.Warn().Msg("forced exit: daemon goroutines did not stop within 10s")
	}

	log.Info().Msg("daemon stopped")
	return nil
}

func daemonDistinctID() string {
	hostname, err := os.Hostname()
	if err != nil {
		return libtelemetry.DaemonDistinctID("")
	}
	return daemonDistinctIDFromHostname(hostname)
}

func daemonDistinctIDFromHostname(hostname string) string {
	return libtelemetry.DaemonDistinctID(hostname)
}

// sessionGetterAdapter wires db.SessionStore.Get into the
// upstream.SessionReader interface used by the command handler adapter.
type sessionGetterAdapter struct {
	sessions db.SessionStore
}

func (a sessionGetterAdapter) GetSession(ctx context.Context, id string) (*bossanovav1.Session, error) {
	sess, err := a.sessions.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return server.SessionToProto(sess), nil
}

// automationToggleAdapter exposes db.SessionStore.Update's
// AutomationEnabled field as a narrow interface so the pause/resume
// command path doesn't need the full update surface.
type automationToggleAdapter struct {
	sessions db.SessionStore
}

func (a automationToggleAdapter) SetAutomationEnabled(ctx context.Context, sessionID string, enabled bool) error {
	_, err := a.sessions.Update(ctx, sessionID, db.UpdateSessionParams{AutomationEnabled: &enabled})
	return err
}

// newDispatcherLookup builds the lookup closure used by agent.Dispatcher
// to resolve an ID to its AgentName. It accepts EITHER a bossd session ID
// (lifecycle paths) OR an agent session ID — the liveness checker and the
// interactive attach adapter both pass the latter, since they only ever
// know the agent's tracking key. The chats table indexes agent session IDs,
// so we fall through to it when sessions.Get misses. Returning an error on
// double-miss lets the dispatcher's existing fallback (defaultAgent /
// single-loaded-runner shortcut) kick in for genuinely unknown IDs.
func newDispatcherLookup(sessions db.SessionStore, chats db.AgentChatStore) func(string) (string, error) {
	return func(id string) (string, error) {
		if sess, err := sessions.Get(context.Background(), id); err == nil {
			return sess.AgentName, nil
		}
		if chats != nil {
			if chat, err := chats.GetByAgentSessionID(context.Background(), id); err == nil {
				return chat.AgentName, nil
			}
		}
		return "", fmt.Errorf("no session or chat found for id %q", id)
	}
}

// attachLookupAdapter resolves a session ID to its current claude
// session ID and state — the two bits the attacher needs to decide
// whether to tail or bounce straight to SessionEnded.
type attachLookupAdapter struct {
	sessions db.SessionStore
}

func (a attachLookupAdapter) LookupAttachTarget(ctx context.Context, sessionID string) (string, int32, error) {
	sess, err := a.sessions.Get(ctx, sessionID)
	if err != nil {
		return "", 0, err
	}
	agentSessionID := ""
	if sess.AgentSessionID != nil {
		agentSessionID = *sess.AgentSessionID
	}
	return agentSessionID, int32(sess.State), nil
}

// claudeAttachAdapter converts claude.Runner's OutputLine channel into
// the upstream-package AttachOutputLine shape so the attacher's
// interface stays free of the claude package.
type claudeAttachAdapter struct {
	runner agent.AgentRunner
}

func (a claudeAttachAdapter) IsRunning(claudeSessionID string) bool {
	return a.runner.IsRunning(claudeSessionID)
}

func (a claudeAttachAdapter) History(claudeSessionID string) []upstream.AttachOutputLine {
	lines := a.runner.History(claudeSessionID)
	out := make([]upstream.AttachOutputLine, len(lines))
	for i, l := range lines {
		out[i] = upstream.AttachOutputLine{Text: l.Text, Timestamp: l.Timestamp}
	}
	return out
}

func (a claudeAttachAdapter) Subscribe(ctx context.Context, claudeSessionID string) (<-chan upstream.AttachOutputLine, error) {
	ch, err := a.runner.Subscribe(ctx, claudeSessionID)
	if err != nil {
		return nil, err
	}
	out := make(chan upstream.AttachOutputLine, 64)
	go func() {
		defer close(out)
		for line := range ch {
			select {
			case out <- upstream.AttachOutputLine{Text: line.Text, Timestamp: line.Timestamp}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func runSnapshotPublisher(
	ctx context.Context,
	client bossanovav1connect.OrchestratorServiceClient,
	sessionToken *upstream.SessionTokenHolder,
	stores upstream.StreamStores,
	daemonID, hostname string,
	reRegister func(context.Context) (string, error),
	closeIdle func(),
	interval time.Duration,
	logger zerolog.Logger,
) {
	// attempt sends one snapshot with the given bearer token and returns the
	// raw PublishDaemonSnapshot error (nil on success).
	attempt := func(pubCtx context.Context, snap *bossanovav1.DaemonSnapshot, token string) error {
		req := connect.NewRequest(&bossanovav1.PublishDaemonSnapshotRequest{Snapshot: snap})
		req.Header().Set("Authorization", "Bearer "+token)
		_, err := client.PublishDaemonSnapshot(pubCtx, req)
		return err
	}

	publish := func() {
		if closeIdle != nil {
			defer closeIdle()
		}
		token := ""
		if sessionToken != nil {
			token = sessionToken.Get()
		}
		if token == "" {
			logger.Debug().Msg("snapshot publisher waiting for daemon session token")
			return
		}
		pubCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		snap, err := buildSnapshotForPublish(pubCtx, stores, daemonID, hostname)
		if err != nil {
			logger.Warn().Err(err).Msg("snapshot publisher: build snapshot")
			return
		}
		err = attempt(pubCtx, snap, token)
		// Self-heal a stale session_token. CodeUnauthenticated ("invalid
		// credentials") means bosso's daemons row for our token is gone —
		// bosso restarted, or another bossd with our daemon_id rotated it via
		// UPSERT. The bidi stream normally re-registers on its own auth
		// rejection, but if that loop is wedged (e.g. blocked in a half-open
		// Receive) the publisher is the only feed still running. Without a
		// re-register here it would fail every tick forever and the daemon
		// would stay invisible on the web. Rotate the shared holder (which
		// fans out to both stream openers) and retry once.
		if err != nil && connect.CodeOf(err) == connect.CodeUnauthenticated && reRegister != nil {
			newTok, regErr := reRegister(pubCtx)
			switch {
			case regErr != nil:
				logger.Warn().Err(regErr).Msg("snapshot publisher: re-register after auth rejection failed")
			case newTok == "":
				logger.Warn().Msg("snapshot publisher: re-register returned empty session token")
			default:
				retryToken := newTok
				if sessionToken != nil {
					if sessionToken.CompareAndSwap(token, newTok) {
						logger.Info().Msg("snapshot publisher: rotated session_token after auth rejection")
					} else if current := sessionToken.Get(); current != "" {
						retryToken = current
						logger.Info().Msg("snapshot publisher: session_token already rotated after auth rejection")
					} else {
						sessionToken.Set(newTok)
						logger.Info().Msg("snapshot publisher: rotated session_token after auth rejection")
					}
				} else {
					logger.Info().Msg("snapshot publisher: using re-registered session_token after auth rejection")
				}
				err = attempt(pubCtx, snap, retryToken)
			}
		}
		if err != nil {
			// CodeUnimplemented means the orchestrator has no read model
			// (single-instance / local dev) — there is nothing to reconcile,
			// so don't spam warnings on every steady-state tick.
			if connect.CodeOf(err) == connect.CodeUnimplemented {
				logger.Debug().Msg("snapshot publisher: read model not configured")
			} else {
				logger.Warn().Err(err).Msg("snapshot publisher: publish failed")
			}
			return
		}
		logger.Debug().Int("sessions", len(snap.GetSessions())).Msg("snapshot publisher: published")
	}

	publish()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			publish()
		}
	}
}

func buildSnapshotForPublish(ctx context.Context, stores upstream.StreamStores, daemonID, hostname string) (*bossanovav1.DaemonSnapshot, error) {
	snap := &bossanovav1.DaemonSnapshot{
		DaemonId: daemonID,
		Hostname: hostname,
	}
	if stores.Repos != nil {
		repos, err := stores.Repos.SnapshotRepoIDs(ctx)
		if err != nil {
			return nil, fmt.Errorf("snapshot repos: %w", err)
		}
		snap.RepoIds = repos
	}
	if stores.Sessions != nil {
		sessions, err := stores.Sessions.SnapshotSessions(ctx)
		if err != nil {
			return nil, fmt.Errorf("snapshot sessions: %w", err)
		}
		snap.Sessions = sessions
	}
	if stores.Chats != nil {
		chats, err := stores.Chats.SnapshotChats(ctx)
		if err != nil {
			return nil, fmt.Errorf("snapshot chats: %w", err)
		}
		snap.Chats = chats
	}
	if stores.Statuses != nil {
		statuses, err := stores.Statuses.SnapshotStatuses(ctx)
		if err != nil {
			return nil, fmt.Errorf("snapshot statuses: %w", err)
		}
		snap.Statuses = statuses
	}
	return snap, nil
}

// streamAuthAdapter implements server.AuthNotifier by reloading
// credentials from the keychain on login and signalling active streams on
// logout. The shared AuthState is wired into both DaemonStream and
// TerminalStream, so MarkNeedsLogin cancels any open bidi immediately and
// the outer Run loops pause until NotifyLogin marks auth OK again.
type streamAuthAdapter struct {
	streamClient  *upstream.StreamClient
	tokenProvider *upstream.KeychainTokenProvider
	authState     *upstream.AuthState
	logger        zerolog.Logger
}

// NotifyLogin reloads keychain credentials so the running stream picks
// up the freshly stored tokens from `boss login`. Calling Refresh here
// would use the in-memory refresh_token, which has been superseded by
// the new login — WorkOS rejects it with "Session has already ended".
// Reading the keychain instead picks up the access+refresh pair the CLI
// just wrote. The next reconnect (or the periodic refresher when the
// current JWT nears expiry) propagates the new token to bosso.
//
// MarkOK clears the "needs re-login" flag set by the opener when WorkOS
// rejected the previous refresh token as invalid_grant. The Run loops
// are blocked on AuthState.Wait() in that case; clearing the flag wakes
// them so they reconnect with the freshly-loaded keychain credentials.
func (a *streamAuthAdapter) NotifyLogin(_ context.Context, _ []string) error {
	if a.tokenProvider != nil {
		a.tokenProvider.Reload()
	}
	if a.authState != nil {
		a.authState.MarkOK()
	}
	return nil
}

// NotifyLogout marks the auth state as "needs login". Active streams watch
// this transition and cancel their contexts immediately, then the Run loops
// pause before reconnecting. Idempotent.
func (a *streamAuthAdapter) NotifyLogout() {
	if a.authState != nil {
		a.authState.MarkNeedsLogin()
	}
}

// decodeJWTClaimsForLog extracts iss/sub/aud/exp from an unverified JWT
// for diagnostic logging. It deliberately does not validate the signature
// — it's just pulling fields out of the base64url-encoded payload so the
// log line tells us whether the token is expired, for the wrong client,
// or malformed. Returns empty strings + err on any parse failure.
func decodeJWTClaimsForLog(token string) (iss, sub, aud, exp string, err error) {
	if token == "" {
		return "", "", "", "", fmt.Errorf("empty token")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", "", "", "", fmt.Errorf("not a JWT (%d parts)", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", "", "", fmt.Errorf("base64 decode payload: %w", err)
	}
	var claims struct {
		Iss string          `json:"iss"`
		Sub string          `json:"sub"`
		Aud json.RawMessage `json:"aud"`
		Exp int64           `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", "", "", "", fmt.Errorf("unmarshal claims: %w", err)
	}
	expStr := ""
	if claims.Exp > 0 {
		t := time.Unix(claims.Exp, 0)
		expStr = fmt.Sprintf("%s (in %s)", t.Format(time.RFC3339), time.Until(t).Round(time.Second))
	}
	return claims.Iss, claims.Sub, string(claims.Aud), expStr, nil
}

func mergeSessionEvents(ctx context.Context, a, b <-chan session.SessionEvent) <-chan session.SessionEvent {
	out := make(chan session.SessionEvent, 64)
	go func() {
		defer close(out)
		for a != nil || b != nil {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-a:
				if !ok {
					a = nil
					continue
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			case ev, ok := <-b:
				if !ok {
					b = nil
					continue
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}
