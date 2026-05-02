package db

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/models"
)

func createTestCronJob(t *testing.T, store *SQLiteCronJobStore, repoID, name string) *models.CronJob {
	t.Helper()
	job, err := store.Create(context.Background(), CreateCronJobParams{
		RepoID:   repoID,
		Name:     name,
		Prompt:   "Run health checks and report failures",
		Schedule: "0 9 * * *",
		Enabled:  true,
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
	job, err := store.Create(ctx, CreateCronJobParams{
		RepoID:   repo.ID,
		Name:     "Daily summary",
		Prompt:   "Summarize yesterday's PR activity",
		Schedule: "0 9 * * *",
		Timezone: &tz,
		Enabled:  true,
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
	if !job.Enabled {
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
}

func TestCronJobStore_Create_NilTimezoneAndDisabled(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	store := NewCronJobStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)

	job, err := store.Create(ctx, CreateCronJobParams{
		RepoID:   repo.ID,
		Name:     "Disabled job",
		Prompt:   "noop",
		Schedule: "@daily",
		Enabled:  false,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if job.Timezone != nil {
		t.Errorf("timezone = %v, want nil", job.Timezone)
	}
	if job.Enabled {
		t.Error("enabled should be false")
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
		RepoID:   repo.ID,
		Name:     "duplicate",
		Prompt:   "second",
		Schedule: "@hourly",
		Enabled:  true,
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
		RepoID:   repoB.ID,
		Name:     "shared-name",
		Prompt:   "ok",
		Schedule: "@hourly",
		Enabled:  true,
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

	all, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("list len = %d, want 3", len(all))
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
}

func TestCronJobStore_ListEnabled(t *testing.T) {
	db := setupTestDB(t)
	repoStore := NewRepoStore(db)
	store := NewCronJobStore(db)
	ctx := context.Background()

	repo := createTestRepo(t, repoStore)
	createTestCronJob(t, store, repo.ID, "enabled-1")
	disabled, _ := store.Create(ctx, CreateCronJobParams{
		RepoID:   repo.ID,
		Name:     "disabled-1",
		Prompt:   "noop",
		Schedule: "@daily",
		Enabled:  false,
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
	disabled := false
	nextRun := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Millisecond)
	nextRunPtr := &nextRun

	updated, err := store.Update(ctx, job.ID, UpdateCronJobParams{
		Name:      &newName,
		Prompt:    &newPrompt,
		Schedule:  &newSchedule,
		Timezone:  &tzPtr,
		Enabled:   &disabled,
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
	if updated.Enabled {
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
