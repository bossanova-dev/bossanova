// Package testharness provides an E2E test harness for the bossd daemon.
// It wires together an in-memory SQLite database, mock git/claude/VCS
// implementations, and a real ConnectRPC server on a Unix socket.
package testharness

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/migrate"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/server"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/status"
	"github.com/recurser/bossd/internal/tmux"
	"github.com/rs/zerolog"
)

var socketCounter atomic.Int64

// Options configures harness construction. Zero-value Options is equivalent
// to New(t).
type Options struct {
	// DBPath, when non-empty, opens a file-backed SQLite DB at that path.
	// Required for restart-recovery tests that need state to survive a
	// harness Close + re-open cycle.
	DBPath string
	// TmuxCommandFactory, when non-nil, is plumbed into the daemon's
	// internal tmux.Client so RecordChat can be exercised end-to-end with
	// canned `tmux` responses. Tests that don't care about tmux can leave
	// this nil — the daemon then short-circuits ensureChatTmuxSession on
	// the !Available check.
	TmuxCommandFactory tmux.CommandFactory
}

// Harness provides a fully wired bossd daemon for E2E tests.
type Harness struct {
	DB          *sql.DB
	Repos       db.RepoStore
	Sessions    db.SessionStore
	Attempts    db.AttemptStore
	ClaudeChats db.ClaudeChatStore
	Lifecycle   *session.Lifecycle
	Server      *server.Server
	Tmux        *tmux.Client
	Git         *MockWorktreeManager
	Claude      *MockClaudeRunner
	VCS         *MockVCSProvider
	// DisplayTracker backs the MergeSession "PR is not passing" guard. Leave
	// entries empty to let merges through (the guard skips when no entry
	// exists); call DisplayTracker.Set with a non-passing status to block.
	DisplayTracker *status.DisplayTracker

	// Client is a ConnectRPC client connected to the test server.
	Client bossanovav1connect.DaemonServiceClient

	socketPath string
	httpServer *http.Server
	listener   net.Listener
	closed     atomic.Bool
}

// New creates a new E2E test harness with an in-memory database,
// mock dependencies, and a running ConnectRPC server on a temp Unix socket.
func New(t *testing.T) *Harness {
	t.Helper()
	return newHarness(t, Options{})
}

// NewWithDBPath creates a harness backed by a file SQLite database at
// dbPath. The DB file persists across Close()/NewWithDBPath cycles, which
// is what restart-recovery tests rely on. If dbPath is empty the harness
// falls back to an in-memory DB (equivalent to New).
func NewWithDBPath(t *testing.T, dbPath string) *Harness {
	t.Helper()
	return newHarness(t, Options{DBPath: dbPath})
}

// NewWithOptions is the explicit constructor used by tests that need to
// inject options beyond just the DB path — currently a custom tmux
// command factory for RecordChat coverage.
func NewWithOptions(t *testing.T, opts Options) *Harness {
	t.Helper()
	return newHarness(t, opts)
}

// newHarness is the shared implementation behind New, NewWithDBPath, and
// NewWithOptions. Options.DBPath="" selects an in-memory database;
// otherwise the path is opened (and migrated) directly.
func newHarness(t *testing.T, opts Options) *Harness {
	dbPath := opts.DBPath
	t.Helper()

	var (
		database *sql.DB
		err      error
	)
	if dbPath == "" {
		database, err = db.OpenInMemory()
		if err != nil {
			t.Fatalf("open in-memory db: %v", err)
		}
	} else {
		database, err = db.Open(dbPath)
		if err != nil {
			t.Fatalf("open db at %s: %v", dbPath, err)
		}
	}
	if err := migrate.Run(database, os.DirFS(migrationsDir())); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

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
	lifecycle := session.NewLifecycle(sessions, repos, claudeChats, gitMock, claudeMock, nil, vcsMock, logger)

	// PR display tracker. Wired through to the server so MergeSession's
	// "PR is not passing" guard is reachable from tests — entries default
	// to empty, so merges fall through unless a test explicitly calls
	// DisplayTracker.Set with a non-passing status.
	display := status.NewDisplayTracker()

	// Tmux client. Tests that need RecordChat to drive tmux pass a custom
	// command factory; everyone else gets a client whose Available()
	// returns false, which short-circuits ensureChatTmuxSession.
	var tmuxClient *tmux.Client
	if opts.TmuxCommandFactory != nil {
		tmuxClient = tmux.NewClient(tmux.WithCommandFactory(opts.TmuxCommandFactory))
	} else {
		tmuxClient = tmux.NewClient(tmux.WithCommandFactory(unavailableTmuxFactory))
	}

	// Server.
	srv := server.New(server.Config{
		Repos:          repos,
		Sessions:       sessions,
		Attempts:       attempts,
		ClaudeChats:    claudeChats,
		DisplayTracker: display,
		Lifecycle:      lifecycle,
		Claude:         claudeMock,
		Worktrees:      gitMock,
		Provider:       vcsMock,
		Tmux:           tmuxClient,
		Logger:         logger,
	})

	// Start server on a temp Unix socket.
	// Use /tmp directly — t.TempDir() paths can exceed the 104-char Unix socket limit on macOS.
	socketPath := filepath.Join("/tmp", fmt.Sprintf("bossd-t%d.sock", socketCounter.Add(1)))
	_ = os.Remove(socketPath) // remove stale socket from previous run

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}

	mux := http.NewServeMux()
	path, handler := bossanovav1connect.NewDaemonServiceHandler(srv)
	mux.Handle(path, handler)

	httpServer := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = httpServer.Serve(ln) }()

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

	h := &Harness{
		DB:             database,
		Repos:          repos,
		Sessions:       sessions,
		Attempts:       attempts,
		ClaudeChats:    claudeChats,
		Lifecycle:      lifecycle,
		Server:         srv,
		Tmux:           tmuxClient,
		Git:            gitMock,
		Claude:         claudeMock,
		VCS:            vcsMock,
		DisplayTracker: display,
		Client:         client,
		socketPath:     socketPath,
		httpServer:     httpServer,
		listener:       ln,
	}

	// Single cleanup hook ensures Close runs at test teardown even when
	// the test forgets to call it explicitly. Close is idempotent.
	t.Cleanup(func() { h.Close() })

	return h
}

// Close shuts down the harness's HTTP server, closes the database, and
// removes the Unix socket file. Safe to call multiple times. Tests that
// need to simulate a daemon restart should Close, then call NewWithDBPath
// with the same dbPath. Production code never calls Close — it runs at
// test cleanup automatically.
func (h *Harness) Close() {
	if !h.closed.CompareAndSwap(false, true) {
		return
	}
	if h.httpServer != nil {
		_ = h.httpServer.Close()
	}
	if h.DB != nil {
		_ = h.DB.Close()
	}
	if h.socketPath != "" {
		_ = os.Remove(h.socketPath)
	}
}

// TempRepoDir creates a temporary directory that can pass RegisterRepo path
// validation. The directory is cleaned up when the test finishes.
func TempRepoDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return dir
}

// AttachAndDrain opens a server-streaming AttachSession RPC for the given
// session and collects responses until stop returns true, the context
// deadline is reached, or the stream ends. It returns every message received
// (including the one that satisfied stop).
//
// The predicate stop runs on each received response; returning true closes
// the stream and returns. This is the canonical drain pattern for tests that
// want to verify a particular event sequence without sleeping.
func (h *Harness) AttachAndDrain(ctx context.Context, sessionID string, stop func(*pb.AttachSessionResponse) bool) ([]*pb.AttachSessionResponse, error) {
	stream, err := h.Client.AttachSession(ctx, connect.NewRequest(&pb.AttachSessionRequest{Id: sessionID}))
	if err != nil {
		return nil, fmt.Errorf("attach session: %w", err)
	}
	defer stream.Close() //nolint:errcheck // test cleanup

	var msgs []*pb.AttachSessionResponse
	for stream.Receive() {
		msg := stream.Msg()
		msgs = append(msgs, msg)
		if stop != nil && stop(msg) {
			return msgs, nil
		}
	}
	if err := stream.Err(); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return msgs, fmt.Errorf("attach session stream: %w", err)
	}
	return msgs, nil
}

// SetArchivedAt backdates a session's archived_at timestamp for use in
// age-filter tests (e.g., EmptyTrash with older_than). This is a test-only
// helper that reaches past the normal API surface — production code must
// never do this.
func (h *Harness) SetArchivedAt(t *testing.T, sessionID string, at time.Time) {
	t.Helper()
	// Format matches sqlutil.TimeNow so the stored value is parseable by the
	// session store's scan path.
	res, err := h.DB.ExecContext(context.Background(),
		`UPDATE sessions SET archived_at = ? WHERE id = ?`,
		at.UTC().Format("2006-01-02T15:04:05.000Z"), sessionID,
	)
	if err != nil {
		t.Fatalf("set archived_at: %v", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("rows affected: %v", err)
	}
	if n != 1 {
		t.Fatalf("set archived_at: expected 1 row updated, got %d", n)
	}
}

// SeedSessionInState creates a session under repoID and advances it to the
// requested state. Returns the session ID (and the created PR number when
// one was opened along the way, or 0 otherwise). It t.Fatals on any step
// failure so tests can treat the returned ID as valid.
//
// Supported states:
//   - SESSION_STATE_IMPLEMENTING_PLAN — default landing state after create.
//   - SESSION_STATE_AWAITING_CHECKS   — submits a draft PR.
//   - SESSION_STATE_READY_FOR_REVIEW  — submits a PR, then fires ChecksPassed
//     via the dispatcher; the caller must have set CanAutoMerge=true on the
//     repo before calling (otherwise the machine lands in GreenDraft).
//   - SESSION_STATE_BLOCKED           — submits a PR, pre-seeds AttemptCount
//     to MaxAttempts-1, then fires ChecksFailed; the state machine lands in
//     Blocked rather than looping FixingChecks.
//   - SESSION_STATE_CLOSED            — creates then closes via CloseSession.
func (h *Harness) SeedSessionInState(t *testing.T, ctx context.Context, repoID string, state pb.SessionState, title, plan string) (sessionID string, prNumber int) {
	t.Helper()

	stream, err := h.Client.CreateSession(ctx, connect.NewRequest(&pb.CreateSessionRequest{
		RepoId: repoID, Title: title, Plan: plan,
	}))
	if err != nil {
		t.Fatalf("SeedSessionInState: create session: %v", err)
	}
	defer stream.Close() //nolint:errcheck // test cleanup
	var created *pb.Session
	for stream.Receive() {
		if sc := stream.Msg().GetSessionCreated(); sc != nil {
			created = sc.GetSession()
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("SeedSessionInState: create stream: %v", err)
	}
	if created == nil {
		t.Fatal("SeedSessionInState: no SessionCreated in stream")
	}
	sessionID = created.Id

	switch state {
	case pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN:
		return sessionID, 0

	case pb.SessionState_SESSION_STATE_AWAITING_CHECKS:
		if err := h.Lifecycle.SubmitPR(ctx, sessionID); err != nil {
			t.Fatalf("SeedSessionInState: submit PR: %v", err)
		}
		prNumber = getPRNumber(t, h, ctx, sessionID)
		return sessionID, prNumber

	case pb.SessionState_SESSION_STATE_READY_FOR_REVIEW:
		if err := h.Lifecycle.SubmitPR(ctx, sessionID); err != nil {
			t.Fatalf("SeedSessionInState: submit PR: %v", err)
		}
		prNumber = getPRNumber(t, h, ctx, sessionID)
		dispatcher := session.NewDispatcher(h.Sessions, h.Repos, h.VCS, nil, zerolog.Nop())
		events := make(chan session.SessionEvent, 1)
		events <- session.SessionEvent{SessionID: sessionID, Event: vcs.ChecksPassed{PRID: prNumber}}
		close(events)
		dispCtx, dispCancel := context.WithTimeout(ctx, 5*time.Second)
		defer dispCancel()
		dispatcher.Run(dispCtx, events)
		return sessionID, prNumber

	case pb.SessionState_SESSION_STATE_BLOCKED:
		if err := h.Lifecycle.SubmitPR(ctx, sessionID); err != nil {
			t.Fatalf("SeedSessionInState: submit PR: %v", err)
		}
		prNumber = getPRNumber(t, h, ctx, sessionID)
		// Pre-seed attempt count so the next failure lands in Blocked
		// rather than looping through another FixingChecks attempt.
		attempts := machine.MaxAttempts - 1
		if _, err := h.Sessions.Update(ctx, sessionID, db.UpdateSessionParams{AttemptCount: &attempts}); err != nil {
			t.Fatalf("SeedSessionInState: seed attempt count: %v", err)
		}
		failureConclusion := vcs.CheckConclusionFailure
		dispatcher := session.NewDispatcher(h.Sessions, h.Repos, h.VCS, nil, zerolog.Nop())
		events := make(chan session.SessionEvent, 1)
		events <- session.SessionEvent{
			SessionID: sessionID,
			Event: vcs.ChecksFailed{
				PRID:         prNumber,
				FailedChecks: []vcs.CheckResult{{ID: "check-1", Name: "lint", Status: vcs.CheckStatusCompleted, Conclusion: &failureConclusion}},
			},
		}
		close(events)
		dispCtx, dispCancel := context.WithTimeout(ctx, 5*time.Second)
		defer dispCancel()
		dispatcher.Run(dispCtx, events)
		return sessionID, prNumber

	case pb.SessionState_SESSION_STATE_CLOSED:
		if _, err := h.Client.CloseSession(ctx, connect.NewRequest(&pb.CloseSessionRequest{Id: sessionID})); err != nil {
			t.Fatalf("SeedSessionInState: close: %v", err)
		}
		return sessionID, 0

	default:
		t.Fatalf("SeedSessionInState: unsupported target state %v", state)
		return "", 0
	}
}

func getPRNumber(t *testing.T, h *Harness, ctx context.Context, sessionID string) int {
	t.Helper()
	resp, err := h.Client.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{Id: sessionID}))
	if err != nil {
		t.Fatalf("SeedSessionInState: get session: %v", err)
	}
	if resp.Msg.Session.PrNumber == nil {
		t.Fatalf("SeedSessionInState: expected PR number on session %s", sessionID)
	}
	return int(*resp.Msg.Session.PrNumber)
}

// migrationsDir returns the absolute path to the bossd migrations directory.
func migrationsDir() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..", "migrations")
}

// unavailableTmuxFactory is the default command factory used by the harness
// when no tmux behavior is requested. It returns a command that always exits
// non-zero so tmux.Client.Available reports false and ensureChatTmuxSession
// short-circuits — preserving the prior "no tmux side effects" behavior of
// every existing test that doesn't care about tmux.
func unavailableTmuxFactory(ctx context.Context, _ string, _ ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "false")
}
