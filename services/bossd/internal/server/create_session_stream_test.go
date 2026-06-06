package server

import (
	"context"
	"database/sql"
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
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	gitpkg "github.com/recurser/bossd/internal/git"
	"github.com/recurser/bossd/internal/session"
	"github.com/rs/zerolog"
)

var setupServerTestDBMigrateMu sync.Mutex

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

type createSessionStreamHarness struct {
	client   bossanovav1connect.DaemonServiceClient
	repo     *models.Repo
	repos    db.RepoStore
	sessions db.SessionStore
}

func newCreateSessionStreamHarness(t *testing.T, worktrees *setupStreamWorktree, runner *setupStreamAgent) *createSessionStreamHarness {
	t.Helper()

	sqlDB := setupServerTestDB(t)
	repos := db.NewRepoStore(sqlDB)
	sessions := db.NewSessionStore(sqlDB)
	setupScript := "echo setup"
	repo, err := repos.Create(context.Background(), db.CreateRepoParams{
		DisplayName:       "repo",
		LocalPath:         "/tmp/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		SetupScript:       &setupScript,
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}

	provider := setupStreamProvider{}
	lifecycle := session.NewLifecycle(sessions, repos, nil, nil, worktrees, runner, nil, provider, zerolog.Nop())
	s := New(Config{
		Repos:     repos,
		Sessions:  sessions,
		Worktrees: worktrees,
		Provider:  provider,
		Lifecycle: lifecycle,
		Logger:    zerolog.Nop(),
	})

	mux := http.NewServeMux()
	path, handler := bossanovav1connect.NewDaemonServiceHandler(s)
	mux.Handle(path, handler)
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	return &createSessionStreamHarness{
		client:   bossanovav1connect.NewDaemonServiceClient(httpServer.Client(), httpServer.URL),
		repo:     repo,
		repos:    repos,
		sessions: sessions,
	}
}

func (h *createSessionStreamHarness) createSession(t *testing.T, title string) ([]*pb.CreateSessionResponse, error) {
	t.Helper()

	prNumber := int32(123)
	agentName := "claude"
	stream, err := h.client.CreateSession(context.Background(), connect.NewRequest(&pb.CreateSessionRequest{
		RepoId:    h.repo.ID,
		Title:     title,
		Plan:      "do work",
		PrNumber:  &prNumber,
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
func (w *setupStreamWorktree) VerifyPushedBranchAheadOfBase(context.Context, string, string, string) (*gitpkg.BranchVerification, error) {
	return &gitpkg.BranchVerification{HeadSHA: "head", BaseSHA: "base", RemoteHeadSHA: "head", AheadCount: 1}, nil
}
func (w *setupStreamWorktree) Status(context.Context, string) (string, error) { return "", nil }
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
func (w *setupStreamWorktree) EnsureBaseBranchReadyForSync(context.Context, string, string) error {
	return nil
}
func (w *setupStreamWorktree) SyncBaseBranch(context.Context, string, string) error { return nil }
func (w *setupStreamWorktree) IsAncestor(context.Context, string, string, string) (bool, error) {
	return true, nil
}
func (w *setupStreamWorktree) FetchBase(context.Context, string, string) error { return nil }
func (w *setupStreamWorktree) MergeLocalBranch(context.Context, string, string, string, string) error {
	return nil
}

type setupStreamAgent struct {
	startErr error
}

func (a *setupStreamAgent) Start(context.Context, string, string, *string, string) (string, error) {
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
func (a *setupStreamAgent) StartByAgent(context.Context, string, string, string, *string, string) (string, error) {
	return "agent-session", a.startErr
}
func (a *setupStreamAgent) StopByAgent(string, string) error     { return nil }
func (a *setupStreamAgent) IsRunningByAgent(string, string) bool { return false }

type setupStreamProvider struct{}

func (setupStreamProvider) CreateDraftPR(context.Context, vcs.CreatePROpts) (*vcs.PRInfo, error) {
	return &vcs.PRInfo{}, nil
}
func (setupStreamProvider) GetPRStatus(context.Context, string, int) (*vcs.PRStatus, error) {
	return &vcs.PRStatus{}, nil
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
