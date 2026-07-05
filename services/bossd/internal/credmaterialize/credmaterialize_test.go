package credmaterialize

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// fakeStore is an in-memory CredentialStore with a SaveCredential call counter.
// It never touches a real keyring or the network.
type fakeStore struct {
	mu        sync.Mutex
	blob      []byte
	loadErr   error
	saveErr   error
	saveCalls int64
	lastSaved []byte
}

func (f *fakeStore) LoadCredential(_ context.Context, _ string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	return append([]byte(nil), f.blob...), nil
}

func (f *fakeStore) SaveCredential(_ context.Context, _ string, blob []byte) error {
	atomic.AddInt64(&f.saveCalls, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveErr != nil {
		return f.saveErr
	}
	f.lastSaved = append([]byte(nil), blob...)
	// A real store persists the saved blob, so a subsequent LoadCredential (as
	// persistBack does to pick its merge baseline) observes it.
	f.blob = append([]byte(nil), blob...)
	return nil
}

// setBlob overwrites the stored blob out-of-band, simulating another session
// persisting a refresh into the same account between our materialize and
// persist-back.
func (f *fakeStore) setBlob(b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blob = append([]byte(nil), b...)
}

func (f *fakeStore) saveCount() int64 { return atomic.LoadInt64(&f.saveCalls) }

func newTestMaterializer(t *testing.T, store CredentialStore) *Materializer {
	t.Helper()
	m, err := New(store, zerolog.Nop(), WithBaseDir(t.TempDir()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

const (
	fakeAccess  = "FAKE-ACCESS"
	fakeID      = "FAKE-ID-TOKEN"
	fakeRefresh = "FAKE-REFRESH"
	fakeAccount = "FAKE-ACCOUNT-ID"
)

func codexBlob(t *testing.T, access, id, refresh string) []byte {
	t.Helper()
	doc := map[string]any{
		"tokens": map[string]any{
			"access_token":  access,
			"id_token":      id,
			"refresh_token": refresh,
			"account_id":    fakeAccount,
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal codex blob: %v", err)
	}
	return raw
}

func TestMaterializeCodexRoundTrip(t *testing.T) {
	blob := codexBlob(t, fakeAccess, fakeID, fakeRefresh)
	store := &fakeStore{blob: blob}
	m := newTestMaterializer(t, store)

	got, _, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	if got.HomeDir == "" {
		t.Fatal("expected non-empty HomeDir")
	}
	if got.Env["CODEX_HOME"] != got.HomeDir {
		t.Fatalf("CODEX_HOME %q != HomeDir %q", got.Env["CODEX_HOME"], got.HomeDir)
	}
	if fi, statErr := os.Stat(got.HomeDir); statErr != nil || !fi.IsDir() {
		t.Fatalf("dir not created: %v", statErr)
	}
	onDisk, err := os.ReadFile(filepath.Join(got.HomeDir, authFileName))
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}
	if string(onDisk) != string(blob) {
		t.Fatalf("auth.json not byte-identical to blob")
	}
}

func TestMaterializeCodexNormalizesTopLevelShape(t *testing.T) {
	// Account-store top-level "{access,refresh,id_token}" shape, as validated by
	// AddAccount/TestAccount — not a codex auth.json.
	blob := []byte(`{"access":"FAKE-ACCESS","refresh":"FAKE-REFRESH","id_token":"FAKE-ID"}`)
	store := &fakeStore{blob: blob}
	m := newTestMaterializer(t, store)

	res, persist, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	onDisk, err := os.ReadFile(filepath.Join(res.HomeDir, authFileName))
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}
	var doc struct {
		Tokens struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			IDToken      string `json:"id_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(onDisk, &doc); err != nil {
		t.Fatalf("auth.json not valid JSON: %v", err)
	}
	if doc.Tokens.AccessToken != "FAKE-ACCESS" || doc.Tokens.RefreshToken != "FAKE-REFRESH" || doc.Tokens.IDToken != "FAKE-ID" {
		t.Fatalf("tokens not mirrored from top-level shape: %+v", doc.Tokens)
	}
	// The recorded hash must cover the written (normalized) bytes, so an
	// unmodified auth.json persists nothing.
	if err := persist(context.Background()); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if got := store.saveCount(); got != 0 {
		t.Fatalf("SaveCredential called %d times; want 0 (unchanged gate)", got)
	}
}

func TestMaterializeCodexFillsEmptyTokens(t *testing.T) {
	// Validated blob: real secrets top-level, but an empty tokens object that the
	// account validator still accepts. The first spawn must get filled tokens.
	blob := []byte(`{"access":"FAKE-ACCESS","refresh":"FAKE-REFRESH","id_token":"FAKE-ID","tokens":{}}`)
	store := &fakeStore{blob: blob}
	m := newTestMaterializer(t, store)

	res, persist, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	onDisk, err := os.ReadFile(filepath.Join(res.HomeDir, authFileName))
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}
	var doc struct {
		Tokens struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			IDToken      string `json:"id_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(onDisk, &doc); err != nil {
		t.Fatalf("auth.json not valid JSON: %v", err)
	}
	if doc.Tokens.AccessToken != "FAKE-ACCESS" || doc.Tokens.RefreshToken != "FAKE-REFRESH" || doc.Tokens.IDToken != "FAKE-ID" {
		t.Fatalf("empty tokens not filled from top-level: %+v", doc.Tokens)
	}
	if err := persist(context.Background()); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if got := store.saveCount(); got != 0 {
		t.Fatalf("SaveCredential called %d times; want 0 (unchanged gate)", got)
	}
}

func TestMaterializeCodexFillsWithOpenaiScalar(t *testing.T) {
	// Valid top-level secrets plus a scalar "openai" the validator ignores. The
	// openai key must not short-circuit the top-level fill.
	blob := []byte(`{"access":"FAKE-ACCESS","refresh":"FAKE-REFRESH","id_token":"FAKE-ID","openai":""}`)
	assertMaterializedTokens(t, blob)
}

func TestMaterializeCodexRebuildsNonObjectTokens(t *testing.T) {
	// Valid top-level secrets plus a malformed non-object tokens value that codex
	// cannot read. It must be rebuilt from the top-level fields.
	blob := []byte(`{"access":"FAKE-ACCESS","refresh":"FAKE-REFRESH","id_token":"FAKE-ID","tokens":[1,2,3]}`)
	assertMaterializedTokens(t, blob)
}

func TestMaterializeCodexPromotesOpenaiOverUnusableTokens(t *testing.T) {
	// A valid opencode "openai" object alongside an unusable "tokens" value (here
	// an explicit null) and no top-level secrets. The unusable tokens must not
	// block promoting openai's credentials.
	blob := []byte(`{"openai":{"access_token":"FAKE-ACCESS","refresh_token":"FAKE-REFRESH","id_token":"FAKE-ID"},"tokens":null}`)
	assertMaterializedTokens(t, blob)
}

func TestMaterializeCodexPromotesOpenaiOverEmptyObjectTokens(t *testing.T) {
	// Same promotion, but "tokens" is an empty object rather than null. An empty
	// object carries no credentials, so openai must still win.
	blob := []byte(`{"openai":{"access_token":"FAKE-ACCESS","refresh_token":"FAKE-REFRESH","id_token":"FAKE-ID"},"tokens":{}}`)
	assertMaterializedTokens(t, blob)
}

func TestMaterializeCodexCompletesPartialTokensFromOpenai(t *testing.T) {
	// A usable but partial "tokens" (only access_token) alongside a full "openai"
	// object and no top-level secrets. The missing refresh_token/id_token must be
	// backfilled from openai; the present access_token is preserved.
	blob := []byte(`{"openai":{"access_token":"STALE","refresh_token":"FAKE-REFRESH","id_token":"FAKE-ID"},"tokens":{"access_token":"FAKE-ACCESS"}}`)
	assertMaterializedTokens(t, blob)
}

// assertMaterializedTokens materializes blob and asserts the written auth.json
// carries a codex tokens object with the standard fake secrets, and that an
// untouched auth.json persists nothing (the recorded hash covers the written
// bytes). Every caller shares the same expected secrets, so they are fixed here.
func assertMaterializedTokens(t *testing.T, blob []byte) {
	t.Helper()
	const (
		wantAccess  = "FAKE-ACCESS"
		wantRefresh = "FAKE-REFRESH"
		wantID      = "FAKE-ID"
	)
	store := &fakeStore{blob: blob}
	m := newTestMaterializer(t, store)

	res, persist, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	onDisk, err := os.ReadFile(filepath.Join(res.HomeDir, authFileName))
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}
	var doc struct {
		Tokens struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			IDToken      string `json:"id_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(onDisk, &doc); err != nil {
		t.Fatalf("auth.json not valid JSON: %v", err)
	}
	if doc.Tokens.AccessToken != wantAccess || doc.Tokens.RefreshToken != wantRefresh || doc.Tokens.IDToken != wantID {
		t.Fatalf("tokens not filled from top-level: %+v", doc.Tokens)
	}
	if err := persist(context.Background()); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if got := store.saveCount(); got != 0 {
		t.Fatalf("SaveCredential called %d times; want 0 (unchanged gate)", got)
	}
}

func TestPersistBackUnchanged(t *testing.T) {
	blob := codexBlob(t, fakeAccess, fakeID, fakeRefresh)
	store := &fakeStore{blob: blob}
	m := newTestMaterializer(t, store)

	_, persist, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	if err := persist(context.Background()); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if got := store.saveCount(); got != 0 {
		t.Fatalf("SaveCredential called %d times; want 0", got)
	}
	// Second call is still a no-op.
	if err := persist(context.Background()); err != nil {
		t.Fatalf("persist (2nd): %v", err)
	}
	if got := store.saveCount(); got != 0 {
		t.Fatalf("SaveCredential called %d times after 2nd; want 0", got)
	}
}

func TestPersistBackChanged(t *testing.T) {
	blob := codexBlob(t, fakeAccess, fakeID, fakeRefresh)
	store := &fakeStore{blob: blob}
	m := newTestMaterializer(t, store)

	res, persist, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}

	// Simulate an agent-side refresh: new access token, dropped id_token.
	refreshed := codexBlob(t, "FAKE-ACCESS-2", "", "FAKE-REFRESH-2")
	if err := os.WriteFile(filepath.Join(res.HomeDir, authFileName), refreshed, 0o600); err != nil {
		t.Fatalf("rewrite auth.json: %v", err)
	}

	if err := persist(context.Background()); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if got := store.saveCount(); got != 1 {
		t.Fatalf("SaveCredential called %d times; want 1", got)
	}

	var saved map[string]json.RawMessage
	if err := json.Unmarshal(store.lastSaved, &saved); err != nil {
		t.Fatalf("unmarshal saved blob: %v", err)
	}
	var toks tokenFields
	if err := json.Unmarshal(saved["tokens"], &toks); err != nil {
		t.Fatalf("unmarshal saved tokens: %v", err)
	}
	if toks.AccessToken != "FAKE-ACCESS-2" {
		t.Fatalf("access_token = %q; want refreshed", toks.AccessToken)
	}
	if toks.IDToken != fakeID {
		t.Fatalf("id_token = %q; want preserved %q", toks.IDToken, fakeID)
	}

	// Idempotent: a second persist with no further change is a no-op.
	if err := persist(context.Background()); err != nil {
		t.Fatalf("persist (2nd): %v", err)
	}
	if got := store.saveCount(); got != 1 {
		t.Fatalf("SaveCredential called %d times after idempotent 2nd; want 1", got)
	}
}

// TestPersistBackMergesAgainstCurrentStore proves persist-back reloads the
// current stored blob as the merge baseline (not the stale materialize-time
// snapshot), so a newer id_token persisted by another session is never rolled
// back to the older one this session materialized from.
func TestPersistBackMergesAgainstCurrentStore(t *testing.T) {
	original := codexBlob(t, "A1", "FAKE-ID-OLD", fakeRefresh)
	store := &fakeStore{blob: original}
	m := newTestMaterializer(t, store)

	res, persist, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}

	// Another session rotates the account and persists a NEW id_token to the
	// store after we materialized (our snapshot still holds FAKE-ID-OLD).
	store.setBlob(codexBlob(t, "A2", "FAKE-ID-NEW", "R2"))

	// Our agent refreshes auth.json dropping id_token entirely.
	refreshed := codexBlob(t, "A3", "", "R3")
	if err := os.WriteFile(filepath.Join(res.HomeDir, authFileName), refreshed, 0o600); err != nil {
		t.Fatalf("rewrite auth.json: %v", err)
	}
	if err := persist(context.Background()); err != nil {
		t.Fatalf("persist: %v", err)
	}

	var toks tokenFields
	var saved map[string]json.RawMessage
	if err := json.Unmarshal(store.lastSaved, &saved); err != nil {
		t.Fatalf("unmarshal saved: %v", err)
	}
	if err := json.Unmarshal(saved["tokens"], &toks); err != nil {
		t.Fatalf("unmarshal saved tokens: %v", err)
	}
	// Must preserve the NEWER store id_token, never roll back to FAKE-ID-OLD.
	if toks.IDToken != "FAKE-ID-NEW" {
		t.Fatalf("id_token = %q; want current-store FAKE-ID-NEW (no rollback)", toks.IDToken)
	}
	if toks.AccessToken != "A3" {
		t.Fatalf("access_token = %q; want refreshed A3", toks.AccessToken)
	}
}

func TestMergePreservingIDToken(t *testing.T) {
	tests := []struct {
		name     string
		prev     string
		next     string
		wantTok  map[string]string
		wantTop  map[string]string // extra top-level string fields expected
		wantsErr bool
	}{
		{
			name:    "next lacks id_token keeps prev",
			prev:    `{"tokens":{"id_token":"FAKE-ID-1","access_token":"A1","refresh_token":"R1","account_id":"ACC"}}`,
			next:    `{"tokens":{"access_token":"A2","refresh_token":"R2","account_id":"ACC"}}`,
			wantTok: map[string]string{"id_token": "FAKE-ID-1", "access_token": "A2", "refresh_token": "R2", "account_id": "ACC"},
		},
		{
			name:    "next has new id_token uses next",
			prev:    `{"tokens":{"id_token":"FAKE-ID-1","access_token":"A1"}}`,
			next:    `{"tokens":{"id_token":"FAKE-ID-2","access_token":"A2"}}`,
			wantTok: map[string]string{"id_token": "FAKE-ID-2", "access_token": "A2"},
		},
		{
			name:    "opencode openai-shape normalizes to tokens",
			prev:    `{"tokens":{"id_token":"FAKE-ID-1","access_token":"A1","refresh_token":"R1"}}`,
			next:    `{"openai":{"access_token":"A2"}}`,
			wantTok: map[string]string{"id_token": "FAKE-ID-1", "access_token": "A2", "refresh_token": "R1"},
		},
		{
			name:    "unknown top-level field on prev survives",
			prev:    `{"tokens":{"id_token":"FAKE-ID-1","access_token":"A1"},"last_refresh":"2026-01-01"}`,
			next:    `{"tokens":{"access_token":"A2"}}`,
			wantTok: map[string]string{"id_token": "FAKE-ID-1", "access_token": "A2"},
			wantTop: map[string]string{"last_refresh": "2026-01-01"},
		},
		{
			name:    "account-store top-level shape mirrors into tokens preserving id_token",
			prev:    `{"access":"A1","refresh":"R1","id_token":"FAKE-ID-1"}`,
			next:    `{"tokens":{"access_token":"A2","refresh_token":"R2"}}`,
			wantTok: map[string]string{"id_token": "FAKE-ID-1", "access_token": "A2", "refresh_token": "R2"},
			// Top-level account-store keys survive so TestAccount validation still passes.
			wantTop: map[string]string{"access": "A1", "refresh": "R1", "id_token": "FAKE-ID-1"},
		},
		{
			name:    "account-store top-level shape yields to newer next id_token",
			prev:    `{"access":"A1","refresh":"R1","id_token":"FAKE-ID-OLD"}`,
			next:    `{"tokens":{"id_token":"FAKE-ID-NEW","access_token":"A2"}}`,
			wantTok: map[string]string{"id_token": "FAKE-ID-NEW", "access_token": "A2", "refresh_token": "R1"},
			wantTop: map[string]string{"access": "A1"},
		},
		{
			name:    "top-level fields fill empty tokens object",
			prev:    `{"access":"A1","refresh":"R1","id_token":"FAKE-ID-1","tokens":{}}`,
			next:    `{"tokens":{"access_token":"A2"}}`,
			wantTok: map[string]string{"id_token": "FAKE-ID-1", "access_token": "A2", "refresh_token": "R1"},
			wantTop: map[string]string{"access": "A1"},
		},
		{
			name:    "top-level fields fill empty and missing tokens fields but keep present ones",
			prev:    `{"access":"A1","refresh":"R1","id_token":"FAKE-ID-1","tokens":{"access_token":"","id_token":"FAKE-ID-KEEP"}}`,
			next:    `{"tokens":{"access_token":"A2"}}`,
			wantTok: map[string]string{"id_token": "FAKE-ID-KEEP", "access_token": "A2", "refresh_token": "R1"},
			wantTop: map[string]string{"access": "A1"},
		},
		{
			name:    "top-level fields fill null tokens object",
			prev:    `{"access":"A1","refresh":"R1","id_token":"FAKE-ID-1","tokens":null}`,
			next:    `{"tokens":{"access_token":"A2"}}`,
			wantTok: map[string]string{"id_token": "FAKE-ID-1", "access_token": "A2", "refresh_token": "R1"},
			wantTop: map[string]string{"access": "A1"},
		},
		{
			// A non-object tokens (array) as the merge baseline previously errored
			// out of mergePreservingIDToken; it is now rebuilt from top-level.
			name:    "non-object tokens rebuilt from top-level",
			prev:    `{"access":"A1","refresh":"R1","id_token":"FAKE-ID-1","tokens":[1,2,3]}`,
			next:    `{"tokens":{"access_token":"A2"}}`,
			wantTok: map[string]string{"id_token": "FAKE-ID-1", "access_token": "A2", "refresh_token": "R1"},
			wantTop: map[string]string{"access": "A1"},
		},
		{
			name:    "access refresh account fall back to prev when absent",
			prev:    `{"tokens":{"id_token":"FAKE-ID-1","access_token":"A1","refresh_token":"R1","account_id":"ACC1"}}`,
			next:    `{"tokens":{"id_token":"FAKE-ID-2"}}`,
			wantTok: map[string]string{"id_token": "FAKE-ID-2", "access_token": "A1", "refresh_token": "R1", "account_id": "ACC1"},
		},
		{
			name:     "invalid prev JSON errors",
			prev:     `not json`,
			next:     `{"tokens":{}}`,
			wantsErr: true,
		},
		{
			name:     "invalid next JSON errors",
			prev:     `{"tokens":{}}`,
			next:     `not json`,
			wantsErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := mergePreservingIDToken([]byte(tc.prev), []byte(tc.next))
			if tc.wantsErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("mergePreservingIDToken: %v", err)
			}
			var top map[string]json.RawMessage
			if err := json.Unmarshal(out, &top); err != nil {
				t.Fatalf("unmarshal merged: %v", err)
			}
			var toks map[string]string
			if err := json.Unmarshal(top["tokens"], &toks); err != nil {
				t.Fatalf("unmarshal merged tokens: %v", err)
			}
			for k, want := range tc.wantTok {
				if toks[k] != want {
					t.Fatalf("tokens[%q] = %q; want %q", k, toks[k], want)
				}
			}
			if _, ok := top["openai"]; ok {
				t.Fatal("openai key should be normalized away")
			}
			for k, want := range tc.wantTop {
				var got string
				if err := json.Unmarshal(top[k], &got); err != nil {
					t.Fatalf("unmarshal top[%q]: %v", k, err)
				}
				if got != want {
					t.Fatalf("top[%q] = %q; want %q", k, got, want)
				}
			}
		})
	}
}

func TestMaterializeConcurrentNoRace(t *testing.T) {
	blob := codexBlob(t, fakeAccess, fakeID, fakeRefresh)
	store := &fakeStore{blob: blob}
	m := newTestMaterializer(t, store)

	const n = 16
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			res, persist, err := m.MaterializeCodex(context.Background(), "shared-acct")
			if err != nil {
				t.Errorf("MaterializeCodex: %v", err)
				return
			}
			// Simulate a distinct agent-side refresh per goroutine that drops
			// id_token, so the persist-back save+merge path fires under
			// contention and id_token preservation is exercised concurrently.
			refreshed := codexBlob(t, fmt.Sprintf("FAKE-ACCESS-%d", i), "", fmt.Sprintf("FAKE-REFRESH-%d", i))
			// Write atomically (as codex/atomicWriteFile does) so a concurrent
			// persist-back read on the shared dir never sees a torn file — the
			// point under test is serialized saves, not a non-atomic test writer.
			if err := atomicWriteFile(filepath.Join(res.HomeDir, authFileName), refreshed, 0o600); err != nil {
				t.Errorf("rewrite auth.json: %v", err)
				return
			}
			if err := persist(context.Background()); err != nil {
				t.Errorf("persist: %v", err)
				return
			}
		}(i)
	}
	wg.Wait()

	// File must be valid JSON at the end (no torn writes).
	dir := m.codexAccountDir("shared-acct")
	onDisk, err := os.ReadFile(filepath.Join(dir, authFileName))
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(onDisk, &probe); err != nil {
		t.Fatalf("auth.json not valid JSON after concurrency: %v", err)
	}
	// At least one refresh must have been persisted, and every serialized save
	// went through the merge (fakeStore copies under its own lock, so a torn or
	// interleaved write would surface as invalid JSON here).
	if got := store.saveCount(); got < 1 {
		t.Fatalf("SaveCredential called %d times; want >= 1", got)
	}
	store.mu.Lock()
	lastSaved := append([]byte(nil), store.lastSaved...)
	store.mu.Unlock()
	var savedToks struct {
		Tokens tokenFields `json:"tokens"`
	}
	if err := json.Unmarshal(lastSaved, &savedToks); err != nil {
		t.Fatalf("last saved blob not valid JSON after concurrency: %v", err)
	}
	// id_token is dropped by every refresh, so the merge must have restored it
	// from prev on every serialized save — proving preservation under -race.
	if savedToks.Tokens.IDToken != fakeID {
		t.Fatalf("id_token = %q after concurrent persists; want preserved %q", savedToks.Tokens.IDToken, fakeID)
	}
}

func TestMaterializePermissionBits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits not applicable on Windows")
	}
	blob := codexBlob(t, fakeAccess, fakeID, fakeRefresh)
	store := &fakeStore{blob: blob}
	m := newTestMaterializer(t, store)

	res, _, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	dirInfo, err := os.Stat(res.HomeDir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("dir perm = %o; want 0700", perm)
	}
	fileInfo, err := os.Stat(filepath.Join(res.HomeDir, authFileName))
	if err != nil {
		t.Fatalf("stat auth.json: %v", err)
	}
	if perm := fileInfo.Mode().Perm(); perm != 0o600 {
		t.Fatalf("auth.json perm = %o; want 0600", perm)
	}
}

func TestMaterializeTightensExistingDirPerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits not applicable on Windows")
	}
	base := t.TempDir()
	blob := codexBlob(t, fakeAccess, fakeID, fakeRefresh)
	store := &fakeStore{blob: blob}
	m, err := New(store, zerolog.Nop(), WithBaseDir(base))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Simulate a pre-existing accounts tree with broad permissions on the leaf
	// AND its parents (restored backup, prior buggy build, or manual
	// precreation). os.MkdirAll returns nil without tightening any existing dir,
	// so materialize must chmod the whole accounts/<provider>/<id> chain to 0700.
	accountsRoot := filepath.Join(base, "accounts")
	providerDir := filepath.Join(accountsRoot, providerCodex)
	preDir := filepath.Join(providerDir, "acct-pre")
	if err := os.MkdirAll(preDir, 0o777); err != nil {
		t.Fatalf("precreate dir: %v", err)
	}
	for _, d := range []string{accountsRoot, providerDir, preDir} {
		if err := os.Chmod(d, 0o777); err != nil { // force broad past umask
			t.Fatalf("chmod precreate %s: %v", d, err)
		}
	}

	res, _, err := m.MaterializeCodex(context.Background(), "acct-pre")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	// Every level of the chain — accounts root, provider dir, and leaf — must be
	// tightened to owner-only, not just the leaf.
	for _, d := range []string{accountsRoot, providerDir, res.HomeDir} {
		info, statErr := os.Stat(d)
		if statErr != nil {
			t.Fatalf("stat %s: %v", d, statErr)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Fatalf("pre-existing dir %s perm = %o; want 0700 (materialize must tighten)", d, perm)
		}
	}
}

func TestRemoveAccount(t *testing.T) {
	blob := codexBlob(t, fakeAccess, fakeID, fakeRefresh)
	store := &fakeStore{blob: blob}
	m := newTestMaterializer(t, store)

	res, _, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	if err := m.RemoveAccount(context.Background(), providerCodex, "acct-1"); err != nil {
		t.Fatalf("RemoveAccount codex: %v", err)
	}
	if _, err := os.Stat(res.HomeDir); !os.IsNotExist(err) {
		t.Fatalf("codex dir still present after removal: %v", err)
	}
	// Idempotent: removing again on an absent dir is a no-op.
	if err := m.RemoveAccount(context.Background(), providerCodex, "acct-1"); err != nil {
		t.Fatalf("RemoveAccount codex (absent): %v", err)
	}
	// claude is a no-op.
	if err := m.RemoveAccount(context.Background(), providerClaude, "acct-1"); err != nil {
		t.Fatalf("RemoveAccount claude: %v", err)
	}
}

func TestRemoveAccountRejectsSymlinkParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	base := t.TempDir()
	store := &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}
	m, err := New(store, zerolog.Nop(), WithBaseDir(base))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// An external tree that must NOT be touched — reachable only if RemoveAll
	// follows a symlinked provider dir out of the accounts tree.
	external := t.TempDir()
	sentinel := filepath.Join(external, "acct-x", "keep.txt")
	if err := os.MkdirAll(filepath.Dir(sentinel), 0o700); err != nil {
		t.Fatalf("mkdir victim: %v", err)
	}
	if err := os.WriteFile(sentinel, []byte("do not delete"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	// Plant a symlinked provider dir: <base>/accounts/codex -> external.
	accountsRoot := filepath.Join(base, "accounts")
	if err := os.MkdirAll(accountsRoot, 0o700); err != nil {
		t.Fatalf("mkdir accounts root: %v", err)
	}
	if err := os.Symlink(external, filepath.Join(accountsRoot, providerCodex)); err != nil {
		t.Fatalf("symlink provider dir: %v", err)
	}
	// Removal would resolve to external/acct-x through the symlink; it must
	// refuse instead of deleting outside the accounts tree.
	if err := m.RemoveAccount(context.Background(), providerCodex, "acct-x"); err == nil {
		t.Fatalf("RemoveAccount followed a symlinked parent; want refusal")
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("external victim was deleted through the symlink: %v", err)
	}
}

func TestMaterializeRejectsSymlinkParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	base := t.TempDir()
	store := &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}
	m, err := New(store, zerolog.Nop(), WithBaseDir(base))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// External tree that must NOT receive credential bytes if MkdirAll/Chmod/
	// atomicWriteFile follow a symlinked provider dir.
	external := t.TempDir()
	accountsRoot := filepath.Join(base, "accounts")
	if err := os.MkdirAll(accountsRoot, 0o700); err != nil {
		t.Fatalf("mkdir accounts root: %v", err)
	}
	if err := os.Symlink(external, filepath.Join(accountsRoot, providerCodex)); err != nil {
		t.Fatalf("symlink provider dir: %v", err)
	}
	if _, _, err := m.MaterializeCodex(context.Background(), "acct-x"); err == nil {
		t.Fatalf("MaterializeCodex followed a symlinked parent; want refusal")
	}
	// No auth.json should have been written into the external target.
	if _, err := os.Stat(filepath.Join(external, "acct-x", authFileName)); !os.IsNotExist(err) {
		t.Fatalf("credentials written outside the accounts tree via symlink: %v", err)
	}
}

func TestMaterializeClaude(t *testing.T) {
	store := &fakeStore{blob: []byte("  FAKE-CLAUDE-TOKEN\n")}
	m := newTestMaterializer(t, store)

	res, err := m.MaterializeClaude(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeClaude: %v", err)
	}
	if res.Env["CLAUDE_CODE_OAUTH_TOKEN"] != "FAKE-CLAUDE-TOKEN" {
		t.Fatalf("token = %q; want trimmed FAKE-CLAUDE-TOKEN", res.Env["CLAUDE_CODE_OAUTH_TOKEN"])
	}
	if res.HomeDir != "" {
		t.Fatalf("HomeDir = %q; want empty", res.HomeDir)
	}
	// No directory should have been created.
	if _, err := os.Stat(m.accountsRoot()); !os.IsNotExist(err) {
		t.Fatalf("accounts root created by claude materialize: %v", err)
	}
}

func TestRedactionNoSecretsInErrors(t *testing.T) {
	const sentinel = "SENTINEL-SUPER-SECRET-VALUE"

	// (a) Store load error: the error message must not echo the (unavailable)
	// blob, and the wrapped error must not contain the sentinel.
	loadErrStore := &fakeStore{loadErr: errors.New("keyring unavailable for account")}
	m := newTestMaterializer(t, loadErrStore)
	_, _, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err == nil {
		t.Fatal("expected load error")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Fatalf("load error leaked sentinel: %q", err.Error())
	}

	// (b) Parse-error path in persist-back merge: feed an invalid-JSON refresh
	// containing the sentinel and assert the error omits it.
	blob := codexBlob(t, fakeAccess, sentinel, fakeRefresh)
	store := &fakeStore{blob: blob}
	m2 := newTestMaterializer(t, store)
	res, persist, err := m2.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	// Overwrite auth.json with invalid JSON that embeds the sentinel.
	if err := os.WriteFile(filepath.Join(res.HomeDir, authFileName),
		[]byte("not-json "+sentinel), 0o600); err != nil {
		t.Fatalf("corrupt auth.json: %v", err)
	}
	perr := persist(context.Background())
	if perr == nil {
		t.Fatal("expected persist merge error")
	}
	if strings.Contains(perr.Error(), sentinel) {
		t.Fatalf("persist error leaked sentinel: %q", perr.Error())
	}

	// (c) Save error path must not echo blob content either.
	saveErrStore := &fakeStore{blob: blob, saveErr: errors.New("store write failed")}
	m3 := newTestMaterializer(t, saveErrStore)
	res3, persist3, err := m3.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	refreshed := codexBlob(t, "A2", "FAKE-ID-2", "R2")
	if err := os.WriteFile(filepath.Join(res3.HomeDir, authFileName), refreshed, 0o600); err != nil {
		t.Fatalf("rewrite auth.json: %v", err)
	}
	serr := persist3(context.Background())
	if serr == nil {
		t.Fatal("expected save error")
	}
	if strings.Contains(serr.Error(), sentinel) || strings.Contains(serr.Error(), "FAKE-ID-2") {
		t.Fatalf("save error leaked secret: %q", serr.Error())
	}
}

func TestCacheDirTagWritten(t *testing.T) {
	blob := codexBlob(t, fakeAccess, fakeID, fakeRefresh)
	store := &fakeStore{blob: blob}
	m := newTestMaterializer(t, store)

	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	tag, err := os.ReadFile(filepath.Join(m.accountsRoot(), cacheDirTagName))
	if err != nil {
		t.Fatalf("read CACHEDIR.TAG: %v", err)
	}
	// The first line MUST be the exact spec magic or no backup tool recognizes
	// the marker (that is the whole point of dropping it).
	if !strings.HasPrefix(string(tag), "Signature: 8a477f597d28d172789f06886806bc55\n") {
		t.Fatalf("CACHEDIR.TAG missing canonical spec signature: %q", string(tag))
	}
}

func TestCacheDirTagRewritesNonCanonical(t *testing.T) {
	base := t.TempDir()
	blob := codexBlob(t, fakeAccess, fakeID, fakeRefresh)
	store := &fakeStore{blob: blob}
	m, err := New(store, zerolog.Nop(), WithBaseDir(base))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// A stale/noncanonical CACHEDIR.TAG (manual precreation or a prior build's
	// wrong tag) is silently ignored by backup tools, so materialize must
	// overwrite it with the canonical body rather than trusting mere existence.
	accountsRoot := filepath.Join(base, "accounts")
	if err := os.MkdirAll(accountsRoot, 0o700); err != nil {
		t.Fatalf("mkdir accounts root: %v", err)
	}
	tagPath := filepath.Join(accountsRoot, cacheDirTagName)
	if err := os.WriteFile(tagPath, []byte("stale non-canonical junk\n"), 0o600); err != nil {
		t.Fatalf("write stale tag: %v", err)
	}
	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	tag, err := os.ReadFile(tagPath)
	if err != nil {
		t.Fatalf("read CACHEDIR.TAG: %v", err)
	}
	if !strings.HasPrefix(string(tag), cacheDirTagSignature+"\n") {
		t.Fatalf("noncanonical CACHEDIR.TAG not rewritten: %q", string(tag))
	}

	// A tag whose first line merely starts with the magic but carries trailing
	// garbage on that line (e.g. a stale/manually-precreated file) is NOT the
	// canonical signature backup tools match, so it must be rewritten.
	malformed := cacheDirTagSignature + "-bad\n"
	if err := os.WriteFile(tagPath, []byte(malformed), 0o600); err != nil {
		t.Fatalf("write malformed tag: %v", err)
	}
	if _, _, err := m.MaterializeCodex(context.Background(), "acct-malformed"); err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	tag, err = os.ReadFile(tagPath)
	if err != nil {
		t.Fatalf("read CACHEDIR.TAG: %v", err)
	}
	if string(tag) != cacheDirTagBody {
		t.Fatalf("malformed-signature CACHEDIR.TAG not rewritten to canonical body: %q", string(tag))
	}

	// A tag that already carries the canonical signature (even a foreign app's)
	// is honored by backup tools, so it must be preserved, not clobbered.
	foreign := cacheDirTagSignature + "\n# created by some other tool\n"
	if err := os.WriteFile(tagPath, []byte(foreign), 0o600); err != nil {
		t.Fatalf("write foreign canonical tag: %v", err)
	}
	if _, _, err := m.MaterializeCodex(context.Background(), "acct-2"); err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	tag, err = os.ReadFile(tagPath)
	if err != nil {
		t.Fatalf("re-read CACHEDIR.TAG: %v", err)
	}
	if string(tag) != foreign {
		t.Fatalf("canonical tag was clobbered: got %q want %q", string(tag), foreign)
	}
}

func TestCacheDirTagSymlinkNotFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	base := t.TempDir()
	store := &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}
	m, err := New(store, zerolog.Nop(), WithBaseDir(base))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// An out-of-tree file the symlinked tag points at; it must NOT be clobbered.
	external := t.TempDir()
	victim := filepath.Join(external, "victim.txt")
	const victimBody = "unrelated user data"
	if err := os.WriteFile(victim, []byte(victimBody), 0o600); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	accountsRoot := filepath.Join(base, "accounts")
	if err := os.MkdirAll(accountsRoot, 0o700); err != nil {
		t.Fatalf("mkdir accounts root: %v", err)
	}
	if err := os.Symlink(victim, filepath.Join(accountsRoot, cacheDirTagName)); err != nil {
		t.Fatalf("symlink cache tag: %v", err)
	}

	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	// The external victim must be untouched.
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("read victim: %v", err)
	}
	if string(got) != victimBody {
		t.Fatalf("symlinked CACHEDIR.TAG followed; victim clobbered: %q", string(got))
	}
	// The tag is now a regular canonical file, not a symlink.
	tagPath := filepath.Join(accountsRoot, cacheDirTagName)
	info, err := os.Lstat(tagPath)
	if err != nil {
		t.Fatalf("lstat tag: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("CACHEDIR.TAG is still a symlink after materialize")
	}
	tag, err := os.ReadFile(tagPath)
	if err != nil {
		t.Fatalf("read tag: %v", err)
	}
	if !strings.HasPrefix(string(tag), cacheDirTagSignature+"\n") {
		t.Fatalf("CACHEDIR.TAG not canonical after replacing symlink: %q", string(tag))
	}
}

func TestCacheDirTagFifoNotOpened(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFO semantics differ on Windows")
	}
	base := t.TempDir()
	store := &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}
	m, err := New(store, zerolog.Nop(), WithBaseDir(base))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// A restored/manually-precreated accounts root holds CACHEDIR.TAG as a FIFO.
	// The symlink guard would not catch it, and ReadFile on a writerless FIFO
	// blocks forever while MaterializeCodex holds the account lock. The Lstat/
	// IsRegular guard must drop the FIFO instead of opening it, so materialize
	// completes and rewrites the tag as a regular canonical file.
	accountsRoot := filepath.Join(base, "accounts")
	if err := os.MkdirAll(accountsRoot, 0o700); err != nil {
		t.Fatalf("mkdir accounts root: %v", err)
	}
	tagPath := filepath.Join(accountsRoot, cacheDirTagName)
	if err := syscall.Mkfifo(tagPath, 0o600); err != nil {
		t.Fatalf("mkfifo cache tag: %v", err)
	}

	// Run in a goroutine with a timeout so a regression (an actual open of the
	// FIFO) fails the test loudly instead of hanging the whole suite.
	done := make(chan error, 1)
	go func() {
		_, _, mErr := m.MaterializeCodex(context.Background(), "acct-1")
		done <- mErr
	}()
	select {
	case mErr := <-done:
		if mErr != nil {
			t.Fatalf("MaterializeCodex: %v", mErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("MaterializeCodex hung: CACHEDIR.TAG FIFO was opened instead of replaced")
	}

	// The FIFO is gone, replaced by a regular canonical tag.
	info, err := os.Lstat(tagPath)
	if err != nil {
		t.Fatalf("lstat tag: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("CACHEDIR.TAG is not a regular file after materialize: mode %v", info.Mode())
	}
	tag, err := os.ReadFile(tagPath)
	if err != nil {
		t.Fatalf("read tag: %v", err)
	}
	if !strings.HasPrefix(string(tag), cacheDirTagSignature+"\n") {
		t.Fatalf("CACHEDIR.TAG not canonical after replacing FIFO: %q", string(tag))
	}
}

func TestMaterializeRejectsTraversalAccountID(t *testing.T) {
	blob := codexBlob(t, fakeAccess, fakeID, fakeRefresh)
	store := &fakeStore{blob: blob}
	m := newTestMaterializer(t, store)

	for _, bad := range []string{"", ".", "..", "../evil", "a/b", `a\b`, "with\x00nul"} {
		if _, _, err := m.MaterializeCodex(context.Background(), bad); err == nil {
			t.Fatalf("MaterializeCodex(%q) = nil error; want rejection", bad)
		}
		if _, err := m.MaterializeClaude(context.Background(), bad); err == nil {
			t.Fatalf("MaterializeClaude(%q) = nil error; want rejection", bad)
		}
		if err := m.RemoveAccount(context.Background(), providerCodex, bad); err == nil {
			t.Fatalf("RemoveAccount(%q) = nil error; want rejection", bad)
		}
	}
	// The accounts root must never have been created by a rejected call.
	if _, err := os.Stat(m.accountsRoot()); !os.IsNotExist(err) {
		t.Fatalf("accounts root created by a rejected traversal id: %v", err)
	}
}
