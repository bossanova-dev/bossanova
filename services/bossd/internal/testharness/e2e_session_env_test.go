package testharness_test

// e2e_session_env_test.go — proves the canonical BOSS_* session environment
// (Layer E / ManagedSessionEnv) actually reaches the spawned tmux session.
//
// The unit tests in services/bossd/internal/session cover what ManagedSessionEnv
// *returns*; this E2E test covers that the returned map is delivered all the way
// to `tmux new-session -e KEY=VALUE`. It drives the real cron spawn path through
// the harness (no agent auth needed) and asserts on the env the tmux fake
// received. The cron path is used because it is the readily-triggerable real
// spawn; the fresh (StartTmuxChat) and wake (spawnChatTmux) paths set the env
// from the identical ManagedSessionEnv call sites, with the cron-vs-non-cron
// branching unit-covered.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/testharness"
)

// tmuxEnvFromNewSession scans the fake's recorded tmux invocations for the
// `new-session` call and returns the environment it carried. The production
// tmux client renders Env as repeated `-e KEY=VALUE` arguments
// (see services/bossd/internal/tmux/tmux.go), so every value following a `-e`
// flag is one session-environment entry.
func tmuxEnvFromNewSession(t *testing.T, fake *testharness.CronReadyTmuxFake) map[string]string {
	t.Helper()
	env := map[string]string{}
	found := false
	for _, call := range fake.Calls() {
		if len(call) == 0 || call[0] != "new-session" {
			continue
		}
		found = true
		args := call[1:]
		for i := 0; i < len(args); i++ {
			if args[i] == "-e" && i+1 < len(args) {
				k, v, ok := strings.Cut(args[i+1], "=")
				if ok {
					env[k] = v
				}
				i++
			}
		}
	}
	if !found {
		t.Fatalf("no tmux new-session call recorded; calls: %v", fake.Calls())
	}
	return env
}

// TestE2ESessionEnvReachesTmux verifies that a real session spawn passes the
// full canonical BOSS_* environment to tmux new-session, including the cron
// superset for a scheduler-spawned session.
func TestE2ESessionEnvReachesTmux(t *testing.T) {
	h, fake := cronTestHarness(t)
	ctx := context.Background()

	repoID := registerTestRepo(t, h, ctx, withWorktreeBaseDir(t.TempDir()))
	useTempWorktrees(t, h)

	job, err := h.CronJobs.Create(ctx, db.CreateCronJobParams{
		RepoID:    repoID,
		Name:      "env-probe",
		Prompt:    "do the thing",
		Schedule:  "* * * * *",
		IsEnabled: true,
	})
	if err != nil {
		t.Fatalf("create cron job: %v", err)
	}

	sched := newCronScheduler(h)
	if err := sched.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Tick spawns the session synchronously, driving the real tmux new-session
	// through the fake.
	sched.Tick(tickTime)

	sess := sessionFromCronJob(t, h, job.ID)
	chats, err := h.AgentChats.ListBySession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("list chats: %v", err)
	}
	if len(chats) != 1 {
		t.Fatalf("expected exactly 1 chat after cron fire, got %d", len(chats))
	}
	agentSessionID := chats[0].AgentSessionID

	env := tmuxEnvFromNewSession(t, fake)

	// Identity vars must match the spawned session/chat exactly.
	exact := map[string]string{
		"BOSS_SESSION_ID":       sess.ID,
		"BOSS_AGENT_SESSION_ID": agentSessionID,
		"BOSS_REPO_ID":          sess.RepoID,
	}
	for k, want := range exact {
		if env[k] != want {
			t.Errorf("tmux env[%s] = %q, want %q", k, env[k], want)
		}
	}

	// These vars must be present and non-empty on every managed spawn.
	for _, k := range []string{"BOSS_AGENT", "BOSS_WORKTREE", "BOSS_SETTINGS_PATH", "BOSS_SOCKET"} {
		if env[k] == "" {
			t.Errorf("tmux env missing/empty %s; full env: %v", k, env)
		}
	}

	// Cron superset: a scheduler-spawned session also carries BOSS_CRON*.
	if env["BOSS_CRON"] != "true" {
		t.Errorf("tmux env BOSS_CRON = %q, want true", env["BOSS_CRON"])
	}
	if env["BOSS_CRON_JOB_ID"] != job.ID {
		t.Errorf("tmux env BOSS_CRON_JOB_ID = %q, want %q", env["BOSS_CRON_JOB_ID"], job.ID)
	}

	// The stale hand-listed MCP tool names must never leak into the env either
	// (they were never env, but this keeps the drift guard close to the spawn).
	for k := range env {
		if strings.Contains(k, "mcp__boss__") {
			t.Errorf("unexpected mcp surface in env key %q", k)
		}
	}
}

// TestE2ESessionEnvIncludesWorktreeDotenv verifies that a worktree's
// repo-local .env is overlaid beneath the managed environment on a real
// spawn: keys only the .env defines reach tmux (so ${VAR} expansions in the
// worktree's .mcp.json resolve), while managed BOSS_* keys always win over
// same-named .env entries.
func TestE2ESessionEnvIncludesWorktreeDotenv(t *testing.T) {
	h, fake := cronTestHarness(t)
	ctx := context.Background()

	repoID := registerTestRepo(t, h, ctx, withWorktreeBaseDir(t.TempDir()))
	useTempWorktrees(t, h, func(dir string) {
		content := "# repo-local env\nLINEAR_API_KEY=lin_api_e2e_probe\nBOSS_SESSION_ID=shadow-attempt\n"
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(content), 0o600); err != nil {
			t.Fatalf("seed worktree .env: %v", err)
		}
	})

	job, err := h.CronJobs.Create(ctx, db.CreateCronJobParams{
		RepoID:    repoID,
		Name:      "dotenv-probe",
		Prompt:    "do the thing",
		Schedule:  "* * * * *",
		IsEnabled: true,
	})
	if err != nil {
		t.Fatalf("create cron job: %v", err)
	}

	sched := newCronScheduler(h)
	if err := sched.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	sched.Tick(tickTime)

	sess := sessionFromCronJob(t, h, job.ID)
	env := tmuxEnvFromNewSession(t, fake)

	if got := env["LINEAR_API_KEY"]; got != "lin_api_e2e_probe" {
		t.Errorf("tmux env LINEAR_API_KEY = %q, want %q (worktree .env not delivered)", got, "lin_api_e2e_probe")
	}
	if got := env["BOSS_SESSION_ID"]; got != sess.ID {
		t.Errorf("tmux env BOSS_SESSION_ID = %q, want managed %q (.env must not shadow managed keys)", got, sess.ID)
	}
}

// linearPtr is a small *string helper for setting repo-config secrets in tests.
func linearPtr(s string) *string { return &s }

// TestE2ESessionEnvIncludesRepoConfigLinearKey verifies that a repo's stored
// LINEAR_API_KEY (db.Repo.LinearAPIKey) is delivered to the spawned tmux session
// even when the worktree has NO .env — the case git worktree add produces, and
// the one that previously fell through to the daemon's ambient key.
func TestE2ESessionEnvIncludesRepoConfigLinearKey(t *testing.T) {
	h, fake := cronTestHarness(t)
	ctx := context.Background()

	repoID := registerTestRepo(t, h, ctx, withWorktreeBaseDir(t.TempDir()))
	if _, err := h.Repos.Update(ctx, repoID, db.UpdateRepoParams{
		LinearAPIKey: linearPtr("lin_from_repo_config"),
	}); err != nil {
		t.Fatalf("set repo LinearAPIKey: %v", err)
	}
	useTempWorktrees(t, h) // no .env seeded

	job, err := h.CronJobs.Create(ctx, db.CreateCronJobParams{
		RepoID:    repoID,
		Name:      "repo-key-probe",
		Prompt:    "do the thing",
		Schedule:  "* * * * *",
		IsEnabled: true,
	})
	if err != nil {
		t.Fatalf("create cron job: %v", err)
	}

	sched := newCronScheduler(h)
	if err := sched.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	sched.Tick(tickTime)

	env := tmuxEnvFromNewSession(t, fake)
	if got := env["LINEAR_API_KEY"]; got != "lin_from_repo_config" {
		t.Errorf("tmux env LINEAR_API_KEY = %q, want %q (repo-config key not delivered)", got, "lin_from_repo_config")
	}
}

// TestE2ESessionEnvWorktreeDotenvOverridesRepoConfig verifies the chosen
// precedence: when BOTH the repo config and the worktree .env set
// LINEAR_API_KEY, the worktree .env value wins.
func TestE2ESessionEnvWorktreeDotenvOverridesRepoConfig(t *testing.T) {
	h, fake := cronTestHarness(t)
	ctx := context.Background()

	repoID := registerTestRepo(t, h, ctx, withWorktreeBaseDir(t.TempDir()))
	if _, err := h.Repos.Update(ctx, repoID, db.UpdateRepoParams{
		LinearAPIKey: linearPtr("lin_from_repo_config"),
	}); err != nil {
		t.Fatalf("set repo LinearAPIKey: %v", err)
	}
	useTempWorktrees(t, h, func(dir string) {
		content := "LINEAR_API_KEY=lin_from_worktree_env\n"
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(content), 0o600); err != nil {
			t.Fatalf("seed worktree .env: %v", err)
		}
	})

	job, err := h.CronJobs.Create(ctx, db.CreateCronJobParams{
		RepoID:    repoID,
		Name:      "override-probe",
		Prompt:    "do the thing",
		Schedule:  "* * * * *",
		IsEnabled: true,
	})
	if err != nil {
		t.Fatalf("create cron job: %v", err)
	}

	sched := newCronScheduler(h)
	if err := sched.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	sched.Tick(tickTime)

	env := tmuxEnvFromNewSession(t, fake)
	if got := env["LINEAR_API_KEY"]; got != "lin_from_worktree_env" {
		t.Errorf("tmux env LINEAR_API_KEY = %q, want %q (worktree .env must override repo config)", got, "lin_from_worktree_env")
	}
}

// TestE2ESessionEnvFailsCleanWithoutAnyLinearKey verifies the fail-clean
// guarantee: with no repo-config key and no worktree .env, LINEAR_API_KEY is
// still emitted to the session — empty — so Linear MCP is unauthenticated rather
// than silently inheriting the daemon's ambient key.
func TestE2ESessionEnvFailsCleanWithoutAnyLinearKey(t *testing.T) {
	h, fake := cronTestHarness(t)
	ctx := context.Background()

	repoID := registerTestRepo(t, h, ctx, withWorktreeBaseDir(t.TempDir()))
	useTempWorktrees(t, h) // no repo key, no .env

	job, err := h.CronJobs.Create(ctx, db.CreateCronJobParams{
		RepoID:    repoID,
		Name:      "failclean-probe",
		Prompt:    "do the thing",
		Schedule:  "* * * * *",
		IsEnabled: true,
	})
	if err != nil {
		t.Fatalf("create cron job: %v", err)
	}

	sched := newCronScheduler(h)
	if err := sched.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	sched.Tick(tickTime)

	env := tmuxEnvFromNewSession(t, fake)
	got, ok := env["LINEAR_API_KEY"]
	if !ok {
		t.Fatalf("tmux env missing LINEAR_API_KEY; the fail-clean guarantee must always emit it (empty). full env: %v", env)
	}
	if got != "" {
		t.Errorf("tmux env LINEAR_API_KEY = %q, want empty (nothing configured → unauthenticated, not the daemon's key)", got)
	}
}
