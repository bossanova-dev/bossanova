package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestTokensJSONSharedShape pins the on-disk contract with bossd: both
// binaries read and write the same "workos-tokens" item, so an older record
// with no BOS-659 marker must load and behave exactly as before, and a clean
// record must not gain marker noise on the way back out. The mirror of this
// test lives in services/bossd/internal/upstream.
func TestTokensJSONSharedShape(t *testing.T) {
	const legacy = `{"access_token":"a","refresh_token":"r","expires_at":"2099-01-01T00:00:00Z","email":"dave@example.com"}`
	var old Tokens
	if err := json.Unmarshal([]byte(legacy), &old); err != nil {
		t.Fatalf("unmarshal legacy record: %v", err)
	}
	if old.NeedsRelogin || old.ReloginReason != "" {
		t.Fatalf("legacy record gained a marker: %+v", old)
	}
	if !old.Valid() {
		t.Fatal("legacy record should still be Valid()")
	}
	out, err := json.Marshal(&old)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "needs_relogin") || strings.Contains(string(out), "relogin_reason") {
		t.Fatalf("clean record serialized marker fields: %s", out)
	}

	// A record written by the daemon must round-trip through the CLI shape.
	const daemonWritten = `{"access_token":"a","refresh_token":"r","expires_at":"2099-01-01T00:00:00Z","email":"dave@example.com","needs_relogin":true,"relogin_reason":"refresh_outcome_unknown"}`
	var marked Tokens
	if err := json.Unmarshal([]byte(daemonWritten), &marked); err != nil {
		t.Fatalf("unmarshal daemon record: %v", err)
	}
	if !marked.NeedsRelogin || marked.ReloginReason != ReloginReasonRefreshOutcomeUnknown {
		t.Fatalf("marker did not survive: %+v", marked)
	}
	if marked.Email != "dave@example.com" {
		t.Fatalf("identity lost: %+v", marked)
	}
	if marked.Valid() {
		t.Fatal("a record flagged for re-login must not report Valid()")
	}
	back, err := json.Marshal(&marked)
	if err != nil {
		t.Fatalf("marshal marked: %v", err)
	}
	if !strings.Contains(string(back), `"needs_relogin":true`) ||
		!strings.Contains(string(back), `"relogin_reason":"refresh_outcome_unknown"`) {
		t.Fatalf("marker did not round-trip: %s", back)
	}
}

func TestManagerStatus_ReloginRequired(t *testing.T) {
	tests := []struct {
		name       string
		reason     string
		wantReason string
	}{
		{name: "unknown outcome", reason: ReloginReasonRefreshOutcomeUnknown, wantReason: ReloginReasonRefreshOutcomeUnknown},
		{name: "rejected", reason: ReloginReasonRefreshTokenRejected, wantReason: ReloginReasonRefreshTokenRejected},
		{name: "unrecognized reason is treated as rejected", reason: "something-new", wantReason: ReloginReasonRefreshTokenRejected},
		{name: "missing reason is treated as rejected", reason: "", wantReason: ReloginReasonRefreshTokenRejected},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &mockTokenStore{tokens: &Tokens{
				AccessToken:   "expired",
				RefreshToken:  "r",
				Email:         "dave@example.com",
				ExpiresAt:     time.Now().Add(time.Hour),
				NeedsRelogin:  true,
				ReloginReason: tc.reason,
			}}
			mgr := NewManager(store, Config{})

			status := mgr.Status()
			if status.LoggedIn {
				t.Error("a record flagged for re-login must not report LoggedIn")
			}
			if !status.NeedsRelogin {
				t.Error("expected NeedsRelogin = true")
			}
			if status.ReloginReason != tc.wantReason {
				t.Errorf("ReloginReason = %q, want %q", status.ReloginReason, tc.wantReason)
			}
			// The retained identity is the whole point of not deleting the entry.
			if status.Email != "dave@example.com" {
				t.Errorf("Email = %q, want the retained identity", status.Email)
			}
		})
	}
}

func TestManagerStatus_CleanRecordHasNoReason(t *testing.T) {
	store := &mockTokenStore{tokens: &Tokens{
		AccessToken: "token",
		Email:       "dave@example.com",
		ExpiresAt:   time.Now().Add(time.Hour),
	}}
	status := NewManager(store, Config{}).Status()
	if !status.LoggedIn || status.NeedsRelogin || status.ReloginReason != "" {
		t.Fatalf("clean record status = %+v, want logged in with no re-login reason", status)
	}
}

func TestManagerStatus_UnreadableCredentialsUnchanged(t *testing.T) {
	store := &mockTokenStore{loadErr: ErrCredentialsUnreadable}
	status := NewManager(store, Config{}).Status()
	if status.LoggedIn {
		t.Error("expected LoggedIn = false when credentials are unreadable")
	}
	if status.NeedsRelogin || status.ReloginReason != "" {
		t.Errorf("unreadable credentials must not look like a retained re-login marker: %+v", status)
	}
}

func TestManagerAccessToken_RefusesMarkedRecord(t *testing.T) {
	store := &mockTokenStore{tokens: &Tokens{
		AccessToken:   "still-unexpired",
		RefreshToken:  "r",
		Email:         "dave@example.com",
		ExpiresAt:     time.Now().Add(time.Hour),
		NeedsRelogin:  true,
		ReloginReason: ReloginReasonRefreshOutcomeUnknown,
	}}
	mgr := NewManager(store, Config{})

	var exchanges int
	origRefresh := refreshAccessToken
	refreshAccessToken = func(context.Context, Config, string) (*Tokens, error) {
		exchanges++
		return nil, errors.New("must not be called")
	}
	t.Cleanup(func() { refreshAccessToken = origRefresh })

	tok, err := mgr.AccessToken(context.Background())
	if !errors.Is(err, ErrReloginRequired) {
		t.Fatalf("AccessToken error = %v, want ErrReloginRequired", err)
	}
	if tok != "" {
		t.Fatalf("AccessToken = %q, want empty for a flagged record", tok)
	}
	if exchanges != 0 {
		t.Fatalf("WorkOS exchanges = %d, want 0 from a flagged record", exchanges)
	}
	if !strings.Contains(err.Error(), "boss login") {
		t.Fatalf("error should point at the remedy, got %v", err)
	}
	if strings.Contains(err.Error(), "still-unexpired") || strings.Contains(err.Error(), "\"r\"") {
		t.Fatalf("error leaked token material: %v", err)
	}
}

func TestManagerRefresh_RefusesMarkedRecord(t *testing.T) {
	store := &mockTokenStore{tokens: &Tokens{
		AccessToken:   "expired",
		RefreshToken:  "r",
		ExpiresAt:     time.Now().Add(-time.Hour),
		NeedsRelogin:  true,
		ReloginReason: ReloginReasonRefreshTokenRejected,
	}}
	mgr := NewManager(store, Config{})

	var exchanges int
	var lockMu sync.Mutex
	// Stub the cross-process lock: without this the test takes the real
	// machine-global WorkOS flock and contends with every other worktree.
	withRefreshHooks(t, &lockMu, func(context.Context, Config, string) (*Tokens, error) {
		exchanges++
		return nil, errors.New("must not be called")
	})

	if err := mgr.Refresh(context.Background()); !errors.Is(err, ErrReloginRequired) {
		t.Fatalf("Refresh error = %v, want ErrReloginRequired", err)
	}
	if exchanges != 0 {
		t.Fatalf("WorkOS exchanges = %d, want 0 from a flagged record", exchanges)
	}
}

// TestManagerLogin_ClearsMarker proves the documented recovery path: logging
// in again replaces the flagged record with a clean one, so the retained
// marker never outlives the session it described.
func TestManagerLogin_ClearsMarker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new","refresh_token":"new-refresh","expires_in":3600,"user":{"email":"dave@example.com"}}`))
	}))
	defer srv.Close()
	origBase := workosAPIBase
	origClient := workosHTTPClient
	workosAPIBase = srv.URL
	workosHTTPClient = srv.Client()
	t.Cleanup(func() {
		workosAPIBase = origBase
		workosHTTPClient = origClient
	})

	store := &mockTokenStore{tokens: &Tokens{
		AccessToken:   "old",
		RefreshToken:  "old-refresh",
		Email:         "dave@example.com",
		ExpiresAt:     time.Now().Add(-time.Hour),
		NeedsRelogin:  true,
		ReloginReason: ReloginReasonRefreshOutcomeUnknown,
	}}
	mgr := NewManager(store, Config{ClientID: "test-client"})

	if err := mgr.PollLogin(context.Background(), "device", 0); err != nil {
		t.Fatalf("PollLogin returned error: %v", err)
	}
	if store.tokens.NeedsRelogin || store.tokens.ReloginReason != "" {
		t.Fatalf("login saved a flagged record: %+v", store.tokens)
	}
	if status := mgr.Status(); !status.LoggedIn || status.NeedsRelogin || status.ReloginReason != "" {
		t.Fatalf("status after login = %+v, want a clean logged-in state", status)
	}
}

func TestManagerRefresh_ClearsStaleMarkerOnSuccess(t *testing.T) {
	// The stored record is clean (the daemon has not flagged it), but the
	// refreshed result must still be written clean so a marker can never leak
	// back onto a working session.
	store := &mockTokenStore{tokens: &Tokens{
		AccessToken:  "expired",
		RefreshToken: "r",
		Email:        "dave@example.com",
		ExpiresAt:    time.Now().Add(-time.Hour),
	}}
	mgr := NewManager(store, Config{})

	origRefresh := refreshAccessToken
	refreshAccessToken = func(context.Context, Config, string) (*Tokens, error) {
		return &Tokens{
			AccessToken:   "fresh",
			RefreshToken:  "fresh-refresh",
			ExpiresAt:     time.Now().Add(time.Hour),
			NeedsRelogin:  true,
			ReloginReason: ReloginReasonRefreshTokenRejected,
		}, nil
	}
	t.Cleanup(func() { refreshAccessToken = origRefresh })

	if err := mgr.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh returned error: %v", err)
	}
	if store.tokens.NeedsRelogin || store.tokens.ReloginReason != "" {
		t.Fatalf("successful refresh saved a flagged record: %+v", store.tokens)
	}
}

// reloginRaceStore hands back a clean record on the first Load and a flagged
// one afterwards, modelling the BOS-659 race from the CLI's side: bossd flags
// the shared keychain item while this process is queued on the cross-process
// refresh lock. Only the under-lock re-check can catch it — the pre-lock guard
// has already seen the clean record.
type reloginRaceStore struct {
	mu    sync.Mutex
	loads int
	saved *Tokens
}

func (s *reloginRaceStore) Load() (*Tokens, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	if s.loads == 1 {
		return &Tokens{AccessToken: "expired", RefreshToken: "r", ExpiresAt: time.Now().Add(-time.Hour)}, nil
	}
	return &Tokens{
		AccessToken:   "expired",
		RefreshToken:  "r",
		ExpiresAt:     time.Now().Add(-time.Hour),
		NeedsRelogin:  true,
		ReloginReason: ReloginReasonRefreshOutcomeUnknown,
	}, nil
}

func (s *reloginRaceStore) Save(tokens *Tokens) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved = tokens
	return nil
}

func (s *reloginRaceStore) Delete() error { return nil }

func TestManagerRefresh_RefusesRecordFlaggedWhileWaitingForLock(t *testing.T) {
	store := &reloginRaceStore{}
	mgr := NewManager(store, Config{})

	var exchanges int
	var lockMu sync.Mutex
	withRefreshHooks(t, &lockMu, func(context.Context, Config, string) (*Tokens, error) {
		exchanges++
		return nil, errors.New("must not be called")
	})

	err := mgr.Refresh(context.Background())
	if !errors.Is(err, ErrReloginRequired) {
		t.Fatalf("Refresh error = %v, want ErrReloginRequired", err)
	}
	if exchanges != 0 {
		t.Fatalf("WorkOS exchanges = %d, want 0 — the record was flagged before we got the lock", exchanges)
	}
	if store.loads < 2 {
		t.Fatalf("store loads = %d, want the under-lock re-read to have happened", store.loads)
	}
	if store.saved != nil {
		t.Fatalf("a refusal saved %+v; it must not write", store.saved)
	}
}
