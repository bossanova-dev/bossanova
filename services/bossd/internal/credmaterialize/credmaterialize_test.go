package credmaterialize

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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

// setSaveErr makes every subsequent SaveCredential fail, simulating a keyring
// that became unwritable between two materializations.
func (f *fakeStore) setSaveErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saveErr = err
}

func (f *fakeStore) saveCount() int64 { return atomic.LoadInt64(&f.saveCalls) }

// currentBlob returns what the store holds now, which is what a test asserting
// that a credential was left ALONE needs: savedTokens only ever sees the last
// blob handed to SaveCredential, and there is no such blob when nothing saved.
func (f *fakeStore) currentBlob() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.blob...)
}

// serializedStore lets the reconciliation test stop exactly after its load
// snapshot. Its WithCredentialLock is the same optional lock production uses
// for an operator refresh, so the test can prove the entire load-merge-save
// sequence — not only the final save — is serialized.
type serializedStore struct {
	mu             sync.Mutex
	credentialMu   sync.Mutex
	blob           []byte
	loadedSnapshot chan struct{}
	releaseLoad    chan struct{}
	lockEntered    chan struct{}
}

func (s *serializedStore) LoadCredential(_ context.Context, _ string) ([]byte, error) {
	s.mu.Lock()
	snapshot := append([]byte(nil), s.blob...)
	s.mu.Unlock()
	close(s.loadedSnapshot)
	<-s.releaseLoad
	return snapshot, nil
}

func (s *serializedStore) SaveCredential(_ context.Context, _ string, blob []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blob = append([]byte(nil), blob...)
	return nil
}

func (s *serializedStore) WithCredentialLock(_ string, fn func() error) error {
	s.lockEntered <- struct{}{}
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()
	return fn()
}

func (s *serializedStore) storedBlob() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.blob...)
}

func newTestMaterializer(t *testing.T, store CredentialStore) *Materializer {
	return newTestMaterializerWithCodexHome(t, store, t.TempDir())
}

func newTestMaterializerWithCodexHome(t *testing.T, store CredentialStore, codexHome string) *Materializer {
	t.Helper()
	t.Setenv("CODEX_HOME", codexHome)
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

// codexBlobAt builds a codex auth blob that also carries "last_refresh", the
// generation marker codex stamps on every token rotation. codexBlob omits it on
// purpose: most reconcile tests want the UNORDERED case, where neither blob can
// be placed after the other and the fold-back applies.
func codexBlobAt(t *testing.T, access, id, refresh, lastRefresh string) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(codexBlob(t, access, id, refresh), &doc); err != nil {
		t.Fatalf("unmarshal codex blob: %v", err)
	}
	doc[authGenerationKey] = lastRefresh
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

func TestMaterializeCodexProjectsBaseHomeAndKeepsLocalAuth(t *testing.T) {
	baseHome := t.TempDir()
	t.Setenv("CODEX_HOME", baseHome)
	for _, name := range []string{
		"config.toml",
		"plugins/linear/state.json",
		"sessions/session-1.jsonl",
		"session_index.jsonl",
	} {
		path := filepath.Join(baseHome, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("mkdir base home fixture: %v", err)
		}
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatalf("write base home fixture: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(baseHome, authFileName), []byte("base-auth"), 0o600); err != nil {
		t.Fatalf("write base auth fixture: %v", err)
	}
	canonicalBaseHome, err := filepath.EvalSymlinks(baseHome)
	if err != nil {
		t.Fatalf("resolve base home: %v", err)
	}

	m := newTestMaterializerWithCodexHome(t, &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}, baseHome)
	res, _, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}

	for _, name := range []string{
		"config.toml",
		"plugins",
		"sessions",
		"session_index.jsonl",
	} {
		projected := filepath.Join(res.HomeDir, name)
		info, err := os.Lstat(projected)
		if err != nil {
			t.Fatalf("lstat projected %q: %v", name, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("projected %q is not a symlink", name)
		}
		target, err := os.Readlink(projected)
		if err != nil {
			t.Fatalf("readlink projected %q: %v", name, err)
		}
		if target != filepath.Join(canonicalBaseHome, name) {
			t.Fatalf("projected %q target = %q; want base-home entry", name, target)
		}
	}

	authPath := filepath.Join(res.HomeDir, authFileName)
	authInfo, err := os.Lstat(authPath)
	if err != nil {
		t.Fatalf("lstat account auth: %v", err)
	}
	if authInfo.Mode()&os.ModeSymlink != 0 {
		t.Fatal("account auth.json must not be a symlink")
	}
}

// TestMaterializeCodexKeepsLocalAuthWriteRecord asserts the account's auth.json
// write record is account-local like auth.json itself: an entry of that name in
// the inherited codex home is never projected over it. Projection insists on
// owning every name it links, so a projected record would not merely leak one
// account's state into another — it would make the FIRST materialization write the
// sidecar and every later one fail permanently on the existing non-symlink.
func TestMaterializeCodexKeepsLocalAuthWriteRecord(t *testing.T) {
	baseHome := t.TempDir()
	t.Setenv("CODEX_HOME", baseHome)
	if err := os.WriteFile(filepath.Join(baseHome, authHashFileName), []byte("base-record"), 0o600); err != nil {
		t.Fatalf("write base write-record fixture: %v", err)
	}

	m := newTestMaterializerWithCodexHome(t, &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}, baseHome)
	res, _, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	// The second materialization is the one that regresses: the first writes the
	// sidecar, so a projection attempt afterwards finds a non-symlink in the way.
	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
		t.Fatalf("MaterializeCodex (2nd): %v", err)
	}

	recordPath := filepath.Join(res.HomeDir, authHashFileName)
	info, err := os.Lstat(recordPath)
	if err != nil {
		t.Fatalf("lstat account write record: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("account auth.json write record must not be a symlink to the base home")
	}
	if _, ok := m.lastAuthWrite(filepath.Join(res.HomeDir, authFileName)); !ok {
		t.Fatal("account write record is not readable; the base-home entry displaced it")
	}
}

// TestMaterializeCodexReconcileIgnoresCorruptWriteRecord pins that a record which
// is not a hash reads as absent. It is right-length but non-hex, the shape a
// length-only check would accept: it would then compare unequal to the file's real
// hash and be read as an agent-side rotation, folding whatever is on disk over the
// stored credential.
func TestMaterializeCodexReconcileIgnoresCorruptWriteRecord(t *testing.T) {
	store := &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}
	m := newTestMaterializer(t, store)

	res, _, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	authPath := filepath.Join(res.HomeDir, authFileName)
	if err := os.WriteFile(authPath, codexBlob(t, fakeAccess, "", "FAKE-REFRESH-2"), 0o600); err != nil {
		t.Fatalf("rewrite auth.json: %v", err)
	}
	corrupt := strings.Repeat("z", hex.EncodedLen(sha256.Size))
	if err := os.WriteFile(authHashPath(authPath), []byte(corrupt), 0o600); err != nil {
		t.Fatalf("corrupt write record: %v", err)
	}
	if _, ok := m.lastAuthWrite(authPath); ok {
		t.Fatal("lastAuthWrite accepted a non-hex record; a corrupt record must read as absent")
	}

	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
		t.Fatalf("MaterializeCodex (2nd): %v", err)
	}
	if store.saveCount() != 0 {
		t.Fatalf("SaveCredential calls = %d; want 0: an unusable record must not drive a fold-back", store.saveCount())
	}
	if got := onDiskTokens(t, authPath).RefreshToken; got != fakeRefresh {
		t.Fatalf("on-disk refresh_token = %q; want the stored credential restored", got)
	}
}

// TestMaterializeCodexReconcileMatchesCaseMangledWriteRecord pins that a record
// comparison is a comparison of hashes, not of their encodings. Hex decoding
// accepts either case, so an upper-cased record is still a perfectly usable
// digest of the untouched file; comparing it as text instead would make it match
// nothing, read as an agent-side rotation, and fold the unchanged on-disk bytes
// over a stored credential an operator may just have refreshed.
func TestMaterializeCodexReconcileMatchesCaseMangledWriteRecord(t *testing.T) {
	store := &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}
	m := newTestMaterializer(t, store)

	res, _, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	authPath := filepath.Join(res.HomeDir, authFileName)
	// auth.json is left exactly as bossd wrote it: only the record's case changes.
	recordPath := authHashPath(authPath)
	raw, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("read write record: %v", err)
	}
	if err := os.WriteFile(recordPath, []byte(strings.ToUpper(strings.TrimSpace(string(raw)))), 0o600); err != nil {
		t.Fatalf("upper-case write record: %v", err)
	}

	onDisk, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}
	recorded, ok := m.lastAuthWrite(authPath)
	if !ok {
		t.Fatal("lastAuthWrite rejected an upper-case hex record; either case is a usable digest")
	}
	if recorded != sha256.Sum256(onDisk) {
		t.Fatal("an upper-case record did not match the untouched file's hash; the comparison is encoding-sensitive")
	}

	// End-to-end, through the reconcile: the operator re-authenticates, so the
	// stored credential no longer equals the file and the digest gate is the thing
	// that decides. An encoding-sensitive comparison reads the untouched file as an
	// agent rotation here and folds it over the refresh.
	store.setBlob(codexBlob(t, "FAKE-ACCESS-2", "FAKE-ID-2", "FAKE-REFRESH-2"))
	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
		t.Fatalf("MaterializeCodex (2nd): %v", err)
	}
	if store.saveCount() != 0 {
		t.Fatalf("SaveCredential calls = %d; want 0: an untouched file must not drive a fold-back", store.saveCount())
	}
	if got := onDiskTokens(t, authPath); got.RefreshToken != "FAKE-REFRESH-2" || got.IDToken != "FAKE-ID-2" {
		t.Fatalf("auth.json tokens = %+v; want the operator-refreshed credential", got)
	}
}

func TestMaterializeCodexReconcilesLaterBaseHomeAdditions(t *testing.T) {
	baseHome := t.TempDir()
	t.Setenv("CODEX_HOME", baseHome)
	if err := os.WriteFile(filepath.Join(baseHome, "config.toml"), []byte("fixture"), 0o600); err != nil {
		t.Fatalf("write base config fixture: %v", err)
	}
	m := newTestMaterializerWithCodexHome(t, &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}, baseHome)
	res, _, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("first MaterializeCodex: %v", err)
	}

	pluginPath := filepath.Join(baseHome, "plugins", "linear", "state.json")
	if err := os.MkdirAll(filepath.Dir(pluginPath), 0o700); err != nil {
		t.Fatalf("mkdir later plugin fixture: %v", err)
	}
	if err := os.WriteFile(pluginPath, []byte("fixture"), 0o600); err != nil {
		t.Fatalf("write later plugin fixture: %v", err)
	}
	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
		t.Fatalf("second MaterializeCodex: %v", err)
	}

	info, err := os.Lstat(filepath.Join(res.HomeDir, "plugins"))
	if err != nil {
		t.Fatalf("lstat reconciled plugin entry: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("later base-home plugin entry is not projected as a symlink")
	}
}

func TestMaterializeCodexReconcilesSafeBaseSymlinkRetarget(t *testing.T) {
	baseHome := t.TempDir()
	t.Setenv("CODEX_HOME", baseHome)
	for _, name := range []string{"plugins-v1", "plugins-v2"} {
		if err := os.Mkdir(filepath.Join(baseHome, name), 0o700); err != nil {
			t.Fatalf("mkdir plugin target fixture: %v", err)
		}
	}
	pluginsPath := filepath.Join(baseHome, "plugins")
	if err := os.Symlink("plugins-v1", pluginsPath); err != nil {
		t.Fatalf("create initial plugin symlink: %v", err)
	}
	m := newTestMaterializerWithCodexHome(t, &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}, baseHome)
	res, _, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("first MaterializeCodex: %v", err)
	}

	if err := os.Remove(pluginsPath); err != nil {
		t.Fatalf("remove initial plugin symlink: %v", err)
	}
	if err := os.Symlink("plugins-v2", pluginsPath); err != nil {
		t.Fatalf("retarget plugin symlink: %v", err)
	}
	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
		t.Fatalf("second MaterializeCodex: %v", err)
	}

	resolved, err := filepath.EvalSymlinks(filepath.Join(res.HomeDir, "plugins"))
	if err != nil {
		t.Fatalf("resolve reconciled plugin projection: %v", err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(baseHome, "plugins-v2"))
	if err != nil {
		t.Fatalf("resolve retargeted plugin target: %v", err)
	}
	if resolved != want {
		t.Fatalf("reconciled plugin target = %q; want retargeted base entry", resolved)
	}
}

func TestMaterializeCodexSkipsOptionalBaseHomeSymlinkOutsideHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	baseHome := t.TempDir()
	t.Setenv("CODEX_HOME", baseHome)
	external := t.TempDir()
	if err := os.Symlink(external, filepath.Join(baseHome, "skills")); err != nil {
		t.Fatalf("create external base-home symlink: %v", err)
	}
	m := newTestMaterializerWithCodexHome(t, &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}, baseHome)
	res, _, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(res.HomeDir, "skills")); !os.IsNotExist(err) {
		t.Fatalf("unsupported external base-home entry was projected: %v", err)
	}
	authInfo, err := os.Lstat(filepath.Join(res.HomeDir, authFileName))
	if err != nil {
		t.Fatalf("lstat account auth: %v", err)
	}
	if authInfo.Mode()&os.ModeSymlink != 0 {
		t.Fatal("account auth.json must remain local when skipping unsupported base entry")
	}
}

func TestMaterializeCodexSkipsOptionalNestedSkillsSymlinkOutsideHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	baseHome := t.TempDir()
	t.Setenv("CODEX_HOME", baseHome)
	if err := os.WriteFile(filepath.Join(baseHome, authFileName), []byte("base-auth"), 0o600); err != nil {
		t.Fatalf("write base auth fixture: %v", err)
	}
	skillsDir := filepath.Join(baseHome, "skills")
	if err := os.MkdirAll(skillsDir, 0o700); err != nil {
		t.Fatalf("mkdir skills fixture: %v", err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(skillsDir, "external-skill")); err != nil {
		t.Fatalf("create nested external skills symlink: %v", err)
	}
	m := newTestMaterializerWithCodexHome(t, &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}, baseHome)

	res, _, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(res.HomeDir, "skills")); !os.IsNotExist(err) {
		t.Fatalf("unsupported nested skills entry was projected: %v", err)
	}
}

func TestMaterializeCodexRejectsSymlinkedMaterializationBaseBeforeCredentialWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	base := t.TempDir()
	external := t.TempDir()
	linkedBase := filepath.Join(base, "linked-base")
	if err := os.Symlink(external, linkedBase); err != nil {
		t.Fatalf("create linked materialization base: %v", err)
	}
	m, err := New(&fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}, zerolog.Nop(), WithBaseDir(linkedBase))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err == nil {
		t.Fatal("MaterializeCodex accepted a symlinked materialization base")
	}
	if _, err := os.Lstat(filepath.Join(external, "accounts", providerCodex, "acct-1", authFileName)); !os.IsNotExist(err) {
		t.Fatalf("account auth.json written through symlinked materialization base: %v", err)
	}
}

func TestMaterializeCodexRejectsBaseHomeAuthAliasBeforeCredentialWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	baseHome := t.TempDir()
	t.Setenv("CODEX_HOME", baseHome)
	if err := os.WriteFile(filepath.Join(baseHome, authFileName), []byte("base-auth"), 0o600); err != nil {
		t.Fatalf("write base auth fixture: %v", err)
	}
	if err := os.Symlink("auth.json", filepath.Join(baseHome, "auth-alias-direct")); err != nil {
		t.Fatalf("create direct auth alias: %v", err)
	}
	if err := os.Symlink("auth-alias-direct", filepath.Join(baseHome, "auth-alias-indirect")); err != nil {
		t.Fatalf("create indirect auth alias: %v", err)
	}
	m := newTestMaterializerWithCodexHome(t, &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}, baseHome)

	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err == nil {
		t.Fatal("MaterializeCodex accepted a base-home alias resolving to auth.json")
	}
	if _, err := os.Lstat(filepath.Join(m.codexAccountDir("acct-1"), authFileName)); !os.IsNotExist(err) {
		t.Fatalf("account auth.json written after base-home auth alias rejection: %v", err)
	}
}

func TestMaterializeCodexRejectsBaseHomeAuthHardLinkBeforeCredentialWrite(t *testing.T) {
	baseHome := t.TempDir()
	t.Setenv("CODEX_HOME", baseHome)
	authPath := filepath.Join(baseHome, authFileName)
	if err := os.WriteFile(authPath, []byte("base-auth"), 0o600); err != nil {
		t.Fatalf("write base auth fixture: %v", err)
	}
	if err := os.Link(authPath, filepath.Join(baseHome, "auth-alias-hard-link")); err != nil {
		t.Fatalf("create auth hard link: %v", err)
	}
	m := newTestMaterializerWithCodexHome(t, &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}, baseHome)

	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err == nil {
		t.Fatal("MaterializeCodex accepted a base-home hard link to auth.json")
	}
	if _, err := os.Lstat(filepath.Join(m.codexAccountDir("acct-1"), authFileName)); !os.IsNotExist(err) {
		t.Fatalf("account auth.json written after base-home auth hard-link rejection: %v", err)
	}
}

func TestMaterializeCodexRejectsNestedBaseHomeAuthSymlinkBeforeCredentialWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	baseHome := t.TempDir()
	t.Setenv("CODEX_HOME", baseHome)
	authPath := filepath.Join(baseHome, authFileName)
	if err := os.WriteFile(authPath, []byte("base-auth"), 0o600); err != nil {
		t.Fatalf("write base auth fixture: %v", err)
	}
	pluginDir := filepath.Join(baseHome, "plugins")
	if err := os.MkdirAll(pluginDir, 0o700); err != nil {
		t.Fatalf("mkdir plugin fixture: %v", err)
	}
	if err := os.Symlink("../auth.json", filepath.Join(pluginDir, "auth-alias")); err != nil {
		t.Fatalf("create nested auth symlink: %v", err)
	}
	m := newTestMaterializerWithCodexHome(t, &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}, baseHome)

	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err == nil {
		t.Fatal("MaterializeCodex accepted a nested base-home symlink to auth.json")
	}
	if _, err := os.Lstat(filepath.Join(m.codexAccountDir("acct-1"), authFileName)); !os.IsNotExist(err) {
		t.Fatalf("account auth.json written after nested auth symlink rejection: %v", err)
	}
}

func TestMaterializeCodexRejectsNestedBaseHomeAuthHardLinkBeforeCredentialWrite(t *testing.T) {
	baseHome := t.TempDir()
	t.Setenv("CODEX_HOME", baseHome)
	authPath := filepath.Join(baseHome, authFileName)
	if err := os.WriteFile(authPath, []byte("base-auth"), 0o600); err != nil {
		t.Fatalf("write base auth fixture: %v", err)
	}
	pluginDir := filepath.Join(baseHome, "plugins")
	if err := os.MkdirAll(pluginDir, 0o700); err != nil {
		t.Fatalf("mkdir plugin fixture: %v", err)
	}
	if err := os.Link(authPath, filepath.Join(pluginDir, "auth-alias")); err != nil {
		t.Fatalf("create nested auth hard link: %v", err)
	}
	m := newTestMaterializerWithCodexHome(t, &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}, baseHome)

	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err == nil {
		t.Fatal("MaterializeCodex accepted a nested base-home hard link to auth.json")
	}
	if _, err := os.Lstat(filepath.Join(m.codexAccountDir("acct-1"), authFileName)); !os.IsNotExist(err) {
		t.Fatalf("account auth.json written after nested auth hard-link rejection: %v", err)
	}
}

func TestMaterializeCodexRejectsNestedBaseHomeSymlinkOutsideBaseBeforeCredentialWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	baseHome := t.TempDir()
	t.Setenv("CODEX_HOME", baseHome)
	if err := os.WriteFile(filepath.Join(baseHome, authFileName), []byte("base-auth"), 0o600); err != nil {
		t.Fatalf("write base auth fixture: %v", err)
	}
	pluginDir := filepath.Join(baseHome, "plugins")
	if err := os.MkdirAll(pluginDir, 0o700); err != nil {
		t.Fatalf("mkdir plugin fixture: %v", err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(pluginDir, "outside")); err != nil {
		t.Fatalf("create nested outside-base symlink: %v", err)
	}
	m := newTestMaterializerWithCodexHome(t, &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}, baseHome)

	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err == nil {
		t.Fatal("MaterializeCodex accepted a nested base-home symlink outside the base home")
	}
	if _, err := os.Lstat(filepath.Join(m.codexAccountDir("acct-1"), authFileName)); !os.IsNotExist(err) {
		t.Fatalf("account auth.json written after nested outside-base symlink rejection: %v", err)
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

// TestPersistBackRecordsWriteSoNextMaterializeSkipsRefold covers the handoff
// between the two fold-back paths. PersistBack saves the refreshed auth.json to
// the store but never rewrites the file, so unless it also advances the durable
// write record the next spawn sees a file that differs from BOTH the store and
// our last recorded write, and folds the very same tokens back a second time.
// Harmless in value, wasteful in keyring writes — and it makes the record's
// meaning drift from "the bytes on disk that are known to be reconciled".
func TestPersistBackRecordsWriteSoNextMaterializeSkipsRefold(t *testing.T) {
	store := &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}
	m := newTestMaterializer(t, store)

	res, persist, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	// An agent-side refresh that drops id_token, so the merged blob the store
	// receives is NOT byte-equal to the file persist-back read.
	if err := os.WriteFile(filepath.Join(res.HomeDir, authFileName), codexBlob(t, "FAKE-ACCESS-2", "", "FAKE-REFRESH-2"), 0o600); err != nil {
		t.Fatalf("rewrite auth.json: %v", err)
	}
	if err := persist(context.Background()); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if got := store.saveCount(); got != 1 {
		t.Fatalf("SaveCredential called %d times; want 1 (the persist-back)", got)
	}

	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
		t.Fatalf("MaterializeCodex (2nd): %v", err)
	}
	if got := store.saveCount(); got != 1 {
		t.Fatalf("SaveCredential called %d times; want 1: bytes already persisted must not be folded again", got)
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

// TestPersistBackSerializesWithOperatorRefresh reproduces the P1 window
// deterministically at the production entry point: persist-back has already
// read the agent-refreshed auth.json when an explicit RefreshAccount wants to
// save a replacement. The shared store lock must make the replacement run after
// the persisted save, so the explicit refresh remains authoritative.
//
// It also pins the durable hash record inside that same transaction. persistBack
// records the hash of the bytes it just folded into the store; if that record
// landed after the lock was released, a peer materializer holding the lock could
// write its own auth.json in between and have its record clobbered by this stale
// one. The sidecar would then describe bytes no longer on disk, and the next
// reconcile would misread a bossd write as an agent rotation. Asserting the
// record from inside the operator's lock body is what proves it happened before
// the lock changed hands. m.locks cannot substitute: production builds a
// Materializer per account source, so two of them share no local lock map.
func TestPersistBackSerializesWithOperatorRefresh(t *testing.T) {
	store := &serializedStore{
		blob:           codexBlob(t, "A1", "ID-OLD", "R1"),
		loadedSnapshot: make(chan struct{}),
		releaseLoad:    make(chan struct{}),
		lockEntered:    make(chan struct{}, 2),
	}
	m := newTestMaterializer(t, store)

	// auth.json as the agent left it, with a recorded hash of some earlier
	// bossd write so persistBack's unchanged gate sees a real agent-side change.
	authPath := filepath.Join(t.TempDir(), "auth.json")
	refreshedByAgent := codexBlob(t, "A2", "", "R2")
	if err := os.WriteFile(authPath, refreshedByAgent, 0o600); err != nil {
		t.Fatalf("seed auth.json: %v", err)
	}
	wantRecord := sha256.Sum256(refreshedByAgent)
	recorded := sha256.Sum256(store.blob)

	persistDone := make(chan error, 1)
	go func() {
		persistDone <- m.persistBack(context.Background(), "acct-1", authPath, &recorded)
	}()
	releasedLoad := false
	defer func() {
		if !releasedLoad {
			close(store.releaseLoad)
		}
	}()

	// The materializer must have entered the shared lock before loading its
	// snapshot. Waiting for it here makes the ordering independent of scheduler
	// timing and fails if the RMW is not entirely inside the lock.
	waitForSignal(t, store.loadedSnapshot, "materializer load snapshot")
	waitForSignal(t, store.lockEntered, "materializer credential lock")

	operatorCredential := codexBlob(t, "OPERATOR-ACCESS", "OPERATOR-ID", "OPERATOR-REFRESH")
	operatorDone := make(chan error, 1)
	recordAtHandover := make(chan [sha256.Size]byte, 1)
	go func() {
		operatorDone <- store.WithCredentialLock("acct-1", func() error {
			// Runs the instant the materializer releases the lock: whatever the
			// sidecar holds now is what persistBack committed inside its
			// transaction.
			got, ok := m.lastAuthWrite(authPath)
			if !ok {
				close(recordAtHandover)
			} else {
				recordAtHandover <- got
			}
			return store.SaveCredential(context.Background(), "acct-1", operatorCredential)
		})
	}()
	// The operator has attempted the same lock and must be waiting behind the
	// materializer's in-flight RMW.
	waitForSignal(t, store.lockEntered, "operator credential lock attempt")
	close(store.releaseLoad)
	releasedLoad = true

	if err := <-persistDone; err != nil {
		t.Fatalf("persistBack: %v", err)
	}
	if err := <-operatorDone; err != nil {
		t.Fatalf("operator refresh save: %v", err)
	}
	gotRecord, ok := <-recordAtHandover
	if !ok {
		t.Fatal("no durable auth-write record when the lock changed hands; persistBack recorded it outside its transaction")
	}
	if gotRecord != wantRecord {
		t.Fatalf("durable auth-write record at lock handover = %x; want the persisted auth.json hash %x", gotRecord, wantRecord)
	}
	if recorded != wantRecord {
		t.Fatalf("in-memory recorded hash = %x; want %x", recorded, wantRecord)
	}
	got := decodeTokens(t, store.storedBlob())
	if got.AccessToken != "OPERATOR-ACCESS" || got.IDToken != "OPERATOR-ID" || got.RefreshToken != "OPERATOR-REFRESH" {
		t.Fatalf("stored tokens = %+v; want explicit operator refresh", got)
	}
}

// TestMaterializeCodexSerializesLoadAndWriteWithOperatorRefresh proves a
// materialization cannot write a credential snapshot after RefreshAccount has
// saved a newer one. The shared store lock must cover the initial load through
// the auth.json write, not only reconciliation's later merge/save.
func TestMaterializeCodexSerializesLoadAndWriteWithOperatorRefresh(t *testing.T) {
	materialized := codexBlob(t, "A1", "ID-OLD", "R1")
	store := &serializedStore{
		blob:           materialized,
		loadedSnapshot: make(chan struct{}),
		releaseLoad:    make(chan struct{}),
		lockEntered:    make(chan struct{}, 2),
	}
	m := newTestMaterializer(t, store)
	materializeDone := make(chan struct {
		result Materialized
		err    error
	}, 1)
	go func() {
		result, _, err := m.MaterializeCodex(context.Background(), "acct-1")
		materializeDone <- struct {
			result Materialized
			err    error
		}{result, err}
	}()

	// MaterializeCodex must own the shared lock before it reads the credential.
	waitForSignal(t, store.lockEntered, "materializer credential lock")
	waitForSignal(t, store.loadedSnapshot, "materializer load snapshot")

	operatorCredential := codexBlob(t, "OPERATOR-ACCESS", "OPERATOR-ID", "OPERATOR-REFRESH")
	operatorDone := make(chan error, 1)
	go func() {
		operatorDone <- store.WithCredentialLock("acct-1", func() error {
			return store.SaveCredential(context.Background(), "acct-1", operatorCredential)
		})
	}()
	waitForSignal(t, store.lockEntered, "operator credential lock attempt")

	select {
	case err := <-operatorDone:
		t.Fatalf("operator refresh completed before materialization wrote auth.json: %v", err)
	default:
	}
	close(store.releaseLoad)

	mat := <-materializeDone
	if mat.err != nil {
		t.Fatalf("MaterializeCodex: %v", mat.err)
	}
	written, err := os.ReadFile(filepath.Join(mat.result.HomeDir, authFileName))
	if err != nil {
		t.Fatalf("read materialized auth.json: %v", err)
	}
	if !bytes.Equal(written, materialized) {
		t.Fatal("auth.json did not contain the credential loaded by the serialized materialization")
	}
	if err := <-operatorDone; err != nil {
		t.Fatalf("operator refresh save: %v", err)
	}
	got := decodeTokens(t, store.storedBlob())
	if got.AccessToken != "OPERATOR-ACCESS" || got.IDToken != "OPERATOR-ID" || got.RefreshToken != "OPERATOR-REFRESH" {
		t.Fatalf("stored tokens = %+v; want explicit operator refresh", got)
	}
}

func waitForSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// decodeTokens decodes the codex "tokens" object out of a credential blob. Only
// the obviously-fake test constants ever flow through it, and callers compare
// against those constants rather than dumping whole blobs.
func decodeTokens(t *testing.T, blob []byte) tokenFields {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(blob, &top); err != nil {
		t.Fatalf("blob is not a JSON object: %v", err)
	}
	var toks tokenFields
	if err := json.Unmarshal(top["tokens"], &toks); err != nil {
		t.Fatalf("tokens is not a JSON object: %v", err)
	}
	return toks
}

// onDiskTokens decodes the tokens object of the auth.json at path.
func onDiskTokens(t *testing.T, path string) tokenFields {
	t.Helper()
	// #nosec G304 -- test-local temp-dir auth path; owner=@recurser review-by=2027-01-18 issue=BOS-621
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}
	return decodeTokens(t, raw)
}

// savedTokens decodes the tokens object of the last blob handed to the store.
func savedTokens(t *testing.T, store *fakeStore) tokenFields {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.lastSaved == nil {
		t.Fatal("SaveCredential was never called")
	}
	return decodeTokens(t, store.lastSaved)
}

// TestMaterializeCodexPersistsAgentRefreshBeforeOverwrite is the BOS-621
// regression. The spawn seam drops the PersistBack closure, so a refresh codex
// wrote into auth.json mid-run only reaches the store when the NEXT
// materialization reconciles it. Without that reconcile the second materialize
// restores the stale stored token and invalidates the account.
func TestMaterializeCodexPersistsAgentRefreshBeforeOverwrite(t *testing.T) {
	store := &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}
	m := newTestMaterializer(t, store)

	res, _, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	authPath := filepath.Join(res.HomeDir, authFileName)

	// Agent-side rotation: codex writes a new refresh token into auth.json.
	if err := os.WriteFile(authPath, codexBlob(t, fakeAccess, fakeID, "FAKE-REFRESH-2"), 0o600); err != nil {
		t.Fatalf("rewrite auth.json: %v", err)
	}

	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
		t.Fatalf("MaterializeCodex (2nd): %v", err)
	}

	if got := onDiskTokens(t, authPath).RefreshToken; got != "FAKE-REFRESH-2" {
		t.Fatalf("auth.json refresh_token = %q; want the agent-refreshed FAKE-REFRESH-2", got)
	}
	if got := store.saveCount(); got != 1 {
		t.Fatalf("SaveCredential called %d times; want 1 (the reconcile)", got)
	}
	if got := savedTokens(t, store).RefreshToken; got != "FAKE-REFRESH-2" {
		t.Fatalf("stored refresh_token = %q; want the agent-refreshed FAKE-REFRESH-2", got)
	}
}

// TestMaterializeCodexReconcileConvergesAfterFoldBack pins the steady state the
// reconcile leaves behind, which no single-fold test reaches. A fold-back saves
// the merged blob and then re-materializes it, so the third spawn compares a file
// derived from the merged blob against the merged blob itself. If that round trip
// were not the identity — or if the write record were not advanced with it — every
// later spawn would re-fold the same tokens and write the keyring once per spawn
// forever: silently converging on the right value while doing unbounded work.
func TestMaterializeCodexReconcileConvergesAfterFoldBack(t *testing.T) {
	store := &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}
	m := newTestMaterializer(t, store)

	res, _, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	authPath := filepath.Join(res.HomeDir, authFileName)

	if err := os.WriteFile(authPath, codexBlob(t, fakeAccess, fakeID, "FAKE-REFRESH-2"), 0o600); err != nil {
		t.Fatalf("rewrite auth.json: %v", err)
	}
	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
		t.Fatalf("MaterializeCodex (2nd): %v", err)
	}
	if got := store.saveCount(); got != 1 {
		t.Fatalf("SaveCredential called %d times after the fold-back; want 1", got)
	}
	afterFold, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read auth.json after the fold-back: %v", err)
	}

	// A third spawn with nothing changed on either side.
	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
		t.Fatalf("MaterializeCodex (3rd): %v", err)
	}
	if got := store.saveCount(); got != 1 {
		t.Fatalf("SaveCredential called %d times; want 1: the folded bytes must not be folded again", got)
	}
	steady, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read auth.json at the steady state: %v", err)
	}
	if !bytes.Equal(steady, afterFold) {
		// Compare bytes only; the contents are credentials and are never logged.
		t.Fatal("auth.json changed on a no-op materialization; the fold-back did not converge")
	}
	if got := savedTokens(t, store).RefreshToken; got != "FAKE-REFRESH-2" {
		t.Fatalf("stored refresh_token = %q; want the agent-refreshed FAKE-REFRESH-2 preserved", got)
	}
}

// TestMaterializeCodexTwiceWithoutAgentChangeSkipsStoreWrite proves the
// reconcile is byte-gated: a spawn that finds exactly the auth.json it last
// wrote must not touch the keyring.
func TestMaterializeCodexTwiceWithoutAgentChangeSkipsStoreWrite(t *testing.T) {
	store := &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}
	m := newTestMaterializer(t, store)

	for i := 1; i <= 2; i++ {
		if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
			t.Fatalf("MaterializeCodex (%d): %v", i, err)
		}
	}
	if got := store.saveCount(); got != 0 {
		t.Fatalf("SaveCredential called %d times; want 0 (no keyring write per spawn)", got)
	}
}

// TestMaterializeCodexFirstMaterializationSkipsReconcile covers the absent
// auth.json case. The store is primed to fail every save, so a materialization
// that succeeded proves no reconcile was attempted.
func TestMaterializeCodexFirstMaterializationSkipsReconcile(t *testing.T) {
	store := &fakeStore{
		blob:    codexBlob(t, fakeAccess, fakeID, fakeRefresh),
		saveErr: errors.New("store write failed"),
	}
	m := newTestMaterializer(t, store)

	res, _, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	if got := store.saveCount(); got != 0 {
		t.Fatalf("SaveCredential called %d times on first materialization; want 0", got)
	}
	if got := onDiskTokens(t, filepath.Join(res.HomeDir, authFileName)).RefreshToken; got != fakeRefresh {
		t.Fatalf("auth.json refresh_token = %q; want the stored value", got)
	}
}

// TestMaterializeCodexReconcileKeepsNewerStoredIDToken proves the reconcile runs
// through mergePreservingIDToken semantics: an id_token another session already
// persisted is not rolled back by an on-disk file that dropped it.
func TestMaterializeCodexReconcileKeepsNewerStoredIDToken(t *testing.T) {
	store := &fakeStore{blob: codexBlob(t, fakeAccess, "FAKE-ID-OLD", fakeRefresh)}
	m := newTestMaterializer(t, store)

	res, _, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	authPath := filepath.Join(res.HomeDir, authFileName)

	// Our agent rotates the refresh token and drops id_token entirely.
	if err := os.WriteFile(authPath, codexBlob(t, fakeAccess, "", "FAKE-REFRESH-2"), 0o600); err != nil {
		t.Fatalf("rewrite auth.json: %v", err)
	}
	// Meanwhile another session persists a NEWER id_token for the same account.
	store.setBlob(codexBlob(t, fakeAccess, "FAKE-ID-NEW", fakeRefresh))

	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
		t.Fatalf("MaterializeCodex (2nd): %v", err)
	}

	saved := savedTokens(t, store)
	if saved.IDToken != "FAKE-ID-NEW" {
		t.Fatalf("stored id_token = %q; want current-store FAKE-ID-NEW (no rollback)", saved.IDToken)
	}
	if saved.RefreshToken != "FAKE-REFRESH-2" {
		t.Fatalf("stored refresh_token = %q; want the agent-refreshed FAKE-REFRESH-2", saved.RefreshToken)
	}
	onDisk := onDiskTokens(t, authPath)
	if onDisk.IDToken != "FAKE-ID-NEW" || onDisk.RefreshToken != "FAKE-REFRESH-2" {
		t.Fatalf("auth.json tokens = %+v; want the reconciled pair", onDisk)
	}
}

// TestMaterializeCodexReconcileKeepsLaterOperatorRefresh covers the other half
// of the both-sides-moved case: the agent rotated auth.json AND the store moved,
// but the store moved second. The hash record cannot tell those apart — it only
// proves the file changed — so the credential's own "last_refresh" marker orders
// them, and the later operator refresh stays authoritative instead of being
// overwritten by the older on-disk tokens.
func TestMaterializeCodexReconcileKeepsLaterOperatorRefresh(t *testing.T) {
	store := &fakeStore{blob: codexBlobAt(t, fakeAccess, fakeID, fakeRefresh, "2026-01-01T00:00:00Z")}
	m := newTestMaterializer(t, store)

	res, _, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	authPath := filepath.Join(res.HomeDir, authFileName)

	// Codex rotates the tokens in auth.json mid-session, so the file no longer
	// matches the hash bossd recorded for its own last write.
	agentBlob := codexBlobAt(t, "FAKE-ACCESS-AGENT", fakeID, "FAKE-REFRESH-AGENT", "2026-01-01T01:00:00Z")
	if err := os.WriteFile(authPath, agentBlob, 0o600); err != nil {
		t.Fatalf("rewrite auth.json: %v", err)
	}
	// AFTERWARDS an operator completes RefreshAccount, which saves a brand-new
	// credential to the store and never touches auth.json or the write record.
	store.setBlob(codexBlobAt(t, "FAKE-ACCESS-OP", "FAKE-ID-OP", "FAKE-REFRESH-OP", "2026-01-01T02:00:00Z"))
	savesBefore := store.saveCount()

	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
		t.Fatalf("MaterializeCodex (2nd): %v", err)
	}

	if got := store.saveCount(); got != savesBefore {
		t.Fatalf("SaveCredential calls = %d; want %d (the operator credential must not be merged over)", got, savesBefore)
	}
	stored := decodeTokens(t, store.currentBlob())
	if stored.AccessToken != "FAKE-ACCESS-OP" || stored.IDToken != "FAKE-ID-OP" || stored.RefreshToken != "FAKE-REFRESH-OP" {
		t.Fatalf("stored tokens = %+v; want the operator credential untouched", stored)
	}
	onDisk := onDiskTokens(t, authPath)
	if onDisk.AccessToken != "FAKE-ACCESS-OP" || onDisk.IDToken != "FAKE-ID-OP" || onDisk.RefreshToken != "FAKE-REFRESH-OP" {
		t.Fatalf("auth.json tokens = %+v; want the operator credential materialized", onDisk)
	}
}

// TestMaterializeCodexReconcileFoldsBackWhenOnDiskRefreshIsLater is the mirror
// of the test above: same both-sides-moved shape, but the FILE carries the later
// marker, so the fold-back still runs and the agent's refresh token reaches the
// store. Ordering must decide the direction, not merely suppress the fold.
func TestMaterializeCodexReconcileFoldsBackWhenOnDiskRefreshIsLater(t *testing.T) {
	store := &fakeStore{blob: codexBlobAt(t, fakeAccess, fakeID, fakeRefresh, "2026-01-01T00:00:00Z")}
	m := newTestMaterializer(t, store)

	res, _, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	authPath := filepath.Join(res.HomeDir, authFileName)

	store.setBlob(codexBlobAt(t, fakeAccess, "FAKE-ID-PEER", fakeRefresh, "2026-01-01T01:00:00Z"))
	agentBlob := codexBlobAt(t, "FAKE-ACCESS-AGENT", "", "FAKE-REFRESH-AGENT", "2026-01-01T02:00:00Z")
	if err := os.WriteFile(authPath, agentBlob, 0o600); err != nil {
		t.Fatalf("rewrite auth.json: %v", err)
	}

	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
		t.Fatalf("MaterializeCodex (2nd): %v", err)
	}

	saved := savedTokens(t, store)
	if saved.RefreshToken != "FAKE-REFRESH-AGENT" {
		t.Fatalf("stored refresh_token = %q; want the agent-refreshed FAKE-REFRESH-AGENT", saved.RefreshToken)
	}
	if saved.IDToken != "FAKE-ID-PEER" {
		t.Fatalf("stored id_token = %q; want the store-only FAKE-ID-PEER to survive the merge", saved.IDToken)
	}
}

func TestStoredCredentialIsNewer(t *testing.T) {
	const (
		early = "2026-01-01T00:00:00Z"
		late  = "2026-01-01T01:00:00Z"
	)
	withStamp := func(stamp string) []byte {
		return codexBlobAt(t, fakeAccess, fakeID, fakeRefresh, stamp)
	}
	noStamp := codexBlob(t, fakeAccess, fakeID, fakeRefresh)

	for _, tc := range []struct {
		name           string
		stored, onDisk []byte
		want           bool
	}{
		{name: "store rotated later", stored: withStamp(late), onDisk: withStamp(early), want: true},
		{name: "file rotated later", stored: withStamp(early), onDisk: withStamp(late)},
		{name: "same generation", stored: withStamp(early), onDisk: withStamp(early)},
		// Unordered pairs must all report false: without a marker on both sides
		// there is no proof the store moved second, and the fold-back is what
		// preserves an agent-rotated refresh token.
		{name: "store has no marker", stored: noStamp, onDisk: withStamp(early)},
		{name: "file has no marker", stored: withStamp(late), onDisk: noStamp},
		{name: "neither has a marker", stored: noStamp, onDisk: noStamp},
		{name: "store marker is not a timestamp", stored: withStamp("whenever"), onDisk: withStamp(early)},
		{name: "store marker is empty", stored: withStamp(""), onDisk: withStamp(early)},
		{name: "store is not JSON", stored: []byte("not json"), onDisk: withStamp(early)},
		{name: "file is not JSON", stored: withStamp(late), onDisk: []byte("not json")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := storedCredentialIsNewer(tc.stored, tc.onDisk); got != tc.want {
				t.Fatalf("storedCredentialIsNewer = %v; want %v", got, tc.want)
			}
		})
	}
}

// TestMaterializeCodexReconcileStoreErrorLeavesAuthFileIntact asserts the
// fail-closed contract: a reconcile that cannot reach the store aborts
// materialization rather than overwriting a possibly-valid refreshed token.
func TestMaterializeCodexReconcileStoreErrorLeavesAuthFileIntact(t *testing.T) {
	store := &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}
	m := newTestMaterializer(t, store)

	res, _, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	authPath := filepath.Join(res.HomeDir, authFileName)
	rotated := codexBlob(t, fakeAccess, fakeID, "FAKE-REFRESH-2")
	if err := os.WriteFile(authPath, rotated, 0o600); err != nil {
		t.Fatalf("rewrite auth.json: %v", err)
	}
	store.setSaveErr(errors.New("store write failed"))

	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err == nil {
		t.Fatal("MaterializeCodex succeeded despite a failing reconcile save")
	}
	// #nosec G304 -- test-local temp-dir auth path; owner=@recurser review-by=2027-01-18 issue=BOS-621
	after, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}
	if string(after) != string(rotated) {
		t.Fatal("MaterializeCodex modified the refreshed auth.json after a reconcile failure")
	}
}

// TestMaterializeCodexReconcileUnusableAuthFileSelfHeals pins resilience the
// reconcile must not cost us. Before it existed MaterializeCodex overwrote
// auth.json unconditionally, so a file truncated by a killed codex healed on the
// next spawn. Bytes that are not a usable credential object hold no refreshed
// token to preserve, so the reconcile must skip them — folding them into the
// store would be wrong, and aborting would fail this materialization and every
// later one identically, since nothing else ever rewrites auth.json.
func TestMaterializeCodexReconcileUnusableAuthFileSelfHeals(t *testing.T) {
	store := &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}
	m := newTestMaterializer(t, store)

	res, _, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	authPath := filepath.Join(res.HomeDir, authFileName)

	for _, unusable := range []string{
		"",                             // zero-byte file
		`{"tokens":{"access_token":"F`, // truncated mid-write
		`{"tokens":"not-an-object"}`,   // parses as an object, tokens is not one
		`["not-an-object"]`,            // parses as JSON, not as an object
	} {
		if err := os.WriteFile(authPath, []byte(unusable), 0o600); err != nil {
			t.Fatalf("write unusable auth.json: %v", err)
		}
		before := store.saveCount()
		if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
			t.Fatalf("MaterializeCodex over an unusable auth.json (%d bytes): %v", len(unusable), err)
		}
		if got := onDiskTokens(t, authPath); got.RefreshToken != fakeRefresh || got.IDToken != fakeID {
			t.Fatal("MaterializeCodex did not replace the unusable auth.json with the stored credential")
		}
		if extra := store.saveCount() - before; extra != 0 {
			t.Fatalf("SaveCredential calls = %d; want 0: unusable bytes must never reach the store", extra)
		}
	}
}

// TestMaterializeCodexReconcileIgnoresNonRegularAuthFile pins the leaf guard.
// assertNoSymlinkChain covers directory components only, and before the reconcile
// existed the leaf was merely rename-replaced, which never follows a symlink.
// Reading it blind would fold a FOREIGN credential into this account's store
// entry; the same guard keeps a FIFO from blocking ReadFile forever while the
// per-account lock is held.
func TestMaterializeCodexReconcileIgnoresNonRegularAuthFile(t *testing.T) {
	store := &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}
	m := newTestMaterializer(t, store)

	res, _, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	authPath := filepath.Join(res.HomeDir, authFileName)

	// A credential belonging to some other account, outside this account's tree and
	// reachable only by following the symlink.
	foreignBlob := codexBlob(t, "FAKE-FOREIGN-ACCESS", "FAKE-FOREIGN-ID", "FAKE-FOREIGN-REFRESH")
	foreignPath := filepath.Join(t.TempDir(), "foreign-auth.json")
	if err := os.WriteFile(foreignPath, foreignBlob, 0o600); err != nil {
		t.Fatalf("write foreign credential: %v", err)
	}
	if err := os.Remove(authPath); err != nil {
		t.Fatalf("remove auth.json: %v", err)
	}
	if err := os.Symlink(foreignPath, authPath); err != nil {
		t.Fatalf("symlink auth.json: %v", err)
	}

	before := store.saveCount()
	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
		t.Fatalf("MaterializeCodex over a symlinked auth.json: %v", err)
	}
	if extra := store.saveCount() - before; extra != 0 {
		t.Fatalf("SaveCredential calls = %d; want 0: a symlink target must never be folded into the account", extra)
	}
	info, err := os.Lstat(authPath)
	if err != nil {
		t.Fatalf("lstat auth.json: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatal("MaterializeCodex left auth.json a symlink instead of replacing it with a regular file")
	}
	if got := onDiskTokens(t, authPath); got.RefreshToken != fakeRefresh {
		t.Fatal("auth.json does not hold the stored credential after the symlink was replaced")
	}
	// The atomic write renames over the link, so the foreign file is untouched.
	// #nosec G304 -- test-local temp-dir path; owner=@recurser review-by=2027-01-18 issue=BOS-621
	afterForeign, err := os.ReadFile(foreignPath)
	if err != nil {
		t.Fatalf("read foreign credential: %v", err)
	}
	if string(afterForeign) != string(foreignBlob) {
		t.Fatal("MaterializeCodex wrote through the symlink instead of replacing it")
	}
}

// TestMaterializeCodexReconcileIgnoresHardLinkedAuthFile prevents a restored
// account tree from folding a foreign credential into this account's store. A
// hard link is regular, so Lstat alone cannot distinguish it from an
// account-local auth.json; atomicWriteFile must replace only this directory
// entry, leaving the foreign credential untouched.
func TestMaterializeCodexReconcileIgnoresHardLinkedAuthFile(t *testing.T) {
	store := &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}
	m := newTestMaterializer(t, store)

	res, _, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	authPath := filepath.Join(res.HomeDir, authFileName)

	foreignBlob := codexBlob(t, "FAKE-FOREIGN-ACCESS", "FAKE-FOREIGN-ID", "FAKE-FOREIGN-REFRESH")
	foreignPath := filepath.Join(t.TempDir(), "foreign-auth.json")
	if err := os.WriteFile(foreignPath, foreignBlob, 0o600); err != nil {
		t.Fatalf("write foreign credential: %v", err)
	}
	if err := os.Remove(authPath); err != nil {
		t.Fatalf("remove auth.json: %v", err)
	}
	if err := os.Link(foreignPath, authPath); err != nil {
		t.Skipf("hard links unavailable in test filesystem: %v", err)
	}

	before := store.saveCount()
	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
		t.Fatalf("MaterializeCodex over a hard-linked auth.json: %v", err)
	}
	if extra := store.saveCount() - before; extra != 0 {
		t.Fatalf("SaveCredential calls = %d; want 0: a hard-linked credential must never be folded into the account", extra)
	}
	if got := onDiskTokens(t, authPath); got.RefreshToken != fakeRefresh {
		t.Fatal("auth.json does not hold the stored credential after the hard link was replaced")
	}

	// The atomic write renames over this link only, preserving the foreign file.
	// #nosec G304 -- test-local temp-dir path; owner=@recurser review-by=2027-01-18 issue=BOS-621
	afterForeign, err := os.ReadFile(foreignPath)
	if err != nil {
		t.Fatalf("read foreign credential: %v", err)
	}
	if string(afterForeign) != string(foreignBlob) {
		t.Fatal("MaterializeCodex modified the foreign credential through the hard link")
	}
	foreignInfo, err := os.Stat(foreignPath)
	if err != nil {
		t.Fatalf("stat foreign credential: %v", err)
	}
	authInfo, err := os.Stat(authPath)
	if err != nil {
		t.Fatalf("stat auth.json: %v", err)
	}
	if os.SameFile(authInfo, foreignInfo) {
		t.Fatal("MaterializeCodex left auth.json hard-linked to the foreign credential")
	}
}

// TestMaterializeCodexReconcileKeepsOperatorRefreshedCredential is the inverse
// of the BOS-621 regression, and the reason the reconcile cannot key on "the
// file differs from the store". An operator refresh (server.RefreshAccount)
// saves a new blob and nothing invalidates the already-materialized auth.json,
// so the next materialization finds a file that differs from the store while
// holding the tokens the operator just replaced. Folding it back would save
// those stale tokens over the fresh credential and defeat every later refresh
// identically.
func TestMaterializeCodexReconcileKeepsOperatorRefreshedCredential(t *testing.T) {
	store := &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}
	m := newTestMaterializer(t, store)

	res, _, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	authPath := filepath.Join(res.HomeDir, authFileName)

	// The operator re-authenticates: a wholly new credential lands in the store
	// while auth.json still holds exactly what bossd last wrote.
	store.setBlob(codexBlob(t, "FAKE-ACCESS-2", "FAKE-ID-2", "FAKE-REFRESH-2"))

	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
		t.Fatalf("MaterializeCodex (2nd): %v", err)
	}

	if got := store.saveCount(); got != 0 {
		t.Fatalf("SaveCredential called %d times; want 0: the stale auth.json must not be folded over an operator refresh", got)
	}
	if got := onDiskTokens(t, authPath); got.RefreshToken != "FAKE-REFRESH-2" || got.IDToken != "FAKE-ID-2" {
		t.Fatalf("auth.json tokens = %+v; want the operator-refreshed credential", got)
	}
}

// TestMaterializeCodexReconcileWithoutWriteRecordOverwritesFromStore pins the
// fallback for an account materialized before the write record existed (or whose
// sidecar could not be written). Nothing on disk can then say whether the file or
// the store moved, so the reconcile must behave as this package did before it
// existed — overwrite from the store — and re-record the hash so the very next
// materialization can tell the two apart again.
func TestMaterializeCodexReconcileWithoutWriteRecordOverwritesFromStore(t *testing.T) {
	store := &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}
	m := newTestMaterializer(t, store)

	res, _, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	authPath := filepath.Join(res.HomeDir, authFileName)
	hashPath := filepath.Join(res.HomeDir, authHashFileName)

	if runtime.GOOS != "windows" {
		info, statErr := os.Stat(hashPath)
		if statErr != nil {
			t.Fatalf("stat write record: %v", statErr)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("write record perm = %o; want 0600", perm)
		}
	}

	// A pre-sidecar account: auth.json rotated by the agent, no record of our write.
	if err := os.WriteFile(authPath, codexBlob(t, fakeAccess, fakeID, "FAKE-REFRESH-2"), 0o600); err != nil {
		t.Fatalf("rewrite auth.json: %v", err)
	}
	if err := os.Remove(hashPath); err != nil {
		t.Fatalf("remove write record: %v", err)
	}

	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
		t.Fatalf("MaterializeCodex (2nd): %v", err)
	}
	if got := store.saveCount(); got != 0 {
		t.Fatalf("SaveCredential called %d times; want 0 without a write record", got)
	}
	if got := onDiskTokens(t, authPath).RefreshToken; got != fakeRefresh {
		t.Fatalf("auth.json refresh_token = %q; want the stored value", got)
	}

	// Recorded again, so an agent rotation from here IS reconciled.
	if err := os.WriteFile(authPath, codexBlob(t, fakeAccess, fakeID, "FAKE-REFRESH-3"), 0o600); err != nil {
		t.Fatalf("rewrite auth.json (2nd): %v", err)
	}
	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
		t.Fatalf("MaterializeCodex (3rd): %v", err)
	}
	if got := store.saveCount(); got != 1 {
		t.Fatalf("SaveCredential called %d times; want 1 once the write record is back", got)
	}
	if got := savedTokens(t, store).RefreshToken; got != "FAKE-REFRESH-3" {
		t.Fatalf("stored refresh_token = %q; want the agent-refreshed FAKE-REFRESH-3", got)
	}
}

// TestMaterializeCodexReconcileIgnoresNonRegularWriteRecord keeps the sidecar
// read on the same footing as the auth.json leaf: it is only ever
// rename-written, so a non-regular entry is not ours, and reading one blind
// could block forever on a writerless FIFO while the account lock is held.
// Refusing it degrades to the no-record path, which never folds anything back.
func TestMaterializeCodexReconcileIgnoresNonRegularWriteRecord(t *testing.T) {
	store := &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}
	m := newTestMaterializer(t, store)

	res, _, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	authPath := filepath.Join(res.HomeDir, authFileName)
	hashPath := filepath.Join(res.HomeDir, authHashFileName)

	// A record that is a directory: not a regular file, and not readable as one.
	if err := os.Remove(hashPath); err != nil {
		t.Fatalf("remove write record: %v", err)
	}
	if err := os.Mkdir(hashPath, 0o700); err != nil {
		t.Fatalf("mkdir over write record: %v", err)
	}
	if err := os.WriteFile(authPath, codexBlob(t, fakeAccess, fakeID, "FAKE-REFRESH-2"), 0o600); err != nil {
		t.Fatalf("rewrite auth.json: %v", err)
	}

	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
		t.Fatalf("MaterializeCodex over a non-regular write record: %v", err)
	}
	if got := store.saveCount(); got != 0 {
		t.Fatalf("SaveCredential called %d times; want 0 for an unusable write record", got)
	}

	// Degrading to the no-record path must not be permanent. A rename cannot
	// replace the directory, so that materialization has to clear it and record
	// the write it just made; leaving the sidecar absent would strand the account
	// on the no-record path, where the NEXT agent rotation is overwritten from the
	// store — the loss this record exists to prevent.
	info, err := os.Lstat(hashPath)
	if err != nil {
		t.Fatalf("lstat write record after a non-regular one: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("write record mode = %v; want a regular file restored over the directory", info.Mode())
	}

	// Prove it by rotating auth.json once more: with a usable record again, the
	// refresh folds back instead of being overwritten.
	if err := os.WriteFile(authPath, codexBlob(t, fakeAccess, fakeID, "FAKE-REFRESH-3"), 0o600); err != nil {
		t.Fatalf("rewrite auth.json after the record was restored: %v", err)
	}
	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
		t.Fatalf("MaterializeCodex after the write record was restored: %v", err)
	}
	if got := store.saveCount(); got != 1 {
		t.Fatalf("SaveCredential called %d times; want 1 once the record was restored", got)
	}
	if got := savedTokens(t, store).RefreshToken; got != "FAKE-REFRESH-3" {
		t.Fatalf("stored refresh_token = %q; want the agent-refreshed FAKE-REFRESH-3", got)
	}
}

// TestMaterializeCodexReconcileIgnoresHardLinkedWriteRecord prevents a restored
// account tree from trusting another account's last-write digest. A hard link is
// regular, but its digest must be treated as absent: otherwise a foreign digest
// that differs from the account auth.json reads as an agent-side refresh and
// folds that credential into the current account's store.
func TestMaterializeCodexReconcileIgnoresHardLinkedWriteRecord(t *testing.T) {
	store := &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}
	m := newTestMaterializer(t, store)

	res, _, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	authPath := filepath.Join(res.HomeDir, authFileName)
	hashPath := authHashPath(authPath)
	if err := os.WriteFile(authPath, codexBlob(t, fakeAccess, "", "FAKE-REFRESH-2"), 0o600); err != nil {
		t.Fatalf("rewrite auth.json: %v", err)
	}

	foreignRecord := filepath.Join(t.TempDir(), authHashFileName)
	foreignDigest := sha256.Sum256([]byte("foreign auth.json"))
	if err := os.WriteFile(foreignRecord, []byte(hex.EncodeToString(foreignDigest[:])), 0o600); err != nil {
		t.Fatalf("write foreign write record: %v", err)
	}
	if err := os.Remove(hashPath); err != nil {
		t.Fatalf("remove account write record: %v", err)
	}
	if err := os.Link(foreignRecord, hashPath); err != nil {
		t.Skipf("hard links unavailable in test filesystem: %v", err)
	}

	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
		t.Fatalf("MaterializeCodex over a hard-linked write record: %v", err)
	}
	if got := store.saveCount(); got != 0 {
		t.Fatalf("SaveCredential called %d times; want 0: a hard-linked write record must not drive a fold-back", got)
	}
	if got := onDiskTokens(t, authPath).RefreshToken; got != fakeRefresh {
		t.Fatalf("on-disk refresh_token = %q; want the stored credential restored", got)
	}

	// The atomic write replaces only the local sidecar entry, preserving the
	// foreign record and removing the hard-link alias.
	afterForeign, err := os.ReadFile(foreignRecord)
	if err != nil {
		t.Fatalf("read foreign write record: %v", err)
	}
	if string(afterForeign) != hex.EncodeToString(foreignDigest[:]) {
		t.Fatal("MaterializeCodex modified the foreign write record through the hard link")
	}
	foreignInfo, err := os.Stat(foreignRecord)
	if err != nil {
		t.Fatalf("stat foreign write record: %v", err)
	}
	hashInfo, err := os.Stat(hashPath)
	if err != nil {
		t.Fatalf("stat account write record: %v", err)
	}
	if os.SameFile(hashInfo, foreignInfo) {
		t.Fatal("MaterializeCodex left the write record hard-linked to the foreign record")
	}
}

func TestMergePreservingIDToken(t *testing.T) {
	tests := []struct {
		name    string
		prev    string
		next    string
		wantTok map[string]string
		wantTop map[string]string // extra top-level string fields expected
		// wantsErr expects a merge failure; wantIncoming then asserts which SIDE the
		// failure is attributed to via errIncomingUnusable. That attribution is what
		// makes reconcileRefreshedAuth safe to continue past a garbage auth.json
		// while still aborting on a corrupt stored blob, so it is pinned in both
		// directions: a mis-wrap either way silently turns one behavior into the
		// other, and no other test would notice.
		wantsErr     bool
		wantIncoming bool
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
			// Attributed to the STORE: a caller that read next off disk must abort
			// rather than treat this as "the file is garbage" and overwrite the only
			// valid credential left in the system.
			name:     "invalid prev JSON errors",
			prev:     `not json`,
			next:     `{"tokens":{}}`,
			wantsErr: true,
		},
		{
			name:         "invalid next JSON errors",
			prev:         `{"tokens":{}}`,
			next:         `not json`,
			wantsErr:     true,
			wantIncoming: true,
		},
		{
			// The second prev-side path: a non-object tokens with no top-level
			// secrets to rebuild it from, so normalization leaves the array in place.
			name:     "non-object prev tokens errors",
			prev:     `{"tokens":[1,2,3]}`,
			next:     `{"tokens":{}}`,
			wantsErr: true,
		},
		{
			name:         "non-object next tokens errors",
			prev:         `{"tokens":{}}`,
			next:         `{"tokens":[1,2,3]}`,
			wantsErr:     true,
			wantIncoming: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := mergePreservingIDToken([]byte(tc.prev), []byte(tc.next))
			if tc.wantsErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if got := errors.Is(err, errIncomingUnusable); got != tc.wantIncoming {
					t.Fatalf("errors.Is(err, errIncomingUnusable) = %t; want %t: the failure is attributed to the wrong blob", got, tc.wantIncoming)
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
	t.Setenv("CODEX_HOME", t.TempDir())
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

// End-to-end-ish purge assertion for the account-removal path (BOS-622): after
// materialize → remove, the per-account dir and the plaintext auth.json in it are
// absent, while the shared accounts tree (and its CACHEDIR.TAG) survives — the
// purge is scoped to the one account the materializer derived, nothing wider.
func TestRemoveAccountPurgesMaterializedAuthFile(t *testing.T) {
	store := &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}
	m := newTestMaterializer(t, store)

	res, _, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	authPath := filepath.Join(res.HomeDir, authFileName)
	if _, err := os.Stat(authPath); err != nil {
		t.Fatalf("auth.json missing before removal: %v", err)
	}

	if err := m.RemoveAccount(context.Background(), providerCodex, "acct-1"); err != nil {
		t.Fatalf("RemoveAccount: %v", err)
	}
	if _, err := os.Stat(authPath); !os.IsNotExist(err) {
		t.Fatalf("auth.json survived removal: %v", err)
	}
	if _, err := os.Stat(res.HomeDir); !os.IsNotExist(err) {
		t.Fatalf("codex account dir survived removal: %v", err)
	}
	for _, keep := range []string{m.accountsRoot(), filepath.Join(m.accountsRoot(), cacheDirTagName)} {
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("removal widened past the account dir and deleted %q: %v", keep, err)
		}
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
	t.Setenv("CODEX_HOME", t.TempDir())
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
	t.Setenv("CODEX_HOME", t.TempDir())
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
	t.Setenv("CODEX_HOME", t.TempDir())
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

// projectedOnce materializes acct-1 once against baseHome and returns the
// materializer plus the account home, asserting name was projected as a symlink.
// It is the shared setup for the became-unsafe cases below: the projection must
// exist before the base entry turns unsafe, or the test would pass vacuously.
func projectedOnce(t *testing.T, baseHome, name string) (*Materializer, string) {
	t.Helper()
	m := newTestMaterializerWithCodexHome(t, &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}, baseHome)
	res, _, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("first MaterializeCodex: %v", err)
	}
	info, err := os.Lstat(filepath.Join(res.HomeDir, name))
	if err != nil {
		t.Fatalf("lstat initial projection %q: %v", name, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("initial projection %q is not a symlink", name)
	}
	return m, res.HomeDir
}

func TestRemoveStaleProjectionContract(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	t.Run("absent is a no-op", func(t *testing.T) {
		if err := removeStaleProjection(filepath.Join(t.TempDir(), "skills"), t.TempDir()); err != nil {
			t.Fatalf("removeStaleProjection on absent path: %v", err)
		}
	})
	t.Run("symlink is unlinked without following it", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target")
		if err := os.MkdirAll(target, 0o700); err != nil {
			t.Fatalf("mkdir target: %v", err)
		}
		link := filepath.Join(dir, "skills")
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("create link: %v", err)
		}
		if err := removeStaleProjection(link, dir); err != nil {
			t.Fatalf("removeStaleProjection: %v", err)
		}
		if _, err := os.Lstat(link); !os.IsNotExist(err) {
			t.Fatalf("projection still present: %v", err)
		}
		if _, err := os.Stat(target); err != nil {
			t.Fatalf("link target was followed and removed: %v", err)
		}
	})
	t.Run("foreign non-symlink state is refused, never deleted", func(t *testing.T) {
		dir := t.TempDir()
		for _, tc := range []struct {
			name  string
			setup func(path string)
		}{
			{"regular file", func(path string) {
				if err := os.WriteFile(path, []byte("foreign"), 0o600); err != nil {
					t.Fatalf("write foreign file: %v", err)
				}
			}},
			{"real directory", func(path string) {
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatalf("mkdir foreign dir: %v", err)
				}
			}},
		} {
			path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-"))
			tc.setup(path)
			if err := removeStaleProjection(path, dir); err == nil {
				t.Fatalf("%s: removeStaleProjection accepted foreign state", tc.name)
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatalf("%s: foreign state was deleted: %v", tc.name, err)
			}
		}
	})
	t.Run("auth.json is refused by name", func(t *testing.T) {
		dir := t.TempDir()
		// Refused whether it is the account-local credential or a symlink: the
		// account credential is never a projection, so the helper never unlinks it.
		authPath := filepath.Join(dir, authFileName)
		if err := os.WriteFile(authPath, []byte("local-auth"), 0o600); err != nil {
			t.Fatalf("write account auth: %v", err)
		}
		if err := removeStaleProjection(authPath, dir); err == nil {
			t.Fatal("removeStaleProjection accepted account auth.json")
		}
		if _, err := os.Lstat(authPath); err != nil {
			t.Fatalf("account auth.json was deleted: %v", err)
		}

		linkDir := t.TempDir()
		linkedAuth := filepath.Join(linkDir, authFileName)
		if err := os.Symlink(authPath, linkedAuth); err != nil {
			t.Fatalf("create auth symlink: %v", err)
		}
		if err := removeStaleProjection(linkedAuth, linkDir); err == nil {
			t.Fatal("removeStaleProjection accepted a symlinked auth.json")
		}
		if _, err := os.Lstat(linkedAuth); err != nil {
			t.Fatalf("symlinked auth.json was deleted: %v", err)
		}
	})
	t.Run("a symlink this package did not create is refused, never deleted", func(t *testing.T) {
		// ensureProjectedSymlink already refuses to retarget a link pointing outside
		// the base home; withdrawal holds the same line, so an operator's own
		// account-home symlink is never collateral damage of a stale base entry.
		base := t.TempDir()
		accountHome := t.TempDir()
		elsewhere := t.TempDir()
		for _, tc := range []struct {
			name   string
			target string
		}{
			{"absolute target outside the base home", elsewhere},
			{"relative link text", "../elsewhere"},
		} {
			link := filepath.Join(accountHome, strings.ReplaceAll(tc.name, " ", "-"))
			if err := os.Symlink(tc.target, link); err != nil {
				t.Fatalf("%s: create foreign link: %v", tc.name, err)
			}
			if err := removeStaleProjection(link, base); err == nil {
				t.Fatalf("%s: removeStaleProjection accepted a foreign symlink", tc.name)
			}
			if _, err := os.Lstat(link); err != nil {
				t.Fatalf("%s: foreign symlink was deleted: %v", tc.name, err)
			}
		}
	})
	t.Run("a dangling in-base projection is still withdrawn", func(t *testing.T) {
		// The base entry disappearing is precisely when withdrawal must happen, so
		// provenance is proved from link text alone rather than by resolving it.
		base := t.TempDir()
		link := filepath.Join(t.TempDir(), "skills")
		if err := os.Symlink(filepath.Join(base, "skills"), link); err != nil {
			t.Fatalf("create dangling link: %v", err)
		}
		if err := removeStaleProjection(link, base); err != nil {
			t.Fatalf("removeStaleProjection on dangling projection: %v", err)
		}
		if _, err := os.Lstat(link); !os.IsNotExist(err) {
			t.Fatalf("dangling projection still present: %v", err)
		}
	})
}

// TestMaterializeCodexRemovesProjectionWhenOptionalBaseEntryBecomesExternalSymlink
// is the regression: a safe entry is projected, its base-home entry is then
// replaced with a symlink outside the base home (the !projectable branch), and
// the next materialization must withdraw the projection instead of leaving the
// account home resolving through it.
func TestMaterializeCodexRemovesProjectionWhenOptionalBaseEntryBecomesExternalSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	baseHome := t.TempDir()
	skillsPath := filepath.Join(baseHome, "skills")
	if err := os.MkdirAll(skillsPath, 0o700); err != nil {
		t.Fatalf("mkdir base skills: %v", err)
	}
	m, accountHome := projectedOnce(t, baseHome, "skills")

	external := t.TempDir()
	if err := os.RemoveAll(skillsPath); err != nil {
		t.Fatalf("remove safe base skills: %v", err)
	}
	if err := os.Symlink(external, skillsPath); err != nil {
		t.Fatalf("replace base skills with external symlink: %v", err)
	}

	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
		t.Fatalf("second MaterializeCodex: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(accountHome, "skills")); !os.IsNotExist(err) {
		t.Fatalf("stale projection survived a base entry that became external: %v", err)
	}
	if _, err := os.Stat(external); err != nil {
		t.Fatalf("external base target was removed through the projection: %v", err)
	}
	authInfo, err := os.Lstat(filepath.Join(accountHome, authFileName))
	if err != nil {
		t.Fatalf("lstat account auth: %v", err)
	}
	if authInfo.Mode()&os.ModeSymlink != 0 {
		t.Fatal("account auth.json must remain local after withdrawing a projection")
	}
}

// TestMaterializeCodexRemovesProjectionWhenOptionalBaseEntryTargetDisappears
// covers the other half of the !projectable branch: the base entry is still
// listed by os.ReadDir but no longer resolves, so it is not projectable.
func TestMaterializeCodexRemovesProjectionWhenOptionalBaseEntryTargetDisappears(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	baseHome := t.TempDir()
	target := filepath.Join(baseHome, "skills-v1")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatalf("mkdir base skills target: %v", err)
	}
	if err := os.Symlink("skills-v1", filepath.Join(baseHome, "skills")); err != nil {
		t.Fatalf("create base skills symlink: %v", err)
	}
	m, accountHome := projectedOnce(t, baseHome, "skills")

	if err := os.RemoveAll(target); err != nil {
		t.Fatalf("delete base skills target: %v", err)
	}

	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
		t.Fatalf("second MaterializeCodex: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(accountHome, "skills")); !os.IsNotExist(err) {
		t.Fatalf("stale projection survived a base entry that stopped resolving: %v", err)
	}
}

// TestMaterializeCodexRemovesProjectionWhenNestedOptionalSymlinkBecomesExternal
// covers the second skipping branch: the entry itself still resolves inside the
// base home, but a nested symlink now leaves it (errSymlinkOutsideBase).
func TestMaterializeCodexRemovesProjectionWhenNestedOptionalSymlinkBecomesExternal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	baseHome := t.TempDir()
	skillsPath := filepath.Join(baseHome, "skills")
	if err := os.MkdirAll(skillsPath, 0o700); err != nil {
		t.Fatalf("mkdir base skills: %v", err)
	}
	m, accountHome := projectedOnce(t, baseHome, "skills")

	if err := os.Symlink(t.TempDir(), filepath.Join(skillsPath, "external-skill")); err != nil {
		t.Fatalf("add nested external skills symlink: %v", err)
	}

	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
		t.Fatalf("second MaterializeCodex: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(accountHome, "skills")); !os.IsNotExist(err) {
		t.Fatalf("stale projection survived a nested symlink leaving the base home: %v", err)
	}
}

// TestMaterializeCodexKeepsFailingClosedWhenRequiredBaseEntryBecomesUnsafe
// pins the unchanged behaviour for non-optional entries: they fail
// materialization rather than being withdrawn.
func TestMaterializeCodexKeepsFailingClosedWhenRequiredBaseEntryBecomesUnsafe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	for _, name := range []string{"config.toml", "plugins"} {
		t.Run(name, func(t *testing.T) {
			baseHome := t.TempDir()
			entryPath := filepath.Join(baseHome, name)
			if err := os.MkdirAll(entryPath, 0o700); err != nil {
				t.Fatalf("mkdir base %q: %v", name, err)
			}
			m, accountHome := projectedOnce(t, baseHome, name)

			if err := os.RemoveAll(entryPath); err != nil {
				t.Fatalf("remove safe base %q: %v", name, err)
			}
			if err := os.Symlink(t.TempDir(), entryPath); err != nil {
				t.Fatalf("replace base %q with external symlink: %v", name, err)
			}

			if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err == nil {
				t.Fatalf("MaterializeCodex accepted an unsafe required entry %q", name)
			}
			if _, err := os.Lstat(filepath.Join(accountHome, name)); err != nil {
				t.Fatalf("required entry %q was withdrawn instead of failing closed: %v", name, err)
			}
		})
	}
}

// TestMaterializeCodexLeavesForeignAccountEntryIntact proves the safety
// property: real state at a projected name is not something this package
// created, so it is never deleted or rewritten. Since BOS-973 that refusal is
// SCOPED to the entry — materialization still succeeds — because failing the
// whole materialize silently downgrades the spawn to the ambient CLI login,
// which is strictly worse than one account-home entry that has stopped
// tracking the base home. removeStaleProjection itself still refuses the entry
// (see TestRemoveStaleProjectionContract); only the escalation is gone.
func TestMaterializeCodexLeavesForeignAccountEntryIntact(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	baseHome := t.TempDir()
	skillsPath := filepath.Join(baseHome, "skills")
	if err := os.MkdirAll(skillsPath, 0o700); err != nil {
		t.Fatalf("mkdir base skills: %v", err)
	}
	m, accountHome := projectedOnce(t, baseHome, "skills")

	accountSkills := filepath.Join(accountHome, "skills")
	if err := os.Remove(accountSkills); err != nil {
		t.Fatalf("remove account projection: %v", err)
	}
	if err := os.WriteFile(accountSkills, []byte("foreign"), 0o600); err != nil {
		t.Fatalf("write foreign account entry: %v", err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(skillsPath, "external-skill")); err != nil {
		t.Fatalf("add nested external skills symlink: %v", err)
	}

	if _, _, err := m.MaterializeCodex(context.Background(), "acct-1"); err != nil {
		t.Fatalf("MaterializeCodex failed on foreign account state at a projected name: %v", err)
	}
	got, err := os.ReadFile(accountSkills)
	if err != nil {
		t.Fatalf("foreign account entry was deleted: %v", err)
	}
	if string(got) != "foreign" {
		t.Fatalf("foreign account entry content changed: %q", got)
	}
}

// --- BOS-973: agent-created account-home state must never be fatal ----------
//
// Codex runs with CODEX_HOME pointed at the account home and atomically
// rewrites config.toml and recreates skills/ there on every run. Before
// BOS-973 the reconciler escalated "I do not own this entry" into "this
// account cannot be materialized at all", and the resolver's degrade path then
// silently ran the session on the ambient ~/.codex login. The safety invariant
// is unchanged — foreign state is still never deleted or replaced — but it is
// now skipped rather than fatal.

func TestProjectCodexBaseHomeSkipsForeignRequiredFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	baseHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(baseHome, "config.toml"), []byte("base = true\n"), 0o600); err != nil {
		t.Fatalf("write base config.toml: %v", err)
	}
	m := newTestMaterializerWithCodexHome(t, &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}, baseHome)

	// config.toml is a REQUIRED entry, and it is exactly the entry the observed
	// production failure hit: codex had rewritten it as a real file.
	accountHome := t.TempDir()
	accountConfig := filepath.Join(accountHome, "config.toml")
	const agentWritten = "model = \"agent-written\"\n"
	if err := os.WriteFile(accountConfig, []byte(agentWritten), 0o600); err != nil {
		t.Fatalf("write account config.toml: %v", err)
	}

	if err := m.projectCodexBaseHome(accountHome); err != nil {
		t.Fatalf("projectCodexBaseHome with a real account-home config.toml: %v", err)
	}

	got, err := os.ReadFile(accountConfig)
	if err != nil {
		t.Fatalf("read account config.toml after projection: %v", err)
	}
	if string(got) != agentWritten {
		t.Fatalf("account config.toml was rewritten: got %q want %q", string(got), agentWritten)
	}
	info, err := os.Lstat(accountConfig)
	if err != nil {
		t.Fatalf("lstat account config.toml: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("foreign account config.toml was replaced by a projection")
	}
}

func TestProjectCodexBaseHomeSkipsForeignOptionalDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	baseHome := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "ETHOS.md"), []byte("tool-managed\n"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	baseSkills := filepath.Join(baseHome, "skills")
	if err := os.MkdirAll(baseSkills, 0o700); err != nil {
		t.Fatalf("mkdir base skills: %v", err)
	}
	// The real-world shape: ~/.codex/skills holds symlinks into tool-managed
	// trees outside the base home, so the entry is unprojectable and the
	// reconciler tries to WITHDRAW any projection at that name.
	if err := os.Symlink(filepath.Join(outside, "ETHOS.md"), filepath.Join(baseSkills, "ETHOS.md")); err != nil {
		t.Fatalf("create escaping skill link: %v", err)
	}
	m := newTestMaterializerWithCodexHome(t, &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}, baseHome)

	accountHome := t.TempDir()
	accountSkills := filepath.Join(accountHome, "skills")
	if err := os.MkdirAll(accountSkills, 0o700); err != nil {
		t.Fatalf("mkdir account skills: %v", err)
	}
	marker := filepath.Join(accountSkills, "agent-created.md")
	if err := os.WriteFile(marker, []byte("recreated on every codex run\n"), 0o600); err != nil {
		t.Fatalf("write account skill marker: %v", err)
	}

	if err := m.projectCodexBaseHome(accountHome); err != nil {
		t.Fatalf("projectCodexBaseHome with a real account-home skills dir: %v", err)
	}

	info, err := os.Lstat(accountSkills)
	if err != nil {
		t.Fatalf("lstat account skills after projection: %v", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("foreign account skills dir was replaced: mode %v", info.Mode())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("agent-created skill file was removed: %v", err)
	}
}

func TestMaterializeCodexTwiceWithEscapingOptionalBaseEntry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	baseHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(baseHome, "config.toml"), []byte("base = true\n"), 0o600); err != nil {
		t.Fatalf("write base config.toml: %v", err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "ETHOS.md"), []byte("tool-managed\n"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	baseSkills := filepath.Join(baseHome, "skills")
	if err := os.MkdirAll(baseSkills, 0o700); err != nil {
		t.Fatalf("mkdir base skills: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "ETHOS.md"), filepath.Join(baseSkills, "ETHOS.md")); err != nil {
		t.Fatalf("create escaping skill link: %v", err)
	}
	m := newTestMaterializerWithCodexHome(t, &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}, baseHome)

	accountDir := m.codexAccountDir("acct-1")
	first, _, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("first MaterializeCodex: %v", err)
	}
	if got := first.Env["CODEX_HOME"]; got != accountDir {
		t.Fatalf("first CODEX_HOME = %q, want %q", got, accountDir)
	}

	// Simulate the codex run the first materialization enabled: it rewrites
	// config.toml as a real file and recreates skills/ as a real directory in
	// the account home. This is what made the failure RECUR — each run
	// re-broke the next materialization, permanently disabling injection.
	accountConfig := filepath.Join(accountDir, "config.toml")
	if err := os.Remove(accountConfig); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove projected config.toml: %v", err)
	}
	if err := os.WriteFile(accountConfig, []byte("model = \"agent-written\"\n"), 0o600); err != nil {
		t.Fatalf("simulate agent config write: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(accountDir, "skills"), 0o700); err != nil {
		t.Fatalf("simulate agent skills recreate: %v", err)
	}

	second, _, err := m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("second MaterializeCodex (the BOS-973 recurrence): %v", err)
	}
	if got := second.Env["CODEX_HOME"]; got != accountDir {
		t.Fatalf("second CODEX_HOME = %q, want %q", got, accountDir)
	}
}

// newTestMaterializerWithLogger is newTestMaterializerWithCodexHome with the
// logger exposed, so a test can assert on what the reconciler actually told the
// operator. The WARN is the ONLY signal a projected entry was skipped, so an
// unasserted one can be deleted by a future refactor without a test noticing.
func newTestMaterializerWithLogger(t *testing.T, store CredentialStore, codexHome string, out *bytes.Buffer) *Materializer {
	t.Helper()
	t.Setenv("CODEX_HOME", codexHome)
	m, err := New(store, zerolog.New(out), WithBaseDir(t.TempDir()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

// TestProjectCodexBaseHomeWarnsWhenSkippingForeignEntry pins the operator
// signal for the skip introduced by BOS-973. Skipping is quieter than the old
// hard failure by design, so the WARN — naming the entry — is what stops the
// drift it permits from being invisible.
func TestProjectCodexBaseHomeWarnsWhenSkippingForeignEntry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	baseHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(baseHome, "config.toml"), []byte("base = true\n"), 0o600); err != nil {
		t.Fatalf("write base config.toml: %v", err)
	}
	var logs bytes.Buffer
	m := newTestMaterializerWithLogger(t, &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}, baseHome, &logs)

	accountHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(accountHome, "config.toml"), []byte("model = \"agent\"\n"), 0o600); err != nil {
		t.Fatalf("write account config.toml: %v", err)
	}
	if err := m.projectCodexBaseHome(accountHome); err != nil {
		t.Fatalf("projectCodexBaseHome: %v", err)
	}

	out := logs.String()
	if !strings.Contains(out, `"level":"warn"`) {
		t.Fatalf("skipping a foreign entry must WARN:\n%s", out)
	}
	if !strings.Contains(out, `"entry":"config.toml"`) {
		t.Fatalf("the WARN must name the skipped entry:\n%s", out)
	}
}

// TestProjectCodexBaseHomeFailsOnNonForeignWithdrawalError is the negative of
// the errors.Join branch. That branch joins the errSymlinkOutsideBase diagnosis
// with the withdrawal error and must decide fatality from the WITHDRAWAL error
// alone: a `errors.Is(joined, errForeignAccountEntry)` refactor would look
// identical on the skip path and silently swallow a real removal failure here.
func TestProjectCodexBaseHomeFailsOnNonForeignWithdrawalError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	baseHome := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "ETHOS.md"), []byte("tool-managed\n"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	baseSkills := filepath.Join(baseHome, "skills")
	if err := os.MkdirAll(baseSkills, 0o700); err != nil {
		t.Fatalf("mkdir base skills: %v", err)
	}
	// Makes `skills` projectable-but-unsafe, so the reconciler reaches the
	// withdrawal branch that joins the two errors.
	if err := os.Symlink(filepath.Join(outside, "ETHOS.md"), filepath.Join(baseSkills, "ETHOS.md")); err != nil {
		t.Fatalf("create escaping skill link: %v", err)
	}
	m := newTestMaterializerWithCodexHome(t, &fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}, baseHome)

	// A RELATIVE link text is refused by removeStaleProjection with a plain
	// error — not errForeignAccountEntry — so it must still be fatal.
	accountHome := t.TempDir()
	if err := os.Symlink("relative/elsewhere", filepath.Join(accountHome, "skills")); err != nil {
		t.Fatalf("create relative account projection: %v", err)
	}

	err := m.projectCodexBaseHome(accountHome)
	if err == nil {
		t.Fatal("a non-foreign withdrawal failure must stay fatal, not be skipped with a WARN")
	}
	if errors.Is(err, errForeignAccountEntry) {
		t.Fatalf("error must not be classified as foreign account state: %v", err)
	}
	if !strings.Contains(err.Error(), "withdraw stale projection") {
		t.Fatalf("error = %v, want the withdrawal diagnosis", err)
	}
	// The joined errSymlinkOutsideBase diagnosis must survive — it names WHY
	// the entry went unsafe, which the removal error alone does not say.
	if !errors.Is(err, errSymlinkOutsideBase) {
		t.Fatalf("error = %v, want the errSymlinkOutsideBase diagnosis preserved in the join", err)
	}
}

// --- BOS-1174: access-token expiry and the refresh assertion ---------------
//
// CREDENTIAL SAFETY: every token in this file is SYNTHETIC. The payloads are
// hand-built claim objects, the signature segment is a fixed non-secret
// literal, and no assertion below ever prints a token — the helpers under test
// return only times and booleans, so a failure message cannot carry token
// material even by accident. TestRefreshAssertionCarriesNoTokenMaterial pins
// that property rather than leaving it to reviewer vigilance.

// synthToken builds a SYNTHETIC three-segment JWT-shaped string whose payload
// segment encodes claims. It is never a real credential: the header is fixed,
// the signature segment is a literal, and no key material is involved.
func synthToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal synthetic claims: %v", err)
	}
	return "eyJhbGciOiJub25lIn0." +
		base64.RawURLEncoding.EncodeToString(payload) +
		".signature-not-verified"
}

// expiryBlob wraps an access token in the codex auth.json shape, optionally
// stamped with a last_refresh marker ("" leaves the marker out entirely).
func expiryBlob(t *testing.T, accessToken, lastRefresh string) []byte {
	t.Helper()
	top := map[string]any{
		"tokens": map[string]any{
			"access_token":  accessToken,
			"refresh_token": "synthetic-refresh-token",
		},
	}
	if lastRefresh != "" {
		top[authGenerationKey] = lastRefresh
	}
	blob, err := json.Marshal(top)
	if err != nil {
		t.Fatalf("marshal synthetic codex blob: %v", err)
	}
	return blob
}

// TestTokenNumericClaim is the malformed-token table. Absence of a readable
// claim must always degrade to "cannot evaluate" (ok=false) — never to a zero
// time a caller could mistake for a real instant, and never to an error that
// could carry token bytes into a log line.
func TestTokenNumericClaim(t *testing.T) {
	t.Parallel()
	exp := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		token  string
		want   time.Time
		wantOK bool
	}{
		{
			name:   "valid token",
			token:  synthToken(t, map[string]any{"exp": exp.Unix()}),
			want:   exp,
			wantOK: true,
		},
		{
			name:  "empty token string",
			token: "",
		},
		{
			name:  "missing payload segment",
			token: "eyJhbGciOiJub25lIn0",
		},
		{
			name:  "only two segments",
			token: "eyJhbGciOiJub25lIn0.",
		},
		{
			name:  "non-base64 payload",
			token: "eyJhbGciOiJub25lIn0.!!!not-base64!!!.signature-not-verified",
		},
		{
			name:  "payload is not a JSON object",
			token: "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString([]byte(`["exp",1]`)) + ".sig",
		},
		{
			name:  "payload without exp",
			token: synthToken(t, map[string]any{"iat": exp.Unix()}),
		},
		{
			name:  "exp is not a number",
			token: synthToken(t, map[string]any{"exp": "tomorrow"}),
		},
		{
			name:  "exp is null",
			token: synthToken(t, map[string]any{"exp": nil}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := tokenNumericClaim(tc.token, "exp")
			if ok != tc.wantOK {
				t.Fatalf("ok: got %v want %v", ok, tc.wantOK)
			}
			if ok && !got.Equal(tc.want) {
				t.Fatalf("claim: got %v want %v", got.UTC(), tc.want)
			}
			if !ok && !got.IsZero() {
				t.Fatalf("unreadable claim must return the zero time, got %v", got.UTC())
			}
		})
	}
}

// TestRefreshAssertion covers both directions the plan requires: a clean
// credential well inside its access-token lifetime must NOT be reported as
// overdue (the common quiet case), and one past the refresh point must be.
// Everything that cannot be evaluated stays Unknown.
func TestRefreshAssertion(t *testing.T) {
	t.Parallel()
	issued := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	expires := issued.Add(10 * 24 * time.Hour)
	issuedRFC := issued.Format(time.RFC3339)

	live := synthToken(t, map[string]any{"iat": issued.Unix(), "exp": expires.Unix()})
	noIat := synthToken(t, map[string]any{"exp": expires.Unix()})

	for _, tc := range []struct {
		name string
		blob []byte
		now  time.Time
		want RefreshAssertion
	}{
		{
			name: "well inside the access-token lifetime",
			blob: expiryBlob(t, live, issuedRFC),
			now:  issued.Add(24 * time.Hour),
			want: RefreshAssertionNotDue,
		},
		{
			name: "exactly at the refresh point is not yet overdue",
			blob: expiryBlob(t, live, issuedRFC),
			now:  issued.Add(8 * 24 * time.Hour),
			want: RefreshAssertionNotDue,
		},
		{
			name: "past the refresh point",
			blob: expiryBlob(t, live, issuedRFC),
			now:  issued.Add(9 * 24 * time.Hour),
			want: RefreshAssertionOverdue,
		},
		{
			name: "past expiry entirely",
			blob: expiryBlob(t, live, issuedRFC),
			now:  expires.Add(time.Hour),
			want: RefreshAssertionOverdue,
		},
		{
			name: "no iat falls back to the last_refresh stamp",
			blob: expiryBlob(t, noIat, issuedRFC),
			now:  issued.Add(9 * 24 * time.Hour),
			want: RefreshAssertionOverdue,
		},
		{
			name: "no iat and no last_refresh cannot be evaluated",
			blob: expiryBlob(t, noIat, ""),
			now:  issued.Add(9 * 24 * time.Hour),
			want: RefreshAssertionUnknown,
		},
		{
			name: "malformed token degrades to cannot-evaluate, never overdue",
			blob: expiryBlob(t, "not-a-jwt", issuedRFC),
			now:  issued.Add(9 * 24 * time.Hour),
			want: RefreshAssertionUnknown,
		},
		{
			name: "blob with no tokens object",
			blob: []byte(`{"last_refresh":"2026-09-03T12:00:00Z"}`),
			now:  issued.Add(9 * 24 * time.Hour),
			want: RefreshAssertionUnknown,
		},
		{
			name: "blob that is not JSON",
			blob: []byte("not json at all"),
			now:  issued.Add(9 * 24 * time.Hour),
			want: RefreshAssertionUnknown,
		},
		{
			name: "an expiry no later than issuance is not a lifetime",
			blob: expiryBlob(t, synthToken(t, map[string]any{"iat": expires.Unix(), "exp": issued.Unix()}), issuedRFC),
			now:  expires.Add(24 * time.Hour),
			want: RefreshAssertionUnknown,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := refreshAssertion(tc.blob, tc.now); got != tc.want {
				t.Fatalf("assertion: got %v want %v", got, tc.want)
			}
		})
	}
}

// TestRefreshAssertionRejectsFutureIssuance is the clock-skew guard. A
// last_refresh (or iat) stamped in the future must never be read as a refresh
// that just happened: that would silently vouch for a credential whose chain is
// dead. The honest answer is "cannot evaluate", which is also the only answer
// that cannot bench a working account on a skewed clock.
func TestRefreshAssertionRejectsFutureIssuance(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	future := now.Add(48 * time.Hour)
	expires := now.Add(24 * time.Hour)

	skewedStamp := expiryBlob(t,
		synthToken(t, map[string]any{"exp": expires.Unix()}),
		future.Format(time.RFC3339),
	)
	if got := refreshAssertion(skewedStamp, now); got != RefreshAssertionUnknown {
		t.Fatalf("future last_refresh: got %v want %v", got, RefreshAssertionUnknown)
	}

	skewedIat := expiryBlob(t,
		synthToken(t, map[string]any{"iat": future.Unix(), "exp": expires.Unix()}),
		now.Add(-10*24*time.Hour).Format(time.RFC3339),
	)
	if got := refreshAssertion(skewedIat, now); got != RefreshAssertionUnknown {
		t.Fatalf("future iat: got %v want %v", got, RefreshAssertionUnknown)
	}
}

// TestRefreshAssertionCarriesNoTokenMaterial pins the redaction property that
// the whole design rests on: what leaves this package is a classification, not
// a claim. Formatting every value these helpers return must never reproduce a
// byte of the token they were derived from.
func TestRefreshAssertionCarriesNoTokenMaterial(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	issued := now.Add(-9 * 24 * time.Hour)
	expires := issued.Add(10 * 24 * time.Hour)
	token := synthToken(t, map[string]any{"iat": issued.Unix(), "exp": expires.Unix()})
	blob := expiryBlob(t, token, issued.Format(time.RFC3339))

	assertion := refreshAssertion(blob, now)
	if assertion != RefreshAssertionOverdue {
		t.Fatalf("precondition: got %v want %v", assertion, RefreshAssertionOverdue)
	}
	rendered := fmt.Sprintf("%v %d", assertion, assertion)
	for _, segment := range strings.Split(token, ".") {
		if strings.Contains(rendered, segment) {
			t.Fatalf("rendered assertion %q reproduced a token segment", rendered)
		}
	}
	if strings.Contains(rendered, "synthetic-refresh-token") {
		t.Fatalf("rendered assertion %q reproduced the refresh token", rendered)
	}
}

// TestMaterializeCodexCarriesTheRefreshAssertion is the missing link BOS-1174's
// review found: refreshAssertion itself was exhaustively table-tested, but
// NOTHING asserted that MaterializeCodex actually puts its verdict on the
// Materialized value it returns. Deleting that single assignment left the whole
// suite green while the feature silently degraded back to a permanent Unknown —
// the original bug, with passing CI.
//
// No clock injection is needed to test it. The threshold is a FRACTION of the
// token's own observed lifetime, not a wall-clock duration, so a token issued 9
// days ago and expiring in 1 day is 90% through a 10-day life under any clock
// this test could plausibly run on; the verdict is skew-tolerant by
// construction. The not-due half is pinned in the same shape so the test cannot
// pass against a function that returns a constant.
func TestMaterializeCodexCarriesTheRefreshAssertion(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name    string
		issued  time.Time
		expires time.Time
		want    RefreshAssertion
	}{
		{
			name:    "90% through its own lifetime is overdue",
			issued:  now.Add(-9 * 24 * time.Hour),
			expires: now.Add(24 * time.Hour),
			want:    RefreshAssertionOverdue,
		},
		{
			name:    "10% through its own lifetime is not due",
			issued:  now.Add(-24 * time.Hour),
			expires: now.Add(9 * 24 * time.Hour),
			want:    RefreshAssertionNotDue,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token := synthToken(t, map[string]any{"iat": tc.issued.Unix(), "exp": tc.expires.Unix()})
			m := newTestMaterializer(t, &fakeStore{blob: expiryBlob(t, token, "")})

			mat, _, err := m.MaterializeCodex(context.Background(), "acct-1")
			if err != nil {
				t.Fatalf("MaterializeCodex: %v", err)
			}
			if mat.RefreshAssertion != tc.want {
				t.Fatalf("Materialized.RefreshAssertion = %v, want %v — the verdict is not reaching the caller",
					mat.RefreshAssertion, tc.want)
			}
		})
	}
}

// TestMaterializeClaudeReportsUnknownRefreshAssertion pins the documented claude
// answer. Claude materialization reads no OAuth access token this package can
// evaluate, so "cannot ask" must be reported as Unknown — never as Overdue,
// which would put an unproven-refresh warning on every claude account.
func TestMaterializeClaudeReportsUnknownRefreshAssertion(t *testing.T) {
	m := newTestMaterializer(t, &fakeStore{blob: []byte("sk-ant-oat01-secret")})
	mat, err := m.MaterializeClaude(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeClaude: %v", err)
	}
	if mat.RefreshAssertion != RefreshAssertionUnknown {
		t.Fatalf("claude Materialized.RefreshAssertion = %v, want Unknown", mat.RefreshAssertion)
	}
}
