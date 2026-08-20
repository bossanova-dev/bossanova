package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// --- Mock token store ---

type mockTokenStore struct {
	tokens  *Tokens
	saveErr error
	loadErr error
	delErr  error

	saveCalled   bool
	deleteCalled bool
	// loadCalled counts read-backs so a test can prove the post-save
	// verification never ran (a failed save has nothing to verify).
	loadCalled int
	// loadGate, when non-nil, blocks every Load until it is closed. It exists
	// so a test can hold the read-back past its own bound.
	loadGate chan struct{}
}

type errorRefreshLock struct{ err error }

func (l errorRefreshLock) Unlock() error { return l.err }

func (m *mockTokenStore) Save(tokens *Tokens) error {
	m.saveCalled = true
	if m.saveErr != nil {
		return m.saveErr
	}
	m.tokens = tokens
	return nil
}

func (m *mockTokenStore) Load() (*Tokens, error) {
	m.loadCalled++
	if m.loadGate != nil {
		<-m.loadGate
	}
	if m.loadErr != nil {
		return nil, m.loadErr
	}
	if m.tokens == nil {
		// Mirror KeychainStore: "nothing stored" is a missing-key error, which
		// callers discriminate from an unreadable record with tokenKeyMissing.
		return nil, fmt.Errorf("no tokens stored: %w", fs.ErrNotExist)
	}
	return m.tokens, nil
}

func (m *mockTokenStore) Delete() error {
	m.deleteCalled = true
	if m.delErr != nil {
		return m.delErr
	}
	m.tokens = nil
	return nil
}

// --- Token validity ---

func TestTokens_Valid(t *testing.T) {
	tests := []struct {
		name   string
		tokens Tokens
		want   bool
	}{
		{
			name:   "valid token",
			tokens: Tokens{AccessToken: "abc", ExpiresAt: time.Now().Add(time.Hour)},
			want:   true,
		},
		{
			name:   "expired token",
			tokens: Tokens{AccessToken: "abc", ExpiresAt: time.Now().Add(-time.Hour)},
			want:   false,
		},
		{
			name:   "empty access token",
			tokens: Tokens{AccessToken: "", ExpiresAt: time.Now().Add(time.Hour)},
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.tokens.Valid(); got != tt.want {
				t.Errorf("Valid() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- Manager.AccessToken ---

func TestManager_AccessToken_ValidToken(t *testing.T) {
	store := &mockTokenStore{
		tokens: &Tokens{
			AccessToken: "valid-token",
			ExpiresAt:   time.Now().Add(time.Hour),
		},
	}
	mgr := NewManager(store, Config{})

	token, err := mgr.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "valid-token" {
		t.Errorf("got %q, want %q", token, "valid-token")
	}
}

func TestManager_AccessToken_NoTokens(t *testing.T) {
	store := &mockTokenStore{loadErr: fmt.Errorf("no tokens")}
	mgr := NewManager(store, Config{})

	token, err := mgr.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "" {
		t.Errorf("got %q, want empty string (unauthenticated)", token)
	}
}

func TestManager_AccessToken_ExpiredNoRefresh(t *testing.T) {
	store := &mockTokenStore{
		tokens: &Tokens{
			AccessToken: "expired-token",
			ExpiresAt:   time.Now().Add(-time.Hour),
		},
	}
	mgr := NewManager(store, Config{})

	_, err := mgr.AccessToken(context.Background())
	if err == nil {
		t.Fatal("expected error for expired token without refresh token")
	}
}

func TestManager_AccessToken_ExpiredWithRefresh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("grant_type") != "refresh_token" {
			http.Error(w, "bad grant_type", 400)
			return
		}
		if r.FormValue("refresh_token") != "my-refresh" {
			http.Error(w, "bad refresh token", 400)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-access-token",
			"refresh_token": "new-refresh-token",
			"expires_in":    3600,
			"user":          map[string]string{"id": "user_01", "email": "test@example.com"},
		})
	}))
	defer srv.Close()

	origBase := workosAPIBase
	defer func() { workosAPIBase = origBase }()
	workosAPIBase = srv.URL

	store := &mockTokenStore{
		tokens: &Tokens{
			AccessToken:  "expired-token",
			RefreshToken: "my-refresh",
			ExpiresAt:    time.Now().Add(-time.Hour),
		},
	}

	cfg := Config{ClientID: "test-client"}
	mgr := NewManager(store, cfg)

	token, err := mgr.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "new-access-token" {
		t.Errorf("got %q, want %q", token, "new-access-token")
	}
	if !store.saveCalled {
		t.Error("expected Save to be called with refreshed tokens")
	}
	if store.tokens.RefreshToken != "new-refresh-token" {
		t.Errorf("refresh token not updated: got %q", store.tokens.RefreshToken)
	}
}

func TestManager_Refresh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("grant_type") != "refresh_token" {
			http.Error(w, "bad grant_type", 400)
			return
		}
		if r.FormValue("refresh_token") != "my-refresh" {
			http.Error(w, "bad refresh token", 400)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fresh-access-token",
			"refresh_token": "fresh-refresh-token",
			"expires_in":    3600,
			"user":          map[string]string{"id": "user_01", "email": "refresh@example.com"},
		})
	}))
	defer srv.Close()

	origBase := workosAPIBase
	defer func() { workosAPIBase = origBase }()
	workosAPIBase = srv.URL

	store := &mockTokenStore{
		tokens: &Tokens{
			AccessToken:  "old-access-token",
			RefreshToken: "my-refresh",
			ExpiresAt:    time.Now().Add(time.Hour),
		},
	}
	mgr := NewManager(store, Config{ClientID: "test-client"})

	if err := mgr.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh returned error: %v", err)
	}
	if !store.saveCalled {
		t.Fatal("expected Save to be called with refreshed tokens")
	}
	if store.tokens.AccessToken != "fresh-access-token" {
		t.Fatalf("access token = %q, want %q", store.tokens.AccessToken, "fresh-access-token")
	}
	if store.tokens.RefreshToken != "fresh-refresh-token" {
		t.Fatalf("refresh token = %q, want %q", store.tokens.RefreshToken, "fresh-refresh-token")
	}
}

func TestManager_Refresh_NoRefreshToken(t *testing.T) {
	store := &mockTokenStore{
		tokens: &Tokens{
			AccessToken: "access-token",
			ExpiresAt:   time.Now().Add(time.Hour),
		},
	}
	mgr := NewManager(store, Config{})

	err := mgr.Refresh(context.Background())
	if err == nil {
		t.Fatal("expected error for missing refresh token")
	}
	if !strings.Contains(err.Error(), "no refresh token available; run 'boss login'") {
		t.Fatalf("error = %q", err.Error())
	}
}

// --- Manager.Logout ---

func TestManager_Logout(t *testing.T) {
	store := &mockTokenStore{
		tokens: &Tokens{AccessToken: "token", ExpiresAt: time.Now().Add(time.Hour)},
	}
	mgr := NewManager(store, Config{})

	var lockMu sync.Mutex
	withRefreshHooks(t, &lockMu, func(context.Context, Config, string) (*Tokens, error) {
		return nil, errors.New("refresh should not be called")
	})
	if err := mgr.Logout(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !store.deleteCalled {
		t.Error("expected Delete to be called")
	}
	if store.tokens != nil {
		t.Error("tokens should be nil after logout")
	}
}

func TestManager_LogoutWaitsForCredentialLock(t *testing.T) {
	store := &mockTokenStore{
		tokens: &Tokens{AccessToken: "token", ExpiresAt: time.Now().Add(time.Hour)},
	}
	mgr := NewManager(store, Config{})

	origAcquire := acquireRefreshLock
	defer func() { acquireRefreshLock = origAcquire }()
	var lockMu sync.Mutex
	lockMu.Lock() // Simulate a daemon refresh holding the shared file lock.
	acquireRefreshLock = func(context.Context) (refreshUnlocker, error) {
		lockMu.Lock()
		return &mutexRefreshLock{mu: &lockMu}, nil
	}

	done := make(chan error, 1)
	go func() { done <- mgr.Logout(context.Background()) }()
	select {
	case err := <-done:
		t.Fatalf("Logout completed while credential lock was held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if store.deleteCalled {
		t.Fatal("Delete called while credential lock was held")
	}

	lockMu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Logout returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Logout did not complete after credential lock was released")
	}
	if !store.deleteCalled {
		t.Fatal("Delete was not called after credential lock was released")
	}
}

func TestManager_LogoutSucceedsWhenUnlockFailsAfterDelete(t *testing.T) {
	store := &mockTokenStore{
		tokens: &Tokens{AccessToken: "token", ExpiresAt: time.Now().Add(time.Hour)},
	}
	mgr := NewManager(store, Config{})

	origAcquire := acquireRefreshLock
	t.Cleanup(func() { acquireRefreshLock = origAcquire })
	acquireRefreshLock = func(context.Context) (refreshUnlocker, error) {
		return errorRefreshLock{err: errors.New("unlock failed")}, nil
	}

	if err := mgr.Logout(context.Background()); err != nil {
		t.Fatalf("Logout returned error after successful Delete: %v", err)
	}
	if !store.deleteCalled {
		t.Fatal("expected Delete to be called")
	}
	if store.tokens != nil {
		t.Fatal("tokens should be nil after logout")
	}
}

// --- Manager.Status ---

func TestManager_Status_LoggedIn(t *testing.T) {
	store := &mockTokenStore{
		tokens: &Tokens{
			AccessToken: "token",
			Email:       "dave@example.com",
			ExpiresAt:   time.Now().Add(time.Hour),
		},
	}
	mgr := NewManager(store, Config{})

	status := mgr.Status()
	if !status.LoggedIn {
		t.Error("expected LoggedIn = true")
	}
	if status.Email != "dave@example.com" {
		t.Errorf("got email %q, want %q", status.Email, "dave@example.com")
	}
}

func TestManager_Status_ExpiredButRefreshable(t *testing.T) {
	store := &mockTokenStore{
		tokens: &Tokens{
			AccessToken:  "expired-token",
			RefreshToken: "my-refresh",
			Email:        "dave@example.com",
			ExpiresAt:    time.Now().Add(-time.Hour),
		},
	}
	mgr := NewManager(store, Config{})

	status := mgr.Status()
	if !status.LoggedIn {
		t.Error("expected LoggedIn = true when refresh token is available")
	}
	if status.Email != "dave@example.com" {
		t.Errorf("got email %q, want %q", status.Email, "dave@example.com")
	}
}

func TestManager_Status_NotLoggedIn(t *testing.T) {
	store := &mockTokenStore{loadErr: fmt.Errorf("no tokens")}
	mgr := NewManager(store, Config{})

	status := mgr.Status()
	if status.LoggedIn {
		t.Error("expected LoggedIn = false")
	}
}

// --- Device code flow ---

func TestRequestDeviceCode_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user_management/authorize/device" {
			http.Error(w, "not found", 404)
			return
		}
		if r.FormValue("client_id") != "test-client" {
			t.Errorf("expected client_id=test-client, got %q", r.FormValue("client_id"))
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code":               "dev-code-123",
			"user_code":                 "ABCD-1234",
			"verification_uri":          "https://auth.example.com/activate",
			"verification_uri_complete": "https://auth.example.com/activate?code=ABCD-1234",
			"expires_in":                300,
			"interval":                  5,
		})
	}))
	defer srv.Close()

	origBase := workosAPIBase
	defer func() { workosAPIBase = origBase }()
	workosAPIBase = srv.URL

	cfg := Config{ClientID: "test-client"}
	resp, err := RequestDeviceCode(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.DeviceCode != "dev-code-123" {
		t.Errorf("device_code = %q, want %q", resp.DeviceCode, "dev-code-123")
	}
	if resp.UserCode != "ABCD-1234" {
		t.Errorf("user_code = %q, want %q", resp.UserCode, "ABCD-1234")
	}
	if resp.ExpiresIn != 300 {
		t.Errorf("expires_in = %d, want 300", resp.ExpiresIn)
	}
	if resp.Interval != 5 {
		t.Errorf("interval = %d, want 5", resp.Interval)
	}
}

func TestRequestDeviceCode_NetworkError(t *testing.T) {
	origBase := workosAPIBase
	defer func() { workosAPIBase = origBase }()
	workosAPIBase = "http://127.0.0.1:1"

	cfg := Config{ClientID: "test-client"}
	_, err := RequestDeviceCode(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
}

func TestRequestDeviceCode_BadResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte("internal server error"))
	}))
	defer srv.Close()

	origBase := workosAPIBase
	defer func() { workosAPIBase = origBase }()
	workosAPIBase = srv.URL

	cfg := Config{ClientID: "test-client"}
	_, err := RequestDeviceCode(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error for bad response")
	}
}

func TestPollForToken_PendingThenSuccess(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		if callCount < 3 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":             "authorization_pending",
				"error_description": "user hasn't completed login yet",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "my-access-token",
			"refresh_token": "my-refresh-token",
			"expires_in":    3600,
			"user":          map[string]string{"id": "user_01H", "email": "test@example.com"},
		})
	}))
	defer srv.Close()

	origBase := workosAPIBase
	defer func() { workosAPIBase = origBase }()
	workosAPIBase = srv.URL

	cfg := Config{ClientID: "test-client"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := PollForToken(ctx, cfg, "dev-code-123", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Tokens.AccessToken != "my-access-token" {
		t.Errorf("access_token = %q, want %q", result.Tokens.AccessToken, "my-access-token")
	}
	if result.Email != "test@example.com" {
		t.Errorf("email = %q, want %q", result.Email, "test@example.com")
	}
	if callCount < 3 {
		t.Errorf("expected at least 3 poll calls, got %d", callCount)
	}
}

func TestPollForToken_SlowDown(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":             "slow_down",
				"error_description": "slow down",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "token",
			"refresh_token": "refresh",
			"expires_in":    3600,
			"user":          map[string]string{"id": "user_01", "email": "test@example.com"},
		})
	}))
	defer srv.Close()

	origBase := workosAPIBase
	defer func() { workosAPIBase = origBase }()
	workosAPIBase = srv.URL

	cfg := Config{ClientID: "test-client"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := PollForToken(ctx, cfg, "dev-code", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Tokens.AccessToken != "token" {
		t.Errorf("access_token = %q, want %q", result.Tokens.AccessToken, "token")
	}
}

func TestPollForToken_AccessDenied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":             "access_denied",
			"error_description": "user denied access",
		})
	}))
	defer srv.Close()

	origBase := workosAPIBase
	defer func() { workosAPIBase = origBase }()
	workosAPIBase = srv.URL

	cfg := Config{ClientID: "test-client"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := PollForToken(ctx, cfg, "dev-code", 0)
	if err == nil {
		t.Fatal("expected error for access_denied")
	}
}

func TestPollForToken_ExpiredToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":             "expired_token",
			"error_description": "device code expired",
		})
	}))
	defer srv.Close()

	origBase := workosAPIBase
	defer func() { workosAPIBase = origBase }()
	workosAPIBase = srv.URL

	cfg := Config{ClientID: "test-client"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := PollForToken(ctx, cfg, "dev-code", 0)
	if err == nil {
		t.Fatal("expected error for expired_token")
	}
}

func TestPollForToken_ContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":             "authorization_pending",
			"error_description": "waiting",
		})
	}))
	defer srv.Close()

	origBase := workosAPIBase
	defer func() { workosAPIBase = origBase }()
	workosAPIBase = srv.URL

	cfg := Config{ClientID: "test-client"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := PollForToken(ctx, cfg, "dev-code", 0)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestLogin_DeviceCodeFlow(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user_management/authorize/device":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code":               "dev-code-login",
				"user_code":                 "LOGIN-CODE",
				"verification_uri":          "https://auth.example.com/activate",
				"verification_uri_complete": "https://auth.example.com/activate?code=LOGIN-CODE",
				"expires_in":                300,
				"interval":                  0,
			})
		case "/user_management/authenticate":
			callCount++
			if callCount < 2 {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error":             "authorization_pending",
					"error_description": "waiting",
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "login-access-token",
				"refresh_token": "login-refresh-token",
				"expires_in":    3600,
				"user":          map[string]string{"id": "user_01", "email": "login@example.com"},
			})
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	origBase := workosAPIBase
	defer func() { workosAPIBase = origBase }()
	workosAPIBase = srv.URL

	origOpen := openBrowserFn
	openBrowserFn = func(url string) error { return nil }
	defer func() { openBrowserFn = origOpen }()

	cfg := Config{ClientID: "test-client"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := Login(ctx, cfg)
	if err != nil {
		t.Fatalf("Login failed: %v", err)
	}
	if result.Tokens.AccessToken != "login-access-token" {
		t.Errorf("access_token = %q, want %q", result.Tokens.AccessToken, "login-access-token")
	}
	if result.Email != "login@example.com" {
		t.Errorf("email = %q, want %q", result.Email, "login@example.com")
	}
}

func TestRefreshAccessToken_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("grant_type") != "refresh_token" {
			t.Errorf("expected grant_type=refresh_token, got %q", r.FormValue("grant_type"))
		}
		if r.FormValue("client_id") != "my-client" {
			t.Errorf("expected client_id=my-client, got %q", r.FormValue("client_id"))
		}
		if r.FormValue("refresh_token") != "old-refresh" {
			t.Errorf("expected refresh_token=old-refresh, got %q", r.FormValue("refresh_token"))
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fresh-access",
			"refresh_token": "fresh-refresh",
			"expires_in":    7200,
			"user":          map[string]string{"id": "user_01", "email": "test@example.com"},
		})
	}))
	defer srv.Close()

	origBase := workosAPIBase
	defer func() { workosAPIBase = origBase }()
	workosAPIBase = srv.URL

	cfg := Config{ClientID: "my-client"}
	tokens, err := RefreshAccessToken(context.Background(), cfg, "old-refresh")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tokens.AccessToken != "fresh-access" {
		t.Errorf("got access token %q, want %q", tokens.AccessToken, "fresh-access")
	}
	if tokens.RefreshToken != "fresh-refresh" {
		t.Errorf("got refresh token %q, want %q", tokens.RefreshToken, "fresh-refresh")
	}
	if tokens.Email != "test@example.com" {
		t.Errorf("got email %q, want %q", tokens.Email, "test@example.com")
	}
}

func TestRefreshAccessToken_KeepsOldRefreshIfNotReissued(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fresh-access",
			"expires_in":   7200,
			"user":         map[string]string{"id": "user_01", "email": "test@example.com"},
		})
	}))
	defer srv.Close()

	origBase := workosAPIBase
	defer func() { workosAPIBase = origBase }()
	workosAPIBase = srv.URL

	cfg := Config{ClientID: "c"}
	tokens, err := RefreshAccessToken(context.Background(), cfg, "keep-me")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tokens.RefreshToken != "keep-me" {
		t.Errorf("expected old refresh token to be preserved, got %q", tokens.RefreshToken)
	}
}

func TestRefreshAccessToken_UsesInjectedHTTPClient(t *testing.T) {
	origBase := workosAPIBase
	origClient := workosHTTPClient
	t.Cleanup(func() {
		workosAPIBase = origBase
		workosHTTPClient = origClient
	})
	workosAPIBase = "https://workos.test"
	called := false
	workosHTTPClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			called = true
			if req.URL.String() != "https://workos.test/user_management/authenticate" {
				t.Fatalf("url = %q", req.URL.String())
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: io.NopCloser(strings.NewReader(`{
					"access_token":"fresh-access",
					"refresh_token":"fresh-refresh",
					"expires_in":7200,
					"user":{"email":"test@example.com"}
				}`)),
			}, nil
		}),
	}

	tokens, err := RefreshAccessToken(context.Background(), Config{ClientID: "c"}, "old-refresh")
	if err != nil {
		t.Fatalf("RefreshAccessToken returned error: %v", err)
	}
	if !called {
		t.Fatal("injected HTTP client was not called")
	}
	if tokens.AccessToken != "fresh-access" {
		t.Fatalf("access token = %q, want fresh-access", tokens.AccessToken)
	}
}

func TestRefreshAccessToken_ErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":             "invalid_grant",
			"error_description": "refresh token is expired",
		})
	}))
	defer srv.Close()

	origBase := workosAPIBase
	defer func() { workosAPIBase = origBase }()
	workosAPIBase = srv.URL

	cfg := Config{ClientID: "c"}
	_, err := RefreshAccessToken(context.Background(), cfg, "expired-refresh")
	if err == nil {
		t.Fatal("expected error for invalid grant")
	}
}

// noopRefreshLock is a lock whose Unlock always succeeds.
type noopRefreshLock struct{}

func (noopRefreshLock) Unlock() error { return nil }

// TestManager_WithCredentialLockBoundsAcquisition pins the sibling contract:
// refreshStoredTokens and bossd's KeychainTokenProvider.refresh both bound lock
// acquisition at refreshLockTimeout, but withCredentialLock passed the caller's
// raw context. `boss logout` passes cmd.Context() (signal-cancelled, no
// deadline) and the TUI passes an app-lifetime context, so a daemon mid-refresh
// could block a logout for as long as it held the lock, silently.
func TestManager_WithCredentialLockBoundsAcquisition(t *testing.T) {
	store := &mockTokenStore{tokens: &Tokens{AccessToken: "token", ExpiresAt: time.Now().Add(time.Hour)}}
	mgr := NewManager(store, Config{})

	origAcquire := acquireRefreshLock
	t.Cleanup(func() { acquireRefreshLock = origAcquire })
	var deadline time.Time
	var hasDeadline bool
	acquireRefreshLock = func(ctx context.Context) (refreshUnlocker, error) {
		deadline, hasDeadline = ctx.Deadline()
		return noopRefreshLock{}, nil
	}

	before := time.Now()
	if err := mgr.Logout(context.Background()); err != nil {
		t.Fatalf("Logout returned error: %v", err)
	}
	if !hasDeadline {
		t.Fatal("credential lock acquired with no deadline; a busy daemon can block logout indefinitely")
	}
	if budget := deadline.Sub(before); budget <= 0 || budget > refreshLockTimeout+time.Second {
		t.Fatalf("acquisition budget = %v, want ~%v (the sibling refresh paths' timeout)", budget, refreshLockTimeout)
	}
}

// TestManager_WithCredentialLockReportsUnlockFailureOnSuccess keeps the
// swallow deliberate but no longer silent. A failed flock Unlock leaves the
// descriptor locked for the rest of the process, so in the long-lived TUI every
// later login, logout, or refresh blocks on a lock nothing will release — a
// warning at the point of failure beats a mystery timeout later. The mutation
// itself stays authoritative (TestManager_LogoutSucceedsWhenUnlockFailsAfterDelete).
func TestManager_WithCredentialLockReportsUnlockFailureOnSuccess(t *testing.T) {
	store := &mockTokenStore{tokens: &Tokens{AccessToken: "token", ExpiresAt: time.Now().Add(time.Hour)}}
	mgr := NewManager(store, Config{})

	origAcquire := acquireRefreshLock
	origOut := credentialLockWarnOut
	t.Cleanup(func() {
		acquireRefreshLock = origAcquire
		credentialLockWarnOut = origOut
	})
	acquireRefreshLock = func(context.Context) (refreshUnlocker, error) {
		return errorRefreshLock{err: errors.New("flock release refused")}, nil
	}
	var warnings bytes.Buffer
	credentialLockWarnOut = &warnings

	if err := mgr.Logout(context.Background()); err != nil {
		t.Fatalf("Logout returned error after successful Delete: %v", err)
	}
	if !strings.Contains(warnings.String(), "flock release refused") {
		t.Fatalf("unlock failure was swallowed silently; warnings = %q", warnings.String())
	}
}

// --- Login verification (BOS-942) ---

// stubCredentialLock replaces the cross-process credential lock with a
// process-local one and returns a pointer to the acquisition count. Without
// this every test here would take the machine-global WorkOS flock and contend
// with every other worktree on the box.
func stubCredentialLock(t *testing.T) *int {
	t.Helper()
	orig := acquireRefreshLock
	var mu sync.Mutex
	count := 0
	acquireRefreshLock = func(context.Context) (refreshUnlocker, error) {
		mu.Lock()
		count++
		return &mutexRefreshLock{mu: &mu}, nil
	}
	t.Cleanup(func() { acquireRefreshLock = orig })
	return &count
}

// stubWorkOSLogin serves both device-code endpoints so Manager.Login and
// Manager.PollLogin can run end to end without the network.
func stubWorkOSLogin(t *testing.T, accessToken, refreshToken, email string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user_management/authorize/device":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code":               "device-code",
				"user_code":                 "USER-CODE",
				"verification_uri":          srvVerificationURI,
				"verification_uri_complete": srvVerificationURI,
				"expires_in":                600,
				"interval":                  0,
			})
		case "/user_management/authenticate":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  accessToken,
				"refresh_token": refreshToken,
				"expires_in":    3600,
				"user":          map[string]string{"id": "user_01", "email": email},
			})
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	origBase := workosAPIBase
	origOpen := openBrowserFn
	workosAPIBase = srv.URL
	openBrowserFn = func(string) error { return nil }
	t.Cleanup(func() {
		workosAPIBase = origBase
		openBrowserFn = origOpen
	})
}

const srvVerificationURI = "https://auth.example.test/device"

// The whole point of the feature: a login that really persisted comes back
// verified, with no reason and no error to explain away.
func TestManagerLogin_VerifiesPersistedRecord(t *testing.T) {
	stubCredentialLock(t)
	stubWorkOSLogin(t, "fresh-access", "fresh-refresh", "dave@example.com")
	store := &mockTokenStore{}
	mgr := NewManager(store, Config{ClientID: "test-client"})

	verdict, err := mgr.Login(context.Background())
	if err != nil {
		t.Fatalf("Login returned error: %v", err)
	}
	if verdict.Outcome != LoginVerified {
		t.Fatalf("outcome = %s (reason %q), want verified", verdict.Outcome, verdict.Reason)
	}
	if verdict.Reason != "" {
		t.Errorf("verified verdict carried reason %q", verdict.Reason)
	}
	if verdict.Err != nil {
		t.Errorf("verified verdict carried error %v", verdict.Err)
	}
	if verdict.Email != "dave@example.com" {
		t.Errorf("email = %q, want dave@example.com", verdict.Email)
	}
}

func TestManagerPollLogin_VerifiesPersistedRecord(t *testing.T) {
	stubCredentialLock(t)
	stubWorkOSLogin(t, "fresh-access", "fresh-refresh", "dave@example.com")
	store := &mockTokenStore{}
	mgr := NewManager(store, Config{ClientID: "test-client"})

	verdict, err := mgr.PollLogin(context.Background(), "device", 0)
	if err != nil {
		t.Fatalf("PollLogin returned error: %v", err)
	}
	if verdict.Outcome != LoginVerified || verdict.Reason != "" || verdict.Err != nil {
		t.Fatalf("verdict = %s/%q/%v, want a clean verified verdict", verdict.Outcome, verdict.Reason, verdict.Err)
	}
}

// A full login must take the credential lock exactly once. Verifying under a
// second acquisition would let a concurrent daemon refresh land between the
// write and the read, and the verdict would then describe somebody else's
// record.
func TestManagerLogin_AcquiresCredentialLockOnce(t *testing.T) {
	count := stubCredentialLock(t)
	stubWorkOSLogin(t, "fresh-access", "fresh-refresh", "dave@example.com")
	store := &mockTokenStore{}
	mgr := NewManager(store, Config{ClientID: "test-client"})

	if _, err := mgr.Login(context.Background()); err != nil {
		t.Fatalf("Login returned error: %v", err)
	}
	if *count != 1 {
		t.Fatalf("credential lock acquisitions = %d, want exactly 1", *count)
	}
	if store.loadCalled != 1 {
		t.Fatalf("read-backs = %d, want exactly 1", store.loadCalled)
	}
}

// The incident's own signature: Save() returns nil, the record never changes,
// and the old release announced success anyway.
func TestManagerCommitLogin_ReportsAccessTokenMismatch(t *testing.T) {
	stubCredentialLock(t)
	stale := &Tokens{
		AccessToken:  "stale-access",
		RefreshToken: "stale-refresh",
		Email:        "dave@example.com",
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	store := &mockTokenStore{tokens: stale}
	mgr := NewManager(store, Config{})
	fresh := &Tokens{
		AccessToken:  "fresh-access",
		RefreshToken: "fresh-refresh",
		Email:        "dave@example.com",
		ExpiresAt:    time.Now().Add(time.Hour),
	}

	verdict, err := mgr.commitLogin(context.Background(), fresh, func() error { return nil })
	if err != nil {
		t.Fatalf("commitLogin returned error: %v", err)
	}
	if verdict.Outcome != LoginVerifyRecordNotUpdated {
		t.Fatalf("outcome = %s, want record_not_updated", verdict.Outcome)
	}
	if verdict.Reason != LoginVerifyReasonAccessTokenMismatch {
		t.Fatalf("reason = %q, want %q", verdict.Reason, LoginVerifyReasonAccessTokenMismatch)
	}
}

// A record that survived the save still flagged for re-login is not a login:
// the daemon would refuse to use it.
func TestManagerCommitLogin_ReportsRetainedReloginFlag(t *testing.T) {
	stubCredentialLock(t)
	flagged := &Tokens{
		AccessToken:   "fresh-access",
		RefreshToken:  "fresh-refresh",
		Email:         "dave@example.com",
		ExpiresAt:     time.Now().Add(time.Hour),
		NeedsRelogin:  true,
		ReloginReason: ReloginReasonRefreshOutcomeUnknown,
	}
	store := &mockTokenStore{tokens: flagged}
	mgr := NewManager(store, Config{})

	verdict, err := mgr.commitLogin(context.Background(), flagged, func() error { return nil })
	if err != nil {
		t.Fatalf("commitLogin returned error: %v", err)
	}
	if verdict.Outcome != LoginVerifyRecordNotUpdated {
		t.Fatalf("outcome = %s, want record_not_updated", verdict.Outcome)
	}
	if verdict.Reason != ReloginReasonRefreshOutcomeUnknown {
		t.Fatalf("reason = %q, want %q", verdict.Reason, ReloginReasonRefreshOutcomeUnknown)
	}
}

// An absent record after a save that claimed success is a verdict, not a
// mystery — and it must not be confused with an unreadable one.
func TestManagerCommitLogin_ReportsRecordAbsent(t *testing.T) {
	stubCredentialLock(t)
	store := &mockTokenStore{}
	mgr := NewManager(store, Config{})

	verdict, err := mgr.commitLogin(context.Background(), &Tokens{AccessToken: "a"}, func() error { return nil })
	if err != nil {
		t.Fatalf("commitLogin returned error: %v", err)
	}
	if verdict.Outcome != LoginVerifyRecordNotUpdated || verdict.Reason != LoginVerifyReasonRecordAbsent {
		t.Fatalf("verdict = %s/%q, want record_not_updated/record_absent", verdict.Outcome, verdict.Reason)
	}
	if verdict.Err != nil {
		t.Fatalf("record_absent must not carry an error: %v", verdict.Err)
	}
}

func TestManagerCommitLogin_ReportsMissingTokens(t *testing.T) {
	stubCredentialLock(t)
	cases := []struct {
		name   string
		stored *Tokens
		want   string
	}{
		{
			name:   "no access token",
			stored: &Tokens{RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour)},
			want:   LoginVerifyReasonNoAccessToken,
		},
		{
			name:   "no refresh token",
			stored: &Tokens{AccessToken: "a", ExpiresAt: time.Now().Add(time.Hour)},
			want:   LoginVerifyReasonNoRefreshToken,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &mockTokenStore{tokens: tc.stored}
			mgr := NewManager(store, Config{})
			verdict, err := mgr.commitLogin(context.Background(), tc.stored, func() error { return nil })
			if err != nil {
				t.Fatalf("commitLogin returned error: %v", err)
			}
			if verdict.Outcome != LoginVerifyRecordNotUpdated || verdict.Reason != tc.want {
				t.Fatalf("verdict = %s/%q, want record_not_updated/%s", verdict.Outcome, verdict.Reason, tc.want)
			}
		})
	}
}

// Verification checks persistence, not freshness. An already-expired access
// token is still a correctly persisted login, because the daemon can refresh
// it — so Tokens.Valid() must stay out of this path.
func TestManagerCommitLogin_ExpiredAccessTokenStillVerifies(t *testing.T) {
	stubCredentialLock(t)
	expired := &Tokens{
		AccessToken:  "fresh-access",
		RefreshToken: "fresh-refresh",
		Email:        "dave@example.com",
		ExpiresAt:    time.Now().Add(-time.Hour),
	}
	if expired.Valid() {
		t.Fatal("fixture is meant to be expired")
	}
	store := &mockTokenStore{tokens: expired}
	mgr := NewManager(store, Config{})

	verdict, err := mgr.commitLogin(context.Background(), expired, func() error { return nil })
	if err != nil {
		t.Fatalf("commitLogin returned error: %v", err)
	}
	if verdict.Outcome != LoginVerified {
		t.Fatalf("outcome = %s (reason %q), want verified", verdict.Outcome, verdict.Reason)
	}
}

// An unreadable record leaves the question open: the login may well have
// persisted. That is inconclusive, with a nil returned error so the command
// still exits zero, and the sentinel reachable through errors.Is.
func TestManagerCommitLogin_UnreadableRecordIsInconclusive(t *testing.T) {
	stubCredentialLock(t)
	store := &mockTokenStore{loadErr: fmt.Errorf("decrypt: %w", ErrCredentialsUnreadable)}
	mgr := NewManager(store, Config{})

	verdict, err := mgr.commitLogin(context.Background(), &Tokens{AccessToken: "a"}, func() error { return nil })
	if err != nil {
		t.Fatalf("commitLogin returned error: %v", err)
	}
	if verdict.Outcome != LoginVerifyInconclusive || verdict.Reason != LoginVerifyReasonReadFailed {
		t.Fatalf("verdict = %s/%q, want inconclusive/read_failed", verdict.Outcome, verdict.Reason)
	}
	if !errors.Is(verdict.Err, ErrCredentialsUnreadable) {
		t.Fatalf("Err = %v, want it to wrap ErrCredentialsUnreadable", verdict.Err)
	}
}

// A keychain that hangs must not pin the credential lock against every other
// process: the read-back carries its own bound, and blowing it releases the
// lock with an inconclusive verdict.
func TestManagerCommitLogin_ReadBoundReleasesLock(t *testing.T) {
	stubCredentialLock(t)
	// The caller's own deadline bounds the read-back too — it is the tighter
	// of the two — so a short parent context exercises the same abandon path a
	// blown refreshLockTimeout would.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	gate := make(chan struct{})
	t.Cleanup(func() { close(gate) })
	store := &mockTokenStore{
		tokens:   &Tokens{AccessToken: "a", RefreshToken: "r"},
		loadGate: gate,
	}
	mgr := NewManager(store, Config{})

	done := make(chan LoginVerification, 1)
	go func() {
		verdict, err := mgr.commitLogin(ctx, &Tokens{AccessToken: "a"}, func() error { return nil })
		if err != nil {
			t.Errorf("commitLogin returned error: %v", err)
		}
		done <- verdict
	}()

	select {
	case verdict := <-done:
		if verdict.Outcome != LoginVerifyInconclusive || verdict.Reason != LoginVerifyReasonLockTimeout {
			t.Fatalf("verdict = %s/%q, want inconclusive/lock_timeout", verdict.Outcome, verdict.Reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("commitLogin never returned; the read-back bound did not fire")
	}

	// The lock is released, so a second credential mutation can proceed.
	released := make(chan error, 1)
	go func() { released <- mgr.Logout(context.Background()) }()
	select {
	case err := <-released:
		if err != nil {
			t.Fatalf("Logout after an abandoned read-back failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("credential lock was never released")
	}
}

// A failed save has nothing to prove: the error comes back unchanged and the
// read-back never runs.
func TestManagerCommitLogin_SaveFailureSkipsVerification(t *testing.T) {
	stubCredentialLock(t)
	saveErr := errors.New("keychain write refused")
	store := &mockTokenStore{}
	mgr := NewManager(store, Config{})

	verdict, err := mgr.commitLogin(context.Background(), &Tokens{AccessToken: "a"}, func() error { return saveErr })
	if !errors.Is(err, saveErr) {
		t.Fatalf("err = %v, want the save error", err)
	}
	if verdict.Outcome != 0 {
		t.Fatalf("outcome = %s, want the zero 'never ran' sentinel", verdict.Outcome)
	}
	if store.loadCalled != 0 {
		t.Fatalf("read-backs = %d, want 0 after a failed save", store.loadCalled)
	}
}

// The e2e login seam persists from inside its own callback, so the Manager
// never sees the record it wrote. It records that record on the Manager
// instead, and commitLogin falls back to it — without which a silently
// no-oped save over an unflagged record would render as a successful login,
// exactly the class this verification exists to catch.
func TestManagerPollLogin_SeamBranchRunsEqualityLeg(t *testing.T) {
	stubCredentialLock(t)
	stale := &Tokens{
		AccessToken:  "stale-access",
		RefreshToken: "stale-refresh",
		Email:        "dave@example.com",
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	store := &mockTokenStore{tokens: stale}
	mgr := NewManager(store, Config{})
	// Stand in for SetE2ELogin: record what the seam meant to persist, then
	// let the save silently do nothing.
	mgr.pollLogin = func(context.Context, string, int) error {
		mgr.lastE2ETokens = &Tokens{
			AccessToken:  "seam-access",
			RefreshToken: "seam-refresh",
			Email:        "dave@example.com",
			ExpiresAt:    time.Now().Add(time.Hour),
		}
		return nil
	}

	verdict, err := mgr.PollLogin(context.Background(), "device", 0)
	if err != nil {
		t.Fatalf("PollLogin returned error: %v", err)
	}
	if verdict.Outcome != LoginVerifyRecordNotUpdated {
		t.Fatalf("outcome = %s, want record_not_updated", verdict.Outcome)
	}
	if verdict.Reason != LoginVerifyReasonAccessTokenMismatch {
		t.Fatalf("reason = %q, want %q", verdict.Reason, LoginVerifyReasonAccessTokenMismatch)
	}
}

// A seam record left over from an earlier login must never satisfy a later
// login's equality leg.
func TestManagerPollLogin_SeamRecordIsResetPerAttempt(t *testing.T) {
	stubCredentialLock(t)
	stored := &Tokens{
		AccessToken:  "stored-access",
		RefreshToken: "stored-refresh",
		Email:        "dave@example.com",
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	store := &mockTokenStore{tokens: stored}
	mgr := NewManager(store, Config{})
	mgr.lastE2ETokens = &Tokens{AccessToken: "stored-access"}
	mgr.pollLogin = func(context.Context, string, int) error { return nil }

	verdict, err := mgr.PollLogin(context.Background(), "device", 0)
	if err != nil {
		t.Fatalf("PollLogin returned error: %v", err)
	}
	// With the stale seam record cleared there is no expectation to compare
	// against, so the remaining legs decide: the record is present and usable.
	if verdict.Outcome != LoginVerified {
		t.Fatalf("outcome = %s (reason %q), want verified", verdict.Outcome, verdict.Reason)
	}
	if mgr.lastE2ETokens != nil {
		t.Fatal("seam record survived a login that did not set one")
	}
}

// --- Save-path instrumentation (BOS-942 U4) ---

// captureGlobalLog installs a buffer on the global zerolog logger for the
// duration of the test and returns it.
func captureGlobalLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := zlog.Logger
	zlog.Logger = zerolog.New(&buf)
	t.Cleanup(func() { zlog.Logger = orig })
	return &buf
}

// assertNoTokenMaterial fails if any fixture token value reached the buffer.
// The save-path lines exist to make the next incident diagnosable; they must
// not make the log a credential store.
func assertNoTokenMaterial(t *testing.T, buf *bytes.Buffer, values ...string) {
	t.Helper()
	got := buf.String()
	for _, v := range values {
		if v == "" {
			continue
		}
		if strings.Contains(got, v) {
			t.Fatalf("log leaked token material %q: %s", v, got)
		}
	}
}

func TestCommitLogin_LogsSaveAndVerdict(t *testing.T) {
	stubCredentialLock(t)
	buf := captureGlobalLog(t)

	saved := &Tokens{
		AccessToken:  "secret-access-value",
		RefreshToken: "secret-refresh-value",
		Email:        "dave@example.com",
		ExpiresAt:    time.Now().Add(time.Hour),
	}
	store := &mockTokenStore{tokens: saved}
	mgr := NewManager(store, Config{})

	verdict, err := mgr.commitLogin(context.Background(), saved, func() error { return nil })
	if err != nil {
		t.Fatalf("commitLogin returned error: %v", err)
	}
	if verdict.Outcome != LoginVerified {
		t.Fatalf("outcome = %s, want verified", verdict.Outcome)
	}

	got := buf.String()
	for _, want := range []string{
		`"component":"auth-store"`,
		`"email":"dave@example.com"`,
		`"expires_at"`,
		`"needs_relogin":false`,
		`"relogin_reason":""`,
		`"outcome":"verified"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log missing %s\ngot: %s", want, got)
		}
	}
	assertNoTokenMaterial(t, buf, saved.AccessToken, saved.RefreshToken)
}

func TestCommitLogin_LogsMarkerStateOnANotUpdatedVerdict(t *testing.T) {
	stubCredentialLock(t)
	buf := captureGlobalLog(t)

	stored := &Tokens{
		AccessToken:   "secret-access-value",
		RefreshToken:  "secret-refresh-value",
		Email:         "dave@example.com",
		ExpiresAt:     time.Now().Add(time.Hour),
		NeedsRelogin:  true,
		ReloginReason: ReloginReasonRefreshTokenRejected,
	}
	store := &mockTokenStore{tokens: stored}
	mgr := NewManager(store, Config{})

	if _, err := mgr.commitLogin(context.Background(), stored, func() error { return nil }); err != nil {
		t.Fatalf("commitLogin returned error: %v", err)
	}

	got := buf.String()
	if !strings.Contains(got, `"needs_relogin":true`) {
		t.Errorf("log missing the marker state\ngot: %s", got)
	}
	if !strings.Contains(got, `"outcome":"record_not_updated"`) {
		t.Errorf("log missing the verdict outcome\ngot: %s", got)
	}
	if !strings.Contains(got, `"reason":"`+ReloginReasonRefreshTokenRejected+`"`) {
		t.Errorf("log missing the enumerated reason\ngot: %s", got)
	}
	assertNoTokenMaterial(t, buf, stored.AccessToken, stored.RefreshToken)
}

// A failed save has nothing to report: no save line, no verdict line.
func TestCommitLogin_NoLogWhenSaveFails(t *testing.T) {
	stubCredentialLock(t)
	buf := captureGlobalLog(t)

	store := &mockTokenStore{}
	mgr := NewManager(store, Config{})
	saved := &Tokens{AccessToken: "secret-access-value", RefreshToken: "secret-refresh-value"}

	_, err := mgr.commitLogin(context.Background(), saved, func() error { return errors.New("write refused") })
	if err == nil {
		t.Fatal("expected the save error to be returned")
	}
	if strings.Contains(buf.String(), "auth-store") {
		t.Fatalf("a failed save emitted a save-path log line: %s", buf.String())
	}
}

// The inconclusive verdict's Err can wrap a keyring error whose text embeds
// record bytes. It must never reach the log.
func TestCommitLogin_InconclusiveVerdictNeverLogsTheError(t *testing.T) {
	stubCredentialLock(t)
	buf := captureGlobalLog(t)

	store := &mockTokenStore{loadErr: fmt.Errorf("keyring blob secret-access-value: %w", ErrCredentialsUnreadable)}
	mgr := NewManager(store, Config{})
	saved := &Tokens{AccessToken: "secret-access-value", RefreshToken: "secret-refresh-value", Email: "dave@example.com"}

	verdict, err := mgr.commitLogin(context.Background(), saved, func() error { return nil })
	if err != nil {
		t.Fatalf("commitLogin returned error: %v", err)
	}
	if verdict.Outcome != LoginVerifyInconclusive {
		t.Fatalf("outcome = %s, want inconclusive", verdict.Outcome)
	}
	if !strings.Contains(buf.String(), `"outcome":"inconclusive"`) {
		t.Errorf("log missing the inconclusive outcome\ngot: %s", buf.String())
	}
	assertNoTokenMaterial(t, buf, saved.AccessToken, saved.RefreshToken)
	if strings.Contains(buf.String(), "keyring blob") {
		t.Fatalf("log rendered the raw verification error: %s", buf.String())
	}
}
