package db

import (
	"os"
	"testing"

	"github.com/recurser/bossd/internal/dbtest"
)

// sessionPRIdentityVersion adds sessions.pr_author_login and
// sessions.pr_target_branch; preSessionPRIdentityVersion is the one before it.
const (
	sessionPRIdentityVersion    int64 = 20261125000000
	preSessionPRIdentityVersion int64 = 20261123140000
)

// The migration adds both columns as non-null empty-default TEXT, backfills the
// target from base_branch only for sessions with a PR, and rolls back without
// losing rows.
func TestSessionPRIdentityMigration(t *testing.T) {
	migrations := os.DirFS(migrationsDir())
	db, err := OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := dbtest.RunUpTo(db, migrations, preSessionPRIdentityVersion); err != nil {
		t.Fatalf("run up to %d: %v", preSessionPRIdentityVersion, err)
	}
	if _, ok := tableColumns(t, db, "sessions")["pr_target_branch"]; ok {
		t.Fatal("pr_target_branch present before its migration")
	}
	repo := insertPreMigrationRepo(t, db)
	for _, row := range []struct {
		id, base string
		pr       any
	}{{"s1", "dev", 41}, {"s2", "main", nil}} {
		if _, err := db.Exec(`INSERT INTO sessions (id, repo_id, title, worktree_path, branch_name, base_branch, pr_number)
		                      VALUES (?, ?, 'legacy', '/tmp/worktrees/'||?, 'branch-'||?, ?, ?)`,
			row.id, repo.ID, row.id, row.id, row.base, row.pr); err != nil {
			t.Fatalf("insert legacy session %s: %v", row.id, err)
		}
	}

	if err := dbtest.RunUpTo(db, migrations, sessionPRIdentityVersion); err != nil {
		t.Fatalf("run up to %d: %v", sessionPRIdentityVersion, err)
	}
	cols := tableColumns(t, db, "sessions")
	for _, name := range []string{"pr_author_login", "pr_target_branch"} {
		if cols[name] != "TEXT" {
			t.Errorf("sessions.%s type = %q, want TEXT", name, cols[name])
		}
		var notnull int
		var dflt *string
		if err := db.QueryRow(`SELECT "notnull", dflt_value FROM pragma_table_info('sessions') WHERE name = ?`, name).Scan(&notnull, &dflt); err != nil {
			t.Fatalf("pragma_table_info(sessions.%s): %v", name, err)
		}
		if notnull != 1 || dflt == nil || *dflt != "''" {
			t.Errorf("sessions.%s notnull=%d default=%v, want NOT NULL DEFAULT ''", name, notnull, dflt)
		}
	}
	for id, want := range map[string]string{"s1": "dev", "s2": ""} {
		var author, target string
		if err := db.QueryRow(`SELECT pr_author_login, pr_target_branch FROM sessions WHERE id = ?`, id).Scan(&author, &target); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if author != "" || target != want {
			t.Errorf("%s backfill (author, target) = (%q, %q), want (\"\", %q)", id, author, target, want)
		}
	}

	if err := dbtest.RunDownTo(db, migrations, preSessionPRIdentityVersion); err != nil {
		t.Fatalf("run down: %v", err)
	}
	cols = tableColumns(t, db, "sessions")
	for _, name := range []string{"pr_author_login", "pr_target_branch"} {
		if _, ok := cols[name]; ok {
			t.Errorf("sessions.%s still present after down", name)
		}
	}
	assertSessionsSurvive(t, db)
}
