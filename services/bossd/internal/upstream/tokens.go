// Package upstream — tokens.go owns the keychain-backed WorkOS token
// loader used by both the legacy heartbeat Manager (behind the
// legacy_upstream build tag) and the new StreamClient. T3.7 lifted this
// out from upstream.go so the default build can compile without the
// legacy RPCs present. Phase 8 deletes the legacy copy; this file
// survives as the single source of truth.
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/99designs/keyring"
	"github.com/recurser/bossalib/keyringutil"
)

// keychainTokens mirrors the boss CLI token structure for keychain reading.
type keychainTokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// defaultWorkOSClientID is the production WorkOS client used when
// BOSS_WORKOS_CLIENT_ID is unset. Mirrors services/boss/cmd/auth.go so
// `boss login` and the bossd refresh path agree on the same client out of
// the box. Override for staging / self-host.
const defaultWorkOSClientID = "client_01KP805YXXAMZSN2YB4NGXS9XB"

// openKeyring opens the shared bossanova keyring. bossd runs as a daemon
// with no flag plumbing, so allowInsecure is hard-wired to false here — a
// broken environment should surface a real error rather than silently
// reverting to the hardcoded passphrase.
func openKeyring() (keyring.Keyring, error) {
	return keyring.Open(keyring.Config{
		ServiceName:              "bossanova",
		KeychainTrustApplication: true,
		FileDir:                  "~/.config/bossanova/keyring",
		FilePasswordFunc:         keyring.PromptFunc(keyringutil.New(false)),
		// Optional override via BOSS_KEYRING_BACKEND. Stays in lock-step
		// with the boss CLI so a developer who exports the env var sees
		// the same backend on both processes.
		AllowedBackends: keyringutil.Backends(),
	})
}

// refreshWorkOSToken exchanges a refresh token for a new access token.
func refreshWorkOSToken(clientID, refreshToken string) (*keychainTokens, error) {
	data := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {refreshToken},
	}

	resp, err := http.Post(
		"https://api.workos.com/user_management/authenticate",
		"application/x-www-form-urlencoded",
		strings.NewReader(data.Encode()),
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("refresh failed (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		User         struct {
			Email string `json:"email"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	return &keychainTokens{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(result.ExpiresIn) * time.Second),
	}, nil
}

// KeychainTokenProvider is a TokenProvider backed by the "boss login"
// keychain entry. It caches the last-known access token in memory so the
// StreamClient's refresher can observe Token()/ExpiresAt() without a
// keychain read on every tick, then falls back to the keychain on Refresh.
type KeychainTokenProvider struct {
	mu           sync.RWMutex
	accessToken  string
	refreshToken string
	expiresAt    time.Time

	// clientIDEnv is the env var that holds the WorkOS client ID. Split
	// out so tests can point at a fake without touching the real env.
	clientIDEnv string
}

// NewKeychainTokenProvider constructs a provider and populates it from
// the keychain at construction time. A missing keychain entry is not an
// error — Token() simply returns "". Callers can still run the stream
// without auth (local-only mode) or let bosso reject the handshake.
func NewKeychainTokenProvider() *KeychainTokenProvider {
	p := &KeychainTokenProvider{clientIDEnv: "BOSS_WORKOS_CLIENT_ID"}
	p.loadFromKeychain()
	return p
}

// loadFromKeychain snapshots the keychain entry into the in-memory cache.
// Safe to call repeatedly; no-op when the entry is missing.
func (p *KeychainTokenProvider) loadFromKeychain() {
	ring, err := openKeyring()
	if err != nil {
		return
	}
	item, err := ring.Get("workos-tokens")
	if err != nil {
		return
	}
	var tokens keychainTokens
	if err := json.Unmarshal(item.Data, &tokens); err != nil {
		return
	}
	p.mu.Lock()
	p.accessToken = tokens.AccessToken
	p.refreshToken = tokens.RefreshToken
	p.expiresAt = tokens.ExpiresAt
	p.mu.Unlock()
}

// Token implements TokenProvider.Token.
func (p *KeychainTokenProvider) Token() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.accessToken
}

// ExpiresAt implements TokenProvider.ExpiresAt.
func (p *KeychainTokenProvider) ExpiresAt() time.Time {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.expiresAt
}

// Refresh implements TokenProvider.Refresh by invoking the WorkOS
// refresh flow and persisting the new tokens back to the keychain. Errors
// from the keychain write are swallowed (the refreshed token is still
// usable in memory for this daemon's lifetime).
func (p *KeychainTokenProvider) Refresh(_ context.Context) (string, error) {
	p.mu.RLock()
	refreshTok := p.refreshToken
	p.mu.RUnlock()

	clientID := os.Getenv(p.clientIDEnv)
	if clientID == "" {
		clientID = defaultWorkOSClientID
	}
	if refreshTok == "" || clientID == "" {
		return "", fmt.Errorf("refresh not configured (empty refresh token or %s)", p.clientIDEnv)
	}

	refreshed, err := refreshWorkOSToken(clientID, refreshTok)
	if err != nil {
		return "", err
	}
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = refreshTok
	}

	p.mu.Lock()
	p.accessToken = refreshed.AccessToken
	p.refreshToken = refreshed.RefreshToken
	p.expiresAt = refreshed.ExpiresAt
	p.mu.Unlock()

	// Persist to keychain best-effort. A failure here doesn't block the
	// stream — the in-memory cache still holds the fresh token until the
	// daemon restarts.
	if ring, err := openKeyring(); err == nil {
		if data, err := json.Marshal(refreshed); err == nil {
			_ = ring.Set(keyring.Item{
				Key:         "workos-tokens",
				Data:        data,
				Label:       "Bossanova",
				Description: "WorkOS authentication tokens",
			})
		}
	}

	return refreshed.AccessToken, nil
}
