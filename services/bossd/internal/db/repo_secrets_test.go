package db

import (
	"context"
	"database/sql"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/recurser/bossalib/models"

	"github.com/recurser/bossd/internal/reposecrets"
	"github.com/recurser/bossd/internal/reposecrets/reposecretstest"
)

// markerKeys builds marker key values at runtime so no secret-shaped literal
// is committed.
func markerKeys(tag string) (linear, sentry string) {
	return "lin-marker-" + tag, "sen-marker-" + tag
}

// rawRepoSecretColumns reads a repo row's key columns and keyring flag straight
// from SQLite, bypassing the store's hydration.
func rawRepoSecretColumns(t *testing.T, dbConn *sql.DB, id string) (linear, sentry string, hasKeyring int) {
	t.Helper()
	if err := dbConn.QueryRowContext(context.Background(),
		`SELECT linear_api_key, sentry_api_key, has_keyring_secrets FROM repos WHERE id = ?`, id,
	).Scan(&linear, &sentry, &hasKeyring); err != nil {
		t.Fatalf("read raw repo secret columns: %v", err)
	}
	return linear, sentry, hasKeyring
}

func setRepoKeys(t *testing.T, store *SQLiteRepoStore, id, linear, sentry string) *models.Repo {
	t.Helper()
	repo, err := store.Update(context.Background(), id, UpdateRepoParams{LinearAPIKey: &linear, SentryAPIKey: &sentry})
	if err != nil {
		t.Fatalf("update repo keys: %v", err)
	}
	return repo
}

// TestRepoStore_KeysLiveInKeyring is the BOS-1587 contract: with a keyring
// available, writing a repo's keys leaves both SQLite columns empty and the
// flag set, while every reader still sees the keys.
func TestRepoStore_KeysLiveInKeyring(t *testing.T) {
	ctx := context.Background()
	dbConn := setupTestDB(t)
	ring := reposecretstest.NewRing()
	store := NewRepoStore(dbConn, WithRepoSecrets(reposecretstest.NewStore(ring)))
	repo := createTestRepo(t, store)
	linear, sentry := markerKeys("kr")

	updated := setRepoKeys(t, store, repo.ID, linear, sentry)
	if updated.LinearAPIKey != linear || updated.SentryAPIKey != sentry {
		t.Fatal("Update did not return the keys it wrote")
	}

	rawLinear, rawSentry, flag := rawRepoSecretColumns(t, dbConn, repo.ID)
	if rawLinear != "" || rawSentry != "" {
		t.Fatal("repo key columns still hold plaintext after a write with a keyring available")
	}
	if flag != 1 {
		t.Fatalf("has_keyring_secrets = %d, want 1", flag)
	}
	if !ring.Has(reposecrets.ItemKey(repo.ID)) {
		t.Fatal("no keyring item written for the repo")
	}

	got, err := store.Get(ctx, repo.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.LinearAPIKey != linear || got.SentryAPIKey != sentry {
		t.Fatal("Get did not hydrate the keys from the keyring")
	}
	repos, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(repos) != 1 || repos[0].LinearAPIKey != linear || repos[0].SentryAPIKey != sentry {
		t.Fatal("List did not hydrate the keys from the keyring")
	}
	byPath, err := store.GetByPath(ctx, repo.LocalPath)
	if err != nil || byPath.SentryAPIKey != sentry {
		t.Fatalf("GetByPath did not hydrate the keys (err %v)", err)
	}

	// A partial update keeps the other key.
	newLinear, _ := markerKeys("kr2")
	if _, err := store.Update(ctx, repo.ID, UpdateRepoParams{LinearAPIKey: &newLinear}); err != nil {
		t.Fatalf("partial update: %v", err)
	}
	got, _ = store.Get(ctx, repo.ID)
	if got.LinearAPIKey != newLinear || got.SentryAPIKey != sentry {
		t.Fatal("partial key update lost or failed to apply a key")
	}

	// An update that touches no key leaves the keys alone.
	name := "renamed"
	if _, err := store.Update(ctx, repo.ID, UpdateRepoParams{DisplayName: &name}); err != nil {
		t.Fatalf("non-key update: %v", err)
	}
	got, _ = store.Get(ctx, repo.ID)
	if got.LinearAPIKey != newLinear || got.SentryAPIKey != sentry {
		t.Fatal("non-key update changed the keys")
	}

	// Clearing both keys removes the keyring item.
	empty := ""
	if _, err := store.Update(ctx, repo.ID, UpdateRepoParams{LinearAPIKey: &empty, SentryAPIKey: &empty}); err != nil {
		t.Fatalf("clear keys: %v", err)
	}
	if ring.Has(reposecrets.ItemKey(repo.ID)) {
		t.Fatal("keyring item survived clearing both keys")
	}
	got, _ = store.Get(ctx, repo.ID)
	if got.LinearAPIKey != "" || got.SentryAPIKey != "" {
		t.Fatal("keys still readable after clearing them")
	}

	// Deleting the repo deletes its keyring item.
	setRepoKeys(t, store, repo.ID, linear, sentry)
	if err := store.Delete(ctx, repo.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ring.Has(reposecrets.ItemKey(repo.ID)) {
		t.Fatal("keyring item survived deleting the repo")
	}
}

// TestRepoStore_FailedUpdateRestoresKeyring: the keyring write happens before
// the SQLite UPDATE commits, so an UPDATE that aborts must put the keyring item
// back. Otherwise the caller sees an error while the new key is live (or, for a
// row not yet flagged, the keyring holds keys the columns still own).
func TestRepoStore_FailedUpdateRestoresKeyring(t *testing.T) {
	ctx := context.Background()
	boom := "boom"

	// setup returns a keyring-backed store whose UPDATE aborts when the
	// display name is set to "boom", is silently skipped (zero rows affected)
	// for "skip", and succeeds but fails at COMMIT for "commitboom" (a
	// deferred foreign-key violation, checked only at COMMIT), plus a reader
	// of the keyring item that bypasses the writing store's cache.
	setup := func(t *testing.T) (*sql.DB, *reposecretstest.Ring, *SQLiteRepoStore, func(string) reposecrets.Secrets) {
		t.Helper()
		dbConn := setupTestDB(t)
		ring := reposecretstest.NewRing()
		store := NewRepoStore(dbConn, WithRepoSecrets(reposecretstest.NewStore(ring)))
		if _, err := dbConn.ExecContext(ctx, `CREATE TRIGGER boom BEFORE UPDATE ON repos
			WHEN NEW.display_name = 'boom' BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
			t.Fatalf("create trigger: %v", err)
		}
		for _, stmt := range []string{
			`CREATE TRIGGER skip BEFORE UPDATE ON repos
			 WHEN NEW.display_name = 'skip' BEGIN SELECT RAISE(IGNORE); END`,
			`CREATE TABLE commit_guard_parent (id TEXT PRIMARY KEY)`,
			`CREATE TABLE commit_guard (parent_id TEXT
			 REFERENCES commit_guard_parent(id) DEFERRABLE INITIALLY DEFERRED)`,
			`CREATE TRIGGER commitboom AFTER UPDATE ON repos
			 WHEN NEW.display_name = 'commitboom' BEGIN INSERT INTO commit_guard VALUES ('missing'); END`,
		} {
			if _, err := dbConn.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("create failure fixture: %v", err)
			}
		}
		ringView := func(id string) reposecrets.Secrets {
			t.Helper()
			sec, err := reposecretstest.NewStore(ring).Get(ctx, id)
			if err != nil {
				t.Fatalf("read keyring item: %v", err)
			}
			return sec
		}
		return dbConn, ring, store, ringView
	}

	t.Run("flagged row keeps its old keys", func(t *testing.T) {
		_, _, store, ringView := setup(t)
		repo := createTestRepo(t, store)
		linear, sentry := markerKeys("old")
		setRepoKeys(t, store, repo.ID, linear, sentry)

		newLinear, _ := markerKeys("new")
		if _, err := store.Update(ctx, repo.ID, UpdateRepoParams{DisplayName: &boom, LinearAPIKey: &newLinear}); err == nil {
			t.Fatal("Update succeeded through an aborting trigger")
		}

		got, err := store.Get(ctx, repo.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.LinearAPIKey != linear || got.SentryAPIKey != sentry {
			t.Fatal("failed Update left the new key live in the store")
		}
		if sec := ringView(repo.ID); sec.LinearAPIKey != linear || sec.SentryAPIKey != sentry {
			t.Fatal("failed Update left the new key in the keyring item")
		}
	})

	for _, failure := range []struct{ name, displayName, wantErr string }{
		{"commit failure", "commitboom", "commit repo update"},
		{"zero rows updated", "skip", sql.ErrNoRows.Error()},
	} {
		t.Run(failure.name+" keeps a flagged row's old keys", func(t *testing.T) {
			_, _, store, ringView := setup(t)
			repo := createTestRepo(t, store)
			linear, sentry := markerKeys("old")
			setRepoKeys(t, store, repo.ID, linear, sentry)

			newLinear, _ := markerKeys("new")
			name := failure.displayName
			_, err := store.Update(ctx, repo.ID, UpdateRepoParams{DisplayName: &name, LinearAPIKey: &newLinear})
			if err == nil || !strings.Contains(err.Error(), failure.wantErr) {
				t.Fatalf("Update err = %v, want one containing %q", err, failure.wantErr)
			}

			got, err := store.Get(ctx, repo.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.DisplayName == name {
				t.Fatal("failed Update's row change is visible")
			}
			if got.LinearAPIKey != linear || got.SentryAPIKey != sentry {
				t.Fatal("failed Update left the new key live in the store")
			}
			if sec := ringView(repo.ID); sec.LinearAPIKey != linear || sec.SentryAPIKey != sentry {
				t.Fatal("failed Update left the new key in the keyring item")
			}
		})
	}

	t.Run("unflagged row gets no keyring item", func(t *testing.T) {
		dbConn, ring, store, _ := setup(t)
		repo := createTestRepo(t, store)
		linear, sentry := markerKeys("cols")
		if _, err := dbConn.ExecContext(ctx,
			`UPDATE repos SET linear_api_key = ?, sentry_api_key = ?, has_keyring_secrets = 0 WHERE id = ?`,
			linear, sentry, repo.ID); err != nil {
			t.Fatalf("seed key columns: %v", err)
		}

		newLinear, _ := markerKeys("new2")
		if _, err := store.Update(ctx, repo.ID, UpdateRepoParams{DisplayName: &boom, LinearAPIKey: &newLinear}); err == nil {
			t.Fatal("Update succeeded through an aborting trigger")
		}

		if ring.Has(reposecrets.ItemKey(repo.ID)) {
			t.Fatal("failed Update left a keyring item for a row whose columns are authoritative")
		}
		rawLinear, rawSentry, flag := rawRepoSecretColumns(t, dbConn, repo.ID)
		if rawLinear != linear || rawSentry != sentry || flag != 0 {
			t.Fatal("failed Update changed the key columns")
		}
	})
}

// TestRepoStore_UnavailableKeyringUsesColumns: with no keyring, writes fall
// back to the SQLite columns with the flag clear, so the keys still work. A
// store built without WithRepoSecrets takes the same path.
func TestRepoStore_UnavailableKeyringUsesColumns(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []RepoStoreOption
	}{
		{"unavailable keyring", []RepoStoreOption{WithRepoSecrets(reposecrets.NewUnavailable())}},
		{"no secret store configured", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testUnavailableKeyringUsesColumns(t, tc.opts...)
		})
	}
}

func testUnavailableKeyringUsesColumns(t *testing.T, opts ...RepoStoreOption) {
	ctx := context.Background()
	dbConn := setupTestDB(t)
	store := NewRepoStore(dbConn, opts...)
	repo := createTestRepo(t, store)
	linear, sentry := markerKeys("nokr")

	setRepoKeys(t, store, repo.ID, linear, sentry)

	rawLinear, rawSentry, flag := rawRepoSecretColumns(t, dbConn, repo.ID)
	if rawLinear != linear || rawSentry != sentry {
		t.Fatal("keys not stored in the columns when the keyring is unavailable")
	}
	if flag != 0 {
		t.Fatalf("has_keyring_secrets = %d, want 0", flag)
	}
	got, err := store.Get(ctx, repo.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.LinearAPIKey != linear || got.SentryAPIKey != sentry {
		t.Fatal("Get did not return the column-stored keys")
	}
	// A row flagged as keyring-held reads as having no keys, not as an error.
	if _, err := dbConn.ExecContext(ctx,
		`UPDATE repos SET linear_api_key = '', sentry_api_key = '', has_keyring_secrets = 1 WHERE id = ?`, repo.ID); err != nil {
		t.Fatalf("flag row: %v", err)
	}
	got, err = store.Get(ctx, repo.ID)
	if err != nil {
		t.Fatalf("Get flagged row: %v", err)
	}
	if got.LinearAPIKey != "" || got.SentryAPIKey != "" {
		t.Fatal("flagged row without a keyring read back keys")
	}
}

// TestMigrateRepoSecretsFromSQLite covers the one-time startup move: values
// move to the keyring, the columns are cleared, a re-run is a no-op, and a row
// whose keyring write fails is left exactly as it was.
func TestMigrateRepoSecretsFromSQLite(t *testing.T) {
	ctx := context.Background()
	dbConn := setupTestDB(t)

	// Pre-move rows: written by a store with no keyring, i.e. today's columns.
	plain := NewRepoStore(dbConn)
	repoA := createTestRepo(t, plain)
	linearA, sentryA := markerKeys("a")
	setRepoKeys(t, plain, repoA.ID, linearA, sentryA)

	repoB, err := plain.Create(ctx, CreateRepoParams{
		DisplayName: "repo-b", LocalPath: "/tmp/repo-b", OriginURL: "https://github.com/test/repo-b.git",
		DefaultBaseBranch: "main", WorktreeBaseDir: "/tmp/wt-b",
	})
	if err != nil {
		t.Fatalf("create repo B: %v", err)
	}
	linearB, _ := markerKeys("b")
	if _, err := plain.Update(ctx, repoB.ID, UpdateRepoParams{LinearAPIKey: &linearB}); err != nil {
		t.Fatalf("set repo B key: %v", err)
	}

	// A row with no keys is not touched.
	repoC, err := plain.Create(ctx, CreateRepoParams{
		DisplayName: "repo-c", LocalPath: "/tmp/repo-c", OriginURL: "https://github.com/test/repo-c.git",
		DefaultBaseBranch: "main", WorktreeBaseDir: "/tmp/wt-c",
	})
	if err != nil {
		t.Fatalf("create repo C: %v", err)
	}

	ring := reposecretstest.NewRing()
	secrets := reposecretstest.NewStore(ring)
	moved, err := MigrateRepoSecretsFromSQLite(ctx, dbConn, secrets)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if moved != 2 {
		t.Fatalf("moved = %d, want 2", moved)
	}
	for _, id := range []string{repoA.ID, repoB.ID} {
		rl, rs, flag := rawRepoSecretColumns(t, dbConn, id)
		if rl != "" || rs != "" || flag != 1 {
			t.Fatalf("repo %s after move: columns not cleared or flag %d != 1", id, flag)
		}
	}
	if _, _, flag := rawRepoSecretColumns(t, dbConn, repoC.ID); flag != 0 {
		t.Fatal("a repo with no keys was flagged as moved")
	}
	if ring.Has(reposecrets.ItemKey(repoC.ID)) {
		t.Fatal("a keyring item was written for a repo with no keys")
	}

	store := NewRepoStore(dbConn, WithRepoSecrets(secrets))
	gotA, _ := store.Get(ctx, repoA.ID)
	if gotA.LinearAPIKey != linearA || gotA.SentryAPIKey != sentryA {
		t.Fatal("repo A keys not readable after the move")
	}
	gotB, _ := store.Get(ctx, repoB.ID)
	if gotB.LinearAPIKey != linearB || gotB.SentryAPIKey != "" {
		t.Fatal("repo B keys not readable after the move")
	}

	assertScrubRequests(t, dbConn, 1)
	if _, err := dbConn.Exec(`DELETE FROM pending_secret_scrubs`); err != nil {
		t.Fatal(err)
	}
	// Idempotent: a second run moves nothing and writes nothing.
	setsBefore := ring.Sets
	moved, err = MigrateRepoSecretsFromSQLite(ctx, dbConn, secrets)
	if err != nil || moved != 0 {
		t.Fatalf("second migrate = (%d, %v), want (0, nil)", moved, err)
	}
	assertScrubRequests(t, dbConn, 0)
	if ring.Sets != setsBefore {
		t.Fatal("second migrate wrote to the keyring")
	}

	// A failed keyring write leaves the row as it was, to retry next start.
	repoD, err := plain.Create(ctx, CreateRepoParams{
		DisplayName: "repo-d", LocalPath: "/tmp/repo-d", OriginURL: "https://github.com/test/repo-d.git",
		DefaultBaseBranch: "main", WorktreeBaseDir: "/tmp/wt-d",
	})
	if err != nil {
		t.Fatalf("create repo D: %v", err)
	}
	linearD, sentryD := markerKeys("d")
	setRepoKeys(t, plain, repoD.ID, linearD, sentryD)
	ring.SetFailSet(true)
	moved, err = MigrateRepoSecretsFromSQLite(ctx, dbConn, secrets)
	if err == nil {
		t.Fatal("migrate with a failing keyring returned nil error")
	}
	if moved != 0 {
		t.Fatalf("moved = %d with a failing keyring, want 0", moved)
	}
	rl, rs, flag := rawRepoSecretColumns(t, dbConn, repoD.ID)
	if rl != linearD || rs != sentryD || flag != 0 {
		t.Fatal("a row whose keyring write failed was changed")
	}

	// The next start, with the keyring working again, moves it.
	ring.SetFailSet(false)
	moved, err = MigrateRepoSecretsFromSQLite(ctx, dbConn, secrets)
	if err != nil || moved != 1 {
		t.Fatalf("retry migrate = (%d, %v), want (1, nil)", moved, err)
	}
	gotD, _ := store.Get(ctx, repoD.ID)
	if gotD.LinearAPIKey != linearD || gotD.SentryAPIKey != sentryD {
		t.Fatal("repo D keys not readable after the retried move")
	}
}

// TestClassifyKeyCommit pins every rule classifyKeyCommit applies to a keyed
// write's COMMIT error (BOS-1658).
func TestClassifyKeyCommit(t *testing.T) {
	const (
		t0 = "2026-01-01T00:00:00.000Z"
		t1 = "2026-01-01T00:00:00.001Z"
		t2 = "2026-01-01T00:00:00.002Z"
	)
	unflagged := repoKeyState{updatedAt: t0, flagged: false}
	flaggedT0 := repoKeyState{updatedAt: t0, flagged: true}
	flaggedT1 := repoKeyState{updatedAt: t1, flagged: true}
	readErr := errors.New("disk I/O error")

	for _, tc := range []struct {
		name                 string
		before, after, fresh repoKeyState
		freshErr             error
		want                 commitOutcome
	}{
		{"flag 0 to 1, fresh flagged: landed", unflagged, flaggedT1, flaggedT1, nil, commitLanded},
		{"flag 0 to 1, fresh unflagged: not landed", unflagged, flaggedT1, unflagged, nil, commitNotLanded},
		{"flag 0 to 1 decided by the flag despite a concurrent updated_at", unflagged, flaggedT1,
			repoKeyState{updatedAt: t2, flagged: true}, nil, commitLanded},
		{"flag 1 to 0, fresh unflagged: landed", flaggedT0, repoKeyState{updatedAt: t1}, repoKeyState{updatedAt: t1}, nil, commitLanded},
		{"move with no updated_at, fresh flagged: landed", repoKeyState{}, repoKeyState{flagged: true},
			repoKeyState{updatedAt: t0, flagged: true}, nil, commitLanded},
		{"move with no updated_at, fresh unflagged: not landed", repoKeyState{}, repoKeyState{flagged: true},
			repoKeyState{updatedAt: t0}, nil, commitNotLanded},
		{"flagged row, fresh has the write's updated_at: landed", flaggedT0, flaggedT1, flaggedT1, nil, commitLanded},
		{"flagged row, fresh has the previous updated_at: not landed", flaggedT0, flaggedT1, flaggedT0, nil, commitNotLanded},
		{"flagged row, concurrent updated_at change: unknown", flaggedT0, flaggedT1,
			repoKeyState{updatedAt: t2, flagged: true}, nil, commitUnknown},
		{"flagged row, write did not move updated_at: unknown", flaggedT0, flaggedT0, flaggedT0, nil, commitUnknown},
		{"row gone: unknown", unflagged, flaggedT1, repoKeyState{}, sql.ErrNoRows, commitUnknown},
		{"read error: unknown", flaggedT0, flaggedT1, repoKeyState{}, readErr, commitUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyKeyCommit(tc.before, tc.after, tc.fresh, tc.freshErr); got != tc.want {
				t.Fatalf("classifyKeyCommit = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestNextUpdatedAt: a keyed update always moves updated_at strictly forward,
// even within one millisecond or under a clock that stepped back, so
// classifyKeyCommit can decide a flagged row's commit (BOS-1658).
func TestNextUpdatedAt(t *testing.T) {
	const (
		t0 = "2026-01-01T00:00:00.000Z"
		t1 = "2026-01-01T00:00:00.001Z"
	)
	for _, tc := range []struct{ name, now, after, want string }{
		{"no floor", t0, "", t0},
		{"now already later", t1, t0, t1},
		{"same millisecond", t0, t0, t1},
		{"clock stepped back", t0, t1, "2026-01-01T00:00:00.002Z"},
		{"unparsable floor ignored", t0, "zzzz", t0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextUpdatedAt(tc.now, tc.after); got != tc.want {
				t.Fatalf("nextUpdatedAt(%q, %q) = %q, want %q", tc.now, tc.after, got, tc.want)
			}
		})
	}
}

// errInjectedCommit is the COMMIT failure the commit seams below report.
var errInjectedCommit = errors.New("injected commit failure")

// commitThenFail runs the real COMMIT and then reports a failure: a COMMIT
// error whose transaction landed anyway.
func commitThenFail(ctx context.Context, conn *sql.Conn) error {
	if err := execCommit(ctx, conn); err != nil {
		return err
	}
	return errInjectedCommit
}

// failWithoutCommit reports a COMMIT failure without committing, so the
// caller's rollback discards the transaction: a COMMIT error that did not land.
func failWithoutCommit(context.Context, *sql.Conn) error {
	return errInjectedCommit
}

// commitDeleteThenFail runs the real COMMIT, deletes the repo row, then reports
// a failure, so the read that resolves the error finds no row: an outcome that
// cannot be decided.
func commitDeleteThenFail(id string) commitFunc {
	return func(ctx context.Context, conn *sql.Conn) error {
		if err := execCommit(ctx, conn); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM repos WHERE id = ?`, id); err != nil {
			return err
		}
		return errInjectedCommit
	}
}

// TestRepoStore_CommitErrorResolvedFromFreshRead (BOS-1658): when a key
// update's COMMIT reports an error, a fresh read decides what happened before
// the keyring write is compensated. A commit that landed keeps the new keyring
// item and Update succeeds; one that did not land is undone exactly as before;
// one that cannot be decided is left alone rather than undone on a guess.
func TestRepoStore_CommitErrorResolvedFromFreshRead(t *testing.T) {
	ctx := context.Background()

	type fixture struct {
		dbConn               *sql.DB
		ring                 *reposecretstest.Ring
		store                *SQLiteRepoStore
		id                   string
		oldLinear, oldSentry string
	}
	rows := []struct {
		name    string
		flagged bool
	}{
		{"unflagged row", false},
		{"flagged row", true},
	}
	setup := func(t *testing.T, flagged bool) fixture {
		t.Helper()
		dbConn := setupTestDB(t)
		ring := reposecretstest.NewRing()
		store := NewRepoStore(dbConn, WithRepoSecrets(reposecretstest.NewStore(ring)))
		repo := createTestRepo(t, store)
		linear, sentry := markerKeys("old")
		if flagged {
			setRepoKeys(t, store, repo.ID, linear, sentry)
		} else if _, err := dbConn.ExecContext(ctx,
			`UPDATE repos SET linear_api_key = ?, sentry_api_key = ?, has_keyring_secrets = 0 WHERE id = ?`,
			linear, sentry, repo.ID); err != nil {
			t.Fatalf("seed key columns: %v", err)
		}
		return fixture{dbConn: dbConn, ring: ring, store: store, id: repo.ID, oldLinear: linear, oldSentry: sentry}
	}
	// ringView reads the keyring item through a fresh store, bypassing the
	// writing store's cache.
	ringView := func(t *testing.T, f fixture) reposecrets.Secrets {
		t.Helper()
		sec, err := reposecretstest.NewStore(f.ring).Get(ctx, f.id)
		if err != nil {
			t.Fatalf("read keyring item: %v", err)
		}
		return sec
	}

	for _, row := range rows {
		t.Run("landed/"+row.name, func(t *testing.T) {
			f := setup(t, row.flagged)
			f.store.commit = commitThenFail
			newLinear, _ := markerKeys("new")

			got, err := f.store.Update(ctx, f.id, UpdateRepoParams{LinearAPIKey: &newLinear})
			if err != nil {
				t.Fatalf("Update whose COMMIT landed returned %v, want nil", err)
			}
			if got.LinearAPIKey != newLinear || got.SentryAPIKey != f.oldSentry {
				t.Fatal("Update did not return the keys it committed")
			}
			if rl, rs, flag := rawRepoSecretColumns(t, f.dbConn, f.id); rl != "" || rs != "" || flag != 1 {
				t.Fatalf("row after a landed commit: columns not cleared or flag %d != 1", flag)
			}
			if sec := ringView(t, f); sec.LinearAPIKey != newLinear || sec.SentryAPIKey != f.oldSentry {
				t.Fatal("a landed commit's keyring item was undone")
			}
		})

		t.Run("not landed/"+row.name, func(t *testing.T) {
			f := setup(t, row.flagged)
			f.store.commit = failWithoutCommit
			newLinear, _ := markerKeys("new")

			_, err := f.store.Update(ctx, f.id, UpdateRepoParams{LinearAPIKey: &newLinear})
			if err == nil || !errors.Is(err, errInjectedCommit) || !strings.Contains(err.Error(), "commit repo update") {
				t.Fatalf("Update err = %v, want the commit repo update error", err)
			}
			rl, rs, flag := rawRepoSecretColumns(t, f.dbConn, f.id)
			if row.flagged {
				if rl != "" || rs != "" || flag != 1 {
					t.Fatal("a commit that did not land changed the flagged row")
				}
				if sec := ringView(t, f); sec.LinearAPIKey != f.oldLinear || sec.SentryAPIKey != f.oldSentry {
					t.Fatal("a commit that did not land left the new key in the keyring item")
				}
				return
			}
			if rl != f.oldLinear || rs != f.oldSentry || flag != 0 {
				t.Fatal("a commit that did not land changed the unflagged row's key columns")
			}
			if f.ring.Has(reposecrets.ItemKey(f.id)) {
				t.Fatal("a commit that did not land left a keyring item for a row whose columns are authoritative")
			}
		})

		t.Run("unknown/"+row.name, func(t *testing.T) {
			f := setup(t, row.flagged)
			f.store.commit = commitDeleteThenFail(f.id)
			newLinear, _ := markerKeys("new")

			_, err := f.store.Update(ctx, f.id, UpdateRepoParams{LinearAPIKey: &newLinear})
			if err == nil || !errors.Is(err, errInjectedCommit) || !strings.Contains(err.Error(), "unknown") {
				t.Fatalf("Update err = %v, want an outcome-unknown error", err)
			}
			if sec := ringView(t, f); sec.LinearAPIKey != newLinear || sec.SentryAPIKey != f.oldSentry {
				t.Fatal("an undecidable commit's keyring item was undone on a guess")
			}
		})
	}
}

// TestMigrateRepoSecretsFromSQLite_CommitError (BOS-1658): the startup move
// resolves a COMMIT error from a fresh read too. A move that landed is reported
// as moved; one that did not leaves the row as it was, to retry next start.
func TestMigrateRepoSecretsFromSQLite_CommitError(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*sql.DB, *reposecretstest.Ring, *reposecrets.Store, string, string, string) {
		t.Helper()
		dbConn := setupTestDB(t)
		plain := NewRepoStore(dbConn)
		repo := createTestRepo(t, plain)
		linear, sentry := markerKeys("mv")
		setRepoKeys(t, plain, repo.ID, linear, sentry)
		ring := reposecretstest.NewRing()
		return dbConn, ring, reposecretstest.NewStore(ring), repo.ID, linear, sentry
	}

	t.Run("landed", func(t *testing.T) {
		dbConn, ring, secrets, id, linear, sentry := setup(t)
		moved, err := migrateRepoSecrets(ctx, dbConn, secrets, commitThenFail)
		if err != nil || moved != 1 {
			t.Fatalf("migrate = (%d, %v), want (1, nil) for a move whose COMMIT landed", moved, err)
		}
		if rl, rs, flag := rawRepoSecretColumns(t, dbConn, id); rl != "" || rs != "" || flag != 1 {
			t.Fatalf("row after a landed move: columns not cleared or flag %d != 1", flag)
		}
		sec, err := reposecretstest.NewStore(ring).Get(ctx, id)
		if err != nil || sec.LinearAPIKey != linear || sec.SentryAPIKey != sentry {
			t.Fatalf("keyring item after a landed move does not hold the keys (err %v)", err)
		}
	})

	t.Run("not landed", func(t *testing.T) {
		dbConn, _, secrets, id, linear, sentry := setup(t)
		moved, err := migrateRepoSecrets(ctx, dbConn, secrets, failWithoutCommit)
		if moved != 0 || !errors.Is(err, errInjectedCommit) {
			t.Fatalf("migrate = (%d, %v), want (0, the commit error)", moved, err)
		}
		if rl, rs, flag := rawRepoSecretColumns(t, dbConn, id); rl != linear || rs != sentry || flag != 0 {
			t.Fatal("a move whose COMMIT did not land changed the row")
		}

		// The next start moves it.
		moved, err = migrateRepoSecrets(ctx, dbConn, secrets, execCommit)
		if err != nil || moved != 1 {
			t.Fatalf("retry migrate = (%d, %v), want (1, nil)", moved, err)
		}
		if rl, rs, flag := rawRepoSecretColumns(t, dbConn, id); rl != "" || rs != "" || flag != 1 {
			t.Fatalf("row after the retried move: columns not cleared or flag %d != 1", flag)
		}
	})
}

// TestMigrateRepoSecretsFromSQLite_UnavailableKeyring: with no keyring at all,
// the move changes nothing and reports the keyring as unavailable.
func TestMigrateRepoSecretsFromSQLite_UnavailableKeyring(t *testing.T) {
	ctx := context.Background()
	dbConn := setupTestDB(t)
	plain := NewRepoStore(dbConn)
	repo := createTestRepo(t, plain)
	linear, sentry := markerKeys("u")
	setRepoKeys(t, plain, repo.ID, linear, sentry)

	moved, err := MigrateRepoSecretsFromSQLite(ctx, dbConn, reposecrets.NewUnavailable())
	if moved != 0 || err == nil {
		t.Fatalf("migrate = (%d, %v), want (0, non-nil)", moved, err)
	}
	rl, rs, flag := rawRepoSecretColumns(t, dbConn, repo.ID)
	if rl != linear || rs != sentry || flag != 0 {
		t.Fatal("row changed although the keyring is unavailable")
	}
}

// keyringPromptly is how long a repo store call that must not wait on a
// blocked keychain may take. It is well under the 5s SQLite busy timeout, so a
// write that queued behind a held write lock reads as a failure.
const keyringPromptly = 2 * time.Second

// returnsPromptly runs call and fails the test, naming what, unless it returns
// within keyringPromptly.
func returnsPromptly(t *testing.T, what string, call func() error) error {
	t.Helper()
	errCh := make(chan error, 1)
	go func() { errCh <- call() }()
	select {
	case err := <-errCh:
		return err
	case <-time.After(keyringPromptly):
		t.Fatalf("%s did not return within %v while the keychain was blocked", what, keyringPromptly)
		return nil
	}
}

// awaitRingEntered waits until a keyring call is blocked inside ring.
func awaitRingEntered(t *testing.T, ring *reposecretstest.Ring) {
	t.Helper()
	select {
	case <-ring.Entered():
	case <-time.After(5 * time.Second):
		t.Fatal("no keyring call started")
	}
}

// awaitKeyringPending waits until n keyring ops are queued or running on
// repoID's item.
func awaitKeyringPending(t *testing.T, secrets *reposecrets.Store, repoID string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for secrets.Pending(repoID) != n {
		if time.Now().After(deadline) {
			t.Fatalf("pending keyring ops = %d, want %d", secrets.Pending(repoID), n)
		}
		runtime.Gosched()
	}
}

// otherWriteSucceeds proves the SQLite write lock (and, on the single-conn
// in-memory test database, the connection) is free: a plain write must finish
// promptly.
func otherWriteSucceeds(t *testing.T, dbConn *sql.DB, id string) {
	t.Helper()
	err := returnsPromptly(t, "a concurrent SQLite write", func() error {
		_, err := dbConn.ExecContext(context.Background(),
			`UPDATE repos SET setup_script = 'other-writer' WHERE id = ?`, id)
		return err
	})
	if err != nil {
		t.Fatalf("concurrent SQLite write: %v", err)
	}
}

// keyringRepo returns a store over a fresh blocking-capable ring with one repo
// whose keys (tag "kept") are already in the keyring.
func keyringRepo(t *testing.T) (*sql.DB, *reposecretstest.Ring, *reposecrets.Store, *SQLiteRepoStore, *models.Repo) {
	t.Helper()
	dbConn := setupTestDB(t)
	ring := reposecretstest.NewRing()
	secrets := reposecretstest.NewStore(ring)
	store := NewRepoStore(dbConn, WithRepoSecrets(secrets))
	repo := createTestRepo(t, store)
	linear, sentry := markerKeys("kept")
	setRepoKeys(t, store, repo.ID, linear, sentry)
	return dbConn, ring, secrets, store, repo
}

// ringItem reads repoID's keyring item through a fresh store, bypassing the
// writing store's cache.
func ringItem(t *testing.T, ring *reposecretstest.Ring, repoID string) reposecrets.Secrets {
	t.Helper()
	sec, err := reposecretstest.NewStore(ring).Get(context.Background(), repoID)
	if err != nil {
		t.Fatalf("read keyring item: %v", err)
	}
	return sec
}

// TestRepoStore_AbandonedKeyringWriteReleasesWriteLock: when Update's ctx ends
// while its keyring write is stuck in the keychain, Update returns at once and
// releases the SQLite write lock, without waiting for the keychain. The write
// may still land later, so its undo is queued behind it and puts the item back.
func TestRepoStore_AbandonedKeyringWriteReleasesWriteLock(t *testing.T) {
	ctx := context.Background()
	dbConn, ring, _, store, repo := keyringRepo(t)
	linear, sentry := markerKeys("kept")

	release := ring.Block(reposecrets.ItemKey(repo.ID))
	defer release()
	updateCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	newLinear, _ := markerKeys("abandoned")
	errCh := make(chan error, 1)
	go func() {
		_, err := store.Update(updateCtx, repo.ID, UpdateRepoParams{LinearAPIKey: &newLinear})
		errCh <- err
	}()
	awaitRingEntered(t, ring)
	cancel()

	if err := returnsPromptly(t, "Update with a cancelled ctx", func() error { return <-errCh }); err == nil {
		t.Fatal("Update succeeded although its keyring write was abandoned")
	}
	otherWriteSucceeds(t, dbConn, repo.ID)

	release()
	// Read through the writing store first: its Get queues behind the
	// abandoned write and the undo, so it sees what they leave behind.
	got, err := store.Get(ctx, repo.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.LinearAPIKey != linear || got.SentryAPIKey != sentry {
		t.Fatal("failed Update's abandoned key is what the repo now reads")
	}
	if sec := ringItem(t, ring, repo.ID); sec.LinearAPIKey != linear || sec.SentryAPIKey != sentry {
		t.Fatal("abandoned keyring write from a failed Update is live in the keyring item")
	}
}

// TestRepoStore_LaterUpdateSurvivesAbandonedWriteUndo: a key Update started
// after an abandoned one returned queues its keyring read and write behind the
// abandoned write's undo, so the later keys are what remain. (That the undo is
// queued before the repo's key lock is released, for an Update already waiting
// on it, is TestRepoStore_AbandonedWriteUndoQueuedBeforeNextKeyUpdate.)
func TestRepoStore_LaterUpdateSurvivesAbandonedWriteUndo(t *testing.T) {
	ctx := context.Background()
	_, ring, secrets, store, repo := keyringRepo(t)
	linear, _ := markerKeys("kept")

	release := ring.Block(reposecrets.ItemKey(repo.ID))
	defer release()
	abandonCtx, abandon := context.WithCancel(ctx)
	defer abandon()
	abandonedLinear, _ := markerKeys("abandoned")
	abandonedErr := make(chan error, 1)
	go func() {
		_, err := store.Update(abandonCtx, repo.ID, UpdateRepoParams{LinearAPIKey: &abandonedLinear})
		abandonedErr <- err
	}()
	awaitRingEntered(t, ring)
	abandon()
	if err := returnsPromptly(t, "abandoned Update", func() error { return <-abandonedErr }); err == nil {
		t.Fatal("abandoned Update succeeded")
	}
	// The stuck write and its undo are queued.
	awaitKeyringPending(t, secrets, repo.ID, 2)

	_, laterSentry := markerKeys("later")
	laterErr := make(chan error, 1)
	go func() {
		_, err := store.Update(ctx, repo.ID, UpdateRepoParams{SentryAPIKey: &laterSentry})
		laterErr <- err
	}()
	// The later Update's keyring read is queued behind both.
	awaitKeyringPending(t, secrets, repo.ID, 3)

	release()
	if err := returnsPromptly(t, "later Update", func() error { return <-laterErr }); err != nil {
		t.Fatalf("later Update: %v", err)
	}
	got, err := store.Get(ctx, repo.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.LinearAPIKey != linear || got.SentryAPIKey != laterSentry {
		t.Fatal("repo keys after the later Update are not the kept Linear key with the later Sentry key")
	}
	if sec := ringItem(t, ring, repo.ID); sec.LinearAPIKey != linear || sec.SentryAPIKey != laterSentry {
		t.Fatal("keyring item after the later Update is not the kept Linear key with the later Sentry key")
	}
}

// TestRepoStore_KeyringPhaseIsBounded: a request ctx with no deadline must not
// let a hung keychain hold the repo's key lock indefinitely. Update's keyring
// phase gives up after keyringTimeout and releases the lock.
func TestRepoStore_KeyringPhaseIsBounded(t *testing.T) {
	ctx := context.Background()
	dbConn, ring, _, store, repo := keyringRepo(t)
	linear, sentry := markerKeys("kept")
	store.keyringTimeout = 50 * time.Millisecond

	release := ring.Block(reposecrets.ItemKey(repo.ID))
	defer release()
	newLinear, _ := markerKeys("hung")
	err := returnsPromptly(t, "Update with a hung keychain and no deadline", func() error {
		_, err := store.Update(ctx, repo.ID, UpdateRepoParams{LinearAPIKey: &newLinear})
		return err
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Update err = %v, want context.DeadlineExceeded", err)
	}
	otherWriteSucceeds(t, dbConn, repo.ID)

	release()
	got, err := store.Get(ctx, repo.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.LinearAPIKey != linear || got.SentryAPIKey != sentry {
		t.Fatal("timed-out Update's key is what the repo now reads")
	}
}

// TestRepoStore_KeyLockWaitIsBounded: a repo's key lock held past every
// holder's own bound (here by the test) must not block a key Update or a
// Delete forever. Update gives up after keyringTimeout; Delete gives up after
// deleteKeyLockWait and still removes the deleted repo's keyring item.
func TestRepoStore_KeyLockWaitIsBounded(t *testing.T) {
	ctx := context.Background()
	_, ring, secrets, store, repo := keyringRepo(t)
	store.keyringTimeout = 50 * time.Millisecond

	unlock, _, err := store.keyLocks.Acquire(ctx, repo.ID, time.Second)
	if err != nil {
		t.Fatalf("hold the repo key lock: %v", err)
	}
	defer unlock()

	newLinear, _ := markerKeys("waiting")
	err = returnsPromptly(t, "key Update behind a held key lock", func() error {
		_, err := store.Update(ctx, repo.ID, UpdateRepoParams{LinearAPIKey: &newLinear})
		return err
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Update err = %v, want context.DeadlineExceeded", err)
	}

	if err := returnsPromptly(t, "Delete behind a held key lock", func() error {
		return store.Delete(ctx, repo.ID)
	}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	awaitKeyringPending(t, secrets, repo.ID, 0)
	if ring.Has(reposecrets.ItemKey(repo.ID)) {
		t.Fatal("a deleted repo's keyring item survived a key lock held past Delete's wait")
	}
}

// TestRepoStore_UpdateHydratesOutsideWriteLock: an Update that writes no key
// still reads a flagged row's keys back, and that keychain read must not run
// while the update holds the SQLite write lock (or its connection): the update
// is committed and other writers proceed while the read is stuck.
func TestRepoStore_UpdateHydratesOutsideWriteLock(t *testing.T) {
	ctx := context.Background()
	dbConn, ring, secrets, store, repo := keyringRepo(t)
	linear, sentry := markerKeys("kept")
	secrets.Forget(repo.ID) // make the read-back reach the keychain

	release := ring.Block(reposecrets.ItemKey(repo.ID))
	defer release()
	origin := "https://github.com/test/moved.git"
	type result struct {
		repo *models.Repo
		err  error
	}
	done := make(chan result, 1)
	go func() {
		r, err := store.Update(ctx, repo.ID, UpdateRepoParams{OriginURL: &origin})
		done <- result{r, err}
	}()
	awaitRingEntered(t, ring)

	var committed string
	if err := returnsPromptly(t, "reading the committed origin", func() error {
		return dbConn.QueryRowContext(ctx, `SELECT origin_url FROM repos WHERE id = ?`, repo.ID).Scan(&committed)
	}); err != nil {
		t.Fatalf("read origin: %v", err)
	}
	if committed != origin {
		t.Fatal("origin update not committed while its keychain read-back was stuck")
	}
	otherWriteSucceeds(t, dbConn, repo.ID)

	release()
	res := <-done
	if res.err != nil {
		t.Fatalf("Update: %v", res.err)
	}
	if res.repo.OriginURL != origin || res.repo.LinearAPIKey != linear || res.repo.SentryAPIKey != sentry {
		t.Fatal("Update did not return the new origin with the keys hydrated")
	}
}

// TestRepoStore_KeychainWaitHoldsNoWriteLock: a key Update waiting on the
// keychain (an unanswered approval prompt) must not hold the SQLite write lock
// or a connection, or every other bossd writer fails at the busy timeout while
// the prompt sits there. A direct write to the same row and a store Update of
// another repo proceed while the keychain is blocked, and the key update still
// lands once it answers.
func TestRepoStore_KeychainWaitHoldsNoWriteLock(t *testing.T) {
	ctx := context.Background()
	dbConn, ring, _, store, repo := keyringRepo(t)
	_, sentry := markerKeys("kept")
	other, err := store.Create(ctx, CreateRepoParams{
		DisplayName: "other", LocalPath: "/tmp/other-repo", OriginURL: "https://github.com/test/other.git",
		DefaultBaseBranch: "main", WorktreeBaseDir: "/tmp/wt-other",
	})
	if err != nil {
		t.Fatalf("create other repo: %v", err)
	}

	release := ring.Block(reposecrets.ItemKey(repo.ID))
	defer release()
	newLinear, _ := markerKeys("prompted")
	done := make(chan error, 1)
	go func() {
		_, err := store.Update(ctx, repo.ID, UpdateRepoParams{LinearAPIKey: &newLinear})
		done <- err
	}()
	awaitRingEntered(t, ring)

	otherWriteSucceeds(t, dbConn, repo.ID)
	name := "renamed-while-prompting"
	if err := returnsPromptly(t, "another repo's Update", func() error {
		_, err := store.Update(ctx, other.ID, UpdateRepoParams{DisplayName: &name})
		return err
	}); err != nil {
		t.Fatalf("another repo's Update while the keychain was blocked: %v", err)
	}

	release()
	if err := <-done; err != nil {
		t.Fatalf("key Update after the keychain answered: %v", err)
	}
	got, err := store.Get(ctx, repo.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.SetupScript == nil || *got.SetupScript != "other-writer" || got.LinearAPIKey != newLinear || got.SentryAPIKey != sentry {
		t.Fatal("repo after both writes is not the other writer's change with the new Linear key and the kept Sentry key")
	}
	if _, _, flag := rawRepoSecretColumns(t, dbConn, repo.ID); flag != 1 {
		t.Fatalf("has_keyring_secrets = %d after the key update, want 1", flag)
	}
}

// TestRepoStore_DeleteRemovesKeyringItemAfterQueuedUndo: a repo Delete whose
// ctx ends while the repo's keyring item is busy (here an abandoned Update's
// write is stuck in the keychain with its undo queued behind it) must still
// remove the item once the keychain answers. A removal skipped as abandoned
// would leave the undo's restored item — a deleted repo's API keys — in the
// keychain indefinitely.
func TestRepoStore_DeleteRemovesKeyringItemAfterQueuedUndo(t *testing.T) {
	ctx := context.Background()
	_, ring, secrets, store, repo := keyringRepo(t)

	release := ring.Block(reposecrets.ItemKey(repo.ID))
	defer release()
	abandonCtx, abandon := context.WithCancel(ctx)
	defer abandon()
	abandonedLinear, _ := markerKeys("abandoned")
	abandonedErr := make(chan error, 1)
	go func() {
		_, err := store.Update(abandonCtx, repo.ID, UpdateRepoParams{LinearAPIKey: &abandonedLinear})
		abandonedErr <- err
	}()
	awaitRingEntered(t, ring)
	abandon()
	if err := returnsPromptly(t, "abandoned Update", func() error { return <-abandonedErr }); err == nil {
		t.Fatal("abandoned Update succeeded")
	}
	awaitKeyringPending(t, secrets, repo.ID, 2) // the stuck write and its undo

	deleteCtx, endDelete := context.WithCancel(ctx)
	defer endDelete()
	deleteErr := make(chan error, 1)
	go func() { deleteErr <- store.Delete(deleteCtx, repo.ID) }()
	awaitKeyringPending(t, secrets, repo.ID, 3) // the removal, queued behind both
	endDelete()
	if err := returnsPromptly(t, "Delete whose ctx ended", func() error { return <-deleteErr }); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	release()
	awaitKeyringPending(t, secrets, repo.ID, 0)
	if ring.Has(reposecrets.ItemKey(repo.ID)) {
		t.Fatal("a deleted repo's keyring item survived because its removal was skipped")
	}
}

// observedKeyLocks wraps a store's repo key lock to count, per repo, the
// callers that hold or wait for it, and (when onRelease is set) to hand each
// release to the test. onRelease must call release exactly once.
type observedKeyLocks struct {
	inner     repoKeyLocker
	onRelease func(id string, release func())

	mu    sync.Mutex
	users map[string]int
}

// observeKeyLocks installs an observedKeyLocks on store. Install it before
// starting the callers it should count; onRelease, if needed, must be set
// before then too.
func observeKeyLocks(store *SQLiteRepoStore) *observedKeyLocks {
	o := &observedKeyLocks{inner: store.keyLocks, users: make(map[string]int)}
	store.keyLocks = o
	return o
}

func (o *observedKeyLocks) add(id string, n int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.users[id] += n
}

func (o *observedKeyLocks) count(id string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.users[id]
}

func (o *observedKeyLocks) Acquire(ctx context.Context, id string, timeout time.Duration) (func(), time.Duration, error) {
	o.add(id, 1)
	release, waited, err := o.inner.Acquire(ctx, id, timeout)
	if err != nil {
		o.add(id, -1)
		return nil, waited, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			o.add(id, -1)
			if o.onRelease != nil {
				o.onRelease(id, release)
				return
			}
			release()
		})
	}, waited, nil
}

// awaitKeyLockUsers waits until n callers hold or wait for repoID's key lock,
// or until queued keyring ops on its item reach pending (what a key update
// that skipped the lock would leave instead).
func awaitKeyLockUsers(t *testing.T, locks *observedKeyLocks, secrets *reposecrets.Store, repoID string, n, pending int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for locks.count(repoID) != n && secrets.Pending(repoID) != pending {
		if time.Now().After(deadline) {
			t.Fatalf("key lock users = %d (want %d), pending keyring ops = %d (want %d)",
				locks.count(repoID), n, secrets.Pending(repoID), pending)
		}
		runtime.Gosched()
	}
}

// TestRepoStore_ConcurrentKeyUpdatesKeepEachOthersKey: two key updates of one
// repo whose keys are still in the columns must not merge from the same
// snapshot. The second waits while the first's keyring write is stuck, then
// reads the keys the first recorded, so neither drops the key the other set.
func TestRepoStore_ConcurrentKeyUpdatesKeepEachOthersKey(t *testing.T) {
	ctx := context.Background()
	dbConn := setupTestDB(t)
	ring := reposecretstest.NewRing()
	secrets := reposecretstest.NewStore(ring)
	store := NewRepoStore(dbConn, WithRepoSecrets(secrets))
	locks := observeKeyLocks(store)
	repo := createTestRepo(t, store)
	colLinear, colSentry := markerKeys("cols")
	if _, err := dbConn.ExecContext(ctx,
		`UPDATE repos SET linear_api_key = ?, sentry_api_key = ?, has_keyring_secrets = 0 WHERE id = ?`,
		colLinear, colSentry, repo.ID); err != nil {
		t.Fatalf("seed key columns: %v", err)
	}

	release := ring.Block(reposecrets.ItemKey(repo.ID))
	defer release()
	firstLinear, _ := markerKeys("first")
	firstErr := make(chan error, 1)
	go func() {
		_, err := store.Update(ctx, repo.ID, UpdateRepoParams{LinearAPIKey: &firstLinear})
		firstErr <- err
	}()
	awaitRingEntered(t, ring)

	_, secondSentry := markerKeys("second")
	secondErr := make(chan error, 1)
	go func() {
		_, err := store.Update(ctx, repo.ID, UpdateRepoParams{SentryAPIKey: &secondSentry})
		secondErr <- err
	}()
	awaitKeyLockUsers(t, locks, secrets, repo.ID, 2, 2)

	release()
	for _, ch := range []chan error{firstErr, secondErr} {
		if err := <-ch; err != nil {
			t.Fatalf("concurrent key Update: %v", err)
		}
	}
	if sec := ringItem(t, ring, repo.ID); sec.LinearAPIKey != firstLinear || sec.SentryAPIKey != secondSentry {
		t.Fatal("keyring item after two concurrent key updates lost one of their keys")
	}
	if rl, rs, flag := rawRepoSecretColumns(t, dbConn, repo.ID); rl != "" || rs != "" || flag != 1 {
		t.Fatal("key columns not cleared and flagged after two concurrent key updates")
	}
}

// TestRepoStore_AbandonedWriteUndoQueuedBeforeNextKeyUpdate: an Update whose
// keyring write is abandoned must queue its undo before it lets the next key
// update of the repo start. Otherwise the next update can read the abandoned
// value (its read queued ahead of the undo) and write it back after the undo.
// The onRelease hook holds the abandoning Update just after it lets go of the
// key lock until the waiting Update has queued its keyring read, which is the
// window an undo queued after the release would lose. It counts the queued
// ops while the lock is still held, when the waiting Update cannot have queued
// anything and the blocked write keeps every op queued, so that read is the
// one op it waits for whenever the release lets it in.
func TestRepoStore_AbandonedWriteUndoQueuedBeforeNextKeyUpdate(t *testing.T) {
	ctx := context.Background()
	_, ring, secrets, store, repo := keyringRepo(t)
	linear, _ := markerKeys("kept")

	locks := observeKeyLocks(store)
	var holdOnce sync.Once
	locks.onRelease = func(id string, release func()) {
		held := true
		holdOnce.Do(func() {
			before := secrets.Pending(id)
			release()
			held = false
			deadline := time.Now().Add(10 * time.Second)
			for secrets.Pending(id) == before {
				if time.Now().After(deadline) {
					t.Errorf("waiting Update queued no keyring read after the key lock was released (pending ops = %d)", before)
					return
				}
				runtime.Gosched()
			}
		})
		if held {
			release()
		}
	}

	release := ring.Block(reposecrets.ItemKey(repo.ID))
	defer release()
	abandonCtx, abandon := context.WithCancel(ctx)
	defer abandon()
	abandonedLinear, _ := markerKeys("abandoned")
	abandonedErr := make(chan error, 1)
	go func() {
		_, err := store.Update(abandonCtx, repo.ID, UpdateRepoParams{LinearAPIKey: &abandonedLinear})
		abandonedErr <- err
	}()
	awaitRingEntered(t, ring)

	_, laterSentry := markerKeys("later")
	laterErr := make(chan error, 1)
	go func() {
		_, err := store.Update(ctx, repo.ID, UpdateRepoParams{SentryAPIKey: &laterSentry})
		laterErr <- err
	}()
	awaitKeyLockUsers(t, locks, secrets, repo.ID, 2, 2)

	abandon()
	if err := returnsPromptly(t, "abandoned Update", func() error { return <-abandonedErr }); err == nil {
		t.Fatal("abandoned Update succeeded")
	}
	// The stuck write, its undo and the later Update's keyring read.
	awaitKeyringPending(t, secrets, repo.ID, 3)

	release()
	if err := returnsPromptly(t, "later Update", func() error { return <-laterErr }); err != nil {
		t.Fatalf("later Update: %v", err)
	}
	if sec := ringItem(t, ring, repo.ID); sec.LinearAPIKey != linear || sec.SentryAPIKey != laterSentry {
		t.Fatal("keyring item after the later Update is not the kept Linear key with the later Sentry key")
	}
	got, err := store.Get(ctx, repo.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.LinearAPIKey != linear || got.SentryAPIKey != laterSentry {
		t.Fatal("repo keys after the later Update are not the kept Linear key with the later Sentry key")
	}
}

// TestRepoStore_DeleteDuringKeyUpdateRemovesItem: a repo deleted while a key
// update of it is stuck in the keychain must end with no keyring item. The
// update then fails (its row is gone) and its undo puts the previous item
// back, so Delete's removal has to land after that undo, not between the stuck
// write and it.
func TestRepoStore_DeleteDuringKeyUpdateRemovesItem(t *testing.T) {
	ctx := context.Background()
	_, ring, secrets, store, repo := keyringRepo(t)
	locks := observeKeyLocks(store)

	release := ring.Block(reposecrets.ItemKey(repo.ID))
	defer release()
	newLinear, _ := markerKeys("racing")
	updateErr := make(chan error, 1)
	go func() {
		_, err := store.Update(ctx, repo.ID, UpdateRepoParams{LinearAPIKey: &newLinear})
		updateErr <- err
	}()
	awaitRingEntered(t, ring)

	deleteErr := make(chan error, 1)
	go func() { deleteErr <- store.Delete(ctx, repo.ID) }()
	awaitKeyLockUsers(t, locks, secrets, repo.ID, 2, 2)

	release()
	if err := <-updateErr; !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Update of a repo deleted under it: err = %v, want sql.ErrNoRows", err)
	}
	if err := <-deleteErr; err != nil {
		t.Fatalf("Delete: %v", err)
	}
	awaitKeyringPending(t, secrets, repo.ID, 0)
	if ring.Has(reposecrets.ItemKey(repo.ID)) {
		t.Fatal("a repo deleted during a key update kept its keyring item")
	}
}

// TestRepoStore_HydrateReadsAreBounded: repo reads run on paths with no
// deadline (daemon startup lists repos with context.Background), so a keychain
// read that never answers must not block them. Get returns once its read hits
// readTimeout, and List returns once its reads together hit listReadBudget,
// with the unreadable keys degraded to empty.
func TestRepoStore_HydrateReadsAreBounded(t *testing.T) {
	t.Run("per read", func(t *testing.T) {
		dbConn, ring, _, _, repo := keyringRepo(t)
		// A fresh secret store has an empty cache, so the read reaches the ring.
		fresh := NewRepoStore(dbConn, WithRepoSecrets(reposecretstest.NewStore(ring)))
		fresh.readTimeout = 50 * time.Millisecond
		fresh.listReadBudget = time.Hour

		release := ring.Block(reposecrets.ItemKey(repo.ID))
		defer release()
		var got *models.Repo
		err := returnsPromptly(t, "Get with a hung keychain and no deadline", func() error {
			var err error
			got, err = fresh.Get(context.Background(), repo.ID)
			return err
		})
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.LinearAPIKey != "" || got.SentryAPIKey != "" {
			t.Fatal("Get returned keys although the keychain never answered")
		}
	})

	t.Run("cached keys survive a spent budget", func(t *testing.T) {
		dbConn, ring, _, _, repo := keyringRepo(t)
		linear, sentry := markerKeys("kept")
		freshSecrets := reposecretstest.NewStore(ring)
		fresh := NewRepoStore(dbConn, WithRepoSecrets(freshSecrets))
		// Warm the fresh store's cache, then give listings no read budget at
		// all: a repo answerable from the cache must still read its keys.
		if _, err := fresh.Get(context.Background(), repo.ID); err != nil {
			t.Fatalf("warm Get: %v", err)
		}
		// Get returns before the worker marks the item idle; only an idle
		// item is served from the cache.
		awaitKeyringPending(t, freshSecrets, repo.ID, 0)
		// Zero, not a tiny positive budget: WithTimeout cancels a zero budget
		// before it returns, while a 1ns timer can still read as live at the
		// first Get and let the pre-fix order serve the cache too.
		fresh.listReadBudget = 0
		repos, err := fresh.List(context.Background())
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(repos) != 1 || repos[0].LinearAPIKey != linear || repos[0].SentryAPIKey != sentry {
			t.Fatal("a spent listing budget blanked keys the store could answer from its cache")
		}
	})

	t.Run("per listing", func(t *testing.T) {
		dbConn, ring, _, store, first := keyringRepo(t)
		second, err := store.Create(context.Background(), CreateRepoParams{
			DisplayName:       "second-repo",
			LocalPath:         "/tmp/second-repo",
			OriginURL:         "https://github.com/test/second.git",
			DefaultBaseBranch: "main",
			WorktreeBaseDir:   "/tmp/worktrees",
		})
		if err != nil {
			t.Fatalf("create second repo: %v", err)
		}
		linear, sentry := markerKeys("second")
		setRepoKeys(t, store, second.ID, linear, sentry)
		fresh := NewRepoStore(dbConn, WithRepoSecrets(reposecretstest.NewStore(ring)))
		fresh.readTimeout = time.Hour
		fresh.listReadBudget = 50 * time.Millisecond

		defer ring.Block(reposecrets.ItemKey(first.ID))()
		defer ring.Block(reposecrets.ItemKey(second.ID))()
		err = returnsPromptly(t, "List with a hung keychain and no deadline", func() error {
			_, err := fresh.List(context.Background())
			return err
		})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
	})
}
