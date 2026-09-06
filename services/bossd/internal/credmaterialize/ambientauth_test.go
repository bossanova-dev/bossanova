package credmaterialize

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// Synthetic credential material for the ambient-comparison tests. Nothing here
// is or resembles a real credential; the FAKE- prefix is the sentinel the
// redaction assertions below search for.
const (
	ambientOtherAccount = "FAKE-OTHER-ACCOUNT-ID"
	ambientNewRefresh   = "FAKE-ROTATED-REFRESH"
)

// ambientSecrets are every synthetic value that must never escape the
// comparison — into a log line, or into a test failure message. The tests below
// assert both, so this list is the single place the sentinel set is written.
var ambientSecrets = []string{
	fakeAccess, fakeID, fakeRefresh, fakeAccount,
	ambientOtherAccount, ambientNewRefresh,
}

// ambientBlob builds a synthetic codex auth.json body with an explicit
// account_id, which codexBlob pins to fakeAccount. The identity gate is the
// whole point of this feature, so its tests need to vary that field.
func ambientBlob(t *testing.T, accountID, refresh string) []byte {
	t.Helper()
	doc := map[string]any{
		"tokens": map[string]any{
			"access_token":  fakeAccess,
			"id_token":      fakeID,
			"refresh_token": refresh,
			"account_id":    accountID,
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal ambient blob: %v", err)
	}
	return raw
}

// ambientFixture wires a Materializer whose ambient CODEX_HOME is a temp dir the
// test controls, and returns the ambient auth.json path plus a buffer capturing
// every log line the comparison emits.
type ambientFixture struct {
	m        *Materializer
	store    *fakeStore
	home     string
	authPath string
	logs     *lockedBuffer
}

// lockedBuffer is a zerolog sink safe to read while the materializer writes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newAmbientFixture(t *testing.T, storedBlob []byte) *ambientFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	store := &fakeStore{blob: storedBlob}
	logs := &lockedBuffer{}
	// Debug level so even the quietest refusal line is captured; the redaction
	// assertions are only meaningful against the most verbose output we emit.
	m, err := New(store, zerolog.New(logs).Level(zerolog.DebugLevel), WithBaseDir(t.TempDir()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &ambientFixture{m: m, store: store, home: home, authPath: filepath.Join(home, authFileName), logs: logs}
}

// writeAmbient writes the ambient auth.json for the fixture.
func (f *ambientFixture) writeAmbient(t *testing.T, blob []byte) {
	t.Helper()
	if err := os.WriteFile(f.authPath, blob, 0o600); err != nil {
		t.Fatalf("write ambient auth.json: %v", err)
	}
}

// assertNoSecretsLogged fails when any synthetic credential value reached a log
// line. The failure message names only WHICH sentinel leaked, never the log
// body, so this assertion cannot itself become the leak it guards against.
func (f *ambientFixture) assertNoSecretsLogged(t *testing.T) {
	t.Helper()
	out := f.logs.String()
	for _, secret := range ambientSecrets {
		if strings.Contains(out, secret) {
			t.Fatalf("a synthetic credential value reached a log line (sentinel index %d)", indexOfSecret(secret))
		}
	}
}

func indexOfSecret(secret string) int {
	for i, s := range ambientSecrets {
		if s == secret {
			return i
		}
	}
	return -1
}

// TestCompareAmbientCodexAuthOutcomes is the four-outcome table: same identity
// and same token, same identity and a rotated token, a DIFFERENT identity, and
// no ambient file at all.
//
// The no-op and not-evaluable rows are asserted to be the same value on purpose.
// "An ambient login for another account" must produce no signal of any kind, and
// a distinct enum value would itself be a signal — it would report that an
// unrelated login exists.
func TestCompareAmbientCodexAuthOutcomes(t *testing.T) {
	cases := []struct {
		name string
		// ambient is nil when no ambient auth.json should exist at all.
		ambient func(t *testing.T) []byte
		want    AmbientAuthState
	}{
		{
			name:    "same identity same token is in sync",
			ambient: func(t *testing.T) []byte { return ambientBlob(t, fakeAccount, fakeRefresh) },
			want:    AmbientAuthInSync,
		},
		{
			name:    "same identity different token is superseded",
			ambient: func(t *testing.T) []byte { return ambientBlob(t, fakeAccount, ambientNewRefresh) },
			want:    AmbientAuthSuperseded,
		},
		{
			name:    "different identity is not evaluable",
			ambient: func(t *testing.T) []byte { return ambientBlob(t, ambientOtherAccount, ambientNewRefresh) },
			want:    AmbientAuthNotEvaluable,
		},
		{
			name:    "absent ambient file is not evaluable",
			ambient: nil,
			want:    AmbientAuthNotEvaluable,
		},
		{
			name:    "unparseable ambient file is not evaluable",
			ambient: func(*testing.T) []byte { return []byte("{not json") },
			want:    AmbientAuthNotEvaluable,
		},
		{
			name: "ambient file without account_id is not evaluable",
			ambient: func(t *testing.T) []byte {
				t.Helper()
				raw, err := json.Marshal(map[string]any{
					"tokens": map[string]any{"refresh_token": ambientNewRefresh},
				})
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				return raw
			},
			want: AmbientAuthNotEvaluable,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAmbientFixture(t, codexBlob(t, fakeAccess, fakeID, fakeRefresh))
			if tc.ambient != nil {
				f.writeAmbient(t, tc.ambient(t))
			}
			if got := f.m.CompareAmbientCodexAuth(context.Background(), "acct-1"); got != tc.want {
				t.Fatalf("CompareAmbientCodexAuth = %v; want %v", got, tc.want)
			}
			f.assertNoSecretsLogged(t)
		})
	}
}

// TestCompareAmbientCodexAuthTwoAccountsStaySilent is the headline false
// positive, named so a regression is unmistakable. An operator running one
// account bound to the daemon and a personal ambient login has a different
// account_id in each; without the identity gate that ordinary setup is
// indistinguishable from a real external rotation and would alarm forever.
func TestCompareAmbientCodexAuthTwoAccountsStaySilent(t *testing.T) {
	f := newAmbientFixture(t, codexBlob(t, fakeAccess, fakeID, fakeRefresh))
	// The personal login rotates repeatedly; the bound account's stored
	// credential never moves. Every round must stay silent.
	for i, refresh := range []string{ambientNewRefresh, ambientNewRefresh + "-2", ambientNewRefresh + "-3"} {
		f.writeAmbient(t, ambientBlob(t, ambientOtherAccount, refresh))
		if got := f.m.CompareAmbientCodexAuth(context.Background(), "acct-1"); got != AmbientAuthNotEvaluable {
			t.Fatalf("round %d: CompareAmbientCodexAuth = %v; want %v for an ambient login belonging to another account",
				i, got, AmbientAuthNotEvaluable)
		}
	}
	f.assertNoSecretsLogged(t)
}

// TestCompareAmbientCodexAuthReadsAccountStoreShapedBlob pins that the stored
// blob is normalized before comparison. The account-store shape keeps its
// secrets in top-level "access"/"refresh" fields; without normalizeTokens the
// stored side would fail to parse and every account registered that way would
// report not-evaluable forever, silently disabling the whole feature.
func TestCompareAmbientCodexAuthReadsAccountStoreShapedBlob(t *testing.T) {
	stored, err := json.Marshal(map[string]any{
		"access":  fakeAccess,
		"refresh": fakeRefresh,
		"tokens":  map[string]any{"account_id": fakeAccount},
	})
	if err != nil {
		t.Fatalf("marshal stored blob: %v", err)
	}
	f := newAmbientFixture(t, stored)
	f.writeAmbient(t, ambientBlob(t, fakeAccount, ambientNewRefresh))
	if got := f.m.CompareAmbientCodexAuth(context.Background(), "acct-1"); got != AmbientAuthSuperseded {
		t.Fatalf("CompareAmbientCodexAuth = %v; want %v", got, AmbientAuthSuperseded)
	}
	f.assertNoSecretsLogged(t)
}

// TestCompareAmbientCodexAuthUnreadableStoreIsNotEvaluable pins that a store
// failure is non-evidence rather than a verdict — and never an error that fails
// the verification the comparison rides along with.
func TestCompareAmbientCodexAuthUnreadableStoreIsNotEvaluable(t *testing.T) {
	f := newAmbientFixture(t, codexBlob(t, fakeAccess, fakeID, fakeRefresh))
	f.writeAmbient(t, ambientBlob(t, fakeAccount, ambientNewRefresh))
	f.store.loadErr = errors.New("keyring unavailable")
	if got := f.m.CompareAmbientCodexAuth(context.Background(), "acct-1"); got != AmbientAuthNotEvaluable {
		t.Fatalf("CompareAmbientCodexAuth = %v; want %v", got, AmbientAuthNotEvaluable)
	}
	f.assertNoSecretsLogged(t)
}

// TestCompareAmbientCodexAuthRefusesSymlinkedAmbientPath is the safe-read
// refusal that matters most. Lstat (never Stat) keeps a symlinked leaf caught,
// so the comparison never reads whatever the link points at — and because this
// path never writes, no foreign credential can reach this account's store by
// any route.
func TestCompareAmbientCodexAuthRefusesSymlinkedAmbientPath(t *testing.T) {
	f := newAmbientFixture(t, codexBlob(t, fakeAccess, fakeID, fakeRefresh))

	// The link target is a blob that WOULD report superseded if it were read.
	foreign := filepath.Join(t.TempDir(), "foreign-auth.json")
	if err := os.WriteFile(foreign, ambientBlob(t, fakeAccount, ambientNewRefresh), 0o600); err != nil {
		t.Fatalf("write foreign credential: %v", err)
	}
	if err := os.Symlink(foreign, f.authPath); err != nil {
		t.Skipf("symlinks unavailable in test filesystem: %v", err)
	}

	if got := f.m.CompareAmbientCodexAuth(context.Background(), "acct-1"); got != AmbientAuthNotEvaluable {
		t.Fatalf("CompareAmbientCodexAuth followed a symlinked ambient auth.json: got %v, want %v", got, AmbientAuthNotEvaluable)
	}
	// The link itself is left exactly as found: this path never writes.
	info, err := os.Lstat(f.authPath)
	if err != nil {
		t.Fatalf("lstat ambient auth.json: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the comparison replaced the ambient symlink; it must never write")
	}
	f.assertNoSecretsLogged(t)
}

// TestCompareAmbientCodexAuthRefusesDirectoryAmbientPath covers the non-regular
// refusal with the case a restored home most plausibly produces.
func TestCompareAmbientCodexAuthRefusesDirectoryAmbientPath(t *testing.T) {
	f := newAmbientFixture(t, codexBlob(t, fakeAccess, fakeID, fakeRefresh))
	if err := os.MkdirAll(f.authPath, 0o700); err != nil {
		t.Fatalf("mkdir ambient auth.json: %v", err)
	}
	if got := f.m.CompareAmbientCodexAuth(context.Background(), "acct-1"); got != AmbientAuthNotEvaluable {
		t.Fatalf("CompareAmbientCodexAuth = %v; want %v for a directory at the ambient auth path", got, AmbientAuthNotEvaluable)
	}
	if info, err := os.Lstat(f.authPath); err != nil || !info.IsDir() {
		t.Fatal("the comparison replaced the ambient directory; it must never write")
	}
	f.assertNoSecretsLogged(t)
}

// TestCompareAmbientCodexAuthRefusesFifoAmbientPathWithoutBlocking pins the
// refusal that a plain "read it and see" cannot survive: ReadFile on a
// writerless FIFO blocks forever. The Lstat/IsRegular guard must drop it BEFORE
// any open, so the comparison returns promptly. The goroutine-plus-timeout shape
// is what makes a regression fail loudly instead of hanging the whole suite.
func TestCompareAmbientCodexAuthRefusesFifoAmbientPathWithoutBlocking(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFO semantics differ on Windows")
	}
	f := newAmbientFixture(t, codexBlob(t, fakeAccess, fakeID, fakeRefresh))
	if err := syscall.Mkfifo(f.authPath, 0o600); err != nil {
		t.Skipf("FIFOs unavailable in test filesystem: %v", err)
	}

	done := make(chan AmbientAuthState, 1)
	go func() { done <- f.m.CompareAmbientCodexAuth(context.Background(), "acct-1") }()
	select {
	case got := <-done:
		if got != AmbientAuthNotEvaluable {
			t.Fatalf("CompareAmbientCodexAuth = %v; want %v for a FIFO at the ambient auth path", got, AmbientAuthNotEvaluable)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("CompareAmbientCodexAuth hung: the ambient FIFO was opened instead of refused")
	}

	info, err := os.Lstat(f.authPath)
	if err != nil {
		t.Fatalf("lstat ambient auth.json: %v", err)
	}
	if info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatal("the comparison replaced the ambient FIFO; it must never write")
	}
	f.assertNoSecretsLogged(t)
}

// TestCompareAmbientCodexAuthRefusesHardLinkedAmbientPath covers the alias a
// symlink check cannot see: a hard link is a regular file, so only the link
// count distinguishes it from an ambient credential this home actually owns.
func TestCompareAmbientCodexAuthRefusesHardLinkedAmbientPath(t *testing.T) {
	f := newAmbientFixture(t, codexBlob(t, fakeAccess, fakeID, fakeRefresh))

	foreign := filepath.Join(t.TempDir(), "foreign-auth.json")
	if err := os.WriteFile(foreign, ambientBlob(t, fakeAccount, ambientNewRefresh), 0o600); err != nil {
		t.Fatalf("write foreign credential: %v", err)
	}
	if err := os.Link(foreign, f.authPath); err != nil {
		t.Skipf("hard links unavailable in test filesystem: %v", err)
	}

	if got := f.m.CompareAmbientCodexAuth(context.Background(), "acct-1"); got != AmbientAuthNotEvaluable {
		t.Fatalf("CompareAmbientCodexAuth read a multiply-linked ambient auth.json: got %v, want %v", got, AmbientAuthNotEvaluable)
	}
	f.assertNoSecretsLogged(t)
}

// TestCompareAmbientCodexAuthWritesNothing is the acceptance criterion in its
// strongest form: after the comparison reports superseded, the store blob, the
// account-local auth.json, and the .bossd-auth-sha256 sidecar must all be
// byte-identical to what they were before, and the store must have taken no
// save.
func TestCompareAmbientCodexAuthWritesNothing(t *testing.T) {
	f := newAmbientFixture(t, codexBlob(t, fakeAccess, fakeID, fakeRefresh))

	// Materialize first, so there IS an account-local auth.json and sidecar to
	// leave alone. Their absence would make this assertion vacuous.
	res, _, err := f.m.MaterializeCodex(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("MaterializeCodex: %v", err)
	}
	accountAuth := filepath.Join(res.HomeDir, authFileName)
	sidecar := filepath.Join(res.HomeDir, authHashFileName)

	f.writeAmbient(t, ambientBlob(t, fakeAccount, ambientNewRefresh))

	beforeStore := f.store.currentBlob()
	beforeSaves := f.store.saveCount()
	beforeAuth := readFileForTest(t, accountAuth)
	beforeSidecar := readFileForTest(t, sidecar)
	beforeAmbient := readFileForTest(t, f.authPath)

	if got := f.m.CompareAmbientCodexAuth(context.Background(), "acct-1"); got != AmbientAuthSuperseded {
		t.Fatalf("CompareAmbientCodexAuth = %v; want %v", got, AmbientAuthSuperseded)
	}

	if extra := f.store.saveCount() - beforeSaves; extra != 0 {
		t.Fatalf("SaveCredential calls = %d; want 0: the comparison must never write", extra)
	}
	if !bytes.Equal(f.store.currentBlob(), beforeStore) {
		t.Fatal("the comparison changed the stored credential")
	}
	if !bytes.Equal(readFileForTest(t, accountAuth), beforeAuth) {
		t.Fatal("the comparison changed the account-local auth.json")
	}
	if !bytes.Equal(readFileForTest(t, sidecar), beforeSidecar) {
		t.Fatal("the comparison changed the .bossd-auth-sha256 sidecar")
	}
	if !bytes.Equal(readFileForTest(t, f.authPath), beforeAmbient) {
		t.Fatal("the comparison changed the ambient auth.json")
	}
	f.assertNoSecretsLogged(t)
}

// readFileForTest reads a file the test itself created under t.TempDir().
func readFileForTest(t *testing.T, path string) []byte {
	t.Helper()
	// #nosec G304 -- test-local temp-dir path; owner=@recurser review-by=2027-01-18 issue=BOS-1175
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Base(path), err)
	}
	return raw
}

// lockCountingStore records every WithCredentialLock entry so a test can prove
// the account lock was never taken.
type lockCountingStore struct {
	fakeStore
	lockMu sync.Mutex
	locks  int
}

func (s *lockCountingStore) WithCredentialLock(_ string, fn func() error) error {
	s.lockMu.Lock()
	s.locks++
	s.lockMu.Unlock()
	return fn()
}

func (s *lockCountingStore) lockCount() int {
	s.lockMu.Lock()
	defer s.lockMu.Unlock()
	return s.locks
}

// TestCompareAmbientCodexAuthDoesNotTakeTheAccountLock pins the strongest form
// of "the account lock is not held across the ambient read": the lock is never
// taken at all, so it cannot be held. The lock guards this account's own
// directory; a foreign path that blocks — a FIFO, a dead network mount — would
// otherwise stall every materialization for an account that does not own it.
func TestCompareAmbientCodexAuthDoesNotTakeTheAccountLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	store := &lockCountingStore{fakeStore: fakeStore{blob: codexBlob(t, fakeAccess, fakeID, fakeRefresh)}}
	m, err := New(store, zerolog.Nop(), WithBaseDir(t.TempDir()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, authFileName), ambientBlob(t, fakeAccount, ambientNewRefresh), 0o600); err != nil {
		t.Fatalf("write ambient auth.json: %v", err)
	}

	if got := m.CompareAmbientCodexAuth(context.Background(), "acct-1"); got != AmbientAuthSuperseded {
		t.Fatalf("CompareAmbientCodexAuth = %v; want %v", got, AmbientAuthSuperseded)
	}
	if n := store.lockCount(); n != 0 {
		t.Fatalf("WithCredentialLock called %d times; want 0: the account lock must not be taken for a foreign path", n)
	}
}

// TestAmbientAuthStateCarriesNoCredentialMaterial pins that the ONLY value that
// escapes the comparison — the enum and its durable string form — is a closed
// set of fixed tokens. That is what makes the state safe to place on
// SmokeResult, in the durable auth-check row, and in a log line.
func TestAmbientAuthStateCarriesNoCredentialMaterial(t *testing.T) {
	want := map[AmbientAuthState]string{
		AmbientAuthNotEvaluable: "not_evaluable",
		AmbientAuthInSync:       "in_sync",
		AmbientAuthSuperseded:   "superseded",
	}
	for state, token := range want {
		if got := state.String(); got != token {
			t.Fatalf("AmbientAuthState(%d).String() = %q; want %q", int(state), got, token)
		}
		for i, secret := range ambientSecrets {
			if strings.Contains(state.String(), secret) {
				t.Fatalf("AmbientAuthState(%d).String() carries synthetic credential material (sentinel index %d)", int(state), i)
			}
		}
	}
	// An unrecognised state must degrade to the non-evidence token, never to a
	// numeric value a caller could mistake for a verdict.
	if got := AmbientAuthState(99).String(); got != "not_evaluable" {
		t.Fatalf("unknown AmbientAuthState renders %q; want %q", got, "not_evaluable")
	}
}

// TestMain points CODEX_HOME at an empty directory for the whole package, so a
// test that does not set it never reads the developer's real ~/.codex — a live
// credential file this package now compares against (BOS-1175). Individual tests
// still override it with t.Setenv, which restores to this value rather than to
// the operator's real home.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "credmaterialize-codex-home-*")
	if err != nil {
		panic("create hermetic CODEX_HOME: " + err.Error())
	}
	if err := os.Setenv("CODEX_HOME", dir); err != nil {
		panic("set hermetic CODEX_HOME: " + err.Error())
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
