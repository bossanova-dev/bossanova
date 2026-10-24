package main

import (
	"context"

	"github.com/recurser/bossd/internal/adapter/dotenv"
	"github.com/recurser/bossd/internal/adapter/verifybridge"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog/log"

	server "github.com/recurser/bossd/api/daemon"
	"github.com/recurser/bossd/api/upstream"
	gitpkg "github.com/recurser/bossd/internal/adapter/git"
	"github.com/recurser/bossd/internal/adapter/proofenv"
	"github.com/recurser/bossd/internal/adapter/sessionports"
	"github.com/recurser/bossd/internal/adapter/tccprobe"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/taskorchestrator"
)

func (w *daemonWiring) wireDaemon() {
	// sessionPortsTracker discovers verified machine-local HTTP endpoints for a
	// session's tmux process tree (BOS-472). It backs the BOS-473 opt-in local
	// endpoint hydration and is nil-safe on the server side. The tracker owns a
	// background worker over the tmux pane lister; Close cancels it on shutdown.
	// It only scans sessions a caller has requested, so an idle daemon does no
	// work. tmux errors surface as empty endpoint lists (fail-soft).
	sessionPortsTracker := sessionports.New(context.Background(), w.tmuxClient.PanePID, sessionports.WithLogger(log.Logger))
	w.deferCleanup(func() { sessionPortsTracker.Close() })

	// The daemon-scoped context. Created here rather than beside the poller
	// below because server.New now needs it too: BOS-720's bootstrap runner
	// starts session bootstraps on THIS context, so a client disconnect cannot
	// abort one and daemon shutdown still can. It is handed to the lifecycle
	// unchanged further down (lifecycle.SetDaemonCtx), and pollerCancel is
	// invoked during shutdown, draining all armed polls.
	w.pollerCtx, w.pollerCancel = context.WithCancel(context.Background())
	w.deferCleanup(func() { w.pollerCancel() })

	// bootstrapRunner must be built on pollerCtx, never on a request context and
	// never on context.Background(): the first would put us back where BOS-720
	// started, and the second would leave bootstraps running past shutdown.
	bootstrapRunner := session.NewBootstrapRunner(w.pollerCtx, w.lifecycle, log.Logger)

	w.srv = server.New(server.Config{
		ChatTmux:            w.tmuxClient,
		ProtectedRootsProbe: tccprobe.Probe,
		NewVerifyVerbs:      func(env []string) server.VerifyVerbs { return &verifybridge.Bridge{Env: env} },
		EnvironmentOverlay:  dotenv.OverlayWithRepo,
		ValidateBranchName:  gitpkg.ValidateBranchName,
		VerifyProofKeys:     proofenv.Keys(),
		BootstrapRunner:     bootstrapRunner,
		AgentLogsDir:        w.agentLogsDir,
		Repos:               w.repos,
		Sessions:            w.sessions,
		AgentChats:          w.agentChats,
		TaskMappings:        w.taskMappings,
		CronJobs:            w.cronJobs,
		GithubCallbacks:     w.githubCallbacks,
		Telemetry:           w.telemetryClient,
		Notes:               w.notes,
		NoteSyncStates:      w.noteSyncStates,
		NoteSyncWorker:      noteSyncNudger(w.noteSyncWorker),
		Broadcasts:          w.broadcasts,
		BroadcastResolver:   w.broadcastResolver,
		// Cross-daemon egress (BOS-558): both are zero unless an upstream is
		// configured, which leaves the send path purely local. DaemonID is the
		// one persisted id resolved for the reverse stream — the same value the
		// ingress loop guard compares against, so it is threaded, never
		// re-derived.
		BroadcastEgress: w.broadcastEgress,
		DaemonID:        w.persistedDaemonID,
		WebhookEvidence: w.webhookEvidence,
		// The RPC surface gets create/read/cancel only; firing stays with the
		// evaluator wired into the session store above.
		BroadcastSubscriptions:  w.broadcastSubscriptions,
		Accounts:                w.accounts,
		RotationEngine:          w.rotationEngine,
		Resolver:                w.accountResolver,
		AccountCredentials:      w.accountCreds,
		AccountSmokeRunner:      w.accountSmokeHook,
		UsageProbe:              w.accountUsageProbe,
		AccountMaterializations: w.accountMaterializations,
		CheckSnapshots:          w.checkSnapshots,
		AgentRuns:               w.agentRuns,
		RotationEvents:          w.rotationEvents,
		Evidence:                w.evidenceReader,
		ResumeAttempts:          w.resumeAttempts,
		PendingTrackerMerges:    w.pendingTrackerMerges,
		WedgeActivityCache:      w.wedgeActivityCache,
		CronScheduler:           w.cronScheduler,
		CronActivity:            w.cronActivity,
		ChatStatus:              w.chatStatusTracker,
		StatusRecomputer:        w.displayComputer,
		DisplayTracker:          w.displayTracker,
		PRRefresher:             w.displayPoller,
		RepairLease:             w.repairLease,
		TmuxPoller:              w.tmuxStatusPoller,
		Lifecycle:               w.lifecycle,
		Agent:                   w.agentRunner,
		AgentClients:            w.agentClients,
		Worktrees:               w.worktrees,
		Provider:                w.ghProvider,

		PRResolver:         w.prAssociationResolver,
		PluginHost:         w.pluginHost,
		Tmux:               w.tmuxClient,
		CompletionNotifier: w.orchestrator,
		AuthNotifier:       w.authNotifier,
		AuthStateReporter:  w.authStateReporter,
		// Publish a SessionDelta_KIND_DELETED on the reverse stream for
		// every session row removed from the DB (failed setup cleanup,
		// RemoveSession, EmptyTrash). Without this, bosso's in-memory
		// Registry retains the session until the daemon reconnects and
		// replaces its state from a fresh DaemonSnapshot — so failed
		// sessions linger as "stopped" rows in the web UI long after the
		// local TUI has lost sight of them.
		OnSessionDeleted: func(_ context.Context, sessionID string) {
			w.streamBus.Publish(upstream.StreamEvent{
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
			w.hydrateSessionForStream(ctx, sess)
			w.streamBus.Publish(upstream.StreamEvent{
				Session: &upstream.SessionEvent{
					Kind:    bossanovav1.SessionDelta_KIND_UPDATED,
					Session: sess,
				},
			})
		},
		Logger:         log.Logger,
		FileLimitSoft:  achievedFileLimitSoft,
		EndpointReader: sessionPortsTracker,
		ProtectedRoots: w.startupProtectedRoots,
	})

	w.lifecycle.SetRunResumeStore(w.resumeAttempts)
	w.streamAttentionServer.Store(w.srv)

	w.archiveSeam.Store(w.srv)

	// Every automatic archiver below goes through the server's deferral seam
	// (BOS-1380), not straight to ArchiveSessionAndNotify: a session whose chat
	// is still working is archived once every chat is idle, so an in-session
	// merge no longer tears the worktree out from under its own agent. With no
	// busy chat the seam archives synchronously, exactly as before.
	//
	// Auto-archive dependabot repair sessions when their PR merges (BOS-101).
	// The server's archive-and-notify path also emits the stream update so the
	// session leaves the TUI immediately.
	w.orchestrator.SetSessionArchiver(taskorchestrator.SessionArchiverFunc(w.srv.RequestArchiveAutomatic), w.trackArchiveDone)

	// Auto-archive a session when its PR merges, if the repo has the
	// ShouldArchiveSessionsAfterMerge flag on (BOS-46). Reuses the same
	// archive path as the dependabot auto-archive above.
	w.dispatcher.SetArchiver(session.ArchiverFunc(w.srv.RequestArchiveAutomatic), w.trackArchiveDone)

	// The webhook is not the only path to Merged (BOS-697). The display
	// poller's terminal reconcile lands it whenever the merge webhook never
	// arrives — and also backs MergeSession's synchronous post-merge refresh —
	// so it needs the same archiver, or those merges never auto-archive.
	w.displayPoller.SetArchiver(session.ArchiverFunc(w.srv.RequestArchiveAutomatic), w.trackArchiveDone)

	// Heal rows that reached Merged while the archive hook was unreachable
	// (BOS-697). Wired here rather than into the builder chain above because
	// srv — and therefore its archive seam — does not exist until now; the
	// option mutates the resolver in place, so the periodic reconcile picks it
	// up. The startup reconcile above runs without it, which only defers the
	// heal to the first tick. After a daemon restart this sweep is also what
	// re-derives merge-driven archives that were pending in memory.
	w.prAssociationResolver.WithArchiver(session.ArchiverFunc(w.srv.RequestArchiveAutomatic), w.trackArchiveDone)

	// Every archiver above must have been handed trackArchiveDone, or its
	// archives run outside shutdown coordination again — the exact defect BOS-923 fixed, and
	// one with no shape at runtime. Report liveness (and the tracker itself) so a
	// startup test can assert both the wiring and the join it buys.
	if w.opts.onArchiveTrackerSeamsWired != nil {
		w.opts.onArchiveTrackerSeamsWired(
			w.orchestrator.HasArchiveTracker() &&
				w.dispatcher.HasArchiveTracker() &&
				w.displayPoller.HasArchiveTracker() &&
				w.prAssociationResolver.HasArchiveTracker(),
			w.trackArchiveDone,
		)
	}

	// Rebase the repo's other in-flight branches onto the base a merged PR just
	// advanced, when the repo opted in (BOS-521). Wired on the merged-webhook
	// path so it also covers PRs merged outside boss.
	w.dispatcher.SetBaseAdvanceNotifier(session.BaseAdvanceNotifierFunc(w.srv.NotifyBaseAdvanced))

	// Wire the chat-waker on the existing CommandHandlerAdapter now that
	// the server is constructed. Done post-hoc rather than at adapter
	// construction time because the adapter is built before srv.New
	// (it's passed into the StreamClient configuration above) — and
	// CommandHandlerAdapter is a pointer, so this mutates the same
	// instance the StreamClient's interface dispatches through.
	if w.cmdHandlerStream != nil {
		w.cmdHandlerStream.Waker = w.srv
		w.cmdHandlerStream.Commands = w.srv
		if w.cmdHandlerStream.Triggers != nil {
			w.cmdHandlerStream.Triggers.Creator = w.srv
		}
	}
	// The web terminal attach needs the same waker (BOS-885): a chat whose
	// tmux_session_name was cleared is revived on attach instead of rejected,
	// exactly as the TUI attach already does. Post-hoc for the same reason as
	// above — terminalStreamClient is constructed before srv — and safely so,
	// because its Run loop is not started until further below.
	if w.terminalStreamClient != nil {
		w.terminalStreamClient.SetWaker(w.srv)
	}
	// Wire the creator adapter's server now that srv exists (it was passed
	// into the StreamClient config above as a pointer). *server.Server
	// satisfies upstream.StreamCreateSessioner via StreamCreateSession.
	if w.creatorAdapter != nil {
		w.creatorAdapter.Server = w.srv
	}
	// Same post-hoc wiring for the BOS-661 upload manager's chat sender:
	// the manager was built above (the terminal stream client needs it),
	// but its one output is a verified SendChatMessage, which only exists
	// now. Bound before any stream runs, so no upload can outrun it.
	w.chatUploads.setServer(w.srv)

}
