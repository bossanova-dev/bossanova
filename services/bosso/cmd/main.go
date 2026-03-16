// Package main is the entry point for the bosso orchestrator.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pressly/goose/v3"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bosso/internal/auth"
	"github.com/recurser/bosso/internal/db"
	"github.com/recurser/bosso/internal/relay"
	"github.com/recurser/bosso/internal/server"
	"github.com/recurser/bosso/migrations"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "bosso: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Human-friendly console logging.
	log.Logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).
		With().Timestamp().Str("service", "bosso").Logger()

	// --- Configuration ---

	addr := envOr("BOSSO_ADDR", ":8080")
	oidcIssuer := envOr("BOSSO_OIDC_ISSUER", "")
	oidcAudience := envOr("BOSSO_OIDC_AUDIENCE", "")

	if oidcIssuer == "" || oidcAudience == "" {
		return fmt.Errorf("BOSSO_OIDC_ISSUER and BOSSO_OIDC_AUDIENCE are required")
	}

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

	users := db.NewUserStore(database)
	daemons := db.NewDaemonStore(database)
	sessions := db.NewSessionRegistryStore(database)
	audit := db.NewAuditStore(database)

	// --- Auth ---

	jwtValidator := auth.NewJWTValidator(auth.JWTConfig{
		Issuer:   oidcIssuer,
		Audience: oidcAudience,
	})
	authMiddleware := auth.NewMiddleware(jwtValidator, users, daemons)

	// --- Server ---

	pool := relay.NewPool()
	srv := server.New(users, daemons, sessions, audit, pool)

	mux := http.NewServeMux()
	path, handler := bossanovav1connect.NewOrchestratorServiceHandler(srv)
	mux.Handle(path, handler)

	httpServer := &http.Server{
		Addr:    addr,
		Handler: authMiddleware.Wrap(mux),
	}

	// Start server in a goroutine.
	errCh := make(chan error, 1)
	go func() {
		log.Info().Str("addr", addr).Msg("starting orchestrator")
		errCh <- httpServer.ListenAndServe()
	}()

	// --- Signal handling ---

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		log.Info().Str("signal", sig.String()).Msg("shutting down")
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("server: %w", err)
		}
	}

	// Graceful shutdown with 5-second timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}

	log.Info().Msg("orchestrator stopped")
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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
