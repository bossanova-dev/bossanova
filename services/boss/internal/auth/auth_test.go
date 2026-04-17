package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// --- Mock token store ---

type mockTokenStore struct {
	tokens  *Tokens
	saveErr error
	loadErr error
	delErr  error

	saveCalled   bool
	deleteCalled bool
}

func (m *mockTokenStore) Save(tokens *Tokens) error {
	m.saveCalled = true
	if m.saveErr != nil {
		return m.saveErr
	}
	m.tokens = tokens
	return nil
}

func (m *mockTokenStore) Load() (*Tokens, error) {
	if m.loadErr != nil {
		return nil, m.loadErr
	}
	if m.tokens == nil {
		return nil, fmt.Errorf("no tokens stored")
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

// --- Manager.Logout ---

func TestManager_Logout(t *testing.T) {
	store := &mockTokenStore{
		tokens: &Tokens{AccessToken: "token", ExpiresAt: time.Now().Add(time.Hour)},
	}
	mgr := NewManager(store, Config{})

	if err := mgr.Logout(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !store.deleteCalled {
		t.Error("expected Delete to be called")
	}
	if store.tokens != nil {
		t.Error("tokens should be nil after logout")
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
