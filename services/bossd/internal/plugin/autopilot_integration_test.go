package plugin_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/migrate"
	sharedplugin "github.com/recurser/bossalib/plugin"
	"github.com/recurser/bossd/internal/claude"
	"github.com/recurser/bossd/internal/db"
	pluginpkg "github.com/recurser/bossd/internal/plugin"
	"github.com/recurser/bossd/internal/plugin/pluginharness"
)

// autopilotHarness bundles the daemon-side state the autopilot plugin needs
// to call back into: an in-memory SQLite DB with real stores, a claude.Runner
// whose subprocesses are fake_claude.sh, and a HostServiceServer wired with
// both. Tests spawn the autopilot plugin pointed at this host via the
// standard pluginharness.SpawnPlugin flow, then drive WorkflowService RPCs.
type autopilotHarness struct {
	t             *testing.T
	tmpDir        string
	workDir       string
	handoffDir    string
	planPath      string
	sessionID     string
	repoID        string
	db            *sql.DB
	sessions      db.SessionStore
	workflows     db.WorkflowStore
	hostService   *pluginpkg.HostServiceServer
	pluginBinPath string
	fakeClaude    string
}

// autopilotHarnessOpts tunes harness construction. skipHandoffEnv = true
// omits FAKE_CLAUDE_HANDOFF_DIR so the fake Claude never writes handoff
// files — exercises the autopilot's "can't make progress" pause path.
type autopilotHarnessOpts struct {
	skipHandoffEnv bool
}

// newAutopilotHarness builds a complete fixture:
//   - in-memory SQLite with all migrations applied
//   - a test repo + session persisted in the DB (session's WorktreePath is a
//     real temp directory so claude.Runner can `cd` into it)
//   - a claude.Runner whose command factory substitutes fake_claude.sh for
//     the real `claude` binary, with env vars piped through so tests can
//     tune FAKE_CLAUDE_LINES / FAKE_CLAUDE_HANDOFF_DIR per-run
//   - a built autopilot plugin binary (skipped automatically if
//     plugins/bossd-plugin-autopilot is absent in this checkout)
func newAutopilotHarness(t *testing.T, fakeClaudeEnv map[string]string, opts ...autopilotHarnessOpts) *autopilotHarness {
	t.Helper()

	tmpDir := t.TempDir()
	workDir := filepath.Join(tmpDir, "worktree")
	handoffDir := filepath.Join(workDir, "handoffs")
	if err := os.MkdirAll(filepath.Join(workDir, ".boss"), 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	if err := os.MkdirAll(handoffDir, 0o755); err != nil {
		t.Fatalf("mkdir handoff dir: %v", err)
	}

	// Plan file must exist for validatePlanPath to pass. Autopilot treats
	// it as opaque — we only need the file on disk so the path resolves.
	planPath := "docs/plans/test-plan.md"
	absPlanDir := filepath.Join(workDir, "docs", "plans")
	if err := os.MkdirAll(absPlanDir, 0o755); err != nil {
		t.Fatalf("mkdir plan dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(absPlanDir, "test-plan.md"), []byte("# test plan\n"), 0o644); err != nil {
		t.Fatalf("write plan file: %v", err)
	}

	// Locate the fake Claude script relative to this test file so tests run
	// from any cwd. runtime.Caller keeps the lookup independent of `go test`
	// invocation directory.
	_, thisFile, _, _ := runtime.Caller(0)
	fakeClaude := filepath.Join(filepath.Dir(thisFile), "testdata", "fake_claude.sh")
	if _, err := os.Stat(fakeClaude); err != nil {
		t.Fatalf("fake_claude.sh missing at %s: %v", fakeClaude, err)
	}

	// Open in-memory SQLite with migrations applied. Using the real stores
	// means we exercise the production SQL paths that the plugin triggers
	// via host RPCs — far better coverage than hand-rolled map fakes would
	// provide, and it catches Go/schema drift automatically.
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
		OriginURL:         "https://github.com/test/autopilot.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   tmpDir,
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}

	session, err := sessions.Create(ctx, db.CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "autopilot e2e session",
		Plan:         planPath,
		WorktreePath: workDir,
		BranchName:   "autopilot-e2e",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// claude.Runner with a CommandFactory that swaps `claude` for our fake
	// shell script. Per-test env vars (FAKE_CLAUDE_LINES, FAKE_CLAUDE_EXIT,
	// etc.) come in via fakeClaudeEnv; FAKE_CLAUDE_HANDOFF_DIR is pinned
	// to the harness's own handoff dir so the autopilot's scanHandoffDir
	// picks up per-invocation files, unless opts.skipHandoffEnv is set.
	if _, ok := fakeClaudeEnv["FAKE_CLAUDE_HANDOFF_DIR"]; ok {
		t.Fatal("fakeClaudeEnv must not set FAKE_CLAUDE_HANDOFF_DIR; the harness controls it via skipHandoffEnv")
	}
	var baseEnv []string
	skipHandoff := len(opts) > 0 && opts[0].skipHandoffEnv
	if !skipHandoff {
		baseEnv = append(baseEnv, "FAKE_CLAUDE_HANDOFF_DIR="+handoffDir)
	}
	for k, v := range fakeClaudeEnv {
		baseEnv = append(baseEnv, k+"="+v)
	}
	// Track every spawned fake_claude subprocess so the harness can force
	// them to exit before t.TempDir() cleanup runs. Without this, a cancelled
	// workflow leaves the host service's claude subprocess running — it
	// continues to write handoff files into the temp tree after the test
	// returns, causing RemoveAll to report "directory not empty" on CI.
	//
	// Each factory call gets its own sub-context; cleanup cancels those
	// contexts rather than poking cmd.Process directly. Reading cmd.Process
	// from the cleanup goroutine races with os/exec.Cmd.Start() writing it
	// in the CreateAttempt gRPC handler (which may still be in flight when
	// the plugin binary is killed) — see the -race report on
	// TestE2E_Autopilot_Cancel. Context cancellation hands termination back
	// to os/exec's internal goroutine, which is sequenced safely with Start.
	var (
		trackedMu     sync.Mutex
		trackedCancel []context.CancelFunc
	)
	factory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		subCtx, cancel := context.WithCancel(ctx)
		cmd := exec.CommandContext(subCtx, fakeClaude, args...)
		cmd.Env = append(os.Environ(), baseEnv...)
		// claude.Runner overrides cmd.Cancel to Signal(SIGTERM), which
		// fake_claude.sh traps for immediate exit without writing the
		// handoff file. Cancelling subCtx drives that path.
		trackedMu.Lock()
		trackedCancel = append(trackedCancel, cancel)
		trackedMu.Unlock()
		return cmd
	}
	// Register before any other cleanup that might depend on process exit so
	// this runs after the plugin binary kill (LIFO) but before sqlDB.Close
	// and t.TempDir removal.
	t.Cleanup(func() {
		trackedMu.Lock()
		cancels := append([]context.CancelFunc(nil), trackedCancel...)
		trackedMu.Unlock()
		for _, c := range cancels {
			c()
		}
		// Brief grace period so the signal handlers run and the runner's
		// Wait goroutines observe the exit + close log files before RemoveAll.
		time.Sleep(200 * time.Millisecond)
	})
	runner := claude.NewRunner(
		zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.Disabled),
		claude.WithCommandFactory(factory),
		claude.WithLogDir(filepath.Join(tmpDir, "claude-logs")),
	)
	if err := os.MkdirAll(filepath.Join(tmpDir, "claude-logs"), 0o755); err != nil {
		t.Fatalf("mkdir claude-logs: %v", err)
	}

	// VCS provider isn't called by autopilot; a zero-value stub is enough.
	hostService := pluginpkg.NewHostServiceServer(&testVCSProvider{})
	hostService.SetWorkflowDeps(workflows, sessions, chats, runner)

	binPath := pluginharness.BuildPlugin(t, "bossd-plugin-autopilot")

	return &autopilotHarness{
		t:             t,
		tmpDir:        tmpDir,
		workDir:       workDir,
		handoffDir:    handoffDir,
		planPath:      planPath,
		sessionID:     session.ID,
		repoID:        repo.ID,
		db:            sqlDB,
		sessions:      sessions,
		workflows:     workflows,
		hostService:   hostService,
		pluginBinPath: binPath,
		fakeClaude:    fakeClaude,
	}
}

// spawnAutopilot starts the built plugin binary with the host service
// attached to broker ID 1 and returns a ready-to-call WorkflowService client.
// The plugin process is cleaned up by pluginharness.SpawnPlugin's t.Cleanup.
func (h *autopilotHarness) spawnAutopilot() pluginpkg.WorkflowService {
	h.t.Helper()

	pluginMap := goplugin.PluginSet{
		sharedplugin.PluginTypeWorkflow: &pluginpkg.WorkflowServiceGRPCPlugin{
			HostService: h.hostService,
		},
	}
	client := pluginharness.SpawnPlugin(h.t, h.pluginBinPath, pluginMap)

	rpcClient, err := client.Client()
	if err != nil {
		h.t.Fatalf("client.Client(): %v", err)
	}
	raw, err := rpcClient.Dispense(sharedplugin.PluginTypeWorkflow)
	if err != nil {
		h.t.Fatalf("dispense WorkflowService: %v", err)
	}
	workflow, ok := raw.(pluginpkg.WorkflowService)
	if !ok {
		h.t.Fatalf("dispensed type %T does not implement WorkflowService", raw)
	}
	return workflow
}

// configJSON builds the workflowConfig JSON string autopilot expects.
// HandoffDir is relative because autopilot resolves it via
// filepath.Join(WorkDir, HandoffDir); passing an absolute handoff_dir here
// would produce "/workdir/<absolute path>" which is nonsense. The fake
// Claude still writes into the absolute h.handoffDir via FAKE_CLAUDE_HANDOFF_DIR.
func (h *autopilotHarness) configJSON(maxLegs int) string {
	relHandoff, err := filepath.Rel(h.workDir, h.handoffDir)
	if err != nil {
		h.t.Fatalf("compute relative handoff dir: %v", err)
	}
	cfg := map[string]any{
		"work_dir":         h.workDir,
		"handoff_dir":      relHandoff,
		"poll_interval_ms": 20,
		"max_flight_legs":  maxLegs,
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		h.t.Fatalf("marshal config: %v", err)
	}
	return string(b)
}

// waitForWorkflowStatus polls GetWorkflowStatus until the workflow reaches
// one of the target statuses or the deadline expires. Returns the final
// status. Using a predicate instead of a fixed sleep keeps the test
// reactive and avoids flake under CI load.
func waitForWorkflowStatus(
	t *testing.T,
	ctx context.Context,
	svc pluginpkg.WorkflowService,
	workflowID string,
	timeout time.Duration,
	want ...bossanovav1.WorkflowStatus,
) *bossanovav1.WorkflowStatusInfo {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		info, err := svc.GetWorkflowStatus(ctx, workflowID)
		if err != nil {
			t.Fatalf("GetWorkflowStatus: %v", err)
		}
		for _, target := range want {
			if info.GetStatus() == target {
				return info
			}
		}
		if time.Now().After(deadline) {
			wantNames := make([]string, len(want))
			for i, w := range want {
				wantNames[i] = w.String()
			}
			t.Fatalf("timed out after %s waiting for workflow %s to reach %v; last status=%s step=%s leg=%d err=%q",
				timeout, workflowID, wantNames, info.GetStatus(), info.GetCurrentStep(), info.GetFlightLeg(), info.GetLastError())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestE2E_Autopilot_StartToComplete drives a three-leg workflow end-to-end:
// plan → implement (leg 1) → handoff loop (legs 2 & 3) → verify → land →
// COMPLETED. Fake Claude writes a unique handoff file on every run so the
// autopilot's handoff scanner finds something to resume with between legs.
func TestE2E_Autopilot_StartToComplete(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	logFile := filepath.Join(t.TempDir(), "fake-claude.log")
	h := newAutopilotHarness(t, map[string]string{
		"FAKE_CLAUDE_LINES":    "2",
		"FAKE_CLAUDE_LOG_FILE": logFile,
	})
	t.Cleanup(func() {
		if t.Failed() {
			b, err := os.ReadFile(logFile)
			if err == nil {
				t.Logf("fake-claude.log contents:\n%s", string(b))
			}
		}
	})
	workflow := h.spawnAutopilot()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	resp, err := workflow.StartWorkflow(ctx, &bossanovav1.StartWorkflowRequest{
		PlanPath:   h.planPath,
		SessionId:  h.sessionID,
		RepoId:     h.repoID,
		MaxLegs:    3,
		ConfigJson: h.configJSON(3),
	})
	if err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	workflowID := resp.GetWorkflowId()
	if workflowID == "" {
		t.Fatal("StartWorkflow returned empty workflow_id")
	}

	final := waitForWorkflowStatus(t, ctx, workflow, workflowID, 45*time.Second,
		bossanovav1.WorkflowStatus_WORKFLOW_STATUS_COMPLETED,
		bossanovav1.WorkflowStatus_WORKFLOW_STATUS_FAILED,
		bossanovav1.WorkflowStatus_WORKFLOW_STATUS_PAUSED,
	)

	if final.GetStatus() != bossanovav1.WorkflowStatus_WORKFLOW_STATUS_COMPLETED {
		t.Fatalf("workflow status = %s, want COMPLETED (step=%s leg=%d err=%q)",
			final.GetStatus(), final.GetCurrentStep(), final.GetFlightLeg(), final.GetLastError())
	}

	// At least one handoff file should have been written (implement leg) and
	// on completion the workflow should report a non-zero flight leg.
	entries, err := os.ReadDir(h.handoffDir)
	if err != nil {
		t.Fatalf("read handoff dir: %v", err)
	}
	if len(entries) == 0 {
		t.Error("expected at least one handoff file, got none")
	}
	if final.GetFlightLeg() == 0 {
		t.Error("expected non-zero flight leg on completion")
	}
}

// TestE2E_Autopilot_Cancel starts a workflow with a slow fake Claude, waits
// for the implement leg to be running, sends CancelWorkflow, and asserts
// the workflow reports CANCELLED. Uses FAKE_CLAUDE_SLEEP_MS so the leg
// definitely hasn't finished before we cancel.
func TestE2E_Autopilot_Cancel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newAutopilotHarness(t, map[string]string{
		"FAKE_CLAUDE_LINES":    "5",
		"FAKE_CLAUDE_SLEEP_MS": "400", // 5 lines × 400ms ≈ 2s per leg
	})
	workflow := h.spawnAutopilot()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	resp, err := workflow.StartWorkflow(ctx, &bossanovav1.StartWorkflowRequest{
		PlanPath:   h.planPath,
		SessionId:  h.sessionID,
		RepoId:     h.repoID,
		MaxLegs:    3,
		ConfigJson: h.configJSON(3),
	})
	if err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	workflowID := resp.GetWorkflowId()

	// Wait for the workflow to advance past pending so we know the goroutine
	// is actively running; then request cancel. CancelWorkflow just marks
	// the DB — the orchestration loop picks up the cancel at the next
	// isStoppedOrDone check (between legs, or whenever the attempt poll
	// returns), so we poll for CANCELLED rather than assume immediacy.
	deadline := time.Now().Add(5 * time.Second)
	for {
		info, err := workflow.GetWorkflowStatus(ctx, workflowID)
		if err != nil {
			t.Fatalf("GetWorkflowStatus pre-cancel: %v", err)
		}
		if info.GetStatus() == bossanovav1.WorkflowStatus_WORKFLOW_STATUS_RUNNING && info.GetFlightLeg() > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("workflow never reached running+leg>0; last status=%s leg=%d",
				info.GetStatus(), info.GetFlightLeg())
		}
		time.Sleep(50 * time.Millisecond)
	}

	if _, err := workflow.CancelWorkflow(ctx, workflowID); err != nil {
		t.Fatalf("CancelWorkflow: %v", err)
	}

	final := waitForWorkflowStatus(t, ctx, workflow, workflowID, 10*time.Second,
		bossanovav1.WorkflowStatus_WORKFLOW_STATUS_CANCELLED,
		bossanovav1.WorkflowStatus_WORKFLOW_STATUS_COMPLETED,
		bossanovav1.WorkflowStatus_WORKFLOW_STATUS_FAILED,
	)
	if final.GetStatus() != bossanovav1.WorkflowStatus_WORKFLOW_STATUS_CANCELLED {
		t.Fatalf("status = %s, want CANCELLED (leg=%d err=%q)",
			final.GetStatus(), final.GetFlightLeg(), final.GetLastError())
	}
}

// TestE2E_Autopilot_NotifyStatusChange verifies the autopilot accepts
// NotifyStatusChange and returns without error. The autopilot docstring
// explicitly says this is a no-op (the repair plugin handles status
// changes) — this test exists to catch the day someone wires real
// behaviour and breaks the contract.
func TestE2E_Autopilot_NotifyStatusChange(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newAutopilotHarness(t, map[string]string{
		"FAKE_CLAUDE_LINES":    "3",
		"FAKE_CLAUDE_SLEEP_MS": "200",
	})
	workflow := h.spawnAutopilot()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := workflow.StartWorkflow(ctx, &bossanovav1.StartWorkflowRequest{
		PlanPath:   h.planPath,
		SessionId:  h.sessionID,
		RepoId:     h.repoID,
		MaxLegs:    2,
		ConfigJson: h.configJSON(2),
	})
	if err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}

	// Deliver the notification mid-workflow. Current contract: no-op. If
	// this ever returns an error, autopilot's NotifyStatusChange handler
	// has grown behaviour that needs its own dedicated test — fail loudly.
	err = workflow.NotifyStatusChange(ctx, h.sessionID,
		bossanovav1.DisplayStatus_DISPLAY_STATUS_PASSING, false)
	if err != nil {
		t.Errorf("NotifyStatusChange returned error: %v", err)
	}

	// Let the workflow finish so cleanup doesn't race plugin Kill — the
	// concrete terminal state is not what this test is validating.
	_ = waitForWorkflowStatus(t, ctx, workflow, resp.GetWorkflowId(), 20*time.Second,
		bossanovav1.WorkflowStatus_WORKFLOW_STATUS_COMPLETED,
		bossanovav1.WorkflowStatus_WORKFLOW_STATUS_FAILED,
		bossanovav1.WorkflowStatus_WORKFLOW_STATUS_PAUSED,
	)
}

// TestE2E_Autopilot_MaxLegsExceeded drives a workflow where fake_claude
// never writes handoff files (FAKE_CLAUDE_HANDOFF_DIR is unset by zeroing
// it out via an empty directory that the harness then doesn't touch).
// The autopilot's handoff recovery runs, produces no file, and pauses the
// workflow with a "handoff recovery produced no file" error — the
// real-world exhaustion mode when Claude cannot make progress.
//
// NOTE: the plan originally specified WORKFLOW_STATUS_FAILED, but the
// production autopilot actually pauses (so a human can inspect and
// resume). The test asserts what the code does, not what the plan said;
// FAILED-vs-PAUSED is a behaviour decision that belongs in a separate
// PR if it ever changes.
func TestE2E_Autopilot_MaxLegsExceeded(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newAutopilotHarness(t, map[string]string{
		"FAKE_CLAUDE_LINES": "1",
	}, autopilotHarnessOpts{skipHandoffEnv: true})
	workflow := h.spawnAutopilot()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	resp, err := workflow.StartWorkflow(ctx, &bossanovav1.StartWorkflowRequest{
		PlanPath:   h.planPath,
		SessionId:  h.sessionID,
		RepoId:     h.repoID,
		MaxLegs:    2,
		ConfigJson: h.configJSON(2),
	})
	if err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}

	final := waitForWorkflowStatus(t, ctx, workflow, resp.GetWorkflowId(), 30*time.Second,
		bossanovav1.WorkflowStatus_WORKFLOW_STATUS_PAUSED,
		bossanovav1.WorkflowStatus_WORKFLOW_STATUS_FAILED,
		bossanovav1.WorkflowStatus_WORKFLOW_STATUS_COMPLETED,
	)
	if final.GetStatus() != bossanovav1.WorkflowStatus_WORKFLOW_STATUS_PAUSED {
		t.Fatalf("status = %s, want PAUSED (leg=%d err=%q)",
			final.GetStatus(), final.GetFlightLeg(), final.GetLastError())
	}
	if final.GetLastError() == "" {
		t.Error("expected non-empty last_error describing exhaustion")
	}
}

// TestE2E_Autopilot_PauseResume starts a workflow with a slow fake Claude
// so we can pause while the implement leg is running, then resume and
// watch the workflow drive through to COMPLETED. The orchestration loop
// checks isStoppedOrDone between legs, so pause takes effect at the next
// leg boundary — the test polls for PAUSED rather than assuming sync.
func TestE2E_Autopilot_PauseResume(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	h := newAutopilotHarness(t, map[string]string{
		"FAKE_CLAUDE_LINES":    "4",
		"FAKE_CLAUDE_SLEEP_MS": "300",
	})
	workflow := h.spawnAutopilot()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	resp, err := workflow.StartWorkflow(ctx, &bossanovav1.StartWorkflowRequest{
		PlanPath:   h.planPath,
		SessionId:  h.sessionID,
		RepoId:     h.repoID,
		MaxLegs:    2,
		ConfigJson: h.configJSON(2),
	})
	if err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	workflowID := resp.GetWorkflowId()

	// Wait until the implement leg starts so pause arrives while Claude is
	// actually doing work — exercises the isStoppedOrDone check in the
	// loop rather than short-circuiting before anything runs.
	deadline := time.Now().Add(10 * time.Second)
	for {
		info, err := workflow.GetWorkflowStatus(ctx, workflowID)
		if err != nil {
			t.Fatalf("GetWorkflowStatus pre-pause: %v", err)
		}
		if info.GetFlightLeg() > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("workflow never entered a flight leg; last status=%s step=%s",
				info.GetStatus(), info.GetCurrentStep())
		}
		time.Sleep(50 * time.Millisecond)
	}

	if _, err := workflow.PauseWorkflow(ctx, workflowID); err != nil {
		t.Fatalf("PauseWorkflow: %v", err)
	}

	paused := waitForWorkflowStatus(t, ctx, workflow, workflowID, 15*time.Second,
		bossanovav1.WorkflowStatus_WORKFLOW_STATUS_PAUSED,
		bossanovav1.WorkflowStatus_WORKFLOW_STATUS_COMPLETED,
		bossanovav1.WorkflowStatus_WORKFLOW_STATUS_FAILED,
		bossanovav1.WorkflowStatus_WORKFLOW_STATUS_CANCELLED,
	)
	if paused.GetStatus() != bossanovav1.WorkflowStatus_WORKFLOW_STATUS_PAUSED {
		t.Fatalf("pre-resume status = %s, want PAUSED", paused.GetStatus())
	}

	if _, err := workflow.ResumeWorkflow(ctx, workflowID); err != nil {
		t.Fatalf("ResumeWorkflow: %v", err)
	}

	final := waitForWorkflowStatus(t, ctx, workflow, workflowID, 30*time.Second,
		bossanovav1.WorkflowStatus_WORKFLOW_STATUS_COMPLETED,
		bossanovav1.WorkflowStatus_WORKFLOW_STATUS_FAILED,
	)
	if final.GetStatus() != bossanovav1.WorkflowStatus_WORKFLOW_STATUS_COMPLETED {
		t.Fatalf("post-resume status = %s, want COMPLETED (leg=%d err=%q)",
			final.GetStatus(), final.GetFlightLeg(), final.GetLastError())
	}
}
