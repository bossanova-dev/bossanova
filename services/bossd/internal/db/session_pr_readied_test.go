package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossd/internal/dbtest"
)

func TestSessionPRReadiedStamp(t *testing.T) {
	database := setupTestDB(t)
	store := NewSessionStore(database)
	repo := createTestRepo(t, NewRepoStore(database))
	ctx := context.Background()
	sess, err := store.Create(ctx, CreateSessionParams{RepoID: repo.ID, Title: "ready stamp", WorktreePath: "/tmp/stamp", BranchName: "stamp", BaseBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	prNumber := 42
	prPointer := &prNumber
	if _, err := store.Update(ctx, sess.ID, UpdateSessionParams{PRNumber: &prPointer}); err != nil {
		t.Fatal(err)
	}
	for _, state := range []machine.State{machine.GreenDraft, machine.ReadyForReview, machine.AwaitingChecks, machine.ReadyForReview} {
		value := int(state)
		got, err := store.Update(ctx, sess.ID, UpdateSessionParams{State: &value})
		if err != nil {
			t.Fatal(err)
		}
		if state == machine.GreenDraft {
			if got.PRReadiedAt != nil {
				t.Fatal("never-ready session stamped")
			}
			continue
		}
		if got.PRReadiedAt == nil {
			t.Fatal("ready stamp missing")
		}
		if sess.PRReadiedAt != nil && !sess.PRReadiedAt.Equal(*got.PRReadiedAt) {
			t.Fatal("first ready stamp changed")
		}
		// A historical value makes a restamp observable without a wall-clock sleep.
		if sess.PRReadiedAt == nil {
			first := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			if _, err := database.ExecContext(ctx, "UPDATE sessions SET pr_readied_at=? WHERE id=?", first.Format(time.RFC3339Nano), sess.ID); err != nil {
				t.Fatal(err)
			}
			sess.PRReadiedAt = &first
		}
	}
}

func TestSessionPRReadiedRequiresPR(t *testing.T) {
	for _, tc := range []struct {
		name      string
		initialPR bool
		setPR     bool
		linkPR    bool
		wantStamp bool
	}{
		{name: "no PR"},
		{name: "link with ready", setPR: true, linkPR: true, wantStamp: true},
		{name: "unlink with ready", initialPR: true, setPR: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := setupTestDB(t)
			store := NewSessionStore(database)
			repo := createTestRepo(t, NewRepoStore(database))
			ctx := context.Background()
			sess, err := store.Create(ctx, CreateSessionParams{RepoID: repo.ID, Title: "first PR", WorktreePath: "/tmp/first-pr", BranchName: "first-pr", BaseBranch: "main"})
			if err != nil {
				t.Fatal(err)
			}
			pr := 42
			prPointer := &pr
			if tc.initialPR {
				if _, err := store.Update(ctx, sess.ID, UpdateSessionParams{PRNumber: &prPointer}); err != nil {
					t.Fatal(err)
				}
			}
			ready := int(machine.ReadyForReview)
			params := UpdateSessionParams{State: &ready}
			var nextPR *int
			if tc.linkPR {
				nextPR = &pr
			}
			if tc.setPR {
				params.PRNumber = &nextPR
			}
			got, err := store.Update(ctx, sess.ID, params)
			if err != nil {
				t.Fatal(err)
			}
			if (got.PRReadiedAt != nil) != tc.wantStamp {
				t.Fatalf("stamp = %v, want present %v", got.PRReadiedAt, tc.wantStamp)
			}
			if !tc.wantStamp {
				got, err = store.Update(ctx, sess.ID, UpdateSessionParams{PRNumber: &prPointer})
				if err != nil {
					t.Fatal(err)
				}
				if got.PRReadiedAt != nil {
					t.Fatal("linking first PR inherited a ready stamp")
				}
				got, err = store.Update(ctx, sess.ID, UpdateSessionParams{State: &ready})
				if err != nil {
					t.Fatal(err)
				}
				if got.PRReadiedAt == nil {
					t.Fatal("first ready with a PR was not stamped")
				}
			}
		})
	}
}

func TestSessionPRReadiedMigration(t *testing.T) {
	database, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	migrations := os.DirFS(migrationsDir())
	const previous int64 = 20261030130000
	if err := dbtest.RunUpTo(database, migrations, previous); err != nil {
		t.Fatal(err)
	}
	repo := createTestRepo(t, NewRepoStore(database))
	const updated = "2026-10-31T00:32:37Z"
	for _, tc := range []struct {
		id    string
		state machine.State
		pr    any
	}{
		{"ready", machine.ReadyForReview, 42}, {"repairing", machine.GreenDraft, 43}, {"no-pr", machine.ReadyForReview, nil},
	} {
		_, err := database.Exec("INSERT INTO sessions (id,repo_id,title,worktree_path,branch_name,base_branch,state,pr_number,updated_at) VALUES (?,?,?,'/tmp/stamp',?,'main',?,?,?)", tc.id, repo.ID, tc.id, tc.id, int(tc.state), tc.pr, updated)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := dbtest.Run(database, migrations); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"ready", "repairing", "no-pr"} {
		var stamp *string
		if err := database.QueryRow("SELECT pr_readied_at FROM sessions WHERE id=?", id).Scan(&stamp); err != nil {
			t.Fatal(err)
		}
		if id == "ready" {
			if stamp == nil || *stamp != updated {
				t.Fatalf("backfill = %v, want %s", stamp, updated)
			}
		} else if stamp != nil {
			t.Fatalf("%s unexpectedly backfilled", id)
		}
	}
	if err := dbtest.RunDownTo(database, migrations, previous); err != nil {
		t.Fatal(err)
	}
	var columns int
	if err := database.QueryRow("SELECT count(*) FROM pragma_table_info('sessions') WHERE name='pr_readied_at'").Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 0 {
		t.Fatal("rollback retained ready stamp column")
	}
}
