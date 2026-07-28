package db

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/recurser/bossalib/migrate"
)

// broadcastsVersion is the goose timestamp of the BOS-554 migration.
const broadcastsVersion int64 = 20260726000001

// preBroadcastsVersion is the migration immediately before BOS-554; rolling
// down to it exercises the broadcasts Down step.
const preBroadcastsVersion int64 = 20260726000000

// broadcastIndexes are the indexes the BOS-554 migration declares, each named
// for the access path it serves. The first four are the plan's declared set;
// idx_broadcasts_origin_chat backs ListBroadcastsFilter.OriginChatID, which
// otherwise full-scans.
var broadcastIndexes = []string{
	"idx_broadcast_deliveries_claimable",
	"idx_broadcast_deliveries_broadcast",
	"idx_broadcast_deliveries_chat",
	"idx_broadcasts_state_expires",
	"idx_broadcasts_origin_chat",
}

// broadcastColumn is the subset of PRAGMA table_info a schema assertion needs:
// the declared type plus whether the column is NOT NULL and part of the primary
// key. tableColumns (shared with the other migration tests) drops both flags,
// and this migration's contract is specifically that nullable _at columns stay
// nullable while created_at/updated_at/expires_at do not.
//
// Note the SQLite quirk the pk flag exists to cover: a non-INTEGER PRIMARY KEY
// column reports notnull=0 unless it also carries an explicit NOT NULL, so `id`
// is asserted via pk rather than notNull.
type broadcastColumn struct {
	declType string
	notNull  bool
	pk       bool
}

// broadcastTableInfo returns a name->{type,notnull,pk} map for a table via PRAGMA.
func broadcastTableInfo(t *testing.T, db *sql.DB, table string) map[string]broadcastColumn {
	t.Helper()
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatalf("pragma table_info(%s): %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	cols := map[string]broadcastColumn{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		cols[name] = broadcastColumn{declType: ctype, notNull: notnull == 1, pk: pk > 0}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return cols
}

// indexNames returns the set of index names currently declared on a table.
func indexNames(t *testing.T, db *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.Query(
		"SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = ?", table)
	if err != nil {
		t.Fatalf("query indexes for %s: %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	names := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan index name: %v", err)
		}
		names[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return names
}

// assertColumns compares a want map against the live table shape.
func assertColumns(t *testing.T, got map[string]broadcastColumn, table string, want map[string]broadcastColumn) {
	t.Helper()
	for col, wantCol := range want {
		gotCol, ok := got[col]
		if !ok {
			t.Errorf("%s.%s missing", table, col)
			continue
		}
		if gotCol.declType != wantCol.declType {
			t.Errorf("%s.%s type = %q, want %q", table, col, gotCol.declType, wantCol.declType)
		}
		if gotCol.notNull != wantCol.notNull {
			t.Errorf("%s.%s notNull = %v, want %v", table, col, gotCol.notNull, wantCol.notNull)
		}
		if gotCol.pk != wantCol.pk {
			t.Errorf("%s.%s pk = %v, want %v", table, col, gotCol.pk, wantCol.pk)
		}
	}
}

// TestBroadcastsMigrationSchema asserts the BOS-554 migration creates both
// broadcast tables with the expected columns, canonical timestamp types, and
// nullability.
func TestBroadcastsMigrationSchema(t *testing.T) {
	db := setupTestDB(t)

	var applied int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM goose_db_version WHERE version_id = ?", broadcastsVersion,
	).Scan(&applied); err != nil {
		t.Fatalf("query goose version: %v", err)
	}
	if applied == 0 {
		t.Fatalf("migration version %d not applied", broadcastsVersion)
	}

	broadcasts := broadcastTableInfo(t, db, "broadcasts")
	if len(broadcasts) == 0 {
		t.Fatal("broadcasts table missing after migration")
	}
	assertColumns(t, broadcasts, "broadcasts", map[string]broadcastColumn{
		"id":             {declType: "TEXT", pk: true},
		"origin_chat_id": {declType: "TEXT"},
		"selector":       {declType: "TEXT", notNull: true},
		"message":        {declType: "TEXT", notNull: true},
		"state":          {declType: "TEXT", notNull: true},
		"target_count":   {declType: "INTEGER", notNull: true},
		"expires_at":     {declType: "TEXT", notNull: true},
		"created_at":     {declType: "TEXT", notNull: true},
		"updated_at":     {declType: "TEXT", notNull: true},
	})

	deliveries := broadcastTableInfo(t, db, "broadcast_deliveries")
	if len(deliveries) == 0 {
		t.Fatal("broadcast_deliveries table missing after migration")
	}
	assertColumns(t, deliveries, "broadcast_deliveries", map[string]broadcastColumn{
		"id":                {declType: "TEXT", pk: true},
		"broadcast_id":      {declType: "TEXT", notNull: true},
		"target_chat_id":    {declType: "TEXT", notNull: true},
		"target_daemon_id":  {declType: "TEXT", notNull: true},
		"state":             {declType: "TEXT", notNull: true},
		"lease_owner":       {declType: "TEXT"},
		"lease_deadline_at": {declType: "TEXT"},
		"attempt_count":     {declType: "INTEGER", notNull: true},
		"next_attempt_at":   {declType: "TEXT"},
		"delivered_at":      {declType: "TEXT"},
		"last_error":        {declType: "TEXT"},
		"created_at":        {declType: "TEXT", notNull: true},
		"updated_at":        {declType: "TEXT", notNull: true},
	})
}

// TestBroadcastsMigrationIndexes asserts every declared index exists after Up.
// The four names are load-bearing: the worker's claim scan, per-broadcast
// listing, the per-chat history query, and the lazy expiry sweep.
func TestBroadcastsMigrationIndexes(t *testing.T) {
	db := setupTestDB(t)

	present := indexNames(t, db, "broadcasts")
	for name, ok := range indexNames(t, db, "broadcast_deliveries") {
		present[name] = ok
	}
	for _, want := range broadcastIndexes {
		if !present[want] {
			t.Errorf("index %s missing after migration", want)
		}
	}
}

// TestBroadcastsMigrationTimestampDefaults pins the half of the timestamp
// contract PRAGMA-type assertions cannot see: created_at/updated_at default via
// strftime in the canonical sqlutil.TimeLayout shape, and nullable _at columns
// carry no default at all. The store always supplies both non-null timestamps
// explicitly, so nothing else in the suite exercises the DEFAULT clauses — a
// typo there would only surface via a raw INSERT in a later child.
func TestBroadcastsMigrationTimestampDefaults(t *testing.T) {
	db := setupTestDB(t)

	defaultOf := func(table, col string) *string {
		t.Helper()
		var dflt *string
		if err := db.QueryRow(
			`SELECT dflt_value FROM pragma_table_info(?) WHERE name = ?`, table, col,
		).Scan(&dflt); err != nil {
			t.Fatalf("pragma_table_info(%s).%s: %v", table, col, err)
		}
		return dflt
	}

	const wantDefault = `strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`
	for _, table := range []string{"broadcasts", "broadcast_deliveries"} {
		for _, col := range []string{"created_at", "updated_at"} {
			got := defaultOf(table, col)
			if got == nil || !strings.Contains(*got, wantDefault) {
				t.Errorf("%s.%s default = %v, want it to contain %s", table, col, got, wantDefault)
			}
		}
	}

	// Nullable _at columns must stay bare TEXT: a strftime default would make an
	// unset lease deadline or backoff read as "now", silently changing the
	// claimability predicates.
	for _, col := range []string{"lease_deadline_at", "next_attempt_at", "delivered_at"} {
		if got := defaultOf("broadcast_deliveries", col); got != nil {
			t.Errorf("broadcast_deliveries.%s default = %q, want none", col, *got)
		}
	}
	// expires_at is caller-supplied: a strftime('now') default would mean
	// "already expired" on every row that forgot to set it.
	if got := defaultOf("broadcasts", "expires_at"); got != nil {
		t.Errorf("broadcasts.expires_at default = %q, want none", *got)
	}
}

// TestBroadcastsMigrationForeignKeys asserts the deliberate FK asymmetry:
// broadcast_id cascades from its owning broadcast, target_chat_id is a bare
// logical reference with no FK (a broadcast may outlive the chat it targeted).
func TestBroadcastsMigrationForeignKeys(t *testing.T) {
	db := setupTestDB(t)

	rows, err := db.Query("PRAGMA foreign_key_list(broadcast_deliveries)")
	if err != nil {
		t.Fatalf("pragma foreign_key_list: %v", err)
	}
	defer func() { _ = rows.Close() }()

	type fk struct{ table, from, to, onDelete string }
	var fks []fk
	for rows.Next() {
		var id, seq int
		var table, from, to, onUpdate, onDelete, match string
		if err := rows.Scan(&id, &seq, &table, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			t.Fatalf("scan foreign_key_list: %v", err)
		}
		fks = append(fks, fk{table: table, from: from, to: to, onDelete: onDelete})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	if len(fks) != 1 {
		t.Fatalf("broadcast_deliveries foreign keys = %+v, want exactly one (broadcast_id)", fks)
	}
	got := fks[0]
	if got.from != "broadcast_id" || got.table != "broadcasts" || got.to != "id" {
		t.Errorf("foreign key = %+v, want broadcast_id -> broadcasts(id)", got)
	}
	if got.onDelete != "CASCADE" {
		t.Errorf("foreign key ON DELETE = %q, want CASCADE", got.onDelete)
	}
}

// TestBroadcastsMigrationDown asserts the Down step removes both tables (and
// with them their indexes) so the migration is reversible.
//
// The ceiling is capped with RunUpTo(broadcastsVersion) rather than the shared
// fully-migrated helper: RunDownTo rolls back everything above its target, so an
// uncapped run would drag every migration that lands after BOS-554 through this
// test's Down leg and could go red in a PR that never touches broadcasts. Same
// reasoning as TestCronZeroOutputMigrationDown.
func TestBroadcastsMigrationDown(t *testing.T) {
	migrations := os.DirFS(migrationsDir())
	db, err := OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := migrate.RunUpTo(db, migrations, broadcastsVersion); err != nil {
		t.Fatalf("run up to %d: %v", broadcastsVersion, err)
	}
	if cols := tableColumns(t, db, "broadcasts"); len(cols) == 0 {
		t.Fatal("broadcasts missing before down")
	}

	if err := migrate.RunDownTo(db, migrations, preBroadcastsVersion); err != nil {
		t.Fatalf("run down: %v", err)
	}
	for _, table := range []string{"broadcasts", "broadcast_deliveries"} {
		if cols := tableColumns(t, db, table); len(cols) != 0 {
			t.Errorf("%s table should be gone after down, got columns: %v", table, cols)
		}
	}

	var remaining int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name LIKE 'idx_broadcast%'`,
	).Scan(&remaining); err != nil {
		t.Fatalf("count remaining indexes: %v", err)
	}
	if remaining != 0 {
		t.Errorf("broadcast indexes remaining after down = %d, want 0", remaining)
	}

	// Re-applying must succeed: a Down that leaves state the Up cannot re-enter
	// is not actually reversible.
	if err := migrate.RunUpTo(db, migrations, broadcastsVersion); err != nil {
		t.Fatalf("re-run up after down: %v", err)
	}
	if cols := tableColumns(t, db, "broadcasts"); len(cols) == 0 {
		t.Error("broadcasts missing after re-applying the migration")
	}
}
