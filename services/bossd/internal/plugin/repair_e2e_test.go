package plugin_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	goplugin "github.com/hashicorp/go-plugin"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/migrate"
	sharedplugin "github.com/recurser/bossalib/plugin"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	pluginpkg "github.com/recurser/bossd/internal/plugin"
	"github.com/recurser/bossd/internal/plugin/pluginharness"
	"github.com/recurser/bossd/internal/status"
)

// TestE2E_Repair_RealPluginPair drives the full production gRPC chain:
//
//	bossd HostService ⟷ bossd-plugin-repair (real subprocess)
//	bossd HostService ⟷ bossd-plugin-claude  (real subprocess) ⟷ `claude` on PATH
//
// The existing repair_integration_test.go uses an in-process fake agent
// client (newFakeAgentClient + execRunner) which bypasses the bossd ⟷
// claude-plugin gRPC boundary. PR #237's SIGTERM bug lived exactly there:
// the bossd-plugin-claude subprocess inherited the gRPC handler's per-call
// ctx and was killed milliseconds after StartRun returned. The fake client
// short-circuits that path, so the existing integration test couldn't catch
// it. This test does, by spawning both real plugin binaries against an
// in-process HostService and running a real claude exec via fake_claude.sh
// staged on PATH.
//
// The test asserts two things end-to-end:
//
//  1. fake_claude.sh's FAKE_CLAUDE_LOG_FILE captures `/boss-repair` as the
//     prompt — proving the repair plugin called StartAgentRun, the host
//     service routed to the claude plugin, and the claude plugin's runner
//     spawned the fake-claude subprocess correctly.
//  2. The agent log file at agentLogsDir/repair-<sessionID>.log contains
//     both the `[runner] spawning claude` preamble and an `[runner] exited`
//     trailer — proving Phase 1a's diagnostic capture round-trips through
//     the production binary, not just the unit test.
func TestE2E_Repair_RealPluginPair(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E repair plugin pair test in short mode")
	}

	// 1. Stage fake_claude.sh as `claude` on PATH so the bossd-plugin-claude
	// subprocess (spawned by go-plugin from the test process) inherits a
	// PATH that resolves to our fake. t.Setenv restores PATH at test end
	// and prevents parallel-test interference.
	_, thisFile, _, _ := runtime.Caller(0)
	fakeClaude := filepath.Join(filepath.Dir(thisFile), "testdata", "fake_claude.sh")
	if _, err := os.Stat(fakeClaude); err != nil {
		t.Fatalf("fake_claude.sh missing at %s: %v", fakeClaude, err)
	}
	pathDir := t.TempDir()
	claudeShim := filepath.Join(pathDir, "claude")
	if err := os.Symlink(fakeClaude, claudeShim); err != nil {
		t.Fatalf("symlink claude shim: %v", err)
	}
	fakeLogDir := t.TempDir()
	fakeLog := filepath.Join(fakeLogDir, "fake-claude.log")
	t.Setenv("PATH", pathDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLAUDE_LINES", "1")
	t.Setenv("FAKE_CLAUDE_EXIT", "0")
	t.Setenv("FAKE_CLAUDE_LOG_FILE", fakeLog)

	// 2. Worktree + bossd state.
	tmpDir := t.TempDir()
	workDir := filepath.Join(tmpDir, "worktree")
	if err := os.MkdirAll(filepath.Join(workDir, ".boss"), 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	agentLogsDir := filepath.Join(tmpDir, "agent-logs")
	if err := os.MkdirAll(agentLogsDir, 0o755); err != nil {
		t.Fatalf("mkdir agent-logs: %v", err)
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
	chats := db.NewAgentChatStore(sqlDB)

	ctx := context.Background()
	repo, err := repos.Create(ctx, db.CreateRepoParams{
		DisplayName:       "test-repo",
		LocalPath:         workDir,
		OriginURL:         "https://github.com/test/repair-e2e.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   tmpDir,
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	session, err := sessions.Create(ctx, db.CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "real plugin pair e2e",
		Plan:         "docs/plans/test-plan.md",
		WorktreePath: workDir,
		BranchName:   "repair-e2e-real",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	// AwaitingChecks is one of the four states isSessionRepairable accepts.
	stateInt := int(machine.AwaitingChecks)
	if _, err := sessions.Update(ctx, session.ID, db.UpdateSessionParams{State: &stateInt}); err != nil {
		t.Fatalf("update session state: %v", err)
	}

	// 3. HostService shared by both plugin subprocesses. Both plugins will
	// dial back to this server through the go-plugin GRPCBroker (different
	// broker IDs per plugin type — see grpc_plugins.go and
	// agent_runner_grpc.go).
	displayTracker := status.NewDisplayTracker()
	chatTracker := status.NewTracker()
	hostService := pluginpkg.NewHostServiceServer(&testVCSProvider{})
	hostService.SetSessionDeps(repos, sessions, chats, displayTracker, chatTracker)
	hostService.SetAgentLogsDir(agentLogsDir)

	// 4. Spawn the real bossd-plugin-claude subprocess. The dispensed
	// AgentRunner satisfies agent.AgentRunnerClient because pluginpkg's
	// AgentRunner is a superset (adds GetInfo). Wire it into the host
	// service so StartAgentRun routes here.
	claudeBin := pluginharness.BuildPlugin(t, "bossd-plugin-claude")
	claudePluginMap := goplugin.PluginSet{
		sharedplugin.PluginTypeAgentRunner: pluginpkg.NewAgentRunnerGRPCPlugin(hostService),
	}
	claudeClient := pluginharness.SpawnPlugin(t, claudeBin, claudePluginMap)
	claudeRPC, err := claudeClient.Client()
	if err != nil {
		t.Fatalf("claude client.Client(): %v", err)
	}
	claudeRaw, err := claudeRPC.Dispense(sharedplugin.PluginTypeAgentRunner)
	if err != nil {
		t.Fatalf("dispense AgentRunner: %v", err)
	}
	claudeAgent, ok := claudeRaw.(agent.AgentRunnerClient)
	if !ok {
		t.Fatalf("claude plugin client %T does not satisfy agent.AgentRunnerClient", claudeRaw)
	}
	hostService.SetAgentClients(map[string]agent.AgentRunnerClient{
		"claude": claudeAgent,
	})

	// 5. Spawn the real bossd-plugin-repair subprocess against the same
	// HostService. This is the boundary the existing integration test
	// already exercises; what's new here is the real claude plugin behind
	// it. SetAgentClients above is what makes StartAgentRun reach a real
	// subprocess instead of the in-process fakeAgentClient.
	repairBin := pluginharness.BuildPlugin(t, "bossd-plugin-repair")
	repairPluginMap := goplugin.PluginSet{
		sharedplugin.PluginTypeWorkflow: &pluginpkg.WorkflowServiceGRPCPlugin{HostService: hostService},
	}
	repairClient := pluginharness.SpawnPlugin(t, repairBin, repairPluginMap)
	repairRPC, err := repairClient.Client()
	if err != nil {
		t.Fatalf("repair client.Client(): %v", err)
	}
	repairRaw, err := repairRPC.Dispense(sharedplugin.PluginTypeWorkflow)
	if err != nil {
		t.Fatalf("dispense WorkflowService: %v", err)
	}
	workflow, ok := repairRaw.(pluginpkg.WorkflowService)
	if !ok {
		t.Fatalf("repair plugin client %T does not satisfy WorkflowService", repairRaw)
	}

	if _, err := workflow.StartWorkflow(ctx, &bossanovav1.StartWorkflowRequest{ConfigJson: ""}); err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}

	// 6. Trigger the repair via NotifyStatusChange. The chain is now:
	//   test → repair plugin → host.StartAgentRun → claude plugin
	//        → runner.Start → exec("claude" → fake_claude.sh in PATH)
	if err := workflow.NotifyStatusChange(ctx,
		session.ID,
		bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING,
		true,
	); err != nil {
		t.Fatalf("NotifyStatusChange: %v", err)
	}

	// 7. Wait for fake_claude to record its invocation. The first repair
	// attempt is debounced behind the host's 500ms WaitAgentRun poll plus
	// the plugin handshake; 15s is generous on a loaded CI box.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(fakeLog); err == nil && info.Size() > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	contents, err := os.ReadFile(fakeLog)
	if err != nil || len(contents) == 0 {
		t.Fatalf("fake-claude was never invoked (file=%q size=%d err=%v)", fakeLog, len(contents), err)
	}
	if !contains(contents, "/boss-repair") {
		t.Fatalf("fake-claude not invoked with /boss-repair prompt; log=%q", contents)
	}

	// 8. The runner's diagnostic NDJSON must round-trip through the real
	// claude plugin too. Phase 1a's contract: every spawn writes an opening
	// `[runner] spawning claude` and a closing `[runner] exited` line into
	// the agentLogsDir log. If either is missing, the regression that left
	// us with empty repair-*.log files is back.
	runnerLog := filepath.Join(agentLogsDir, "repair-"+session.ID+".log")
	deadline = time.Now().Add(10 * time.Second)
	var runnerLogContents []byte
	for time.Now().Before(deadline) {
		runnerLogContents, _ = os.ReadFile(runnerLog)
		// Wait until we see the closing marker, since lineWriter flushes
		// it from the wait goroutine after cmd.Wait returns.
		if contains(runnerLogContents, "[runner] exited") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !contains(runnerLogContents, "[runner] spawning claude") {
		t.Errorf("runner log missing spawn preamble: %s", string(runnerLogContents))
	}
	if !contains(runnerLogContents, "[runner] exited") {
		t.Errorf("runner log missing exit trailer: %s", string(runnerLogContents))
	}
}

// TestE2E_Repair_RealPluginPair_PropagatesStartFailure is the negative
// counterpart: when the spawned subprocess fails to start (PATH has no
// `claude`), Phase 1a's contract says the failure reason must be persisted
// into the agent log file, not silently swallowed. The previous behavior
// left a 0-byte log; this test enforces the new "[runner] cmd.Start failed"
// marker survives the production gRPC boundary.
func TestE2E_Repair_RealPluginPair_PropagatesStartFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping E2E repair plugin pair test in short mode")
	}

	// Build BOTH plugin binaries before narrowing PATH — pluginharness.BuildPlugin
	// shells out to `go build`, which requires `go` to remain resolvable. We
	// build into a stable tmpdir so the binaries survive the PATH change.
	claudeBin := pluginharness.BuildPlugin(t, "bossd-plugin-claude")
	repairBin := pluginharness.BuildPlugin(t, "bossd-plugin-repair")

	// Now narrow PATH so `claude` is unresolvable. We use Setenv directly
	// (not append) so any system claude on the developer's machine doesn't
	// accidentally satisfy the lookup.
	pathDir := t.TempDir()
	t.Setenv("PATH", pathDir)

	tmpDir := t.TempDir()
	workDir := filepath.Join(tmpDir, "worktree")
	if err := os.MkdirAll(filepath.Join(workDir, ".boss"), 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	agentLogsDir := filepath.Join(tmpDir, "agent-logs")
	if err := os.MkdirAll(agentLogsDir, 0o755); err != nil {
		t.Fatalf("mkdir agent-logs: %v", err)
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
	chats := db.NewAgentChatStore(sqlDB)

	ctx := context.Background()
	repo, err := repos.Create(ctx, db.CreateRepoParams{
		DisplayName:       "test-repo",
		LocalPath:         workDir,
		OriginURL:         "https://github.com/test/repair-e2e-fail.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   tmpDir,
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	session, err := sessions.Create(ctx, db.CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "real plugin pair e2e (start failure)",
		Plan:         "docs/plans/test-plan.md",
		WorktreePath: workDir,
		BranchName:   "repair-e2e-fail",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	stateInt := int(machine.AwaitingChecks)
	if _, err := sessions.Update(ctx, session.ID, db.UpdateSessionParams{State: &stateInt}); err != nil {
		t.Fatalf("update session state: %v", err)
	}

	displayTracker := status.NewDisplayTracker()
	chatTracker := status.NewTracker()
	hostService := pluginpkg.NewHostServiceServer(&testVCSProvider{})
	hostService.SetSessionDeps(repos, sessions, chats, displayTracker, chatTracker)
	hostService.SetAgentLogsDir(agentLogsDir)

	claudePluginMap := goplugin.PluginSet{
		sharedplugin.PluginTypeAgentRunner: pluginpkg.NewAgentRunnerGRPCPlugin(hostService),
	}
	claudeClient := pluginharness.SpawnPlugin(t, claudeBin, claudePluginMap)
	claudeRPC, err := claudeClient.Client()
	if err != nil {
		t.Fatalf("claude client.Client(): %v", err)
	}
	claudeRaw, err := claudeRPC.Dispense(sharedplugin.PluginTypeAgentRunner)
	if err != nil {
		t.Fatalf("dispense AgentRunner: %v", err)
	}
	claudeAgent, ok := claudeRaw.(agent.AgentRunnerClient)
	if !ok {
		t.Fatalf("claude plugin client %T does not satisfy agent.AgentRunnerClient", claudeRaw)
	}
	hostService.SetAgentClients(map[string]agent.AgentRunnerClient{
		"claude": claudeAgent,
	})

	repairPluginMap := goplugin.PluginSet{
		sharedplugin.PluginTypeWorkflow: &pluginpkg.WorkflowServiceGRPCPlugin{HostService: hostService},
	}
	repairClient := pluginharness.SpawnPlugin(t, repairBin, repairPluginMap)
	repairRPC, err := repairClient.Client()
	if err != nil {
		t.Fatalf("repair client.Client(): %v", err)
	}
	repairRaw, err := repairRPC.Dispense(sharedplugin.PluginTypeWorkflow)
	if err != nil {
		t.Fatalf("dispense WorkflowService: %v", err)
	}
	workflow, ok := repairRaw.(pluginpkg.WorkflowService)
	if !ok {
		t.Fatalf("repair plugin client %T does not satisfy WorkflowService", repairRaw)
	}
	if _, err := workflow.StartWorkflow(ctx, &bossanovav1.StartWorkflowRequest{ConfigJson: ""}); err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}

	if err := workflow.NotifyStatusChange(ctx,
		session.ID,
		bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING,
		true,
	); err != nil {
		t.Fatalf("NotifyStatusChange: %v", err)
	}

	// The agent log file must contain the spawn preamble (proving we got
	// past openLogNoFollow) AND the cmd.Start failed marker (proving the
	// exec error was persisted before the file was closed). With the bug
	// from the start of this work, this file would be 0 bytes.
	runnerLog := filepath.Join(agentLogsDir, "repair-"+session.ID+".log")
	deadline := time.Now().Add(15 * time.Second)
	var contents []byte
	for time.Now().Before(deadline) {
		contents, _ = os.ReadFile(runnerLog)
		if contains(contents, "[runner] cmd.Start failed") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(contents) == 0 {
		t.Fatalf("runner log %q is empty — Phase 1a regressed: cmd.Start failures must be persisted to the log file", runnerLog)
	}
	if !contains(contents, "[runner] spawning claude") {
		t.Errorf("runner log missing spawn preamble: %s", string(contents))
	}
	if !contains(contents, "[runner] cmd.Start failed") {
		t.Errorf("runner log missing cmd.Start failure marker: %s", string(contents))
	}
}
