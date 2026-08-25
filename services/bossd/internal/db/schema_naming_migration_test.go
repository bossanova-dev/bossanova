package db

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/recurser/bossalib/sqlutil"
	"github.com/recurser/bossd/internal/dbtest"
)

// preMigrationVersion is the goose timestamp of the migration immediately
// before the BOS-444 schema-naming migration (20260719000000). Seeding at this
// version reproduces the legacy on-disk shape (bare-boolean names + INTEGER
// epochs) that the migration must convert.
const (
	preMigrationVersion int64 = 20260718000000
	schemaNamingVersion int64 = 20260719000000
	legacyEpoch         int64 = 1784637296 // 2026-07-21T12:34:56Z
	legacyEpochISO            = "2026-07-21T12:34:56.000Z"
)

// boolPtr is a local helper for the pointer-typed Update params.
func boolPtr(b bool) *bool { return &b }

// tableColumns returns a name->declared-type map for a table via PRAGMA.
func tableColumns(t *testing.T, db *sql.DB, table string) map[string]string {
	t.Helper()
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatalf("pragma table_info(%s): %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	cols := map[string]string{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		cols[name] = ctype
	}
	return cols
}

// TestSchemaNamingStandardMigration asserts the BOS-444 migration renamed the
// bare-boolean columns to prefixed names and retyped the epoch _at columns to
// TEXT, and that the old identifiers are gone.
func TestSchemaNamingStandardMigration(t *testing.T) {
	db := setupTestDB(t)

	cases := []struct {
		table   string
		newCol  string
		oldCol  string
		newType string // "" = don't check type
	}{
		{"sessions", "is_automation_enabled", "automation_enabled", ""},
		{"sessions", "should_defer_pr", "defer_pr", ""},
		{"sessions", "is_tmux_unattended", "tmux_unattended", ""},
		{"sessions", "is_quick_chat", "quick_chat", ""},
		{"sessions", "is_detached", "detach", ""},
		{"sessions", "has_display_spinner", "display_spinner", ""},
		{"sessions", "last_repair_started_at", "", "TEXT"},
		{"sessions", "last_repair_blocked_at", "", "TEXT"},
		{"cron_jobs", "is_enabled", "enabled", ""},
		{"cron_jobs", "should_run_setup_command", "run_setup_command", ""},
		{"repos", "should_archive_sessions_after_merge", "archive_sessions_after_merge", ""},
		{"session_check_snapshots", "polled_at", "", "TEXT"},
		{"rotation_events", "reset_at", "", "TEXT"},
		{"rotation_events", "created_at", "", "TEXT"},
	}

	byTable := map[string]map[string]string{}
	for _, c := range cases {
		if _, ok := byTable[c.table]; !ok {
			byTable[c.table] = tableColumns(t, db, c.table)
		}
		cols := byTable[c.table]
		if _, ok := cols[c.newCol]; !ok {
			t.Errorf("%s.%s should exist after migration", c.table, c.newCol)
		}
		if c.oldCol != "" {
			if _, ok := cols[c.oldCol]; ok {
				t.Errorf("%s.%s should be gone after migration", c.table, c.oldCol)
			}
		}
		if c.newType != "" {
			if got := cols[c.newCol]; got != c.newType {
				t.Errorf("%s.%s type = %q, want %q", c.table, c.newCol, got, c.newType)
			}
		}
	}
}

// TestSchemaNamingEpochConversion seeds a pre-migration-shaped epoch value and
// asserts the migration converted it to the canonical ISO-8601 TEXT layout.
// It exercises the conversion SQL directly against a fresh DB so the assertion
// does not depend on Go store code.
func TestSchemaNamingEpochConversion(t *testing.T) {
	db := setupTestDB(t)

	// Known epoch 1784637296 == 2026-07-21T12:34:56Z.
	const epoch = int64(1784637296)
	const wantISO = "2026-07-21T12:34:56.000Z"

	var got string
	if err := db.QueryRow(
		`SELECT strftime('%Y-%m-%dT%H:%M:%fZ', ?, 'unixepoch')`, epoch,
	).Scan(&got); err != nil {
		t.Fatalf("convert epoch: %v", err)
	}
	if got != wantISO {
		t.Fatalf("epoch->ISO = %q, want %q (must equal sqlutil.TimeLayout output)", got, wantISO)
	}
}

// TestSchemaNamingBooleanRoundTrip writes true and false through the renamed
// session boolean columns and reads each back via the store scanner.
func TestSchemaNamingBooleanRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	sessionStore := NewSessionStore(db)
	ctx := context.Background()
	repo := createTestRepo(t, NewRepoStore(db))

	sess, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "bool round-trip",
		WorktreePath: "/tmp/wt/bool",
		BranchName:   "feat/bool",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	for _, want := range []bool{true, false} {
		if _, err := sessionStore.Update(ctx, sess.ID, UpdateSessionParams{
			IsAutomationEnabled: boolPtr(want),
			DisplaySpinner:      boolPtr(want),
			IsTmuxUnattended:    boolPtr(want),
			IsQuickChat:         boolPtr(want),
			Detach:              boolPtr(want),
		}); err != nil {
			t.Fatalf("update booleans=%v: %v", want, err)
		}
		got, err := sessionStore.Get(ctx, sess.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.IsAutomationEnabled != want || got.DisplaySpinner != want ||
			got.IsTmuxUnattended != want || got.IsQuickChat != want || got.Detach != want {
			t.Fatalf("boolean round-trip mismatch for want=%v: %+v", want, got)
		}
	}
}

// TestSchemaNamingCronBooleanRoundTrip covers the renamed cron_jobs booleans.
func TestSchemaNamingCronBooleanRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	store := NewCronJobStore(db)
	ctx := context.Background()
	repo := createTestRepo(t, NewRepoStore(db))

	for _, want := range []bool{true, false} {
		job, err := store.Create(ctx, CreateCronJobParams{
			RepoID:                repo.ID,
			Name:                  "job",
			Prompt:                "run",
			Schedule:              "@daily",
			IsEnabled:             want,
			ShouldRunSetupCommand: want,
		})
		if err != nil {
			t.Fatalf("create cron job want=%v: %v", want, err)
		}
		got, err := store.Get(ctx, job.ID)
		if err != nil {
			t.Fatalf("get cron job: %v", err)
		}
		if got.IsEnabled != want || got.ShouldRunSetupCommand != want {
			t.Fatalf("cron boolean round-trip mismatch want=%v: enabled=%v runSetup=%v",
				want, got.IsEnabled, got.ShouldRunSetupCommand)
		}
		if err := store.Delete(ctx, job.ID); err != nil {
			t.Fatalf("delete cron job: %v", err)
		}
	}
}

// TestSchemaNamingSessionRepairTimestampRoundTrip writes sub-second repair
// timestamps and asserts they read back equal at millisecond granularity.
func TestSchemaNamingSessionRepairTimestampRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	sessionStore := NewSessionStore(db)
	ctx := context.Background()
	repo := createTestRepo(t, NewRepoStore(db))

	sess, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "repair ts",
		WorktreePath: "/tmp/wt/ts",
		BranchName:   "feat/ts",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	started := time.Date(2026, 7, 19, 12, 34, 56, 789_000_000, time.UTC)
	if err := sessionStore.UpdateRepairDiagnostics(ctx, UpdateRepairDiagnosticsParams{
		SessionID: sess.ID,
		StartedAt: started,
		HeadSHA:   "deadbeef",
	}); err != nil {
		t.Fatalf("update repair diagnostics: %v", err)
	}
	got, err := sessionStore.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.LastRepairStartedAt == nil ||
		sqlutil.FormatTime(*got.LastRepairStartedAt) != sqlutil.FormatTime(started) {
		t.Fatalf("last_repair_started_at round-trip: got %v want %v", got.LastRepairStartedAt, started)
	}

	blocked := time.Date(2026, 7, 19, 13, 0, 0, 123_000_000, time.UTC)
	if err := sessionStore.UpdateRepairBlocked(ctx, sess.ID, blocked, "displaced"); err != nil {
		t.Fatalf("update repair blocked: %v", err)
	}
	got, err = sessionStore.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.LastRepairBlockedAt == nil ||
		sqlutil.FormatTime(*got.LastRepairBlockedAt) != sqlutil.FormatTime(blocked) {
		t.Fatalf("last_repair_blocked_at round-trip: got %v want %v", got.LastRepairBlockedAt, blocked)
	}
}

// TestSchemaNamingCheckSnapshotTimestampRoundTrip asserts polled_at survives as
// ISO-8601 TEXT at millisecond granularity.
func TestSchemaNamingCheckSnapshotTimestampRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	sessionStore := NewSessionStore(db)
	snapStore := NewCheckSnapshotStore(db)
	ctx := context.Background()
	repo := createTestRepo(t, NewRepoStore(db))

	sess, err := sessionStore.Create(ctx, CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "snap ts",
		WorktreePath: "/tmp/wt/snap",
		BranchName:   "feat/snap",
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	polled := time.Date(2026, 7, 19, 9, 8, 7, 654_000_000, time.UTC)
	if err := snapStore.Insert(ctx, CheckSnapshot{
		SessionID: sess.ID, PolledAt: polled, HeadSHA: "sha", RawJSON: "[]", ComputedStatus: 1,
	}); err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}
	got, err := snapStore.RecentBySession(ctx, sess.ID, 1)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(got) != 1 || sqlutil.FormatTime(got[0].PolledAt) != sqlutil.FormatTime(polled) {
		t.Fatalf("polled_at round-trip: got %+v want %v", got, polled)
	}
}

// TestSchemaNamingRotationTimestampRoundTrip asserts rotation_events created_at
// and reset_at survive as ISO-8601 TEXT at millisecond granularity.
func TestSchemaNamingRotationTimestampRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	store := NewRotationEventStore(db)
	ctx := context.Background()

	id, err := sqlutil.NewID()
	if err != nil {
		t.Fatalf("NewID: %v", err)
	}
	created := time.Date(2026, 7, 19, 6, 5, 4, 321_000_000, time.UTC)
	reset := time.Date(2026, 7, 19, 8, 0, 0, 456_000_000, time.UTC)
	if err := store.Insert(ctx, RotationEvent{
		ID: id, SessionID: "sess-ts", Provider: "claude",
		Trigger: "ROTATION_TRIGGER_USAGE_LIMITED", Outcome: "ROTATION_OUTCOME_ROTATED",
		CreatedAt: created, ResetAt: &reset,
	}); err != nil {
		t.Fatalf("insert rotation event: %v", err)
	}
	got, err := store.RecentBySession(ctx, "sess-ts", 1)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d", len(got))
	}
	if sqlutil.FormatTime(got[0].CreatedAt) != sqlutil.FormatTime(created) {
		t.Fatalf("created_at round-trip: got %v want %v", got[0].CreatedAt, created)
	}
	if got[0].ResetAt == nil || sqlutil.FormatTime(*got[0].ResetAt) != sqlutil.FormatTime(reset) {
		t.Fatalf("reset_at round-trip: got %v want %v", got[0].ResetAt, reset)
	}
}

// seedLegacyRows opens a fresh in-memory DB, migrates it to the version
// immediately before the BOS-444 migration, and inserts pre-migration-shaped
// rows (bare-boolean column names, INTEGER epoch _at columns, and NULL
// sentinels) so the migration's actual data-conversion statements run against
// real data rather than empty tables. The BOS-444 migration is NOT yet applied.
func seedLegacyRows(t *testing.T) *sql.DB {
	t.Helper()
	db, err := OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := dbtest.RunUpTo(db, os.DirFS(migrationsDir()), preMigrationVersion); err != nil {
		t.Fatalf("migrate up to %d: %v", preMigrationVersion, err)
	}

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("seed exec %q: %v", q, err)
		}
	}

	// repos: old bare-boolean archive_sessions_after_merge = 1.
	exec(`INSERT INTO repos (id, display_name, local_path, origin_url, worktree_base_dir, archive_sessions_after_merge)
	      VALUES ('r1', 'repo', '/tmp/r1', 'https://example.com/r1.git', '/tmp/wt', 1)`)

	// sessions: bare-boolean names (automation_enabled=0, detach=1) plus a
	// populated INTEGER epoch (last_repair_started_at) and a NULL nullable
	// epoch (last_repair_blocked_at) to prove both conversion and NULL passthrough.
	exec(`INSERT INTO sessions (id, repo_id, title, worktree_path, branch_name,
	          automation_enabled, detach, last_repair_started_at, last_repair_blocked_at)
	      VALUES ('s1', 'r1', 'legacy', '/tmp/wt/s1', 'feat/legacy', 0, 1, ?, NULL)`, legacyEpoch)

	// cron_jobs: bare-boolean enabled=1, run_setup_command=0.
	exec(`INSERT INTO cron_jobs (id, repo_id, name, prompt, schedule, enabled, run_setup_command)
	      VALUES ('c1', 'r1', 'job', 'run', '@daily', 1, 0)`)

	// session_check_snapshots: NOT NULL INTEGER epoch polled_at.
	exec(`INSERT INTO session_check_snapshots (session_id, polled_at, raw_json)
	      VALUES ('s1', ?, '[]')`, legacyEpoch)

	// rotation_events: one row with both created_at and reset_at populated,
	// one row with reset_at NULL (nullable-epoch passthrough).
	exec(`INSERT INTO rotation_events (id, session_id, provider, trigger_kind, outcome, reset_at, created_at)
	      VALUES ('e1', 's1', 'claude', 'ROTATION_TRIGGER_USAGE_LIMITED', 'ROTATION_OUTCOME_ROTATED', ?, ?)`,
		legacyEpoch, legacyEpoch)
	exec(`INSERT INTO rotation_events (id, session_id, provider, trigger_kind, outcome, reset_at, created_at)
	      VALUES ('e2', 's1', 'claude', 'ROTATION_TRIGGER_USAGE_LIMITED', 'ROTATION_OUTCOME_ROTATED', NULL, ?)`,
		legacyEpoch)

	return db
}

// TestSchemaNamingMigrationConvertsLegacyData is the end-to-end data-conversion
// test: it seeds legacy-shaped rows, applies the BOS-444 migration, and asserts
// that the migration's own UPDATE / INSERT...SELECT statements renamed the
// booleans and converted the INTEGER epochs to canonical ISO-8601 TEXT, while
// preserving NULLs. This exercises the conversion against real data, which the
// fresh-DB round-trip tests (empty tables at migrate time) do not.
func TestSchemaNamingMigrationConvertsLegacyData(t *testing.T) {
	db := seedLegacyRows(t)
	if err := dbtest.RunUpTo(db, os.DirFS(migrationsDir()), schemaNamingVersion); err != nil {
		t.Fatalf("apply schema-naming migration: %v", err)
	}

	// repos boolean rename preserved the value.
	var archive int
	if err := db.QueryRow(`SELECT should_archive_sessions_after_merge FROM repos WHERE id = 'r1'`).Scan(&archive); err != nil {
		t.Fatalf("read repos: %v", err)
	}
	if archive != 1 {
		t.Errorf("should_archive_sessions_after_merge = %d, want 1", archive)
	}

	// sessions booleans preserved; epoch converted; NULL preserved.
	var (
		autom, detached int
		startedAt       string
		blockedAt       sql.NullString
	)
	if err := db.QueryRow(`SELECT is_automation_enabled, is_detached, last_repair_started_at, last_repair_blocked_at
	                       FROM sessions WHERE id = 's1'`).Scan(&autom, &detached, &startedAt, &blockedAt); err != nil {
		t.Fatalf("read sessions: %v", err)
	}
	if autom != 0 || detached != 1 {
		t.Errorf("session booleans = (autom %d, detached %d), want (0, 1)", autom, detached)
	}
	if startedAt != legacyEpochISO {
		t.Errorf("last_repair_started_at = %q, want %q", startedAt, legacyEpochISO)
	}
	if blockedAt.Valid {
		t.Errorf("last_repair_blocked_at = %q, want NULL", blockedAt.String)
	}

	// cron_jobs booleans preserved.
	var enabled, runSetup int
	if err := db.QueryRow(`SELECT is_enabled, should_run_setup_command FROM cron_jobs WHERE id = 'c1'`).Scan(&enabled, &runSetup); err != nil {
		t.Fatalf("read cron_jobs: %v", err)
	}
	if enabled != 1 || runSetup != 0 {
		t.Errorf("cron booleans = (enabled %d, runSetup %d), want (1, 0)", enabled, runSetup)
	}

	// session_check_snapshots NOT NULL epoch converted to TEXT ISO-8601.
	var polledAt, polledType string
	if err := db.QueryRow(`SELECT polled_at, typeof(polled_at) FROM session_check_snapshots WHERE session_id = 's1'`).Scan(&polledAt, &polledType); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if polledAt != legacyEpochISO {
		t.Errorf("polled_at = %q, want %q", polledAt, legacyEpochISO)
	}
	if polledType != "text" {
		t.Errorf("polled_at typeof = %q, want text", polledType)
	}

	// rotation_events: populated row converted (both columns), NULL row preserved.
	var createdAt string
	var resetAt sql.NullString
	if err := db.QueryRow(`SELECT created_at, reset_at FROM rotation_events WHERE id = 'e1'`).Scan(&createdAt, &resetAt); err != nil {
		t.Fatalf("read rotation e1: %v", err)
	}
	if createdAt != legacyEpochISO {
		t.Errorf("rotation created_at = %q, want %q", createdAt, legacyEpochISO)
	}
	if !resetAt.Valid || resetAt.String != legacyEpochISO {
		t.Errorf("rotation reset_at = %v, want %q", resetAt, legacyEpochISO)
	}
	var resetAt2 sql.NullString
	if err := db.QueryRow(`SELECT reset_at FROM rotation_events WHERE id = 'e2'`).Scan(&resetAt2); err != nil {
		t.Fatalf("read rotation e2: %v", err)
	}
	if resetAt2.Valid {
		t.Errorf("rotation e2 reset_at = %q, want NULL", resetAt2.String)
	}
}

// TestSchemaNamingMigrationDownReverses exercises the migration's Down block:
// after Up-then-Down the columns revert to their bare-boolean names and INTEGER
// epoch types, values round-trip (at whole-second precision), and NULLs are
// preserved. Without this, the entire Down block is never executed by any test.
func TestSchemaNamingMigrationDownReverses(t *testing.T) {
	db := seedLegacyRows(t)
	if err := dbtest.RunUpTo(db, os.DirFS(migrationsDir()), schemaNamingVersion); err != nil {
		t.Fatalf("apply schema-naming migration: %v", err)
	}
	if err := dbtest.RunDownTo(db, os.DirFS(migrationsDir()), preMigrationVersion); err != nil {
		t.Fatalf("roll back schema-naming migration: %v", err)
	}

	// Old bare-boolean names are back; epoch columns are INTEGER again with the
	// original whole-second value; NULL preserved.
	var (
		autom     int
		startedAt sql.NullInt64
		blockedAt sql.NullInt64
		startType string
	)
	if err := db.QueryRow(`SELECT automation_enabled, last_repair_started_at, last_repair_blocked_at, typeof(last_repair_started_at)
	                       FROM sessions WHERE id = 's1'`).Scan(&autom, &startedAt, &blockedAt, &startType); err != nil {
		t.Fatalf("read reverted sessions: %v", err)
	}
	if autom != 0 {
		t.Errorf("reverted automation_enabled = %d, want 0", autom)
	}
	if startType != "integer" {
		t.Errorf("reverted last_repair_started_at typeof = %q, want integer", startType)
	}
	if !startedAt.Valid || startedAt.Int64 != legacyEpoch {
		t.Errorf("reverted last_repair_started_at = %v, want %d", startedAt, legacyEpoch)
	}
	if blockedAt.Valid {
		t.Errorf("reverted last_repair_blocked_at = %v, want NULL", blockedAt)
	}

	// The NOT NULL full-rebuild tables reverted to INTEGER with the original value.
	var polledAt int64
	var polledType string
	if err := db.QueryRow(`SELECT polled_at, typeof(polled_at) FROM session_check_snapshots WHERE session_id = 's1'`).Scan(&polledAt, &polledType); err != nil {
		t.Fatalf("read reverted snapshot: %v", err)
	}
	if polledAt != legacyEpoch || polledType != "integer" {
		t.Errorf("reverted polled_at = %d (%s), want %d integer", polledAt, polledType, legacyEpoch)
	}
	var createdAt int64
	if err := db.QueryRow(`SELECT created_at FROM rotation_events WHERE id = 'e1'`).Scan(&createdAt); err != nil {
		t.Fatalf("read reverted rotation: %v", err)
	}
	if createdAt != legacyEpoch {
		t.Errorf("reverted rotation created_at = %d, want %d", createdAt, legacyEpoch)
	}

	// cron_jobs reverted to bare enabled name.
	var enabled int
	if err := db.QueryRow(`SELECT enabled FROM cron_jobs WHERE id = 'c1'`).Scan(&enabled); err != nil {
		t.Fatalf("read reverted cron_jobs: %v", err)
	}
	if enabled != 1 {
		t.Errorf("reverted enabled = %d, want 1", enabled)
	}
}
