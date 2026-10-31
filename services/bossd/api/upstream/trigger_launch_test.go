package upstream

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/recurser/bossalib/contract/cloud/sqlutil"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/dbtest"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/reflect/protoreflect"
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
	// acceptedID, when non-empty, is the id the accepted frame carries instead
	// of sessionID, so a test can tell which frame's id the launcher recorded.
	acceptedID string
	// settledWorktree, when non-empty, is the settled frame's worktree_path —
	// the repo checkout a zero-output session settles on — instead of a
	// per-session worktree.
	settledWorktree string
}

func (f *fakeTriggerCreator) StreamCreateSession(ctx context.Context, req *pb.CreateSessionRequest, emit func(*pb.CreateSessionResponse) error) error {
	f.calls.Add(1)
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	if f.started != nil {
		f.startOnce.Do(func() { close(f.started) })
	}

	acceptedID := f.sessionID
	if f.acceptedID != "" {
		acceptedID = f.acceptedID
	}
	accepted := &pb.Session{Id: acceptedID, Title: req.GetTitle()}
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
	worktree := "/tmp/wt/" + f.sessionID
	if f.settledWorktree != "" {
		worktree = f.settledWorktree
	}
	settled := &pb.Session{Id: f.sessionID, Title: req.GetTitle(), WorktreePath: worktree}
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

func (c *countingTriggerLaunches) Upsert(ctx context.Context, invocationID, triggerID string, staleBefore time.Time) (string, bool, error) {
	token, won, err := c.TriggerLaunchStore.Upsert(ctx, invocationID, triggerID, staleBefore)
	n := c.claims.Add(1)
	if c.onClaimN != nil {
		c.onClaimN(n)
	}
	return token, won, err
}

// triggerFixture is a launcher over a real in-memory daemon DB with one
// registered repo.
type triggerFixture struct {
	sqlDB    *sql.DB
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
		sqlDB:    sqlDB,
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

// TestLaunchTriggerSessionClassifiesEveryCreateSessionRequestField makes new
// session-create fields require an explicit decision for the trigger path.
func TestLaunchTriggerSessionClassifiesEveryCreateSessionRequestField(t *testing.T) {
	requestPolicy := map[protoreflect.Name]string{
		"repo_id":                  "mapped",
		"title":                    "mapped",
		"plan":                     "mapped",
		"base_branch":              "mapped",
		"agent_name":               "mapped",
		"model":                    "mapped",
		"effort":                   "mapped",
		"should_run_setup_command": "mapped",
		"is_zero_output":           "mapped",
		"detach":                   "fixed",
		"is_tmux_unattended":       "fixed",
		"defer_pr":                 "fixed",
		"force":                    "fixed",
		"pr_number":                "unused",
		"is_verify_only":           "unused", // Trigger sessions have no existing PR to adopt.
		"force_branch":             "unused",
		"is_quick_chat":            "unused",
		"branch_name":              "unused",
		"tracker_id":               "unused",
		"tracker_url":              "unused",
		"tracker_issue":            "unused",
		"tracker_source":           "unused",
		"account_id":               "unused",
	}
	requestFields := (&pb.CreateSessionRequest{}).ProtoReflect().Descriptor().Fields()
	for i := 0; i < requestFields.Len(); i++ {
		field := requestFields.Get(i)
		switch requestPolicy[field.Name()] {
		case "mapped", "fixed", "unused":
		default:
			t.Fatalf("classify the new CreateSessionRequest field %s for trigger launches: mapped, fixed or unused", field.Name())
		}
	}
	for name := range requestPolicy {
		if requestFields.ByName(name) == nil {
			t.Fatalf("remove stale CreateSessionRequest field %s from the trigger launch classification", name)
		}
	}

	commandPolicy := map[protoreflect.Name]string{
		"invocation_id":            "idempotency",
		"trigger_id":               "idempotency",
		"repo_origin_url":          "mapped",
		"title":                    "mapped",
		"prompt":                   "mapped",
		"agent_name":               "mapped",
		"model":                    "mapped",
		"effort":                   "mapped",
		"base_branch":              "mapped",
		"should_run_setup_command": "mapped",
		"is_zero_output":           "mapped",
	}
	cmd := &pb.LaunchTriggerSessionCommand{}
	command := cmd.ProtoReflect()
	commandFields := command.Descriptor().Fields()
	for i := 0; i < commandFields.Len(); i++ {
		field := commandFields.Get(i)
		switch commandPolicy[field.Name()] {
		case "idempotency", "mapped":
		default:
			t.Fatalf("classify the new LaunchTriggerSessionCommand field %s: idempotency or mapped", field.Name())
		}
		switch field.Kind() {
		case protoreflect.StringKind:
			value := "value-" + string(field.Name())
			if field.Name() == "repo_origin_url" {
				value = triggerTestOrigin
			}
			command.Set(field, protoreflect.ValueOfString(value))
		case protoreflect.BoolKind:
			// A zero-output launch clears base_branch; exercise its mapping
			// with an explicitly present false instead.
			command.Set(field, protoreflect.ValueOfBool(field.Name() != "is_zero_output"))
		default:
			t.Fatalf("trigger command field %s has unsupported test kind %s", field.FullName(), field.Kind())
		}
		if !command.Has(field) {
			t.Fatalf("test must populate command field %s, including optional presence", field.Name())
		}
	}
	for name := range commandPolicy {
		if commandFields.ByName(name) == nil {
			t.Fatalf("remove stale LaunchTriggerSessionCommand field %s from the trigger launch classification", name)
		}
	}

	fx := newTriggerFixture(t, &fakeTriggerCreator{sessionID: "sess-field-guard"})
	if _, _, err := fx.launcher.Launch(context.Background(), cmd); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	req := fx.creator.lastRequest()
	if req == nil {
		t.Fatal("Launch did not reach StreamCreateSession")
	}
	got := req.ProtoReflect()
	for i := 0; i < requestFields.Len(); i++ {
		field := requestFields.Get(i)
		t.Run(string(field.Name()), func(t *testing.T) {
			switch requestPolicy[field.Name()] {
			case "mapped":
				var want protoreflect.Value
				switch field.Name() {
				case "repo_id":
					want = protoreflect.ValueOfString(fx.repoID)
				case "plan":
					want = command.Get(commandFields.ByName("prompt"))
				default:
					source := commandFields.ByName(field.Name())
					if source == nil {
						t.Fatalf("mapped request field %s needs a command source", field.Name())
					}
					want = command.Get(source)
				}
				if got.Get(field).Interface() != want.Interface() {
					t.Errorf("mapped value = %v, want %v", got.Get(field), want)
				}
				// Non-optional false is the proto default and has no presence.
				if field.Name() != "is_zero_output" && !got.Has(field) {
					t.Error("mapped request field is unset")
				}
			case "fixed":
				if !got.Has(field) || !got.Get(field).Bool() {
					t.Error("fixed request field must be true")
				}
			case "unused":
				if got.Has(field) {
					t.Errorf("unused request field is set to %v", got.Get(field))
				}
			}
		})
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

// TestLaunchTriggerSessionForwardsRunSetupAndZeroOutput pins BOS-1477's
// launcher half: the trigger's Run setup command and Zero output settings reach
// the daemon CreateSessionRequest, a zero-output launch sends no base branch, and
// a trigger with neither set sends exactly what it did before.
func TestLaunchTriggerSessionForwardsRunSetupAndZeroOutput(t *testing.T) {
	off, on := false, true
	for _, tc := range []struct {
		name           string
		runSetup       *bool
		zeroOutput     *bool
		wantRunSetup   *bool
		wantZeroOutput bool
		wantBaseBranch string
	}{
		{name: "neither set keeps today's request", wantBaseBranch: "develop"},
		{name: "run setup off", runSetup: &off, wantRunSetup: &off, wantBaseBranch: "develop"},
		{name: "run setup on", runSetup: &on, wantRunSetup: &on, wantBaseBranch: "develop"},
		{name: "zero output drops the base branch", zeroOutput: &on, wantZeroOutput: true, wantBaseBranch: ""},
		{name: "zero output off keeps the base branch", zeroOutput: &off, wantBaseBranch: "develop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newTriggerFixture(t, &fakeTriggerCreator{sessionID: "sess-fwd"})
			cmd := triggerCmd("inv-fwd")
			cmd.ShouldRunSetupCommand = tc.runSetup
			cmd.IsZeroOutput = tc.zeroOutput
			if _, _, err := fx.launcher.Launch(context.Background(), cmd); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			req := fx.creator.lastRequest()
			switch {
			case tc.wantRunSetup == nil && req.ShouldRunSetupCommand != nil:
				t.Fatalf("should_run_setup_command = %v, want absent", req.GetShouldRunSetupCommand())
			case tc.wantRunSetup != nil && (req.ShouldRunSetupCommand == nil || req.GetShouldRunSetupCommand() != *tc.wantRunSetup):
				t.Fatalf("should_run_setup_command = %v (present=%v), want %v", req.GetShouldRunSetupCommand(), req.ShouldRunSetupCommand != nil, *tc.wantRunSetup)
			}
			if got := req.GetIsZeroOutput(); got != tc.wantZeroOutput {
				t.Fatalf("is_zero_output = %v, want %v", got, tc.wantZeroOutput)
			}
			if got := req.GetBaseBranch(); got != tc.wantBaseBranch {
				t.Fatalf("base_branch = %q, want %q", got, tc.wantBaseBranch)
			}
			if !req.GetDetach() || !req.GetIsTmuxUnattended() || !req.GetDeferPr() || !req.GetForce() {
				t.Fatalf("create must stay detached+unattended+defer_pr+force, got %+v", req)
			}
			if req.GetIsQuickChat() || req.PrNumber != nil || req.GetBranchName() != "" {
				t.Fatalf("trigger create must carry no quick-chat/PR/branch fields, got %+v", req)
			}
		})
	}
}

// TestLaunchTriggerSessionZeroOutput proves a zero-output trigger launch is
// recorded as launched rather than left stuck accepted: the daemon settles a
// zero-output session on the repo checkout, so the launcher's existing
// accepted-then-settled rule applies, and the budget-expiry path still records
// the accepted id so a retry replays instead of launching a second agent in the
// same checkout.
func TestLaunchTriggerSessionZeroOutput(t *testing.T) {
	zeroOutputCmd := func(invocationID string) *pb.LaunchTriggerSessionCommand {
		on := true
		cmd := triggerCmd(invocationID)
		cmd.IsZeroOutput = &on
		return cmd
	}

	t.Run("records and returns the settled session id", func(t *testing.T) {
		fx := newTriggerFixture(t, &fakeTriggerCreator{
			sessionID:       "sess-zero",
			acceptedID:      "sess-zero-accepted",
			settledWorktree: "/tmp/widgets",
		})
		res, code, err := fx.launcher.Launch(context.Background(), zeroOutputCmd("inv-zero"))
		if err != nil || code != pb.CommandResult_ERROR_CODE_UNSPECIFIED {
			t.Fatalf("Launch: code=%v err=%v", code, err)
		}
		if res.GetSessionId() != "sess-zero" || res.GetIsReplay() {
			t.Fatalf("result = %+v, want the settled session sess-zero, not a replay", res)
		}
		row, err := fx.launches.Get(context.Background(), "inv-zero")
		if err != nil || row == nil || row.SessionID != "sess-zero" {
			t.Fatalf("claim row = %+v, %v; want the settled session recorded", row, err)
		}
		if !fx.creator.lastRequest().GetIsZeroOutput() {
			t.Fatal("create request did not carry is_zero_output")
		}
	})

	t.Run("budget expiry after acceptance records the accepted id for replay", func(t *testing.T) {
		creator := &fakeTriggerCreator{sessionID: "sess-zb", outlastBudget: true}
		fx := newTriggerFixture(t, creator)
		fx.launcher.Budget = 20 * time.Millisecond
		_, code, err := fx.launcher.Launch(context.Background(), zeroOutputCmd("inv-zb"))
		if err == nil || code != pb.CommandResult_ERROR_CODE_FAILED_PRECONDITION {
			t.Fatalf("code=%v err=%v, want FAILED_PRECONDITION", code, err)
		}
		row, _ := fx.launches.Get(context.Background(), "inv-zb")
		if row == nil || row.SessionID != "sess-zb" {
			t.Fatalf("row = %+v, want the still-bootstrapping session recorded on the claim", row)
		}
		retry, _, err := fx.launcher.Launch(context.Background(), zeroOutputCmd("inv-zb"))
		if err != nil || retry.GetSessionId() != "sess-zb" || !retry.GetIsReplay() {
			t.Fatalf("retry = %+v, %v; want a replay of sess-zb", retry, err)
		}
		if n := creator.calls.Load(); n != 1 {
			t.Fatalf("creates = %d, want 1 (the retry must not launch a second zero-output agent)", n)
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

// failingTriggerLaunches exercises persistence failures against the real store.
type failingTriggerLaunches struct {
	db.TriggerLaunchStore
	setFailures int
	setCalls    int
	markError   error
}

func (s *failingTriggerLaunches) SetSession(ctx context.Context, invocationID, token, sessionID string) error {
	s.setCalls++
	if s.setFailures < 0 || s.setCalls <= s.setFailures {
		return errors.New("temporary SQLite failure")
	}
	return s.TriggerLaunchStore.SetSession(ctx, invocationID, token, sessionID)
}
func (s *failingTriggerLaunches) MarkAccepted(ctx context.Context, invocationID, token, sessionID string) error {
	if s.markError != nil {
		return s.markError
	}
	return s.TriggerLaunchStore.MarkAccepted(ctx, invocationID, token, sessionID)
}

func TestLaunchTriggerSessionDurableAcceptance(t *testing.T) {
	t.Run("settled launch replays after session writes fail", func(t *testing.T) {
		fx := newTriggerFixture(t, &fakeTriggerCreator{sessionID: "sess-durable"})
		failing := &failingTriggerLaunches{TriggerLaunchStore: fx.launches, setFailures: -1}
		fx.launcher.Launches = failing
		first, _, err := fx.launcher.Launch(context.Background(), triggerCmd("inv-durable"))
		if err != nil || first.GetSessionId() != "sess-durable" {
			t.Fatalf("launch = %v, %v", first, err)
		}
		if failing.setCalls != 3 {
			t.Fatalf("SetSession attempts = %d, want 3", failing.setCalls)
		}
		if _, err := fx.sqlDB.Exec("UPDATE trigger_launches SET created_at = ?", sqlutil.FormatTime(time.Now().Add(-time.Hour))); err != nil {
			t.Fatal(err)
		}
		second, _, err := fx.launcher.Launch(context.Background(), triggerCmd("inv-durable"))
		if err != nil || second.GetSessionId() != first.GetSessionId() || !second.GetIsReplay() {
			t.Fatalf("replay = %v, %v", second, err)
		}
		if fx.creator.calls.Load() != 1 {
			t.Fatal("replay created a duplicate session")
		}
		row, err := fx.launches.Get(context.Background(), "inv-durable")
		if err != nil || row == nil || row.SessionID != "sess-durable" {
			t.Fatalf("promoted row = %+v, %v", row, err)
		}
	})
	t.Run("transient session write is retried", func(t *testing.T) {
		fx := newTriggerFixture(t, &fakeTriggerCreator{sessionID: "sess-retry"})
		failing := &failingTriggerLaunches{TriggerLaunchStore: fx.launches, setFailures: 1}
		fx.launcher.Launches = failing
		if _, _, err := fx.launcher.Launch(context.Background(), triggerCmd("inv-retry")); err != nil {
			t.Fatal(err)
		}
		if failing.setCalls != 2 {
			t.Fatalf("SetSession attempts = %d, want 2", failing.setCalls)
		}
		replay, _, err := fx.launcher.Launch(context.Background(), triggerCmd("inv-retry"))
		if err != nil || !replay.GetIsReplay() || replay.GetSessionId() != "sess-retry" {
			t.Fatalf("replay = %v, %v", replay, err)
		}
		if fx.creator.calls.Load() != 1 {
			t.Fatal("retry created a duplicate")
		}
	})
	t.Run("crash after accept replays without repo resolution", func(t *testing.T) {
		fx := newTriggerFixture(t, &fakeTriggerCreator{sessionID: "never"})
		if _, err := fx.sqlDB.Exec(`INSERT INTO trigger_launches (invocation_id, trigger_id, claim_token, accepted_session_id, created_at) VALUES ('inv-crash', 'trg', 'old', 'sess-crash', ?)`, sqlutil.FormatTime(time.Now().Add(-time.Hour))); err != nil {
			t.Fatal(err)
		}
		cmd := triggerCmd("inv-crash")
		cmd.RepoOriginUrl = "https://github.com/missing/repo.git"
		replay, _, err := fx.launcher.Launch(context.Background(), cmd)
		if err != nil || !replay.GetIsReplay() || replay.GetSessionId() != "sess-crash" {
			t.Fatalf("replay = %v, %v", replay, err)
		}
		if fx.creator.calls.Load() != 0 {
			t.Fatal("crashed launch relaunched")
		}
	})
	t.Run("accepted id is durable before settle", func(t *testing.T) {
		creator := &fakeTriggerCreator{sessionID: "sess-gated", gate: make(chan struct{})}
		fx := newTriggerFixture(t, creator)
		done := make(chan error, 1)
		go func() { _, _, err := fx.launcher.Launch(context.Background(), triggerCmd("inv-gated")); done <- err }()
		defer func() {
			close(creator.gate)
			if err := <-done; err != nil {
				t.Error(err)
			}
		}()
		deadline := time.After(5 * time.Second)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			row, err := fx.launches.Get(context.Background(), "inv-gated")
			if err != nil {
				t.Fatal(err)
			}
			if row != nil && row.AcceptedSessionID == "sess-gated" {
				if row.SessionID != "" {
					t.Fatal("session settled while gate held")
				}
				break
			}
			select {
			case <-deadline:
				t.Fatal("accepted id was not persisted before settle")
			case <-ticker.C:
			}
		}
	})
	t.Run("accept persistence failure never aborts stream", func(t *testing.T) {
		for _, markErr := range []error{errors.New("temporary SQLite failure"), db.ErrTriggerLaunchClaimLost} {
			fx := newTriggerFixture(t, &fakeTriggerCreator{sessionID: "sess-mark"})
			fx.launcher.Launches = &failingTriggerLaunches{TriggerLaunchStore: fx.launches, markError: markErr}
			var logs bytes.Buffer
			fx.launcher.Logger = zerolog.New(&logs)
			res, _, err := fx.launcher.Launch(context.Background(), triggerCmd("inv-mark"))
			if err != nil || res.GetSessionId() != "sess-mark" {
				t.Fatalf("launch = %v, %v", res, err)
			}
			wantMessage := "record accepted session on claim"
			if errors.Is(markErr, db.ErrTriggerLaunchClaimLost) {
				wantMessage = "claim taken over mid-create"
			}
			if !strings.Contains(logs.String(), wantMessage) || !strings.Contains(logs.String(), `"level":"error"`) {
				t.Fatalf("acceptance error not logged: %s", logs.String())
			}
		}
	})
}
