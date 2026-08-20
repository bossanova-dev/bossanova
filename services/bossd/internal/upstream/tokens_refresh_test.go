package upstream

import (
	"bytes"
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
	"github.com/rs/zerolog"
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

// TestKeychainTokenProviderRefreshMarksUnknownOutcome pins what survives
// BOS-941 of BOS-659's core behavior: once the replay budget is spent, a
// refresh whose outcome could never be confirmed still preserves the identity,
// clears token material, records the ambiguous reason, and refuses to dispatch
// again even in an older binary that ignores the marker.
//
// The exchange counts here are maxAmbiguousDispatches rather than the 1 this
// test asserted before BOS-941: the first unconfirmed dispatch is now replayed
// inside WorkOS's grace window instead of flagging immediately. What the test
// actually guards — that the DURABLE marker is final, across a later Refresh
// and across a daemon restart — is unchanged, and both counts stay pinned so a
// marked record replaying even once still fails here.
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
	if got := atomic.LoadInt32(&exchanges); got != maxAmbiguousDispatches {
		t.Fatalf("WorkOS exchanges = %d, want exactly %d (the spent replay budget, and not one more from the marked record)", got, maxAmbiguousDispatches)
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
	if got := atomic.LoadInt32(&exchanges); got != maxAmbiguousDispatches {
		t.Fatalf("WorkOS exchanges after reload = %d, want exactly %d", got, maxAmbiguousDispatches)
	}
}

// TestKeychainTokenProviderRefreshGraceWindowRecovery pins BOS-941's core
// reversal. WorkOS keeps a 30s replay grace window in which re-presenting the
// SAME refresh token returns the tokens it already rotated, idempotently
// (workOSReplayGraceWindow). So an unconfirmed dispatch is a stall to ride
// out, not a sign-out: dispatch 1 never confirms, dispatch 2 replays the same
// token and succeeds, and the daemon recovers with no marker, no pause and no
// `boss login`. Before BOS-941 dispatch 1 flagged the record terminally and no
// second dispatch ever happened.
func TestKeychainTokenProviderRefreshGraceWindowRecovery(t *testing.T) {
	t.Setenv("BOSS_TEST_WORKOS_CLIENT_ID", "client")
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	past := time.Now().Add(-time.Hour)
	ring := &fakeKeychain{record: &keychainTokens{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    past,
		Email:        "dave@example.com",
	}}
	ring.install(t)

	var calls int
	refreshWorkOSTokenFn = func(_ context.Context, _, refreshTok string) (*keychainTokens, error) {
		calls++
		if refreshTok != "old-refresh" {
			t.Fatalf("dispatch %d used %q, want the SAME old-refresh replayed", calls, refreshTok)
		}
		switch calls {
		case 1:
			return nil, fmt.Errorf("%w: %w", ErrRefreshOutcomeUnknown, context.DeadlineExceeded)
		case 2:
			// The documented idempotent replay: WorkOS hands back the tokens
			// it had already rotated for the dispatch whose response we lost.
			return &keychainTokens{
				AccessToken:  "rotated-access",
				RefreshToken: "rotated-refresh",
				ExpiresAt:    time.Now().Add(time.Hour),
			}, nil
		}
		t.Fatalf("unexpected exchange attempt %d", calls)
		return nil, nil
	}

	p := testProvider(ring.snapshot())
	tok, err := p.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh error = %v, want the in-window replay to recover", err)
	}
	if tok != "rotated-access" {
		t.Fatalf("Refresh token = %q, want rotated-access", tok)
	}
	if calls != 2 {
		t.Fatalf("WorkOS exchanges = %d, want exactly 2 (the original plus one replay)", calls)
	}
	saved := ring.snapshot()
	if saved == nil {
		t.Fatal("a recovered refresh removed the keychain record")
	}
	if saved.NeedsRelogin || saved.ReloginReason != "" {
		t.Fatalf("saved marker = (%v, %q), want a clean record after recovery", saved.NeedsRelogin, saved.ReloginReason)
	}
	if saved.AccessToken != "rotated-access" || saved.RefreshToken != "rotated-refresh" {
		t.Fatalf("saved record = %+v, want the rotated tokens persisted", saved)
	}
	if saved.Email != "dave@example.com" {
		t.Fatalf("saved email = %q, want the retained identity", saved.Email)
	}
	if reason := p.ReloginReason(); reason != "" {
		t.Fatalf("provider relogin reason = %q, want none", reason)
	}
}

// TestWorkOSReplayBudgetFitsGraceWindow is the static half of BOS-941's
// design: the replay budget is a COUNT with no wall-clock check, and it is
// only safe because the worst-case elapsed time is an arithmetic property of
// three constants. Pure arithmetic, no mocks — it exists so that raising
// workOSRefreshTimeout back toward the old 30s, or raising the dispatch count,
// fails here instead of silently pushing every replay outside the window where
// WorkOS still answers idempotently.
func TestWorkOSReplayBudgetFitsGraceWindow(t *testing.T) {
	worst := time.Duration(maxAmbiguousDispatches) * workOSRefreshTimeout
	if worst >= workOSReplayGraceWindow {
		t.Fatalf("worst-case replay span = %v (%d x %v), want strictly less than the %v WorkOS grace window",
			worst, maxAmbiguousDispatches, workOSRefreshTimeout, workOSReplayGraceWindow)
	}
}

// TestKeychainTokenProviderRefreshReplayBudgetExhaustionIsTerminal pins the
// other end of the budget from the grace-window recovery above: when every
// dispatch goes unconfirmed the provider must stop at exactly
// maxAmbiguousDispatches, replaying the SAME token each time, and then fail
// closed once — one marker write, credentials cleared, identity retained.
// Without the exact count an off-by-one either wastes a dispatch outside the
// window or drops the recovery this ticket exists to add.
func TestKeychainTokenProviderRefreshReplayBudgetExhaustionIsTerminal(t *testing.T) {
	t.Setenv("BOSS_TEST_WORKOS_CLIENT_ID", "client")
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	past := time.Now().Add(-time.Hour)
	ring := &fakeKeychain{record: &keychainTokens{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    past,
		Email:        "dave@example.com",
	}}
	loadKeychainTokensFn = ring.load
	var saves int
	saveKeychainTokensFn = func(tokens *keychainTokens) error {
		saves++
		return ring.save(tokens)
	}

	var calls int
	refreshWorkOSTokenFn = func(_ context.Context, _, refreshTok string) (*keychainTokens, error) {
		calls++
		if refreshTok != "old-refresh" {
			t.Fatalf("dispatch %d used %q, want the SAME old-refresh replayed", calls, refreshTok)
		}
		return nil, fmt.Errorf("%w: dial timeout", ErrRefreshOutcomeUnknown)
	}

	p := testProvider(ring.snapshot())
	if _, err := p.Refresh(context.Background()); !errors.Is(err, ErrRefreshOutcomeUnknown) {
		t.Fatalf("Refresh error = %v, want ErrRefreshOutcomeUnknown once the budget is spent", err)
	}
	if calls != maxAmbiguousDispatches {
		t.Fatalf("WorkOS exchanges = %d, want exactly %d", calls, maxAmbiguousDispatches)
	}
	if saves != 1 {
		t.Fatalf("keychain saves = %d, want exactly 1 (one marker write, not one per replay)", saves)
	}
	saved := ring.snapshot()
	if saved == nil {
		t.Fatal("budget exhaustion deleted the keychain record")
	}
	if !saved.NeedsRelogin || saved.ReloginReason != reloginReasonRefreshOutcomeUnknown {
		t.Fatalf("saved marker = (%v, %q), want (true, %q)", saved.NeedsRelogin, saved.ReloginReason, reloginReasonRefreshOutcomeUnknown)
	}
	if saved.AccessToken != "" || saved.RefreshToken != "" {
		t.Fatalf("saved record = %+v, want cleared token material", saved)
	}
	if saved.Email != "dave@example.com" {
		t.Fatalf("saved email = %q, want the retained identity", saved.Email)
	}
}

// TestKeychainTokenProviderRefreshReplayEmitsWarnPerReplay pins the
// observability half of BOS-941. The replay budget is invisible in production
// unless each replay says so: a sign-out preceded by two replay lines is an
// exhausted budget, one with none is an authoritative rejection, and a recovery
// leaves the replay lines as the only trace that a stall was ridden out at all.
//
// It also guards the seam the warning rides on. logRefreshReplay reads the
// logger off the CONTEXT (zerolog.Ctx), so a Refresh call site that forgets
// Logger.WithContext degrades to zerolog's disabled logger and drops every
// line with no error, no panic and no failing test. That is a silent
// regression by construction, so assert the fields rather than the call.
func TestKeychainTokenProviderRefreshReplayEmitsWarnPerReplay(t *testing.T) {
	t.Setenv("BOSS_TEST_WORKOS_CLIENT_ID", "client")
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	past := time.Now().Add(-time.Hour)
	ring := &fakeKeychain{record: &keychainTokens{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    past,
		Email:        "dave@example.com",
	}}
	ring.install(t)

	var calls int
	refreshWorkOSTokenFn = func(context.Context, string, string) (*keychainTokens, error) {
		calls++
		// A reused pooled connection that then timed out — the half-open
		// signature the enumerated fields exist to make greppable.
		return nil, withRefreshFailure(refreshFailureTimeout, true,
			fmt.Errorf("%w: dial timeout", ErrRefreshOutcomeUnknown))
	}

	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	ctx := logger.WithContext(context.Background())

	p := testProvider(ring.snapshot())
	if _, err := p.Refresh(ctx); !errors.Is(err, ErrRefreshOutcomeUnknown) {
		t.Fatalf("Refresh error = %v, want ErrRefreshOutcomeUnknown", err)
	}
	if calls != maxAmbiguousDispatches {
		t.Fatalf("WorkOS exchanges = %d, want exactly %d", calls, maxAmbiguousDispatches)
	}

	var replays []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		if _, ok := entry["refresh_replay"]; ok {
			replays = append(replays, entry)
		}
	}

	// One warn per REPLAY, not per dispatch: the terminal dispatch is not a
	// replay about to happen, and logReloginPause already reports it.
	if want := maxAmbiguousDispatches - 1; len(replays) != want {
		t.Fatalf("replay warnings = %d, want exactly %d\nlog:\n%s", len(replays), want, buf.String())
	}
	for i, entry := range replays {
		if entry["level"] != "warn" {
			t.Errorf("replay %d level = %v, want warn", i+1, entry["level"])
		}
		if got, want := entry["refresh_replay"], float64(i+1); got != want {
			t.Errorf("replay index = %v, want %v (indices must run 1..N in order)", got, want)
		}
		if got := entry["refresh_failure_class"]; got != string(refreshFailureTimeout) {
			t.Errorf("replay %d refresh_failure_class = %v, want %q", i+1, got, refreshFailureTimeout)
		}
		if got := entry["conn_reused"]; got != true {
			t.Errorf("replay %d conn_reused = %v, want true", i+1, got)
		}
	}
}

// TestKeychainTokenProviderRefreshPostGraceReplayIsTerminalAndLabelled covers
// the outcome the grace window does not save: the replay lands after WorkOS
// has already rotated the token out from under it, so WorkOS answers
// authoritatively. That must be terminal at exactly 2 dispatches — the replay
// is not retried against an answer — and it must persist
// refresh_outcome_unknown rather than refresh_token_rejected, because the
// unconfirmed dispatch we made is what consumed the token. Before BOS-941 that
// labelling was reachable only through a cross-daemon race (see
// TestKeychainTokenProviderRefreshKeepsPendingUnknownReason); now one daemon
// reaches it on its own.
func TestKeychainTokenProviderRefreshPostGraceReplayIsTerminalAndLabelled(t *testing.T) {
	t.Setenv("BOSS_TEST_WORKOS_CLIENT_ID", "client")
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	past := time.Now().Add(-time.Hour)
	ring := &fakeKeychain{record: &keychainTokens{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    past,
		Email:        "dave@example.com",
	}}
	ring.install(t)

	var calls int
	refreshWorkOSTokenFn = func(_ context.Context, _, refreshTok string) (*keychainTokens, error) {
		calls++
		if refreshTok != "old-refresh" {
			t.Fatalf("dispatch %d used %q, want the SAME old-refresh replayed", calls, refreshTok)
		}
		switch calls {
		case 1:
			return nil, fmt.Errorf("%w: dial timeout", ErrRefreshOutcomeUnknown)
		case 2:
			return nil, fmt.Errorf("refresh failed (HTTP 400): %w", ErrRefreshTokenAlreadyExchanged)
		}
		t.Fatalf("unexpected exchange attempt %d", calls)
		return nil, nil
	}

	p := testProvider(ring.snapshot())
	_, err := p.Refresh(context.Background())
	if !errors.Is(err, ErrAuthExpired) {
		t.Fatalf("Refresh error = %v, want a terminal ErrAuthExpired", err)
	}
	if calls != 2 {
		t.Fatalf("WorkOS exchanges = %d, want exactly 2 — an authoritative answer ends the replay", calls)
	}
	saved := ring.snapshot()
	if saved == nil {
		t.Fatal("a post-grace replay deleted the keychain record")
	}
	if !saved.NeedsRelogin || saved.ReloginReason != reloginReasonRefreshOutcomeUnknown {
		t.Fatalf("saved marker = (%v, %q), want (true, %q) — our own unconfirmed dispatch consumed the token",
			saved.NeedsRelogin, saved.ReloginReason, reloginReasonRefreshOutcomeUnknown)
	}
	if saved.AccessToken != "" || saved.RefreshToken != "" {
		t.Fatalf("saved record = %+v, want cleared token material", saved)
	}
}

// TestKeychainTokenProviderRefreshRejectedTokenGetsNoReplayBudget is the
// negative half of the reversal, and the reason the replay lives lexically
// inside the ErrRefreshOutcomeUnknown branch. An authoritative rejection is an
// ANSWER, not a lost response: WorkOS has said this token is unusable, so
// re-presenting it buys nothing and only delays the sign-out the user has to
// act on. Exactly 1 dispatch, zero replays.
func TestKeychainTokenProviderRefreshRejectedTokenGetsNoReplayBudget(t *testing.T) {
	t.Setenv("BOSS_TEST_WORKOS_CLIENT_ID", "client")
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	past := time.Now().Add(-time.Hour)
	ring := &fakeKeychain{record: &keychainTokens{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    past,
		Email:        "dave@example.com",
	}}
	ring.install(t)

	var calls int
	refreshWorkOSTokenFn = func(context.Context, string, string) (*keychainTokens, error) {
		calls++
		return nil, fmt.Errorf("refresh failed (HTTP 400): %w", ErrRefreshTokenRejected)
	}

	p := testProvider(ring.snapshot())
	if _, err := p.Refresh(context.Background()); !errors.Is(err, ErrRefreshTokenRejected) {
		t.Fatalf("Refresh error = %v, want ErrRefreshTokenRejected", err)
	}
	if calls != 1 {
		t.Fatalf("WorkOS exchanges = %d, want exactly 1 — an authoritative rejection must not inherit the replay budget", calls)
	}
	saved := ring.snapshot()
	if saved == nil {
		t.Fatal("an authoritative rejection deleted the keychain record")
	}
	if !saved.NeedsRelogin || saved.ReloginReason != reloginReasonRefreshTokenRejected {
		t.Fatalf("saved marker = (%v, %q), want (true, %q)", saved.NeedsRelogin, saved.ReloginReason, reloginReasonRefreshTokenRejected)
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
//
// BOS-941 extended the scripted exchange from 2 attempts to 4. The supersede
// adoption and the replay budget are separate counters, so the ambiguous
// outcome on the adopted token is now replayed to exhaustion (attempts 2-4)
// before it flags — which is precisely the property that would be lost if the
// two shared one counter, and the reason this test scripts every dispatch and
// pins the total rather than assuming it.
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
		case 2, 3, 4:
			// The adopted token now gets the full replay budget: the same
			// newer-refresh re-presented until maxAmbiguousDispatches is spent.
			if refreshTok != "newer-refresh" {
				t.Fatalf("attempt %d used %q, want newer-refresh", calls, refreshTok)
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

	if want := 1 + maxAmbiguousDispatches; calls != want {
		t.Fatalf("exchange attempts = %d, want exactly %d (one supersede adoption plus the replay budget)", calls, want)
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

// TestRefreshWorkOSTokenAnnotatesFailureClass pins the diagnostics added
// alongside the BOS-659 sanitization. The pause warning deliberately drops the
// wrapped error, so without an enumerated class the logs cannot tell a stalled
// laptop network apart from a response WorkOS sent but we could not read —
// which is the difference between "raise the timeout" and "handle the body".
// The classes are a closed set defined in this package; no upstream string
// ever reaches them.
func TestRefreshWorkOSTokenAnnotatesFailureClass(t *testing.T) {
	cases := []struct {
		name      string
		handler   http.HandlerFunc
		wantClass refreshFailureClass
		ctx       func() (context.Context, context.CancelFunc)
	}{
		{
			name: "response read",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", "4096")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"access_token":`))
			},
			wantClass: refreshFailureResponseRead,
		},
		{
			name: "response decode",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`not json at all`))
			},
			wantClass: refreshFailureResponseDecode,
		},
		{
			name: "no access token",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"expires_in":300}`))
			},
			wantClass: refreshFailureNoAccessToken,
		},
		{
			name: "timeout after dispatch",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				time.Sleep(200 * time.Millisecond)
				w.WriteHeader(http.StatusOK)
			},
			wantClass: refreshFailureTimeout,
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 20*time.Millisecond)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var lockMu sync.Mutex
			withTokenRefreshHooks(t, &lockMu)

			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			workOSAuthenticateURL = srv.URL
			workOSRefreshHTTPClient = srv.Client()

			ctx := context.Background()
			if tc.ctx != nil {
				c, cancel := tc.ctx()
				defer cancel()
				ctx = c
			}
			_, err := refreshWorkOSToken(ctx, "client", "refresh")
			if err == nil {
				t.Fatal("refreshWorkOSToken returned no error")
			}
			failure, ok := refreshFailureOf(err)
			if !ok {
				t.Fatalf("error %v carries no refreshFailure annotation", err)
			}
			if failure.class != tc.wantClass {
				t.Fatalf("failure class = %q, want %q", failure.class, tc.wantClass)
			}
			// Each of these is a first exchange on a fresh connection. A true
			// value here is the half-open-pool signature and must not appear.
			if failure.connReused {
				t.Errorf("connReused = true on a freshly dialled connection")
			}
			// The annotation must not disturb the terminal classification the
			// daemon keys its re-login decision on.
			if !errors.Is(err, ErrRefreshOutcomeUnknown) {
				t.Errorf("annotated error lost ErrRefreshOutcomeUnknown: %v", err)
			}
		})
	}
}

// TestRefreshWorkOSTokenClassifiesUndispatchedAsDial proves the safe class is
// still reported as such: nothing reached WorkOS, so the refresh token was not
// consumed and the failure must stay retryable rather than terminal.
func TestRefreshWorkOSTokenClassifiesUndispatchedAsDial(t *testing.T) {
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	// Port 1 on loopback refuses immediately: no connection is ever
	// established, so httptrace's GotConn never fires.
	workOSAuthenticateURL = "http://127.0.0.1:1/"
	workOSRefreshHTTPClient = &http.Client{Timeout: 2 * time.Second}

	_, err := refreshWorkOSToken(context.Background(), "client", "refresh")
	if err == nil {
		t.Fatal("refreshWorkOSToken returned no error")
	}
	failure, ok := refreshFailureOf(err)
	if !ok {
		t.Fatalf("error %v carries no refreshFailure annotation", err)
	}
	if failure.class != refreshFailureDial {
		t.Fatalf("failure class = %q, want %q", failure.class, refreshFailureDial)
	}
	if errors.Is(err, ErrAuthExpired) {
		t.Fatalf("an undispatched exchange must stay retryable, got terminal: %v", err)
	}
}

// TestKeychainTokenProviderRefreshAdoptedTokenRejectionKeepsRejectedReason
// pins the token-identity half of the re-login reason override. `replays` is
// loop-global and deliberately survives a supersede adoption, but `refreshTok`
// does not, so a count-only override ("we replayed something, and this answer
// is already-exchanged") mislabels the ordering scripted here: token A goes
// unconfirmed and is replayed, a supersede adopts token B, and WorkOS then
// reports B as already exchanged. Nothing ever replayed B — another daemon
// consumed it — so refresh_token_rejected is the accurate reason, and
// persisting refresh_outcome_unknown would tell the next reader a transient
// stall signed them out when a credential race did.
func TestKeychainTokenProviderRefreshAdoptedTokenRejectionKeepsRejectedReason(t *testing.T) {
	t.Setenv("BOSS_TEST_WORKOS_CLIENT_ID", "client")
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	past := time.Now().Add(-time.Hour)
	ring := &fakeKeychain{record: &keychainTokens{
		AccessToken:  "a-access",
		RefreshToken: "token-a",
		ExpiresAt:    past,
		Email:        "dave@example.com",
	}}
	ring.install(t)

	var calls int
	refreshWorkOSTokenFn = func(_ context.Context, _, refreshTok string) (*keychainTokens, error) {
		calls++
		switch calls {
		case 1:
			if refreshTok != "token-a" {
				t.Fatalf("dispatch 1 used %q, want token-a", refreshTok)
			}
			// Unconfirmed: this is the dispatch that arms the replay.
			return nil, fmt.Errorf("%w: dial timeout", ErrRefreshOutcomeUnknown)
		case 2:
			if refreshTok != "token-a" {
				t.Fatalf("dispatch 2 (the replay) used %q, want token-a", refreshTok)
			}
			// Another daemon rotated the shared record while we replayed, so
			// the recovery branch adopts token-b. Its access token is already
			// expired, so the loop must exchange rather than adopt wholesale.
			if err := ring.save(&keychainTokens{
				AccessToken:  "b-access",
				RefreshToken: "token-b",
				ExpiresAt:    past,
				Email:        "dave@example.com",
			}); err != nil {
				t.Fatalf("seed newer record: %v", err)
			}
			return nil, fmt.Errorf("refresh failed (HTTP 400): %w", ErrRefreshTokenRejected)
		case 3:
			if refreshTok != "token-b" {
				t.Fatalf("dispatch 3 used %q, want the adopted token-b", refreshTok)
			}
			// The adopted token was consumed by whoever rotated it — an answer
			// about token-b, not about the unconfirmed exchange on token-a.
			return nil, fmt.Errorf("refresh failed (HTTP 400): %w", ErrRefreshTokenAlreadyExchanged)
		}
		t.Fatalf("unexpected exchange attempt %d", calls)
		return nil, nil
	}

	p := testProvider(&keychainTokens{AccessToken: "a-access", RefreshToken: "token-a", ExpiresAt: past})
	if _, err := p.Refresh(context.Background()); !errors.Is(err, ErrRefreshTokenAlreadyExchanged) {
		t.Fatalf("Refresh error = %v, want ErrRefreshTokenAlreadyExchanged", err)
	}
	if calls != 3 {
		t.Fatalf("exchange attempts = %d, want exactly 3 (one replay, one supersede adoption, one rejection)", calls)
	}

	got := ring.snapshot()
	if got == nil {
		t.Fatal("the keychain record was removed")
	}
	if !got.NeedsRelogin {
		t.Fatalf("stored marker = %v, want the record flagged", got.NeedsRelogin)
	}
	if got.ReloginReason != reloginReasonRefreshTokenRejected {
		t.Fatalf("stored reason = %q, want %q: the replay was spent on token-a, and token-b was never re-presented by this loop",
			got.ReloginReason, reloginReasonRefreshTokenRejected)
	}
}

// TestKeychainTokenProviderRefreshDeadContextDoesNotSpendReplayBudget pins the
// guard that keeps the caller's deadline from manufacturing a sign-out. The
// replay budget is sized in wall-clock terms — maxAmbiguousDispatches whole
// workOSRefreshTimeouts inside WorkOS's grace window — but every requestCtx is
// derived from the caller's ctx, so a cancelled or nearly-expired parent
// collapses the remaining dispatches to microseconds. They still classify as
// AMBIGUOUS rather than transient, because net/http reports GotConn from the
// idle pool before roundTrip observes the dead context, so without this guard a
// shutdown or the startup refresh's 10s cap would burn the whole budget
// instantly and write the durable marker that only an exhausted WorkOS
// conversation should ever justify.
func TestKeychainTokenProviderRefreshDeadContextDoesNotSpendReplayBudget(t *testing.T) {
	t.Setenv("BOSS_TEST_WORKOS_CLIENT_ID", "client")
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	past := time.Now().Add(-time.Hour)
	ring := &fakeKeychain{record: &keychainTokens{
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ExpiresAt:    past,
		Email:        "dave@example.com",
	}}
	ring.install(t)

	var calls int32
	refreshWorkOSTokenFn = func(context.Context, string, string) (*keychainTokens, error) {
		atomic.AddInt32(&calls, 1)
		return nil, withRefreshFailure(refreshFailureTimeout, true,
			fmt.Errorf("%w: context deadline exceeded", ErrRefreshOutcomeUnknown))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p := testProvider(ring.snapshot())
	_, err := p.Refresh(ctx)
	if err == nil {
		t.Fatal("Refresh error = nil, want a non-terminal error")
	}
	if errors.Is(err, ErrAuthExpired) {
		t.Fatalf("Refresh error = %v, want an error that does NOT compose with ErrAuthExpired: a dead caller context must leave the tick-level retry free to try again", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Refresh error = %v, want it to wrap context.Canceled", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("WorkOS exchanges = %d, want exactly 1: a dead context must not spend replays", got)
	}

	got := ring.snapshot()
	if got == nil {
		t.Fatal("the keychain record was removed")
	}
	if got.NeedsRelogin || got.ReloginReason != "" {
		t.Fatalf("stored marker = (%v, %q), want an unflagged record", got.NeedsRelogin, got.ReloginReason)
	}
	if got.RefreshToken != "old-refresh" || got.AccessToken != "old-access" {
		t.Fatalf("stored tokens = (%q, %q), want the credentials untouched", got.AccessToken, got.RefreshToken)
	}
}

// TestLogRefreshReplayFallsBackWhenContextCarriesNoLogger pins the structural
// guarantee that replaces a test no call site can supply. The replay warning is
// read off the context (zerolog.Ctx), and zerolog answers a context with no
// logger with a DISABLED one — so a Refresh call site that forgets
// Logger.WithContext drops every replay line with no error, no panic and no
// failing test. That already happened once on this branch, and it will keep
// being possible: a call site added tomorrow cannot be covered by a test
// written today. So the seam is made non-silent instead of merely watched — a
// missing context logger costs the caller's component field, never the line.
func TestLogRefreshReplayFallsBackWhenContextCarriesNoLogger(t *testing.T) {
	var fallback bytes.Buffer
	orig := replayWarnFallback
	replayWarnFallback = &fallback
	t.Cleanup(func() { replayWarnFallback = orig })

	err := withRefreshFailure(refreshFailureTimeout, true,
		fmt.Errorf("%w: dial timeout", ErrRefreshOutcomeUnknown))

	// A context with no logger attached: the exact shape of a forgotten
	// WithContext at a call site.
	logRefreshReplay(context.Background(), 2, err)

	entry := map[string]any{}
	line := strings.TrimSpace(fallback.String())
	if line == "" {
		t.Fatal("no replay warning reached the fallback writer: the seam still fails silently")
	}
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		t.Fatalf("fallback line %q is not JSON: %v", line, err)
	}
	if entry["level"] != "warn" {
		t.Errorf("level = %v, want warn", entry["level"])
	}
	if got, want := entry["refresh_replay"], float64(2); got != want {
		t.Errorf("refresh_replay = %v, want %v", got, want)
	}
	if got := entry["refresh_failure_class"]; got != string(refreshFailureTimeout) {
		t.Errorf("refresh_failure_class = %v, want %q", got, refreshFailureTimeout)
	}
	if entry["conn_reused"] != true {
		t.Errorf("conn_reused = %v, want true", entry["conn_reused"])
	}

	// The fallback must not shadow a caller that DID attach a logger: that
	// logger carries the component field the sign-out line is read against.
	fallback.Reset()
	var attached bytes.Buffer
	logger := zerolog.New(&attached)
	logRefreshReplay(logger.WithContext(context.Background()), 1, err)
	if attached.Len() == 0 {
		t.Error("the context logger received nothing")
	}
	if fallback.Len() != 0 {
		t.Errorf("the fallback also fired: %s", fallback.String())
	}
}

// TestKeychainTokenProviderReloadResultReportsTheRead pins the distinction the
// post-login instrumentation depends on: the cache after a reload does not say
// whether the reload actually read anything, so ReloadResult reports the read
// itself, with an enumerated class that never carries the underlying error.
func TestKeychainTokenProviderReloadResultReportsTheRead(t *testing.T) {
	var lockMu sync.Mutex
	withTokenRefreshHooks(t, &lockMu)

	cached := &keychainTokens{
		AccessToken:  "cached-access",
		RefreshToken: "cached-refresh",
		ExpiresAt:    time.Now().Add(time.Hour),
	}

	ok := testProvider(cached)
	loadKeychainTokensFn = func() (*keychainTokens, error) { return cached, nil }
	if got := ok.ReloadResult(); !got.ReadOK || got.ErrorClass != "" {
		t.Fatalf("successful reload = %+v, want ReadOK with no error class", got)
	}

	unreadable := testProvider(cached)
	loadKeychainTokensFn = func() (*keychainTokens, error) { return nil, errors.New("keychain locked") }
	got := unreadable.ReloadResult()
	if got.ReadOK || got.ErrorClass != ReloadErrorReadFailed {
		t.Fatalf("unreadable reload = %+v, want a read_failed classification", got)
	}
	// The cache is deliberately preserved — which is exactly why the caller
	// cannot infer the read outcome from it.
	if unreadable.Token() != "cached-access" {
		t.Fatalf("provider token = %q, want the cache preserved across a failed read", unreadable.Token())
	}

	deleted := testProvider(cached)
	loadKeychainTokensFn = func() (*keychainTokens, error) { return nil, keyring.ErrKeyNotFound }
	if got := deleted.ReloadResult(); got.ReadOK || got.ErrorClass != ReloadErrorRecordDeleted {
		t.Fatalf("deleted reload = %+v, want a record_deleted classification", got)
	}
}

// KeyringBackend names configuration, never credentials, and reports the
// platform default when nothing is pinned.
func TestKeyringBackendNamesTheResolvedBackend(t *testing.T) {
	t.Setenv("BOSS_KEYRING_BACKEND", "")
	if got := KeyringBackend(); got != "platform-default" {
		t.Fatalf("KeyringBackend() = %q with no override, want platform-default", got)
	}
	t.Setenv("BOSS_KEYRING_BACKEND", "file")
	if got := KeyringBackend(); got != "file" {
		t.Fatalf("KeyringBackend() = %q with the file override, want file", got)
	}
}
