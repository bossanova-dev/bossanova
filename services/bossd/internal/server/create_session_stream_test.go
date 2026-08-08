package server

import (
	"bytes"
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
	"github.com/recurser/bossalib/sessionreason"
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
	// Since BOS-720 the stream opens with the accepted SessionCreated frame, so
	// the setup output sits BETWEEN the accepted and settled frames rather than
	// before a single one.
	if len(events) != 4 {
		t.Fatalf("events len = %d, want 4", len(events))
	}
	if events[0].GetSessionCreated() == nil {
		t.Fatalf("event[0] = %T, want the accepted SessionCreated", events[0].GetEvent())
	}
	if got := events[1].GetSetupOutput().Text; got != "first line" {
		t.Fatalf("event[1] setup output = %q, want %q", got, "first line")
	}
	if got := events[2].GetSetupOutput().Text; got != "second line" {
		t.Fatalf("event[2] setup output = %q, want %q", got, "second line")
	}
	if events[3].GetSessionCreated() == nil {
		t.Fatalf("event[3] = %T, want SessionCreated", events[3].GetEvent())
	}
	if events[3].GetSessionCreated().GetAttachedExisting() {
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
	// events[0] is the BOS-720 accepted frame.
	if len(events) != 3 {
		t.Fatalf("events len = %d, want 3", len(events))
	}
	if got := events[1].GetSetupOutput().Text; got != longLine {
		t.Fatalf("long setup output length = %d, want %d", len(got), len(longLine))
	}
	if events[2].GetSessionCreated() == nil {
		t.Fatalf("event[2] = %T, want SessionCreated", events[2].GetEvent())
	}
}

// Inverted for BOS-720. This test previously pinned that an oversized setup
// line failed the whole stream with ResourceExhausted. Under the new ownership
// split that is the wrong lever: the bootstrap no longer belongs to this
// stream, so a rendering problem in ONE client's view must not fail anything.
// relaySetupOutput truncates and keeps going.
func TestRelaySetupOutputTruncatesOversizedLine(t *testing.T) {
	t.Parallel()

	lines := make(chan string, 1)
	lines <- strings.Repeat("x", maxSetupOutputLineBytes+1)
	close(lines)

	sender := &setupOutputCaptureSender{}
	done := make(chan struct{})
	if err := relaySetupOutput(context.Background(), lines, done, sender); err != nil {
		t.Fatalf("relaySetupOutput error = %v, want nil", err)
	}
	if len(sender.events) != 1 {
		t.Fatalf("events len = %d, want 1", len(sender.events))
	}
	if got := len(sender.events[0].GetSetupOutput().Text); got != maxSetupOutputLineBytes {
		t.Fatalf("relayed line length = %d, want %d", got, maxSetupOutputLineBytes)
	}
}

// A caller that goes away without the transport surfacing a send error — the
// reverse-stream command context, chiefly — must still release the handler,
// rather than pin it until the bootstrap's deadline expires.
func TestRelaySetupOutputReturnsWhenTheCallerContextIsCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Neither the bus nor the bootstrap ever settles: only ctx can end this.
	lines := make(chan string)
	done := make(chan struct{})

	relayed := make(chan error, 1)
	go func() { relayed <- relaySetupOutput(ctx, lines, done, &setupOutputCaptureSender{}) }()
	select {
	case err := <-relayed:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("relaySetupOutput error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("relaySetupOutput ignored the cancelled caller context")
	}
}

// relaySetupOutput must drain what the bus buffered before it closed rather
// than returning the instant done is ready. The runner closes the bus and THEN
// done, microseconds apart, so both select cases are ready at once and Go picks
// uniformly at random — a naive `case <-done: return nil` drops the tail of the
// setup output at random.
func TestRelaySetupOutputDrainsBufferedLinesWhenDoneIsAlreadyClosed(t *testing.T) {
	t.Parallel()

	lines := make(chan string, 2)
	lines <- "first"
	lines <- "second"
	close(lines)
	done := make(chan struct{})
	close(done)

	sender := &setupOutputCaptureSender{}
	if err := relaySetupOutput(context.Background(), lines, done, sender); err != nil {
		t.Fatalf("relaySetupOutput error = %v, want nil", err)
	}
	if len(sender.events) != 2 {
		t.Fatalf("events len = %d, want 2 (the buffered tail must not be dropped)", len(sender.events))
	}
	for i, want := range []string{"first", "second"} {
		if got := sender.events[i].GetSetupOutput().Text; got != want {
			t.Fatalf("event[%d] = %q, want %q", i, got, want)
		}
	}
}

// Inverted for BOS-720. This test previously pinned that an oversized setup
// line failed the create and deleted the row. The bootstrap succeeded then and
// succeeds now — the only thing that had gone wrong was one client's view of
// its output, which is no longer grounds for reaping anything.
func TestCreateSessionSurvivesAnOversizedSetupOutputLine(t *testing.T) {
	t.Parallel()

	h := newCreateSessionStreamHarness(t, &setupStreamWorktree{
		output: []string{strings.Repeat("x", maxSetupOutputLineBytes+1)},
	}, &setupStreamAgent{})

	events, err := h.createSession(t, "oversized setup output")
	if err != nil {
		t.Fatalf("CreateSession stream error = %v, want nil", err)
	}
	// events[0] is the BOS-720 accepted frame.
	if len(events) != 3 {
		t.Fatalf("events len = %d, want 3", len(events))
	}
	if got := len(events[1].GetSetupOutput().Text); got != maxSetupOutputLineBytes {
		t.Fatalf("setup output length = %d, want it truncated to %d", got, maxSetupOutputLineBytes)
	}
	if events[2].GetSessionCreated() == nil {
		t.Fatalf("event[2] = %T, want SessionCreated", events[2].GetEvent())
	}
	sessions, listErr := h.sessions.List(context.Background(), h.repo.ID)
	if listErr != nil {
		t.Fatalf("list sessions: %v", listErr)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions len = %d, want 1 (the bootstrap succeeded)", len(sessions))
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
	// The accepted frame lands before the bootstrap fails, so the client keeps
	// the session id even on the error path; the setup line follows it.
	if len(events) != 2 {
		t.Fatalf("events len = %d, want 2", len(events))
	}
	if events[0].GetSessionCreated() == nil {
		t.Fatalf("event[0] = %T, want the accepted SessionCreated", events[0].GetEvent())
	}
	if got := events[1].GetSetupOutput().Text; got != "setup before failure" {
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

	events, err := h.createIsQuickChat(t, "quick chat")
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
// obscurely deep in worktree setup, while a IsQuickChat — which runs directly in
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

	// The guard is gated on !IsQuickChat: a IsQuickChat create still succeeds with
	// the empty worktree base.
	events, err := h.createIsQuickChat(t, "quick chat")
	if err != nil {
		t.Fatalf("CreateSession quick chat error = %v, want success despite empty worktree base", err)
	}
	if len(events) != 1 || events[0].GetSessionCreated() == nil {
		t.Fatalf("quick chat events = %v, want a single SessionCreated", events)
	}
}

// gatedDraftPRProvider parks the first CreateDraftPR call until released, so a
// test can inspect the exact window BOS-540 opened: the session exists and is
// ready, but its draft PR has not been created yet.
type gatedDraftPRProvider struct {
	setupStreamProvider
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
	releaseOnce sync.Once
}

// unpark releases the parked create. Idempotent, so the test can register it as
// a cleanup and still close the gate explicitly on the happy path: without the
// cleanup, any t.Fatalf before the release leaves the provider goroutine parked
// forever and the harness drain burns its full timeout before reporting.
func (p *gatedDraftPRProvider) unpark() {
	p.releaseOnce.Do(func() { close(p.release) })
}

func (p *gatedDraftPRProvider) CreateDraftPR(context.Context, vcs.CreatePROpts) (*vcs.PRInfo, error) {
	// Both closes are once-guarded. A second call — a retry path added later —
	// would otherwise panic on close-of-closed-channel from a background
	// goroutine, taking down the whole test binary rather than one test.
	p.enteredOnce.Do(func() { close(p.entered) })
	<-p.release
	return &vcs.PRInfo{Number: 7, URL: "https://github.com/org/repo/pull/7"}, nil
}

// TestStreamCreateSession_SessionCreatedBeforeDraftPRExists is the client-facing
// half of BOS-540. Moving the draft-PR create into the background changes what a
// CreateSession client sees, so the change is asserted here rather than left to
// be discovered: the stream still emits its normal SetupOutput → SessionCreated
// sequence, but the SessionCreated frame now carries a session with NO PR number
// — ready, attachable, with its worktree and branch — plus a blocked reason that
// says a PR is on its way. The PR number arrives on a later read.
func TestStreamCreateSession_SessionCreatedBeforeDraftPRExists(t *testing.T) {
	provider := &gatedDraftPRProvider{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	h := newCreateSessionStreamHarnessWithProvider(
		t, &setupStreamWorktree{}, &setupStreamAgent{}, provider, zerolog.Nop(),
	)
	// Registered AFTER the harness so LIFO pops it BEFORE the harness's drain:
	// a t.Fatalf below must release the parked provider first, or the drain sits
	// out its full timeout and reports a spurious still-in-flight error.
	t.Cleanup(provider.unpark)

	agentName := "claude"
	var created *pb.Session
	// No PrNumber: this is the default path that opens a draft PR.
	err := h.server.StreamCreateSession(context.Background(), &pb.CreateSessionRequest{
		RepoId:    h.repo.ID,
		Title:     "background draft pr",
		Plan:      "do work",
		AgentName: &agentName,
		Detach:    true,
	}, func(resp *pb.CreateSessionResponse) error {
		if sc := resp.GetSessionCreated(); sc != nil {
			created = sc.GetSession()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("StreamCreateSession: %v", err)
	}
	if created == nil {
		t.Fatal("no SessionCreated frame emitted")
	}

	// The RPC returned while the create is still parked inside the provider.
	<-provider.entered

	if created.PrNumber != nil {
		t.Fatalf("SessionCreated PrNumber = %d, want unset: the client is told about the session, not the PR", *created.PrNumber)
	}
	if created.PrUrl != nil {
		t.Fatalf("SessionCreated PrUrl = %q, want unset", *created.PrUrl)
	}
	// Everything a client needs to use the session is already there.
	if created.State != pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN {
		t.Fatalf("SessionCreated State = %v, want IMPLEMENTING_PLAN", created.State)
	}
	if created.WorktreePath == "" || created.BranchName == "" {
		t.Fatalf("SessionCreated worktree/branch = %q/%q, want both set", created.WorktreePath, created.BranchName)
	}
	if got := created.GetBlockedReason(); got != sessionreason.DraftPRCreationInFlight() {
		t.Fatalf("SessionCreated BlockedReason = %q, want the in-flight marker %q", got, sessionreason.DraftPRCreationInFlight())
	}

	provider.unpark()
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer waitCancel()
	if err := h.lifecycle.WaitForBackgroundDraftPR(waitCtx, created.Id); err != nil {
		t.Fatalf("background draft PR did not finish: %v", err)
	}

	// The PR reaches the client on its next read of the session row — no new
	// event type, no second frame on this stream.
	after, err := h.sessions.Get(context.Background(), created.Id)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if after.PRNumber == nil || *after.PRNumber != 7 {
		t.Fatalf("persisted PRNumber = %v, want 7", after.PRNumber)
	}
	if after.BlockedReason != nil {
		t.Fatalf("persisted BlockedReason = %q, want nil once the PR landed", *after.BlockedReason)
	}
}

func TestStreamCreateSession_LinearSuggestedBranchCreatesFreshWorktree(t *testing.T) {
	var logs bytes.Buffer
	logger := zerolog.New(zerolog.SyncWriter(&logs))
	worktrees := &setupStreamWorktree{}
	h := newCreateSessionStreamHarnessWithProvider(
		t,
		worktrees,
		&setupStreamAgent{},
		setupStreamProvider{},
		logger,
	)
	branchName := "dave/bos-504-speed-up-startup"
	trackerID := "BOS-504"
	agentName := "claude"
	var created bool

	err := h.server.StreamCreateSession(context.Background(), &pb.CreateSessionRequest{
		RepoId:     h.repo.ID,
		Title:      "speed up startup",
		Plan:       "implement BOS-504",
		TrackerId:  &trackerID,
		BranchName: &branchName,
		AgentName:  &agentName,
		DeferPr:    true,
	}, func(resp *pb.CreateSessionResponse) error {
		created = created || resp.GetSessionCreated() != nil
		return nil
	})
	if err != nil {
		t.Fatalf("StreamCreateSession: %v", err)
	}
	if !created {
		t.Fatal("SessionCreated frame not emitted")
	}

	createCalls, existingCalls := worktrees.callRecords()
	if len(createCalls) != 1 {
		t.Fatalf("Create calls = %d, want 1", len(createCalls))
	}
	if got := createCalls[0].BranchName; got != branchName {
		t.Fatalf("Create BranchName = %q, want %q", got, branchName)
	}
	if len(existingCalls) != 0 {
		t.Fatalf("CreateFromExistingBranch calls = %d, want 0", len(existingCalls))
	}
	if got := logs.String(); !strings.Contains(got, `"branch_mode":"fresh-branch"`) {
		t.Fatalf("logs missing fresh branch mode:\n%s", got)
	}
}

func TestStreamCreateSession_PRUsesExistingCheckout(t *testing.T) {
	var logs bytes.Buffer
	logger := zerolog.New(zerolog.SyncWriter(&logs))
	worktrees := &setupStreamWorktree{}
	h := newCreateSessionStreamHarnessWithProvider(
		t,
		worktrees,
		&setupStreamAgent{},
		setupStreamProvider{},
		logger,
	)

	if _, err := h.createSession(t, "existing PR"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	createCalls, existingCalls := worktrees.callRecords()
	if len(createCalls) != 0 {
		t.Fatalf("Create calls = %d, want 0", len(createCalls))
	}
	if len(existingCalls) != 1 {
		t.Fatalf("CreateFromExistingBranch calls = %d, want 1", len(existingCalls))
	}
	if got, want := existingCalls[0].BranchName, "dependabot/npm/pkg-1.0.0"; got != want {
		t.Fatalf("CreateFromExistingBranch BranchName = %q, want %q", got, want)
	}
	if got := logs.String(); !strings.Contains(got, `"result":"checked_out_existing"`) {
		t.Fatalf("logs missing existing checkout result:\n%s", got)
	}
}

func TestStreamCreateSession_QuickChatDoesNotWaitForWorktreeStart(t *testing.T) {
	var logs bytes.Buffer
	logger := zerolog.New(zerolog.SyncWriter(&logs))
	createStarted := make(chan struct{})
	releaseCreate := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() { close(releaseCreate) })
	}
	t.Cleanup(release)

	worktrees := &setupStreamWorktree{
		createFn: func(_ context.Context, _ gitpkg.CreateOpts) (*gitpkg.CreateResult, error) {
			close(createStarted)
			<-releaseCreate
			return &gitpkg.CreateResult{
				WorktreePath: "/tmp/worktrees/repo/normal",
				BranchName:   "normal",
			}, nil
		},
	}
	h := newCreateSessionStreamHarnessWithProvider(
		t,
		worktrees,
		&setupStreamAgent{},
		setupStreamProvider{},
		logger,
	)
	agentName := "claude"
	normalDone := make(chan error, 1)
	go func() {
		normalDone <- h.server.StreamCreateSession(context.Background(), &pb.CreateSessionRequest{
			RepoId:    h.repo.ID,
			Title:     "normal",
			Plan:      "do normal work",
			AgentName: &agentName,
			DeferPr:   true,
		}, func(*pb.CreateSessionResponse) error { return nil })
	}()

	select {
	case <-createStarted:
	case <-time.After(time.Second):
		t.Fatal("normal request did not enter worktree Create")
	}
	select {
	case err := <-normalDone:
		t.Fatalf("normal request returned before Create was released: %v", err)
	default:
	}

	type streamResult struct {
		err     error
		created bool
	}
	quickDone := make(chan streamResult, 1)
	go func() {
		result := streamResult{}
		result.err = h.server.StreamCreateSession(context.Background(), &pb.CreateSessionRequest{
			RepoId:      h.repo.ID,
			Title:       "quick",
			Plan:        "answer a question",
			AgentName:   &agentName,
			IsQuickChat: true,
		}, func(resp *pb.CreateSessionResponse) error {
			result.created = result.created || resp.GetSessionCreated() != nil
			return nil
		})
		quickDone <- result
	}()

	select {
	case result := <-quickDone:
		if result.err != nil {
			t.Fatalf("Quick Chat StreamCreateSession: %v", result.err)
		}
		if !result.created {
			t.Fatal("Quick Chat did not emit SessionCreated")
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("Quick Chat waited for blocked worktree startup")
	}
	select {
	case err := <-normalDone:
		t.Fatalf("normal request returned before explicit release: %v", err)
	default:
	}

	release()
	if err := <-normalDone; err != nil {
		t.Fatalf("normal StreamCreateSession after release: %v", err)
	}
	gotLogs := logs.String()
	for _, want := range []string{
		`"quick_chat":true`,
		`"branch_mode":"quick-chat"`,
		`"start_lock_wait":0`,
	} {
		if !strings.Contains(gotLogs, want) {
			t.Fatalf("logs missing %q:\n%s", want, gotLogs)
		}
	}
}

func TestStreamCreateSession_LogsStartupTimingAndLifecycleBranchResult(t *testing.T) {
	var logs bytes.Buffer
	logger := zerolog.New(zerolog.SyncWriter(&logs))
	// Distinct sentinel phase durations so the assertions below pin which
	// CreateResult field feeds which log key. With the fake's zero-valued
	// result every phase renders as 0 and a crossed wire (e.g. logging
	// WorktreeAddDuration under fetch_ms) would still satisfy a bare
	// key-presence check.
	worktrees := &setupStreamWorktree{
		createFromExistingErr: errors.New("remote branch missing"),
		createFn: func(_ context.Context, _ gitpkg.CreateOpts) (*gitpkg.CreateResult, error) {
			return &gitpkg.CreateResult{
				WorktreePath:        "/tmp/worktrees/repo/branch",
				BranchName:          "branch",
				FetchDuration:       11 * time.Millisecond,
				BranchProbeDuration: 22 * time.Millisecond,
				WorktreeAddDuration: 33 * time.Millisecond,
				SetupScriptDuration: 44 * time.Millisecond,
			}, nil
		},
	}
	h := newCreateSessionStreamHarnessWithProvider(
		t,
		worktrees,
		&setupStreamAgent{},
		setupStreamProvider{},
		logger,
	)

	if _, err := h.createSession(t, "logged fallback"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got := logs.String()
	for _, want := range []string{
		`"message":"session startup complete"`,
		`"quick_chat":false`,
		`"branch_mode":"existing-pr"`,
		`"startup_duration":`,
		`"start_lock_wait":`,
		`"message":"existing branch checkout failed; creating fresh branch"`,
		`"result":"existing_checkout_failed"`,
		`"message":"worktree startup complete"`,
		`"result":"created_fresh_after_existing_checkout_error"`,
		`"worktree_duration":`,
		// Value-level, not just key presence: zerolog's default
		// DurationFieldUnit is time.Millisecond (this repo sets no override),
		// so each sentinel above renders as its millisecond count.
		`"fetch_ms":11`,
		`"branch_probe_ms":22`,
		`"worktree_add_ms":33`,
		`"setup_script_ms":44`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("logs missing %q:\n%s", want, got)
		}
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
	// lifecycle is exposed so tests can join the background draft-PR step
	// (BOS-540), which outlives the CreateSession RPC.
	lifecycle *session.Lifecycle
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

	lifecycle := session.NewLifecycle(sessions, repos, nil, nil, worktrees, runner, nil, provider, logger)
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

	h := &createSessionStreamHarness{
		client: bossanovav1connect.NewDaemonServiceClient(
			httpServer.Client(), httpServer.URL,
			connect.WithInterceptors(socketauth.NewClientInterceptor(streamTestToken)),
		),
		server:    s,
		repo:      repo,
		repos:     repos,
		sessions:  sessions,
		lifecycle: lifecycle,
	}

	// A create that is still opening its draft PR when the test ends would keep
	// writing to a closed test DB, so every harness drains the background step.
	t.Cleanup(func() { h.awaitDraftPRs(t) })

	return h
}

// awaitDraftPRs blocks until every background draft-PR create this harness
// started has finished (BOS-540). Any test that both creates a default-path
// session AND reads shared state the step writes — most sharply a zerolog
// bytes.Buffer sink, which the step logs into after the RPC has returned — must
// call this BEFORE that read. Draining, not sleeping: the step's registry
// channel closes after its last write, so the read is ordered behind it.
func (h *createSessionStreamHarness) awaitDraftPRs(t *testing.T) {
	t.Helper()
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer drainCancel()
	if abandoned := h.lifecycle.WaitForBackgroundDraftPRs(drainCtx); len(abandoned) > 0 {
		t.Errorf("background draft PR still in flight for %v", abandoned)
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

func (h *createSessionStreamHarness) createIsQuickChat(t *testing.T, title string) ([]*pb.CreateSessionResponse, error) {
	t.Helper()

	agentName := "claude"
	stream, err := h.client.CreateSession(context.Background(), connect.NewRequest(&pb.CreateSessionRequest{
		RepoId:      h.repo.ID,
		Title:       title,
		Plan:        "quick question",
		IsQuickChat: true,
		AgentName:   &agentName,
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
	mu sync.Mutex

	output                []string
	createErr             error
	createFromExistingErr error
	createFn              func(context.Context, gitpkg.CreateOpts) (*gitpkg.CreateResult, error)

	createCalls             []gitpkg.CreateOpts
	createFromExistingCalls []gitpkg.CreateFromExistingBranchOpts

	// Cleanup records (BOS-717): which branches were force-deleted and which
	// worktrees purged, so a failed-create test can assert the artifacts this
	// attempt made were actually reclaimed.
	reapedBranches []string
	purgedBranches []string
}

func (w *setupStreamWorktree) cleanupRecords() (reaped, purged []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.reapedBranches...), append([]string(nil), w.purgedBranches...)
}

func (w *setupStreamWorktree) Create(ctx context.Context, opts gitpkg.CreateOpts) (*gitpkg.CreateResult, error) {
	w.mu.Lock()
	w.createCalls = append(w.createCalls, opts)
	output := append([]string(nil), w.output...)
	createErr := w.createErr
	createFn := w.createFn
	w.mu.Unlock()

	if createFn != nil {
		return createFn(ctx, opts)
	}
	for _, text := range output {
		if opts.SetupScriptOutput == nil {
			continue
		}
		if _, err := io.WriteString(opts.SetupScriptOutput, text); err != nil {
			return nil, err
		}
	}
	if createErr != nil {
		return nil, createErr
	}
	return &gitpkg.CreateResult{WorktreePath: "/tmp/worktrees/repo/branch", BranchName: "branch"}, nil
}

func (w *setupStreamWorktree) CreateFromExistingBranch(_ context.Context, opts gitpkg.CreateFromExistingBranchOpts) (*gitpkg.CreateResult, error) {
	w.mu.Lock()
	w.createFromExistingCalls = append(w.createFromExistingCalls, opts)
	output := append([]string(nil), w.output...)
	createErr := w.createFromExistingErr
	if createErr == nil {
		createErr = w.createErr
	}
	w.mu.Unlock()

	for _, text := range output {
		if opts.SetupScriptOutput == nil {
			continue
		}
		if _, err := io.WriteString(opts.SetupScriptOutput, text); err != nil {
			return nil, err
		}
	}
	if createErr != nil {
		return nil, createErr
	}
	return &gitpkg.CreateResult{WorktreePath: "/tmp/worktrees/repo/branch", BranchName: opts.BranchName}, nil
}

func (w *setupStreamWorktree) callRecords() ([]gitpkg.CreateOpts, []gitpkg.CreateFromExistingBranchOpts) {
	w.mu.Lock()
	defer w.mu.Unlock()

	return append([]gitpkg.CreateOpts(nil), w.createCalls...),
		append([]gitpkg.CreateFromExistingBranchOpts(nil), w.createFromExistingCalls...)
}

func (w *setupStreamWorktree) Archive(context.Context, string) error { return nil }
func (w *setupStreamWorktree) Resurrect(context.Context, gitpkg.ResurrectOpts) error {
	return nil
}
func (w *setupStreamWorktree) ReapLocalBranches(_ context.Context, _ string, branches []string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.reapedBranches = append(w.reapedBranches, branches...)
	return nil
}

func (w *setupStreamWorktree) PurgeWorktree(_ context.Context, _, _, _, branch string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.purgedBranches = append(w.purgedBranches, branch)
	return nil
}
func (w *setupStreamWorktree) EmptyCommit(context.Context, string, string) error         { return nil }
func (w *setupStreamWorktree) VerifyCurrentBranch(context.Context, string, string) error { return nil }
func (w *setupStreamWorktree) Push(context.Context, string, string) error                { return nil }
func (w *setupStreamWorktree) PushWithLease(context.Context, string, string, string) (string, error) {
	return "pushed-head-sha", nil
}
func (w *setupStreamWorktree) InjectPRNumbers(context.Context, string, string, int, string) error {
	return nil
}
func (w *setupStreamWorktree) VerifyPushedBranchAheadOfBase(context.Context, string, string, string, gitpkg.VerifyPushedBranchAheadOfBaseOpts) (*gitpkg.BranchVerification, error) {
	return &gitpkg.BranchVerification{HeadSHA: "head", BaseSHA: "base", RemoteHeadSHA: "head", AheadCount: 1}, nil
}
func (w *setupStreamWorktree) Status(context.Context, string) (string, error) { return "", nil }
func (w *setupStreamWorktree) CommitSubjects(context.Context, string, string) ([]string, error) {
	return nil, nil
}

func (w *setupStreamWorktree) HasDiffAgainstBase(context.Context, string, string) (bool, error) {
	return true, nil
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
func (w *setupStreamWorktree) CountMergeCommits(context.Context, string, string, string) (int, error) {
	return 0, nil
}
func (w *setupStreamWorktree) DeleteLocalBranch(context.Context, string, string) error { return nil }
func (w *setupStreamWorktree) BranchSafeToDelete(context.Context, string, string, string) (bool, error) {
	return false, nil
}
func (w *setupStreamWorktree) FetchBase(context.Context, string, string) error { return nil }
func (w *setupStreamWorktree) MergeLocalBranch(context.Context, string, string, string, string) error {
	return nil
}
func (w *setupStreamWorktree) CountBehindBase(context.Context, string, string, string) (int, error) {
	return 0, nil
}
func (w *setupStreamWorktree) RebaseOntoBaseAndPush(context.Context, string, string, string) (*gitpkg.RebaseResult, error) {
	return &gitpkg.RebaseResult{}, nil
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
func (a *setupStreamAgent) StopByAgent(context.Context, string, string) error { return nil }
func (a *setupStreamAgent) IsRunningByAgent(string, string) bool              { return false }

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
