package server

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/session"
)

// fakeProxyTokenStore is an in-memory db.ProxyTokenStore that records what the
// proxy wrote through, so the registry tests need no database and no socket.
// It applies the same upsert semantics as the real store (same digest updates
// in place) — a fake that appended instead would hide exactly the drift the
// TokenForChat refresh test exists to catch.
type fakeProxyTokenStore struct {
	mu              sync.Mutex
	rows            map[string]db.ProxyTokenRecord
	upserts         int
	deletedSessions []string
	deletedAgents   []string
	upsertErr       error
	pruneCutoffs    []time.Time
	pruneErr        error
	gets            []string
	getErr          error
}

func newFakeProxyTokenStore() *fakeProxyTokenStore {
	return &fakeProxyTokenStore{rows: map[string]db.ProxyTokenRecord{}}
}

func (f *fakeProxyTokenStore) Upsert(_ context.Context, rec db.ProxyTokenRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upserts++
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.rows[rec.TokenSHA256] = rec
	return nil
}

func (f *fakeProxyTokenStore) List(context.Context) ([]db.ProxyTokenRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]db.ProxyTokenRecord, 0, len(f.rows))
	for _, rec := range f.rows {
		out = append(out, rec)
	}
	return out, nil
}

// GetByTokenHash mirrors the real store's primary-key read, including its
// (nil, nil) miss — the shape the unknown-token repair path branches on.
func (f *fakeProxyTokenStore) GetByTokenHash(_ context.Context, tokenSHA256 string) (*db.ProxyTokenRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	rec, ok := f.rows[tokenSHA256]
	if !ok {
		return nil, nil
	}
	f.gets = append(f.gets, tokenSHA256)
	out := rec
	return &out, nil
}

// gets returns a copy of the digests this store was asked to resolve. The
// unknown-token repair path reads the store from a goroutine the handler does
// not join (BOS-982), so the recording is only safe to read through the same
// mutex that guards the append.
func (f *fakeProxyTokenStore) getsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.gets...)
}

func (f *fakeProxyTokenStore) DeleteBySessionID(_ context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletedSessions = append(f.deletedSessions, sessionID)
	for hash, rec := range f.rows {
		if rec.SessionID == sessionID {
			delete(f.rows, hash)
		}
	}
	return nil
}

func (f *fakeProxyTokenStore) RebindResumedChat(_ context.Context, _ string, _ db.RebindResumedChatParams) error {
	return nil
}

func (f *fakeProxyTokenStore) DeleteByAgentSessionID(_ context.Context, agentSessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletedAgents = append(f.deletedAgents, agentSessionID)
	for hash, rec := range f.rows {
		if rec.AgentSessionID == agentSessionID {
			delete(f.rows, hash)
		}
	}
	return nil
}

// DeleteOlderThan applies the same age semantics as the real store so a test
// that swaps the fake in still exercises a bounded registry. Rows the fake was
// handed with a zero CreatedAt (the common case — the proxy stamps created_at in
// SQL, not in the record) are treated as fresh rather than infinitely old.
func (f *fakeProxyTokenStore) DeleteOlderThan(_ context.Context, cutoff time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruneCutoffs = append(f.pruneCutoffs, cutoff)
	if f.pruneErr != nil {
		return 0, f.pruneErr
	}
	var removed int64
	for hash, rec := range f.rows {
		if !rec.CreatedAt.IsZero() && rec.CreatedAt.Before(cutoff) {
			delete(f.rows, hash)
			removed++
		}
	}
	return removed, nil
}

func (f *fakeProxyTokenStore) snapshot() map[string]db.ProxyTokenRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]db.ProxyTokenRecord, len(f.rows))
	for hash, rec := range f.rows {
		out[hash] = rec
	}
	return out
}

func (f *fakeProxyTokenStore) upsertCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.upserts
}

var _ db.ProxyTokenStore = (*fakeProxyTokenStore)(nil)

// newPersistProxy builds an un-listened ProxyServer wired to a durable token
// store, mirroring newAdoptProxy: every registry mutation is an in-memory map
// write, so none of these tests needs a live socket.
func newPersistProxy(t *testing.T, store db.ProxyTokenStore, buf *bytes.Buffer) *ProxyServer {
	t.Helper()
	ps, err := NewProxyServer(ProxyServerConfig{Logger: zerolog.New(buf), ProxyTokens: store})
	if err != nil {
		t.Fatalf("NewProxyServer: %v", err)
	}
	return ps
}

// requireRow returns the persisted row for a token, failing if absent.
func requireRow(t *testing.T, store *fakeProxyTokenStore, token string) db.ProxyTokenRecord {
	t.Helper()
	rec, ok := store.snapshot()[proxyTokenHash(token)]
	if !ok {
		t.Fatalf("no persisted row for token %s…", tokenFingerprint(token))
	}
	return rec
}

// TestTokenForChat_existingTokenRefreshPersistsAccount guards the site a naive
// "persist on mint" reading misses. TokenForChat's existing-token branch does
// not mint — it REWRITES the live token's target to pick up a changed fallback
// account. If only the mint persisted, the stored row would keep the account
// the chat had at spawn, and a post-restart rebuild would resolve the pane to
// the wrong account while the in-memory map had the right one.
func TestTokenForChat_existingTokenRefreshPersistsAccount(t *testing.T) {
	store := newFakeProxyTokenStore()
	var buf bytes.Buffer
	ps := newPersistProxy(t, store, &buf)

	tok := ps.TokenForChat("sess-1", "agent-1", "acct-old")
	if tok == "" {
		t.Fatal("TokenForChat returned empty token")
	}
	again := ps.TokenForChat("sess-1", "agent-1", "acct-new")
	if again != tok {
		t.Fatalf("token changed on refresh: %s… -> %s…", tokenFingerprint(tok), tokenFingerprint(again))
	}

	rows := store.snapshot()
	if len(rows) != 1 {
		t.Fatalf("persisted rows = %d, want 1 (refresh must update in place)", len(rows))
	}
	rec := requireRow(t, store, tok)
	if rec.AccountID != "acct-new" {
		t.Errorf("persisted account_id = %q, want acct-new", rec.AccountID)
	}
	if !rec.IsChatShaped || rec.AgentSessionID != "agent-1" || rec.SessionID != "sess-1" {
		t.Errorf("persisted row = %+v, want chat-shaped sess-1/agent-1", rec)
	}

	// The refreshed row must reassemble to the target the live map holds, or
	// the rebuild resolves the pane differently from the running daemon.
	wantTarget := session.ProxyTargetForChat(rec.AgentSessionID, rec.AccountID)
	if got, _ := ps.sessionForToken(tok); got != wantTarget {
		t.Errorf("live target = %q, persisted target = %q", got, wantTarget)
	}
}

// TestTokenForSession_persistsSessionShapedRow covers the mint site: a bare
// session target, with the chat columns left empty.
func TestTokenForSession_persistsSessionShapedRow(t *testing.T) {
	store := newFakeProxyTokenStore()
	var buf bytes.Buffer
	ps := newPersistProxy(t, store, &buf)

	tok := ps.TokenForSession("sess-1")
	rec := requireRow(t, store, tok)
	if rec.SessionID != "sess-1" || rec.IsChatShaped || rec.AgentSessionID != "" || rec.AccountID != "" {
		t.Errorf("persisted row = %+v, want plain sess-1 target", rec)
	}

	// A second call returns the cached token without re-persisting: the target
	// cannot have changed, so a write there would be pure churn.
	if again := ps.TokenForSession("sess-1"); again != tok {
		t.Fatalf("token changed on second call")
	}
	if got := store.upsertCount(); got != 1 {
		t.Errorf("upserts = %d, want 1 (cached token must not re-persist)", got)
	}
}

// TestTokenForChat_mintPersistsChatShapedRow covers the chat mint site.
func TestTokenForChat_mintPersistsChatShapedRow(t *testing.T) {
	store := newFakeProxyTokenStore()
	var buf bytes.Buffer
	ps := newPersistProxy(t, store, &buf)

	tok := ps.TokenForChat("sess-1", "agent-1", "acct-1")
	rec := requireRow(t, store, tok)
	if !rec.IsChatShaped || rec.SessionID != "sess-1" || rec.AgentSessionID != "agent-1" || rec.AccountID != "acct-1" {
		t.Errorf("persisted row = %+v, want chat-shaped sess-1/agent-1/acct-1", rec)
	}
}

// TestAdoptToken_persistsAdoptedRows covers both adoption sites: a token
// reconstructed from a surviving pane must become durable, or the NEXT restart
// loses it again and the sweep has to succeed twice in a row.
func TestAdoptToken_persistsAdoptedRows(t *testing.T) {
	const sessTok = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const chatTok = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	store := newFakeProxyTokenStore()
	var buf bytes.Buffer
	ps := newPersistProxy(t, store, &buf)

	ps.AdoptToken(sessTok, "sess-1")
	ps.AdoptTokenForChat("sess-1", "agent-1", "acct-1", chatTok)

	rec := requireRow(t, store, sessTok)
	if rec.SessionID != "sess-1" || rec.IsChatShaped {
		t.Errorf("adopted session row = %+v, want plain sess-1", rec)
	}
	chatRec := requireRow(t, store, chatTok)
	if !chatRec.IsChatShaped || chatRec.AgentSessionID != "agent-1" || chatRec.AccountID != "acct-1" {
		t.Errorf("adopted chat row = %+v, want chat-shaped agent-1/acct-1", chatRec)
	}
}

// TestAdoptToken_skippedAdoptionPersistsNothing pins the write-through to the
// mutations that actually happened. Every adopt path has conflict arms that
// leave the maps untouched; persisting from those would let a losing adopter
// overwrite the winner's durable row and invert the precedence on restart.
func TestAdoptToken_skippedAdoptionPersistsNothing(t *testing.T) {
	const liveTok = "1111111111111111111111111111111111111111111111111111111111111111"
	const staleTok = "2222222222222222222222222222222222222222222222222222222222222222"

	t.Run("session adoption loses to a live spawn", func(t *testing.T) {
		store := newFakeProxyTokenStore()
		var buf bytes.Buffer
		ps := newPersistProxy(t, store, &buf)
		spawned := ps.TokenForSession("sess-1")

		ps.AdoptToken(staleTok, "sess-1")

		if _, ok := store.snapshot()[proxyTokenHash(staleTok)]; ok {
			t.Error("skipped adoption persisted a row")
		}
		if _, ok := store.snapshot()[proxyTokenHash(spawned)]; !ok {
			t.Error("the winning spawn's row was lost")
		}
	})

	t.Run("chat adoption loses to a live spawn", func(t *testing.T) {
		store := newFakeProxyTokenStore()
		var buf bytes.Buffer
		ps := newPersistProxy(t, store, &buf)
		ps.TokenForChat("sess-1", "agent-1", "acct-1")

		ps.AdoptTokenForChat("sess-1", "agent-1", "acct-2", staleTok)

		if _, ok := store.snapshot()[proxyTokenHash(staleTok)]; ok {
			t.Error("skipped chat adoption persisted a row")
		}
	})

	t.Run("token already registered to a different target", func(t *testing.T) {
		store := newFakeProxyTokenStore()
		var buf bytes.Buffer
		ps := newPersistProxy(t, store, &buf)
		ps.AdoptToken(liveTok, "sess-1")
		before := store.upsertCount()

		ps.AdoptToken(liveTok, "sess-2") // same token, different session

		if got := store.upsertCount(); got != before {
			t.Errorf("upserts = %d, want %d (conflicting adoption must not write)", got, before)
		}
		if rec := requireRow(t, store, liveTok); rec.SessionID != "sess-1" {
			t.Errorf("persisted session = %q, want the first registration sess-1", rec.SessionID)
		}
	})
}

// TestDeregister_removesSessionAndChatRows asserts the eviction arm: ending a
// session drops its own token row AND every chat row underneath it, so the
// table does not accumulate targets nothing can reach.
func TestDeregister_removesSessionAndChatRows(t *testing.T) {
	store := newFakeProxyTokenStore()
	var buf bytes.Buffer
	ps := newPersistProxy(t, store, &buf)

	ps.TokenForSession("sess-1")
	ps.TokenForChat("sess-1", "agent-1", "acct-1")
	ps.TokenForChat("sess-1", "agent-2", "acct-2")
	keep := ps.TokenForSession("sess-2")
	if len(store.snapshot()) != 4 {
		t.Fatalf("setup persisted %d rows, want 4", len(store.snapshot()))
	}

	ps.Deregister("sess-1")

	rows := store.snapshot()
	if len(rows) != 1 {
		t.Fatalf("rows after Deregister = %d, want 1", len(rows))
	}
	if _, ok := rows[proxyTokenHash(keep)]; !ok {
		t.Error("Deregister removed another session's row")
	}
}

// TestForgetBearers_removeNothingFromStore pins a deliberate asymmetry: a
// bearer drop clears the sticky swapped credential but the PATH TOKEN must
// survive it. The pane's baked URL still carries that token, so evicting the
// row here would 401 a live pane on the next restart — the exact failure this
// table exists to prevent.
func TestForgetBearers_removeNothingFromStore(t *testing.T) {
	store := newFakeProxyTokenStore()
	var buf bytes.Buffer
	ps := newPersistProxy(t, store, &buf)

	ps.TokenForSession("sess-1")
	ps.TokenForChat("sess-1", "agent-1", "acct-1")
	before := store.snapshot()

	ps.ForgetBearer("sess-1")
	ps.ForgetAllBearers()

	after := store.snapshot()
	if len(after) != len(before) {
		t.Fatalf("rows after bearer drops = %d, want %d unchanged", len(after), len(before))
	}
	if len(store.deletedSessions) != 0 || len(store.deletedAgents) != 0 {
		t.Errorf("bearer drops issued deletes: sessions=%v agents=%v", store.deletedSessions, store.deletedAgents)
	}
}

// TestPersistFailureStillMintsUsableToken encodes the priority: a proxy that
// refuses to hand out a token is worse than one whose token is not durable. A
// store error is logged and the mint proceeds.
func TestPersistFailureStillMintsUsableToken(t *testing.T) {
	store := newFakeProxyTokenStore()
	store.upsertErr = errors.New("disk on fire")
	var buf bytes.Buffer
	ps := newPersistProxy(t, store, &buf)

	tok := ps.TokenForSession("sess-1")
	if tok == "" {
		t.Fatal("mint failed when persistence failed")
	}
	if sid, ok := ps.sessionForToken(tok); !ok || sid != "sess-1" {
		t.Fatalf("sessionForToken = %q,%v want sess-1,true", sid, ok)
	}
	if !strings.Contains(buf.String(), "disk on fire") {
		t.Errorf("persistence failure was not logged: %s", buf.String())
	}
}

// TestPersistNilStoreIsNoOp keeps the feature optional: with no store wired the
// daemon behaves exactly as it did before, rather than panicking on mint.
func TestPersistNilStoreIsNoOp(t *testing.T) {
	var buf bytes.Buffer
	ps := newPersistProxy(t, nil, &buf)

	tok := ps.TokenForSession("sess-1")
	ps.TokenForChat("sess-1", "agent-1", "acct-1")
	ps.AdoptToken("3333333333333333333333333333333333333333333333333333333333333333", "sess-9")
	ps.Deregister("sess-1")

	if tok == "" {
		t.Fatal("mint failed with no store wired")
	}
	if buf.Len() != 0 {
		t.Fatalf("nil store logged: %s", buf.String())
	}
}

// TestPersistNeverStoresRawToken is asserted mechanically rather than by
// inspection: neither a persisted field nor a log line may contain the token's
// own bytes. hex(sha256(token)) is what is durable; the token is the secret.
func TestPersistNeverStoresRawToken(t *testing.T) {
	store := newFakeProxyTokenStore()
	store.upsertErr = errors.New("forced failure so the error log path is covered too")
	var buf bytes.Buffer
	ps := newPersistProxy(t, store, &buf)

	// Mint through every persisting path, then repeat with a working store.
	tokens := []string{ps.TokenForSession("sess-1"), ps.TokenForChat("sess-1", "agent-1", "acct-1")}
	store.upsertErr = nil
	ps2 := newPersistProxy(t, store, &buf)
	tokens = append(tokens, ps2.TokenForSession("sess-2"), ps2.TokenForChat("sess-2", "agent-2", "acct-2"))

	logged := buf.String()
	for _, tok := range tokens {
		if tok == "" {
			t.Fatal("empty token in fixture")
		}
		if strings.Contains(logged, tok) {
			t.Errorf("log contains a raw token (%s…)", tokenFingerprint(tok))
		}
		for hash, rec := range store.snapshot() {
			if hash == tok {
				t.Errorf("persisted key is the raw token (%s…)", tokenFingerprint(tok))
			}
			for _, field := range []string{rec.TokenSHA256, rec.SessionID, rec.AgentSessionID, rec.AccountID} {
				if strings.Contains(field, tok) {
					t.Errorf("persisted field %q contains a raw token", field)
				}
			}
		}
	}
}

// TestRetargetChatToken_movesTheLiveTargetAndTheRow covers the handoff that
// rekeys a chat row after its replacement process is already running: the
// process baked /s/<token> into its env against the PRIOR agent session id, so
// the target has to move under the token it already holds. A fresh mint would
// be unreachable, and leaving the target behind makes every later resolution
// miss the row entirely.
func TestRetargetChatToken_movesTheLiveTargetAndTheRow(t *testing.T) {
	store := newFakeProxyTokenStore()
	var buf bytes.Buffer
	ps := newPersistProxy(t, store, &buf)

	tok := ps.TokenForChat("sess-1", "agent-old", "acct-1")
	ps.RetargetChatToken("sess-1", "agent-old", "agent-new", "acct-1")

	// Same token: the running process can never learn a new one.
	if again := ps.TokenForChat("sess-1", "agent-new", "acct-1"); again != tok {
		t.Errorf("TokenForChat after retarget minted a new token %q, want the pane's own %q", again, tok)
	}
	wantTarget := session.ProxyTargetForChat("agent-new", "acct-1")
	if got, ok := ps.sessionForToken(tok); !ok || got != wantTarget {
		t.Errorf("live target = %q (ok=%v), want %q", got, ok, wantTarget)
	}
	// The durable row has to agree, or a restart rebuild resolves the pane to
	// an id no chat row answers to — the exact failure this closes.
	rec := requireRow(t, store, tok)
	if !rec.IsChatShaped || rec.SessionID != "sess-1" || rec.AgentSessionID != "agent-new" || rec.AccountID != "acct-1" {
		t.Errorf("persisted row = %+v, want chat-shaped sess-1/agent-new/acct-1", rec)
	}
}

// TestRetargetChatToken_neverMints pins the never-mint half of the contract: a
// chat with no registered token (the proxy is off, or the spawn took the
// session-scoped branch) must leave the registry untouched rather than inventing
// a registration for a process that holds no token at all.
func TestRetargetChatToken_neverMints(t *testing.T) {
	store := newFakeProxyTokenStore()
	var buf bytes.Buffer
	ps := newPersistProxy(t, store, &buf)

	ps.RetargetChatToken("sess-1", "agent-old", "agent-new", "acct-1")
	if got := store.upsertCount(); got != 0 {
		t.Errorf("upserts = %d, want 0 (an unregistered chat must not mint)", got)
	}
	if tok := ps.TokenForChat("sess-1", "agent-new", "acct-1"); tok == "" {
		t.Fatal("TokenForChat returned an empty token after a no-op retarget")
	}
}
