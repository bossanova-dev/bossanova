package db

import (
	"context"
	"database/sql"
	"slices"
	"testing"
)

type modelUsageFixture struct {
	t        *testing.T
	db       *sql.DB
	repoID   string
	sessions *SQLiteSessionStore
	crons    *SQLiteCronJobStore
}

func (f modelUsageFixture) session(agentName, model, createdAt string) {
	f.t.Helper()
	sess, err := f.sessions.Create(context.Background(), CreateSessionParams{
		RepoID: f.repoID, Title: model, WorktreePath: "/tmp/wt", BranchName: "b-" + createdAt, BaseBranch: "main",
		AgentName: agentName, Model: model,
	})
	if err != nil {
		f.t.Fatalf("create session: %v", err)
	}
	if _, err := f.db.Exec(`UPDATE sessions SET created_at = ? WHERE id = ?`, createdAt, sess.ID); err != nil {
		f.t.Fatalf("set created_at: %v", err)
	}
}

func (f modelUsageFixture) cron(agentName, model, updatedAt string, lastRunAt any) {
	f.t.Helper()
	job, err := f.crons.Create(context.Background(), CreateCronJobParams{
		RepoID: f.repoID, Name: model + updatedAt, Prompt: "p", Schedule: "0 9 * * *", AgentName: agentName, Model: model,
	})
	if err != nil {
		f.t.Fatalf("create cron job: %v", err)
	}
	if _, err := f.db.Exec(`UPDATE cron_jobs SET updated_at = ?, last_run_at = ? WHERE id = ?`, updatedAt, lastRunAt, job.ID); err != nil {
		f.t.Fatalf("set cron times: %v", err)
	}
}

func TestModelUsageStoreListsRecentModelsNewestFirst(t *testing.T) {
	sqlDB := setupTestDB(t)
	f := modelUsageFixture{t: t, db: sqlDB, repoID: createTestRepo(t, NewRepoStore(sqlDB)).ID, sessions: NewSessionStore(sqlDB), crons: NewCronJobStore(sqlDB)}
	f.session("claude", "opus", "2026-11-01T00:00:00.000Z")
	f.session("claude", "sonnet", "2026-11-02T00:00:00.000Z")
	f.session("claude", "opus", "2026-11-05T00:00:00.000Z")
	f.session("claude", "", "2026-11-09T00:00:00.000Z")
	f.session("codex", "gpt-6-sol", "2026-11-09T00:00:00.000Z")
	// The cron job last ran after every session, though it was edited before them.
	f.cron("claude", "fable", "2026-10-01T00:00:00.000Z", "2026-11-08T00:00:00.000Z")
	// A job that never ran counts from its last edit.
	f.cron("claude", "haiku", "2026-11-03T00:00:00.000Z", nil)

	store := NewModelUsageStore(sqlDB)
	got, err := store.ListRecent(context.Background(), "claude", 10)
	if err != nil {
		t.Fatalf("ListRecent: %v", err)
	}
	if want := []string{"fable", "opus", "haiku", "sonnet"}; !slices.Equal(got, want) {
		t.Fatalf("ListRecent = %v, want %v", got, want)
	}

	got, err = store.ListRecent(context.Background(), "claude", 2)
	if err != nil {
		t.Fatalf("ListRecent limited: %v", err)
	}
	if want := []string{"fable", "opus"}; !slices.Equal(got, want) {
		t.Fatalf("ListRecent limit 2 = %v, want %v", got, want)
	}
}
