package auth

import (
	"context"
	"testing"
	"time"

	"github.com/99designs/keyring"
)

// fileBackedStore opens a KeychainStore over a real keyring file backend in a
// temp dir. Building the keyring.Config directly bypasses keyringutil.Backends()
// so the test never touches the developer's OS keychain (and never pops the
// macOS "allow access" prompt), while still exercising the real KeychainStore
// encode/decode/migration path that a mockTokenStore replaces wholesale.
func fileBackedStore(t *testing.T, dir string) *KeychainStore {
	t.Helper()
	ring, err := keyring.Open(keyring.Config{
		ServiceName:      serviceName,
		AllowedBackends:  []keyring.BackendType{keyring.FileBackend},
		FileDir:          dir,
		FilePasswordFunc: func(string) (string, error) { return "roundtrip-test-passphrase", nil },
	})
	if err != nil {
		t.Fatalf("open file keyring: %v", err)
	}
	return &KeychainStore{ring: ring}
}

// BOS-659 shipped a passing mockTokenStore test for "login clears the marker"
// and production still broke, because the fake cannot fail the way the real
// store does. This drives the whole cycle over the real backend: seed a flagged
// record, log in, and read the result back through a FRESH store — so the
// assertion is on bytes that survived a full encode/decode round trip, not on a
// pointer the test itself put in a map.
func TestManagerLogin_ClearsFlaggedRecordOnTheRealFileBackend(t *testing.T) {
	stubCredentialLock(t)
	stubWorkOSLogin(t, "roundtrip-access", "roundtrip-refresh", "dave@example.com")

	dir := t.TempDir()
	store := fileBackedStore(t, dir)

	seeded := &Tokens{
		AccessToken:   "stale-access",
		RefreshToken:  "stale-refresh",
		Email:         "dave@example.com",
		ExpiresAt:     time.Now().Add(-1 * time.Hour),
		NeedsRelogin:  true,
		ReloginReason: ReloginReasonRefreshOutcomeUnknown,
	}
	if err := store.Save(seeded); err != nil {
		t.Fatalf("seed flagged record: %v", err)
	}

	// The seed really is flagged, on the real backend, before login runs.
	seededBack, err := fileBackedStore(t, dir).Load()
	if err != nil {
		t.Fatalf("read back the seed: %v", err)
	}
	if seededBack.ReloginReasonOrEmpty() != ReloginReasonRefreshOutcomeUnknown {
		t.Fatalf("seeded record is not flagged: reason = %q", seededBack.ReloginReasonOrEmpty())
	}

	mgr := NewManager(store, Config{ClientID: "test-client"})
	resp, err := mgr.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	verdict, err := mgr.PollLogin(context.Background(), resp.DeviceCode, resp.Interval)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if verdict.Outcome != LoginVerified {
		t.Fatalf("verdict = %s (reason %q), want verified", verdict.Outcome, verdict.Reason)
	}
	if verdict.Email != "dave@example.com" {
		t.Errorf("verdict email = %q, want dave@example.com", verdict.Email)
	}

	// A fresh store over the same dir: nothing in-process is carrying this.
	got, err := fileBackedStore(t, dir).Load()
	if err != nil {
		t.Fatalf("read back after login: %v", err)
	}
	if got.ReloginReasonOrEmpty() != "" {
		t.Errorf("record is still flagged after login: reason = %q", got.ReloginReasonOrEmpty())
	}
	if got.NeedsRelogin {
		t.Error("NeedsRelogin survived the login")
	}
	if got.AccessToken != "roundtrip-access" {
		t.Error("the persisted access token is not the one login stored")
	}
	if got.RefreshToken != "roundtrip-refresh" {
		t.Error("the persisted refresh token is not the one login stored")
	}
	if got.Email != "dave@example.com" {
		t.Errorf("persisted email = %q, want dave@example.com", got.Email)
	}
}

// The same cycle from a genuinely empty backend: there is no prior record to
// read back by accident, so a verified verdict here means the write landed.
func TestManagerLogin_VerifiesAFirstWriteOnTheRealFileBackend(t *testing.T) {
	stubCredentialLock(t)
	stubWorkOSLogin(t, "first-access", "first-refresh", "new@example.com")

	dir := t.TempDir()
	mgr := NewManager(fileBackedStore(t, dir), Config{ClientID: "test-client"})

	verdict, err := mgr.Login(context.Background())
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if verdict.Outcome != LoginVerified {
		t.Fatalf("verdict = %s (reason %q), want verified", verdict.Outcome, verdict.Reason)
	}

	got, err := fileBackedStore(t, dir).Load()
	if err != nil {
		t.Fatalf("read back after login: %v", err)
	}
	if got.AccessToken != "first-access" || got.Email != "new@example.com" {
		t.Error("the persisted record is not the one login stored")
	}
}

// A store that reports a successful save and stores nothing is the production
// failure this work exists to catch. Over the real backend an absent record
// must come back as record_not_updated, never as a verified login.
func TestManagerLogin_UnwrittenRecordOnTheRealFileBackendIsNotVerified(t *testing.T) {
	stubCredentialLock(t)
	stubWorkOSLogin(t, "ignored-access", "ignored-refresh", "dave@example.com")

	dir := t.TempDir()
	mgr := NewManager(&noopSaveStore{inner: fileBackedStore(t, dir)}, Config{ClientID: "test-client"})

	verdict, err := mgr.Login(context.Background())
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if verdict.Outcome != LoginVerifyRecordNotUpdated {
		t.Fatalf("verdict = %s (reason %q), want record_not_updated", verdict.Outcome, verdict.Reason)
	}
	if verdict.Reason != LoginVerifyReasonRecordAbsent {
		t.Errorf("reason = %q, want %q", verdict.Reason, LoginVerifyReasonRecordAbsent)
	}
}

// noopSaveStore reports a successful save without writing anything. Load and
// Delete still go to the real store, so the read side stays honest.
type noopSaveStore struct {
	inner *KeychainStore
}

func (s *noopSaveStore) Save(*Tokens) error     { return nil }
func (s *noopSaveStore) Load() (*Tokens, error) { return s.inner.Load() }
func (s *noopSaveStore) Delete() error          { return s.inner.Delete() }
