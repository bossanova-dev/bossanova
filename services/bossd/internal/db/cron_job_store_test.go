package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/models"
)

func createTestCronJob(t *testing.T, store *SQLiteCronJobStore, repoID, name string) *models.CronJob {
	t.Helper()
	job, err := store.Create(context.Background(), CreateCronJobParams{
		RepoID:    repoID,
		Name:      name,
		Prompt:    "Run health checks and report failures",
		Schedule:  "0 9 * * *",
		IsEnabled: true,
	})
	if err != nil {
		t.Fatalf("create cron job: %v", err)
	}
	return job
}

func TestCronJobStore_CreateAndGet(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	store := NewCronJobStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)

	tz := "America/New_York"
	agentName := "codex"
	job, err := store.Create(ctx, CreateCronJobParams{
		RepoID:    repo.ID,
		Name:      "Daily summary",
		Prompt:    "Summarize yesterday's PR activity",
		Schedule:  "0 9 * * *",
		Timezone:  &tz,
		AgentName: agentName,
		IsEnabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if job.ID == "" {
		t.Error("id should not be empty")
	}
	if job.RepoID != repo.ID {
		t.Errorf("repo_id = %q, want %q", job.RepoID, repo.ID)
	}
	if job.Name != "Daily summary" {
		t.Errorf("name = %q, want %q", job.Name, "Daily summary")
	}
	if job.Prompt != "Summarize yesterday's PR activity" {
		t.Errorf("prompt = %q", job.Prompt)
	}
	if job.Schedule != "0 9 * * *" {
		t.Errorf("schedule = %q", job.Schedule)
	}
	if job.Timezone == nil || *job.Timezone != "America/New_York" {
		t.Errorf("timezone = %v, want America/New_York", job.Timezone)
	}
	if job.AgentName != "codex" {
		t.Errorf("agent_name = %q, want codex", job.AgentName)
	}
	if !job.IsEnabled {
		t.Error("enabled should be true")
	}
	if job.LastRunSessionID != nil {
		t.Errorf("last_run_session_id = %v, want nil", job.LastRunSessionID)
	}
	if job.LastRunAt != nil {
		t.Errorf("last_run_at = %v, want nil", job.LastRunAt)
	}
	if job.LastRunOutcome != nil {
		t.Errorf("last_run_outcome = %v, want nil", job.LastRunOutcome)
	}
	if job.NextRunAt != nil {
		t.Errorf("next_run_at = %v, want nil", job.NextRunAt)
	}

	got, err := store.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != job.ID {
		t.Errorf("id = %q, want %q", got.ID, job.ID)
	}
	if got.Timezone == nil || *got.Timezone != "America/New_York" {
		t.Errorf("get: timezone = %v", got.Timezone)
	}
	if got.AgentName != "codex" {
		t.Errorf("get: agent_name = %q, want codex", got.AgentName)
	}
}

func TestCronJobStore_PersistsModel(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	store := NewCronJobStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)

	job, err := store.Create(ctx, CreateCronJobParams{
		RepoID: repo.ID, Name: "m", Prompt: "/x", Schedule: "@hourly",
		AgentName: "claude", Model: "sonnet", IsEnabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if job.Model != "sonnet" {
		t.Fatalf("create returned model %q, want sonnet", job.Model)
	}

	got, err := store.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Model != "sonnet" {
		t.Fatalf("reloaded model %q, want sonnet", got.Model)
	}

	// Update round-trips the model (pointer-on-changed).
	newModel := "opus"
	updated, err := store.Update(ctx, job.ID, UpdateCronJobParams{Model: &newModel})
	if err != nil {
		t.Fatalf("update model: %v", err)
	}
	if updated.Model != "opus" {
		t.Fatalf("updated model %q, want opus", updated.Model)
	}

	// Empty model is a legitimate value (plugin default), not "no change".
	empty := ""
	cleared, err := store.Update(ctx, job.ID, UpdateCronJobParams{Model: &empty})
	if err != nil {
		t.Fatalf("clear model: %v", err)
	}
	if cleared.Model != "" {
		t.Fatalf("cleared model %q, want empty", cleared.Model)
	}
}

func TestCronJobStore_Create_DefaultsBlankAgentToClaude(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	store := NewCronJobStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)

	job, err := store.Create(ctx, CreateCronJobParams{
		RepoID:    repo.ID,
		Name:      "Disabled job",
		Prompt:    "noop",
		Schedule:  "@daily",
		IsEnabled: false,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if job.Timezone != nil {
		t.Errorf("timezone = %v, want nil", job.Timezone)
	}
	if job.IsEnabled {
		t.Error("enabled should be false")
	}
	if job.AgentName != "claude" {
		t.Errorf("agent_name = %q, want claude", job.AgentName)
	}
}

func TestCronJobStore_Get_DefaultsLegacyRowAgentToClaude(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	store := NewCronJobStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	_, err := db.ExecContext(ctx,
		`INSERT INTO cron_jobs (id, repo_id, name, prompt, schedule, is_enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"cron-legacy", repo.ID, "Legacy", "Run checks", "@daily", 1,
		"2026-06-04T00:00:00.000Z", "2026-06-04T00:00:00.000Z",
	)
	if err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	got, err := store.Get(ctx, "cron-legacy")
	if err != nil {
		t.Fatalf("get legacy row: %v", err)
	}
	if got.AgentName != "claude" {
		t.Errorf("agent_name = %q, want claude", got.AgentName)
	}
}

func TestCronJobStore_Create_UniqueRepoName(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	store := NewCronJobStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	createTestCronJob(t, store, repo.ID, "duplicate")

	_, err := store.Create(ctx, CreateCronJobParams{
		RepoID:    repo.ID,
		Name:      "duplicate",
		Prompt:    "second",
		Schedule:  "@hourly",
		IsEnabled: true,
	})
	if err == nil {
		t.Fatal("expected UNIQUE constraint failure")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Errorf("error %q does not mention UNIQUE constraint", err)
	}
}

func TestCronJobStore_Create_SameNameDifferentRepo(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	store := NewCronJobStore(db)
	ctx := context.Background()

	repoA := createTestRepo(t, repoStore)
	repoB, err := repoStore.Create(ctx, CreateRepoParams{
		DisplayName:       "repo-b",
		LocalPath:         "/tmp/repo-b",
		OriginURL:         "https://github.com/test/b.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/wt/b",
	})
	if err != nil {
		t.Fatalf("create repoB: %v", err)
	}

	createTestCronJob(t, store, repoA.ID, "shared-name")
	if _, err := store.Create(ctx, CreateCronJobParams{
		RepoID:    repoB.ID,
		Name:      "shared-name",
		Prompt:    "ok",
		Schedule:  "@hourly",
		IsEnabled: true,
	}); err != nil {
		t.Fatalf("create in second repo: %v", err)
	}
}

func TestCronJobStore_Get_NotFound(t *testing.T) {
	db := setupTestDB(t)
	store := NewCronJobStore(db)

	_, err := store.Get(context.Background(), "no-such-id")
	if err != sql.ErrNoRows {
		t.Errorf("got %v, want sql.ErrNoRows", err)
	}
}

func TestCronJobStore_ListAndListByRepo(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	store := NewCronJobStore(db)
	ctx := context.Background()

	repoA := createTestRepo(t, repoStore)
	repoB, _ := repoStore.Create(ctx, CreateRepoParams{
		DisplayName:       "repo-b",
		LocalPath:         "/tmp/repo-b",
		OriginURL:         "https://github.com/test/b.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/wt/b",
	})

	createTestCronJob(t, store, repoA.ID, "a-job-1")
	createTestCronJob(t, store, repoA.ID, "a-job-2")
	createTestCronJob(t, store, repoB.ID, "b-job-1")
	// Mixed-case names: binary collation places "Banana" (B=0x42) before
	// "apple" (a=0x61) and would return [Banana, a-job-1, a-job-2, apple,
	// b-job-1, cherry]. COLLATE NOCASE interleaves them alphabetically as
	// [a-job-1, a-job-2, apple, b-job-1, Banana, cherry].
	createTestCronJob(t, store, repoB.ID, "apple")
	createTestCronJob(t, store, repoB.ID, "Banana")
	createTestCronJob(t, store, repoB.ID, "cherry")

	all, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// Fatal so a length mismatch can't silently skip the ordering assertions below.
	if len(all) != 6 {
		t.Fatalf("list len = %d, want 6", len(all))
	}

	// Verify List() returns jobs case-insensitively alphabetical by name.
	// This assertion fails under binary collation (Banana sorts before apple)
	// and passes only with COLLATE NOCASE.
	wantNames := []string{"a-job-1", "a-job-2", "apple", "b-job-1", "Banana", "cherry"}
	for i, want := range wantNames {
		if all[i].Name != want {
			t.Errorf("List()[%d].Name = %q, want %q (COLLATE NOCASE order)", i, all[i].Name, want)
		}
	}

	a, err := store.ListByRepo(ctx, repoA.ID)
	if err != nil {
		t.Fatalf("list by repo a: %v", err)
	}
	if len(a) != 2 {
		t.Errorf("list by repo a len = %d, want 2", len(a))
	}
	if a[0].Name != "a-job-1" || a[1].Name != "a-job-2" {
		t.Errorf("repo a sort: got %q, %q; want a-job-1, a-job-2", a[0].Name, a[1].Name)
	}

	// ListByRepo must use the identical COLLATE NOCASE ordering as List(); assert
	// it independently so a regression in only ListByRepo cannot hide behind the
	// global List() assertion. repoB holds b-job-1, apple, Banana, cherry; NOCASE
	// orders them apple, b-job-1, Banana, cherry ("b-job-1" < "Banana" because
	// '-' (0x2D) < 'a' (0x61) at the second position).
	b, err := store.ListByRepo(ctx, repoB.ID)
	if err != nil {
		t.Fatalf("list by repo b: %v", err)
	}
	wantB := []string{"apple", "b-job-1", "Banana", "cherry"}
	if len(b) != len(wantB) {
		t.Fatalf("list by repo b len = %d, want %d", len(b), len(wantB))
	}
	for i, want := range wantB {
		if b[i].Name != want {
			t.Errorf("ListByRepo(repoB)[%d].Name = %q, want %q (COLLATE NOCASE order)", i, b[i].Name, want)
		}
	}
}

func TestCronJobStore_ListEnabled(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	store := NewCronJobStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	createTestCronJob(t, store, repo.ID, "enabled-1")
	disabled, _ := store.Create(ctx, CreateCronJobParams{
		RepoID:    repo.ID,
		Name:      "disabled-1",
		Prompt:    "noop",
		Schedule:  "@daily",
		IsEnabled: false,
	})

	got, err := store.ListEnabled(ctx)
	if err != nil {
		t.Fatalf("list enabled: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("list enabled len = %d, want 1", len(got))
	}
	if got[0].ID == disabled.ID {
		t.Error("disabled job should not be in ListEnabled output")
	}
}

func TestCronJobStore_Update(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	store := NewCronJobStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	job := createTestCronJob(t, store, repo.ID, "original")

	newName := "renamed"
	newPrompt := "Updated prompt body"
	newSchedule := "0 12 * * *"
	tz := "Europe/London"
	tzPtr := &tz
	agentName := "codex"
	disabled := false
	nextRun := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Millisecond)
	nextRunPtr := &nextRun

	updated, err := store.Update(ctx, job.ID, UpdateCronJobParams{
		Name:      &newName,
		Prompt:    &newPrompt,
		Schedule:  &newSchedule,
		Timezone:  &tzPtr,
		AgentName: &agentName,
		IsEnabled: &disabled,
		NextRunAt: &nextRunPtr,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Name != "renamed" {
		t.Errorf("name = %q, want renamed", updated.Name)
	}
	if updated.Prompt != newPrompt {
		t.Errorf("prompt = %q", updated.Prompt)
	}
	if updated.Schedule != newSchedule {
		t.Errorf("schedule = %q", updated.Schedule)
	}
	if updated.Timezone == nil || *updated.Timezone != "Europe/London" {
		t.Errorf("timezone = %v", updated.Timezone)
	}
	if updated.AgentName != "codex" {
		t.Errorf("agent_name = %q, want codex", updated.AgentName)
	}
	if updated.IsEnabled {
		t.Error("enabled should be false")
	}
	if updated.NextRunAt == nil || !updated.NextRunAt.Equal(nextRun) {
		t.Errorf("next_run_at = %v, want %v", updated.NextRunAt, nextRun)
	}

	// Clearing timezone (double-pointer with *nil) sets the column to NULL.
	var nilTZ *string
	cleared, err := store.Update(ctx, job.ID, UpdateCronJobParams{Timezone: &nilTZ})
	if err != nil {
		t.Fatalf("clear timezone: %v", err)
	}
	if cleared.Timezone != nil {
		t.Errorf("timezone after clear = %v, want nil", cleared.Timezone)
	}

	// Clearing next_run_at (double-pointer with *nil).
	var nilNext *time.Time
	cleared2, err := store.Update(ctx, job.ID, UpdateCronJobParams{NextRunAt: &nilNext})
	if err != nil {
		t.Fatalf("clear next_run_at: %v", err)
	}
	if cleared2.NextRunAt != nil {
		t.Errorf("next_run_at after clear = %v, want nil", cleared2.NextRunAt)
	}

	blankAgent := " "
	defaulted, err := store.Update(ctx, job.ID, UpdateCronJobParams{AgentName: &blankAgent})
	if err != nil {
		t.Fatalf("blank agent update: %v", err)
	}
	if defaulted.AgentName != "claude" {
		t.Errorf("agent_name after blank update = %q, want claude", defaulted.AgentName)
	}
}

func TestCronJobStore_Update_NotFound(t *testing.T) {
	db := setupTestDB(t)
	store := NewCronJobStore(db)
	name := "x"
	_, err := store.Update(context.Background(), "missing", UpdateCronJobParams{Name: &name})
	if err != sql.ErrNoRows {
		t.Errorf("got %v, want sql.ErrNoRows", err)
	}
}

func TestCronJobStore_UpdateLastRun(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	store := NewCronJobStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	job := createTestCronJob(t, store, repo.ID, "lr-test")
	sess, _ := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "lr-test session",
		WorktreePath: "/tmp/wt/lr",
		BranchName:   "feat/lr",
		BaseBranch:   "main",
	})

	ranAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	nextRun := ranAt.Add(24 * time.Hour)
	if err := store.UpdateLastRun(ctx, job.ID, UpdateCronJobLastRunParams{
		SessionID: &sess.ID,
		RanAt:     ranAt,
		Outcome:   models.CronJobOutcomePRCreated,
		NextRunAt: &nextRun,
	}); err != nil {
		t.Fatalf("UpdateLastRun: %v", err)
	}

	got, err := store.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.LastRunSessionID == nil || *got.LastRunSessionID != sess.ID {
		t.Errorf("last_run_session_id = %v, want %s", got.LastRunSessionID, sess.ID)
	}
	if got.LastRunAt == nil || !got.LastRunAt.Equal(ranAt) {
		t.Errorf("last_run_at = %v, want %v", got.LastRunAt, ranAt)
	}
	if got.LastRunOutcome == nil || *got.LastRunOutcome != models.CronJobOutcomePRCreated {
		t.Errorf("last_run_outcome = %v, want pr_created", got.LastRunOutcome)
	}
	if got.NextRunAt == nil || !got.NextRunAt.Equal(nextRun) {
		t.Errorf("next_run_at = %v, want %v", got.NextRunAt, nextRun)
	}

	// A second call with NextRunAt = nil clears next_run_at (e.g. job disabled).
	if err := store.UpdateLastRun(ctx, job.ID, UpdateCronJobLastRunParams{
		RanAt:   time.Now().UTC(),
		Outcome: models.CronJobOutcomeDeletedNoChanges,
	}); err != nil {
		t.Fatalf("UpdateLastRun (clear next): %v", err)
	}
	got2, _ := store.Get(ctx, job.ID)
	if got2.NextRunAt != nil {
		t.Errorf("next_run_at after clear = %v, want nil", got2.NextRunAt)
	}
	if got2.LastRunOutcome == nil || *got2.LastRunOutcome != models.CronJobOutcomeDeletedNoChanges {
		t.Errorf("outcome after second update = %v", got2.LastRunOutcome)
	}
}

func TestCronJobStore_MarkFireStarted(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	store := NewCronJobStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	job := createTestCronJob(t, store, repo.ID, "fire-test")
	sess, _ := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "fired session",
		WorktreePath: "/tmp/wt/fire",
		BranchName:   "feat/fire",
		BaseBranch:   "main",
	})

	firedAt := time.Now().UTC().Truncate(time.Millisecond)
	nextRun := firedAt.Add(time.Hour)
	if err := store.MarkFireStarted(ctx, job.ID, sess.ID, firedAt, &nextRun); err != nil {
		t.Fatalf("MarkFireStarted: %v", err)
	}

	got, err := store.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.LastRunSessionID == nil || *got.LastRunSessionID != sess.ID {
		t.Errorf("last_run_session_id = %v, want %s", got.LastRunSessionID, sess.ID)
	}
	if got.LastRunAt == nil || !got.LastRunAt.Equal(firedAt) {
		t.Errorf("last_run_at = %v, want %v", got.LastRunAt, firedAt)
	}
	if got.NextRunAt == nil || !got.NextRunAt.Equal(nextRun) {
		t.Errorf("next_run_at = %v, want %v", got.NextRunAt, nextRun)
	}
	// last_run_outcome must remain untouched (nil on a fresh job).
	if got.LastRunOutcome != nil {
		t.Errorf("last_run_outcome = %v, want nil (outcome is set by finalize, not fire)", got.LastRunOutcome)
	}

	// A subsequent UpdateLastRun writes the outcome without affecting the
	// session_id that MarkFireStarted set.
	if err := store.UpdateLastRun(ctx, job.ID, UpdateCronJobLastRunParams{
		RanAt:     firedAt,
		Outcome:   models.CronJobOutcomePRCreated,
		NextRunAt: &nextRun,
	}); err != nil {
		t.Fatalf("UpdateLastRun: %v", err)
	}
	got2, _ := store.Get(ctx, job.ID)
	if got2.LastRunSessionID == nil || *got2.LastRunSessionID != sess.ID {
		t.Errorf("last_run_session_id changed across UpdateLastRun: got %v", got2.LastRunSessionID)
	}
	if got2.LastRunOutcome == nil || *got2.LastRunOutcome != models.CronJobOutcomePRCreated {
		t.Errorf("outcome = %v, want pr_created", got2.LastRunOutcome)
	}

	// Clearing next_run_at.
	if err := store.MarkFireStarted(ctx, job.ID, sess.ID, firedAt, nil); err != nil {
		t.Fatalf("MarkFireStarted (clear next): %v", err)
	}
	got3, _ := store.Get(ctx, job.ID)
	if got3.NextRunAt != nil {
		t.Errorf("next_run_at after clear = %v, want nil", got3.NextRunAt)
	}
}

func TestCronJobStore_MarkFireStarted_ClearsStaleOutcome(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	store := NewCronJobStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	job := createTestCronJob(t, store, repo.ID, "outcome-test")

	// Record a prior outcome.
	if err := store.UpdateLastRun(ctx, job.ID, UpdateCronJobLastRunParams{
		RanAt:   time.Now().UTC(),
		Outcome: models.CronJobOutcomeDeletedNoChanges,
	}); err != nil {
		t.Fatalf("UpdateLastRun: %v", err)
	}

	sess, _ := sessionStore.Create(ctx, CreateSessionParams{
		RepoID: repo.ID, Title: "next fire", WorktreePath: "/tmp/wt/next",
		BranchName: "feat/next", BaseBranch: "main",
	})
	if err := store.MarkFireStarted(ctx, job.ID, sess.ID, time.Now().UTC(), nil); err != nil {
		t.Fatalf("MarkFireStarted: %v", err)
	}

	got, err := store.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// models.CronJob.LastRunOutcome is *CronJobOutcome; cleared == nil.
	if got.LastRunOutcome != nil {
		t.Fatalf("LastRunOutcome=%v, want nil (cleared)", *got.LastRunOutcome)
	}
}

// TestCronJobStore_UpdateLastRun_ExpectedSessionGuard verifies that a guarded
// UpdateLastRun (ExpectedSessionID set) only writes while last_run_session_id
// still matches, so an older run's late finalize cannot move the pointer back
// over a newer run that already called MarkFireStarted (the overlap bug).
func TestCronJobStore_UpdateLastRun_ExpectedSessionGuard(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	store := NewCronJobStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	job := createTestCronJob(t, store, repo.ID, "guard-test")

	mkSession := func(name, path string) string {
		s, err := sessionStore.Create(ctx, CreateSessionParams{
			RepoID: repo.ID, Title: name, WorktreePath: path,
			BranchName: "feat/" + name, BaseBranch: "main",
		})
		if err != nil {
			t.Fatalf("create session %s: %v", name, err)
		}
		return s.ID
	}

	runA := mkSession("run-a", "/tmp/wt/a")
	runB := mkSession("run-b", "/tmp/wt/b")

	// Run A fires, then the next tick fires run B (B's MarkFireStarted moves
	// the pointer forward while A is still idle in ImplementingPlan).
	if err := store.MarkFireStarted(ctx, job.ID, runA, time.Now().UTC(), nil); err != nil {
		t.Fatalf("MarkFireStarted A: %v", err)
	}
	if err := store.MarkFireStarted(ctx, job.ID, runB, time.Now().UTC(), nil); err != nil {
		t.Fatalf("MarkFireStarted B: %v", err)
	}

	// A finalizes late. A guarded write expecting A must be a no-op (B owns the
	// pointer now) and report the supersede sentinel.
	err := store.UpdateLastRun(ctx, job.ID, UpdateCronJobLastRunParams{
		SessionID:         &runA,
		ExpectedSessionID: &runA,
		RanAt:             time.Now().UTC(),
		Outcome:           models.CronJobOutcomePRCreated,
	})
	if !errors.Is(err, ErrCronJobLastRunSuperseded) {
		t.Fatalf("guarded UpdateLastRun(A): got %v, want ErrCronJobLastRunSuperseded", err)
	}
	got, _ := store.Get(ctx, job.ID)
	if got.LastRunSessionID == nil || *got.LastRunSessionID != runB {
		t.Fatalf("pointer moved off newer run: got %v, want %s", got.LastRunSessionID, runB)
	}
	if got.LastRunOutcome != nil {
		t.Fatalf("superseded outcome leaked: got %v, want nil", got.LastRunOutcome)
	}

	// A guarded write expecting the current run B succeeds and records the outcome.
	if err := store.UpdateLastRun(ctx, job.ID, UpdateCronJobLastRunParams{
		SessionID:         &runB,
		ExpectedSessionID: &runB,
		RanAt:             time.Now().UTC(),
		Outcome:           models.CronJobOutcomePRCreated,
	}); err != nil {
		t.Fatalf("guarded UpdateLastRun(B): %v", err)
	}
	got2, _ := store.Get(ctx, job.ID)
	if got2.LastRunOutcome == nil || *got2.LastRunOutcome != models.CronJobOutcomePRCreated {
		t.Fatalf("outcome for current run not recorded: got %v", got2.LastRunOutcome)
	}
}

func TestCronJobStore_UpdateLastRun_DeletedSessionGuardAllowsClearedPointer(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	store := NewCronJobStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	job := createTestCronJob(t, store, repo.ID, "deleted-guard-test")

	runA, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID: repo.ID, Title: "run-a", WorktreePath: "/tmp/wt/a",
		BranchName: "feat/run-a", BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("create run A: %v", err)
	}
	if err := store.MarkFireStarted(ctx, job.ID, runA.ID, time.Now().UTC(), nil); err != nil {
		t.Fatalf("MarkFireStarted A: %v", err)
	}
	if err := sessionStore.Delete(ctx, runA.ID); err != nil {
		t.Fatalf("delete run A: %v", err)
	}

	// Deleting run A clears last_run_session_id; the deleted-no-change finalizer
	// still owns that cleared pointer and may record its outcome.
	if err := store.UpdateLastRun(ctx, job.ID, UpdateCronJobLastRunParams{
		ExpectedSessionID:             &runA.ID,
		AllowClearedExpectedSessionID: true,
		RanAt:                         time.Now().UTC(),
		Outcome:                       models.CronJobOutcomeDeletedNoChanges,
	}); err != nil {
		t.Fatalf("guarded deleted UpdateLastRun(A): %v", err)
	}
	got, _ := store.Get(ctx, job.ID)
	if got.LastRunOutcome == nil || *got.LastRunOutcome != models.CronJobOutcomeDeletedNoChanges {
		t.Fatalf("deleted outcome not recorded: got %v", got.LastRunOutcome)
	}

	runB, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID: repo.ID, Title: "run-b", WorktreePath: "/tmp/wt/b",
		BranchName: "feat/run-b", BaseBranch: "main",
	})
	if err != nil {
		t.Fatalf("create run B: %v", err)
	}
	if err := store.MarkFireStarted(ctx, job.ID, runB.ID, time.Now().UTC(), nil); err != nil {
		t.Fatalf("MarkFireStarted B: %v", err)
	}

	err = store.UpdateLastRun(ctx, job.ID, UpdateCronJobLastRunParams{
		ExpectedSessionID:             &runA.ID,
		AllowClearedExpectedSessionID: true,
		RanAt:                         time.Now().UTC(),
		Outcome:                       models.CronJobOutcomeDeletedNoChanges,
	})
	if !errors.Is(err, ErrCronJobLastRunSuperseded) {
		t.Fatalf("guarded deleted UpdateLastRun(A after B): got %v, want ErrCronJobLastRunSuperseded", err)
	}
	got2, _ := store.Get(ctx, job.ID)
	if got2.LastRunSessionID == nil || *got2.LastRunSessionID != runB.ID {
		t.Fatalf("pointer moved off newer run: got %v, want %s", got2.LastRunSessionID, runB.ID)
	}
	if got2.LastRunOutcome != nil {
		t.Fatalf("superseded deleted outcome leaked: got %v, want nil", got2.LastRunOutcome)
	}
}

func TestCronJobStore_MarkFireStarted_NotFound(t *testing.T) {
	db := setupTestDB(t)
	store := NewCronJobStore(db)
	firedAt := time.Now().UTC()
	err := store.MarkFireStarted(context.Background(), "missing", "sess", firedAt, nil)
	if err != sql.ErrNoRows {
		t.Errorf("got %v, want sql.ErrNoRows", err)
	}
}

// TestCronJobStore_DeleteSetsSessionFKToNull verifies that deleting a cron job
// sets sessions.cron_job_id to NULL (ON DELETE SET NULL) rather than cascading
// the delete to the spawned session row.
func TestCronJobStore_DeleteSetsSessionFKToNull(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	sessionStore := NewSessionStore(db)
	store := NewCronJobStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	job := createTestCronJob(t, store, repo.ID, "delete-test")
	sess, _ := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "spawned by cron",
		WorktreePath: "/tmp/wt/del",
		BranchName:   "feat/del",
		BaseBranch:   "main",
	})

	// Manually link the session to the cron job. UpdateSessionParams does not
	// expose cron_job_id (only the lifecycle code sets it on creation), so
	// drive it via raw SQL for the test.
	if _, err := db.ExecContext(ctx,
		"UPDATE sessions SET cron_job_id = ? WHERE id = ?", job.ID, sess.ID,
	); err != nil {
		t.Fatalf("link session to cron job: %v", err)
	}

	if err := store.Delete(ctx, job.ID); err != nil {
		t.Fatalf("delete cron job: %v", err)
	}

	// Session row must remain.
	if _, err := sessionStore.Get(ctx, sess.ID); err != nil {
		t.Fatalf("session should still exist after cron job delete: %v", err)
	}

	// Session's cron_job_id must now be NULL.
	var cronJobID sql.NullString
	if err := db.QueryRowContext(ctx,
		"SELECT cron_job_id FROM sessions WHERE id = ?", sess.ID,
	).Scan(&cronJobID); err != nil {
		t.Fatalf("scan session cron_job_id: %v", err)
	}
	if cronJobID.Valid {
		t.Errorf("cron_job_id = %q, want NULL", cronJobID.String)
	}
}

// TestCronJobStore_DeleteCascadeFromRepo verifies that deleting a repo cascades
// to its cron jobs (FK ON DELETE CASCADE on repo_id).
func TestCronJobStore_DeleteCascadeFromRepo(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	store := NewCronJobStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	job := createTestCronJob(t, store, repo.ID, "cascade-test")

	if err := repoStore.Delete(ctx, repo.ID); err != nil {
		t.Fatalf("delete repo: %v", err)
	}
	if _, err := store.Get(ctx, job.ID); err != sql.ErrNoRows {
		t.Errorf("cron job should be deleted by repo cascade: got %v", err)
	}
}

func TestCronJobStore_Delete_NotFound(t *testing.T) {
	db := setupTestDB(t)
	store := NewCronJobStore(db)
	if err := store.Delete(context.Background(), "missing"); err != sql.ErrNoRows {
		t.Errorf("got %v, want sql.ErrNoRows", err)
	}
}

// TestCronJobStore_GateCommandAndRunSetupCommand_RoundTrip verifies that
// gate_command and run_setup_command are persisted correctly on Create and
// returned by Get.
func TestCronJobStore_GateCommandAndRunSetupCommand_RoundTrip(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	store := NewCronJobStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)

	job, err := store.Create(ctx, CreateCronJobParams{
		RepoID:                repo.ID,
		Name:                  "gated-job",
		Prompt:                "Run checks",
		Schedule:              "@daily",
		IsEnabled:             true,
		GateCommand:           "make gate-check",
		ShouldRunSetupCommand: false,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if job.GateCommand != "make gate-check" {
		t.Errorf("create: GateCommand = %q, want %q", job.GateCommand, "make gate-check")
	}
	if job.ShouldRunSetupCommand != false {
		t.Errorf("create: ShouldRunSetupCommand = %v, want false", job.ShouldRunSetupCommand)
	}

	got, err := store.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.GateCommand != "make gate-check" {
		t.Errorf("get: GateCommand = %q, want %q", got.GateCommand, "make gate-check")
	}
	if got.ShouldRunSetupCommand != false {
		t.Errorf("get: ShouldRunSetupCommand = %v, want false", got.ShouldRunSetupCommand)
	}
}

// TestCronJobStore_GateCommand_DefaultRow verifies that a row inserted without
// gate_command/run_setup_command (simulating a pre-migration row) is read back
// with empty GateCommand and ShouldRunSetupCommand=true (from the column DEFAULT 1).
func TestCronJobStore_GateCommand_DefaultRow(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	store := NewCronJobStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	_, err := db.ExecContext(ctx,
		`INSERT INTO cron_jobs (id, repo_id, name, prompt, schedule, is_enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"cron-gate-legacy", repo.ID, "Legacy Gate", "Run checks", "@daily", 1,
		"2026-06-27T00:00:00.000Z", "2026-06-27T00:00:00.000Z",
	)
	if err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	got, err := store.Get(ctx, "cron-gate-legacy")
	if err != nil {
		t.Fatalf("get legacy row: %v", err)
	}
	if got.GateCommand != "" {
		t.Errorf("GateCommand = %q, want empty", got.GateCommand)
	}
	if !got.ShouldRunSetupCommand {
		t.Errorf("ShouldRunSetupCommand = false, want true (migration default 1)")
	}
}

// TestCronJobStore_GateCommand_Update verifies that Update can set, change, and
// leave-unchanged GateCommand and ShouldRunSetupCommand.
func TestCronJobStore_GateCommand_Update(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	store := NewCronJobStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	job, err := store.Create(ctx, CreateCronJobParams{
		RepoID:                repo.ID,
		Name:                  "update-gate-job",
		Prompt:                "noop",
		Schedule:              "@daily",
		IsEnabled:             true,
		GateCommand:           "make old-gate",
		ShouldRunSetupCommand: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Set GateCommand and toggle ShouldRunSetupCommand.
	newGate := "make new-gate"
	disableSetup := false
	updated, err := store.Update(ctx, job.ID, UpdateCronJobParams{
		GateCommand:           &newGate,
		ShouldRunSetupCommand: &disableSetup,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.GateCommand != "make new-gate" {
		t.Errorf("after update: GateCommand = %q, want %q", updated.GateCommand, "make new-gate")
	}
	if updated.ShouldRunSetupCommand != false {
		t.Errorf("after update: ShouldRunSetupCommand = %v, want false", updated.ShouldRunSetupCommand)
	}

	// Nil params leave them unchanged.
	unchanged, err := store.Update(ctx, job.ID, UpdateCronJobParams{})
	if err != nil {
		t.Fatalf("no-op update: %v", err)
	}
	if unchanged.GateCommand != "make new-gate" {
		t.Errorf("no-op: GateCommand = %q, want %q", unchanged.GateCommand, "make new-gate")
	}
	if unchanged.ShouldRunSetupCommand != false {
		t.Errorf("no-op: ShouldRunSetupCommand = %v, want false", unchanged.ShouldRunSetupCommand)
	}

	// Clear GateCommand by setting to empty string.
	emptyGate := ""
	cleared, err := store.Update(ctx, job.ID, UpdateCronJobParams{GateCommand: &emptyGate})
	if err != nil {
		t.Fatalf("clear gate: %v", err)
	}
	if cleared.GateCommand != "" {
		t.Errorf("cleared: GateCommand = %q, want empty", cleared.GateCommand)
	}
}

// TestCronJobStore_ZeroOutput_CreateRoundTrip pins the persistence contract for
// the is_zero_output column added in BOS-563: an unset Create defaults it to
// false and an explicit true survives the Create -> Get -> List round trip. The
// List assertion guards against a column being wired into only one SELECT path.
func TestCronJobStore_ZeroOutput_CreateRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	store := NewCronJobStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)

	off, err := store.Create(ctx, CreateCronJobParams{
		RepoID:       repo.ID,
		Name:         "zero-output-off",
		Prompt:       "noop",
		Schedule:     "@daily",
		IsEnabled:    true,
		IsZeroOutput: false,
	})
	if err != nil {
		t.Fatalf("create (false): %v", err)
	}
	if off.IsZeroOutput {
		t.Errorf("create(false): IsZeroOutput = true, want false")
	}
	gotOff, err := store.Get(ctx, off.ID)
	if err != nil {
		t.Fatalf("get (false): %v", err)
	}
	if gotOff.IsZeroOutput {
		t.Errorf("get(false): IsZeroOutput = true, want false")
	}

	on, err := store.Create(ctx, CreateCronJobParams{
		RepoID:       repo.ID,
		Name:         "zero-output-on",
		Prompt:       "noop",
		Schedule:     "@daily",
		IsEnabled:    true,
		IsZeroOutput: true,
	})
	if err != nil {
		t.Fatalf("create (true): %v", err)
	}
	if !on.IsZeroOutput {
		t.Errorf("create(true): IsZeroOutput = false, want true")
	}
	gotOn, err := store.Get(ctx, on.ID)
	if err != nil {
		t.Fatalf("get (true): %v", err)
	}
	if !gotOn.IsZeroOutput {
		t.Errorf("get(true): IsZeroOutput = false, want true")
	}

	// The list path must carry the flag too.
	listed, err := store.ListByRepo(ctx, repo.ID)
	if err != nil {
		t.Fatalf("list by repo: %v", err)
	}
	byID := map[string]bool{}
	for _, j := range listed {
		byID[j.ID] = j.IsZeroOutput
	}
	if got, ok := byID[off.ID]; !ok || got {
		t.Errorf("list: IsZeroOutput for %q = %v (present=%v), want false", off.ID, got, ok)
	}
	if got, ok := byID[on.ID]; !ok || !got {
		t.Errorf("list: IsZeroOutput for %q = %v (present=%v), want true", on.ID, got, ok)
	}
}

// TestCronJobStore_ZeroOutput_Update verifies that Update can both set and
// clear is_zero_output. The clear leg (true -> false) is what proves the
// set-clause actually writes rather than being an accidental no-op.
func TestCronJobStore_ZeroOutput_Update(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	store := NewCronJobStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	job, err := store.Create(ctx, CreateCronJobParams{
		RepoID:    repo.ID,
		Name:      "zero-output-update",
		Prompt:    "noop",
		Schedule:  "@daily",
		IsEnabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if job.IsZeroOutput {
		t.Fatalf("IsZeroOutput = true on creation, want false (default)")
	}

	enable := true
	enabled, err := store.Update(ctx, job.ID, UpdateCronJobParams{IsZeroOutput: &enable})
	if err != nil {
		t.Fatalf("update (true): %v", err)
	}
	if !enabled.IsZeroOutput {
		t.Errorf("after Update(true): IsZeroOutput = false, want true")
	}

	disable := false
	disabled, err := store.Update(ctx, job.ID, UpdateCronJobParams{IsZeroOutput: &disable})
	if err != nil {
		t.Fatalf("update (false): %v", err)
	}
	if disabled.IsZeroOutput {
		t.Errorf("after Update(false): IsZeroOutput = true, want false (cleared)")
	}
	reread, err := store.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("get after clear: %v", err)
	}
	if reread.IsZeroOutput {
		t.Errorf("get after Update(false): IsZeroOutput = true, want false")
	}
}

// TestCronJobStore_ZeroOutput_NilUpdateLeavesUnchanged pins the partial-update
// guard: an Update whose IsZeroOutput pointer is nil must not touch the column,
// even when other fields are being written.
func TestCronJobStore_ZeroOutput_NilUpdateLeavesUnchanged(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	store := NewCronJobStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	job, err := store.Create(ctx, CreateCronJobParams{
		RepoID:       repo.ID,
		Name:         "zero-output-nil-update",
		Prompt:       "noop",
		Schedule:     "@daily",
		IsEnabled:    true,
		IsZeroOutput: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !job.IsZeroOutput {
		t.Fatalf("IsZeroOutput = false after Create(true), want true")
	}

	renamed := "zero-output-nil-update (renamed)"
	updated, err := store.Update(ctx, job.ID, UpdateCronJobParams{Name: &renamed})
	if err != nil {
		t.Fatalf("unrelated update: %v", err)
	}
	if updated.Name != renamed {
		t.Fatalf("Name = %q, want %q", updated.Name, renamed)
	}
	if !updated.IsZeroOutput {
		t.Errorf("after unrelated Update: IsZeroOutput = false, want true (unchanged)")
	}

	// A fully-empty Update must not clear it either.
	noop, err := store.Update(ctx, job.ID, UpdateCronJobParams{})
	if err != nil {
		t.Fatalf("no-op update: %v", err)
	}
	if !noop.IsZeroOutput {
		t.Errorf("after no-op Update: IsZeroOutput = false, want true (unchanged)")
	}
}

// TestCronJobStore_ZeroOutput_DefaultRow verifies that a row inserted without
// is_zero_output (simulating a job created before the migration) reads back
// false, from the column's NOT NULL DEFAULT 0.
func TestCronJobStore_ZeroOutput_DefaultRow(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	store := NewCronJobStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	_, err := db.ExecContext(ctx,
		`INSERT INTO cron_jobs (id, repo_id, name, prompt, schedule, is_enabled, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"cron-zero-output-legacy", repo.ID, "Legacy Zero Output", "noop", "@daily", 1,
		"2026-07-25T00:00:00.000Z", "2026-07-25T00:00:00.000Z",
	)
	if err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	got, err := store.Get(ctx, "cron-zero-output-legacy")
	if err != nil {
		t.Fatalf("get legacy row: %v", err)
	}
	if got.IsZeroOutput {
		t.Errorf("IsZeroOutput = true, want false (migration default 0)")
	}
}
