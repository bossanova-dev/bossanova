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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/99designs/keyring"
	"github.com/recurser/bossalib/authlock"
	"github.com/recurser/bossalib/authtoken"
	"github.com/recurser/bossalib/keyringutil"
)

// ErrAuthExpired is returned (wrapped) by Refresh when WorkOS rejects the
// stored refresh token as terminally invalid — the user's session has
// ended and no amount of retrying will recover it. Callers detect this
// with errors.Is and treat it as "stop retrying, wait for the user to
// log in again" rather than continuing the normal reconnect/backoff loop.
var ErrAuthExpired = errors.New("auth expired: re-login required")

// keychainTokens mirrors the boss CLI token structure for keychain reading.
type keychainTokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	Email        string    `json:"email,omitempty"`
}

func (t *keychainTokens) valid() bool {
	return t != nil && t.AccessToken != "" && time.Now().Before(t.ExpiresAt)
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
var (
	workOSRefreshHTTPClient = &http.Client{Timeout: 10 * time.Second}
	workOSAuthenticateURL   = "https://api.workos.com/user_management/authenticate"
	openKeyring             = func() (keyring.Keyring, error) {
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
	refreshWorkOSTokenFn = refreshWorkOSToken
	acquireRefreshLock   = func(ctx context.Context) (refreshUnlocker, error) {
		return authlock.AcquireWorkOSRefreshLock(ctx)
	}
	loadKeychainTokensFn   = loadKeychainTokens
	saveKeychainTokensFn   = saveKeychainTokens
	removeKeychainTokensFn = removeKeychainTokens
)

type refreshUnlocker interface {
	Unlock() error
}

func loadKeychainTokens() (*keychainTokens, error) {
	ring, err := openKeyring()
	if err != nil {
		return nil, err
	}
	item, err := ring.Get("workos-tokens")
	if err != nil {
		return nil, err
	}
	var tokens keychainTokens
	if err := json.Unmarshal(item.Data, &tokens); err != nil {
		return nil, err
	}
	return &tokens, nil
}

func saveKeychainTokens(tokens *keychainTokens) error {
	ring, err := openKeyring()
	if err != nil {
		return err
	}
	data, err := json.Marshal(tokens)
	if err != nil {
		return err
	}
	return ring.Set(keyring.Item{
		Key:         "workos-tokens",
		Data:        data,
		Label:       "Bossanova",
		Description: "WorkOS authentication tokens",
	})
}

func removeKeychainTokens() error {
	ring, err := openKeyring()
	if err != nil {
		return err
	}
	return ring.Remove("workos-tokens")
}

// refreshWorkOSToken exchanges a refresh token for a new access token.
func refreshWorkOSToken(ctx context.Context, clientID, refreshToken string) (*keychainTokens, error) {
	data := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {refreshToken},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, workOSAuthenticateURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := workOSRefreshHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		// WorkOS returns HTTP 400 with {"error":"invalid_grant"} when the
		// refresh token has been revoked or its session ended. That's
		// terminal — wrap with ErrAuthExpired so the stream client can
		// pause instead of tight-looping on a credential that will never
		// work again.
		if resp.StatusCode == http.StatusBadRequest {
			var errBody struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(body, &errBody) == nil && errBody.Error == "invalid_grant" {
				return nil, fmt.Errorf("refresh failed (HTTP %d): %s: %w", resp.StatusCode, string(body), ErrAuthExpired)
			}
		}
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
		ExpiresAt:    authtoken.AccessTokenExpiry(result.AccessToken, result.ExpiresIn, time.Now()),
		Email:        result.User.Email,
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
	p.Reload()
	return p
}

// Reload re-reads the keychain entry into the in-memory cache. Used by
// the auth-change notifier so a fresh `boss login` (which writes new
// tokens to the keychain) is observable to the running daemon without a
// restart — calling Refresh here would fail because the cached refresh
// token has been superseded by the new login.
func (p *KeychainTokenProvider) Reload() {
	p.loadFromKeychain()
}

// loadFromKeychain snapshots the keychain entry into the in-memory cache.
// Safe to call repeatedly; no-op when the entry is missing.
func (p *KeychainTokenProvider) loadFromKeychain() {
	tokens, err := loadKeychainTokensFn()
	if err != nil {
		return
	}
	p.mu.Lock()
	p.applyTokensLocked(tokens)
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

// Refresh implements TokenProvider.Refresh by invoking the WorkOS refresh flow
// under the shared refresh lock and persisting the new tokens back to keychain.
func (p *KeychainTokenProvider) Refresh(ctx context.Context) (tok string, retErr error) {
	p.mu.RLock()
	originalAccess := p.accessToken
	refreshTok := p.refreshToken
	p.mu.RUnlock()
	originalRefreshTok := refreshTok

	clientID := os.Getenv(p.clientIDEnv)
	if clientID == "" {
		clientID = defaultWorkOSClientID
	}
	if refreshTok == "" || clientID == "" {
		return "", fmt.Errorf("refresh not configured (empty refresh token or %s)", p.clientIDEnv)
	}

	lockCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lock, err := acquireRefreshLock(lockCtx)
	if err != nil {
		return "", fmt.Errorf("acquire refresh lock: %w", err)
	}
	defer func() {
		if unlockErr := lock.Unlock(); unlockErr != nil {
			retErr = errors.Join(retErr, unlockErr)
		}
	}()

	latest, err := loadKeychainTokensFn()
	if err == nil {
		p.mu.Lock()
		p.applyTokensLocked(latest)
		p.mu.Unlock()
		if latest.valid() && (latest.RefreshToken != originalRefreshTok || latest.AccessToken != originalAccess) {
			return latest.AccessToken, nil
		}
		if latest.RefreshToken != "" {
			refreshTok = latest.RefreshToken
		}
	}

	for attempts := 0; attempts < 2; attempts++ {
		requestCtx, requestCancel := context.WithTimeout(ctx, 10*time.Second)
		refreshed, err := refreshWorkOSTokenFn(requestCtx, clientID, refreshTok)
		requestCancel()
		if err != nil {
			if errors.Is(err, ErrAuthExpired) {
				reloaded, loadErr := loadKeychainTokensFn()
				if loadErr == nil && reloaded.RefreshToken != "" && reloaded.RefreshToken != refreshTok {
					p.mu.Lock()
					p.applyTokensLocked(reloaded)
					p.mu.Unlock()
					if reloaded.valid() {
						return reloaded.AccessToken, nil
					}
					refreshTok = reloaded.RefreshToken
					continue
				}
				p.clear()
				return "", errors.Join(err, removeKeychainTokensFn())
			}
			return "", err
		}
		if refreshed.RefreshToken == "" {
			refreshed.RefreshToken = refreshTok
		}
		if latest != nil && refreshed.Email == "" {
			refreshed.Email = latest.Email
		}
		current, loadErr := loadKeychainTokensFn()
		if loadErr == nil && current.RefreshToken != "" && current.RefreshToken != refreshTok {
			p.mu.Lock()
			p.applyTokensLocked(current)
			p.mu.Unlock()
			if current.valid() {
				return current.AccessToken, nil
			}
			latest = current
			refreshTok = current.RefreshToken
			continue
		}
		p.mu.Lock()
		p.applyTokensLocked(refreshed)
		p.mu.Unlock()
		if err := saveKeychainTokensFn(refreshed); err != nil {
			return refreshed.AccessToken, fmt.Errorf("save refreshed tokens: %w", err)
		}
		return refreshed.AccessToken, nil
	}

	return "", ErrAuthExpired
}

func (p *KeychainTokenProvider) applyTokensLocked(tokens *keychainTokens) {
	p.accessToken = tokens.AccessToken
	p.refreshToken = tokens.RefreshToken
	p.expiresAt = tokens.ExpiresAt
}

func (p *KeychainTokenProvider) clear() {
	p.mu.Lock()
	p.accessToken = ""
	p.refreshToken = ""
	p.expiresAt = time.Time{}
	p.mu.Unlock()
}
