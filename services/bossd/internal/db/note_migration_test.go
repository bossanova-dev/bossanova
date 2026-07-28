package db

import (
	"os"
	"testing"

	"github.com/recurser/bossalib/migrate"
)

// notesVersion is the goose timestamp of the BOS-550 migration.
const notesVersion int64 = 20260726000002

// preNotesVersion is the migration immediately before BOS-550; rolling down to
// it exercises the notes Down step.
const preNotesVersion int64 = 20260726000001

// noteIndexes are the indexes the BOS-550 migration declares, each named for
// the access path it serves.
var noteIndexes = []string{
	"idx_notes_repo_created",
	"idx_notes_session",
	"idx_note_tags_tag",
}

// TestNotesMigrationSchema asserts the BOS-550 migration is applied at its
// expected goose version and creates notes + note_tags with the documented
// columns and canonical timestamp types.
func TestNotesMigrationSchema(t *testing.T) {
	db := setupTestDB(t)

	var applied int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM goose_db_version WHERE version_id = ?", notesVersion,
	).Scan(&applied); err != nil {
		t.Fatalf("query goose version: %v", err)
	}
	if applied == 0 {
		t.Fatalf("migration version %d not applied", notesVersion)
	}

	noteCols := tableColumns(t, db, "notes")
	if len(noteCols) == 0 {
		t.Fatal("notes table missing after migration")
	}
	wantNotes := map[string]string{
		"id":         "TEXT",
		"repo_id":    "TEXT",
		"session_id": "TEXT",
		"chat_id":    "TEXT",
		"body":       "TEXT",
		"created_at": "TEXT",
		"updated_at": "TEXT",
	}
	for col, typ := range wantNotes {
		got, ok := noteCols[col]
		if !ok {
			t.Errorf("notes.%s missing", col)
			continue
		}
		if got != typ {
			t.Errorf("notes.%s type = %q, want %q", col, got, typ)
		}
	}
	for col := range noteCols {
		if _, ok := wantNotes[col]; !ok {
			t.Errorf("notes has unexpected column %q", col)
		}
	}

	tagCols := tableColumns(t, db, "note_tags")
	if len(tagCols) == 0 {
		t.Fatal("note_tags table missing after migration")
	}
	wantTags := map[string]string{"note_id": "TEXT", "tag": "TEXT"}
	for col, typ := range wantTags {
		got, ok := tagCols[col]
		if !ok {
			t.Errorf("note_tags.%s missing", col)
			continue
		}
		if got != typ {
			t.Errorf("note_tags.%s type = %q, want %q", col, got, typ)
		}
	}
	for col := range tagCols {
		if _, ok := wantTags[col]; !ok {
			t.Errorf("note_tags has unexpected column %q", col)
		}
	}
}

// TestNotesMigrationNullability asserts the ownership decision at the schema
// level: repo_id and body are required while session_id and chat_id are
// nullable provenance, and the timestamps are non-null.
func TestNotesMigrationNullability(t *testing.T) {
	db := setupTestDB(t)

	cols := broadcastTableInfo(t, db, "notes")
	want := map[string]broadcastColumn{
		// A non-INTEGER PRIMARY KEY reports notnull=0 in SQLite unless it also
		// carries an explicit NOT NULL, so id is asserted via pk.
		"id":         {declType: "TEXT", notNull: false, pk: true},
		"repo_id":    {declType: "TEXT", notNull: true},
		"session_id": {declType: "TEXT", notNull: false},
		"chat_id":    {declType: "TEXT", notNull: false},
		"body":       {declType: "TEXT", notNull: true},
		"created_at": {declType: "TEXT", notNull: true},
		"updated_at": {declType: "TEXT", notNull: true},
	}
	assertColumns(t, cols, "notes", want)

	tagCols := broadcastTableInfo(t, db, "note_tags")
	assertColumns(t, tagCols, "note_tags", map[string]broadcastColumn{
		"note_id": {declType: "TEXT", notNull: true, pk: true},
		"tag":     {declType: "TEXT", notNull: true, pk: true},
	})
}

// TestNotesMigrationIndexes asserts every declared index exists after Up.
func TestNotesMigrationIndexes(t *testing.T) {
	db := setupTestDB(t)

	present := indexNames(t, db, "notes")
	for name, ok := range indexNames(t, db, "note_tags") {
		present[name] = ok
	}
	for _, want := range noteIndexes {
		if !present[want] {
			t.Errorf("index %s missing after migration", want)
		}
	}
}

// TestNotesMigrationNoSessionForeignKey pins the load-bearing ownership
// decision at the schema level: notes declares NO foreign key, so a session
// delete can never cascade into it. note_tags, by contrast, must declare
// exactly one cascading FK onto notes.
func TestNotesMigrationNoSessionForeignKey(t *testing.T) {
	db := setupTestDB(t)

	// Close this result set BEFORE issuing the note_tags query below. The
	// in-memory pool is single-connection, so a *sql.Rows left open holds the
	// only connection and the next Query blocks until the test panics on its
	// timeout. database/sql auto-closes on a Next() that returns false, which is
	// exactly the passing case — so leaving it to the deferred Close would make
	// this guard deadlock precisely when it fires, replacing its diagnostic with
	// a two-minute timeout panic.
	hasFK, err := func() (bool, error) {
		rows, err := db.Query("PRAGMA foreign_key_list(notes)")
		if err != nil {
			return false, err
		}
		defer func() { _ = rows.Close() }()
		return rows.Next(), rows.Err()
	}()
	if err != nil {
		t.Fatalf("pragma foreign_key_list(notes): %v", err)
	}
	if hasFK {
		t.Error("notes declares a foreign key; session/repo/chat references must stay logical so notes outlive their session")
	}

	tagRows, err := db.Query("PRAGMA foreign_key_list(note_tags)")
	if err != nil {
		t.Fatalf("pragma foreign_key_list(note_tags): %v", err)
	}
	defer func() { _ = tagRows.Close() }()
	var fkCount int
	for tagRows.Next() {
		var id, seq int
		var table, from, to, onUpdate, onDelete, match string
		if err := tagRows.Scan(&id, &seq, &table, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			t.Fatalf("scan foreign_key_list: %v", err)
		}
		fkCount++
		if table != "notes" || from != "note_id" {
			t.Errorf("note_tags FK = %s(%s), want notes(note_id)", table, from)
		}
		if onDelete != "CASCADE" {
			t.Errorf("note_tags FK on delete = %q, want CASCADE", onDelete)
		}
	}
	if err := tagRows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if fkCount != 1 {
		t.Errorf("note_tags foreign keys = %d, want 1", fkCount)
	}
}

// TestNotesMigrationDown asserts the Down step removes both tables (and with
// them their indexes) so the migration is reversible.
func TestNotesMigrationDown(t *testing.T) {
	db := setupTestDB(t)
	if err := migrate.RunDownTo(db, os.DirFS(migrationsDir()), preNotesVersion); err != nil {
		t.Fatalf("run down: %v", err)
	}
	if cols := tableColumns(t, db, "notes"); len(cols) != 0 {
		t.Errorf("notes table should be gone after down, got columns: %v", cols)
	}
	if cols := tableColumns(t, db, "note_tags"); len(cols) != 0 {
		t.Errorf("note_tags table should be gone after down, got columns: %v", cols)
	}
	var remaining int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index'
		   AND (name LIKE 'idx_notes_%' OR name LIKE 'idx_note_tags_%')`,
	).Scan(&remaining); err != nil {
		t.Fatalf("count remaining indexes: %v", err)
	}
	if remaining != 0 {
		t.Errorf("note indexes remaining after down = %d, want 0", remaining)
	}
}
