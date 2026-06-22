package upstream

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
	origRemove := removeKeychainTokensFn
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
		removeKeychainTokensFn = origRemove
		workOSRefreshHTTPClient = origHTTPClient
		workOSAuthenticateURL = origURL
	})
}

func testProvider(tokens *keychainTokens) *KeychainTokenProvider {
	return &KeychainTokenProvider{
		accessToken:  tokens.AccessToken,
		refreshToken: tokens.RefreshToken,
		expiresAt:    tokens.ExpiresAt,
		clientIDEnv:  "BOSS_TEST_WORKOS_CLIENT_ID",
	}
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
	removed := false
	removeKeychainTokensFn = func() error {
		removed = true
		return nil
	}

	got, err := p.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh returned error: %v", err)
	}
	if got != "fresh-access" {
		t.Fatalf("token = %q, want fresh-access", got)
	}
	if removed {
		t.Fatal("stale invalid_grant removed fresh keychain tokens")
	}
}

func TestKeychainTokenProviderRefreshDeletesOnlyTerminalInvalidGrant(t *testing.T) {
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
	refreshWorkOSTokenFn = func(context.Context, string, string) (*keychainTokens, error) {
		return nil, ErrAuthExpired
	}
	removed := false
	removeKeychainTokensFn = func() error {
		removed = true
		return nil
	}

	_, err := p.Refresh(context.Background())
	if !errors.Is(err, ErrAuthExpired) {
		t.Fatalf("Refresh error = %v, want ErrAuthExpired", err)
	}
	if !removed {
		t.Fatal("terminal invalid_grant did not remove keychain tokens")
	}
	if p.Token() != "" {
		t.Fatalf("provider token = %q, want cleared", p.Token())
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
}
