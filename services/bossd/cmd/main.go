// Package main is the entry point for the bossd daemon.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/recurser/bossalib/buildinfo"
	"github.com/recurser/bossalib/config"
	bossalog "github.com/recurser/bossalib/log"
	"github.com/recurser/bossalib/migrate"
	"github.com/rs/zerolog/log"

	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossd/internal/claude"
	"github.com/recurser/bossd/internal/db"
	gitpkg "github.com/recurser/bossd/internal/git"
	"github.com/recurser/bossd/internal/server"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/status"
	"github.com/recurser/bossd/internal/upstream"
	"github.com/recurser/bossd/internal/vcs/github"
	"github.com/recurser/bossd/migrations"
)

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

	// --- Lifecycle ---

	worktrees := gitpkg.NewManager(log.Logger)
	claudeRunner := claude.NewRunner(log.Logger)
	ghProvider := github.New(log.Logger)
	lifecycle := session.NewLifecycle(sessions, repos, worktrees, claudeRunner, ghProvider, log.Logger)

	// --- Fix Loop + Dispatcher + Poller ---

	fixLoop := session.NewFixLoop(sessions, attempts, repos, ghProvider, claudeRunner, worktrees, log.Logger)
	dispatcher := session.NewDispatcher(sessions, repos, ghProvider, fixLoop, log.Logger)
	poller := session.NewPoller(sessions, repos, ghProvider, session.DefaultPollInterval, log.Logger)

	// --- Chat Status Tracker ---

	chatStatusTracker := status.NewTracker()

	// --- PR Display Tracker + Poller ---

	prDisplayTracker := status.NewPRTracker()
	settings, _ := config.Load()
	displayPoller := session.NewDisplayPoller(
		sessions, repos, ghProvider, prDisplayTracker,
		settings.DisplayPollInterval(), log.Logger,
	)

	// --- Server ---

	socketPath, err := server.DefaultSocketPath()
	if err != nil {
		return fmt.Errorf("socket path: %w", err)
	}

	srv := server.New(server.Config{
		Repos:       repos,
		Sessions:    sessions,
		Attempts:    attempts,
		ClaudeChats: claudeChats,
		ChatStatus:  chatStatusTracker,
		PRDisplay:   prDisplayTracker,
		Lifecycle:   lifecycle,
		Claude:      claudeRunner,
		Worktrees:   worktrees,
		Provider:    ghProvider,
		Logger:      log.Logger,
	})

	// --- Upstream (optional, cloud mode) ---

	var upstreamMgr *upstream.Manager
	if cfg := upstream.ConfigFromEnv(); cfg != nil {
		upstreamMgr = upstream.NewManager(*cfg, log.Logger)

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
			log.Warn().Err(err).Msg("upstream connection failed, running in local-only mode")
			upstreamMgr = nil
		}
	}

	// Start poller and dispatcher.
	pollerCtx, pollerCancel := context.WithCancel(context.Background())
	defer pollerCancel()
	events := poller.Run(pollerCtx)
	safego.Go(log.Logger, func() { dispatcher.Run(pollerCtx, events) })

	// Start display status poller.
	displayPoller.Run(pollerCtx)

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

	// Stop upstream heartbeat.
	if upstreamMgr != nil {
		upstreamMgr.Stop()
	}

	// Stop poller and dispatcher.
	pollerCancel()

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
