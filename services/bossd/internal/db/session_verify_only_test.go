package db

import (
	"os"
	"testing"

	"github.com/recurser/bossd/internal/dbtest"
)

func TestSessionVerifyOnlyRoundTrip(t *testing.T) {
	database := setupTestDB(t)
	repo := createTestRepo(t, NewRepoStore(database))
	store := NewSessionStore(database)
	for _, want := range []bool{false, true} {
		sess, err := store.Create(t.Context(), CreateSessionParams{RepoID: repo.ID, Title: "verification", IsVerifyOnly: want})
		if err != nil {
			t.Fatal(err)
		}
		if sess.IsVerifyOnly != want {
			t.Fatalf("created verify-only = %v, want %v", sess.IsVerifyOnly, want)
		}
		got, err := store.Get(t.Context(), sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.IsVerifyOnly != want {
			t.Fatalf("stored verify-only = %v, want %v", got.IsVerifyOnly, want)
		}
		title := "renamed"
		updated, err := store.Update(t.Context(), sess.ID, UpdateSessionParams{Title: &title})
		if err != nil {
			t.Fatal(err)
		}
		if updated.IsVerifyOnly != want {
			t.Fatal("update changed creation-time verify-only fact")
		}
	}
}

func TestSessionVerifyOnlyMigrationUpDown(t *testing.T) {
	database, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	migrations := os.DirFS(migrationsDir())
	const before int64 = 20261030120000
	const version int64 = 20261030130000
	if err := dbtest.RunUpTo(database, migrations, before); err != nil {
		t.Fatal(err)
	}
	repo := insertPreMigrationRepo(t, database)
	if _, err := database.Exec(`INSERT INTO sessions (id, repo_id, title, worktree_path, branch_name, state) VALUES ('legacy-verify', ?, 'legacy', '/tmp/legacy', 'legacy', 0)`, repo.ID); err != nil {
		t.Fatal(err)
	}
	if err := dbtest.RunUpTo(database, migrations, version); err != nil {
		t.Fatal(err)
	}
	var flag bool
	if err := database.QueryRow(`SELECT is_verify_only FROM sessions WHERE id = 'legacy-verify'`).Scan(&flag); err != nil {
		t.Fatal(err)
	}
	if flag {
		t.Fatal("legacy session became verify-only")
	}
	if err := dbtest.RunDownTo(database, migrations, before); err != nil {
		t.Fatal(err)
	}
	if _, exists := tableColumns(t, database, "sessions")["is_verify_only"]; exists {
		t.Fatal("column survives rollback")
	}
	if err := dbtest.RunUpTo(database, migrations, version); err != nil {
		t.Fatal(err)
	}
}
