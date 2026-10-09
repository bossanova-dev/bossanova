package upstream

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/dbtest"
	"github.com/rs/zerolog"
)

const triggerTestOrigin = "https://github.com/acme/widgets.git"

// fakeTriggerCreator is a StreamCreateSessioner that replays the BOS-720
// bootstrapping shape: an accepted `created` frame (no worktree path yet), then
// the settled one (worktree path populated). Hooks let a test hold the create
// open or fail it.
type fakeTriggerCreator struct {
	mu       sync.Mutex
	requests []*pb.CreateSessionRequest
	calls    atomic.Int32
	// sessionID is the id the create settles on.
	sessionID string
	// gate, when non-nil, holds the create open after the accepted frame until
	// it is closed.
	gate chan struct{}
	// started is closed (once) when the first create begins.
	started   chan struct{}
	startOnce sync.Once
	// err, when non-nil, fails the create after the accepted frame.
	err error
	// skipSettled ends the create successfully after only the accepted frame.
	skipSettled bool
	// outlastBudget holds the create open after the accepted frame until its
	// context expires, as a bootstrap still running at the deadline does.
	outlastBudget bool
	// ctxErr records the create context's error when the create finished.
	ctxErr error
}

func (f *fakeTriggerCreator) StreamCreateSession(ctx context.Context, req *pb.CreateSessionRequest, emit func(*pb.CreateSessionResponse) error) error {
	f.calls.Add(1)
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	if f.started != nil {
		f.startOnce.Do(func() { close(f.started) })
	}

	accepted := &pb.Session{Id: f.sessionID, Title: req.GetTitle()}
	if err := emit(&pb.CreateSessionResponse{Event: &pb.CreateSessionResponse_SessionCreated{
		SessionCreated: &pb.SessionCreated{Session: accepted},
	}}); err != nil {
		return err
	}
	if f.gate != nil {
		<-f.gate
	}
	if f.outlastBudget {
		<-ctx.Done()
		return ctx.Err()
	}
	f.mu.Lock()
	f.ctxErr = ctx.Err()
	f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if f.skipSettled {
		return nil
	}
	settled := &pb.Session{Id: f.sessionID, Title: req.GetTitle(), WorktreePath: "/tmp/wt/" + f.sessionID}
	return emit(&pb.CreateSessionResponse{Event: &pb.CreateSessionResponse_SessionCreated{
		SessionCreated: &pb.SessionCreated{Session: settled},
	}})
}

func (f *fakeTriggerCreator) lastRequest() *pb.CreateSessionRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return nil
	}
	return f.requests[len(f.requests)-1]
}

// countingTriggerLaunches wraps the real store so a test can observe how many
// launches have reached their claim.
type countingTriggerLaunches struct {
	db.TriggerLaunchStore
	claims   atomic.Int32
	onClaimN func(n int32)
}

func (c *countingTriggerLaunches) Claim(ctx context.Context, invocationID, triggerID string, staleBefore time.Time) (bool, error) {
	won, err := c.TriggerLaunchStore.Claim(ctx, invocationID, triggerID, staleBefore)
	n := c.claims.Add(1)
	if c.onClaimN != nil {
		c.onClaimN(n)
	}
	return won, err
}

// triggerFixture is a launcher over a real in-memory daemon DB with one
// registered repo.
type triggerFixture struct {
	launches *db.SQLiteTriggerLaunchStore
	repoID   string
	launcher *TriggerSessionLauncher
	creator  *fakeTriggerCreator
}

func newTriggerFixture(t *testing.T, creator *fakeTriggerCreator) *triggerFixture {
	t.Helper()
	sqlDB := dbtest.New(t)
	repos := db.NewRepoStore(sqlDB)
	repo, err := repos.Create(context.Background(), db.CreateRepoParams{
		DisplayName:       "widgets",
		LocalPath:         "/tmp/widgets",
		OriginURL:         triggerTestOrigin,
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	launches := db.NewTriggerLaunchStore(sqlDB)
	return &triggerFixture{
		launches: launches,
		repoID:   repo.ID,
		creator:  creator,
		launcher: &TriggerSessionLauncher{
			Launches:     launches,
			Repos:        repos,
			Creator:      creator,
			Logger:       zerolog.Nop(),
			PollInterval: 5 * time.Millisecond,
		},
	}
}

func triggerCmd(invocationID string) *pb.LaunchTriggerSessionCommand {
	model := "opus"
	return &pb.LaunchTriggerSessionCommand{
		InvocationId:  invocationID,
		TriggerId:     "trg-1",
		RepoOriginUrl: triggerTestOrigin,
		Title:         "Trigger: nightly triage",
		Prompt:        "/boss-build triage the inbox",
		AgentName:     "codex",
		Model:         &model,
		BaseBranch:    "develop",
	}
}

// TestLaunchTriggerSession covers the happy path end to end through the
// command dispatcher: one LaunchTriggerSessionCommand for a registered repo
// creates exactly one unattended, detached, defer_pr session and replies with
// its id in CommandResult{launch_trigger_session}.
func TestLaunchTriggerSession(t *testing.T) {
	t.Run("creates one unattended session and returns its id", func(t *testing.T) {
		fx := newTriggerFixture(t, &fakeTriggerCreator{sessionID: "sess-1"})
		client := newDispatcherClient(&CommandHandlerAdapter{Triggers: fx.launcher}, nil, nil)
		out := make(chan *pb.DaemonEvent, 4)

		if ev := client.dispatchCommand(context.Background(), &pb.OrchestratorCommand{
			CommandId: "c-launch",
			Cmd:       &pb.OrchestratorCommand_LaunchTriggerSession{LaunchTriggerSession: triggerCmd("inv-1")},
		}, out); ev != nil {
			t.Fatalf("expected nil synchronous result for async launch, got %+v", ev)
		}
		res := recvEvent(t, out).GetResult()
		if res == nil || !res.GetOk() || res.GetCommandId() != "c-launch" {
			t.Fatalf("expected ok result for c-launch, got %+v", res)
		}
		got := res.GetLaunchTriggerSession()
		if got.GetSessionId() != "sess-1" || got.GetIsReplay() {
			t.Fatalf("payload = %+v, want session sess-1 and is_replay=false", got)
		}

		if n := fx.creator.calls.Load(); n != 1 {
			t.Fatalf("creates = %d, want 1", n)
		}
		req := fx.creator.lastRequest()
		if req.GetRepoId() != fx.repoID || req.GetPlan() != "/boss-build triage the inbox" ||
			req.GetTitle() != "Trigger: nightly triage" || req.GetBaseBranch() != "develop" ||
			req.GetAgentName() != "codex" || req.GetModel() != "opus" || req.Effort != nil {
			t.Fatalf("create request fields = %+v", req)
		}
		if !req.GetDetach() || !req.GetIsTmuxUnattended() || !req.GetDeferPr() || !req.GetForce() {
			t.Fatalf("create must be detached+unattended+defer_pr+force, got %+v", req)
		}
		if req.GetIsQuickChat() || req.TrackerId != nil || req.PrNumber != nil {
			t.Fatalf("trigger create must carry no quick-chat/tracker/PR fields, got %+v", req)
		}

		row, err := fx.launches.Get(context.Background(), "inv-1")
		if err != nil || row == nil || row.SessionID != "sess-1" || row.TriggerID != "trg-1" {
			t.Fatalf("claim row = %+v, %v; want session sess-1 recorded", row, err)
		}
	})

	t.Run("empty title falls back to the trigger id", func(t *testing.T) {
		fx := newTriggerFixture(t, &fakeTriggerCreator{sessionID: "sess-t"})
		cmd := triggerCmd("inv-title")
		cmd.Title = ""
		if _, _, err := fx.launcher.Launch(context.Background(), cmd); err != nil {
			t.Fatalf("Launch: %v", err)
		}
		if got := fx.creator.lastRequest().GetTitle(); got != "Trigger: trg-1" {
			t.Fatalf("title = %q, want %q", got, "Trigger: trg-1")
		}
	})

	t.Run("a started create outlives the command context", func(t *testing.T) {
		creator := &fakeTriggerCreator{sessionID: "sess-late", gate: make(chan struct{}), started: make(chan struct{})}
		fx := newTriggerFixture(t, creator)
		ctx, cancel := context.WithCancel(context.Background())

		done := make(chan error, 1)
		go func() {
			_, _, err := fx.launcher.Launch(ctx, triggerCmd("inv-late"))
			done <- err
		}()
		<-creator.started
		cancel() // bosso's dispatch deadline passes mid-create
		close(creator.gate)
		if err := <-done; err != nil {
			t.Fatalf("Launch: %v", err)
		}
		if creator.ctxErr != nil {
			t.Fatalf("create context was cancelled with the command context: %v", creator.ctxErr)
		}
		row, _ := fx.launches.Get(context.Background(), "inv-late")
		if row == nil || row.SessionID != "sess-late" {
			t.Fatalf("session id not recorded after the command context ended: %+v", row)
		}
	})
}

// TestLaunchTriggerSessionIdempotent proves AC5's "exactly once despite
// retries": a replay of the same invocation returns the original session with
// is_replay=true and creates nothing, and two concurrent launches with one
// invocation id create exactly one session.
func TestLaunchTriggerSessionIdempotent(t *testing.T) {
	t.Run("replay returns the same session and creates nothing", func(t *testing.T) {
		fx := newTriggerFixture(t, &fakeTriggerCreator{sessionID: "sess-r"})
		ctx := context.Background()

		first, _, err := fx.launcher.Launch(ctx, triggerCmd("inv-r"))
		if err != nil || first.GetSessionId() != "sess-r" || first.GetIsReplay() {
			t.Fatalf("first launch = %+v, %v", first, err)
		}
		second, _, err := fx.launcher.Launch(ctx, triggerCmd("inv-r"))
		if err != nil {
			t.Fatalf("replay: %v", err)
		}
		if second.GetSessionId() != "sess-r" || !second.GetIsReplay() {
			t.Fatalf("replay = %+v, want sess-r with is_replay=true", second)
		}
		if n := fx.creator.calls.Load(); n != 1 {
			t.Fatalf("creates = %d, want 1 (the replay created a session)", n)
		}
	})

	t.Run("concurrent launches with one id create exactly one session", func(t *testing.T) {
		creator := &fakeTriggerCreator{sessionID: "sess-c", gate: make(chan struct{})}
		fx := newTriggerFixture(t, creator)
		// Hold the winner's create open until BOTH launches have reached their
		// claim, so the loser's claim really races an in-flight launch.
		var release sync.Once
		counting := &countingTriggerLaunches{TriggerLaunchStore: fx.launches}
		counting.onClaimN = func(n int32) {
			if n >= 2 {
				release.Do(func() { close(creator.gate) })
			}
		}
		fx.launcher.Launches = counting

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		results := make(chan *pb.LaunchTriggerSessionResult, 2)
		errs := make(chan error, 2)
		for range 2 {
			go func() {
				res, _, err := fx.launcher.Launch(ctx, triggerCmd("inv-c"))
				if err != nil {
					errs <- err
					return
				}
				results <- res
			}()
		}

		var got []*pb.LaunchTriggerSessionResult
		for range 2 {
			select {
			case res := <-results:
				got = append(got, res)
			case err := <-errs:
				t.Fatalf("concurrent launch failed: %v", err)
			case <-ctx.Done():
				release.Do(func() { close(creator.gate) })
				t.Fatal("concurrent launches did not finish")
			}
		}
		if n := creator.calls.Load(); n != 1 {
			t.Fatalf("concurrent launches created %d sessions, want exactly 1", n)
		}
		replays := 0
		for _, res := range got {
			if res.GetSessionId() != "sess-c" {
				t.Fatalf("launch returned session %q, want sess-c", res.GetSessionId())
			}
			if res.GetIsReplay() {
				replays++
			}
		}
		if replays != 1 {
			t.Fatalf("replays = %d, want exactly 1 of the 2 launches", replays)
		}
	})
}

// TestLaunchTriggerSessionErrors covers the typed failure modes: malformed
// commands, an unregistered repo, and a failed create — which must release its
// claim so a retry can launch.
func TestLaunchTriggerSessionErrors(t *testing.T) {
	t.Run("missing required fields are invalid", func(t *testing.T) {
		fx := newTriggerFixture(t, &fakeTriggerCreator{sessionID: "never"})
		for name, mutate := range map[string]func(*pb.LaunchTriggerSessionCommand){
			"invocation_id":   func(c *pb.LaunchTriggerSessionCommand) { c.InvocationId = "" },
			"repo_origin_url": func(c *pb.LaunchTriggerSessionCommand) { c.RepoOriginUrl = " " },
			"prompt":          func(c *pb.LaunchTriggerSessionCommand) { c.Prompt = "" },
		} {
			cmd := triggerCmd("inv-bad")
			mutate(cmd)
			_, code, err := fx.launcher.Launch(context.Background(), cmd)
			if err == nil || code != pb.CommandResult_ERROR_CODE_INVALID_ARGUMENT {
				t.Errorf("missing %s: code=%v err=%v, want INVALID_ARGUMENT", name, code, err)
			}
		}
		if n := fx.creator.calls.Load(); n != 0 {
			t.Fatalf("invalid commands created %d sessions", n)
		}
	})

	t.Run("unknown repo origin is not found", func(t *testing.T) {
		fx := newTriggerFixture(t, &fakeTriggerCreator{sessionID: "never"})
		client := newDispatcherClient(&CommandHandlerAdapter{Triggers: fx.launcher}, nil, nil)
		out := make(chan *pb.DaemonEvent, 4)
		cmd := triggerCmd("inv-unknown")
		cmd.RepoOriginUrl = "https://github.com/acme/elsewhere.git"
		client.dispatchCommand(context.Background(), &pb.OrchestratorCommand{
			CommandId: "c-unknown",
			Cmd:       &pb.OrchestratorCommand_LaunchTriggerSession{LaunchTriggerSession: cmd},
		}, out)
		res := recvEvent(t, out).GetResult()
		if res.GetOk() || res.GetErrorCode() != pb.CommandResult_ERROR_CODE_NOT_FOUND {
			t.Fatalf("result = %+v, want ok=false NOT_FOUND", res)
		}
		if want := "repository https://github.com/acme/elsewhere.git is not registered on this daemon"; !strings.Contains(res.GetError(), want) {
			t.Fatalf("error = %q, want it to contain %q", res.GetError(), want)
		}
		if row, _ := fx.launches.Get(context.Background(), "inv-unknown"); row != nil {
			t.Fatalf("an unknown repo left a claim row: %+v", row)
		}
		if n := fx.creator.calls.Load(); n != 0 {
			t.Fatalf("unknown repo created %d sessions", n)
		}
	})

	t.Run("create failure is a failed precondition and releases the claim", func(t *testing.T) {
		creator := &fakeTriggerCreator{sessionID: "sess-f", err: errors.New("worktree setup failed")}
		fx := newTriggerFixture(t, creator)
		client := newDispatcherClient(&CommandHandlerAdapter{Triggers: fx.launcher}, nil, nil)
		out := make(chan *pb.DaemonEvent, 4)
		client.dispatchCommand(context.Background(), &pb.OrchestratorCommand{
			CommandId: "c-fail",
			Cmd:       &pb.OrchestratorCommand_LaunchTriggerSession{LaunchTriggerSession: triggerCmd("inv-f")},
		}, out)
		res := recvEvent(t, out).GetResult()
		if res.GetOk() || res.GetErrorCode() != pb.CommandResult_ERROR_CODE_FAILED_PRECONDITION {
			t.Fatalf("result = %+v, want ok=false FAILED_PRECONDITION", res)
		}
		if !strings.Contains(res.GetError(), "worktree setup failed") {
			t.Fatalf("error = %q, want the daemon's message", res.GetError())
		}
		if row, _ := fx.launches.Get(context.Background(), "inv-f"); row != nil {
			t.Fatalf("failed create left its claim behind: %+v", row)
		}

		// The retry can launch.
		creator.err = nil
		retry, _, err := fx.launcher.Launch(context.Background(), triggerCmd("inv-f"))
		if err != nil || retry.GetSessionId() != "sess-f" || retry.GetIsReplay() {
			t.Fatalf("retry = %+v, %v; want a fresh launch of sess-f", retry, err)
		}
		if n := creator.calls.Load(); n != 2 {
			t.Fatalf("creates = %d, want 2 (failed + retried)", n)
		}
	})

	t.Run("a create that never settles is a failed precondition", func(t *testing.T) {
		fx := newTriggerFixture(t, &fakeTriggerCreator{sessionID: "sess-a", skipSettled: true})
		_, code, err := fx.launcher.Launch(context.Background(), triggerCmd("inv-a"))
		if err == nil || code != pb.CommandResult_ERROR_CODE_FAILED_PRECONDITION {
			t.Fatalf("code=%v err=%v, want FAILED_PRECONDITION (the accepted frame is not terminal)", code, err)
		}
		if row, _ := fx.launches.Get(context.Background(), "inv-a"); row != nil {
			t.Fatalf("unsettled create left its claim behind: %+v", row)
		}
	})

	t.Run("a create that outlasts its budget after acceptance keeps the session for replay", func(t *testing.T) {
		creator := &fakeTriggerCreator{sessionID: "sess-b", outlastBudget: true}
		fx := newTriggerFixture(t, creator)
		fx.launcher.Budget = 20 * time.Millisecond
		_, code, err := fx.launcher.Launch(context.Background(), triggerCmd("inv-b"))
		if err == nil || code != pb.CommandResult_ERROR_CODE_FAILED_PRECONDITION {
			t.Fatalf("code=%v err=%v, want FAILED_PRECONDITION", code, err)
		}
		row, _ := fx.launches.Get(context.Background(), "inv-b")
		if row == nil || row.SessionID != "sess-b" {
			t.Fatalf("row = %+v, want the still-bootstrapping session recorded on the claim", row)
		}
		retry, _, err := fx.launcher.Launch(context.Background(), triggerCmd("inv-b"))
		if err != nil || retry.GetSessionId() != "sess-b" || !retry.GetIsReplay() {
			t.Fatalf("retry = %+v, %v; want a replay of sess-b", retry, err)
		}
		if n := creator.calls.Load(); n != 1 {
			t.Fatalf("creates = %d, want 1 (the retry must not launch a duplicate)", n)
		}
	})

	t.Run("unwired launcher fails rather than dropping the command", func(t *testing.T) {
		_, _, err := (&CommandHandlerAdapter{}).LaunchTriggerSession(context.Background(), triggerCmd("inv-x"))
		if err == nil {
			t.Fatal("expected an error from an adapter with no launcher")
		}
	})
}

// TestLaunchTriggerSessionDoesNotBlockReader proves the dispatcher runs the
// launch asynchronously: a launch still waiting on worktree setup must not wedge
// the single-threaded command reader, so a command sent right behind it (and,
// by the same token, the heartbeat that loop services) completes first.
func TestLaunchTriggerSessionDoesNotBlockReader(t *testing.T) {
	release := make(chan struct{})
	fake := &fakeCommandHandler{
		session:      &pb.Session{Id: "s1"},
		launchBlock:  release,
		launchResult: &pb.LaunchTriggerSessionResult{SessionId: "sess-slow"},
	}
	client := newDispatcherClient(fake, nil, nil)
	out := make(chan *pb.DaemonEvent, 4)
	ctx := context.Background()

	client.handleCommand(ctx, &pb.OrchestratorCommand{
		CommandId: "slow-launch",
		Cmd:       &pb.OrchestratorCommand_LaunchTriggerSession{LaunchTriggerSession: triggerCmd("inv-slow")},
	}, out)
	client.handleCommand(ctx, &pb.OrchestratorCommand{
		CommandId: "fast",
		Cmd:       &pb.OrchestratorCommand_Stop{Stop: &pb.StopSessionCommand{SessionId: "s1"}},
	}, out)

	ev := recvEvent(t, out)
	if got := ev.GetResult().GetCommandId(); got != "fast" {
		t.Fatalf("expected fast command result first, got %q (the launch wedged the reader)", got)
	}

	close(release)
	ev = recvEvent(t, out)
	if got := ev.GetResult().GetCommandId(); got != "slow-launch" {
		t.Fatalf("expected the launch result after release, got %q", got)
	}
	if got := ev.GetResult().GetLaunchTriggerSession().GetSessionId(); got != "sess-slow" {
		t.Fatalf("launch payload session = %q, want sess-slow", got)
	}
}
