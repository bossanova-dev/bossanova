package server

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/migrate"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/socketauth"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	gitpkg "github.com/recurser/bossd/internal/git"
	"github.com/recurser/bossd/internal/proofenv"
	"github.com/recurser/bossd/internal/session"
	"github.com/rs/zerolog"
)

var setupServerTestDBMigrateMu sync.Mutex

// streamTestToken is a fixed valid 64-char hex socket-auth token used to wire
// the in-process httptest server and its client in this file's harness.
const streamTestToken = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

func TestServerListenSetsWriteTimeout(t *testing.T) {
	t.Parallel()

	socketPath := filepath.Join("/tmp", fmt.Sprintf("bossd-%d-%d.sock", os.Getpid(), time.Now().UnixNano()))
	s := New(Config{})

	if err := s.Listen(socketPath); err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	t.Cleanup(func() {
		_ = s.listener.Close()
		_ = os.Remove(socketPath)
	})

	if got := s.srv.WriteTimeout; got != 120*time.Second {
		t.Fatalf("WriteTimeout = %v, want %v", got, 120*time.Second)
	}
}

func TestCreateSessionHandlerClearsWriteDeadline(t *testing.T) {
	t.Parallel()

	rw := &deadlineCaptureResponseWriter{header: http.Header{}}
	handlerCalled := false
	handler := withCreateSessionWriteDeadlineOverride(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		handlerCalled = true
	}))

	req := httptest.NewRequest(http.MethodPost, bossanovav1connect.DaemonServiceCreateSessionProcedure, nil)
	handler.ServeHTTP(rw, req)

	if !handlerCalled {
		t.Fatal("wrapped handler was not called")
	}
	if !rw.writeDeadlineSet {
		t.Fatal("write deadline was not cleared")
	}
	if !rw.writeDeadline.IsZero() {
		t.Fatalf("write deadline = %v, want zero", rw.writeDeadline)
	}
}

type deadlineCaptureResponseWriter struct {
	header           http.Header
	writeDeadline    time.Time
	writeDeadlineSet bool
}

func (w *deadlineCaptureResponseWriter) Header() http.Header { return w.header }
func (w *deadlineCaptureResponseWriter) Write(b []byte) (int, error) {
	return len(b), nil
}
func (w *deadlineCaptureResponseWriter) WriteHeader(int) {}
func (w *deadlineCaptureResponseWriter) SetWriteDeadline(deadline time.Time) error {
	w.writeDeadline = deadline
	w.writeDeadlineSet = true
	return nil
}

func TestCreateSessionStreamsSetupOutputBeforeSessionCreated(t *testing.T) {
	t.Parallel()

	h := newCreateSessionStreamHarness(t, &setupStreamWorktree{
		output: []string{"first line\n", "second line\n"},
	}, &setupStreamAgent{})

	events, err := h.createSession(t, "normal setup")
	if err != nil {
		t.Fatalf("CreateSession stream error = %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("events len = %d, want 3", len(events))
	}
	if got := events[0].GetSetupOutput().Text; got != "first line" {
		t.Fatalf("event[0] setup output = %q, want %q", got, "first line")
	}
	if got := events[1].GetSetupOutput().Text; got != "second line" {
		t.Fatalf("event[1] setup output = %q, want %q", got, "second line")
	}
	if events[2].GetSessionCreated() == nil {
		t.Fatalf("event[2] = %T, want SessionCreated", events[2].GetEvent())
	}
	if events[2].GetSessionCreated().GetAttachedExisting() {
		t.Fatal("genuine create AttachedExisting = true, want false")
	}
}

// TestCreateSessionAttachExistingSignalsAttachedExisting pins BOS-243: when a
// create request resolves to a head branch already owned by an active session,
// the daemon dedups/attaches to that session and MUST flag the emitted
// SessionCreated with attached_existing=true (same session id, no new session,
// the request's plan is NOT run) so callers can tell attach from a fresh create.
func TestCreateSessionAttachExistingSignalsAttachedExisting(t *testing.T) {
	t.Parallel()

	h := newCreateSessionStreamHarness(t, &setupStreamWorktree{}, &setupStreamAgent{})

	// Seed one active session that already owns branch "feat-x" with a present
	// worktree directory, so the create request attaches to it.
	worktree := t.TempDir()
	branch := "feat-x"
	existing, err := h.sessions.Create(context.Background(), db.CreateSessionParams{
		RepoID:       h.repo.ID,
		Title:        "existing",
		Plan:         "original plan",
		BaseBranch:   "main",
		BranchName:   branch,
		WorktreePath: worktree,
		AgentName:    "claude",
	})
	if err != nil {
		t.Fatalf("seed existing session: %v", err)
	}

	var got []*pb.CreateSessionResponse
	emit := func(r *pb.CreateSessionResponse) error {
		got = append(got, r)
		return nil
	}
	if err := h.server.StreamCreateSession(context.Background(), &pb.CreateSessionRequest{
		RepoId:     h.repo.ID,
		Title:      "repair",
		BranchName: &branch,
		Plan:       "/boss-repair watch 1035",
	}, emit); err != nil {
		t.Fatalf("StreamCreateSession: %v", err)
	}

	var created []*pb.SessionCreated
	for _, r := range got {
		if sc := r.GetSessionCreated(); sc != nil {
			created = append(created, sc)
		}
	}
	if len(created) != 1 {
		t.Fatalf("SessionCreated frames = %d, want exactly 1", len(created))
	}
	if !created[0].GetAttachedExisting() {
		t.Fatal("AttachedExisting = false, want true on the attach path")
	}
	if gotID := created[0].GetSession().GetId(); gotID != existing.ID {
		t.Fatalf("attached session id = %q, want existing %q", gotID, existing.ID)
	}

	// The prompt is NOT run on attach: no new session is created.
	sessions, listErr := h.sessions.List(context.Background(), h.repo.ID)
	if listErr != nil {
		t.Fatalf("list sessions: %v", listErr)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions len = %d, want 1 (attach must not create a new session)", len(sessions))
	}
}

// TestCreateSessionPersistsModelFromRequest pins the BOS-179 model wiring: the
// CreateSession handler copies CreateSessionRequest.model into the persisted
// session, so a session created with model=<opus id> runs the headless agent
// with that model.
func TestCreateSessionPersistsModelFromRequest(t *testing.T) {
	t.Parallel()

	h := newCreateSessionStreamHarness(t, &setupStreamWorktree{}, &setupStreamAgent{})

	prNumber := int32(321)
	agentName := "claude"
	model := "claude-opus-4-8"
	stream, err := h.client.CreateSession(context.Background(), connect.NewRequest(&pb.CreateSessionRequest{
		RepoId:    h.repo.ID,
		Title:     "model wiring",
		Plan:      "do work",
		PrNumber:  &prNumber,
		AgentName: &agentName,
		Detach:    true,
		Model:     &model,
	}))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	for stream.Receive() {
		_ = stream.Msg()
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("CreateSession stream error: %v", err)
	}

	sessions, listErr := h.sessions.List(context.Background(), h.repo.ID)
	if listErr != nil {
		t.Fatalf("list sessions: %v", listErr)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions len = %d, want 1", len(sessions))
	}
	if sessions[0].Model != model {
		t.Fatalf("persisted session model = %q, want %q", sessions[0].Model, model)
	}
}

func TestCreateSessionDuplicateActivePRReturnsAlreadyExists(t *testing.T) {
	t.Parallel()

	h := newCreateSessionStreamHarness(t, &setupStreamWorktree{}, &setupStreamAgent{})
	prNumber := 123
	otherBranch := "dependabot/npm/other-1.0.0"
	if _, err := h.sessions.Create(context.Background(), db.CreateSessionParams{
		RepoID:     h.repo.ID,
		Title:      "first repair",
		Plan:       "do work",
		BaseBranch: "main",
		BranchName: otherBranch,
		PRNumber:   &prNumber,
		AgentName:  "claude",
	}); err != nil {
		t.Fatalf("create existing session: %v", err)
	}

	_, err := h.createSession(t, "second repair")
	if err == nil {
		t.Fatal("second CreateSession error = nil, want AlreadyExists")
	}
	if got := connect.CodeOf(err); got != connect.CodeAlreadyExists {
		t.Fatalf("Connect code = %v, want %v; err = %v", got, connect.CodeAlreadyExists, err)
	}
	if want := fmt.Sprintf("active session already exists for repo %s PR #123", h.repo.ID); !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want to contain %q", err.Error(), want)
	}
	sessions, listErr := h.sessions.List(context.Background(), h.repo.ID)
	if listErr != nil {
		t.Fatalf("list sessions: %v", listErr)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions len = %d, want 1", len(sessions))
	}
}

func TestCreateSessionStreamsLongSetupOutputLine(t *testing.T) {
	t.Parallel()

	longLine := strings.Repeat("x", 70*1024)
	h := newCreateSessionStreamHarness(t, &setupStreamWorktree{
		output: []string{longLine + "\n"},
	}, &setupStreamAgent{})

	events, err := h.createSession(t, "long setup output")
	if err != nil {
		t.Fatalf("CreateSession stream error = %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events len = %d, want 2", len(events))
	}
	if got := events[0].GetSetupOutput().Text; got != longLine {
		t.Fatalf("long setup output length = %d, want %d", len(got), len(longLine))
	}
	if events[1].GetSessionCreated() == nil {
		t.Fatalf("event[1] = %T, want SessionCreated", events[1].GetEvent())
	}
}

func TestStreamSetupOutputRejectsOversizedLine(t *testing.T) {
	t.Parallel()

	sender := &setupOutputCaptureSender{}
	err := streamSetupOutput(strings.NewReader(strings.Repeat("x", maxSetupOutputLineBytes+1)), sender)
	if err == nil {
		t.Fatal("streamSetupOutput error = nil, want error")
	}
	if got := connect.CodeOf(err); got != connect.CodeResourceExhausted {
		t.Fatalf("Connect code = %v, want %v; err = %v", got, connect.CodeResourceExhausted, err)
	}
	if len(sender.events) != 0 {
		t.Fatalf("events len = %d, want 0", len(sender.events))
	}
}

func TestCreateSessionCleansUpWhenSetupOutputStreamFailsAfterStartSucceeds(t *testing.T) {
	t.Parallel()

	h := newCreateSessionStreamHarness(t, &setupStreamWorktree{
		output: []string{strings.Repeat("x", maxSetupOutputLineBytes+1)},
	}, &setupStreamAgent{})

	_, err := h.createSession(t, "oversized setup output")
	if err == nil {
		t.Fatal("CreateSession stream error = nil, want error")
	}
	if got := connect.CodeOf(err); got != connect.CodeResourceExhausted {
		t.Fatalf("Connect code = %v, want %v; err = %v", got, connect.CodeResourceExhausted, err)
	}
	sessions, listErr := h.sessions.List(context.Background(), h.repo.ID)
	if listErr != nil {
		t.Fatalf("list sessions: %v", listErr)
	}
	if len(sessions) != 0 {
		t.Fatalf("sessions len = %d, want 0", len(sessions))
	}
}

func TestCreateSessionStartErrorAfterSetupOutputReturnsConnectError(t *testing.T) {
	t.Parallel()

	h := newCreateSessionStreamHarness(t, &setupStreamWorktree{
		output: []string{"setup before failure\n"},
	}, &setupStreamAgent{startErr: errors.New("agent failed")})

	events, err := h.createSession(t, "agent failure")
	if err == nil {
		t.Fatal("CreateSession stream error = nil, want error")
	}
	if got := connect.CodeOf(err); got != connect.CodeInternal {
		t.Fatalf("Connect code = %v, want %v; err = %v", got, connect.CodeInternal, err)
	}
	if strings.Contains(err.Error(), "incomplete envelope") {
		t.Fatalf("error contains protocol framing failure: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events len = %d, want 1", len(events))
	}
	if got := events[0].GetSetupOutput().Text; got != "setup before failure" {
		t.Fatalf("setup output = %q, want %q", got, "setup before failure")
	}
}

func TestCreateSessionConcurrentDuplicateWaitsForFailedInteractiveStartupCleanup(t *testing.T) {
	t.Parallel()

	firstStartEntered := make(chan struct{})
	releaseFirstStart := make(chan struct{})

	var (
		mu         sync.Mutex
		startCalls int
	)
	runner := &setupStreamAgent{
		startFn: func(context.Context, string, string, *string, string) (string, error) {
			mu.Lock()
			startCalls++
			call := startCalls
			mu.Unlock()

			if call == 1 {
				close(firstStartEntered)
				<-releaseFirstStart
				return "", errors.New("agent failed")
			}
			return "agent-session", nil
		},
	}
	h := newCreateSessionStreamHarness(t, &setupStreamWorktree{}, runner)

	firstErr := make(chan error, 1)
	go func() {
		_, err := h.createSession(t, "first repair")
		firstErr <- err
	}()

	select {
	case <-firstStartEntered:
	case <-time.After(time.Second):
		t.Fatal("first session did not enter Start")
	}

	secondErr := make(chan error, 1)
	go func() {
		_, err := h.createSession(t, "second repair")
		secondErr <- err
	}()

	select {
	case err := <-secondErr:
		t.Fatalf("second CreateSession returned before first startup cleanup: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseFirstStart)

	if err := <-firstErr; err == nil {
		t.Fatal("first CreateSession error = nil, want startup failure")
	}
	if err := <-secondErr; err != nil {
		t.Fatalf("second CreateSession error = %v, want nil", err)
	}
	sessions, err := h.sessions.List(context.Background(), h.repo.ID)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions len = %d, want 1", len(sessions))
	}
	if sessions[0].Title != "second repair" {
		t.Fatalf("remaining session title = %q, want second repair", sessions[0].Title)
	}
}

func TestCreateSessionQuickChatAllowsEmptyDefaultBaseBranch(t *testing.T) {
	t.Parallel()

	h := newCreateSessionStreamHarness(t, &setupStreamWorktree{}, &setupStreamAgent{})
	empty := ""
	repo, err := h.repos.Update(context.Background(), h.repo.ID, db.UpdateRepoParams{
		DefaultBaseBranch: &empty,
	})
	if err != nil {
		t.Fatalf("update repo: %v", err)
	}
	h.repo = repo

	events, err := h.createQuickChat(t, "quick chat")
	if err != nil {
		t.Fatalf("CreateSession quick chat error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events len = %d, want 1", len(events))
	}
	if events[0].GetSessionCreated() == nil {
		t.Fatalf("event[0] = %T, want SessionCreated", events[0].GetEvent())
	}
}

// TestCreateSessionEmptyWorktreeBaseDirReturnsInvalidArgument pins the BOS-286
// guard: a worktree session against a repo with no worktree base directory
// fails fast with InvalidArgument (naming the repo id) instead of failing
// obscurely deep in worktree setup, while a QuickChat — which runs directly in
// the repo base and never uses the worktree base — still succeeds.
func TestCreateSessionEmptyWorktreeBaseDirReturnsInvalidArgument(t *testing.T) {
	t.Parallel()

	h := newCreateSessionStreamHarness(t, &setupStreamWorktree{}, &setupStreamAgent{})
	empty := ""
	repo, err := h.repos.Update(context.Background(), h.repo.ID, db.UpdateRepoParams{
		WorktreeBaseDir: &empty,
	})
	if err != nil {
		t.Fatalf("update repo: %v", err)
	}
	h.repo = repo

	if _, err := h.createSession(t, "needs worktree base"); err == nil {
		t.Fatal("CreateSession error = nil, want InvalidArgument for empty worktree base dir")
	} else {
		if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
			t.Fatalf("CreateSession code = %v, want InvalidArgument", got)
		}
		if !strings.Contains(err.Error(), h.repo.ID) {
			t.Fatalf("CreateSession error = %q, want it to name repo id %q", err.Error(), h.repo.ID)
		}
	}

	// The guard is gated on !QuickChat: a QuickChat create still succeeds with
	// the empty worktree base.
	events, err := h.createQuickChat(t, "quick chat")
	if err != nil {
		t.Fatalf("CreateSession quick chat error = %v, want success despite empty worktree base", err)
	}
	if len(events) != 1 || events[0].GetSessionCreated() == nil {
		t.Fatalf("quick chat events = %v, want a single SessionCreated", events)
	}
}

// TestNewHookToken verifies the server's finalize-hook token minter (used for
// tmux_unattended sessions): a 64-char hex string, unique per call.
func TestNewHookToken(t *testing.T) {
	t.Parallel()

	a, err := newHookToken()
	if err != nil {
		t.Fatalf("newHookToken: %v", err)
	}
	if len(a) != 64 {
		t.Fatalf("token len = %d, want 64 hex chars", len(a))
	}
	if _, err := hex.DecodeString(a); err != nil {
		t.Fatalf("token %q is not valid hex: %v", a, err)
	}
	b, err := newHookToken()
	if err != nil {
		t.Fatalf("newHookToken: %v", err)
	}
	if a == b {
		t.Fatal("two minted tokens are identical; expected crypto/rand uniqueness")
	}
}

type createSessionStreamHarness struct {
	client   bossanovav1connect.DaemonServiceClient
	server   *Server
	repo     *models.Repo
	repos    db.RepoStore
	sessions db.SessionStore
}

func newCreateSessionStreamHarness(t *testing.T, worktrees *setupStreamWorktree, runner *setupStreamAgent) *createSessionStreamHarness {
	t.Helper()
	return newCreateSessionStreamHarnessWithProvider(t, worktrees, runner, setupStreamProvider{}, zerolog.Nop())
}

// newCreateSessionStreamHarnessWithProvider builds a harness with an explicit
// vcs.Provider and logger, so guard tests can inject a search-configurable
// provider (BOS-289) and capture the fail-open warning line.
func newCreateSessionStreamHarnessWithProvider(t *testing.T, worktrees *setupStreamWorktree, runner *setupStreamAgent, provider vcs.Provider, logger zerolog.Logger) *createSessionStreamHarness {
	t.Helper()

	sqlDB := setupServerTestDB(t)
	repos := db.NewRepoStore(sqlDB)
	sessions := db.NewSessionStore(sqlDB)
	setupScript := "echo setup"
	repo, err := repos.Create(context.Background(), db.CreateRepoParams{
		DisplayName:       "repo",
		LocalPath:         "/tmp/repo",
		OriginURL:         "https://github.com/org/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		SetupScript:       &setupScript,
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}

	lifecycle := session.NewLifecycle(sessions, repos, nil, nil, worktrees, runner, nil, provider, zerolog.Nop())
	// createSession uses Detach=true (headless StartByAgent), which resolves the
	// proof env overlay. Inject a keyring-free resolver so the test never opens
	// the real OS keyring (non-deterministic; leaks a dbus goroutine on Linux).
	lifecycle.SetProofEnvResolver(proofenv.NewNoop())
	s := New(Config{
		Repos:     repos,
		Sessions:  sessions,
		Worktrees: worktrees,
		Provider:  provider,
		Lifecycle: lifecycle,
		Logger:    logger,
	})

	mux := http.NewServeMux()
	path, handler := bossanovav1connect.NewDaemonServiceHandler(s, connect.WithInterceptors(socketauth.NewServerInterceptor(streamTestToken)))
	mux.Handle(path, handler)
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	return &createSessionStreamHarness{
		client: bossanovav1connect.NewDaemonServiceClient(
			httpServer.Client(), httpServer.URL,
			connect.WithInterceptors(socketauth.NewClientInterceptor(streamTestToken)),
		),
		server:   s,
		repo:     repo,
		repos:    repos,
		sessions: sessions,
	}
}

func (h *createSessionStreamHarness) createSession(t *testing.T, title string) ([]*pb.CreateSessionResponse, error) {
	t.Helper()

	prNumber := int32(123)
	agentName := "claude"
	// Detach=true exercises the headless StartByAgent path these tests probe
	// (agent-start success/failure and startup cleanup). Interactive sessions
	// (Detach=false) are idle until attach and never call the runner here.
	stream, err := h.client.CreateSession(context.Background(), connect.NewRequest(&pb.CreateSessionRequest{
		RepoId:    h.repo.ID,
		Title:     title,
		Plan:      "do work",
		PrNumber:  &prNumber,
		AgentName: &agentName,
		Detach:    true,
	}))
	if err != nil {
		return nil, err
	}

	var events []*pb.CreateSessionResponse
	for stream.Receive() {
		events = append(events, stream.Msg())
	}
	return events, stream.Err()
}

func (h *createSessionStreamHarness) createQuickChat(t *testing.T, title string) ([]*pb.CreateSessionResponse, error) {
	t.Helper()

	agentName := "claude"
	stream, err := h.client.CreateSession(context.Background(), connect.NewRequest(&pb.CreateSessionRequest{
		RepoId:    h.repo.ID,
		Title:     title,
		Plan:      "quick question",
		QuickChat: true,
		AgentName: &agentName,
	}))
	if err != nil {
		return nil, err
	}

	var events []*pb.CreateSessionResponse
	for stream.Receive() {
		events = append(events, stream.Msg())
	}
	return events, stream.Err()
}

func setupServerTestDB(t *testing.T) *sql.DB {
	t.Helper()

	sqlDB, err := db.OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	setupServerTestDBMigrateMu.Lock()
	defer setupServerTestDBMigrateMu.Unlock()
	if err := migrate.Run(sqlDB, os.DirFS(serverTestMigrationsDir())); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	return sqlDB
}

func serverTestMigrationsDir() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..", "migrations")
}

type setupOutputCaptureSender struct {
	events []*pb.CreateSessionResponse
}

func (s *setupOutputCaptureSender) Send(resp *pb.CreateSessionResponse) error {
	s.events = append(s.events, resp)
	return nil
}

type setupStreamWorktree struct {
	output    []string
	createErr error
}

func (w *setupStreamWorktree) Create(_ context.Context, opts gitpkg.CreateOpts) (*gitpkg.CreateResult, error) {
	for _, text := range w.output {
		if opts.SetupScriptOutput == nil {
			continue
		}
		if _, err := io.WriteString(opts.SetupScriptOutput, text); err != nil {
			return nil, err
		}
	}
	if w.createErr != nil {
		return nil, w.createErr
	}
	return &gitpkg.CreateResult{WorktreePath: "/tmp/worktrees/repo/branch", BranchName: "branch"}, nil
}

func (w *setupStreamWorktree) CreateFromExistingBranch(_ context.Context, opts gitpkg.CreateFromExistingBranchOpts) (*gitpkg.CreateResult, error) {
	for _, text := range w.output {
		if opts.SetupScriptOutput == nil {
			continue
		}
		if _, err := io.WriteString(opts.SetupScriptOutput, text); err != nil {
			return nil, err
		}
	}
	if w.createErr != nil {
		return nil, w.createErr
	}
	return &gitpkg.CreateResult{WorktreePath: "/tmp/worktrees/repo/branch", BranchName: opts.BranchName}, nil
}

func (w *setupStreamWorktree) Archive(context.Context, string) error { return nil }
func (w *setupStreamWorktree) Resurrect(context.Context, gitpkg.ResurrectOpts) error {
	return nil
}
func (w *setupStreamWorktree) EmptyTrash(context.Context, string, []string) error            { return nil }
func (w *setupStreamWorktree) PurgeWorktree(context.Context, string, string, string, string) {}
func (w *setupStreamWorktree) EmptyCommit(context.Context, string, string) error             { return nil }
func (w *setupStreamWorktree) VerifyCurrentBranch(context.Context, string, string) error     { return nil }
func (w *setupStreamWorktree) Push(context.Context, string, string) error                    { return nil }
func (w *setupStreamWorktree) PushWithLease(context.Context, string, string, string) (string, error) {
	return "pushed-head-sha", nil
}
func (w *setupStreamWorktree) InjectPRNumbers(context.Context, string, string, int, string) error {
	return nil
}
func (w *setupStreamWorktree) VerifyPushedBranchAheadOfBase(context.Context, string, string, string) (*gitpkg.BranchVerification, error) {
	return &gitpkg.BranchVerification{HeadSHA: "head", BaseSHA: "base", RemoteHeadSHA: "head", AheadCount: 1}, nil
}
func (w *setupStreamWorktree) Status(context.Context, string) (string, error) { return "", nil }
func (w *setupStreamWorktree) CommitSubjects(context.Context, string, string) ([]string, error) {
	return nil, nil
}

func (w *setupStreamWorktree) LatestCommitSubject(context.Context, string) (string, error) {
	return "", nil
}
func (w *setupStreamWorktree) BranchDebugSnapshot(context.Context, string, string, string) (*gitpkg.BranchDebugSnapshot, error) {
	return &gitpkg.BranchDebugSnapshot{}, nil
}
func (w *setupStreamWorktree) Clone(context.Context, string, string) error { return nil }
func (w *setupStreamWorktree) DetectOriginURL(context.Context, string) (string, error) {
	return "", nil
}
func (w *setupStreamWorktree) IsGitRepo(context.Context, string) bool { return true }
func (w *setupStreamWorktree) DetectDefaultBranch(context.Context, string) (string, error) {
	return "main", nil
}
func (w *setupStreamWorktree) SyncBaseBranch(context.Context, string, string) error { return nil }
func (w *setupStreamWorktree) RetryDeferredBaseSyncs(context.Context)               {}
func (w *setupStreamWorktree) IsAncestor(context.Context, string, string, string) (bool, error) {
	return true, nil
}
func (w *setupStreamWorktree) FetchBase(context.Context, string, string) error { return nil }
func (w *setupStreamWorktree) MergeLocalBranch(context.Context, string, string, string, string) error {
	return nil
}

type setupStreamAgent struct {
	startErr error
	startFn  func(context.Context, string, string, *string, string) (string, error)
}

func (a *setupStreamAgent) Start(ctx context.Context, workDir, plan string, resume *string, agentSessionID, _ string, _ map[string]string) (string, error) {
	if a.startFn != nil {
		return a.startFn(ctx, workDir, plan, resume, agentSessionID)
	}
	return "agent-session", a.startErr
}
func (a *setupStreamAgent) Stop(string) error                 { return nil }
func (a *setupStreamAgent) IsRunning(string) bool             { return false }
func (a *setupStreamAgent) ExitError(string) error            { return nil }
func (a *setupStreamAgent) History(string) []agent.OutputLine { return nil }
func (a *setupStreamAgent) Subscribe(context.Context, string) (<-chan agent.OutputLine, error) {
	ch := make(chan agent.OutputLine)
	close(ch)
	return ch, nil
}
func (a *setupStreamAgent) StartByAgent(ctx context.Context, _ string, workDir, plan string, resume *string, agentSessionID, _ string, _ map[string]string) (string, error) {
	if a.startFn != nil {
		return a.startFn(ctx, workDir, plan, resume, agentSessionID)
	}
	return "agent-session", a.startErr
}
func (a *setupStreamAgent) StopByAgent(string, string) error     { return nil }
func (a *setupStreamAgent) IsRunningByAgent(string, string) bool { return false }

type setupStreamProvider struct{}

func (setupStreamProvider) CreateDraftPR(context.Context, vcs.CreatePROpts) (*vcs.PRInfo, error) {
	return &vcs.PRInfo{}, nil
}
func (setupStreamProvider) GetPRStatus(context.Context, string, int) (*vcs.PRStatus, error) {
	return &vcs.PRStatus{HeadBranch: "dependabot/npm/pkg-1.0.0", BaseBranch: "main"}, nil
}
func (setupStreamProvider) GetCheckResults(context.Context, string, int) ([]vcs.CheckResult, error) {
	return nil, nil
}
func (setupStreamProvider) GetFailedCheckLogs(context.Context, string, string) (string, error) {
	return "", nil
}
func (setupStreamProvider) MarkReadyForReview(context.Context, string, int) error { return nil }
func (setupStreamProvider) GetReviewComments(context.Context, string, int) ([]vcs.ReviewComment, error) {
	return nil, nil
}
func (setupStreamProvider) ListOpenPRs(context.Context, string) ([]vcs.PRSummary, error) {
	return nil, nil
}
func (setupStreamProvider) ListClosedPRs(context.Context, string) ([]vcs.PRSummary, error) {
	return nil, nil
}
func (setupStreamProvider) SearchPRsByTitleTag(context.Context, string, string) ([]vcs.PRSummary, error) {
	return nil, nil
}
func (setupStreamProvider) MergePR(context.Context, string, int, string) error { return nil }
func (setupStreamProvider) UpdatePRTitle(context.Context, string, int, string) error {
	return nil
}
func (setupStreamProvider) GetPRMergeCommit(context.Context, string, int) (string, error) {
	return "", vcs.ErrPRNotMerged
}
func (setupStreamProvider) GetAllowedMergeStrategies(context.Context, string) ([]string, error) {
	return []string{"merge"}, nil
}
