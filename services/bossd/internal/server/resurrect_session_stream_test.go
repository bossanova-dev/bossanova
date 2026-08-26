package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/socketauth"
	"github.com/recurser/bossd/internal/db"
	gitpkg "github.com/recurser/bossd/internal/git"
	"github.com/recurser/bossd/internal/proofenv"
	"github.com/recurser/bossd/internal/session"
	"github.com/rs/zerolog"
)

// resurrectStreamHarness stands the DaemonService up behind a real http.Server
// whose WriteTimeout is DELIBERATELY tiny, then wraps the handler in the same
// withStreamingWriteDeadlineOverride the daemon installs in Listen.
//
// That short WriteTimeout is the whole point. It is a scaled model of the
// production 120s ceiling (services/bossd/internal/server/server.go), which is
// the server half of BOS-984: a resurrect whose setup script outran it could
// not write its response at all, whatever deadline the client was willing to
// wait. Scaling the ceiling down rather than the script up keeps the test fast
// while exercising the identical mechanism — remove ResurrectSession from
// setupStreamProcedures and every test in this file that runs a slow setup
// fails.
type resurrectStreamHarness struct {
	client    bossanovav1connect.DaemonServiceClient
	repo      *models.Repo
	sessions  db.SessionStore
	worktrees *setupStreamWorktree
	sessionID string
}

const (
	// resurrectTestWriteTimeout is the scaled http.Server WriteTimeout. Short
	// enough to keep the suite fast, long enough that an unrelated scheduling
	// hiccup does not trip it.
	resurrectTestWriteTimeout = 300 * time.Millisecond
	// resurrectTestSetupDuration is how long the injected setup script "runs".
	// Comfortably past resurrectTestWriteTimeout, so a response that could not
	// outlive the write deadline cannot possibly arrive.
	resurrectTestSetupDuration = 900 * time.Millisecond
)

// newResurrectStreamHarness builds the harness with an archived session ready
// to restore. setupScript nil models a repo with no setup step at all.
func newResurrectStreamHarness(t *testing.T, worktrees *setupStreamWorktree, setupScript *string) *resurrectStreamHarness {
	t.Helper()
	return newResurrectStreamHarnessWithBootstrapTimeout(t, worktrees, setupScript, 0)
}

// newResurrectStreamHarnessWithBootstrapTimeout builds the harness with an
// explicit lifecycle bootstrap bound. Zero keeps the production default.
func newResurrectStreamHarnessWithBootstrapTimeout(t *testing.T, worktrees *setupStreamWorktree, setupScript *string, bootstrapTimeout time.Duration) *resurrectStreamHarness {
	t.Helper()

	sqlDB := setupServerTestDB(t)
	repos := db.NewRepoStore(sqlDB)
	sessions := db.NewSessionStore(sqlDB)
	ctx := context.Background()

	repo, err := repos.Create(ctx, db.CreateRepoParams{
		DisplayName:       "repo",
		LocalPath:         "/tmp/repo",
		OriginURL:         "https://github.com/org/repo",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
		SetupScript:       setupScript,
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}

	sess, err := sessions.Create(ctx, db.CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "archived session",
		Plan:         "do the thing",
		WorktreePath: "/tmp/worktrees/repo/feature",
		BranchName:   "feature",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := sessions.Archive(ctx, sess.ID); err != nil {
		t.Fatalf("archive session: %v", err)
	}

	logger := zerolog.Nop()
	runner := &setupStreamAgent{}
	lifecycle := session.NewLifecycle(sessions, repos, nil, nil, worktrees, runner, nil, setupStreamProvider{}, logger)
	lifecycle.SetProofEnvResolver(proofenv.NewNoop())
	if bootstrapTimeout > 0 {
		lifecycle.SetBootstrapTimeout(bootstrapTimeout)
	}

	s := New(Config{
		Repos:     repos,
		Sessions:  sessions,
		Worktrees: worktrees,
		Provider:  setupStreamProvider{},
		Lifecycle: lifecycle,
		Logger:    logger,
	})

	mux := http.NewServeMux()
	path, handler := bossanovav1connect.NewDaemonServiceHandler(s,
		connect.WithInterceptors(socketauth.NewServerInterceptor(streamTestToken)))
	mux.Handle(path, withStreamingWriteDeadlineOverride(handler))

	httpServer := httptest.NewUnstartedServer(mux)
	httpServer.Config.WriteTimeout = resurrectTestWriteTimeout
	httpServer.Start()
	t.Cleanup(httpServer.Close)

	return &resurrectStreamHarness{
		client: bossanovav1connect.NewDaemonServiceClient(
			httpServer.Client(), httpServer.URL,
			connect.WithInterceptors(socketauth.NewClientInterceptor(streamTestToken)),
		),
		repo:      repo,
		sessions:  sessions,
		worktrees: worktrees,
		sessionID: sess.ID,
	}
}

// resurrectOutcome is the drained result of one resurrect stream.
type resurrectOutcome struct {
	setupLines []string
	session    *pb.Session
	setupError string
	err        error
}

func (h *resurrectStreamHarness) resurrect(t *testing.T, ctx context.Context) resurrectOutcome {
	t.Helper()
	var out resurrectOutcome
	stream, err := h.client.ResurrectSession(ctx,
		connect.NewRequest(&pb.ResurrectSessionRequest{Id: h.sessionID}))
	if err != nil {
		out.err = err
		return out
	}
	defer func() { _ = stream.Close() }()
	for stream.Receive() {
		msg := stream.Msg()
		if so := msg.GetSetupOutput(); so != nil {
			out.setupLines = append(out.setupLines, so.GetText())
		}
		if r := msg.GetSessionResurrected(); r != nil {
			out.session = r.GetSession()
			out.setupError = r.GetSetupError()
		}
	}
	out.err = stream.Err()
	return out
}

// slowSetupResurrect models a setup script that writes progress for longer than
// the server's write deadline before succeeding.
func slowSetupResurrect(d time.Duration, lines ...string) func(context.Context, gitpkg.ResurrectOpts) (*gitpkg.ResurrectResult, error) {
	return func(ctx context.Context, opts gitpkg.ResurrectOpts) (*gitpkg.ResurrectResult, error) {
		per := d / time.Duration(len(lines)+1)
		for _, line := range lines {
			select {
			case <-time.After(per):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if opts.SetupScriptOutput != nil {
				if _, err := io.WriteString(opts.SetupScriptOutput, line+"\n"); err != nil {
					return nil, err
				}
			}
		}
		select {
		case <-time.After(per):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &gitpkg.ResurrectResult{}, nil
	}
}

// TestResurrectSessionSurvivesASetupScriptPastTheWriteTimeout is the BOS-984
// regression. The injected setup script runs three times longer than the
// server's write deadline; the resurrect must still complete and deliver its
// terminal frame.
//
// This is the test that could not pass before the fix. A unary
// ResurrectSession's single response is written after the whole resurrect
// finishes, i.e. after the deadline has already expired — and
// withCreateSessionWriteDeadlineOverride exempted CreateSession only, so the
// deadline was live for this procedure. Never a real `pnpm install`: the script
// is injected, so the assertion is about the ceiling, not about the network.
func TestResurrectSessionSurvivesASetupScriptPastTheWriteTimeout(t *testing.T) {
	t.Parallel()

	script := "pnpm install"
	wt := &setupStreamWorktree{
		resurrectFn: slowSetupResurrect(resurrectTestSetupDuration,
			"installing dependencies", "linking workspace"),
	}
	h := newResurrectStreamHarness(t, wt, &script)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	started := time.Now()
	out := h.resurrect(t, ctx)
	elapsed := time.Since(started)

	if out.err != nil {
		t.Fatalf("resurrect failed after %v with a %v write timeout: %v\n"+
			"This is the BOS-984 defect: a setup script that outlives the daemon's response "+
			"ceiling must not fail a healthy resurrect.", elapsed, resurrectTestWriteTimeout, out.err)
	}
	if elapsed <= resurrectTestWriteTimeout {
		t.Fatalf("resurrect returned in %v, which is inside the %v write timeout — "+
			"the injected slow setup script did not actually run, so this test proves nothing",
			elapsed, resurrectTestWriteTimeout)
	}
	if out.session == nil {
		t.Fatal("no terminal SessionResurrected frame")
	}
	if out.session.GetArchivedAt() != nil {
		t.Fatal("resurrected session is still archived")
	}
	if out.setupError != "" {
		t.Fatalf("setup_error = %q, want empty for a successful setup", out.setupError)
	}
	if !containsLine(out.setupLines, "installing dependencies") {
		t.Fatalf("setup output %q is missing the script's progress lines", out.setupLines)
	}
}

// TestResurrectSessionStreamsProgressBeforeItSettles pins AC2: the user sees
// progress DURING the long leg, not silence and then a result. It asserts the
// ordering the unary shape could not express — a setup line arrives strictly
// before the terminal frame.
func TestResurrectSessionStreamsProgressBeforeItSettles(t *testing.T) {
	t.Parallel()

	script := "pnpm install"
	wt := &setupStreamWorktree{
		resurrectFn: slowSetupResurrect(resurrectTestSetupDuration, "installing dependencies"),
	}
	h := newResurrectStreamHarness(t, wt, &script)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stream, err := h.client.ResurrectSession(ctx,
		connect.NewRequest(&pb.ResurrectSessionRequest{Id: h.sessionID}))
	if err != nil {
		t.Fatalf("open resurrect stream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	var firstProgressAt, terminalAt time.Time
	for stream.Receive() {
		msg := stream.Msg()
		if msg.GetSetupOutput() != nil && firstProgressAt.IsZero() {
			firstProgressAt = time.Now()
		}
		if msg.GetSessionResurrected() != nil {
			terminalAt = time.Now()
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("resurrect stream: %v", err)
	}
	if firstProgressAt.IsZero() {
		t.Fatal("no progress frame arrived; the restore was silent for its whole duration")
	}
	if terminalAt.IsZero() {
		t.Fatal("no terminal frame arrived")
	}
	if !firstProgressAt.Before(terminalAt) {
		t.Fatalf("first progress frame at %v is not before the terminal frame at %v; "+
			"progress that only arrives with the result is not progress",
			firstProgressAt, terminalAt)
	}
}

// TestResurrectSessionReportsASetupFailureWithoutFailingTheRPC pins AC3's first
// half: a failed setup script is reported AS a setup failure on an otherwise
// successful resurrect. Before BOS-984 this error was dropped on the floor, so
// a restore whose dependencies never installed was indistinguishable from a
// clean one.
func TestResurrectSessionReportsASetupFailureWithoutFailingTheRPC(t *testing.T) {
	t.Parallel()

	script := "pnpm install"
	wt := &setupStreamWorktree{
		resurrectFn: func(context.Context, gitpkg.ResurrectOpts) (*gitpkg.ResurrectResult, error) {
			return &gitpkg.ResurrectResult{SetupErr: errors.New("setup script exited 1")}, nil
		},
	}
	h := newResurrectStreamHarness(t, wt, &script)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out := h.resurrect(t, ctx)

	if out.err != nil {
		t.Fatalf("a setup failure must NOT fail the resurrect (the session is back; only its "+
			"dependencies are missing), got %v", out.err)
	}
	if out.session == nil {
		t.Fatal("no terminal SessionResurrected frame")
	}
	if !strings.Contains(out.setupError, "setup script exited 1") {
		t.Fatalf("setup_error = %q, want it to name the script failure", out.setupError)
	}
}

// TestResurrectSessionWorktreeFailureFailsTheRPC pins AC3's other half: a
// worktree failure is a DIFFERENT outcome from a setup failure. It fails the
// RPC with CodeInternal and delivers no terminal frame, so a caller can tell
// "never came back" from "came back without dependencies" — and both from a
// deadline, which arrives as CodeDeadlineExceeded from the transport.
func TestResurrectSessionWorktreeFailureFailsTheRPC(t *testing.T) {
	t.Parallel()

	script := "pnpm install"
	wt := &setupStreamWorktree{
		resurrectFn: func(context.Context, gitpkg.ResurrectOpts) (*gitpkg.ResurrectResult, error) {
			return nil, errors.New("worktree add: fatal: invalid reference")
		},
	}
	h := newResurrectStreamHarness(t, wt, &script)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out := h.resurrect(t, ctx)

	if out.err == nil {
		t.Fatal("a worktree failure must fail the resurrect")
	}
	if got := connect.CodeOf(out.err); got != connect.CodeInternal {
		t.Fatalf("code = %v, want CodeInternal (a deadline would be CodeDeadlineExceeded)", got)
	}
	if !strings.Contains(out.err.Error(), "resurrect worktree") {
		t.Fatalf("error %q does not name the worktree step", out.err)
	}
	if out.session != nil {
		t.Fatal("a failed resurrect must not deliver a terminal SessionResurrected frame")
	}
	if out.setupError != "" {
		t.Fatalf("setup_error = %q; a worktree failure is not a setup failure", out.setupError)
	}
}

// TestResurrectSessionWithoutASetupScriptIsNotSlowedDown pins AC4. A repo with
// no setup script must reach its terminal frame promptly and carry no setup
// output at all: the streaming conversion adds a frame type, not a phase.
//
// The bound is the same scaled write timeout the slow tests blow through, which
// makes the two assertions read against each other — a no-script resurrect
// finishes inside a ceiling a scripted one cannot.
func TestResurrectSessionWithoutASetupScriptIsNotSlowedDown(t *testing.T) {
	t.Parallel()

	wt := &setupStreamWorktree{}
	h := newResurrectStreamHarness(t, wt, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	started := time.Now()
	out := h.resurrect(t, ctx)
	elapsed := time.Since(started)

	if out.err != nil {
		t.Fatalf("resurrect failed: %v", out.err)
	}
	if out.session == nil {
		t.Fatal("no terminal SessionResurrected frame")
	}
	if elapsed > resurrectTestWriteTimeout {
		t.Fatalf("a script-less resurrect took %v, over the %v ceiling — the streaming "+
			"conversion must not add latency to the path that has nothing to stream",
			elapsed, resurrectTestWriteTimeout)
	}
	// The coarse phase lines still stream — they are what makes the restore
	// legible, and they cost nothing. What must NOT appear is setup-script
	// output, because no script ran.
	for _, line := range out.setupLines {
		if !isResurrectPhaseLine(line) {
			t.Fatalf("unexpected setup-script output %q on a repo with no setup script "+
				"(phase lines are fine; script output is not)", line)
		}
	}
	calls := wt.resurrectOpts()
	if len(calls) != 1 {
		t.Fatalf("worktree Resurrect calls = %d, want 1", len(calls))
	}
	if calls[0].SetupScript != nil {
		t.Fatalf("SetupScript = %v, want nil for a repo with no setup script", *calls[0].SetupScript)
	}
}

// TestStreamingWriteDeadlineOverrideCoversResurrect pins the membership that
// makes the fix work at all. The exemption used to be a single equality test
// against CreateSession; a set is only better if resurrect is actually in it.
func TestStreamingWriteDeadlineOverrideCoversResurrect(t *testing.T) {
	t.Parallel()

	for _, procedure := range []string{
		bossanovav1connect.DaemonServiceCreateSessionProcedure,
		bossanovav1connect.DaemonServiceResurrectSessionProcedure,
		bossanovav1connect.DaemonServiceGetRunCostProcedure,
	} {
		rw := &deadlineCaptureResponseWriter{header: http.Header{}}
		called := false
		handler := withStreamingWriteDeadlineOverride(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			called = true
		}))
		handler.ServeHTTP(rw, httptest.NewRequest(http.MethodPost, procedure, nil))

		if !called {
			t.Fatalf("%s: wrapped handler was not called", procedure)
		}
		if !rw.writeDeadlineSet || !rw.writeDeadline.IsZero() {
			t.Fatalf("%s: write deadline not cleared (set=%v, deadline=%v)",
				procedure, rw.writeDeadlineSet, rw.writeDeadline)
		}
	}

	// An ordinary unary procedure keeps its deadline: the exemption is a
	// deliberate carve-out, not a blanket removal of the daemon's ceiling.
	rw := &deadlineCaptureResponseWriter{header: http.Header{}}
	handler := withStreamingWriteDeadlineOverride(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	handler.ServeHTTP(rw, httptest.NewRequest(http.MethodPost,
		bossanovav1connect.DaemonServiceArchiveSessionProcedure, nil))
	if rw.writeDeadlineSet {
		t.Fatal("ArchiveSession must keep the daemon's write deadline")
	}
}

// TestSetupLineEmitterSplitsAndFlushes covers the writer that turns the setup
// script's arbitrary output chunks into whole-line frames, including the tail
// that arrives without a trailing newline.
func TestSetupLineEmitterSplitsAndFlushes(t *testing.T) {
	t.Parallel()

	var got []string
	e := newSetupLineEmitter(func(line string) { got = append(got, line) })

	if _, err := e.Write([]byte("first\nsec")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := e.Write([]byte("ond\nthird-no-newline")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if want := []string{"first", "second"}; !equalLines(got, want) {
		t.Fatalf("before Flush got %q, want %q", got, want)
	}
	e.Flush()
	if want := []string{"first", "second", "third-no-newline"}; !equalLines(got, want) {
		t.Fatalf("after Flush got %q, want %q", got, want)
	}
	// Flush is idempotent — the terminal-frame path calls it unconditionally.
	e.Flush()
	if len(got) != 3 {
		t.Fatalf("second Flush emitted again: %q", got)
	}
}

func containsLine(lines []string, want string) bool {
	for _, line := range lines {
		if strings.Contains(line, want) {
			return true
		}
	}
	return false
}

func equalLines(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// resurrectPhaseLines are the coarse progress lines Lifecycle.ResurrectSession
// writes around the setup step. They are emitted whether or not a setup script
// exists, so a test that asserts "no script output" must exclude them.
var resurrectPhaseLines = []string{"recreating worktree", "worktree restored", "restarting agent"}

func isResurrectPhaseLine(line string) bool {
	for _, phase := range resurrectPhaseLines {
		if line == phase {
			return true
		}
	}
	return false
}

// TestResurrectSessionIsStillBounded pins the ceiling BOS-984 put back. The
// conversion removed two accidental 120s bounds (the client's unary deadline
// and the daemon's write timeout); the lifecycle now imposes its own
// BootstrapTimeout so a resurrect wedged in the agent start still ends instead
// of pinning the stream forever.
//
// Driven by shrinking the bound rather than waiting ten minutes: the injected
// setup script never returns on its own, so only the deadline can end it.
func TestResurrectSessionIsStillBounded(t *testing.T) {
	t.Parallel()

	script := "pnpm install"
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })
	wt := &setupStreamWorktree{
		resurrectFn: func(ctx context.Context, _ gitpkg.ResurrectOpts) (*gitpkg.ResurrectResult, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-blocked:
				return &gitpkg.ResurrectResult{}, nil
			}
		},
	}
	h := newResurrectStreamHarnessWithBootstrapTimeout(t, wt, &script, 400*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	started := time.Now()
	out := h.resurrect(t, ctx)
	elapsed := time.Since(started)

	if out.err == nil {
		t.Fatal("a resurrect that never finishes must be ended by its own bound")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("resurrect ran %v before failing; the bootstrap bound is not being applied", elapsed)
	}
	if out.session != nil {
		t.Fatal("a timed-out resurrect must not deliver a terminal frame")
	}
	// Assert the CODE, not merely that something failed. The three failure
	// modes ResurrectSessionResponse documents are only distinguishable if the
	// daemon's own bound reports DeadlineExceeded; as CodeInternal it collapses
	// into the worktree-failure bucket and AC3 is unmet. Asserting "an error
	// occurred" passes either way, which is what let that slip.
	if got := connect.CodeOf(out.err); got != connect.CodeDeadlineExceeded {
		t.Fatalf("timed-out resurrect code = %v, want %v (a timeout must not read as a worktree failure)",
			got, connect.CodeDeadlineExceeded)
	}
}
