// Package main is the entry point for the bossd daemon.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/recurser/bossalib/buildinfo"
	"github.com/recurser/bossalib/config"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	bossalog "github.com/recurser/bossalib/log"
	"github.com/recurser/bossalib/migrate"
	"github.com/rs/zerolog/log"

	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossalib/skilldata"
	"github.com/recurser/bossd/internal/claude"
	"github.com/recurser/bossd/internal/db"
	gitpkg "github.com/recurser/bossd/internal/git"
	"github.com/recurser/bossd/internal/plugin"
	"github.com/recurser/bossd/internal/plugin/eventbus"
	"github.com/recurser/bossd/internal/server"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/status"
	"github.com/recurser/bossd/internal/taskorchestrator"
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
		pbSessions = append(pbSessions, pbSess)
	}
	return pbSessions, nil
}

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
}

func run(opts runOpts) error {
	// Human-friendly console logging plus rotated file at $XDG_STATE_HOME/bossanova/logs/bossd.log.
	logCloser := bossalog.Setup("bossd")
	defer func() { _ = logCloser.Close() }()

	// --- Database ---

	dbPath := opts.dbPath
	if dbPath == "" {
		p, err := db.DefaultDBPath()
		if err != nil {
			return fmt.Errorf("db path: %w", err)
		}
		dbPath = p
	}

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
	claudeChats := db.NewClaudeChatStore(database)
	taskMappings := db.NewTaskMappingStore(database)
	rawWorkflows := db.NewWorkflowStore(database)

	// The display-status computer needs to read the bare stores; wrap them
	// after construction so the computer's own writes don't recurse through
	// the recompute hooks (the wrapper short-circuits on display-only writes,
	// but reading via the unwrapped store is also free of side effects).
	chatStatusTracker := status.NewTracker()
	displayTracker := status.NewDisplayTracker()
	displayComputer := status.NewDisplayStatusComputer(
		rawSessions, displayTracker, chatStatusTracker, claudeChats, rawWorkflows, log.Logger,
	)
	var sessions db.SessionStore = db.NewRecomputingSessionStore(rawSessions, displayComputer)
	var workflows db.WorkflowStore = db.NewRecomputingWorkflowStore(rawWorkflows, displayComputer)

	// Wire the display tracker so its mutations recompute synchronously.
	displayTracker.SetRecomputer(displayComputer)
	// Wire the chat-status tracker similarly. It is keyed by claude_id, so
	// resolve to a session before calling Recompute.
	chatStatusTracker.SetOnUpdate(func(claudeID string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		chat, err := claudeChats.GetByClaudeID(ctx, claudeID)
		if err != nil || chat == nil {
			return
		}
		_ = displayComputer.Recompute(ctx, chat.SessionID)
	})

	// Create session lister for upstream sync
	sessionLister := &sessionListerAdapter{sessions: sessions}

	// Fail any workflows left in running/pending state from a previous daemon
	// instance. Their driving goroutines no longer exist after a restart.
	if n, err := workflows.FailOrphaned(context.Background()); err != nil {
		log.Warn().Err(err).Msg("failed to clean up orphaned workflows")
	} else if n > 0 {
		log.Info().Int64("count", n).Msg("failed orphaned workflows from previous run")
	}

	// Advance sessions stuck in ImplementingPlan whose driving workflows are
	// no longer running. Must run after FailOrphaned so the subquery sees
	// the updated workflow statuses.
	if n, err := sessions.AdvanceOrphanedSessions(context.Background()); err != nil {
		log.Warn().Err(err).Msg("failed to advance orphaned sessions")
	} else if n > 0 {
		log.Info().Int64("count", n).Msg("advanced orphaned sessions to awaiting_checks")
	}

	// Fail any task mappings left in Pending/InProgress from a previous
	// daemon instance. Their driving goroutines no longer exist.
	if n, err := taskMappings.FailOrphanedMappings(context.Background()); err != nil {
		log.Warn().Err(err).Msg("failed to clean up orphaned task mappings")
	} else if n > 0 {
		log.Info().Int64("count", n).Msg("failed orphaned task mappings from previous run")
	}

	// Backfill the display-status composite for every active session. After
	// a daemon restart the in-memory inputs (chat, display tracker) are
	// empty, so the persisted display_label may not match the stored state.
	// Recomputing once at boot ensures the row matches what Compute would
	// produce given current inputs (typically "stopped" or PR-axis label),
	// so clients reading via the bosso DB-fallback path don't see stale
	// "running 2/4" labels from the previous daemon's last write.
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

	// --- Lifecycle ---

	worktrees := gitpkg.NewManager(log.Logger)
	claudeRunner := claude.NewRunner(log.Logger)
	tmuxClient := tmux.NewClient()
	ghProvider := github.New(log.Logger)
	lifecycle := session.NewLifecycle(sessions, repos, claudeChats, worktrees, claudeRunner, tmuxClient, ghProvider, log.Logger)

	// Reconcile sessions that were created before their PR existed (or
	// where PR creation happened out-of-band). Matches by branch name.
	if n, err := session.ReconcilePRAssociations(
		context.Background(), sessions, repos, ghProvider, log.Logger,
	); err != nil {
		log.Warn().Err(err).Msg("failed to reconcile PR associations")
	} else if n > 0 {
		log.Info().Int64("count", n).Msg("reconciled sessions with existing PRs")
	}

	// --- Dispatcher + Poller ---
	// Note: FixLoop removed - repair functionality moved to plugin

	dispatcher := session.NewDispatcher(sessions, repos, ghProvider, nil, log.Logger)
	poller := session.NewPoller(sessions, repos, ghProvider, session.DefaultPollInterval, session.DefaultPollTimeout, log.Logger)

	// --- Settings + Display Poller ---

	settings, _ := config.Load()

	// Update boss skills if they were previously installed by the CLI.
	if skillsDir, err := skilldata.DefaultSkillsDir(); err == nil && skilldata.BossSkillsInstalled(skillsDir) {
		if err := skilldata.ExtractSkills(skillsDir, skilldata.SkillsFS); err != nil {
			log.Warn().Err(err).Msg("failed to update skills")
		}
	}

	displayPoller := session.NewDisplayPoller(
		sessions, repos, ghProvider, displayTracker,
		settings.DisplayPollInterval(), log.Logger,
	)

	// --- Plugin Host ---

	pluginBus := eventbus.New(log.Logger)
	pluginHost := plugin.New(pluginBus, ghProvider, log.Logger)
	autopilotRunner := claude.NewTmuxRunner(tmuxClient, log.Logger)
	pluginHost.SetWorkflowDeps(workflows, sessions, claudeChats, autopilotRunner)
	pluginHost.SetSessionDeps(repos, sessions, displayTracker, chatStatusTracker)

	// Register DisplayTracker onChange callback to notify plugins of status changes
	displayTracker.SetOnChange(func(sessionID string, oldEntry, newEntry *status.DisplayEntry) {
		if newEntry != nil {
			pluginHost.NotifyStatusChange(context.Background(), sessionID, newEntry.Status, newEntry.HasFailures)
		}
	})

	pluginCfgs := settings.Plugins
	if opts.plugins != nil {
		pluginCfgs = opts.plugins
	} else if len(pluginCfgs) == 0 {
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

	if err := pluginHost.Start(context.Background(), pluginCfgs); err != nil {
		pluginBus.Close()
		return fmt.Errorf("plugin host: %w", err)
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

	// Auto-start repair plugin workflows if available
	trackedGo(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		for _, svc := range pluginHost.GetWorkflowServices() {
			infoResp, err := svc.GetInfo(ctx)
			if err != nil {
				log.Warn().Err(err).Msg("failed to get plugin info for auto-start")
				continue
			}
			if infoResp == nil {
				log.Warn().Msg("plugin returned nil info for auto-start")
				continue
			}

			// Auto-start repair plugin
			if infoResp.Name == "repair" {
				log.Info().Str("plugin_name", infoResp.Name).Msg("auto-starting repair plugin")
				repairCfgJSON, _ := json.Marshal(settings.Repair)
				_, err := svc.StartWorkflow(ctx, &bossanovav1.StartWorkflowRequest{
					ConfigJson: string(repairCfgJSON),
				})
				if err != nil {
					log.Warn().Err(err).Str("plugin_name", infoResp.Name).Msg("failed to auto-start repair plugin")
				}
			}
		}
	})

	// --- Task Orchestrator ---

	sessionCreator := taskorchestrator.NewSessionCreator(sessions, lifecycle, log.Logger)
	// Warn if tmux is not available — interactive sessions will fail at attach time.
	if !tmuxClient.Available(context.Background()) {
		log.Warn().Msg("tmux is not installed or not in PATH; interactive sessions will not work")
	}

	livenessChecker := taskorchestrator.NewLivenessChecker(sessions, claudeChats, claudeRunner, tmuxClient)
	orchestrator := taskorchestrator.New(
		pluginHost, repos, taskMappings, sessionCreator, ghProvider,
		worktrees, livenessChecker, taskorchestrator.DefaultPollInterval, log.Logger,
	)

	// Wire the orchestrator as the completion notifier for the dispatcher
	// and server so that terminal session states unblock the per-repo task queue.
	dispatcher.SetCompletionNotifier(orchestrator)

	// --- Tmux Status Poller ---

	tmuxStatusPoller := status.NewTmuxStatusPoller(chatStatusTracker, claudeChats, sessions, tmuxClient, log.Logger)

	// --- Server ---

	socketPath := opts.socketPath
	if socketPath == "" {
		p, err := server.DefaultSocketPath()
		if err != nil {
			return fmt.Errorf("socket path: %w", err)
		}
		socketPath = p
	}

	// --- Upstream (optional, cloud mode) ---

	var upstreamMgr *upstream.Manager
	if cfg := upstream.ConfigFromEnv(); cfg != nil {
		upstreamMgr = upstream.NewManager(*cfg, log.Logger, sessionLister)

		// Gather repo IDs for registration.
		allRepos, err := repos.List(context.Background())
		if err != nil {
			log.Warn().Err(err).Msg("failed to list repos for upstream registration")
		}
		var repoIDs []string
		for _, r := range allRepos {
			repoIDs = append(repoIDs, r.ID)
		}

		if err := upstreamMgr.Connect(context.Background(), repoIDs); err != nil {
			// Non-fatal: daemon works in local mode without orchestrator.
			// Keep upstreamMgr non-nil so NotifyAuthChange can reconnect later.
			log.Warn().Err(err).Msg("upstream connection failed, running in local-only mode")
		}
	}

	srv := server.New(server.Config{
		Repos:              repos,
		Sessions:           sessions,
		Attempts:           attempts,
		ClaudeChats:        claudeChats,
		Workflows:          workflows,
		ChatStatus:         chatStatusTracker,
		DisplayTracker:     displayTracker,
		TmuxPoller:         tmuxStatusPoller,
		Lifecycle:          lifecycle,
		Claude:             claudeRunner,
		Worktrees:          worktrees,
		Provider:           ghProvider,
		PluginHost:         pluginHost,
		CompletionNotifier: orchestrator,
		UpstreamMgr:        upstreamMgr,
		Logger:             log.Logger,
	})

	// Start poller and dispatcher.
	pollerCtx, pollerCancel := context.WithCancel(context.Background())
	defer pollerCancel()
	events := poller.Run(pollerCtx)
	trackDone(poller.Done())
	dispatcherDone := safego.Go(log.Logger, func() { dispatcher.Run(pollerCtx, events) })
	trackDone(dispatcherDone)

	// Start task orchestrator (polls plugin task sources).
	orchestrator.Start(pollerCtx)
	trackDone(orchestrator.Done())

	// Start display status poller.
	displayPoller.Run(pollerCtx)
	trackDone(displayPoller.Done())

	// Bootstrap tmux status poller with pre-existing sessions before starting
	// the polling loop, so sessions from before a daemon restart show correct
	// status (idle/question) instead of defaulting to unknown.
	tmuxStatusPoller.Bootstrap(context.Background())

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
		errCh <- srv.Serve()
	})

	// --- Ready hook (tests) ---

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

	// Stop upstream heartbeat.
	if upstreamMgr != nil {
		upstreamMgr.Stop()
	}

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
