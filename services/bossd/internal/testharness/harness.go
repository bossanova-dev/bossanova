// Package testharness provides an E2E test harness for the bossd daemon.
// It wires together an in-memory SQLite database, mock git/claude/VCS
// implementations, and a real ConnectRPC server on a Unix socket.
package testharness

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	urlpkg "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/migrate"
	"github.com/recurser/bossalib/safego"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	pluginpkg "github.com/recurser/bossd/internal/plugin"
	"github.com/recurser/bossd/internal/server"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/status"
	"github.com/recurser/bossd/internal/tmux"
	"github.com/recurser/bossd/internal/upstream"
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
	// WorkflowServices receive display status notifications from the same
	// DisplayTracker change point that production wires to the plugin host.
	WorkflowServices []pluginpkg.WorkflowService
}

// Harness provides a fully wired bossd daemon for E2E tests.
type Harness struct {
	DB         *sql.DB
	Repos      db.RepoStore
	Sessions   db.SessionStore
	Attempts   db.AttemptStore
	AgentChats db.AgentChatStore
	CronJobs   db.CronJobStore
	Lifecycle  *session.Lifecycle
	Server     *server.Server
	Provider   *StubProvider
	Dispatcher *upstream.WebhookDispatcher
	Tmux       *tmux.Client
	Git        *MockWorktreeManager
	Agent      *MockAgentRunner
	VCS        *MockVCSProvider
	// DisplayTracker backs the MergeSession "PR is not passing" guard. Leave
	// entries empty to let merges through (the guard skips when no entry
	// exists); call DisplayTracker.Set with a non-passing status to block.
	DisplayTracker *status.DisplayTracker

	// Client is a ConnectRPC client connected to the test server.
	Client bossanovav1connect.DaemonServiceClient

	// HookServer is the loopback hook server wired to the harness's Lifecycle.
	// It is always started by newHarness; tests use PostStopHook to POST to it.
	HookServer *server.HookServer

	// HostService is the same *HostServiceServer the HookServer routes
	// agent-run-complete dispatches into. Exposed so integration tests
	// can register runs (Task 4) without going through the plugin broker.
	HostService *pluginpkg.HostServiceServer

	socketPath string
	httpServer *http.Server
	listener   net.Listener
	ctx        context.Context
	cancel     context.CancelFunc
	closed     atomic.Bool

	deletedMu       sync.Mutex
	deletedSessions []string
	updatedMu       sync.Mutex
	updatedSessions []*pb.Session
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
	agentChats := db.NewAgentChatStore(database)
	cronJobs := db.NewCronJobStore(database)

	// Mocks.
	logger := zerolog.Nop()
	gitMock := NewMockWorktreeManager()
	agentMock := NewMockAgentRunner()
	vcsMock := NewMockVCSProvider()

	// Tmux client. Built BEFORE the Lifecycle so we can pass it in as a
	// dependency for cron-spawned sessions (startCronTmuxChat needs
	// Available()) as well as RecordChat. Tests that need these paths to
	// drive tmux pass a custom command factory; everyone else gets a client
	// whose Available() returns false, which short-circuits both paths and
	// preserves the prior "no tmux side effects" behaviour for legacy tests.
	var tmuxClient *tmux.Client
	if opts.TmuxCommandFactory != nil {
		tmuxClient = tmux.NewClient(tmux.WithCommandFactory(opts.TmuxCommandFactory))
	} else {
		tmuxClient = tmux.NewClient(tmux.WithCommandFactory(unavailableTmuxFactory))
	}

	// Lifecycle. Wired with tmuxClient so cron-spawned sessions route
	// through startCronTmuxChat instead of the headless claude.Start path.
	lifecycle := session.NewLifecycle(sessions, repos, agentChats, cronJobs, gitMock, agentMock, tmuxClient, vcsMock, logger)

	// PR display tracker. Wired through to the server so MergeSession's
	// "PR is not passing" guard is reachable from tests — entries default
	// to empty, so merges fall through unless a test explicitly calls
	// DisplayTracker.Set with a non-passing status.
	display := status.NewDisplayTracker()
	if len(opts.WorkflowServices) > 0 {
		workflowServices := append([]pluginpkg.WorkflowService(nil), opts.WorkflowServices...)
		display.SetOnChange(func(sessionID string, _ *status.DisplayEntry, newEntry *status.DisplayEntry) {
			for _, svc := range workflowServices {
				svc := svc
				safego.Go(logger, func() {
					cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if err := svc.NotifyStatusChange(cctx, sessionID, pb.DisplayStatus(newEntry.Status), newEntry.HasFailures); err != nil {
						logger.Warn().Err(err).Str("session_id", sessionID).Msg("failed to notify workflow service of status change")
					}
				})
			}
		})
	}

	// Realtime webhook harness. This mirrors bossd's poller + dispatcher +
	// webhook fan-in path in-process, with a separate deterministic VCS
	// provider so webhook E2E tests can seed PR/check state without mutating
	// legacy lifecycle mocks.
	realtimeProvider := NewStubProvider()
	realtimeCtx, realtimeCancel := context.WithCancel(context.Background())
	poller := session.NewPoller(sessions, repos, realtimeProvider, time.Hour, session.DefaultPollTimeout, logger)
	realtimeDispatcher := session.NewDispatcher(sessions, repos, realtimeProvider, nil, logger)
	pollerEvents := poller.Run(realtimeCtx)
	select {
	case <-poller.InitialPollDone():
	case <-time.After(time.Second):
		t.Fatal("wait for realtime poller startup poll")
	}
	webhookEventCh := make(chan session.SessionEvent, 64)
	merged := mergeSessionEvents(realtimeCtx, pollerEvents, webhookEventCh)
	emitter := session.NewSessionEventEmitter(&harnessLookup{sessions: sessions, repos: repos}, webhookEventCh, logger)
	displayPoller := session.NewDisplayPoller(sessions, repos, realtimeProvider, display, time.Hour, logger)
	webhookDispatcher := upstream.NewWebhookDispatcherWithEmitter(displayPoller, emitter, logger)
	_ = safego.Go(logger, func() { realtimeDispatcher.Run(realtimeCtx, merged) })

	// Server.
	h := &Harness{}
	mockAgentClient := &MockAgentClient{Name: "claude"}
	mockCodexClient := &MockAgentClient{Name: "codex"}
	srv := server.New(server.Config{
		Repos:          repos,
		Sessions:       sessions,
		Attempts:       attempts,
		AgentChats:     agentChats,
		DisplayTracker: display,
		Lifecycle:      lifecycle,
		Agent:          agentMock,
		// Register both agents so per-agent routing tests (chat.AgentName
		// = "codex") can be exercised end-to-end via RecordChat. The
		// default for legacy chats with empty AgentName routes to claude
		// (see liveArgvBuilder's "" → "claude" fallback).
		AgentClients: map[string]agent.AgentRunnerClient{
			"claude": mockAgentClient,
			"codex":  mockCodexClient,
		},
		Worktrees: gitMock,
		Provider:  vcsMock,
		Tmux:      tmuxClient,
		OnSessionDeleted: func(_ context.Context, sessionID string) {
			h.deletedMu.Lock()
			defer h.deletedMu.Unlock()
			h.deletedSessions = append(h.deletedSessions, sessionID)
		},
		OnSessionUpdated: func(_ context.Context, sess *pb.Session) {
			h.updatedMu.Lock()
			defer h.updatedMu.Unlock()
			h.updatedSessions = append(h.updatedSessions, sess)
		},
		Logger: logger,
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

	// Host service — same instance the plugin broker would dispense in
	// production, wired here so harness-driven tests can register
	// agent-run completion state and exercise the
	// /hooks/agent-run-complete/{id} endpoint without spinning up real
	// plugins. The DisplayTracker is shared with Server so a
	// CompleteAgentRun call here clears the same IsRepairing flag the
	// rest of the harness sees.
	hostService := pluginpkg.NewHostServiceServer(vcsMock)
	hostService.SetSessionDeps(repos, sessions, agentChats, display, status.NewTracker())
	// Wire the lifecycle so tests that exercise StartChatRun directly
	// (Task 4) hit the same plumbing the daemon installs in cmd/main.go.
	hostService.SetLifecycle(lifecycle)

	// Hook server — loopback HTTP server that receives Claude Stop-hook POSTs
	// and dispatches Lifecycle.FinalizeSession. The bound port is plumbed
	// directly into the lifecycle (same as the daemon entrypoint) so tests
	// don't need a port file on disk.
	hookSrv := server.NewHookServer(server.HookServerConfig{
		Sessions:  sessions,
		Finalizer: lifecycle,
		Completer: hostService,
		Logger:    logger,
	})
	if err := hookSrv.Listen(); err != nil {
		t.Fatalf("hook server listen: %v", err)
	}
	lifecycle.SetHookPort(hookSrv.Port())
	lifecycle.SetAgents(map[string]agent.AgentRunnerClient{
		"claude": mockAgentClient,
		"codex":  mockCodexClient,
	})
	// StartTmuxChat (extracted from startCronTmuxChat) requires a non-empty
	// agentLogsDir so it can resolve a per-agent-session log path to feed
	// into BuildInteractiveCommand. Use t.TempDir so each harness run has
	// an isolated directory and the file system is cleaned up automatically.
	lifecycle.SetAgentLogsDir(t.TempDir())
	go func() { _ = hookSrv.Serve() }()

	h.DB = database
	h.Repos = repos
	h.Sessions = sessions
	h.Attempts = attempts
	h.AgentChats = agentChats
	h.CronJobs = cronJobs
	h.Lifecycle = lifecycle
	h.Server = srv
	h.Provider = realtimeProvider
	h.Dispatcher = webhookDispatcher
	h.Tmux = tmuxClient
	h.Git = gitMock
	h.Agent = agentMock
	h.VCS = vcsMock
	h.DisplayTracker = display
	h.Client = client
	h.HookServer = hookSrv
	h.HostService = hostService
	h.socketPath = socketPath
	h.httpServer = httpServer
	h.listener = ln
	h.ctx = realtimeCtx
	h.cancel = realtimeCancel

	// Single cleanup hook ensures Close runs at test teardown even when
	// the test forgets to call it explicitly. Close is idempotent.
	t.Cleanup(func() { h.Close() })

	return h
}

// DeletedSessionIDs returns a snapshot of session IDs the daemon has
// reported deleted via the Server's OnSessionDeleted hook. The slice is
// returned in invocation order; tests assert on it to verify that
// SessionDelta_KIND_DELETED would be published for each removed session.
func (h *Harness) DeletedSessionIDs() []string {
	h.deletedMu.Lock()
	defer h.deletedMu.Unlock()
	out := make([]string, len(h.deletedSessions))
	copy(out, h.deletedSessions)
	return out
}

// UpdatedSessions returns a snapshot of sessions the daemon has reported
// updated via the Server's OnSessionUpdated hook. Tests assert on it to verify
// that SessionDelta_KIND_UPDATED would be published for archive/resurrect
// transitions.
func (h *Harness) UpdatedSessions() []*pb.Session {
	h.updatedMu.Lock()
	defer h.updatedMu.Unlock()
	out := make([]*pb.Session, len(h.updatedSessions))
	copy(out, h.updatedSessions)
	return out
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
	if h.cancel != nil {
		h.cancel()
	}
	if h.HookServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = h.HookServer.Shutdown(ctx)
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

// Ctx returns the harness daemon context used by the in-process realtime path.
func (h *Harness) Ctx() context.Context {
	return h.ctx
}

// SeedRepo creates a repository row and returns its ID.
func (h *Harness) SeedRepo(t *testing.T, originURL string) string {
	t.Helper()
	repo, err := h.Repos.Create(context.Background(), db.CreateRepoParams{
		DisplayName:       "test-repo",
		LocalPath:         t.TempDir(),
		OriginURL:         originURL,
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("SeedRepo: create repo: %v", err)
	}
	return repo.ID
}

// SeedSession creates a session row with a PR number and state, returning its ID.
func (h *Harness) SeedSession(t *testing.T, repoID string, prNumber int, initialState pb.SessionState) string {
	t.Helper()
	prURL := fmt.Sprintf("https://github.com/test/repo/pull/%d", prNumber)
	sess, err := h.Sessions.Create(context.Background(), db.CreateSessionParams{
		RepoID:       repoID,
		Title:        "seed-session",
		Plan:         "seed",
		WorktreePath: t.TempDir(),
		BranchName:   fmt.Sprintf("seed-pr-%d", prNumber),
		BaseBranch:   "main",
		PRNumber:     &prNumber,
		PRURL:        &prURL,
	})
	if err != nil {
		t.Fatalf("SeedSession: create session: %v", err)
	}
	state := int(initialState)
	if _, err := h.Sessions.Update(context.Background(), sess.ID, db.UpdateSessionParams{State: &state}); err != nil {
		t.Fatalf("SeedSession: update state: %v", err)
	}
	return sess.ID
}

// PostGitHubWebhook dispatches a GitHub webhook event through the harness's
// in-process upstream dispatcher.
func (h *Harness) PostGitHubWebhook(t *testing.T, eventType string, payload []byte, prScope int, repoOrigin string) {
	t.Helper()
	if err := h.Dispatcher.Dispatch(h.ctx, &pb.WebhookEvent{
		EventType:     eventType,
		Payload:       payload,
		Provider:      "github",
		RepoOriginUrl: repoOrigin,
		PullRequest:   int32(prScope),
	}); err != nil {
		t.Fatalf("PostGitHubWebhook: dispatch: %v", err)
	}
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
	} else {
		sessionID = created.Id
	}

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

// SeedCronSession creates a bare session row (no worktree, no branch) with the
// given hook token set. This is the cheapest way to give the hook server a
// session to authenticate against in smoke tests for PostStopHook.
func (h *Harness) SeedCronSession(t *testing.T, repoID, hookToken string) string {
	t.Helper()
	ctx := context.Background()
	sess, err := h.Sessions.Create(ctx, db.CreateSessionParams{
		RepoID: repoID,
		Title:  "cron-seed",
		Plan:   "seed",
	})
	if err != nil {
		t.Fatalf("SeedCronSession create: %v", err)
	}
	tok := hookToken
	tokPtr := &tok
	if _, err := h.Sessions.Update(ctx, sess.ID, db.UpdateSessionParams{HookToken: &tokPtr}); err != nil {
		t.Fatalf("SeedCronSession set token: %v", err)
	}
	return sess.ID
}

// SetVCSMode configures the test harness for one of the named VCS failure
// modes. It sets the VCS provider mode (for NoGitHub and CreatePRFail) and
// wires up the git mock for VCSModePushFail.
func (h *Harness) SetVCSMode(mode MockVCSMode) {
	h.VCS.SetMode(mode)
	if mode == VCSModePushFail {
		h.Git.SetPushError(fmt.Errorf("mock: push failed (VCSModePushFail)"))
	} else {
		h.Git.SetPushError(nil)
	}
}

// PostStopHook POSTs to the harness's loopback hook server at
// /hooks/finalize/<sessionID> with an Authorization: Bearer <token> header.
// It uses the bound port on the in-process HookServer directly — no port file
// I/O needed. The caller is responsible for closing the response body.
func (h *Harness) PostStopHook(sessionID, token string) (*http.Response, error) {
	port := h.HookServer.Port()
	url := fmt.Sprintf("http://127.0.0.1:%d/hooks/finalize/%s", port, urlpkg.PathEscape(sessionID))
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(nil))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 5 * time.Second}
	return client.Do(req)
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

type harnessLookup struct {
	sessions db.SessionStore
	repos    db.RepoStore
}

type repoPRSessionLister interface {
	ListByRepoAndPR(ctx context.Context, repoID string, prNumber int) ([]*db.SessionWithRepo, error)
}

func (l *harnessLookup) SessionsForPR(ctx context.Context, repoOriginURL string, prNumber int) ([]session.SessionForPR, error) {
	repo, err := l.repos.GetByOrigin(ctx, repoOriginURL)
	if err != nil {
		return nil, err
	}
	lister, ok := l.sessions.(repoPRSessionLister)
	if !ok {
		return nil, fmt.Errorf("session store does not support ListByRepoAndPR")
	}
	rows, err := lister.ListByRepoAndPR(ctx, repo.ID, prNumber)
	if err != nil {
		return nil, err
	}
	out := make([]session.SessionForPR, 0, len(rows))
	for _, row := range rows {
		out = append(out, session.SessionForPR{ID: row.ID})
	}
	return out, nil
}

func mergeSessionEvents(ctx context.Context, a, b <-chan session.SessionEvent) <-chan session.SessionEvent {
	out := make(chan session.SessionEvent, 64)
	safego.Go(zerolog.Nop(), func() {
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
	})
	return out
}
