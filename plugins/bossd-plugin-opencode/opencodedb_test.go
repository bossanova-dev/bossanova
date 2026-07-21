package main

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

	_ "modernc.org/sqlite"
)

// fixtureDBPath is the vendored synthetic opencode.db under testdata/. It is
// built to the observed opencode schema (see opencodedb.go) and carries:
//   - ses_fixturealpha00000000aa: title "Fixture Alpha Session", dir
//     /tmp/bos435-fixture/alpha, messages ending assistant (LastTurnIsUser=false)
//   - ses_fixturebeta000000000bb: title "Fixture Beta Session", dir
//     /tmp/bos435-fixture/beta, messages ending user (LastTurnIsUser=true)
//   - ses_fixtureshared_old0000cc / ses_fixtureshared_new0000dd: both dir
//     /tmp/bos435-fixture/shared; the "new" row is newer (ResolveInteractive wins)
//   - ses_fixtureempty00000000ee: title "Empty Session", no messages
const fixtureDBPath = "testdata/opencode.db"

// fixtureStore returns a store pinned to the vendored fixture DB (no subprocess,
// no env/XDG probing).
func fixtureStore(t *testing.T) *opencodeStore {
	t.Helper()
	abs, err := filepath.Abs(fixtureDBPath)
	if err != nil {
		t.Fatalf("resolve fixture path: %v", err)
	}
	if !fileExists(abs) {
		t.Fatalf("fixture DB missing at %s — rebuild testdata/opencode.db", abs)
	}
	return &opencodeStore{
		logger:        zerolog.Nop(),
		resolvePathFn: func() string { return abs },
	}
}

func TestGetChatTitle(t *testing.T) {
	s := fixtureStore(t)
	tests := []struct {
		name      string
		sessionID string
		want      string
	}{
		{"known session", "ses_fixturealpha00000000aa", "Fixture Alpha Session"},
		{"other session", "ses_fixturebeta000000000bb", "Fixture Beta Session"},
		{"unknown session", "ses_doesnotexist", ""},
		{"empty id", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := s.GetChatTitle(tt.sessionID); got != tt.want {
				t.Errorf("GetChatTitle(%q) = %q, want %q", tt.sessionID, got, tt.want)
			}
		})
	}
}

func TestTranscriptExists(t *testing.T) {
	s := fixtureStore(t)
	tests := []struct {
		name      string
		sessionID string
		want      bool
	}{
		{"present", "ses_fixturealpha00000000aa", true},
		{"present empty-transcript session", "ses_fixtureempty00000000ee", true},
		{"absent", "ses_doesnotexist", false},
		{"empty id", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := s.TranscriptExists(tt.sessionID); got != tt.want {
				t.Errorf("TranscriptExists(%q) = %v, want %v", tt.sessionID, got, tt.want)
			}
		})
	}
}

func TestLastTurnIsUser(t *testing.T) {
	s := fixtureStore(t)
	tests := []struct {
		name      string
		sessionID string
		want      bool
	}{
		{"ends assistant", "ses_fixturealpha00000000aa", false},
		{"ends user", "ses_fixturebeta000000000bb", true},
		{"no messages", "ses_fixtureempty00000000ee", false},
		{"unknown session", "ses_doesnotexist", false},
		{"empty id", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := s.LastTurnIsUser(tt.sessionID); got != tt.want {
				t.Errorf("LastTurnIsUser(%q) = %v, want %v", tt.sessionID, got, tt.want)
			}
		})
	}
}

func TestResolveInteractiveSessionID(t *testing.T) {
	s := fixtureStore(t)
	tests := []struct {
		name      string
		workDir   string
		wantID    string
		wantFound bool
	}{
		{"unique dir", "/tmp/bos435-fixture/alpha", "ses_fixturealpha00000000aa", true},
		{"shared dir picks newest", "/tmp/bos435-fixture/shared", "ses_fixtureshared_new0000dd", true},
		{"no match", "/tmp/bos435-fixture/nonexistent", "", false},
		{"empty workdir", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotID, gotFound := s.ResolveInteractiveSessionID(tt.workDir)
			if gotID != tt.wantID || gotFound != tt.wantFound {
				t.Errorf("ResolveInteractiveSessionID(%q) = (%q, %v), want (%q, %v)",
					tt.workDir, gotID, gotFound, tt.wantID, tt.wantFound)
			}
		})
	}
}

// TestGracefulDegradation_MissingDB proves every reader degrades to its zero
// value (never panics, never errors to the caller) when the resolved DB file
// does not exist.
func TestGracefulDegradation_MissingDB(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.db")
	s := &opencodeStore{logger: zerolog.Nop(), resolvePathFn: func() string { return missing }}
	if got := s.GetChatTitle("ses_fixturealpha00000000aa"); got != "" {
		t.Errorf("GetChatTitle = %q, want empty on missing DB", got)
	}
	if s.TranscriptExists("ses_fixturealpha00000000aa") {
		t.Error("TranscriptExists = true, want false on missing DB")
	}
	if s.LastTurnIsUser("ses_fixturebeta000000000bb") {
		t.Error("LastTurnIsUser = true, want false on missing DB")
	}
	if _, found := s.ResolveInteractiveSessionID("/tmp/bos435-fixture/alpha"); found {
		t.Error("ResolveInteractiveSessionID found = true, want false on missing DB")
	}
}

// TestGracefulDegradation_UnresolvedPath proves a store that resolves to no path
// at all degrades cleanly.
func TestGracefulDegradation_UnresolvedPath(t *testing.T) {
	s := &opencodeStore{logger: zerolog.Nop(), resolvePathFn: func() string { return "" }}
	if got := s.GetChatTitle("ses_x"); got != "" {
		t.Errorf("GetChatTitle = %q, want empty", got)
	}
	if s.TranscriptExists("ses_x") {
		t.Error("TranscriptExists = true, want false")
	}
	if s.LastTurnIsUser("ses_x") {
		t.Error("LastTurnIsUser = true, want false")
	}
	if _, found := s.ResolveInteractiveSessionID("/tmp/x"); found {
		t.Error("ResolveInteractiveSessionID found = true, want false")
	}
}

// TestGracefulDegradation_MissingTable proves the readers tolerate a DB whose
// expected tables are absent (schema drift), returning zero values rather than
// surfacing "no such table" errors.
func TestGracefulDegradation_MissingTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty-schema.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open temp db: %v", err)
	}
	// A valid SQLite DB with an unrelated table — no session/message tables.
	if _, err := db.Exec("CREATE TABLE unrelated (x integer)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	_ = db.Close()

	s := &opencodeStore{logger: zerolog.Nop(), resolvePathFn: func() string { return path }}
	if got := s.GetChatTitle("ses_x"); got != "" {
		t.Errorf("GetChatTitle = %q, want empty on missing table", got)
	}
	if s.TranscriptExists("ses_x") {
		t.Error("TranscriptExists = true, want false on missing table")
	}
	if s.LastTurnIsUser("ses_x") {
		t.Error("LastTurnIsUser = true, want false on missing table")
	}
	if _, found := s.ResolveInteractiveSessionID("/tmp/x"); found {
		t.Error("ResolveInteractiveSessionID found = true, want false on missing table")
	}
}

// TestGracefulDegradation_MissingColumn proves the readers tolerate a session
// table that lacks an expected column (e.g. `title`), degrading rather than
// erroring.
func TestGracefulDegradation_MissingColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-column.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open temp db: %v", err)
	}
	// session table without a `title` column — GetChatTitle must degrade.
	if _, err := db.Exec("CREATE TABLE session (id text primary key, directory text)"); err != nil {
		t.Fatalf("create session table: %v", err)
	}
	if _, err := db.Exec("INSERT INTO session (id, directory) VALUES ('ses_x', '/tmp/x')"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	_ = db.Close()

	s := &opencodeStore{logger: zerolog.Nop(), resolvePathFn: func() string { return path }}
	if got := s.GetChatTitle("ses_x"); got != "" {
		t.Errorf("GetChatTitle = %q, want empty on missing column", got)
	}
	// TranscriptExists doesn't read title, so it still works against this table.
	if !s.TranscriptExists("ses_x") {
		t.Error("TranscriptExists = false, want true (row present even without title column)")
	}
	// LastTurnIsUser reads the message table, which is absent here → degrade.
	if s.LastTurnIsUser("ses_x") {
		t.Error("LastTurnIsUser = true, want false (no message table)")
	}
}

// TestOpenDB_ReadsLiveWALCommit proves the reader sees a session/message
// committed to a live WAL writer but NOT yet checkpointed into the main DB file.
// This guards against regressing openDB back to an immutable-only open, which
// reads only the main file and would miss (or 500 "no such table" on) fresh
// uncheckpointed state — the exact defect the cross-model review flagged.
func TestOpenDB_ReadsLiveWALCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live-wal.db")
	// Writer in WAL mode, single connection kept OPEN for the whole test so the
	// -wal/-shm sidecars persist and nothing auto-checkpoints on close.
	w, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	defer func() { _ = w.Close() }()
	w.SetMaxOpenConns(1)
	if _, err := w.Exec("CREATE TABLE session (id text primary key, project_id text, directory text, title text, time_created integer, time_updated integer)"); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := w.Exec("CREATE TABLE message (id text primary key, session_id text, time_created integer, time_updated integer, data text)"); err != nil {
		t.Fatalf("create message: %v", err)
	}
	var jm string
	if err := w.QueryRow("PRAGMA journal_mode").Scan(&jm); err != nil || jm != "wal" {
		t.Fatalf("journal_mode = %q (err=%v), want wal", jm, err)
	}
	if _, err := w.Exec("INSERT INTO session (id, project_id, directory, title, time_created, time_updated) VALUES ('ses_live', 'p', '/tmp/live', 'Live WAL Title', 1, 1)"); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if _, err := w.Exec(`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES ('m1', 'ses_live', 1, 1, '{"role":"user"}')`); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	// Deliberately NO wal_checkpoint: the rows live only in the -wal right now.

	s := &opencodeStore{logger: zerolog.Nop(), resolvePathFn: func() string { return path }}
	if got := s.GetChatTitle("ses_live"); got != "Live WAL Title" {
		t.Errorf("GetChatTitle = %q, want %q (uncheckpointed WAL commit must be visible)", got, "Live WAL Title")
	}
	if !s.TranscriptExists("ses_live") {
		t.Error("TranscriptExists = false, want true (uncheckpointed WAL row must be visible)")
	}
	if !s.LastTurnIsUser("ses_live") {
		t.Error("LastTurnIsUser = false, want true (uncheckpointed WAL message must be visible)")
	}
	if id, found := s.ResolveInteractiveSessionID("/tmp/live"); !found || id != "ses_live" {
		t.Errorf("ResolveInteractiveSessionID = (%q, %v), want (ses_live, true)", id, found)
	}
}

// TestFileExists covers the file/dir/absent cases of the path guard.
func TestFileExists(t *testing.T) {
	dir := t.TempDir()
	if fileExists(dir) {
		t.Error("fileExists(dir) = true, want false")
	}
	if fileExists(filepath.Join(dir, "nope")) {
		t.Error("fileExists(absent) = true, want false")
	}
	abs, _ := filepath.Abs(fixtureDBPath)
	if !fileExists(abs) {
		t.Errorf("fileExists(%s) = false, want true", abs)
	}
}

// TestSameWorkDir covers path normalization equality.
func TestSameWorkDir(t *testing.T) {
	if !sameWorkDir("/tmp/a/b", "/tmp/a/b") {
		t.Error("identical paths should match")
	}
	if !sameWorkDir("/tmp/a/b", "/tmp/a/../a/b") {
		t.Error("Clean-equal paths should match")
	}
	if sameWorkDir("/tmp/a/b", "/tmp/a/c") {
		t.Error("different paths should not match")
	}
}
