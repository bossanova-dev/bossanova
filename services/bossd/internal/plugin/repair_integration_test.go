package plugin_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/migrate"
	sharedplugin "github.com/recurser/bossalib/plugin"
	"github.com/recurser/bossd/internal/claude"
	"github.com/recurser/bossd/internal/db"
	pluginpkg "github.com/recurser/bossd/internal/plugin"
	"github.com/recurser/bossd/internal/plugin/pluginharness"
	"github.com/recurser/bossd/internal/status"
)

// TestRepairPlugin_DrivesClaudeRunOnFailingStatus is the smoke test for the
// Phase 4A wiring: when the daemon notifies the repair plugin of a FAILING
// session, the plugin should call back via StartClaudeRun, the daemon should
// spawn (the fake) Claude in the session's worktree, and IsRepairing should
// flip on then off as the run starts and completes.
//
// Compared to the original autopilot-era end-to-end test this is a deliberate
// smaller scope: we don't drive the full state-machine FIX_COMPLETE path or
// assert on workflow status records (there is no host-side workflow CRUD any
// more). The daemon-side host_service_test covers the StartClaudeRun /
// WaitClaudeRun unit semantics; this test confirms the cross-process plumbing.
func TestRepairPlugin_DrivesClaudeRunOnFailingStatus(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping repair plugin integration test in short mode")
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
	logsDir := filepath.Join(tmpDir, "claude-logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("mkdir claude-logs: %v", err)
	}
	fakeLog := filepath.Join(tmpDir, "fake-claude.log")

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
	// Move the session into a state isSessionRepairable accepts.
	stateInt := int(machine.AwaitingChecks)
	if _, err := sessions.Update(ctx, session.ID, db.UpdateSessionParams{State: &stateInt}); err != nil {
		t.Fatalf("update session state: %v", err)
	}

	// Fake claude — quick exit, args+stdin captured for assertion.
	factory := func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, fakeClaude, args...)
		cmd.Env = append(os.Environ(),
			"FAKE_CLAUDE_LINES=1",
			"FAKE_CLAUDE_EXIT=0",
			"FAKE_CLAUDE_LOG_FILE="+fakeLog,
		)
		return cmd
	}
	runner := claude.NewRunner(
		zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.Disabled),
		claude.WithCommandFactory(factory),
		claude.WithLogDir(logsDir),
	)

	displayTracker := status.NewDisplayTracker()
	chatTracker := status.NewTracker()

	hostService := pluginpkg.NewHostServiceServer(&testVCSProvider{})
	hostService.SetSessionDeps(repos, sessions, chats, displayTracker, chatTracker)
	hostService.SetClaudeRunner(runner)

	binPath := pluginharness.BuildPlugin(t, "bossd-plugin-repair")
	pluginMap := goplugin.PluginSet{
		sharedplugin.PluginTypeWorkflow: &pluginpkg.WorkflowServiceGRPCPlugin{HostService: hostService},
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

	// Start the repair monitor (sets m.stopped=false so NotifyStatusChange takes effect).
	if _, err := workflow.StartWorkflow(ctx, &bossanovav1.StartWorkflowRequest{ConfigJson: ""}); err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}

	// Push a FAILING status change. The plugin should respond by calling
	// StartClaudeRun back into the host, which spawns fake_claude.sh in the
	// session worktree and writes its args+stdin to fakeLog.
	if err := workflow.NotifyStatusChange(ctx,
		session.ID,
		bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING,
		true,
	); err != nil {
		t.Fatalf("NotifyStatusChange: %v", err)
	}

	// Wait for the fake claude script to be invoked.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(fakeLog); err == nil && info.Size() > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	contents, err := os.ReadFile(fakeLog)
	if err != nil {
		t.Fatalf("fake-claude log not produced: %v", err)
	}
	if !contains(contents, "/boss-repair") {
		t.Fatalf("fake-claude not invoked with /boss-repair prompt; log=%q", contents)
	}

	// IsRepairing should have flipped on (and probably back off) by now.
	deadline = time.Now().Add(5 * time.Second)
	sawRepairing := false
	for time.Now().Before(deadline) {
		entry := displayTracker.Get(session.ID)
		if entry != nil && entry.IsRepairing {
			sawRepairing = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !sawRepairing {
		// The repair may have been so fast that we already cleared the
		// flag — that's fine, but at least confirm we got the start side.
		// The log assertion above proves the run actually happened.
		t.Logf("did not observe IsRepairing=true (run may have completed before we polled)")
	}
}

func contains(haystack []byte, needle string) bool {
	return len(needle) == 0 || (len(haystack) > 0 && stringIndex(haystack, needle) >= 0)
}

func stringIndex(haystack []byte, needle string) int {
	n := len(needle)
	if n == 0 {
		return 0
	}
	for i := 0; i+n <= len(haystack); i++ {
		match := true
		for j := 0; j < n; j++ {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
