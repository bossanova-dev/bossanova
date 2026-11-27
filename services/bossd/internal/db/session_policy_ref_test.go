package db

import (
	"os"
	"testing"

	"github.com/recurser/bossd/internal/dbtest"
)

func TestSessionPolicyRefRoundTrip(t *testing.T) {
	database := setupTestDB(t)
	repo := createTestRepo(t, NewRepoStore(database))
	store := NewSessionStore(database)
	for _, want := range []string{"", "config/verify-trial"} {
		sess, err := store.Create(t.Context(), CreateSessionParams{RepoID: repo.ID, Title: "verification", IsVerifyOnly: true, PolicyRef: want})
		if err != nil {
			t.Fatal(err)
		}
		title := "renamed"
		if _, err := store.Update(t.Context(), sess.ID, UpdateSessionParams{Title: &title}); err != nil {
			t.Fatal(err)
		}
		got, err := store.Get(t.Context(), sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.PolicyRef != want {
			t.Fatalf("stored policy ref = %q, want %q", got.PolicyRef, want)
		}
	}
}

func TestSessionPolicyRefMigrationUpDown(t *testing.T) {
	database, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	migrations := os.DirFS(migrationsDir())
	const before int64 = 20261123140000
	const version int64 = 20261125120000
	if err := dbtest.RunUpTo(database, migrations, before); err != nil {
		t.Fatal(err)
	}
	repo := insertPreMigrationRepo(t, database)
	if _, err := database.Exec(`INSERT INTO sessions (id, repo_id, title, worktree_path, branch_name, state) VALUES ('legacy-trial', ?, 'legacy', '/tmp/legacy', 'legacy', 0)`, repo.ID); err != nil {
		t.Fatal(err)
	}
	if err := dbtest.RunUpTo(database, migrations, version); err != nil {
		t.Fatal(err)
	}
	var ref string
	if err := database.QueryRow(`SELECT policy_ref FROM sessions WHERE id = 'legacy-trial'`).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	if ref != "" {
		t.Fatalf("legacy session policy ref = %q, want empty", ref)
	}
	if err := dbtest.RunDownTo(database, migrations, before); err != nil {
		t.Fatal(err)
	}
	if _, exists := tableColumns(t, database, "sessions")["policy_ref"]; exists {
		t.Fatal("column survives rollback")
	}
}
