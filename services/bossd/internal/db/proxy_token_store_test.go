package db

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// hexDigest builds a 64-char stand-in for hex(sha256(token)) out of one
// repeated hex digit. Tests must never construct a raw proxy token and hash it
// here: the store's contract is that only digests reach it, and a fixture that
// carried the pre-image would be the first place that contract eroded.
func hexDigest(c byte) string {
	return strings.Repeat(string(c), 64)
}

// createProxyTokenTestSessions creates ONE repo and n sessions under it,
// returning their ids. One repo per database, deliberately: createTestRepo
// always uses the same origin URL, and a second call fails the canonical-origin
// uniqueness check rather than returning a second repo.
func createProxyTokenTestSessions(t *testing.T, db *sql.DB, titles ...string) []string {
	t.Helper()
	repo := createTestRepo(t, NewRepoStore(db))
	store := NewSessionStore(db)
	ids := make([]string, 0, len(titles))
	for _, title := range titles {
		sess, err := store.Create(context.Background(), CreateSessionParams{
			RepoID:       repo.ID,
			Title:        title,
			WorktreePath: "/tmp/wt/" + title,
			BranchName:   "feat/" + title,
			BaseBranch:   "main",
		})
		if err != nil {
			t.Fatalf("create session %q: %v", title, err)
		}
		ids = append(ids, sess.ID)
	}
	return ids
}

// createProxyTokenTestSession is the single-session case.
func createProxyTokenTestSession(t *testing.T, db *sql.DB, title string) string {
	t.Helper()
	return createProxyTokenTestSessions(t, db, title)[0]
}

// TestProxyTokenStore_RoundTripsBothShapes covers AC3's storage half: a
// chat-shaped row keeps its agent session and account, and a session-shaped row
// keeps both of those columns NULL rather than storing an empty string that
// would later read back as a real (empty) agent session.
func TestProxyTokenStore_RoundTripsBothShapes(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	store := NewProxyTokenStore(db)
	sess := createProxyTokenTestSession(t, db, "shapes")

	sessionRow := ProxyTokenRecord{TokenSHA256: hexDigest('a'), SessionID: sess}
	chatRow := ProxyTokenRecord{
		TokenSHA256:    hexDigest('b'),
		SessionID:      sess,
		AgentSessionID: "agent-abc",
		AccountID:      "acct-9",
		IsChatShaped:   true,
	}
	for _, rec := range []ProxyTokenRecord{sessionRow, chatRow} {
		if err := store.Upsert(ctx, rec); err != nil {
			t.Fatalf("upsert %s: %v", rec.TokenSHA256[:8], err)
		}
	}

	got, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("list = %d rows, want 2", len(got))
	}
	byHash := map[string]ProxyTokenRecord{}
	for _, rec := range got {
		byHash[rec.TokenSHA256] = rec
	}

	gotSession := byHash[sessionRow.TokenSHA256]
	if gotSession.SessionID != sess || gotSession.IsChatShaped {
		t.Errorf("session row = %+v, want session %q and is_chat_shaped false", gotSession, sess)
	}
	if gotSession.AgentSessionID != "" || gotSession.AccountID != "" {
		t.Errorf("session row carries chat columns: %+v", gotSession)
	}
	if gotSession.CreatedAt.IsZero() {
		t.Error("session row created_at did not round-trip")
	}

	gotChat := byHash[chatRow.TokenSHA256]
	if gotChat.AgentSessionID != "agent-abc" || gotChat.AccountID != "acct-9" || !gotChat.IsChatShaped {
		t.Errorf("chat row = %+v, want agent-abc/acct-9/chat-shaped", gotChat)
	}

	// The NULL-vs-empty distinction is a schema property, so assert it against
	// the columns rather than through the Go zero value that maps both ways.
	var agentNulls, accountNulls int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM proxy_tokens WHERE token_sha256 = ? AND agent_session_id IS NULL`,
		sessionRow.TokenSHA256,
	).Scan(&agentNulls); err != nil {
		t.Fatalf("query agent null: %v", err)
	}
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM proxy_tokens WHERE token_sha256 = ? AND account_id IS NULL`,
		sessionRow.TokenSHA256,
	).Scan(&accountNulls); err != nil {
		t.Fatalf("query account null: %v", err)
	}
	if agentNulls != 1 || accountNulls != 1 {
		t.Errorf("session-shaped row stored chat columns non-NULL (agent nulls=%d, account nulls=%d)", agentNulls, accountNulls)
	}
}

// TestProxyTokenStore_UpsertRefreshesAccountInPlace is the storage half of the
// site a naive "persist on mint" reading misses: TokenForChat's existing-token
// branch rewrites a LIVE token's target when the chat's fallback account
// changes. The same digest must update its row, never insert a second one.
func TestProxyTokenStore_UpsertRefreshesAccountInPlace(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	store := NewProxyTokenStore(db)
	sess := createProxyTokenTestSession(t, db, "refresh")

	rec := ProxyTokenRecord{
		TokenSHA256:    hexDigest('c'),
		SessionID:      sess,
		AgentSessionID: "agent-1",
		AccountID:      "acct-old",
		IsChatShaped:   true,
	}
	if err := store.Upsert(ctx, rec); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	rec.AccountID = "acct-new"
	if err := store.Upsert(ctx, rec); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}

	got, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("list = %d rows, want 1 (refresh must update, not insert)", len(got))
	}
	if got[0].AccountID != "acct-new" {
		t.Errorf("account_id = %q, want acct-new", got[0].AccountID)
	}
}

// TestProxyTokenStore_DeleteByAgentSessionID covers AC5's chat half: the
// eviction no cascade can express, because agent_chats.agent_session_id is not
// unique. Only the named chat's row goes.
func TestProxyTokenStore_DeleteByAgentSessionID(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	store := NewProxyTokenStore(db)
	sess := createProxyTokenTestSession(t, db, "chatdelete")

	rows := []ProxyTokenRecord{
		{TokenSHA256: hexDigest('d'), SessionID: sess},
		{TokenSHA256: hexDigest('e'), SessionID: sess, AgentSessionID: "agent-keep", IsChatShaped: true},
		{TokenSHA256: hexDigest('f'), SessionID: sess, AgentSessionID: "agent-drop", IsChatShaped: true},
	}
	for _, rec := range rows {
		if err := store.Upsert(ctx, rec); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	if err := store.DeleteByAgentSessionID(ctx, "agent-drop"); err != nil {
		t.Fatalf("delete by agent session: %v", err)
	}
	got, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("list = %d rows, want 2", len(got))
	}
	for _, rec := range got {
		if rec.AgentSessionID == "agent-drop" {
			t.Error("agent-drop row survived its chat delete")
		}
	}

	// Idempotent: a repeat delete, and a delete of an unknown chat, are no-ops.
	if err := store.DeleteByAgentSessionID(ctx, "agent-drop"); err != nil {
		t.Errorf("repeat delete: %v", err)
	}
	if err := store.DeleteByAgentSessionID(ctx, ""); err != nil {
		t.Errorf("empty delete: %v", err)
	}
}

// TestProxyTokenStore_DeleteBySessionID asserts a session's own token and all
// of its chats' tokens go together, and that another session is untouched.
func TestProxyTokenStore_DeleteBySessionID(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	store := NewProxyTokenStore(db)
	ids := createProxyTokenTestSessions(t, db, "dropme", "keepme")
	sess, other := ids[0], ids[1]

	for _, rec := range []ProxyTokenRecord{
		{TokenSHA256: hexDigest('1'), SessionID: sess},
		{TokenSHA256: hexDigest('2'), SessionID: sess, AgentSessionID: "agent-1", IsChatShaped: true},
		{TokenSHA256: hexDigest('3'), SessionID: other},
	} {
		if err := store.Upsert(ctx, rec); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	if err := store.DeleteBySessionID(ctx, sess); err != nil {
		t.Fatalf("delete by session: %v", err)
	}
	got, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].SessionID != other {
		t.Fatalf("list = %+v, want only the other session's row", got)
	}
	if err := store.DeleteBySessionID(ctx, ""); err != nil {
		t.Errorf("empty delete: %v", err)
	}
}

// TestProxyTokenStore_UpsertValidation rejects the shapes that would produce an
// unrebuildable row rather than persisting them and failing at boot.
func TestProxyTokenStore_UpsertValidation(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	store := NewProxyTokenStore(db)
	sess := createProxyTokenTestSession(t, db, "validation")

	cases := map[string]ProxyTokenRecord{
		"missing hash":          {SessionID: sess},
		"missing session":       {TokenSHA256: hexDigest('9')},
		"chat without agent id": {TokenSHA256: hexDigest('8'), SessionID: sess, IsChatShaped: true},
	}
	for name, rec := range cases {
		if err := store.Upsert(ctx, rec); err == nil {
			t.Errorf("%s: Upsert error = nil, want rejection", name)
		}
	}
}

// TestProxyTokenStore_ListEmpty is the boot-time rebuild's empty case: a fresh
// database must read as zero rows and no error, not as a failure the daemon
// then has to decide how to treat.
func TestProxyTokenStore_ListEmpty(t *testing.T) {
	got, err := NewProxyTokenStore(setupTestDB(t)).List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("list = %d rows, want 0", len(got))
	}
}

// TestProxyTokenStore_GetByTokenHash covers the primary-key read the proxy's
// unknown-token 401 branch uses to recover a rejected pane's identity (BOS-982).
// The miss must be (nil, nil), not an error: that branch is reachable by any
// local process presenting an arbitrary token, so "no such row" is the ordinary
// case and has to be cheap and quiet.
func TestProxyTokenStore_GetByTokenHash(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	store := NewProxyTokenStore(db)
	sess := createProxyTokenTestSession(t, db, "get-by-hash")

	chatRow := ProxyTokenRecord{
		TokenSHA256:    hexDigest('b'),
		SessionID:      sess,
		AgentSessionID: "agent-01",
		AccountID:      "acct-1",
		IsChatShaped:   true,
	}
	sessionRow := ProxyTokenRecord{TokenSHA256: hexDigest('c'), SessionID: sess}
	for _, rec := range []ProxyTokenRecord{chatRow, sessionRow} {
		if err := store.Upsert(ctx, rec); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}

	got, err := store.GetByTokenHash(ctx, chatRow.TokenSHA256)
	if err != nil {
		t.Fatalf("get chat row: %v", err)
	}
	if got == nil {
		t.Fatal("chat row not found by its digest")
	}
	if got.SessionID != sess || got.AgentSessionID != "agent-01" || got.AccountID != "acct-1" || !got.IsChatShaped {
		t.Fatalf("chat row = %+v, want the components it was written with", *got)
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("created_at did not round-trip")
	}

	// A session-shaped row reads back with its chat-only columns empty rather
	// than as a real (empty) agent session — the discriminator the repair path
	// branches on.
	gotSess, err := store.GetByTokenHash(ctx, sessionRow.TokenSHA256)
	if err != nil {
		t.Fatalf("get session row: %v", err)
	}
	if gotSess == nil || gotSess.IsChatShaped || gotSess.AgentSessionID != "" || gotSess.AccountID != "" {
		t.Fatalf("session row = %+v, want a chat-less row", gotSess)
	}

	for _, miss := range []string{hexDigest('d'), "", "not-a-digest"} {
		rec, err := store.GetByTokenHash(ctx, miss)
		if err != nil {
			t.Fatalf("miss %q returned an error: %v", miss, err)
		}
		if rec != nil {
			t.Fatalf("miss %q returned a row: %+v", miss, rec)
		}
	}
}
