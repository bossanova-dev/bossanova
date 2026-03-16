package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
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
	// Set up a mock Auth0 token endpoint.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			http.Error(w, "not found", 404)
			return
		}
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
			"id_token":      "",
			"expires_in":    3600,
			"token_type":    "Bearer",
		})
	}))
	defer srv.Close()

	store := &mockTokenStore{
		tokens: &Tokens{
			AccessToken:  "expired-token",
			RefreshToken: "my-refresh",
			ExpiresAt:    time.Now().Add(-time.Hour),
		},
	}

	cfg := Config{
		Issuer:   srv.URL + "/",
		ClientID: "test-client",
	}
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
	// Create a minimal ID token with email claim.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	idToken, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub":   "auth0|user123",
		"email": "dave@example.com",
		"name":  "Dave",
	}).SignedString(key)
	if err != nil {
		t.Fatalf("sign id token: %v", err)
	}

	store := &mockTokenStore{
		tokens: &Tokens{
			AccessToken: "token",
			IDToken:     idToken,
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

func TestManager_Status_NotLoggedIn(t *testing.T) {
	store := &mockTokenStore{loadErr: fmt.Errorf("no tokens")}
	mgr := NewManager(store, Config{})

	status := mgr.Status()
	if status.LoggedIn {
		t.Error("expected LoggedIn = false")
	}
}

// --- Claims parsing ---

func TestParseIDTokenClaims(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	idToken, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub":   "auth0|user456",
		"email": "test@example.com",
		"name":  "Test User",
	}).SignedString(key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	claims, err := parseIDTokenClaims(idToken)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if claims.Sub != "auth0|user456" {
		t.Errorf("got sub %q, want %q", claims.Sub, "auth0|user456")
	}
	if claims.Email != "test@example.com" {
		t.Errorf("got email %q, want %q", claims.Email, "test@example.com")
	}
	if claims.Name != "Test User" {
		t.Errorf("got name %q, want %q", claims.Name, "Test User")
	}
}

func TestParseIDTokenClaims_InvalidFormat(t *testing.T) {
	_, err := parseIDTokenClaims("not-a-jwt")
	if err == nil {
		t.Error("expected error for invalid JWT format")
	}
}

// --- PKCE helpers ---

func TestCodeVerifierAndChallenge(t *testing.T) {
	verifier, err := generateCodeVerifier()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(verifier) < 32 {
		t.Errorf("verifier too short: %d chars", len(verifier))
	}

	challenge := codeChallenge(verifier)
	if challenge == "" {
		t.Error("challenge should not be empty")
	}
	if challenge == verifier {
		t.Error("challenge should not equal verifier")
	}

	// Verify different verifiers produce different challenges.
	verifier2, _ := generateCodeVerifier()
	challenge2 := codeChallenge(verifier2)
	if challenge == challenge2 {
		t.Error("different verifiers should produce different challenges")
	}
}

// --- RefreshAccessToken ---

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
			"token_type":    "Bearer",
		})
	}))
	defer srv.Close()

	cfg := Config{
		Issuer:   srv.URL + "/",
		ClientID: "my-client",
	}

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
}

func TestRefreshAccessToken_KeepsOldRefreshIfNotReissued(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fresh-access",
			// No refresh_token in response — keep old.
			"expires_in": 7200,
			"token_type": "Bearer",
		})
	}))
	defer srv.Close()

	cfg := Config{Issuer: srv.URL + "/", ClientID: "c"}

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

	cfg := Config{Issuer: srv.URL + "/", ClientID: "c"}

	_, err := RefreshAccessToken(context.Background(), cfg, "expired-refresh")
	if err == nil {
		t.Fatal("expected error for invalid grant")
	}
}

// --- Login PKCE flow (end-to-end with mock auth server) ---

func TestLogin_PKCEFlow(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	var capturedState, capturedChallenge, capturedRedirectURI string

	// Mock Auth0 server that handles authorize + token endpoints.
	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/authorize":
			// Capture the PKCE parameters.
			capturedState = r.URL.Query().Get("state")
			capturedChallenge = r.URL.Query().Get("code_challenge")
			capturedRedirectURI = r.URL.Query().Get("redirect_uri")

			if r.URL.Query().Get("code_challenge_method") != "S256" {
				t.Error("expected code_challenge_method=S256")
			}
			if r.URL.Query().Get("response_type") != "code" {
				t.Error("expected response_type=code")
			}

			// Simulate browser redirect to callback with code.
			callbackURL := fmt.Sprintf("%s?code=test-auth-code&state=%s",
				capturedRedirectURI, capturedState)

			http.Redirect(w, r, callbackURL, http.StatusFound)

		case "/oauth/token":
			if r.FormValue("grant_type") != "authorization_code" {
				t.Errorf("expected grant_type=authorization_code, got %q", r.FormValue("grant_type"))
			}
			if r.FormValue("code") != "test-auth-code" {
				t.Errorf("expected code=test-auth-code, got %q", r.FormValue("code"))
			}

			// Generate a real ID token.
			idToken, _ := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
				"sub":   "auth0|testuser",
				"email": "test@example.com",
			}).SignedString(key)

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "pkce-access-token",
				"refresh_token": "pkce-refresh-token",
				"id_token":      idToken,
				"expires_in":    3600,
				"token_type":    "Bearer",
			})
		}
	}))
	defer authSrv.Close()

	cfg := Config{
		Issuer:   authSrv.URL + "/",
		ClientID: "test-pkce-client",
		Audience: "https://api.test.bossanova.dev",
	}

	// Override openBrowser to simulate the browser redirect.
	// In the real flow, the browser opens the authorize URL, user logs in,
	// and Auth0 redirects to the callback. We simulate this by making an
	// HTTP request to the authorize endpoint, which redirects to our callback.
	origOpenBrowser := openBrowserFn
	openBrowserFn = func(url string) error {
		// Follow the authorize URL to trigger the callback redirect.
		client := &http.Client{
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return nil // follow redirects
			},
		}
		resp, err := client.Get(url)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		return nil
	}
	defer func() { openBrowserFn = origOpenBrowser }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tokens, err := Login(ctx, cfg)
	if err != nil {
		t.Fatalf("Login failed: %v", err)
	}

	if tokens.AccessToken != "pkce-access-token" {
		t.Errorf("got access token %q, want %q", tokens.AccessToken, "pkce-access-token")
	}
	if tokens.RefreshToken != "pkce-refresh-token" {
		t.Errorf("got refresh token %q, want %q", tokens.RefreshToken, "pkce-refresh-token")
	}

	if capturedState == "" {
		t.Error("state parameter was not captured")
	}
	if capturedChallenge == "" {
		t.Error("code_challenge parameter was not captured")
	}
}
