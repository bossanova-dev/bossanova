package db

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/dbtest"
)

// triggerLaunchesVersion is the goose timestamp of the BOS-1418 migration that
// creates trigger_launches; preTriggerLaunchesVersion is the one before it.
const (
	triggerLaunchesVersion    int64 = 20261008000000
	preTriggerLaunchesVersion int64 = 20261007000000
)

// TestTriggerLaunchStore covers the BOS-1418 idempotency table end to end: the
// migration applies on a fresh daemon DB (and rolls back), and the store's
// claim / replay / release / prune contract holds.
func TestTriggerLaunchStore(t *testing.T) {
	t.Run("migration applies on a fresh daemon DB", func(t *testing.T) {
		db := dbtest.NewMigrated(t)

		var applied int
		if err := db.QueryRow(
			"SELECT COUNT(*) FROM goose_db_version WHERE version_id = ?", triggerLaunchesVersion,
		).Scan(&applied); err != nil {
			t.Fatalf("query goose version: %v", err)
		}
		if applied == 0 {
			t.Fatalf("migration version %d not applied", triggerLaunchesVersion)
		}

		cols := tableColumns(t, db, "trigger_launches")
		for name, want := range map[string]string{
			"invocation_id": "TEXT",
			"trigger_id":    "TEXT",
			"session_id":    "TEXT",
			"created_at":    "TEXT",
		} {
			if got, ok := cols[name]; !ok || got != want {
				t.Errorf("trigger_launches.%s type = %q (present=%v), want %q", name, got, ok, want)
			}
		}

		var idx int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_trigger_launches_created_at'`,
		).Scan(&idx); err != nil {
			t.Fatalf("query index: %v", err)
		}
		if idx != 1 {
			t.Errorf("idx_trigger_launches_created_at missing")
		}

		// The default created_at is the house ISO-8601 millisecond UTC form.
		if _, err := db.Exec(`INSERT INTO trigger_launches (invocation_id, trigger_id) VALUES ('inv-default', 'trg')`); err != nil {
			t.Fatalf("insert with default created_at: %v", err)
		}
		got, err := NewTriggerLaunchStore(db).Get(context.Background(), "inv-default")
		if err != nil || got == nil {
			t.Fatalf("get default row: %v, %v", got, err)
		}
		if got.CreatedAt.IsZero() {
			t.Error("default created_at did not parse as sqlutil.TimeLayout")
		}
	})

	t.Run("migration down drops the table and re-applies", func(t *testing.T) {
		migrations := os.DirFS(migrationsDir())
		db := dbtest.NewEmpty(t)
		if err := dbtest.RunUpTo(db, migrations, triggerLaunchesVersion); err != nil {
			t.Fatalf("run up: %v", err)
		}
		if err := dbtest.RunDownTo(db, migrations, preTriggerLaunchesVersion); err != nil {
			t.Fatalf("run down: %v", err)
		}
		if cols := tableColumns(t, db, "trigger_launches"); len(cols) != 0 {
			t.Fatalf("trigger_launches still present after down: %v", cols)
		}
		if err := dbtest.RunUpTo(db, migrations, triggerLaunchesVersion); err != nil {
			t.Fatalf("re-run up: %v", err)
		}
	})

	t.Run("claim then replay", func(t *testing.T) {
		ctx := context.Background()
		store := NewTriggerLaunchStore(setupTestDB(t))
		fresh := time.Now().Add(-time.Hour)

		if got, err := store.Get(ctx, "inv-1"); err != nil || got != nil {
			t.Fatalf("Get on empty table = %v, %v; want nil, nil", got, err)
		}
		won, err := store.Claim(ctx, "inv-1", "trg-1", fresh)
		if err != nil || !won {
			t.Fatalf("first Claim = %v, %v; want true, nil", won, err)
		}
		inFlight, err := store.Get(ctx, "inv-1")
		if err != nil || inFlight == nil || inFlight.SessionID != "" || inFlight.TriggerID != "trg-1" {
			t.Fatalf("in-flight row = %+v, %v", inFlight, err)
		}

		won, err = store.Claim(ctx, "inv-1", "trg-1", fresh)
		if err != nil || won {
			t.Fatalf("second Claim on a live claim = %v, %v; want false, nil", won, err)
		}

		if err := store.SetSession(ctx, "inv-1", "sess-1"); err != nil {
			t.Fatalf("SetSession: %v", err)
		}
		done, err := store.Get(ctx, "inv-1")
		if err != nil || done == nil || done.SessionID != "sess-1" {
			t.Fatalf("finished row = %+v, %v", done, err)
		}

		// A finished launch is never stolen nor released, however old.
		won, err = store.Claim(ctx, "inv-1", "trg-1", time.Now().Add(time.Hour))
		if err != nil || won {
			t.Fatalf("Claim over a finished launch = %v, %v; want false, nil", won, err)
		}
		if err := store.Release(ctx, "inv-1"); err != nil {
			t.Fatalf("Release: %v", err)
		}
		if again, _ := store.Get(ctx, "inv-1"); again == nil || again.SessionID != "sess-1" {
			t.Fatalf("Release removed a finished launch: %+v", again)
		}
	})

	t.Run("release lets a retry claim again", func(t *testing.T) {
		ctx := context.Background()
		store := NewTriggerLaunchStore(setupTestDB(t))
		fresh := time.Now().Add(-time.Hour)

		if won, err := store.Claim(ctx, "inv-r", "trg", fresh); err != nil || !won {
			t.Fatalf("Claim = %v, %v", won, err)
		}
		if err := store.Release(ctx, "inv-r"); err != nil {
			t.Fatalf("Release: %v", err)
		}
		if got, _ := store.Get(ctx, "inv-r"); got != nil {
			t.Fatalf("row survived Release: %+v", got)
		}
		if won, err := store.Claim(ctx, "inv-r", "trg", fresh); err != nil || !won {
			t.Fatalf("re-Claim after Release = %v, %v; want true, nil", won, err)
		}
	})

	t.Run("an abandoned claim is taken over", func(t *testing.T) {
		ctx := context.Background()
		store := NewTriggerLaunchStore(setupTestDB(t))

		if won, err := store.Claim(ctx, "inv-s", "trg", time.Now().Add(-time.Hour)); err != nil || !won {
			t.Fatalf("Claim = %v, %v", won, err)
		}
		// staleBefore in the future makes the existing claim count as abandoned.
		if won, err := store.Claim(ctx, "inv-s", "trg", time.Now().Add(time.Hour)); err != nil || !won {
			t.Fatalf("takeover Claim = %v, %v; want true, nil", won, err)
		}
	})

	t.Run("concurrent claims elect exactly one winner", func(t *testing.T) {
		ctx := context.Background()
		store := NewTriggerLaunchStore(setupTestDB(t))
		fresh := time.Now().Add(-time.Hour)

		const n = 16
		var (
			wg   sync.WaitGroup
			mu   sync.Mutex
			wins int
		)
		for range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				won, err := store.Claim(ctx, "inv-c", "trg", fresh)
				if err != nil {
					t.Errorf("Claim: %v", err)
					return
				}
				if won {
					mu.Lock()
					wins++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if wins != 1 {
			t.Fatalf("concurrent claims won %d times, want exactly 1", wins)
		}
	})

	t.Run("set session requires a claim", func(t *testing.T) {
		store := NewTriggerLaunchStore(setupTestDB(t))
		if err := store.SetSession(context.Background(), "inv-none", "sess"); err == nil {
			t.Fatal("SetSession without a claim succeeded, want error")
		}
	})

	t.Run("prune drops only rows older than the cutoff", func(t *testing.T) {
		ctx := context.Background()
		db := setupTestDB(t)
		store := NewTriggerLaunchStore(db)

		if _, err := db.ExecContext(ctx,
			`INSERT INTO trigger_launches (invocation_id, trigger_id, session_id, created_at) VALUES
			 ('inv-old', 'trg', 'sess-old', '2020-01-01T00:00:00.000Z'),
			 ('inv-new', 'trg', 'sess-new', '2099-01-01T00:00:00.000Z')`,
		); err != nil {
			t.Fatalf("seed: %v", err)
		}
		n, err := store.PruneOlderThan(ctx, time.Now().Add(-30*24*time.Hour))
		if err != nil || n != 1 {
			t.Fatalf("PruneOlderThan = %d, %v; want 1, nil", n, err)
		}
		if got, _ := store.Get(ctx, "inv-old"); got != nil {
			t.Errorf("old row survived prune: %+v", got)
		}
		if got, _ := store.Get(ctx, "inv-new"); got == nil {
			t.Error("new row was pruned")
		}
	})
}
