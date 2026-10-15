package db

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/recurser/bossd/internal/dbtest"
	"github.com/recurser/bossd/internal/reposecrets"
	"github.com/recurser/bossd/internal/reposecrets/reposecretstest"
)

func secretScrubDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bossd.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	dbtest.Apply(t, db)
	return db, path
}

func assertRawKeyMarkers(t *testing.T, path, linear, sentry string, wantPresent bool) {
	t.Helper()
	found := make([]bool, 2)
	for _, filename := range []string{path, path + "-wal"} {
		data, err := os.ReadFile(filename)
		if os.IsNotExist(err) && filename == path+"-wal" {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for i, marker := range []string{linear, sentry} {
			if bytes.Contains(data, []byte(marker)) {
				found[i] = true
			}
			if !wantPresent && bytes.Contains(data, []byte(marker)) {
				t.Fatalf("key marker %d remains in %s", i, filepath.Base(filename))
			}
		}
	}
	if wantPresent && (!found[0] || !found[1]) {
		t.Fatal("fixture did not leave both markers in raw database files")
	}
}

func assertScrubRequests(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pending_secret_scrubs`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("scrub requests = %d, want %d", got, want)
	}
}

func TestScrubAfterMoveRemovesFreedKeyCopies(t *testing.T) {
	ctx := context.Background()
	db, path := secretScrubDB(t)
	plain := NewRepoStore(db)
	repo := createTestRepo(t, plain)
	linear, sentry := markerKeys(strings.Repeat("move", 80))
	setRepoKeys(t, plain, repo.ID, linear, sentry)
	// Simulate old table rebuilds with secure_delete disabled, leaving a copy
	// on the freelist as well as historical WAL frames.
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`PRAGMA secure_delete = OFF`,
		`CREATE TABLE repos_copy AS SELECT * FROM repos`,
		`DROP TABLE repos_copy`,
		`UPDATE repos SET updated_at = updated_at || '0'`,
		`UPDATE repos SET updated_at = substr(updated_at, 1, length(updated_at)-1)`,
		`PRAGMA secure_delete = FAST`,
		`PRAGMA wal_checkpoint(TRUNCATE)`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	var free int
	if err := conn.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free); err != nil {
		t.Fatal(err)
	}
	if free == 0 {
		t.Fatal("fixture created no freelist page")
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	assertRawKeyMarkers(t, path, linear, sentry, true)
	t.Log("both marker keys present before move, including freed-page fixture")
	secrets := reposecretstest.NewStore(reposecretstest.NewRing())
	moved, err := MigrateRepoSecretsFromSQLite(ctx, db, secrets)
	if err != nil || moved != 1 {
		t.Fatalf("move = %d, %v", moved, err)
	}
	assertScrubRequests(t, db, 1)
	ran, err := ScrubFreedSecretPages(ctx, db)
	if err != nil || !ran {
		t.Fatalf("scrub = %v, %v", ran, err)
	}
	assertRawKeyMarkers(t, path, linear, sentry, false)
	t.Log("both marker keys absent from database and WAL after scrub")
	assertScrubRequests(t, db, 0)
	got, err := NewRepoStore(db, WithRepoSecrets(secrets)).Get(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LinearAPIKey != linear || got.SentryAPIKey != sentry {
		t.Fatal("keyring keys changed")
	}
}

func TestScrubAfterKeyUpdateClearingPlaintext(t *testing.T) {
	ctx := context.Background()
	db, path := secretScrubDB(t)
	plain := NewRepoStore(db)
	repo := createTestRepo(t, plain)
	linear, sentry := markerKeys(strings.Repeat("update", 60))
	setRepoKeys(t, plain, repo.ID, linear, sentry)
	assertScrubRequests(t, db, 0)
	secrets := reposecretstest.NewStore(reposecretstest.NewRing())
	store := NewRepoStore(db, WithRepoSecrets(secrets))
	replacement, _ := markerKeys("replacement")
	if _, err := store.Update(ctx, repo.ID, UpdateRepoParams{LinearAPIKey: &replacement}); err != nil {
		t.Fatal(err)
	}
	l, s, flag := rawRepoSecretColumns(t, db, repo.ID)
	if l != "" || s != "" || flag != 1 {
		t.Fatal("plaintext columns not cleared")
	}
	assertScrubRequests(t, db, 1)
	if ran, err := ScrubFreedSecretPages(ctx, db); err != nil || !ran {
		t.Fatalf("scrub = %v, %v", ran, err)
	}
	assertRawKeyMarkers(t, path, linear, sentry, false)
	assertScrubRequests(t, db, 0)
	// A keyring-only update creates no further scrub request.
	setRepoKeys(t, store, repo.ID, replacement, sentry)
	assertScrubRequests(t, db, 0)
}

func TestScrubNoRequestIsNoop(t *testing.T) {
	db, path := secretScrubDB(t)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if ran, err := ScrubFreedSecretPages(context.Background(), db); err != nil || ran {
		t.Fatalf("scrub = %v, %v", ran, err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("no-request scrub changed the database file")
	}
}

func TestScrubKeepsColumnStorageFallback(t *testing.T) {
	ctx := context.Background()
	db, _ := secretScrubDB(t)
	store := NewRepoStore(db, WithRepoSecrets(reposecrets.NewUnavailable()))
	repo := createTestRepo(t, store)
	linear, sentry := markerKeys("fallback")
	setRepoKeys(t, store, repo.ID, linear, sentry)
	if _, err := MigrateRepoSecretsFromSQLite(ctx, db, reposecrets.NewUnavailable()); err == nil {
		t.Fatal("expected unavailable keyring")
	}
	assertScrubRequests(t, db, 0)
	if _, err := db.Exec(requestSecretScrubSQL); err != nil {
		t.Fatal(err)
	}
	if ran, err := ScrubFreedSecretPages(ctx, db); err != nil || !ran {
		t.Fatalf("scrub = %v, %v", ran, err)
	}
	l, s, flag := rawRepoSecretColumns(t, db, repo.ID)
	if l != linear || s != sentry || flag != 0 {
		t.Fatal("scrub changed column fallback")
	}
	got, err := store.Get(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LinearAPIKey != linear || got.SentryAPIKey != sentry {
		t.Fatal("fallback keys no longer readable")
	}
}

// scrubCheckpointReader pins a real WAL reader immediately before the selected
// checkpoint, including the final one after the request DELETE committed.
type scrubCheckpointReader struct {
	*sql.Conn
	db          *sql.DB
	reader      *sql.Conn
	checkpoints int
	blockAt     int
	t           *testing.T
}

func (c *scrubCheckpointReader) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if query == `PRAGMA wal_checkpoint(TRUNCATE)` {
		c.checkpoints++
		if c.checkpoints == c.blockAt {
			var err error
			c.reader, err = c.db.Conn(ctx)
			if err != nil {
				c.t.Fatal(err)
			}
			if _, err := c.reader.ExecContext(ctx, `BEGIN`); err != nil {
				c.t.Fatal(err)
			}
			var count int
			if err := c.reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_secret_scrubs`).Scan(&count); err != nil {
				c.t.Fatal(err)
			}
		}
	}
	return c.Conn.QueryRowContext(ctx, query, args...)
}

func TestScrubRetriesBusyCheckpoint(t *testing.T) {
	for _, checkpoint := range []int{1, 2} {
		t.Run(fmt.Sprintf("checkpoint-%d", checkpoint), func(t *testing.T) {
			ctx := context.Background()
			db, _ := secretScrubDB(t)
			if _, err := db.Exec(requestSecretScrubSQL); err != nil {
				t.Fatal(err)
			}
			conn, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout = 1`); err != nil {
				t.Fatal(err)
			}
			blocked := &scrubCheckpointReader{Conn: conn, db: db, blockAt: checkpoint, t: t}
			defer func() {
				if blocked.reader != nil {
					_, _ = blocked.reader.ExecContext(ctx, `ROLLBACK`)
					_ = blocked.reader.Close()
				}
			}()
			ran, err := scrubFreedSecretPages(ctx, blocked)
			if err == nil || !strings.Contains(err.Error(), "checkpoint busy") || ran {
				t.Fatalf("scrub = %v, %v; want busy error", ran, err)
			}
			assertScrubRequests(t, db, 1)
			var secureDelete int
			if err := conn.QueryRowContext(ctx, `PRAGMA secure_delete`).Scan(&secureDelete); err != nil {
				t.Fatal(err)
			}
			if secureDelete != 2 {
				t.Fatalf("secure_delete = %d, want FAST", secureDelete)
			}
			if _, err := blocked.reader.ExecContext(ctx, `ROLLBACK`); err != nil {
				t.Fatal(err)
			}
			if err := blocked.reader.Close(); err != nil {
				t.Fatal(err)
			}
			blocked.reader = nil
			if ran, err := ScrubFreedSecretPages(ctx, db); err != nil || !ran {
				t.Fatalf("retry scrub = %v, %v", ran, err)
			}
			assertScrubRequests(t, db, 0)
		})
	}
}

func TestSecretScrubRequestRollsBackWithKeyUpdate(t *testing.T) {
	ctx := context.Background()
	db, _ := secretScrubDB(t)
	plain := NewRepoStore(db)
	repo := createTestRepo(t, plain)
	linear, sentry := markerKeys("rollback")
	setRepoKeys(t, plain, repo.ID, linear, sentry)
	if _, err := db.Exec(`CREATE TRIGGER reject_scrub BEFORE INSERT ON pending_secret_scrubs BEGIN SELECT RAISE(ABORT, 'scrub request rejected'); END`); err != nil {
		t.Fatal(err)
	}
	secrets := reposecretstest.NewStore(reposecretstest.NewRing())
	store := NewRepoStore(db, WithRepoSecrets(secrets))
	replacement, _ := markerKeys("replacement")
	if _, err := store.Update(ctx, repo.ID, UpdateRepoParams{LinearAPIKey: &replacement}); err == nil {
		t.Fatal("update ignored scrub request failure")
	}
	l, s, flag := rawRepoSecretColumns(t, db, repo.ID)
	if l != linear || s != sentry || flag != 0 {
		t.Fatal("failed scrub request committed plaintext clearing")
	}
	assertScrubRequests(t, db, 0)
	if moved, err := MigrateRepoSecretsFromSQLite(ctx, db, secrets); err == nil || moved != 0 {
		t.Fatalf("move = %d, %v; want failed request", moved, err)
	}
	l, s, flag = rawRepoSecretColumns(t, db, repo.ID)
	if l != linear || s != sentry || flag != 0 {
		t.Fatal("failed scrub request committed move")
	}
}
