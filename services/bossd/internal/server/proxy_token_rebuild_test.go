package server

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/sqlutil"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/dbtest"
	"github.com/recurser/bossd/internal/session"
)

// --- BOS-979: the durable path-token registry --------------------------------
//
// The incident these tests pin: bossd's failover proxy mints a path token and
// bakes http://127.0.0.1:44127/s/<token> into a Claude tmux pane's environment
// at spawn. `tmux set-environment` cannot mutate an already-running process, so
// that URL is frozen for the pane's lifetime and the daemon can NEVER reissue
// it. The registry that resolves it lived only in memory, so a daemon restart
// wiped it and every surviving pane's next request 401'd — a wedged REPL.

// spawnRegistrar is the exact seam session.Lifecycle mints through when it
// builds a pane's ANTHROPIC_BASE_URL (proxyBaseURL / proxyBaseURLForChat call
// these two methods on session.proxyTokenRegistrar). Minting through the
// interface rather than the concrete type is deliberate: it proves these tests
// exercise the real spawn-shaped call, not a test-only shortcut into the maps.
type spawnRegistrar interface {
	TokenForSession(sessionID string) string
	TokenForChat(sessionID, agentSessionID, fallbackAccountID string) string
}

var _ spawnRegistrar = (*ProxyServer)(nil)

// rebuildTestDB returns an in-memory SQLite database carrying bossd's schema.
func rebuildTestDB(t *testing.T) *sql.DB {
	t.Helper()
	return dbtest.New(t)
}

// rebuildTestSession creates the repo + session rows a proxy_tokens row needs:
// session_id carries a real FK with ON DELETE CASCADE, so a token cannot be
// persisted against an id that does not exist.
func rebuildTestSession(t *testing.T, sqlDB *sql.DB) string {
	t.Helper()
	ctx := context.Background()
	repo, err := db.NewRepoStore(sqlDB).Create(ctx, db.CreateRepoParams{
		DisplayName:       "bossanova",
		LocalPath:         "/tmp/repo",
		OriginURL:         "git@github.com:example/bossanova.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/wt",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	sess, err := db.NewSessionStore(sqlDB).Create(ctx, db.CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "bos-979",
		WorktreePath: "/tmp/wt/bos-979",
		BranchName:   "feat/bos-979",
		BaseBranch:   "main",
		AgentName:    "claude",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return sess.ID
}

// newRebuildProxy builds an UNLISTENED ProxyServer wired to store, logging into
// buf. Unlistened on purpose for the registry-level assertions: resolution is a
// pure map read and needs no socket.
func newRebuildProxy(t *testing.T, store db.ProxyTokenStore, buf *bytes.Buffer) *ProxyServer {
	t.Helper()
	ps, err := NewProxyServer(ProxyServerConfig{Logger: zerolog.New(buf), ProxyTokens: store})
	if err != nil {
		t.Fatalf("NewProxyServer: %v", err)
	}
	return ps
}

// TestProxyTokenRegistry_SurvivesDaemonRestart is the BOS-979 regression proof
// and reproduces the real incident end to end.
//
// Daemon A mints both token shapes through the spawn seam and is then DISCARDED
// with no teardown — exactly what a `kill -9` or a crash leaves behind. Daemon B
// is a genuinely fresh ProxyServer with empty maps; the only things that cross
// the restart boundary are the SQLite rows and the token strings the panes have
// already baked into their frozen environments.
//
// NO TMUX IS INVOLVED ANYWHERE in this test — no tmux client is constructed and
// the pane sweep is never called — so a pass cannot be credited to BOS-481's
// tmux-env reconstruction. On today's main, where the registry is memory-only,
// daemon B resolves neither token and this test fails at the first lookup.
func TestProxyTokenRegistry_SurvivesDaemonRestart(t *testing.T) {
	ctx := context.Background()
	sqlDB := rebuildTestDB(t)
	store := db.NewProxyTokenStore(sqlDB)
	sessionID := rebuildTestSession(t, sqlDB)

	const (
		agentSessionID = "agent-session-77"
		chatAccountID  = "acct-chat-77"
	)

	// --- daemon A: two panes spawn and bake their URLs ---
	var spawn spawnRegistrar = newRebuildProxy(t, store, &bytes.Buffer{})
	sessionToken := spawn.TokenForSession(sessionID)
	chatToken := spawn.TokenForChat(sessionID, agentSessionID, chatAccountID)
	if sessionToken == "" || chatToken == "" || sessionToken == chatToken {
		t.Fatalf("mint produced unusable tokens (empty or identical)")
	}
	wantSessionTarget := sessionID
	wantChatTarget := session.ProxyTargetForChat(agentSessionID, chatAccountID)

	// --- the restart: daemon A simply ceases to exist ---
	spawn = nil

	// --- daemon B: fresh process, empty maps ---
	var logB bytes.Buffer
	daemonB := newRebuildProxy(t, store, &logB)
	if got, ok := daemonB.sessionForToken(sessionToken); ok {
		t.Fatalf("fresh daemon resolved a token before the rebuild: %q", got)
	}
	if err := daemonB.RebuildTokenRegistry(ctx); err != nil {
		t.Fatalf("RebuildTokenRegistry: %v", err)
	}

	// AC1: the session-shaped token resolves identically on the new instance.
	got, ok := daemonB.sessionForToken(sessionToken)
	if !ok || got != wantSessionTarget {
		t.Fatalf("session token after restart = %q,%v want %q,true", got, ok, wantSessionTarget)
	}

	// AC3: the chat-shaped token round-trips BYTE-IDENTICALLY, NUL bytes and
	// all. The assembled target is never stored — the rebuild reassembles it
	// from the row's components through session.ProxyTargetForChat — so this is
	// the assertion that catches a rebuild producing a plausible but different
	// target, which would route the pane to the wrong account.
	got, ok = daemonB.sessionForToken(chatToken)
	if !ok || got != wantChatTarget {
		t.Fatalf("chat token after restart = %q,%v want %q,true", got, ok, wantChatTarget)
	}
	if strings.Count(got, "\x00") != 2 || !strings.HasPrefix(got, "chat\x00") {
		t.Fatalf("rebuilt chat target lost its NUL framing: %q", got)
	}
	if !strings.HasSuffix(got, "\x00"+chatAccountID) {
		t.Fatalf("rebuilt chat target lost its fallback account: %q", got)
	}
	// And it parses back into its components, which is what the request path
	// actually does with the resolved target.
	gotAgent, gotAcct, parsed := session.ParseProxyChatTarget(got)
	if !parsed || gotAgent != agentSessionID || gotAcct != chatAccountID {
		t.Fatalf("rebuilt chat target did not parse: agent=%q acct=%q ok=%v", gotAgent, gotAcct, parsed)
	}

	// A token nobody minted still 401s — the rebuild restores registrations, it
	// does not make the proxy permissive.
	if _, ok := daemonB.sessionForToken(strings.Repeat("f", 64)); ok {
		t.Fatalf("rebuild made an unminted token resolvable")
	}
}

// TestProxyTokenRegistry_PaneReconnectsAfterRestart drives the same incident
// through a LIVE socket: the surviving pane's frozen /s/<token> request gets a
// 200 from the restarted daemon instead of the unknown-token 401 that wedged the
// REPL, and resolves to the right session on the way through.
func TestProxyTokenRegistry_PaneReconnectsAfterRestart(t *testing.T) {
	ctx := context.Background()
	up, srv := newFakeUpstream(t)

	sqlDB := rebuildTestDB(t)
	store := db.NewProxyTokenStore(sqlDB)
	sessionID := rebuildTestSession(t, sqlDB)

	// Daemon A mints; the pane bakes the returned token and keeps it forever.
	daemonA := newRebuildProxy(t, store, &bytes.Buffer{})
	paneToken := daemonA.TokenForSession(sessionID)
	if paneToken == "" {
		t.Fatal("mint returned an empty token")
	}

	// Daemon B binds a fresh listener with an empty registry, then rebuilds —
	// the ordering Bootstrap enforces before Serve ever accepts a request.
	fB := &fakeFailover{currentBearer: "second-token"}
	daemonB, base := startProxyConfigured(t, fB, srv.URL, zerolog.Nop(), func(ps *ProxyServer) {
		ps.proxyTokens = store
	})
	if err := daemonB.RebuildTokenRegistry(ctx); err != nil {
		t.Fatalf("RebuildTokenRegistry: %v", err)
	}

	resp, err := httpDo(newSentinelReq(t, base, paneToken))
	if err != nil {
		t.Fatalf("pane request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "pong" {
		t.Fatalf("surviving pane did not reconnect: status=%d body=%q", resp.StatusCode, body)
	}
	if fB.currentBearerID != sessionID {
		t.Fatalf("rebuilt token resolved to the wrong bearer id: %q want %q", fB.currentBearerID, sessionID)
	}
	if len(up.all()) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(up.all()))
	}
}

// TestProxyTokenRegistry_NoRawTokenAtRest is AC2's mechanical assertion. It
// scans EVERY value of EVERY row of EVERY table in the database, plus the whole
// log stream produced by minting, rebuilding, resolving and rejecting, for the
// raw tokens this test minted. A future column, index, or log field that starts
// carrying the pre-image fails here rather than in a postmortem.
func TestProxyTokenRegistry_NoRawTokenAtRest(t *testing.T) {
	ctx := context.Background()
	sqlDB := rebuildTestDB(t)
	store := db.NewProxyTokenStore(sqlDB)
	sessionID := rebuildTestSession(t, sqlDB)

	var logs bytes.Buffer
	daemonA := newRebuildProxy(t, store, &logs)
	sessionToken := daemonA.TokenForSession(sessionID)
	chatToken := daemonA.TokenForChat(sessionID, "agent-session-88", "acct-88")
	secrets := []string{sessionToken, chatToken}

	daemonB := newRebuildProxy(t, store, &logs)
	if err := daemonB.RebuildTokenRegistry(ctx); err != nil {
		t.Fatalf("RebuildTokenRegistry: %v", err)
	}
	// Exercise the token-mentioning log paths too: an unknown-token rejection
	// and a conflicting adoption both emit a line about a specific token.
	daemonB.logUnknownToken(tokenFingerprint(sessionToken), http.MethodPost, "/v1/messages")
	daemonB.AdoptToken(sessionToken, "some-other-session")

	// Guard against a vacuous scan: the rows have to actually be there.
	recs, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("expected 2 durable rows to scan, got %d", len(recs))
	}

	// (a) nothing anywhere in the database holds a pre-image.
	dump := dumpAllTables(t, sqlDB)
	for i, secret := range secrets {
		if strings.Contains(dump, secret) {
			t.Fatalf("RAW proxy token %d reached the database; only hex(sha256(token)) may be stored", i)
		}
	}

	// (b) nothing in the log stream holds a pre-image, and a long prefix is not
	// a loophole either — tokenPrefix/digestFingerprint cap at 8 hex chars.
	logged := logs.String()
	if logged == "" {
		t.Fatal("no log output captured; the log scan would be vacuous")
	}
	for i, secret := range secrets {
		if strings.Contains(logged, secret) {
			t.Fatalf("RAW proxy token %d reached a log line", i)
		}
		if strings.Contains(logged, secret[:16]) {
			t.Fatalf("a log line leaked more of token %d than the 8-hex fingerprint cap", i)
		}
	}

	// (c) positive control: the digest IS what was stored, so (a) passing is
	// evidence of hashing rather than of an empty column.
	digest := proxyTokenHash(sessionToken)
	if len(digest) != 64 || digest == sessionToken {
		t.Fatalf("proxyTokenHash did not produce a distinct 64-char digest")
	}
	if !strings.Contains(dump, digest) {
		t.Fatalf("the session token's digest is not in the database; the scan proves nothing")
	}
}

// dumpAllTables serialises every value of every row of every user table into one
// string. Deliberately schema-agnostic: the point is to catch a column nobody
// thought to check.
func dumpAllTables(t *testing.T, sqlDB *sql.DB) string {
	t.Helper()
	ctx := context.Background()
	tableRows, err := sqlDB.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	var tables []string
	for tableRows.Next() {
		var name string
		if err := tableRows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		tables = append(tables, name)
	}
	_ = tableRows.Close()
	if len(tables) == 0 {
		t.Fatal("no tables found; the scan would be vacuous")
	}

	var out strings.Builder
	for _, table := range tables {
		// Table names come from sqlite_master, not from any input.
		rows, err := sqlDB.QueryContext(ctx, "SELECT * FROM "+table)
		if err != nil {
			t.Fatalf("select from %s: %v", table, err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("columns of %s: %v", table, err)
		}
		for rows.Next() {
			cells := make([]any, len(cols))
			for i := range cells {
				cells[i] = new(sql.RawBytes)
			}
			if err := rows.Scan(cells...); err != nil {
				t.Fatalf("scan %s: %v", table, err)
			}
			for _, cell := range cells {
				raw, ok := cell.(*sql.RawBytes)
				if !ok {
					t.Fatalf("unexpected cell type in %s", table)
				}
				fmt.Fprintf(&out, "%s|", *raw)
			}
			out.WriteString("\n")
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate %s: %v", table, err)
		}
		_ = rows.Close()
	}
	return out.String()
}

// TestProxyTokenRegistry_LiveRegistrationOutranksStoredRow proves the rebuild is
// not a blunt overwrite: anything registered before Bootstrap runs is kept and
// the stored row is skipped with a Warn. That precedence is what makes running
// the rebuild before the pane sweep safe rather than merely ordered.
func TestProxyTokenRegistry_LiveRegistrationOutranksStoredRow(t *testing.T) {
	ctx := context.Background()
	sqlDB := rebuildTestDB(t)
	store := db.NewProxyTokenStore(sqlDB)
	sessionID := rebuildTestSession(t, sqlDB)

	daemonA := newRebuildProxy(t, store, &bytes.Buffer{})
	token := daemonA.TokenForChat(sessionID, "agent-session-99", "acct-old")

	// Daemon B is built WITHOUT the store so the live registration below does
	// not write through and silently agree with the row it is meant to beat.
	var logs bytes.Buffer
	daemonB := newRebuildProxy(t, nil, &logs)
	daemonB.AdoptTokenForChat(sessionID, "agent-session-99", "acct-new", token)
	daemonB.proxyTokens = store

	if err := daemonB.RebuildTokenRegistry(ctx); err != nil {
		t.Fatalf("RebuildTokenRegistry: %v", err)
	}

	got, ok := daemonB.sessionForToken(token)
	want := session.ProxyTargetForChat("agent-session-99", "acct-new")
	if !ok || got != want {
		t.Fatalf("live registration lost to a stored row: got %q,%v want %q", got, ok, want)
	}
	if !strings.Contains(logs.String(), "keeping the live one") {
		t.Fatalf("expected a rebuild-conflict Warn, got: %s", logs.String())
	}
}

// TestProxyTokenRegistry_RebuiltEntryIsEvictable pins the reason the resolution
// index is keyed by digest AND filed under its session: after a rebuild this
// process has never seen the raw token, so a Deregister that walked the
// raw-token maps would leave every restored entry resolvable for the life of the
// daemon — a slow leak of authority for sessions that have already ended.
func TestProxyTokenRegistry_RebuiltEntryIsEvictable(t *testing.T) {
	ctx := context.Background()
	sqlDB := rebuildTestDB(t)
	store := db.NewProxyTokenStore(sqlDB)
	sessionID := rebuildTestSession(t, sqlDB)

	daemonA := newRebuildProxy(t, store, &bytes.Buffer{})
	sessionToken := daemonA.TokenForSession(sessionID)
	chatToken := daemonA.TokenForChat(sessionID, "agent-session-11", "acct-11")

	daemonB := newRebuildProxy(t, store, &bytes.Buffer{})
	if err := daemonB.RebuildTokenRegistry(ctx); err != nil {
		t.Fatalf("RebuildTokenRegistry: %v", err)
	}
	// Both must be live first, or the eviction assertion below proves nothing.
	for name, tok := range map[string]string{"session": sessionToken, "chat": chatToken} {
		if _, ok := daemonB.sessionForToken(tok); !ok {
			t.Fatalf("rebuild did not restore the %s token; the eviction check would be vacuous", name)
		}
	}

	daemonB.Deregister(sessionID)

	for name, tok := range map[string]string{"session": sessionToken, "chat": chatToken} {
		if _, ok := daemonB.sessionForToken(tok); ok {
			t.Fatalf("Deregister left the rebuilt %s token resolvable", name)
		}
	}
	// The durable rows go too, so the NEXT restart does not resurrect them.
	recs, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("Deregister left %d durable rows behind", len(recs))
	}
}

// TestProxyTokenRegistry_RebuildIsInertWithoutAStore proves the nil-store path
// is a clean no-op rather than an error, so a daemon built without the durable
// registry keeps the exact pre-BOS-979 behaviour.
func TestProxyTokenRegistry_RebuildIsInertWithoutAStore(t *testing.T) {
	ps := newRebuildProxy(t, nil, &bytes.Buffer{})
	if err := ps.RebuildTokenRegistry(context.Background()); err != nil {
		t.Fatalf("nil-store rebuild returned %v, want nil", err)
	}
	if len(ps.hashToTarget) != 0 {
		t.Fatalf("nil-store rebuild registered something: %v", ps.hashToTarget)
	}
}

// TestProxyTokenRegistry_PrunesExpiredRowsOnRebuild proves the durable registry
// is actually BOUNDED, not merely durable.
//
// Nothing else evicts a row for an idle session — Deregister has no production
// caller and archived_at is a soft delete — so without the age prune both the
// table and the registry rebuilt from it grow with every session ever spawned,
// and a token minted for a long-dead pane resolves forever. This asserts against
// the real migrated schema (not the fake store) because the cutoff comparison is
// done in SQL, on the canonical timestamp layout: a fake would not exercise it.
func TestProxyTokenRegistry_PrunesExpiredRowsOnRebuild(t *testing.T) {
	ctx := context.Background()
	sqlDB := rebuildTestDB(t)
	store := db.NewProxyTokenStore(sqlDB)
	sessionID := rebuildTestSession(t, sqlDB)

	// --- daemon A mints one token that will go stale and one that stays fresh ---
	daemonA := newRebuildProxy(t, store, &bytes.Buffer{})
	staleToken := daemonA.TokenForChat(sessionID, "agent-session-stale", "acct-stale")
	freshToken := daemonA.TokenForSession(sessionID)
	if staleToken == "" || freshToken == "" {
		t.Fatalf("mint produced unusable tokens")
	}

	// Age the stale row past the TTL by rewriting only its created_at, so the
	// row is otherwise byte-identical to the fresh one.
	staleHash := proxyTokenHash(staleToken)
	expired := time.Now().Add(-proxyTokenTTL - time.Hour)
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE proxy_tokens SET created_at = ? WHERE token_sha256 = ?`,
		sqlutil.FormatTime(expired), staleHash,
	); err != nil {
		t.Fatalf("backdate stale row: %v", err)
	}
	if rows, err := store.List(ctx); err != nil || len(rows) != 2 {
		t.Fatalf("precondition: List = %d rows, err %v; want 2 rows", len(rows), err)
	}

	// --- daemon B: fresh process, rebuilds from the durable rows ---
	daemonB := newRebuildProxy(t, store, &bytes.Buffer{})
	if err := daemonB.RebuildTokenRegistry(ctx); err != nil {
		t.Fatalf("RebuildTokenRegistry: %v", err)
	}

	// The expired token no longer resolves — the bound is real, not cosmetic.
	if target, ok := daemonB.sessionForToken(staleToken); ok {
		t.Fatalf("expired token still resolves after rebuild: %q", target)
	}
	// ...and it is gone from the table, so the registry cannot grow without end.
	rows, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List after rebuild: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("proxy_tokens after prune = %d rows, want 1", len(rows))
	}
	if rows[0].TokenSHA256 == staleHash {
		t.Fatalf("prune removed the wrong row: the expired one survived")
	}

	// The fresh token is untouched: pruning must not revoke a live pane.
	got, ok := daemonB.sessionForToken(freshToken)
	if !ok || got != sessionID {
		t.Fatalf("fresh token after prune = %q,%v want %q,true", got, ok, sessionID)
	}
}

// TestProxyTokenRegistry_PruneFailureDoesNotBlockRebuild pins the degradation
// contract: a prune is best-effort. A store that cannot delete must leave the
// daemon booting with a working (merely oversized) registry, exactly like the
// persist path's log-and-continue.
func TestProxyTokenRegistry_PruneFailureDoesNotBlockRebuild(t *testing.T) {
	ctx := context.Background()
	store := newFakeProxyTokenStore()
	store.pruneErr = errors.New("disk is on fire")

	daemonA := newPersistProxy(t, store, &bytes.Buffer{})
	token := daemonA.TokenForSession("sess-prune-fail")

	var logB bytes.Buffer
	daemonB := newPersistProxy(t, store, &logB)
	if err := daemonB.RebuildTokenRegistry(ctx); err != nil {
		t.Fatalf("prune failure must not fail the rebuild, got: %v", err)
	}
	if got, ok := daemonB.sessionForToken(token); !ok || got != "sess-prune-fail" {
		t.Fatalf("token after failed prune = %q,%v want resolvable", got, ok)
	}
	if !strings.Contains(logB.String(), "pruning expired path tokens failed") {
		t.Fatalf("prune failure was swallowed silently; log = %s", logB.String())
	}
}
