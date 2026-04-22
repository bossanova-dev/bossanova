package plugin_test

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/migrate"
	"github.com/recurser/bossalib/models"
	sharedplugin "github.com/recurser/bossalib/plugin"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
	pluginpkg "github.com/recurser/bossd/internal/plugin"
	"github.com/recurser/bossd/internal/plugin/pluginharness"
	"github.com/recurser/bossd/internal/taskorchestrator"
)

// staticTaskSourceProvider feeds a fixed list of TaskSources into the
// orchestrator. In production the Host is the TaskSourceProvider and it
// requires every HostService dep to be wired; these E2E tests don't touch
// sessions or workflows so they use this minimal wrapper instead.
type staticTaskSourceProvider struct {
	sources []pluginpkg.TaskSource
}

func (p *staticTaskSourceProvider) GetTaskSources() []pluginpkg.TaskSource {
	return p.sources
}

// createSessionCall records one CreateSession invocation so the major-bump
// test can pin the fact that Lifecycle.Create (abstracted behind the
// SessionCreator interface) was reached with the right plan + branch.
type createSessionCall struct {
	Opts    taskorchestrator.CreateSessionOpts
	Returns *models.Session
}

// recordingSessionCreator is a SessionCreator that captures every
// CreateSession call into a mutex-protected slice. Use Calls() to read
// the snapshot after driving the orchestrator.
type recordingSessionCreator struct {
	mu      sync.Mutex
	calls   []createSessionCall
	nextErr error
}

func (r *recordingSessionCreator) CreateSession(_ context.Context, opts taskorchestrator.CreateSessionOpts) (*models.Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.nextErr != nil {
		return nil, r.nextErr
	}
	// Return a synthesised session — the orchestrator treats it as opaque
	// for CREATE_SESSION tasks; the real DB record isn't needed here.
	sess := &models.Session{ID: "sess-" + opts.Title}
	r.calls = append(r.calls, createSessionCall{Opts: opts, Returns: sess})
	return sess, nil
}

func (r *recordingSessionCreator) Calls() []createSessionCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]createSessionCall, len(r.calls))
	copy(out, r.calls)
	return out
}

// openDBWithMigrations opens an in-memory SQLite with every migration
// applied. The orchestrator's task mapping store and repo store both
// require real stores for the routing path under test.
func openDBWithMigrations(t *testing.T) *sql.DB {
	t.Helper()
	sqlDB, err := db.OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := migrate.Run(sqlDB, os.DirFS(pluginharness.MigrationsDir())); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	return sqlDB
}

// dependabotHarness bundles the moving parts of a Dependabot E2E test:
// the built plugin binary, a recording VCS provider, a real DB with
// repo + task mapping stores, a session-creator recorder, and a live
// *taskorchestrator.Orchestrator pointing at the spawned plugin. The
// orchestrator's poll goroutine runs under a cancellable context;
// harness.Stop releases it via t.Cleanup.
type dependabotHarness struct {
	t              *testing.T
	provider       *testVCSProvider
	repos          db.RepoStore
	taskMappings   db.TaskMappingStore
	repoID         string
	repoOriginURL  string
	sessionCreator *recordingSessionCreator
	orchestrator   *taskorchestrator.Orchestrator
	cancel         context.CancelFunc
}

// newDependabotHarness builds a dependabot plugin, wires it into a host
// service, spawns it, and constructs a ready-to-Start orchestrator. The
// provider starts empty; tests set PRs/checks/status before calling Start.
func newDependabotHarness(t *testing.T, provider *testVCSProvider) *dependabotHarness {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping Dependabot E2E test in short mode")
	}

	sqlDB := openDBWithMigrations(t)
	repos := db.NewRepoStore(sqlDB)
	taskMappings := db.NewTaskMappingStore(sqlDB)

	ctx := context.Background()
	repo, err := repos.Create(ctx, db.CreateRepoParams{
		DisplayName:       "dependabot-e2e",
		LocalPath:         t.TempDir(),
		OriginURL:         "https://github.com/org/dependabot-e2e",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	// CanAutoMergeDependabot defaults to true (set in repo_store.Create),
	// which is what we want — confirm defensively so a future default
	// change doesn't silently break this test.
	if !repo.CanAutoMergeDependabot {
		t.Fatalf("repo created without CanAutoMergeDependabot=true; got %+v", repo)
	}

	// Spawn the real dependabot plugin with a broker-registered host
	// service so the plugin's callbacks (ListOpenPRs, GetCheckResults,
	// GetPRStatus) flow through `provider`.
	hostService := pluginpkg.NewHostServiceServer(provider)
	binPath := buildDependabotBinary(t)
	pluginMap := goplugin.PluginSet{
		sharedplugin.PluginTypeTaskSource: pluginpkg.NewTaskSourceGRPCPlugin(hostService),
	}
	client := pluginharness.SpawnPlugin(t, binPath, pluginMap)

	rpcClient, err := client.Client()
	if err != nil {
		t.Fatalf("client.Client(): %v", err)
	}
	raw, err := rpcClient.Dispense(sharedplugin.PluginTypeTaskSource)
	if err != nil {
		t.Fatalf("dispense TaskSource: %v", err)
	}
	ts, ok := raw.(pluginpkg.TaskSource)
	if !ok {
		t.Fatalf("dispensed type %T does not implement TaskSource", raw)
	}

	sessionCreator := &recordingSessionCreator{}

	// A 24h poll interval means the orchestrator's timer.NewTicker never
	// fires during the test — we rely exclusively on the immediate poll
	// Start kicks off. Stagger delay (interval / len(eligibleRepos))
	// would otherwise introduce a fixed sleep between repos; we only use
	// one repo here so stagger is zero.
	orch := taskorchestrator.New(
		&staticTaskSourceProvider{sources: []pluginpkg.TaskSource{ts}},
		repos,
		taskMappings,
		sessionCreator,
		provider,
		nil, // no base branch syncer
		nil, // no liveness checker
		24*time.Hour,
		zerolog.Nop(),
	)

	h := &dependabotHarness{
		t:              t,
		provider:       provider,
		repos:          repos,
		taskMappings:   taskMappings,
		repoID:         repo.ID,
		repoOriginURL:  repo.OriginURL,
		sessionCreator: sessionCreator,
		orchestrator:   orch,
	}
	return h
}

// Start kicks off the orchestrator's poll goroutine (which polls once
// immediately) and registers a t.Cleanup that cancels + drains it.
func (h *dependabotHarness) Start() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.orchestrator.Start(ctx)
	h.t.Cleanup(func() {
		cancel()
		<-h.orchestrator.Done()
	})
}

// waitForMergeCalls polls the provider until at least want calls are
// observed or the deadline expires. Returns the snapshot for assertions.
func (h *dependabotHarness) waitForMergeCalls(want int, timeout time.Duration) []mergePRCall {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		calls := h.provider.MergeCalls()
		if len(calls) >= want {
			return calls
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out after %s waiting for %d MergePR calls; got %d: %+v", timeout, want, len(calls), calls)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// waitForSessionCreations polls the session creator until at least want
// CreateSession invocations are observed or the deadline expires.
func (h *dependabotHarness) waitForSessionCreations(want int, timeout time.Duration) []createSessionCall {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		calls := h.sessionCreator.Calls()
		if len(calls) >= want {
			return calls
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out after %s waiting for %d CreateSession calls; got %d: %+v", timeout, want, len(calls), calls)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// waitForTaskMappingStatus polls the DB for a mapping with the given
// external ID and asserts it reaches `want` (or a terminal status) within
// the deadline. Returns the final mapping.
func (h *dependabotHarness) waitForTaskMappingStatus(externalID string, want models.TaskMappingStatus, timeout time.Duration) *models.TaskMapping {
	h.t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(timeout)
	var last *models.TaskMapping
	for {
		mapping, err := h.taskMappings.GetByExternalID(ctx, externalID)
		if err == nil && mapping != nil {
			last = mapping
			if mapping.Status == want {
				return mapping
			}
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out after %s waiting for mapping %q to reach status %d; last=%+v", timeout, externalID, want, last)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// TestE2E_Dependabot_AutoMergeCycle drives the full behavioural loop:
// the real dependabot plugin sees a passing-checks, mergeable PR via its
// host callbacks, returns TASK_ACTION_AUTO_MERGE, and the orchestrator
// directly calls vcs.Provider.MergePR and marks the task mapping
// Completed. This pins the gap TestIntegration_PluginGRPCRoundTrip left
// open — that test only verified the plugin returned AUTO_MERGE but
// never observed the merge itself (MergePR was a no-op stub).
func TestE2E_Dependabot_AutoMergeCycle(t *testing.T) {
	success := vcs.CheckConclusionSuccess
	mergeable := true

	provider := &testVCSProvider{
		prs: []vcs.PRSummary{
			{
				Number:     42,
				Title:      "Bump lodash from 4.17.20 to 4.17.21",
				HeadBranch: "dependabot/npm_and_yarn/lodash-4.17.21",
				State:      vcs.PRStateOpen,
				Author:     "app/dependabot",
			},
		},
		checks: map[int][]vcs.CheckResult{
			42: {{ID: "ci", Name: "CI", Status: vcs.CheckStatusCompleted, Conclusion: &success}},
		},
		status: map[int]*vcs.PRStatus{
			42: {State: vcs.PRStateOpen, Mergeable: &mergeable},
		},
	}

	h := newDependabotHarness(t, provider)
	h.Start()

	calls := h.waitForMergeCalls(1, 10*time.Second)
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 MergePR call, got %d: %+v", len(calls), calls)
	}

	got := calls[0]
	if got.PRID != 42 {
		t.Errorf("MergePR PRID = %d, want 42", got.PRID)
	}
	if got.RepoPath != h.repoOriginURL {
		t.Errorf("MergePR RepoPath = %q, want %q", got.RepoPath, h.repoOriginURL)
	}
	// DB default merge_strategy is 'merge' (see repo_store.go INSERT).
	// The orchestrator forwards repo.MergeStrategy verbatim; a regression
	// where that strategy gets dropped would surface as "" here.
	if got.Strategy != "merge" {
		t.Errorf("MergePR Strategy = %q, want %q", got.Strategy, "merge")
	}

	// Task mapping must transition to Completed after a successful merge.
	// The orchestrator writes this via updateMappingStatus inside
	// handleAutoMerge — regression risk: if the success branch silently
	// skipped the status write, the next poll would find no mapping and
	// attempt the merge again.
	externalID := "dependabot:pr:" + h.repoOriginURL + ":42"
	mapping := h.waitForTaskMappingStatus(externalID, models.TaskMappingStatusCompleted, 5*time.Second)
	if mapping.PluginName != "dependabot" {
		t.Errorf("mapping PluginName = %q, want %q", mapping.PluginName, "dependabot")
	}

	// No session should be created for AUTO_MERGE — this path skips the
	// session creator entirely. A regression where AUTO_MERGE accidentally
	// went through handleCreateSession would spawn a session here.
	if calls := h.sessionCreator.Calls(); len(calls) != 0 {
		t.Errorf("expected 0 CreateSession calls for AUTO_MERGE, got %d: %+v", len(calls), calls)
	}
}

// TestE2E_Dependabot_CreateSessionForMajorBump pins the CREATE_SESSION
// branch of the dependabot plugin's classification path. The plugin
// today produces CREATE_SESSION only when checks have failed (see
// classifyPR in plugins/bossd-plugin-dependabot/server.go:139) — there
// is no semver-major detection yet, so this test uses the observable
// trigger (failed checks) to drive the loop. The enhancement to
// classify semver-major bumps as CREATE_SESSION regardless of check
// status is tracked separately; once it lands, this test should gain a
// second case that feeds a "Bump foo from 1.x to 2.x" PR title with
// passing checks and asserts the same CREATE_SESSION outcome.
//
// The point of this test isn't the classification trigger — it's the
// post-classification loop: plugin returns CREATE_SESSION → orchestrator
// calls sessionCreator.CreateSession (production's Lifecycle.Create
// adapter) with the plugin-supplied Plan, Title, and existing branch.
func TestE2E_Dependabot_CreateSessionForMajorBump(t *testing.T) {
	failure := vcs.CheckConclusionFailure

	provider := &testVCSProvider{
		prs: []vcs.PRSummary{
			{
				Number:     77,
				Title:      "Bump express from 4.0.0 to 5.0.0",
				HeadBranch: "dependabot/npm_and_yarn/express-5.0.0",
				State:      vcs.PRStateOpen,
				Author:     "app/dependabot",
			},
		},
		checks: map[int][]vcs.CheckResult{
			77: {{ID: "ci", Name: "CI", Status: vcs.CheckStatusCompleted, Conclusion: &failure}},
		},
		// PR status is unread on the CREATE_SESSION path (classifyPR
		// returns before calling GetPRStatus when checks failed); the
		// zero-value status map is fine.
	}

	h := newDependabotHarness(t, provider)
	h.Start()

	calls := h.waitForSessionCreations(1, 10*time.Second)
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 CreateSession call, got %d: %+v", len(calls), calls)
	}

	opts := calls[0].Opts
	if opts.RepoID != h.repoID {
		t.Errorf("CreateSession RepoID = %q, want %q", opts.RepoID, h.repoID)
	}
	if opts.Title != "Bump express from 4.0.0 to 5.0.0" {
		t.Errorf("CreateSession Title = %q, want the PR title verbatim", opts.Title)
	}
	if opts.HeadBranch != "dependabot/npm_and_yarn/express-5.0.0" {
		t.Errorf("CreateSession HeadBranch = %q, want the dependabot branch", opts.HeadBranch)
	}
	// Plugin labels the task with "dependabot", which handleCreateSession
	// keys on to set SkipSetupScript=true. If the label plumbing breaks,
	// the session would run the repo's setup script against a PR branch
	// the setup script doesn't know about — a real production hazard.
	if !opts.SkipSetupScript {
		t.Errorf("CreateSession SkipSetupScript = false, want true for dependabot tasks")
	}
	// parsePRNumberFromExternalID pulls the trailing integer from the
	// external ID; a regression where that parsing breaks would leave
	// PRNumber nil and the TUI wouldn't render a clickable PR link.
	if opts.PRNumber == nil {
		t.Error("CreateSession PRNumber = nil, want 77")
	} else if *opts.PRNumber != 77 {
		t.Errorf("CreateSession PRNumber = %d, want 77", *opts.PRNumber)
	}
	if opts.Plan == "" {
		t.Error("CreateSession Plan is empty; plugin should have supplied a fix plan")
	}

	// Task mapping should exist and be marked InProgress: handleCreateSession
	// does not mark terminal status — that happens when HandleSessionCompleted
	// fires, which our recording creator doesn't drive.
	externalID := "dependabot:pr:" + h.repoOriginURL + ":77"
	mapping, err := h.taskMappings.GetByExternalID(context.Background(), externalID)
	if err != nil {
		t.Fatalf("GetByExternalID: %v", err)
	}
	if mapping == nil {
		t.Fatal("expected task mapping to exist for CREATE_SESSION task")
	}
	if mapping.PluginName != "dependabot" {
		t.Errorf("mapping PluginName = %q, want %q", mapping.PluginName, "dependabot")
	}

	// No merge should occur on the CREATE_SESSION path.
	if merges := h.provider.MergeCalls(); len(merges) != 0 {
		t.Errorf("expected 0 MergePR calls for CREATE_SESSION, got %d: %+v", len(merges), merges)
	}
}
