package plugin_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/migrate"
	"github.com/recurser/bossalib/models"
	sharedplugin "github.com/recurser/bossalib/plugin"
	"github.com/recurser/bossd/internal/claude"
	"github.com/recurser/bossd/internal/db"
	pluginpkg "github.com/recurser/bossd/internal/plugin"
	"github.com/recurser/bossd/internal/plugin/pluginharness"
	"github.com/recurser/bossd/internal/status"
)

// repairHarness bundles the daemon-side state the repair plugin needs to call
// back into: an in-memory SQLite DB with real stores, a claude.Runner whose
// subprocesses are fake_claude.sh, a DisplayTracker that SetRepairStatus mutates,
// and a HostServiceServer wired with all of them.
//
// Unlike autopilotHarness, the repair plugin reads session state via
// ListSessions (to evaluate isSessionRepairable) and writes IsRepairing via
// SetRepairStatus (which goes through DisplayTracker). Both require SetSessionDeps
// in addition to SetWorkflowDeps.
type repairHarness struct {
	t              *testing.T
	tmpDir         string
	workDir        string
	sessionID      string
	repoID         string
	db             *sql.DB
	repos          db.RepoStore
	sessions       db.SessionStore
	workflows      db.WorkflowStore
	chats          db.ClaudeChatStore
	displayTracker *status.DisplayTracker
	chatTracker    *status.Tracker
	hostService    *pluginpkg.HostServiceServer
	claudeRunner   claude.ClaudeRunner
	pluginBinPath  string
	pluginClient   *goplugin.Client // exposed for ReattachConfig and manual Kill in drain tests
	workflow       pluginpkg.WorkflowService
}

// repairHarnessOpts tunes harness construction. InitialState sets the session
// state machine value (typically AwaitingChecks) so isSessionRepairable
// returns true. FakeClaudeEnv injects env vars into the fake Claude script
// (FAKE_CLAUDE_LINES, FAKE_CLAUDE_SLEEP_MS, FAKE_CLAUDE_LOG_FILE, etc.).
type repairHarnessOpts struct {
	InitialState  machine.State
	FakeClaudeEnv map[string]string
}

// newRepairHarness builds a complete fixture. The harness registers
// t.Cleanup callbacks for every resource; callers only need to drive the
// returned WorkflowService client.
func newRepairHarness(t *testing.T, opts repairHarnessOpts) *repairHarness {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping Repair plugin integration test in short mode")
	}

	tmpDir := t.TempDir()
	workDir := filepath.Join(tmpDir, "worktree")
	if err := os.MkdirAll(filepath.Join(workDir, ".boss"), 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}

	_, thisFile, _, _ := runtime.Caller(0)
	fakeClaude := filepath.Join(filepath.Dir(thisFile), "testdata", "fake_claude.sh")
	if _, err := os.Stat(fakeClaude); err != nil {
		t.Fatalf("fake_claude.sh missing at %s: %v", fakeClaude, err)
	}

	sqlDB, err := db.OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	if err := migrate.Run(sqlDB, os.DirFS(pluginharness.MigrationsDir())); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	repos := db.NewRepoStore(sqlDB)
	sessions := db.NewSessionStore(sqlDB)
	chats := db.NewClaudeChatStore(sqlDB)
	workflows := db.NewWorkflowStore(sqlDB)

	ctx := context.Background()
	repo, err := repos.Create(ctx, db.CreateRepoParams{
		DisplayName:       "test-repo",
		LocalPath:         workDir,
		OriginURL:         "https://github.com/test/repair.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   tmpDir,
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}

	session, err := sessions.Create(ctx, db.CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "repair e2e session",
		Plan:         "docs/plans/test-plan.md",
		WorktreePath: workDir,
		BranchName:   "repair-e2e",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Session must be in a repairable state (AwaitingChecks, FixingChecks,
	// GreenDraft, or ReadyForReview) for isSessionRepairable to return true.
	// New sessions default to CreatingWorktree, which is not repairable.
	if opts.InitialState != 0 {
		stateInt := int(opts.InitialState)
		if _, err := sessions.Update(ctx, session.ID, db.UpdateSessionParams{State: &stateInt}); err != nil {
			t.Fatalf("update session state: %v", err)
		}
	}

	// Fake-Claude command factory. The repair plugin's CreateAttempt path
	// invokes claude.Runner.Start, which spawns this script in the session's
	// worktree. Per-test env vars come in via opts.FakeClaudeEnv.
	var baseEnv []string
	for k, v := range opts.FakeClaudeEnv {
		baseEnv = append(baseEnv, k+"="+v)
	}
	factory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, fakeClaude, args...)
		cmd.Env = append(os.Environ(), baseEnv...)
		return cmd
	}
	runner := claude.NewRunner(
		zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.Disabled),
		claude.WithCommandFactory(factory),
		claude.WithLogDir(filepath.Join(tmpDir, "claude-logs")),
	)
	if err := os.MkdirAll(filepath.Join(tmpDir, "claude-logs"), 0o755); err != nil {
		t.Fatalf("mkdir claude-logs: %v", err)
	}

	displayTracker := status.NewDisplayTracker()
	chatTracker := status.NewTracker()

	// VCS provider is unused by repair (it doesn't touch PRs directly) but
	// HostServiceServer requires non-nil for ListOpenPRs / GetCheckResults
	// dispatch; a zero-value stub is sufficient.
	hostService := pluginpkg.NewHostServiceServer(&testVCSProvider{})
	hostService.SetWorkflowDeps(workflows, sessions, chats, runner)
	hostService.SetSessionDeps(repos, sessions, displayTracker, chatTracker)

	binPath := pluginharness.BuildPlugin(t, "bossd-plugin-repair")

	pluginMap := goplugin.PluginSet{
		sharedplugin.PluginTypeWorkflow: &pluginpkg.WorkflowServiceGRPCPlugin{
			HostService: hostService,
		},
	}
	client := pluginharness.SpawnPlugin(t, binPath, pluginMap)

	rpcClient, err := client.Client()
	if err != nil {
		t.Fatalf("client.Client(): %v", err)
	}
	raw, err := rpcClient.Dispense(sharedplugin.PluginTypeWorkflow)
	if err != nil {
		t.Fatalf("dispense WorkflowService: %v", err)
	}
	workflow, ok := raw.(pluginpkg.WorkflowService)
	if !ok {
		t.Fatalf("dispensed type %T does not implement WorkflowService", raw)
	}

	return &repairHarness{
		t:              t,
		tmpDir:         tmpDir,
		workDir:        workDir,
		sessionID:      session.ID,
		repoID:         repo.ID,
		db:             sqlDB,
		repos:          repos,
		sessions:       sessions,
		workflows:      workflows,
		chats:          chats,
		displayTracker: displayTracker,
		chatTracker:    chatTracker,
		hostService:    hostService,
		claudeRunner:   runner,
		pluginBinPath:  binPath,
		pluginClient:   client,
		workflow:       workflow,
	}
}

// configJSON returns a minimal repair config. The plugin's cooldown_minutes
// field has a 1-minute floor (it's an int, not a duration); every test
// triggers at most one repair per session and completes well under a minute,
// so the floor is never exercised.
func (h *repairHarness) configJSON() string {
	cfg := map[string]any{
		"cooldown_minutes":       1,
		"poll_interval_seconds":  1,
		"sweep_interval_minutes": 10,
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		h.t.Fatalf("marshal config: %v", err)
	}
	return string(b)
}

// waitForRepairStatus polls displayTracker until IsRepairing matches `want` or
// the deadline expires.
func (h *repairHarness) waitForRepairStatus(want bool, timeout time.Duration) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		entry := h.displayTracker.Get(h.sessionID)
		got := entry != nil && entry.IsRepairing
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out after %s waiting for IsRepairing=%v; got entry=%+v", timeout, want, entry)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForWorkflow polls workflows.List until at least one workflow record
// exists for the harness's session or the deadline expires. Used to confirm
// the plugin reached CreateWorkflow (the first observable side effect of a
// triggered repair, and the most stable signal — IsRepairing may flip back
// to false before the test can observe it if fake_claude runs fast).
func (h *repairHarness) waitForWorkflow(ctx context.Context, timeout time.Duration) *models.Workflow {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		wfs, err := h.workflows.List(ctx)
		if err != nil {
			h.t.Fatalf("list workflows: %v", err)
		}
		for _, wf := range wfs {
			if wf.SessionID == h.sessionID {
				return wf
			}
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out after %s waiting for workflow; got %d", timeout, len(wfs))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// triggerRepair runs the StartWorkflow + NotifyStatusChange sequence that
// every trigger-path test begins with. Shared so each test keeps its focus
// on the behaviour being asserted.
func (h *repairHarness) triggerRepair(ctx context.Context, displayStatus bossanovav1.DisplayStatus, hasFailures bool) {
	h.t.Helper()
	if _, err := h.workflow.StartWorkflow(ctx, &bossanovav1.StartWorkflowRequest{
		SessionId:  h.sessionID,
		RepoId:     h.repoID,
		ConfigJson: h.configJSON(),
	}); err != nil {
		h.t.Fatalf("StartWorkflow: %v", err)
	}
	if err := h.workflow.NotifyStatusChange(ctx, h.sessionID, displayStatus, hasFailures); err != nil {
		h.t.Fatalf("NotifyStatusChange: %v", err)
	}
}

// TestE2E_Repair_TriggersOnFailing verifies the edge-triggered entry point
// of the repair plugin: NotifyStatusChange with FAILING display status on
// a repairable session causes the plugin to call SetRepairStatus(true) on
// the host, create a workflow record, spawn a Claude attempt with the
// /boss-repair prompt, and clear the repair flag when the attempt exits.
func TestE2E_Repair_TriggersOnFailing(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "fake-claude.log")
	h := newRepairHarness(t, repairHarnessOpts{
		InitialState: machine.AwaitingChecks,
		FakeClaudeEnv: map[string]string{
			"FAKE_CLAUDE_LINES":    "2",
			"FAKE_CLAUDE_SLEEP_MS": "100",
			"FAKE_CLAUDE_LOG_FILE": logPath,
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	h.triggerRepair(ctx, bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)

	// The most reliable side effect: a workflow record for our session.
	// CreateWorkflow is called before CreateAttempt, so if the workflow
	// exists the plugin definitely reached the repair path.
	wf := h.waitForWorkflow(ctx, 10*time.Second)
	if wf.SessionID != h.sessionID {
		t.Errorf("workflow session_id = %q, want %q", wf.SessionID, h.sessionID)
	}

	// IsRepairing should flip back to false after the attempt exits. Polling
	// for false rather than asserting the transient true state avoids a race
	// with fast fake_claude runs.
	h.waitForRepairStatus(false, 10*time.Second)

	// Check workflow status first. The plugin writes LastError verbatim when
	// CreateAttempt fails, so a non-completed status here tells us *why*
	// fake_claude never ran — much more useful than the downstream
	// "log file does not exist" we'd otherwise hit.
	finalWf, err := h.workflows.Get(ctx, wf.ID)
	if err != nil {
		t.Fatalf("get workflow: %v", err)
	}
	if string(finalWf.Status) != string(models.WorkflowStatusCompleted) {
		lastErr := ""
		if finalWf.LastError != nil {
			lastErr = *finalWf.LastError
		}
		t.Fatalf("workflow status = %q, want completed (last_error=%q)", finalWf.Status, lastErr)
	}

	// Verify a Claude attempt was actually spawned: the fake_claude log
	// records args + stdin every invocation. The stdin prompt is "/boss-repair"
	// (the skill name from repairConfig.skillName()). If the log is missing
	// despite a completed workflow, include status details so CI failures
	// surface whatever the race actually is.
	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		lastErr := ""
		if finalWf.LastError != nil {
			lastErr = *finalWf.LastError
		}
		t.Fatalf("read fake_claude log (workflow status=%q last_error=%q): %v", finalWf.Status, lastErr, err)
	}
	log := string(logBytes)
	if !strings.Contains(log, "/boss-repair") {
		t.Errorf("fake_claude log missing /boss-repair prompt:\n%s", log)
	}
}

// TestE2E_Repair_TriggersOnConflict verifies the plugin treats CONFLICT
// identically to FAILING — maybeRepair's OR-chain over DisplayStatus
// is one of the two load-bearing predicates (along with isSessionRepairable)
// that decides whether a notification triggers a repair. has_failures is
// false in the CONFLICT case (conflicts are orthogonal to CI status), so
// the test also pins that the trigger doesn't depend on has_failures.
//
// Only the trigger-side assertions are made here; the full repair lifecycle
// (workflow completion, fake_claude invocation, IsRepairing flip-back) is
// covered by TestE2E_Repair_TriggersOnFailing and not re-asserted.
func TestE2E_Repair_TriggersOnConflict(t *testing.T) {
	h := newRepairHarness(t, repairHarnessOpts{
		InitialState: machine.AwaitingChecks,
		FakeClaudeEnv: map[string]string{
			"FAKE_CLAUDE_LINES":    "2",
			"FAKE_CLAUDE_SLEEP_MS": "100",
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	h.triggerRepair(ctx, bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT, false)

	// CreateWorkflow is the first irreversible side effect; seeing a
	// workflow record for our session is proof the plugin passed every
	// guard inside maybeRepair (stopped/paused/cooldown/isSessionIdle/
	// isSessionRepairable/lastAttemptCommit) for a CONFLICT notification.
	if wf := h.waitForWorkflow(ctx, 10*time.Second); wf.SessionID != h.sessionID {
		t.Errorf("workflow session_id = %q, want %q", wf.SessionID, h.sessionID)
	}

	// Let the cycle drain so plugin Kill in t.Cleanup doesn't race the
	// in-flight SetRepairStatus(false) cleanup RPC.
	h.waitForRepairStatus(false, 10*time.Second)
}

// TestE2E_Repair_GracefulDrain verifies the ordering contract documented by
// the plugin's SIGTERM handler: when the plugin receives SIGTERM while a
// repair is in flight, its Shutdown hook must cancel the repair goroutine,
// wait for repairSession's defer to run SetRepairStatus(false) via a
// detached context, and only *then* allow the process to exit.
//
// Without the signal handler, SIGTERM would terminate the process
// immediately and IsRepairing would remain stuck at true — the session's
// UI would show "repairing" forever. So observing IsRepairing flip back to
// false within the shutdownTimeout window (1500ms in main.go, plus a small
// slack for the detached cleanup RPC) is the load-bearing signal.
//
// Fake Claude is configured to outlive any realistic test window (30 lines
// * 500ms ≈ 15s) so the cycle cannot complete on its own before SIGTERM
// fires. If this test ever passes because fake Claude exited naturally
// rather than because the signal handler worked, bump the natural runtime.
func TestE2E_Repair_GracefulDrain(t *testing.T) {
	h := newRepairHarness(t, repairHarnessOpts{
		InitialState: machine.AwaitingChecks,
		FakeClaudeEnv: map[string]string{
			"FAKE_CLAUDE_LINES":    "30",
			"FAKE_CLAUDE_SLEEP_MS": "500",
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	h.triggerRepair(ctx, bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)

	// Wait until repair is visibly in flight. Without this, SIGTERM could
	// arrive before the repair goroutine has even called SetRepairStatus(true),
	// rendering the post-SIGTERM assertion meaningless.
	h.waitForRepairStatus(true, 10*time.Second)

	// go-plugin's client.Kill does a graceful connection-close but does NOT
	// deliver SIGTERM to the plugin process. We need the real signal so the
	// repair plugin's SIGTERM hook (main.go:37) actually fires. The PID
	// travels out through ReattachConfig, mirroring the production path in
	// Host.Stop (host.go:222).
	rc := h.pluginClient.ReattachConfig()
	if rc == nil || rc.Pid == 0 {
		t.Fatalf("plugin ReattachConfig has no PID; cannot send SIGTERM")
	}
	sigtermAt := time.Now()
	if err := syscall.Kill(rc.Pid, syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM to plugin pid %d: %v", rc.Pid, err)
	}

	// shutdownTimeout is 1500ms and the cleanup RPC plus its 5s detached
	// timeout runs after that; 5s is a comfortable ceiling that still fails
	// loudly if the handler never fires.
	h.waitForRepairStatus(false, 5*time.Second)
	drainedAt := time.Now()

	drainDuration := drainedAt.Sub(sigtermAt)
	if drainDuration > 4*time.Second {
		t.Errorf("drain took %s after SIGTERM, want <4s (handler likely did not fire promptly)", drainDuration)
	}

	// Ordering proof: the plugin process must have still been alive when
	// SetRepairStatus(false) was RPC'd to the host, because once the process
	// is reaped there is nothing left to call back. `kill -0 <pid>` probes
	// liveness without delivering a signal; ESRCH means the process is gone,
	// while nil (or EPERM) means it was alive at this instant. We capture
	// liveness right after observing the cleanup so the gap between the
	// actual RPC and the probe is a few microseconds at most.
	if err := syscall.Kill(rc.Pid, 0); err != nil && err != syscall.EPERM {
		// The handler exits the process after shutdownTimeout elapses
		// (when goplugin.Serve returns), so a dead process here means the
		// cleanup RPC would have had no host to reach — which would have
		// tripped waitForRepairStatus above instead. Still worth checking:
		// if somehow both pass but the process is dead, the ordering is
		// wrong and the test should fail loudly.
		t.Errorf("plugin process gone immediately after drain observed (err=%v); SetRepairStatus(false) may have raced exit", err)
	}
}

// TestE2E_Repair_ForcePushesFix pins the plugin's post-success contract.
//
// Plan↔code note: the plan (and this bd task) asked for a mock Git manager
// that records a force-push after fake Claude succeeds — but the repair
// plugin does not touch git directly. The force-push itself lives inside
// the /boss-repair skill (the Claude subprocess), and claude.Runner is
// the only production-code seam between the plugin and Claude. Asserting
// the push from the plugin's side would require a new Git manager
// abstraction on HostService; that production-code change is tracked
// separately (see the sibling bd task filed alongside this test, matching
// the pattern used for 4tw on the Linear plugin).
//
// What this test *does* pin is the sequence of observable host-side effects
// after Claude reports success. When the initial session state is
// FixingChecks (the production case where repair is expected to help),
// the plugin must:
//  1. Drive the attempt to completion (workflow.status = "completed")
//  2. Clear the repair indicator (DisplayTracker.IsRepairing = false)
//  3. Fire FixComplete to advance the state machine out of FixingChecks
//
// If step 3 regresses, sessions would loop back to repair forever rather
// than advancing to AwaitingChecks for the next CI cycle.
func TestE2E_Repair_ForcePushesFix(t *testing.T) {
	h := newRepairHarness(t, repairHarnessOpts{
		InitialState: machine.FixingChecks,
		FakeClaudeEnv: map[string]string{
			"FAKE_CLAUDE_LINES":    "2",
			"FAKE_CLAUDE_SLEEP_MS": "100",
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	h.triggerRepair(ctx, bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING, true)

	wf := h.waitForWorkflow(ctx, 10*time.Second)

	// Wait for the full cycle to complete. Once IsRepairing is false, the
	// repairSession goroutine has finished all its RPCs (including the
	// FixComplete event fired from the cleanup context), so any post-success
	// side effects observable on the host are already settled.
	h.waitForRepairStatus(false, 10*time.Second)

	finalWf, err := h.workflows.Get(ctx, wf.ID)
	if err != nil {
		t.Fatalf("get workflow: %v", err)
	}
	if string(finalWf.Status) != string(models.WorkflowStatusCompleted) {
		t.Errorf("workflow status = %q, want completed (last_error=%v)", finalWf.Status, finalWf.LastError)
	}

	// FixComplete transitions FixingChecks → AwaitingChecks per the state
	// machine in lib/bossalib/machine/machine.go. This is the assertion that
	// would have caught "plugin succeeds but forgets to fire the event" —
	// the production bug class that most resembles the plan's missing
	// force-push.
	sess, err := h.sessions.Get(ctx, h.sessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.State != machine.AwaitingChecks {
		t.Errorf("session state = %v, want AwaitingChecks (FixComplete did not fire?)", sess.State)
	}
}
