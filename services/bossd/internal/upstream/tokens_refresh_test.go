package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/99designs/keyring"
)

type testRefreshLock struct {
	mu *sync.Mutex
}

func (l *testRefreshLock) Unlock() error {
	l.mu.Unlock()
	return nil
}

func withTokenRefreshHooks(t *testing.T, lockMu *sync.Mutex) {
	t.Helper()
	origAcquire := acquireRefreshLock
	origRefresh := refreshWorkOSTokenFn
	origLoad := loadKeychainTokensFn
	origSave := saveKeychainTokensFn
	origHTTPClient := workOSRefreshHTTPClient
	origURL := workOSAuthenticateURL
	acquireRefreshLock = func(context.Context) (refreshUnlocker, error) {
		lockMu.Lock()
		return &testRefreshLock{mu: lockMu}, nil
	}
	t.Cleanup(func() {
		acquireRefreshLock = origAcquire
		refreshWorkOSTokenFn = origRefresh
		loadKeychainTokensFn = origLoad
		saveKeychainTokensFn = origSave
		workOSRefreshHTTPClient = origHTTPClient
		workOSAuthenticateURL = origURL
	})
}

// fakeKeychain is a durable in-memory stand-in for the shared "workos-tokens"
// entry: what a save writes is what the next load reads, so tests can prove a
// marker survives across Refresh calls the way the real keychain does.
type fakeKeychain struct {
	mu     sync.Mutex
	record *keychainTokens
}

func (f *fakeKeychain) install(t *testing.T) {
	t.Helper()
	loadKeychainTokensFn = f.load
	saveKeychainTokensFn = f.save
}

func (f *fakeKeychain) load() (*keychainTokens, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.record == nil {
		return nil, errors.New("no tokens stored")
	}
	cpy := *f.record
	return &cpy, nil
}

func (f *fakeKeychain) save(tokens *keychainTokens) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cpy := *tokens
	f.record = &cpy
	return nil
}

func (f *fakeKeychain) snapshot() *keychainTokens {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.record == nil {
		return nil
	}
	cpy := *f.record
	return &cpy
}

func testProvider(tokens *keychainTokens) *KeychainTokenProvider {
	return &KeychainTokenProvider{
		accessToken:  tokens.AccessToken,
		refreshToken: tokens.RefreshToken,
		expiresAt:    tokens.ExpiresAt,
		clientIDEnv:  "BOSS_TEST_WORKOS_CLIENT_ID",
	}
}

// TestKeychainTokensJSONSharedShape pins the on-disk contract with the boss
// CLI: both binaries read and write the same "workos-tokens" item, so an old
// record without the BOS-659 marker must load as a normal logged-in record and
// a clean record must not gain marker noise on the way back out. The mirror of
// this test lives in services/boss/internal/auth.
func TestKeychainTokensJSONSharedShape(t *testing.T) {
	const legacy = `{"access_token":"a","refresh_token":"r","expires_at":"2099-01-01T00:00:00Z","email":"dave@example.com"}`
	var old keychainTokens
	if err := json.Unmarshal([]byte(legacy), &old); err != nil {
		t.Fatalf("unmarshal legacy record: %v", err)
	}
	if old.NeedsRelogin || old.ReloginReason != "" {
		t.Fatalf("legacy record gained a marker: %+v", old)
	}
	if !old.valid() {
		t.Fatal("legacy record should still be valid")
	}
	out, err := json.Marshal(&old)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "needs_relogin") || strings.Contains(string(out), "relogin_reason") {
		t.Fatalf("clean record serialized marker fields: %s", out)
	}

	marked := keychainTokens{
		AccessToken:   "a",
		RefreshToken:  "r",
		ExpiresAt:     time.Now().Add(time.Hour),
		Email:         "dave@example.com",
		NeedsRelogin:  true,
		ReloginReason: reloginReasonRefreshOutcomeUnknown,
	}
	data, err := json.Marshal(&marked)
	if err != nil {
		t.Fatalf("marshal marked: %v", err)
	}
	if !strings.Contains(string(data), `"needs_relogin":true`) {
		t.Fatalf("marked record missing needs_relogin: %s", data)
	}
	if !strings.Contains(string(data), `"relogin_reason":"refresh_outcome_unknown"`) {
		t.Fatalf("marked record missing relogin_reason: %s", data)
	}
	if marked.valid() {
		t.Fatal("a record flagged for re-login must not report valid()")
	}
}

func TestLoadKeychainTokensVersionedRecordIsAuthoritative(t *testing.T) {
	legacy := []byte(`{"access_token":"legacy-access","refresh_token":"legacy-refresh","expires_at":"2099-01-01T00:00:00Z","email":"legacy@example.com"}`)
	versioned := []byte(`{"version":1,"access_token":"current-access","refresh_token":"current-refresh","expires_at":"2099-01-01T00:00:00Z","email":"current@example.com"}`)

	withRing := func(t *testing.T, ring keyring.Keyring) {
		t.Helper()
		original := openKeyring
		openKeyring = func() (keyring.Keyring, error) { return ring, nil }
		t.Cleanup(func() { openKeyring = original })
	}

	t.Run("loads legacy only when authoritative key is absent", func(t *testing.T) {
		withRing(t, keyring.NewArrayKeyring([]keyring.Item{{Key: legacyTokenKey, Data: legacy}}))
		tokens, err := loadKeychainTokens()
		if err != nil || tokens.Email != "legacy@example.com" {
			t.Fatalf("loadKeychainTokens() = (%+v, %v), want legacy tokens", tokens, err)
		}
	})

	t.Run("versioned key wins after an old writer rewrites legacy", func(t *testing.T) {
		withRing(t, keyring.NewArrayKeyring([]keyring.Item{
			{Key: legacyTokenKey, Data: legacy},
			{Key: versionedTokenKey, Data: versioned},
		}))
		tokens, err := loadKeychainTokens()
		if err != nil || tokens.Email != "current@example.com" {
			t.Fatalf("loadKeychainTokens() = (%+v, %v), want versioned tokens", tokens, err)
		}
	})

	t.Run("unsupported version fails closed without replaying legacy", func(t *testing.T) {
		withRing(t, keyring.NewArrayKeyring([]keyring.Item{
			{Key: legacyTokenKey, Data: legacy},
			{Key: versionedTokenKey, Data: []byte(`{"version":999,"access_token":"must-not-load"}`)},
		}))
		_, err := loadKeychainTokens()
		if err == nil {
			t.Fatal("loadKeychainTokens() error = nil, want unsupported-version failure")
		}
		if strings.Contains(err.Error(), "must-not-load") || strings.Contains(err.Error(), "legacy-access") {
			t.Fatalf("load error leaked credential material: %v", err)
		}
	})
}

func TestKeychainTokenProviderRefreshPreservesEmailAndReturnsSaveError(t *testing.T) {
	t.Setenv("BOSS_TEST_WORKOS_CLIENT_ID", "client")
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	old := &keychainTokens{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour),
		Email:        "old@example.com",
	}
	p := testProvider(old)
	loadKeychainTokensFn = func() (*keychainTokens, error) {
		cpy := *old
		return &cpy, nil
	}
	refreshWorkOSTokenFn = func(_ context.Context, _, refreshToken string) (*keychainTokens, error) {
		if refreshToken != "old-refresh" {
			t.Fatalf("refresh token = %q", refreshToken)
		}
		return &keychainTokens{
			AccessToken:  "fresh-access",
			RefreshToken: "fresh-refresh",
			ExpiresAt:    time.Now().Add(time.Hour),
		}, nil
	}
	saveKeychainTokensFn = func(tokens *keychainTokens) error {
		if tokens.Email != "old@example.com" {
			t.Fatalf("saved email = %q, want old@example.com", tokens.Email)
		}
		return errors.New("keychain locked")
	}

	got, err := p.Refresh(context.Background())
	if err == nil || !strings.Contains(err.Error(), "save refreshed tokens") {
		t.Fatalf("Refresh error = %v, want save error", err)
	}
	if got != "fresh-access" {
		t.Fatalf("token = %q, want fresh-access", got)
	}
	if p.Token() != "fresh-access" {
		t.Fatalf("provider token = %q, want fresh-access", p.Token())
	}
}

func TestKeychainTokenProviderRefreshRecoversFromStaleInvalidGrant(t *testing.T) {
	t.Setenv("BOSS_TEST_WORKOS_CLIENT_ID", "client")
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	old := &keychainTokens{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour),
	}
	fresh := &keychainTokens{
		AccessToken:  "fresh-access",
		RefreshToken: "fresh-refresh",
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	p := testProvider(old)
	loads := 0
	loadKeychainTokensFn = func() (*keychainTokens, error) {
		loads++
		if loads == 1 {
			cpy := *old
			return &cpy, nil
		}
		cpy := *fresh
		return &cpy, nil
	}
	refreshWorkOSTokenFn = func(context.Context, string, string) (*keychainTokens, error) {
		return nil, ErrAuthExpired
	}
	saveKeychainTokensFn = func(*keychainTokens) error {
		t.Fatal("stale invalid_grant must not overwrite the fresher keychain record")
		return nil
	}

	got, err := p.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh returned error: %v", err)
	}
	if got != "fresh-access" {
		t.Fatalf("token = %q, want fresh-access", got)
	}
}

// TestKeychainTokenProviderRefreshRetainsRecordOnTerminalInvalidGrant pins
// BOS-659: an authoritative rejection must keep the keychain entry's identity
// and record why re-login is needed, rather than deleting the item. It clears
// token material so an older binary that ignores the marker cannot replay it.
func TestKeychainTokenProviderRefreshRetainsRecordOnTerminalInvalidGrant(t *testing.T) {
	t.Setenv("BOSS_TEST_WORKOS_CLIENT_ID", "client")
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	ring := &fakeKeychain{record: &keychainTokens{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour),
		Email:        "dave@example.com",
	}}
	ring.install(t)
	rec := ring.snapshot()
	p := testProvider(rec)
	refreshWorkOSTokenFn = func(context.Context, string, string) (*keychainTokens, error) {
		return nil, ErrRefreshTokenRejected
	}

	_, err := p.Refresh(context.Background())
	if !errors.Is(err, ErrAuthExpired) {
		t.Fatalf("Refresh error = %v, want ErrAuthExpired", err)
	}
	if !errors.Is(err, ErrRefreshTokenRejected) {
		t.Fatalf("Refresh error = %v, want ErrRefreshTokenRejected", err)
	}
	saved := ring.snapshot()
	if saved == nil {
		t.Fatal("terminal invalid_grant deleted the keychain record")
	}
	if !saved.NeedsRelogin || saved.ReloginReason != reloginReasonRefreshTokenRejected {
		t.Fatalf("saved marker = (%v, %q), want (true, %q)", saved.NeedsRelogin, saved.ReloginReason, reloginReasonRefreshTokenRejected)
	}
	if saved.AccessToken != "" || saved.RefreshToken != "" || saved.Email != "dave@example.com" {
		t.Fatalf("saved record = %+v, want cleared tokens with retained identity", saved)
	}
	if p.Token() != "" {
		t.Fatalf("provider token = %q, want cleared", p.Token())
	}
}

// TestKeychainTokenProviderRefreshMarksUnknownOutcome pins BOS-659's core
// behavior: a refresh whose outcome could not be confirmed preserves the
// identity, clears token material, records the ambiguous reason, and refuses
// to retry even in an older binary that ignores the marker.
func TestKeychainTokenProviderRefreshMarksUnknownOutcome(t *testing.T) {
	t.Setenv("BOSS_TEST_WORKOS_CLIENT_ID", "client")
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	ring := &fakeKeychain{record: &keychainTokens{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour),
		Email:        "dave@example.com",
	}}
	ring.install(t)
	rec := ring.snapshot()
	p := testProvider(rec)

	var exchanges int32
	refreshWorkOSTokenFn = func(context.Context, string, string) (*keychainTokens, error) {
		atomic.AddInt32(&exchanges, 1)
		return nil, fmt.Errorf("%w: %w", ErrRefreshOutcomeUnknown, context.DeadlineExceeded)
	}

	_, err := p.Refresh(context.Background())
	if !errors.Is(err, ErrRefreshOutcomeUnknown) {
		t.Fatalf("Refresh error = %v, want ErrRefreshOutcomeUnknown", err)
	}
	if !errors.Is(err, ErrAuthExpired) {
		t.Fatalf("Refresh error = %v, want it to compose with ErrAuthExpired", err)
	}
	saved := ring.snapshot()
	if saved == nil {
		t.Fatal("ambiguous refresh deleted the keychain record")
	}
	if !saved.NeedsRelogin || saved.ReloginReason != reloginReasonRefreshOutcomeUnknown {
		t.Fatalf("saved marker = (%v, %q), want (true, %q)", saved.NeedsRelogin, saved.ReloginReason, reloginReasonRefreshOutcomeUnknown)
	}
	if saved.RefreshToken != "" || saved.AccessToken != "" || saved.Email != "dave@example.com" {
		t.Fatalf("ambiguous refresh saved record = %+v, want cleared tokens with retained identity", saved)
	}
	if p.Token() != "" {
		t.Fatalf("provider token = %q, want cleared", p.Token())
	}

	// A later Refresh must consult the durable marker and never replay the
	// possibly-consumed refresh token.
	_, err = p.Refresh(context.Background())
	if !errors.Is(err, ErrRefreshOutcomeUnknown) {
		t.Fatalf("second Refresh error = %v, want ErrRefreshOutcomeUnknown", err)
	}
	if got := atomic.LoadInt32(&exchanges); got != 1 {
		t.Fatalf("WorkOS exchanges = %d, want exactly 1 (marked record must not be replayed)", got)
	}

	// A daemon restart reloads the marked record: still no bearer token and
	// still no exchange.
	restarted := &KeychainTokenProvider{clientIDEnv: "BOSS_TEST_WORKOS_CLIENT_ID"}
	restarted.Reload()
	if restarted.Token() != "" {
		t.Fatalf("reloaded provider token = %q, want empty for a marked record", restarted.Token())
	}
	if _, err := restarted.Refresh(context.Background()); !errors.Is(err, ErrRefreshOutcomeUnknown) {
		t.Fatalf("reloaded Refresh error = %v, want ErrRefreshOutcomeUnknown", err)
	}
	if got := atomic.LoadInt32(&exchanges); got != 1 {
		t.Fatalf("WorkOS exchanges after reload = %d, want exactly 1", got)
	}
}

// TestKeychainTokenProviderRefreshKeepsPendingUnknownReason covers the
// documented pending-rotation case: WorkOS confirms the token was already
// exchanged, which matches the earlier ambiguous attempt, so the more precise
// unknown-outcome reason is retained rather than downgraded.
func TestKeychainTokenProviderRefreshKeepsPendingUnknownReason(t *testing.T) {
	t.Setenv("BOSS_TEST_WORKOS_CLIENT_ID", "client")
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	pending := &keychainTokens{
		AccessToken:   "old-access",
		RefreshToken:  "old-refresh",
		ExpiresAt:     time.Now().Add(-time.Hour),
		Email:         "dave@example.com",
		NeedsRelogin:  true,
		ReloginReason: reloginReasonRefreshOutcomeUnknown,
	}
	clean := &keychainTokens{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour),
		Email:        "dave@example.com",
	}
	p := testProvider(clean)
	loads := 0
	loadKeychainTokensFn = func() (*keychainTokens, error) {
		loads++
		if loads == 1 {
			cpy := *clean
			return &cpy, nil
		}
		cpy := *pending
		return &cpy, nil
	}
	var saved *keychainTokens
	saveKeychainTokensFn = func(tokens *keychainTokens) error {
		cpy := *tokens
		saved = &cpy
		return nil
	}
	refreshWorkOSTokenFn = func(context.Context, string, string) (*keychainTokens, error) {
		return nil, ErrRefreshTokenAlreadyExchanged
	}

	_, err := p.Refresh(context.Background())
	if !errors.Is(err, ErrAuthExpired) {
		t.Fatalf("Refresh error = %v, want ErrAuthExpired", err)
	}
	if saved == nil {
		t.Fatal("already-exchanged invalid_grant did not persist the record")
	}
	if !saved.NeedsRelogin || saved.ReloginReason != reloginReasonRefreshOutcomeUnknown {
		t.Fatalf("saved marker = (%v, %q), want the pending %q reason retained", saved.NeedsRelogin, saved.ReloginReason, reloginReasonRefreshOutcomeUnknown)
	}
}

// TestKeychainTokenProviderRefreshClearsMarkerOnSuccess proves the happy path
// writes a clean record so a recovered session is not left flagged.
func TestKeychainTokenProviderRefreshClearsMarkerOnSuccess(t *testing.T) {
	t.Setenv("BOSS_TEST_WORKOS_CLIENT_ID", "client")
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	ring := &fakeKeychain{record: &keychainTokens{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour),
		Email:        "dave@example.com",
	}}
	ring.install(t)
	rec := ring.snapshot()
	p := testProvider(rec)
	refreshWorkOSTokenFn = func(context.Context, string, string) (*keychainTokens, error) {
		return &keychainTokens{
			AccessToken:  "fresh-access",
			RefreshToken: "fresh-refresh",
			ExpiresAt:    time.Now().Add(time.Hour),
		}, nil
	}

	got, err := p.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh returned error: %v", err)
	}
	if got != "fresh-access" {
		t.Fatalf("token = %q, want fresh-access", got)
	}
	saved := ring.snapshot()
	if saved.NeedsRelogin || saved.ReloginReason != "" {
		t.Fatalf("successful refresh saved a marked record: %+v", saved)
	}
}

func TestKeychainTokenProviderRefreshSkipsSaveWhenKeychainChanges(t *testing.T) {
	t.Setenv("BOSS_TEST_WORKOS_CLIENT_ID", "client")
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	old := &keychainTokens{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour),
	}
	newer := &keychainTokens{
		AccessToken:  "login-access",
		RefreshToken: "login-refresh",
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	p := testProvider(old)
	loads := 0
	loadKeychainTokensFn = func() (*keychainTokens, error) {
		loads++
		if loads == 1 {
			cpy := *old
			return &cpy, nil
		}
		cpy := *newer
		return &cpy, nil
	}
	refreshWorkOSTokenFn = func(_ context.Context, _, refreshToken string) (*keychainTokens, error) {
		if refreshToken != "old-refresh" {
			t.Fatalf("refresh token = %q", refreshToken)
		}
		return &keychainTokens{
			AccessToken:  "stale-success",
			RefreshToken: "stale-refresh",
			ExpiresAt:    time.Now().Add(time.Hour),
		}, nil
	}
	saveKeychainTokensFn = func(*keychainTokens) error {
		t.Fatal("stale refresh result should not overwrite newer keychain tokens")
		return nil
	}

	got, err := p.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh returned error: %v", err)
	}
	if got != "login-access" {
		t.Fatalf("token = %q, want login-access", got)
	}
	if p.Token() != "login-access" {
		t.Fatalf("provider token = %q, want login-access", p.Token())
	}
}

func TestKeychainTokenProviderRefreshUsesCurrentEmailAfterKeychainChange(t *testing.T) {
	t.Setenv("BOSS_TEST_WORKOS_CLIENT_ID", "client")
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	old := &keychainTokens{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour),
		Email:        "old@example.com",
	}
	current := &keychainTokens{
		AccessToken:  "login-expired",
		RefreshToken: "login-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour),
		Email:        "login@example.com",
	}
	p := testProvider(old)
	loads := 0
	loadKeychainTokensFn = func() (*keychainTokens, error) {
		loads++
		if loads == 1 {
			cpy := *old
			return &cpy, nil
		}
		cpy := *current
		return &cpy, nil
	}
	refreshWorkOSTokenFn = func(_ context.Context, _, refreshToken string) (*keychainTokens, error) {
		return &keychainTokens{
			AccessToken:  "fresh-" + refreshToken,
			RefreshToken: "fresh-refresh",
			ExpiresAt:    time.Now().Add(time.Hour),
		}, nil
	}
	saveKeychainTokensFn = func(tokens *keychainTokens) error {
		if tokens.Email != "login@example.com" {
			t.Fatalf("saved email = %q, want login@example.com", tokens.Email)
		}
		return nil
	}

	got, err := p.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh returned error: %v", err)
	}
	if got != "fresh-login-refresh" {
		t.Fatalf("token = %q, want fresh-login-refresh", got)
	}
}

// TestKeychainTokenProvider_Refresh_CoalescesConcurrentCalls verifies that
// concurrent Refresh calls (the DaemonStream + TerminalStream openers racing on
// an expired token) perform exactly one WorkOS exchange. Without the
// single-flight wrapper each goroutine would acquire the refresh lock serially
// and drive its own exchange, double-spending the rotating refresh token. See
// BOS-44 Strand B.
func TestKeychainTokenProvider_Refresh_CoalescesConcurrentCalls(t *testing.T) {
	t.Setenv("BOSS_TEST_WORKOS_CLIENT_ID", "client")
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	old := &keychainTokens{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour),
	}
	p := testProvider(old)
	loadKeychainTokensFn = func() (*keychainTokens, error) {
		cpy := *old
		return &cpy, nil
	}
	saveKeychainTokensFn = func(*keychainTokens) error { return nil }

	var exchanges int32
	leaderInFlight := make(chan struct{})
	release := make(chan struct{})
	refreshWorkOSTokenFn = func(_ context.Context, _, refreshToken string) (*keychainTokens, error) {
		// Only the single-flight leader reaches here. Signal once, then block
		// so the followers are guaranteed to coalesce onto this in-flight call
		// rather than starting their own exchange.
		if atomic.AddInt32(&exchanges, 1) == 1 {
			close(leaderInFlight)
		}
		<-release
		return &keychainTokens{
			AccessToken:  "fresh-access",
			RefreshToken: "fresh-refresh",
			ExpiresAt:    time.Now().Add(time.Hour),
		}, nil
	}

	const n = 5
	results := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		results[0], errs[0] = p.Refresh(context.Background())
	}()

	// Wait until the leader is inside the (single) exchange and blocked on
	// release; it now holds the single-flight slot.
	<-leaderInFlight

	for i := 1; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = p.Refresh(context.Background())
		}(i)
	}

	// Give the followers time to reach singleflight.Do and coalesce before the
	// leader returns. The leader cannot proceed until release is closed, so this
	// only bounds how long we wait for the followers to join — not correctness.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&exchanges); got != 1 {
		t.Fatalf("WorkOS exchanges = %d, want exactly 1", got)
	}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("Refresh[%d] error: %v", i, errs[i])
		}
		if results[i] != "fresh-access" {
			t.Fatalf("Refresh[%d] = %q, want fresh-access", i, results[i])
		}
	}
}

func TestRefreshWorkOSTokenHonorsContext(t *testing.T) {
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	workOSAuthenticateURL = srv.URL
	workOSRefreshHTTPClient = srv.Client()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := refreshWorkOSToken(ctx, "client", "refresh")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("refreshWorkOSToken error = %v, want context deadline exceeded", err)
	}
	// The request was dispatched, so WorkOS may already have consumed and
	// rotated the refresh token: the outcome is unknown, not a rejection.
	if !errors.Is(err, ErrRefreshOutcomeUnknown) {
		t.Fatalf("refreshWorkOSToken error = %v, want ErrRefreshOutcomeUnknown", err)
	}
}

func TestRefreshWorkOSTokenClassifiesBodyReadFailureAsUnknown(t *testing.T) {
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Promise more bytes than we write, then return: the client sees a
		// truncated body and fails inside io.ReadAll.
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"access_token":`))
	}))
	defer srv.Close()
	workOSAuthenticateURL = srv.URL
	workOSRefreshHTTPClient = srv.Client()

	_, err := refreshWorkOSToken(context.Background(), "client", "refresh")
	if !errors.Is(err, ErrRefreshOutcomeUnknown) {
		t.Fatalf("refreshWorkOSToken error = %v, want ErrRefreshOutcomeUnknown", err)
	}
}

func TestRefreshWorkOSTokenTreatsTruncatedFailureResponseAsAuthoritative(t *testing.T) {
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"temporarily_unavailable"}`))
	}))
	defer srv.Close()
	workOSAuthenticateURL = srv.URL
	workOSRefreshHTTPClient = srv.Client()

	_, err := refreshWorkOSToken(context.Background(), "client", "refresh")
	if err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("refreshWorkOSToken error = %v, want HTTP 503", err)
	}
	if errors.Is(err, ErrRefreshOutcomeUnknown) || errors.Is(err, ErrAuthExpired) {
		t.Fatalf("refreshWorkOSToken error = %v, want authoritative transient failure", err)
	}
}

func TestRefreshWorkOSTokenRequestBuildFailureIsNotAmbiguous(t *testing.T) {
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	// Never dispatched, so nothing could have consumed the refresh token.
	workOSAuthenticateURL = "://not-a-url"

	_, err := refreshWorkOSToken(context.Background(), "client", "refresh")
	if err == nil {
		t.Fatal("refreshWorkOSToken error = nil, want request build failure")
	}
	if errors.Is(err, ErrRefreshOutcomeUnknown) || errors.Is(err, ErrAuthExpired) {
		t.Fatalf("refreshWorkOSToken error = %v, want a plain transient error", err)
	}
}

func TestRefreshWorkOSTokenClassifiesInvalidGrant(t *testing.T) {
	const secret = "sso_refresh_super_secret_value"
	tests := []struct {
		name            string
		body            string
		wantAlready     bool
		wantNotAmbigous bool
	}{
		{
			name: "plain invalid_grant",
			body: `{"error":"invalid_grant","error_description":"Invalid refresh token: ` + secret + `"}`,
		},
		{
			name:        "already exchanged",
			body:        `{"error":"invalid_grant","error_description":"Refresh token already exchanged."}`,
			wantAlready: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var lockMu sync.Mutex
			withTokenRefreshHooks(t, &lockMu)

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			workOSAuthenticateURL = srv.URL
			workOSRefreshHTTPClient = srv.Client()

			_, err := refreshWorkOSToken(context.Background(), "client", "refresh")
			if !errors.Is(err, ErrRefreshTokenRejected) {
				t.Fatalf("error = %v, want ErrRefreshTokenRejected", err)
			}
			if !errors.Is(err, ErrAuthExpired) {
				t.Fatalf("error = %v, want it to compose with ErrAuthExpired", err)
			}
			if errors.Is(err, ErrRefreshOutcomeUnknown) {
				t.Fatalf("error = %v, an authoritative rejection is not ambiguous", err)
			}
			if got := errors.Is(err, ErrRefreshTokenAlreadyExchanged); got != tc.wantAlready {
				t.Fatalf("already-exchanged = %v, want %v (err %v)", got, tc.wantAlready, err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error text leaked the response body: %v", err)
			}
		})
	}
}

func TestRefreshWorkOSTokenDoesNotLeakResponseBody(t *testing.T) {
	const secret = "internal-diagnostic-payload-42"
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(secret))
	}))
	defer srv.Close()
	workOSAuthenticateURL = srv.URL
	workOSRefreshHTTPClient = srv.Client()

	_, err := refreshWorkOSToken(context.Background(), "client", "refresh")
	if err == nil {
		t.Fatal("refreshWorkOSToken error = nil, want HTTP 500 failure")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error text leaked the response body: %v", err)
	}
}

// TestRefreshWorkOSTokenUndispatchedDialFailureIsNotAmbiguous pins the
// boundary the ambiguity classification actually turns on: the request BYTES,
// not the call to Do. A connection that never establishes (DNS failure,
// connection refused, TLS failure, a context cancelled while dialling) means
// WorkOS provably never saw the exchange, so the one-shot refresh token was
// provably not consumed and the correct behaviour is a plain retryable error.
// Classifying it as ambiguous would durably log the user out every time the
// laptop woke up without a network — the very outage BOS-659 exists to
// survive.
func TestRefreshWorkOSTokenUndispatchedDialFailureIsNotAmbiguous(t *testing.T) {
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	workOSAuthenticateURL = srv.URL
	workOSRefreshHTTPClient = srv.Client()
	// Close before dialling: nothing is listening, so the request bytes are
	// never written.
	srv.Close()

	_, err := refreshWorkOSToken(context.Background(), "client", "refresh")
	if err == nil {
		t.Fatal("refreshWorkOSToken error = nil, want a dial failure")
	}
	if errors.Is(err, ErrRefreshOutcomeUnknown) || errors.Is(err, ErrAuthExpired) {
		t.Fatalf("refreshWorkOSToken error = %v, want a plain retryable error", err)
	}
}

// TestRefreshWorkOSTokenUndecodableSuccessIsAmbiguous covers the mirror image
// of the body-read failure: HTTP 200 means WorkOS answered and therefore
// rotated the one-shot token, and we failed to read the replacement out of the
// response. Retrying the old token would replay a definitely-consumed
// credential, so this must be ambiguous, and the undecodable bytes must not
// reach the error string.
func TestRefreshWorkOSTokenUndecodableSuccessIsAmbiguous(t *testing.T) {
	const secret = "sso_refresh_super_secret_value"
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json at all " + secret))
	}))
	defer srv.Close()
	workOSAuthenticateURL = srv.URL
	workOSRefreshHTTPClient = srv.Client()

	_, err := refreshWorkOSToken(context.Background(), "client", "refresh")
	if !errors.Is(err, ErrRefreshOutcomeUnknown) {
		t.Fatalf("refreshWorkOSToken error = %v, want ErrRefreshOutcomeUnknown", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("undecodable response body leaked into the error: %q", err.Error())
	}
}

// TestKeychainTokenProviderRefreshDoesNotMarkFresherKeychainRecord is the AC3
// guard for the marker write-back. An older or external client could save the
// shared record without taking the WorkOS refresh lock while the daemon's
// exchange is failing. A stale caller must not flag those fresh credentials —
// it must adopt them and leave the durable record untouched.
func TestKeychainTokenProviderRefreshDoesNotMarkFresherKeychainRecord(t *testing.T) {
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	past := time.Now().Add(-time.Hour)
	ring := &fakeKeychain{record: &keychainTokens{
		AccessToken:  "stale-access",
		RefreshToken: "stale-refresh",
		ExpiresAt:    past,
		Email:        "user@example.com",
	}}
	ring.install(t)

	refreshWorkOSTokenFn = func(context.Context, string, string) (*keychainTokens, error) {
		// A concurrent `boss login` lands mid-exchange.
		if err := ring.save(&keychainTokens{
			AccessToken:  "login-access",
			RefreshToken: "login-refresh",
			ExpiresAt:    time.Now().Add(time.Hour),
			Email:        "user@example.com",
		}); err != nil {
			t.Fatalf("seed concurrent login: %v", err)
		}
		return nil, fmt.Errorf("%w: dial timeout", ErrRefreshOutcomeUnknown)
	}

	p := testProvider(&keychainTokens{AccessToken: "stale-access", RefreshToken: "stale-refresh", ExpiresAt: past})
	tok, err := p.Refresh(context.Background())
	// The adopted credentials are usable, so this must NOT be reported as a
	// terminal re-login state: the openers and the periodic refresher answer
	// ErrAuthExpired by parking on AuthState, and only a login's NotifyLogin
	// ever un-parks them — one that already fired would strand the daemon.
	if err != nil {
		t.Fatalf("Refresh error = %v, want the adopted credentials", err)
	}
	if tok != "login-access" {
		t.Fatalf("Refresh token = %q, want the adopted login-access", tok)
	}

	got := ring.snapshot()
	if got.NeedsRelogin {
		t.Fatal("a stale caller flagged the credentials a concurrent login had just written")
	}
	if got.RefreshToken != "login-refresh" {
		t.Fatalf("stored refresh token = %q, want the newer login-refresh", got.RefreshToken)
	}
	if p.Token() != "login-access" {
		t.Fatalf("provider token = %q, want the adopted login-access", p.Token())
	}
	if reason := p.ReloginReason(); reason != "" {
		t.Fatalf("provider relogin reason = %q, want none", reason)
	}
}

// TestKeychainTokenProviderRefreshAmbiguousAfterStaleRecoveryClearsNewerToken
// covers the second loop attempt end-to-end. After the stale-invalid_grant
// recovery adopts a newer refresh token, an ambiguous outcome on that second
// attempt must flag the NEWER record and clear its token material — writing
// back the pre-recovery snapshot would persist the older token, while keeping
// the newer token would let an older binary replay it.
//
// Two separate guards produce this outcome: `latest = reloaded` at the
// recovery `continue`, and markNeedsRelogin's re-read. This test pins the
// PROPERTY, so it stays green with either one alone; the sibling below
// isolates the first by making the re-read fail.
func TestKeychainTokenProviderRefreshAmbiguousAfterStaleRecoveryClearsNewerToken(t *testing.T) {
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	past := time.Now().Add(-time.Hour)
	ring := &fakeKeychain{record: &keychainTokens{
		AccessToken:  "stale-access",
		RefreshToken: "stale-refresh",
		ExpiresAt:    past,
		Email:        "user@example.com",
	}}
	ring.install(t)

	var calls int
	refreshWorkOSTokenFn = func(_ context.Context, _, refreshTok string) (*keychainTokens, error) {
		calls++
		switch calls {
		case 1:
			if refreshTok != "stale-refresh" {
				t.Fatalf("attempt 1 used %q, want stale-refresh", refreshTok)
			}
			// Another process rotated the record while we were exchanging;
			// its access token is already expired, so recovery has to retry
			// rather than adopt it wholesale.
			if err := ring.save(&keychainTokens{
				AccessToken:  "newer-access",
				RefreshToken: "newer-refresh",
				ExpiresAt:    past,
				Email:        "user@example.com",
			}); err != nil {
				t.Fatalf("seed newer record: %v", err)
			}
			return nil, fmt.Errorf("refresh failed (HTTP 400): %w", ErrRefreshTokenRejected)
		case 2:
			if refreshTok != "newer-refresh" {
				t.Fatalf("attempt 2 used %q, want newer-refresh", refreshTok)
			}
			return nil, fmt.Errorf("%w: dial timeout", ErrRefreshOutcomeUnknown)
		}
		t.Fatalf("unexpected exchange attempt %d", calls)
		return nil, nil
	}

	p := testProvider(&keychainTokens{AccessToken: "stale-access", RefreshToken: "stale-refresh", ExpiresAt: past})
	if _, err := p.Refresh(context.Background()); !errors.Is(err, ErrRefreshOutcomeUnknown) {
		t.Fatalf("Refresh error = %v, want ErrRefreshOutcomeUnknown", err)
	}

	got := ring.snapshot()
	if got == nil {
		t.Fatal("the keychain record was removed")
	}
	if got.AccessToken != "" || got.RefreshToken != "" {
		t.Fatalf("stored tokens = (%q, %q), want cleared tokens", got.AccessToken, got.RefreshToken)
	}
	if !got.NeedsRelogin || got.ReloginReason != reloginReasonRefreshOutcomeUnknown {
		t.Fatalf("stored marker = (%v, %q), want the unknown-outcome reason", got.NeedsRelogin, got.ReloginReason)
	}
	if got.Email != "user@example.com" {
		t.Fatalf("stored email = %q, want the retained identity", got.Email)
	}
}

// TestKeychainTokenProviderRefreshUnreadableKeychainDoesNotExchange pins the
// pre-exchange rule: a daemon must not replay a cached refresh token when it
// cannot verify the shared keychain record under the refresh lock.
func TestKeychainTokenProviderRefreshUnreadableKeychainDoesNotExchange(t *testing.T) {
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	errKeychainLocked := errors.New("keychain locked")
	loadKeychainTokensFn = func() (*keychainTokens, error) {
		return nil, errKeychainLocked
	}
	var saves int
	saveKeychainTokensFn = func(*keychainTokens) error {
		saves++
		return nil
	}
	var exchanges int
	refreshWorkOSTokenFn = func(context.Context, string, string) (*keychainTokens, error) {
		exchanges++
		return nil, fmt.Errorf("%w: dial timeout", ErrRefreshOutcomeUnknown)
	}

	p := testProvider(&keychainTokens{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Hour)})
	_, err := p.Refresh(context.Background())
	if !errors.Is(err, errKeychainLocked) {
		t.Fatalf("Refresh error = %v, want locked keychain error", err)
	}
	if exchanges != 0 {
		t.Fatalf("exchanges = %d, want 0 — an unreadable record must not be replayed", exchanges)
	}
	if saves != 0 {
		t.Fatalf("saves = %d, want 0 — an unreadable record must not be overwritten", saves)
	}
	if p.Token() != "access" {
		t.Fatalf("provider token = %q, want cached token retained for a later retry", p.Token())
	}
	if reason := p.ReloginReason(); reason != "" {
		t.Fatalf("provider relogin reason = %q, want no terminal state", reason)
	}
}

// TestKeychainTokenProviderRefreshDoesNotRestoreLoggedOutRecord pins the
// logout race: an explicit logout may remove the shared item after a refresh
// starts but before its terminal failure is marked. The daemon must not
// recreate the deleted item from its stale in-memory snapshot.
func TestKeychainTokenProviderRefreshDoesNotRestoreLoggedOutRecord(t *testing.T) {
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	stale := &keychainTokens{
		AccessToken:  "stale-access",
		RefreshToken: "stale-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour),
	}
	loads := 0
	loadKeychainTokensFn = func() (*keychainTokens, error) {
		loads++
		if loads == 1 {
			cpy := *stale
			return &cpy, nil
		}
		return nil, keyring.ErrKeyNotFound
	}
	var saves int
	saveKeychainTokensFn = func(*keychainTokens) error {
		saves++
		return nil
	}
	refreshWorkOSTokenFn = func(context.Context, string, string) (*keychainTokens, error) {
		return nil, fmt.Errorf("%w: dial timeout", ErrRefreshOutcomeUnknown)
	}

	p := testProvider(stale)
	_, err := p.Refresh(context.Background())
	if !errors.Is(err, ErrRefreshOutcomeUnknown) || !errors.Is(err, errReloginMarkerNotPersisted) {
		t.Fatalf("Refresh error = %v, want ambiguous outcome with undurable marker", err)
	}
	if !errors.Is(err, keyring.ErrKeyNotFound) {
		t.Fatalf("Refresh error = %v, want missing keychain record", err)
	}
	if saves != 0 {
		t.Fatalf("saves = %d, want 0 — logout must not be undone by a stale refresh", saves)
	}
	if p.Token() != "" {
		t.Fatalf("provider token = %q, want the cache disabled", p.Token())
	}
}

// TestKeychainTokenProviderRefreshSupersededByRotationIsNotTerminal covers the
// other half of the adopt path: the credentials another process stored are
// newer but not yet usable (expired access token, fresh refresh token). The
// attempt is obsolete, not terminal — returning an ErrAuthExpired-composing
// error would park both stream loops behind AuthState with a clean, unmarked
// keychain record and nothing left to call MarkOK.
func TestKeychainTokenProviderRefreshSupersededByRotationIsNotTerminal(t *testing.T) {
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	past := time.Now().Add(-time.Hour)
	ring := &fakeKeychain{record: &keychainTokens{
		AccessToken:  "stale-access",
		RefreshToken: "stale-refresh",
		ExpiresAt:    past,
		Email:        "user@example.com",
	}}
	ring.install(t)

	refreshWorkOSTokenFn = func(context.Context, string, string) (*keychainTokens, error) {
		if err := ring.save(&keychainTokens{
			AccessToken:  "rotated-access",
			RefreshToken: "rotated-refresh",
			ExpiresAt:    past,
			Email:        "user@example.com",
		}); err != nil {
			t.Fatalf("seed rotated record: %v", err)
		}
		return nil, fmt.Errorf("%w: dial timeout", ErrRefreshOutcomeUnknown)
	}

	p := testProvider(&keychainTokens{AccessToken: "stale-access", RefreshToken: "stale-refresh", ExpiresAt: past})
	_, err := p.Refresh(context.Background())
	if err == nil {
		t.Fatal("Refresh error = nil, want the superseded error")
	}
	if errors.Is(err, ErrAuthExpired) {
		t.Fatalf("Refresh error = %v, must not compose with ErrAuthExpired (it would park the stream loops)", err)
	}
	if got := ring.snapshot(); got.NeedsRelogin {
		t.Fatal("a superseded attempt flagged credentials it did not own")
	}
	// The adopted refresh token is what the next attempt exchanges.
	if reason := p.ReloginReason(); reason != "" {
		t.Fatalf("provider relogin reason = %q, want none", reason)
	}
}

// TestRefreshWorkOSTokenTokenlessSuccessIsAmbiguous pins the well-formed
// sibling of the undecodable-200 case: WorkOS answered 200 (so it rotated the
// one-shot token) but sent no access token back. Saving that verbatim would
// overwrite the good record with a token-less one.
func TestRefreshWorkOSTokenTokenlessSuccessIsAmbiguous(t *testing.T) {
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"expires_in":300}`))
	}))
	defer srv.Close()
	workOSAuthenticateURL = srv.URL
	workOSRefreshHTTPClient = srv.Client()

	if _, err := refreshWorkOSToken(context.Background(), "client", "refresh"); !errors.Is(err, ErrRefreshOutcomeUnknown) {
		t.Fatalf("refreshWorkOSToken error = %v, want ErrRefreshOutcomeUnknown", err)
	}
}

// flakyWriteConn simulates an interface that goes away under a pooled
// keep-alive connection: the socket is open, but every write fails having
// written nothing (VPN drop, sleep, NAT timeout).
type flakyWriteConn struct {
	net.Conn
	fail *atomic.Bool
}

func (c *flakyWriteConn) Write(b []byte) (int, error) {
	if c.fail.Load() {
		return 0, &net.OpError{Op: "write", Net: "tcp", Err: errors.New("network is unreachable")}
	}
	return c.Conn.Write(b)
}

// TestRefreshWorkOSTokenRetryDoesNotInheritDispatchFlag is the guard for the
// unsafe direction of the dispatched-or-not gate, and the sole regression test
// on the decision of whether a one-shot refresh token may be replayed.
//
// net/http hands out a POOLED connection — GotConn fires, so the exchange
// looks dispatched — then the write fails having written nothing and net/http
// retries on a fresh dial that is refused. No byte ever left the host. Without
// the per-attempt GetConn reset the retry inherits the pooled attempt's flag,
// so a pure connectivity outage (VPN drop, sleep, NAT timeout) is misreported
// as an ambiguous exchange and durably logs the user out — the exact failure
// BOS-659 exists to eliminate.
//
// The harness reproduces that interleaving deterministically: one real round
// trip warms the pool, then writes on the pooled connection fail with nothing
// written and the retry's dial is refused.
func TestRefreshWorkOSTokenRetryDoesNotInheritDispatchFlag(t *testing.T) {
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"access_token":"a","refresh_token":"r","expires_in":300}`))
	}))
	defer srv.Close()

	var failWrite atomic.Bool
	var dials atomic.Int32
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if dials.Add(1) > 1 {
				// The retry's dial: nothing is reachable any more.
				return nil, &net.OpError{Op: "dial", Net: network, Err: errors.New("network is unreachable")}
			}
			conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &flakyWriteConn{Conn: conn, fail: &failWrite}, nil
		},
	}
	defer transport.CloseIdleConnections()
	workOSAuthenticateURL = srv.URL
	workOSRefreshHTTPClient = &http.Client{Transport: transport}

	// Warm the connection pool with a real round trip.
	if _, err := refreshWorkOSToken(context.Background(), "client", "refresh"); err != nil {
		t.Fatalf("warm-up refresh failed: %v", err)
	}
	failWrite.Store(true)

	_, err := refreshWorkOSToken(context.Background(), "client", "refresh")
	if err == nil {
		t.Fatal("refreshWorkOSToken error = nil, want a connectivity failure")
	}
	if dials.Load() < 2 {
		t.Fatalf("dials = %d, want a retry after the pooled write failed (harness did not reproduce)", dials.Load())
	}
	if errors.Is(err, ErrRefreshOutcomeUnknown) || errors.Is(err, ErrAuthExpired) {
		t.Fatalf("refreshWorkOSToken error = %v, want a plain retryable error", err)
	}
}

// TestKeychainTokenProviderRefreshDoesNotWriteWhenMarkerReadFails ensures a
// transient final keychain read never writes a re-login marker from an
// unverified in-memory snapshot, which could overwrite newer credentials.
func TestKeychainTokenProviderRefreshDoesNotWriteWhenMarkerReadFails(t *testing.T) {
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	past := time.Now().Add(-time.Hour)
	stale := &keychainTokens{AccessToken: "stale-access", RefreshToken: "stale-refresh", ExpiresAt: past, Email: "user@example.com"}
	newer := &keychainTokens{AccessToken: "newer-access", RefreshToken: "newer-refresh", ExpiresAt: past, Email: "user@example.com"}
	errKeychainBusy := errors.New("keychain busy")

	var loads int
	loadKeychainTokensFn = func() (*keychainTokens, error) {
		loads++
		switch loads {
		case 1:
			cpy := *stale
			return &cpy, nil
		case 2:
			cpy := *newer
			return &cpy, nil
		}
		// The third load is markNeedsRelogin's re-read.
		return nil, errKeychainBusy
	}
	var saved *keychainTokens
	saveKeychainTokensFn = func(tokens *keychainTokens) error {
		cpy := *tokens
		saved = &cpy
		return nil
	}

	var calls int
	refreshWorkOSTokenFn = func(_ context.Context, _, refreshTok string) (*keychainTokens, error) {
		calls++
		if calls == 1 {
			return nil, fmt.Errorf("refresh failed (HTTP 400): %w", ErrRefreshTokenRejected)
		}
		if refreshTok != "newer-refresh" {
			t.Fatalf("attempt 2 used %q, want newer-refresh", refreshTok)
		}
		return nil, fmt.Errorf("%w: dial timeout", ErrRefreshOutcomeUnknown)
	}

	p := testProvider(&keychainTokens{AccessToken: "stale-access", RefreshToken: "stale-refresh", ExpiresAt: past})
	_, err := p.Refresh(context.Background())
	if !errors.Is(err, ErrRefreshOutcomeUnknown) || !errors.Is(err, errReloginMarkerNotPersisted) || !errors.Is(err, errKeychainBusy) {
		t.Fatalf("Refresh error = %v, want ambiguous outcome with undurable marker", err)
	}
	if saved != nil {
		t.Fatalf("saved record = %+v, want no write from an unverified snapshot", saved)
	}
	if p.Token() != "" {
		t.Fatalf("provider token = %q, want cache disabled", p.Token())
	}
}

// TestKeychainTokenProviderRefreshLoopExhaustionIsNotTerminal pins the loop's
// fall-through. Both recovery paths adopt newer, UNFLAGGED credentials, so
// exhausting the two attempts means this caller kept being superseded — not
// that the user must sign in again. An ErrAuthExpired here would park both
// stream loops behind AuthState with a clean record on disk and nothing left
// to call MarkOK.
func TestKeychainTokenProviderRefreshLoopExhaustionIsNotTerminal(t *testing.T) {
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	past := time.Now().Add(-time.Hour)
	ring := &fakeKeychain{record: &keychainTokens{AccessToken: "a0", RefreshToken: "r0", ExpiresAt: past}}
	ring.install(t)

	var calls int
	refreshWorkOSTokenFn = func(context.Context, string, string) (*keychainTokens, error) {
		calls++
		// Each failure races a peer that has already rotated to a newer —
		// but still expired — record, so both attempts take the recovery
		// `continue` and the loop runs out.
		if err := ring.save(&keychainTokens{
			AccessToken:  fmt.Sprintf("a%d", calls),
			RefreshToken: fmt.Sprintf("r%d", calls),
			ExpiresAt:    past,
		}); err != nil {
			t.Fatalf("seed rotation %d: %v", calls, err)
		}
		return nil, fmt.Errorf("refresh failed (HTTP 400): %w", ErrRefreshTokenRejected)
	}

	p := testProvider(&keychainTokens{AccessToken: "a0", RefreshToken: "r0", ExpiresAt: past})
	_, err := p.Refresh(context.Background())
	if err == nil {
		t.Fatal("Refresh error = nil, want the superseded error")
	}
	if errors.Is(err, ErrAuthExpired) {
		t.Fatalf("Refresh error = %v, must not compose with ErrAuthExpired (it would park the stream loops)", err)
	}
	if calls != 2 {
		t.Fatalf("exchange attempts = %d, want the loop's 2", calls)
	}
	if got := ring.snapshot(); got.NeedsRelogin {
		t.Fatal("loop exhaustion flagged a record it had been superseded on")
	}
}

// TestKeychainTokenProviderReloadClearsMarkerAfterLogin pins the daemon half
// of "boss login clears the marker and the streams resume". A flagged provider
// returns before any keychain read, and the periodic refresher skips a zero
// expiresAt, so applyTokensLocked's reset inside Reload() is the ONLY path
// that un-sticks the daemon. Dropping it would park both stream loops forever
// with good credentials on disk — and every other test would stay green.
func TestKeychainTokenProviderReloadClearsMarkerAfterLogin(t *testing.T) {
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	ring := &fakeKeychain{record: &keychainTokens{
		AccessToken:   "stale",
		RefreshToken:  "r",
		ExpiresAt:     time.Now().Add(-time.Hour),
		Email:         "user@example.com",
		NeedsRelogin:  true,
		ReloginReason: reloginReasonRefreshOutcomeUnknown,
	}}
	ring.install(t)

	p := &KeychainTokenProvider{clientIDEnv: "BOSS_TEST_WORKOS_CLIENT_ID"}
	p.Reload()
	if reason := p.ReloginReason(); reason != reloginReasonRefreshOutcomeUnknown {
		t.Fatalf("provider relogin reason = %q, want the persisted marker", reason)
	}
	if p.Token() != "" {
		t.Fatalf("a flagged record exposed the bearer token %q", p.Token())
	}

	// `boss login` writes a clean record, then notifies the daemon, which
	// calls Reload() before re-registering and MarkOK.
	if err := ring.save(&keychainTokens{
		AccessToken:  "fresh",
		RefreshToken: "fresh-refresh",
		ExpiresAt:    time.Now().Add(time.Hour),
		Email:        "user@example.com",
	}); err != nil {
		t.Fatalf("seed login: %v", err)
	}
	p.Reload()

	if reason := p.ReloginReason(); reason != "" {
		t.Fatalf("Reload left the marker set (%q); the daemon would stay paused after a login", reason)
	}
	if p.Token() != "fresh" {
		t.Fatalf("provider token = %q, want the freshly logged-in token", p.Token())
	}
	// And the provider must be willing to exchange again.
	refreshWorkOSTokenFn = func(context.Context, string, string) (*keychainTokens, error) {
		return &keychainTokens{AccessToken: "rotated", RefreshToken: "rotated-refresh", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	if tok, err := p.Refresh(context.Background()); err != nil || tok != "rotated" {
		t.Fatalf("Refresh after login = (%q, %v), want a normal rotation", tok, err)
	}
}

// TestKeychainTokenProviderRefreshDoesNotSignBackInAfterLogout covers the
// SUCCESS path of the logout race, which
// TestKeychainTokenProviderRefreshDoesNotRestoreLoggedOutRecord (a terminal
// FAILURE) does not reach: logout performs no WorkOS revoke, so the cached
// refresh token still exchanges cleanly. Every keychain read reports the item
// gone — `boss logout` deleted it and its NotifyAuthChange to the daemon is
// best-effort — so the provider must recognise the deletion, decline to
// exchange at all, and never write the rotated tokens back. Saving them would
// recreate the item and silently sign the user back in.
func TestKeychainTokenProviderRefreshDoesNotSignBackInAfterLogout(t *testing.T) {
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	loadKeychainTokensFn = func() (*keychainTokens, error) {
		return nil, fs.ErrNotExist
	}
	var saves int
	saveKeychainTokensFn = func(*keychainTokens) error {
		saves++
		return nil
	}
	var exchanges int
	refreshWorkOSTokenFn = func(context.Context, string, string) (*keychainTokens, error) {
		exchanges++
		return &keychainTokens{
			AccessToken:  "resurrected-access",
			RefreshToken: "resurrected-refresh",
			ExpiresAt:    time.Now().Add(time.Hour),
			Email:        "user@example.com",
		}, nil
	}

	p := testProvider(&keychainTokens{
		AccessToken:  "stale-access",
		RefreshToken: "stale-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour),
	})
	tok, err := p.Refresh(context.Background())
	if !errors.Is(err, ErrAuthExpired) {
		t.Fatalf("Refresh error = %v, want an ErrAuthExpired-composing pause", err)
	}
	if tok != "" {
		t.Fatalf("Refresh token = %q, want none after logout", tok)
	}
	if exchanges != 0 {
		t.Fatalf("exchanges = %d, want 0 — a deleted record must not drive a WorkOS exchange", exchanges)
	}
	if saves != 0 {
		t.Fatalf("saves = %d, want 0 — a successful exchange must not recreate a deleted record", saves)
	}
	if p.Token() != "" {
		t.Fatalf("provider token = %q, want the cache dropped so the daemon stops sending it", p.Token())
	}
	if reason := p.ReloginReason(); reason != "" {
		t.Fatalf("provider relogin reason = %q, want none — the record is gone, not retained", reason)
	}
}

// TestKeychainTokenProviderRefreshDoesNotExchangeWhenPreflightReadFails
// ensures a transient preflight read failure cannot dispatch a stale cached
// refresh token, even when a later read would find the record deleted.
func TestKeychainTokenProviderRefreshDoesNotExchangeWhenPreflightReadFails(t *testing.T) {
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	loads := 0
	loadKeychainTokensFn = func() (*keychainTokens, error) {
		loads++
		if loads == 1 {
			return nil, errors.New("keychain temporarily unreadable")
		}
		return nil, keyring.ErrKeyNotFound
	}
	var saves int
	saveKeychainTokensFn = func(*keychainTokens) error {
		saves++
		return nil
	}
	var exchanges int
	refreshWorkOSTokenFn = func(context.Context, string, string) (*keychainTokens, error) {
		exchanges++
		return &keychainTokens{
			AccessToken:  "resurrected-access",
			RefreshToken: "resurrected-refresh",
			ExpiresAt:    time.Now().Add(time.Hour),
		}, nil
	}

	p := testProvider(&keychainTokens{
		AccessToken:  "stale-access",
		RefreshToken: "stale-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour),
	})
	tok, err := p.Refresh(context.Background())
	if err == nil || errors.Is(err, ErrAuthExpired) {
		t.Fatalf("Refresh error = %v, want retryable preflight read error", err)
	}
	if tok != "" {
		t.Fatalf("Refresh token = %q, want none once the record is gone", tok)
	}
	if exchanges != 0 {
		t.Fatalf("exchanges = %d, want 0 — a transient read error must block the exchange", exchanges)
	}
	if saves != 0 {
		t.Fatalf("saves = %d, want 0 — the rotated tokens must not recreate a deleted record", saves)
	}
	if p.Token() != "stale-access" {
		t.Fatalf("provider token = %q, want cached token retained for a later retry", p.Token())
	}
}

// TestKeychainTokenProviderReloadDistinguishesDeletedFromUnreadable pins the
// third conflated read site. loadFromKeychain used to return on ANY error,
// which kept a logged-out daemon serving its cached bearer token until it
// restarted. A missing item is authoritative and must clear the cache; every
// other read error may be transient and must preserve it, which is the whole
// point of BOS-659.
func TestKeychainTokenProviderReloadDistinguishesDeletedFromUnreadable(t *testing.T) {
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	cached := &keychainTokens{
		AccessToken:  "cached-access",
		RefreshToken: "cached-refresh",
		ExpiresAt:    time.Now().Add(time.Hour),
	}

	deleted := testProvider(cached)
	loadKeychainTokensFn = func() (*keychainTokens, error) { return nil, keyring.ErrKeyNotFound }
	deleted.Reload()
	if deleted.Token() != "" {
		t.Fatalf("provider token = %q after logout, want the cache dropped", deleted.Token())
	}
	if !deleted.ExpiresAt().IsZero() {
		t.Fatalf("provider expiry = %v after logout, want zero", deleted.ExpiresAt())
	}
	if reason := deleted.ReloginReason(); reason != "" {
		t.Fatalf("provider relogin reason = %q, want none — nothing was retained", reason)
	}

	unreadable := testProvider(cached)
	loadKeychainTokensFn = func() (*keychainTokens, error) { return nil, errors.New("keychain locked") }
	unreadable.Reload()
	if unreadable.Token() != "cached-access" {
		t.Fatalf("provider token = %q, want the cache preserved through a transient read failure", unreadable.Token())
	}
}
