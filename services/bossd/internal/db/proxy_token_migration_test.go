package db

import (
	"context"
	"os"
	"testing"

	"github.com/recurser/bossd/internal/dbtest"
)

// proxyTokensVersion is the goose timestamp of the BOS-979 migration.
const proxyTokensVersion int64 = 20260824000000

// preProxyTokensVersion is the migration immediately before BOS-979; rolling
// down to it exercises the proxy_tokens Down step.
const preProxyTokensVersion int64 = 20260822000000

// proxyTokenIndexes are the indexes the BOS-979 migration declares, each named
// for the access path it serves.
var proxyTokenIndexes = []string{
	"idx_proxy_tokens_session_id",
	"idx_proxy_tokens_agent_session_id",
}

// TestProxyTokensMigrationSchema asserts the BOS-979 migration is applied at
// its expected goose version and creates proxy_tokens with exactly the
// documented columns — an unexpected column fails, so a later migration cannot
// widen the table without updating the contract here.
func TestProxyTokensMigrationSchema(t *testing.T) {
	db := setupTestDB(t)

	var applied int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM goose_db_version WHERE version_id = ?", proxyTokensVersion,
	).Scan(&applied); err != nil {
		t.Fatalf("query goose version: %v", err)
	}
	if applied == 0 {
		t.Fatalf("migration version %d not applied", proxyTokensVersion)
	}

	cols := tableColumns(t, db, "proxy_tokens")
	if len(cols) == 0 {
		t.Fatal("proxy_tokens table missing after migration")
	}
	want := map[string]string{
		"token_sha256":     "TEXT",
		"session_id":       "TEXT",
		"agent_session_id": "TEXT",
		"account_id":       "TEXT",
		"is_chat_shaped":   "INTEGER",
		"created_at":       "TEXT",
	}
	for col, typ := range want {
		got, ok := cols[col]
		if !ok {
			t.Errorf("proxy_tokens.%s missing", col)
			continue
		}
		if got != typ {
			t.Errorf("proxy_tokens.%s type = %q, want %q", col, got, typ)
		}
	}
	for col := range cols {
		if _, ok := want[col]; !ok {
			t.Errorf("proxy_tokens has unexpected column %q", col)
		}
	}
}

// TestProxyTokensMigrationNullability pins the ownership decision at the schema
// level: the digest and its owning session are required, the chat-only columns
// are nullable, and the shape flag and timestamp are not.
func TestProxyTokensMigrationNullability(t *testing.T) {
	db := setupTestDB(t)

	assertColumns(t, broadcastTableInfo(t, db, "proxy_tokens"), "proxy_tokens", map[string]broadcastColumn{
		// A non-INTEGER PRIMARY KEY reports notnull=0 in SQLite unless it also
		// carries an explicit NOT NULL, so token_sha256 is asserted via pk.
		"token_sha256":     {declType: "TEXT", notNull: false, pk: true},
		"session_id":       {declType: "TEXT", notNull: true},
		"agent_session_id": {declType: "TEXT", notNull: false},
		"account_id":       {declType: "TEXT", notNull: false},
		"is_chat_shaped":   {declType: "INTEGER", notNull: true},
		"created_at":       {declType: "TEXT", notNull: true},
	})
}

// TestProxyTokensMigrationIndexes asserts every declared index exists after Up.
func TestProxyTokensMigrationIndexes(t *testing.T) {
	db := setupTestDB(t)

	present := indexNames(t, db, "proxy_tokens")
	for _, want := range proxyTokenIndexes {
		if !present[want] {
			t.Errorf("index %s missing after migration", want)
		}
	}
}

// TestProxyTokensMigrationForeignKeys pins the load-bearing asymmetry: exactly
// ONE foreign key, sessions(id) ON DELETE CASCADE, and deliberately none on
// agent_session_id — agent_chats.agent_session_id is not unique, so SQLite
// cannot accept it as a parent key and the chat-only delete path must issue an
// explicit DELETE instead. A future migration that "helpfully" adds that FK
// would fail here rather than silently changing the eviction contract.
func TestProxyTokensMigrationForeignKeys(t *testing.T) {
	db := setupTestDB(t)

	rows, err := db.Query("PRAGMA foreign_key_list(proxy_tokens)")
	if err != nil {
		t.Fatalf("pragma foreign_key_list(proxy_tokens): %v", err)
	}
	defer func() { _ = rows.Close() }()
	var count int
	for rows.Next() {
		var id, seq int
		var table, from, to, onUpdate, onDelete, match string
		if err := rows.Scan(&id, &seq, &table, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			t.Fatalf("scan foreign_key_list: %v", err)
		}
		count++
		if table != "sessions" || from != "session_id" {
			t.Errorf("proxy_tokens FK = %s(%s), want sessions(session_id)", table, from)
		}
		if onDelete != "CASCADE" {
			t.Errorf("proxy_tokens FK on delete = %q, want CASCADE", onDelete)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if count != 1 {
		t.Errorf("proxy_tokens foreign keys = %d, want exactly 1 (sessions only)", count)
	}
}

// TestProxyTokensMigrationRejectsUnknownSession proves the FK is live on the
// real connection (PRAGMA foreign_keys=ON), not merely declared: a row naming a
// session that does not exist is refused.
func TestProxyTokensMigrationRejectsUnknownSession(t *testing.T) {
	db := setupTestDB(t)

	_, err := db.Exec(
		`INSERT INTO proxy_tokens (token_sha256, session_id, is_chat_shaped, created_at)
		 VALUES (?, ?, 0, '2026-08-24T00:00:00.000Z')`,
		"00000000000000000000000000000000000000000000000000000000deadbeef", "no-such-session",
	)
	if err == nil {
		t.Fatal("insert with unknown session_id succeeded; foreign key is not enforced")
	}
}

// TestProxyTokensMigrationDown asserts the Down step removes the table and
// leaves no orphaned index behind, so the migration is reversible.
func TestProxyTokensMigrationDown(t *testing.T) {
	db := dbtest.NewMigrated(t)
	if err := dbtest.RunDownTo(db, os.DirFS(migrationsDir()), preProxyTokensVersion); err != nil {
		t.Fatalf("run down: %v", err)
	}
	if cols := tableColumns(t, db, "proxy_tokens"); len(cols) != 0 {
		t.Errorf("proxy_tokens table should be gone after down, got columns: %v", cols)
	}
	var remaining int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name LIKE 'idx_proxy_tokens_%'`,
	).Scan(&remaining); err != nil {
		t.Fatalf("count remaining indexes: %v", err)
	}
	if remaining != 0 {
		t.Errorf("proxy_token indexes remaining after down = %d, want 0", remaining)
	}
}

// TestProxyTokensMigrationUpDownUp round-trips the migration, proving Up is
// replayable after a rollback rather than tripping over a leftover index.
func TestProxyTokensMigrationUpDownUp(t *testing.T) {
	db := dbtest.NewMigrated(t)
	dir := os.DirFS(migrationsDir())

	if err := dbtest.RunDownTo(db, dir, preProxyTokensVersion); err != nil {
		t.Fatalf("run down: %v", err)
	}
	if err := dbtest.RunUpTo(db, dir, proxyTokensVersion); err != nil {
		t.Fatalf("run up again: %v", err)
	}

	if cols := tableColumns(t, db, "proxy_tokens"); len(cols) == 0 {
		t.Fatal("proxy_tokens table missing after up/down/up")
	}
	present := indexNames(t, db, "proxy_tokens")
	for _, want := range proxyTokenIndexes {
		if !present[want] {
			t.Errorf("index %s missing after up/down/up", want)
		}
	}

	// The round trip must leave the table usable, not merely present.
	store := NewProxyTokenStore(db)
	sess := "session-round-trip"
	if _, err := db.Exec(
		`INSERT INTO repos (id, display_name, local_path, origin_url, worktree_base_dir)
		 VALUES ('repo-round-trip', 'repo', '/tmp/proxy-token-round-trip', 'https://example.com/proxy-token-round-trip.git', '/tmp')`,
	); err != nil {
		t.Fatalf("seed repo after up/down/up: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO sessions (id, repo_id, title, worktree_path, branch_name, base_branch)
		 VALUES (?, 'repo-round-trip', 'round trip', '/tmp/proxy-token-round-trip/session', 'feat/round-trip', 'main')`,
		sess,
	); err != nil {
		t.Fatalf("seed session after up/down/up: %v", err)
	}
	if err := store.Upsert(context.Background(), ProxyTokenRecord{
		TokenSHA256: hexDigest('7'),
		SessionID:   sess,
	}); err != nil {
		t.Fatalf("upsert after up/down/up: %v", err)
	}
}

// TestProxyTokensCascadeOnSessionDelete asserts AC5's session half against the
// real schema: two rows for one session (its own token plus a chat's) both
// vanish when the session row is deleted.
func TestProxyTokensCascadeOnSessionDelete(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	store := NewProxyTokenStore(db)
	sess := createProxyTokenTestSession(t, db, "cascade")

	if err := store.Upsert(ctx, ProxyTokenRecord{TokenSHA256: hexDigest('1'), SessionID: sess}); err != nil {
		t.Fatalf("upsert session token: %v", err)
	}
	if err := store.Upsert(ctx, ProxyTokenRecord{
		TokenSHA256: hexDigest('2'), SessionID: sess,
		AgentSessionID: "agent-1", AccountID: "acct-1", IsChatShaped: true,
	}); err != nil {
		t.Fatalf("upsert chat token: %v", err)
	}

	if _, err := db.Exec(`DELETE FROM sessions WHERE id = ?`, sess); err != nil {
		t.Fatalf("delete session: %v", err)
	}

	got, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("proxy_tokens after session delete = %d rows, want 0 (cascade)", len(got))
	}
}
