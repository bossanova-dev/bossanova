// Package testharness provides an E2E test harness for the bossd daemon.
// It wires together an in-memory SQLite database, mock git/claude/VCS
// implementations, and a real ConnectRPC server on a Unix socket.
package testharness

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/migrate"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/server"
	"github.com/recurser/bossd/internal/session"
	"github.com/rs/zerolog"
)

var socketCounter atomic.Int64

// Harness provides a fully wired bossd daemon for E2E tests.
type Harness struct {
	DB          *sql.DB
	Repos       db.RepoStore
	Sessions    db.SessionStore
	Attempts    db.AttemptStore
	ClaudeChats db.ClaudeChatStore
	Lifecycle   *session.Lifecycle
	Server      *server.Server
	Git         *MockWorktreeManager
	Claude      *MockClaudeRunner
	VCS         *MockVCSProvider

	// Client is a ConnectRPC client connected to the test server.
	Client bossanovav1connect.DaemonServiceClient

	socketPath string
	httpServer *http.Server
	listener   net.Listener
}

// New creates a new E2E test harness with an in-memory database,
// mock dependencies, and a running ConnectRPC server on a temp Unix socket.
func New(t *testing.T) *Harness {
	t.Helper()

	// In-memory SQLite with migrations.
	database, err := db.OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	if err := migrate.Run(database, os.DirFS(migrationsDir())); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	// Stores.
	repos := db.NewRepoStore(database)
	sessions := db.NewSessionStore(database)
	attempts := db.NewAttemptStore(database)
	claudeChats := db.NewClaudeChatStore(database)

	// Mocks.
	logger := zerolog.Nop()
	gitMock := NewMockWorktreeManager()
	claudeMock := NewMockClaudeRunner()
	vcsMock := NewMockVCSProvider()

	// Lifecycle.
	lifecycle := session.NewLifecycle(sessions, repos, gitMock, claudeMock, vcsMock, logger)

	// Server.
	srv := server.New(repos, sessions, attempts, claudeChats, lifecycle, claudeMock, gitMock, vcsMock)

	// Start server on a temp Unix socket.
	// Use /tmp directly — t.TempDir() paths can exceed the 104-char Unix socket limit on macOS.
	socketPath := filepath.Join("/tmp", fmt.Sprintf("bossd-t%d.sock", socketCounter.Add(1)))
	_ = os.Remove(socketPath) // remove stale socket from previous run
	t.Cleanup(func() { _ = os.Remove(socketPath) })

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}

	mux := http.NewServeMux()
	path, handler := bossanovav1connect.NewDaemonServiceHandler(srv)
	mux.Handle(path, handler)

	httpServer := &http.Server{Handler: mux}
	go func() { _ = httpServer.Serve(ln) }()

	t.Cleanup(func() {
		_ = httpServer.Close()
	})

	// Create ConnectRPC client over Unix socket.
	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
	}
	client := bossanovav1connect.NewDaemonServiceClient(
		httpClient,
		"http://localhost",
		connect.WithGRPC(),
	)

	return &Harness{
		DB:          database,
		Repos:       repos,
		Sessions:    sessions,
		Attempts:    attempts,
		ClaudeChats: claudeChats,
		Lifecycle:   lifecycle,
		Server:      srv,
		Git:         gitMock,
		Claude:      claudeMock,
		VCS:         vcsMock,
		Client:      client,
		socketPath:  socketPath,
		httpServer:  httpServer,
		listener:    ln,
	}
}

// TempRepoDir creates a temporary directory that can pass RegisterRepo path
// validation. The directory is cleaned up when the test finishes.
func TempRepoDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return dir
}

// migrationsDir returns the absolute path to the bossd migrations directory.
func migrationsDir() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..", "migrations")
}
