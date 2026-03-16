// Package main is the entry point for the bossd daemon.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pressly/goose/v3"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/recurser/bossd/internal/claude"
	"github.com/recurser/bossd/internal/db"
	gitpkg "github.com/recurser/bossd/internal/git"
	"github.com/recurser/bossd/internal/server"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/vcs/github"
	"github.com/recurser/bossd/migrations"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "bossd: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Human-friendly console logging.
	log.Logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).
		With().Timestamp().Str("service", "bossd").Logger()

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

	if err := runMigrations(database); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}

	// --- Stores ---

	repos := db.NewRepoStore(database)
	sessions := db.NewSessionStore(database)
	attempts := db.NewAttemptStore(database)

	// --- Lifecycle ---

	worktrees := gitpkg.NewManager(log.Logger)
	claudeRunner := claude.NewRunner(log.Logger)
	ghProvider := github.New(log.Logger)
	lifecycle := session.NewLifecycle(sessions, repos, worktrees, claudeRunner, ghProvider, log.Logger)

	// --- Fix Loop + Dispatcher + Poller ---

	fixLoop := session.NewFixLoop(sessions, attempts, repos, ghProvider, claudeRunner, worktrees, log.Logger)
	dispatcher := session.NewDispatcher(sessions, repos, ghProvider, log.Logger)
	dispatcher.SetFixLoop(fixLoop)
	poller := session.NewPoller(sessions, repos, ghProvider, session.DefaultPollInterval, log.Logger)

	// --- Server ---

	socketPath, err := server.DefaultSocketPath()
	if err != nil {
		return fmt.Errorf("socket path: %w", err)
	}

	srv := server.New(repos, sessions, attempts, lifecycle, claudeRunner, worktrees, ghProvider)

	// Start poller and dispatcher.
	pollerCtx, pollerCancel := context.WithCancel(context.Background())
	defer pollerCancel()
	events := poller.Run(pollerCtx)
	go dispatcher.Run(pollerCtx, events)

	// Start server in a goroutine.
	errCh := make(chan error, 1)
	go func() {
		log.Info().Str("socket", socketPath).Msg("starting server")
		errCh <- srv.ListenAndServe(socketPath)
	}()

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

func runMigrations(database *sql.DB) error {
	goose.SetBaseFS(migrations.FS)

	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}

	if err := goose.Up(database, "."); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}

	log.Info().Msg("migrations complete")
	return nil
}
