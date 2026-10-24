package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/recurser/bossd/internal/domain"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/daemonstate"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	bossalog "github.com/recurser/bossalib/log"
	"github.com/recurser/bossd/internal/adapter/migrate"
	"github.com/rs/zerolog/log"
	"google.golang.org/protobuf/types/known/timestamppb"

	server "github.com/recurser/bossd/api/daemon"
	"github.com/recurser/bossd/api/upstream"
	"github.com/recurser/bossd/internal/account"
	"github.com/recurser/bossd/internal/accountwiring"
	"github.com/recurser/bossd/internal/adapter/accountcred"
	"github.com/recurser/bossd/internal/adapter/reposecrets"
	"github.com/recurser/bossd/internal/adapter/reposecrets/oskeyring"
	"github.com/recurser/bossd/internal/broadcast"
	"github.com/recurser/bossd/internal/callback"
	"github.com/recurser/bossd/internal/db"
	daemontelemetry "github.com/recurser/bossd/internal/domain/telemetry"
	"github.com/recurser/bossd/internal/rotation"
	"github.com/recurser/bossd/internal/status"
	"github.com/recurser/bossd/migrations"
)

func (w *daemonWiring) wireStores() error {
	var (
		err             error
		tmuxDaemonIDErr error
	)
	// Human-friendly console logging plus rotated file at $XDG_STATE_HOME/bossanova/logs/bossd.log.
	logCloser := bossalog.Setup("bossd")
	w.deferCleanup(func() { _ = logCloser.Close() })

	// startupProtectedRoots is the symlink-resolved protected-root list the
	// startup diagnostic derived. It is handed to the server so the RepairDoctor
	// check probes the same roots the startup path found (BOS-725) — the doctor
	// only re-derives lexically, and a working path that reaches a protected root
	// through a symlink is invisible to a lexical match.

	w.settings, err = config.Load()
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}

	// --- Database ---

	dbPath := w.opts.dbPath
	if dbPath == "" {
		p, err := db.DefaultDBPathForSettings(w.settings)
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
			w.alreadyRunning = true
			return nil
		}
		return fmt.Errorf("acquire singleton lock: %w", err)
	}
	var daemonMetadataWritten bool
	var daemonMetadataDir string
	w.deferCleanup(func() {
		cleanupDaemonShutdownState(lockFile, daemonMetadataWritten, daemonMetadataDir)
	})

	w.socketPath = w.opts.socketPath
	if w.socketPath == "" {
		p, err := server.DefaultSocketPathForSettings(w.settings)
		if err != nil {
			return fmt.Errorf("socket path: %w", err)
		}
		w.socketPath = p
	}

	settingsPath, err := config.Path()
	if err != nil {
		return fmt.Errorf("settings path: %w", err)
	}
	w.executablePath, err = os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}
	w.appDataDir = filepath.Dir(dbPath)

	// Owning-daemon id for tmux panes (BOS-846). Resolved here, ahead of the
	// tmux client, because the stamper and the reaper must agree on it — and
	// the upstream identity resolution further down only runs when an
	// orchestrator is configured. ResolveDaemonID is idempotent: it persists on
	// first call and reads the same id back afterwards, so both callers get the
	// same value. It always returns a usable id; on failure that is the
	// hostname, and an empty one stamps nothing, which leaves every pane
	// unattributable rather than reapable.
	tmuxHostname, _ := os.Hostname()
	w.tmuxDaemonID, tmuxDaemonIDErr = upstream.ResolveDaemonID(os.Getenv, w.appDataDir, tmuxHostname)
	if tmuxDaemonIDErr != nil {
		log.Warn().Err(tmuxDaemonIDErr).Str("daemon_id", w.tmuxDaemonID).
			Msg("stable daemon id unavailable for tmux pane ownership; using fallback")
	}

	w.daemonMetadata = daemonstate.Metadata{
		PID:                 os.Getpid(),
		ExecutablePath:      w.executablePath,
		SettingsPath:        settingsPath,
		SocketPath:          w.socketPath,
		DaemonID:            w.tmuxDaemonID,
		DisplayName:         config.DaemonDisplayName(w.settings, config.DefaultDisplayHostname(tmuxHostname)),
		DisplayNameOverride: strings.TrimSpace(w.settings.DaemonName) != "",
		StartedAt:           time.Now().UTC(),
		FileLimitSoft:       achievedFileLimitSoft,
	}
	if err := daemonstate.Write(w.appDataDir, w.daemonMetadata); err != nil {
		return err
	}
	daemonMetadataDir = w.appDataDir
	daemonMetadataWritten = true

	database, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	w.deferCleanup(func() { _ = database.Close() })

	log.Info().Str("path", dbPath).Msg("database opened")

	// --- Migrations ---

	if err := migrate.Run(database, migrations.FS); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	log.Info().Msg("migrations complete")

	// --- Repo API keys (BOS-1587) ---
	//
	// Repo Linear / Sentry keys live in the shared bossanova keyring. Move any
	// still-plaintext column values there once; a row that fails stays as it
	// was and is retried on the next start. The move is bounded by moveCtx:
	// reposecrets.Store returns ctx.Err() when it expires even while a keychain
	// call is still blocked (an unanswered approval prompt), so a hung keyring
	// delays startup by at most a minute and never blocks it. A write abandoned
	// that way may still land, but its row stays unflagged and its columns stay
	// authoritative until a later start moves it. Values are never logged.
	repoSecrets := reposecrets.New(oskeyring.Open)
	{
		moveCtx, cancelMove := context.WithTimeout(context.Background(), time.Minute)
		moved, err := db.MigrateRepoSecretsFromSQLite(moveCtx, database, repoSecrets)
		cancelMove()
		if err != nil {
			log.Warn().Err(err).Int("moved", moved).Msg("moving repo API keys into the keyring did not finish; remaining rows retry on next start")
		} else if moved > 0 {
			log.Info().Int("moved", moved).Msg("moved repo API keys into the keyring")
		}
	}

	{
		started := time.Now()
		log.Info().Msg("scrubbing freed database pages after the repo key move; one-time")
		ran, err := db.ScrubFreedSecretPages(context.Background(), database)
		if err != nil {
			log.Warn().Err(err).Msg("scrubbing freed database pages failed; retrying on next start")
		} else if ran {
			log.Info().Dur("elapsed", time.Since(started)).Msg("scrubbed freed database pages after the repo key move")
		}
	}

	// --- Stores ---

	// These constructors only wrap the connection; allocate every store here
	// so later wiring stages consume stores without carrying a SQL handle.
	w.checkSnapshots = db.NewCheckSnapshotStore(database)
	w.pendingTrackerMerges = db.NewPendingTrackerMergeStore(database)
	w.proxyTokens = db.NewProxyTokenStore(database)
	w.triggerLaunches = db.NewTriggerLaunchStore(database)

	w.repos = db.NewRepoStore(database, db.WithRepoSecrets(repoSecrets))
	w.rawSessions = db.NewSessionStore(database)
	// Wrap the raw chat store with a notifier so chat lifecycle events
	// reach the upstream stream bus. OnChange is wired below once
	// streamBus exists; calls before that point (none today, but the
	// store is referenced by other subsystems first) no-op safely.
	w.agentChats = upstream.NewNotifyingAgentChatStore(db.NewAgentChatStore(database))
	w.taskMappings = db.NewTaskMappingStore(database)
	rawWorkflows := db.NewWorkflowStore(database)
	w.cronJobs = db.NewCronJobStore(database)
	w.githubCallbacks = db.NewGitHubCallbackStore(database)
	// Notes (BOS-550): durable free-text a run records against a repo so a
	// later sweep can harvest what was learned. Retention (BOS-1384) prunes a
	// repo's expired and over-cap notes on each insert, per the startup
	// settings snapshot; a changed override applies on the next restart.
	w.notes = db.NewNoteStore(database, db.WithNoteRetention(db.NoteRetentionFromSettings(w.settings.Notes)))
	// The note cloud-sync outbox (BOS-1429) the note store writes in every
	// note transaction. The worker that drains it to Bosso (BOS-1435) is built
	// only when an upstream is configured, below; SyncNotesNow counts it either
	// way.
	w.noteSyncStates = db.NewNoteSyncStore(database)
	// Broadcasts (BOS-556): the store persists a broadcast plus the audience
	// frozen into its delivery rows; the resolver turns a validated selector
	// into that audience from the daemon's routable chats and their sessions.
	// The delivery worker that drains the rows is constructed below, once srv
	// exists to provide the wake+submit delivery primitive.
	//
	// rawSessions rather than the display-recomputing wrapper: the resolver only
	// reads a session's repo id, once per candidate chat on the fan-out path, so
	// recomputing display state for every candidate would be pure cost.
	w.broadcasts = db.NewBroadcastStore(database)
	w.broadcastResolver = broadcast.NewResolver(w.agentChats, w.rawSessions, log.Logger)
	// Standing broadcast subscriptions (BOS-557): a rule that fires a broadcast
	// when its owning session reaches a matching outcome. The evaluator fires
	// through the SAME broadcast.Sender the SendBroadcast RPC sends through, so a
	// fired subscription is not a second delivery path.
	//
	// Constructed here, ahead of the session store, because it is wired INTO that
	// store as its transition observer (see below) — the whole point of the
	// design is that there is one place to hook.
	w.broadcastSubscriptions = db.NewBroadcastSubscriptionStore(database)
	w.broadcastSubscriptionEvaluator = broadcast.NewSubscriptionEvaluator(
		w.broadcastSubscriptions,
		broadcast.NewSender(w.broadcasts, w.broadcastResolver, log.Logger),
		nil, // default clock
		log.Logger,
	).WithSessionStates(w.rawSessions)
	w.accounts = db.NewAgentAccountStore(database)
	w.telemetryClient = daemontelemetry.NewClient(w.settings)
	w.deferCleanup(func() { w.telemetryClient.Close() })
	// Account-rotation policy engine (BOS-173). Held on the daemon for the
	// headless/interactive auto-rotate consumers (BOS-174/175) to call; no cap
	// signal invokes it yet.
	w.rotationEngine = rotation.NewEngine(w.accounts, rotation.WithDefaultCooldown(w.settings.ManagedAccounts.DefaultCooldown()), rotation.WithTelemetry(w.telemetryClient))
	// Credential blobs for account rotation live in the OS keyring, never in
	// SQLite (decision D3). accountcred links keyring/dbus and is daemon-only.
	w.accountCreds = accountcred.New()
	w.resumeAttempts = db.NewResumeAttemptStore(database)
	// Rotation audit trail (BOS-176): one store, shared by the Recorder (which
	// every rotation decision path writes through), the gRPC server (which
	// hydrates Session.rotation_events for local reads), and the reverse stream
	// hydrator (which publishes full Session replacements to bosso).
	// Auditing never fails a rotation — the Recorder swallows insert errors.
	w.rotationEvents = db.NewRotationEventStore(database)
	// agentRuns is created early so the reverse-stream hydrator can read a
	// session's runs for the restart-proof agent activity; the open-run
	// reconcile below still runs where it always did.
	w.agentRuns = db.NewAgentRunStore(database)
	// wedgeActivityCache is shared by the reverse-stream hydrator and the
	// server so the web and the TUI read the same restart-proof activity.
	w.wedgeActivityCache = server.NewWedgeActivityCache()
	w.rotationRecorder = rotation.NewRecorder(rotation.NewAuditStoreAdapter(w.rotationEvents), log.Logger)

	// The display-status computer needs to read the bare stores; wrap them
	// after construction so the computer's own writes don't recurse through
	// the recompute hooks (the wrapper short-circuits on display-only writes,
	// but reading via the unwrapped store is also free of side effects).
	w.chatStatusTracker = status.NewTracker()
	w.displayTracker = status.NewDisplayTracker()
	// Single-repairer lease, shared between the plugin host (which takes/releases
	// it via SetRepairStatus) and the API server (which reads it for
	// Session.repair_active). See BOS-234.
	w.repairLease = status.NewRepairLeaseManager()
	w.displayComputer = status.NewDisplayStatusComputer(
		w.rawSessions, w.displayTracker, w.chatStatusTracker, w.agentChats, rawWorkflows, log.Logger,
	)
	// Waiting derivation (BOS-668): a chat whose only outstanding work is an
	// armed GitHub PR callback is parked, not working. The lookup is the ONLY
	// evidence source — without it the computer leaves every chat working — and
	// it reads the callback store directly rather than widening the computer's
	// constructor, so a daemon built without callbacks simply never waits.
	w.displayComputer.SetWaitingLookup(status.WaitingLookupFunc(
		func(ctx context.Context, agentSessionID string) (string, error) {
			return callback.WaitingReasonForChat(ctx, w.githubCallbacks, agentSessionID, time.Now())
		},
	))
	// THE single session state-transition seam (BOS-557). Roughly two dozen call
	// sites write sessions.state, through Update plus the conditional and orphan
	// methods; all of them hold this decorated store, so attaching the standing-
	// subscription evaluator here observes every one of them exactly once without
	// instrumenting any subsystem. See RecomputingSessionStore's doc comment for
	// why it is layered ON the decorator rather than around it, and for the one
	// known gap (MarkOrphansArchived) the periodic reconcile sweep covers.
	w.callbackRetirer = callback.NewRetirer(w.githubCallbacks, w.agentChats, w.rawSessions, w.repos, w.displayComputer, time.Now, log.Logger)
	w.terminalReapObserver = &status.TerminalReapObserver{}
	w.sessions = status.NewRecomputingSessionStore(w.rawSessions, w.displayComputer).
		WithTransitionObserver(broadcast.TransitionObservers{w.callbackRetirer, w.terminalReapObserver, w.broadcastSubscriptionEvaluator})
	var workflows db.WorkflowStore = status.NewRecomputingWorkflowStore(rawWorkflows, w.displayComputer)

	// Wire the display tracker so its mutations recompute synchronously.
	w.displayTracker.SetRecomputer(w.displayComputer)

	// streamBus is the in-process pub/sub the upstream stream client
	// drains. Created here (rather than later, where the upstream
	// machinery is configured) so the chat-status tracker hook below can
	// publish per-chat status deltas onto it. Closed by the daemon's
	// shutdown path; see deferred Close() below.
	w.streamBus = upstream.NewStreamBus(log.Logger)
	w.deferCleanup(func() { w.streamBus.Close() })

	// accountLabeler resolves the human-friendly account label for the reverse
	// stream. It needs only the registry (the full accountResolver below also
	// wires a credential materializer that depends on plugin clients not yet
	// constructed here), so it is a lightweight label-only Resolver. Label
	// degrades safely: "System default" for account 0, a short id on any miss —
	// so the stream overlay never hard-fails.
	accountLabeler := account.NewResolver(accountwiring.NewRegistry(w.accounts), nil, log.Logger)

	w.streamHydrator = &streamSessionHydrator{
		agentChats:        w.agentChats,
		rawSessions:       w.rawSessions,
		repos:             w.repos,
		chatStatusTracker: w.chatStatusTracker,
		displayTracker:    w.displayTracker,
		rotationEvents:    w.rotationEvents,
		wedgeActivity:     server.WedgeActivity{Runs: server.AgentRunsForWedge(w.agentRuns), Cache: w.wedgeActivityCache, Logger: log.Logger},
		accountLabeler:    accountLabeler,
		attention: func(ctx context.Context, p *bossanovav1.Session) {
			if srv := w.streamAttentionServer.Load(); srv != nil {
				srv.HydrateAttentionForStream(ctx, p)
			}
		},
		logger: log.Logger,
	}

	// hydrateSessionForStream applies the same last_agent_activity_at +
	// AGENT_AUTH_FAILED observability overlay, account label, and rotation-event
	// history that the local GetSession/ListSessions RPCs add, to a Session proto
	// bound for the reverse stream.
	// bosso applies session deltas as FULL replacements, so EVERY session
	// UPDATED delta (and the DaemonSnapshot) must carry this overlay
	// consistently — otherwise a display recompute that omits it would clobber a
	// live AGENT_AUTH_FAILED attention back off in the cloud/web read model.
	// Fails toward NOT flagging on any lookup error (the overlay is best-effort
	// enrichment, never a hard dependency of the delta).
	w.hydrateSessionForStream = w.streamHydrator.Hydrate

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

	// transientResumer auto-resumes chats whose turn died on a retryable 5xx API
	// banner (BOS-518). Declared alongside chatRotator and for the same reason:
	// the chat-store and tracker closures below must reference it, but it needs
	// srv (for the delivery seam), which is constructed much later. Every use
	// site is nil-guarded to cover that window.

	// transientResumeHookInstalled records that the tracker transition hook — the
	// resumer's ONLY trigger — was actually wired, so the seams probe can report
	// the lane live rather than merely constructed.
	w.transientResumeHookInstalled = false

	// archiveSeam is the server, published once it is constructed (much later
	// than this hook), so a chat-status transition can nudge the archive
	// deferrer (BOS-1380). Atomic because tracker hooks fire on poller and RPC
	// goroutines; a nil load covers the startup window before srv exists.

	w.chatStatusTracker.SetOnUpdate(func(agentSessionID string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		chat, err := w.agentChats.GetByAgentSessionID(ctx, agentSessionID)
		if err != nil || chat == nil {
			return
		}
		// Publish the per-chat status delta first so the stream sees a
		// fresh status alongside any session-level UPDATED delta the
		// recompute below emits via displayComputer.SetOnUpdate.
		if entry := w.chatStatusTracker.Get(agentSessionID); entry != nil {
			// The waiting marker (BOS-668) is derived by the recompute BELOW,
			// so this delta carries the previous pass's marker. That is the
			// right trade: publishing promptly matters more than a one-tick-old
			// reason, and SetOnWaitingChange re-publishes the moment the
			// derivation actually flips the chat.
			st, reason := status.PromoteWaiting(entry.Status, w.chatStatusTracker.Waiting(agentSessionID))
			w.streamBus.Publish(upstream.StreamEvent{
				Status: &upstream.StatusEvent{
					Status: &bossanovav1.ChatStatusDelta{
						SessionId:      chat.SessionID,
						AgentSessionId: agentSessionID,
						Status:         st,
						WaitingReason:  reason,
						LastOutputAt:   timestamppb.New(entry.LastOutputAt),
					},
				},
			})
		}
		_ = w.displayComputer.Recompute(ctx, chat.SessionID)

		// A chat leaving WORKING may free a session whose archive (BOS-1380)
		// or unattended-run finish (BOS-1508) is waiting on it. The nudges are
		// non-blocking.
		if srv := w.archiveSeam.Load(); srv != nil {
			srv.NudgePendingArchive(chat.SessionID)
			srv.NudgePendingFinish(chat.SessionID)
		}

		// Auto-rotate interactive chats on LIMITED transitions (BOS-175). The
		// tracker only fires this hook on real transitions, and the rotator itself
		// is non-blocking + internally rate-limited, so a no-op call on every other
		// status transition is cheap.
		if w.chatRotator != nil {
			if entry := w.chatStatusTracker.Get(agentSessionID); entry != nil {
				w.chatRotator.OnChatStatus(agentSessionID, entry.Status, entry.ResetAt)
			}
		}
	})

	// Republish a chat's status when its WAITING derivation flips (BOS-668).
	// Entering or leaving "parked on an external event" is not a tracker Update
	// — the heartbeat still says WORKING throughout — so without this hook the
	// stream would carry the change only on the chat's next unrelated
	// transition. The tracker fires it exactly on a real reason change, so a PR
	// that sits armed for an hour produces one event, not one per poll.
	w.chatStatusTracker.SetOnWaitingChange(func(agentSessionID string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		chat, err := w.agentChats.GetByAgentSessionID(ctx, agentSessionID)
		if err != nil || chat == nil {
			return
		}
		entry := w.chatStatusTracker.Get(agentSessionID)
		if entry == nil {
			// The tracker dropped the chat on its way out — Cleanup collecting a
			// pane that stopped heartbeating past StaleThreshold, or an explicit
			// Remove — and fired this hook as it cleared the marker. There is no
			// chat status left to publish a delta for, but the SESSION's persisted
			// composite may still read "waiting", and nothing else would ever
			// clear it: sweepWaitingChats iterates tracker entries and this one is
			// gone, so the next recompute would otherwise wait for an unrelated
			// session-store write or a daemon restart. Recompute here so a dead
			// pane stops claiming an external event is still pending.
			_ = w.displayComputer.Recompute(ctx, chat.SessionID)
			return
		}
		st, reason := status.PromoteWaiting(entry.Status, w.chatStatusTracker.Waiting(agentSessionID))
		w.streamBus.Publish(upstream.StreamEvent{
			Status: &upstream.StatusEvent{
				Status: &bossanovav1.ChatStatusDelta{
					SessionId:      chat.SessionID,
					AgentSessionId: agentSessionID,
					Status:         st,
					WaitingReason:  reason,
					LastOutputAt:   timestamppb.New(entry.LastOutputAt),
				},
			},
		})
	})

	// BOS-1096: recompute a session's display composite when the tracker
	// destroys one of its chat entries. Eviction is the one input to
	// display_label that was never an edge — Recompute fires on status
	// TRANSITIONS, and a chat that simply stops heartbeating never transitions,
	// so the label stood at whatever the last edge wrote until a daemon restart
	// ran the boot backfill. Covers both deleting paths, since the tracker fires
	// this hook from Cleanup's staleness sweep and from Remove (DeleteChat).
	//
	// Installed here rather than inline so the seam probe below can assert a
	// booted daemon actually has it: dropping this single line leaves every
	// other test for this fix green while the reported symptom returns in full.
	wireEvictionRecompute(w.chatStatusTracker, w.agentChats, w.displayComputer, log.Logger)
	w.evictionRecomputeHookInstalled = true

	// Emit a structured audit log each time a chat enters or leaves the
	// usage-limited state. Fired exactly once per transition by the tracker;
	// the durable sink is Epic 4.4. Logging only — no behavior change.
	w.chatStatusTracker.SetOnLimitTransition(func(agentSessionID string, entered bool) {
		event := "limit-recovered"
		if entered {
			event = "limit-entered"
		}
		entry := log.Info().Str("agent_session_id", agentSessionID).Bool("entered", entered)
		if e := w.chatStatusTracker.Get(agentSessionID); e != nil && !e.ResetAt.IsZero() {
			entry = entry.Time("reset_at", e.ResetAt)
		}
		entry.Msg(event)
	})

	// Wire chat-store mutations onto the stream bus. Without this the
	// orchestrator only ever sees chats from the initial DaemonSnapshot,
	// so any chat created/renamed/deleted after the daemon connects is
	// invisible to the web UI's per-session chat list.
	w.agentChats.OnChange = func(kind upstream.ChatChangeKind, chat *domain.AgentChat) {
		var pbKind bossanovav1.ChatDelta_Kind
		switch kind {
		case upstream.ChatChangeCreated:
			pbKind = bossanovav1.ChatDelta_KIND_CREATED
		case upstream.ChatChangeUpdated:
			pbKind = bossanovav1.ChatDelta_KIND_UPDATED
		case upstream.ChatChangeDeleted:
			pbKind = bossanovav1.ChatDelta_KIND_DELETED
			// Tear down any in-memory respawn-in-place state (healthy streak, pending
			// re-probe timer, respawn-cap window) for a chat that is going away, so no
			// orphaned re-probe timer can re-drive the auth path after the pane is gone
			// (BOS-482). Nil-guarded like the rotate hook: the rotator is constructed
			// after this closure is installed.
			if w.chatRotator != nil {
				w.chatRotator.Deregister(chat.AgentSessionID)
			}
			// Same reasoning for the auto-resume lane (BOS-518): a chat whose row is
			// gone must not keep an armed settle/backoff timer that would later
			// deliver a resume prompt into a pane nobody owns.
			if w.transientResumer != nil {
				w.transientResumer.Deregister(chat.AgentSessionID)
			}
		default:
			return
		}
		w.streamBus.Publish(upstream.StreamEvent{
			Chat: &upstream.ChatEvent{
				Kind: pbKind,
				Chat: &bossanovav1.AgentChatMetadata{
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
	w.sessionLister = &sessionListerAdapter{sessions: w.sessions}

	// Fail any workflows left in running/pending state from a previous daemon
	// instance. Their driving goroutines no longer exist after a restart.
	if n, err := workflows.MarkFailed(context.Background()); err != nil {
		log.Warn().Err(err).Msg("failed to clean up orphaned workflows")
	} else if n > 0 {
		log.Info().Int64("count", n).Msg("failed orphaned workflows from previous run")
	}

	// Fail any task mappings left in Pending/InProgress from a previous
	// daemon instance. Their driving goroutines no longer exist.
	if n, err := w.taskMappings.MarkOrphaned(context.Background()); err != nil {
		log.Warn().Err(err).Msg("failed to clean up orphaned task mappings")
	} else if n > 0 {
		log.Info().Int64("count", n).Msg("failed orphaned task mappings from previous run")
	}

	// NOTE: MarkOrphansArchived and the display-status backfill were moved to
	// run *after* lifecycle.Bootstrap (see below). Bootstrap's headless orphan
	// sweep must mark restart-killed `boss new --detach` runs ORPHANED before the
	// MarkOrphansArchived bulk-advance runs — otherwise those workflow-less
	// ImplementingPlan rows would be moved to AwaitingChecks first and their
	// bootstrap-only PR would read as a normal green checks session (BOS-229). The
	// backfill follows both so it recomputes labels from the final states.

	return nil
}
