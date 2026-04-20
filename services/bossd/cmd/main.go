// Package main is the entry point for the bossd daemon.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
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
	repos    db.RepoStore
}

// ListSessions returns all active (non-archived) sessions as protobuf.
func (a *sessionListerAdapter) ListSessions(ctx context.Context) ([]*bossanovav1.Session, error) {
	// List all active sessions from local DB (pass empty string for all repos)
	allSessions, err := a.sessions.ListActive(ctx, "")
	if err != nil {
		return nil, err
	}

	// Convert to protobuf
	var pbSessions []*bossanovav1.Session
	for _, s := range allSessions {
		pbSessions = append(pbSessions, server.SessionToProto(s))
	}

	// Denormalize repo_display_name, caching to avoid redundant DB calls.
	repoCache := make(map[string]string)
	for _, pbSess := range pbSessions {
		if name, ok := repoCache[pbSess.RepoId]; ok {
			pbSess.RepoDisplayName = name
			continue
		}
		repo, err := a.repos.Get(ctx, pbSess.RepoId)
		if err == nil {
			repoCache[pbSess.RepoId] = repo.DisplayName
			pbSess.RepoDisplayName = repo.DisplayName
		}
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

	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "bossd: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Human-friendly console logging.
	bossalog.Setup("bossd")

	// --- Database ---

	dbPath, err := db.DefaultDBPath()
	if err != nil {
		return fmt.Errorf("db path: %w", err)
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
	sessions := db.NewSessionStore(database)
	attempts := db.NewAttemptStore(database)
	claudeChats := db.NewClaudeChatStore(database)
	taskMappings := db.NewTaskMappingStore(database)
	workflows := db.NewWorkflowStore(database)

	// Create session lister for upstream sync
	sessionLister := &sessionListerAdapter{sessions: sessions, repos: repos}

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
	poller := session.NewPoller(sessions, repos, ghProvider, session.DefaultPollInterval, log.Logger)

	// --- Chat Status Tracker ---

	chatStatusTracker := status.NewTracker()

	// --- PR Display Tracker + Poller ---

	prDisplayTracker := status.NewPRTracker()
	settings, _ := config.Load()

	// Update boss skills if they were previously installed by the CLI.
	if skillsDir, err := skilldata.DefaultSkillsDir(); err == nil && skilldata.BossSkillsInstalled(skillsDir) {
		if err := skilldata.ExtractSkills(skillsDir, skilldata.SkillsFS); err != nil {
			log.Warn().Err(err).Msg("failed to update skills")
		}
	}

	displayPoller := session.NewDisplayPoller(
		sessions, repos, ghProvider, prDisplayTracker,
		settings.DisplayPollInterval(), log.Logger,
	)

	// --- Plugin Host ---

	pluginBus := eventbus.New(log.Logger)
	pluginHost := plugin.New(pluginBus, ghProvider, log.Logger)
	autopilotRunner := claude.NewTmuxRunner(tmuxClient, log.Logger)
	pluginHost.SetWorkflowDeps(workflows, sessions, claudeChats, autopilotRunner)
	pluginHost.SetSessionDeps(repos, sessions, prDisplayTracker, chatStatusTracker)

	// Register PRTracker onChange callback to notify plugins of status changes
	prDisplayTracker.SetOnChange(func(sessionID string, oldEntry, newEntry *status.PRDisplayEntry) {
		if newEntry != nil {
			pluginHost.NotifyStatusChange(context.Background(), sessionID, newEntry.Status, newEntry.HasFailures)
		}
	})

	if len(settings.Plugins) == 0 {
		settings.Plugins = config.DiscoverPlugins()
		if len(settings.Plugins) > 0 {
			log.Info().Int("count", len(settings.Plugins)).Msg("auto-discovered plugins")
			if err := config.Save(settings); err != nil {
				log.Warn().Err(err).Msg("failed to persist discovered plugins to settings")
			} else {
				log.Info().Msg("persisted discovered plugins to settings")
			}
		}
	}

	if err := pluginHost.Start(context.Background(), settings.Plugins); err != nil {
		pluginBus.Close()
		return fmt.Errorf("plugin host: %w", err)
	}

	// Auto-start repair plugin workflows if available
	safego.Go(log.Logger, func() {
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
		livenessChecker, taskorchestrator.DefaultPollInterval, log.Logger,
	)

	// Wire the orchestrator as the completion notifier for the dispatcher
	// and server so that terminal session states unblock the per-repo task queue.
	dispatcher.SetCompletionNotifier(orchestrator)

	// --- Tmux Status Poller ---

	tmuxStatusPoller := status.NewTmuxStatusPoller(chatStatusTracker, claudeChats, tmuxClient, log.Logger)

	// --- Server ---

	socketPath, err := server.DefaultSocketPath()
	if err != nil {
		return fmt.Errorf("socket path: %w", err)
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
		PRDisplay:          prDisplayTracker,
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
	safego.Go(log.Logger, func() { dispatcher.Run(pollerCtx, events) })

	// Start task orchestrator (polls plugin task sources).
	orchestrator.Start(pollerCtx)

	// Start display status poller.
	displayPoller.Run(pollerCtx)

	// Bootstrap tmux status poller with pre-existing sessions before starting
	// the polling loop, so sessions from before a daemon restart show correct
	// status (idle/question) instead of defaulting to unknown.
	tmuxStatusPoller.Bootstrap(context.Background())

	// Start tmux status poller (captures pane content to detect question/idle/working).
	tmuxStatusPoller.Run(pollerCtx)

	// Start chat status cleanup goroutine (GC stale entries every 30s).
	safego.Go(log.Logger, func() {
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

	// Start server in a goroutine.
	errCh := make(chan error, 1)
	safego.Go(log.Logger, func() {
		log.Info().Str("socket", socketPath).Msg("starting server")
		errCh <- srv.ListenAndServe(socketPath)
	})

	// --- Signal handling ---

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
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

	log.Info().Msg("daemon stopped")
	return nil
}
