package auth

import (
	"context"
	"fmt"
	"time"
)

// Config holds WorkOS provider configuration.
type Config struct {
	ClientID string // WorkOS application client ID
}

// Manager coordinates token loading, refresh, and persistence.
type Manager struct {
	store  TokenStore
	config Config
}

// NewManager creates a Manager with the given store and WorkOS config.
func NewManager(store TokenStore, cfg Config) *Manager {
	return &Manager{store: store, config: cfg}
}

// AccessToken returns a valid access token, refreshing if needed.
// Returns empty string (no error) if no tokens are stored — callers
// should treat this as unauthenticated (local mode).
func (m *Manager) AccessToken(ctx context.Context) (string, error) {
	tokens, err := m.store.Load()
	if err != nil {
		// No stored tokens — not logged in.
		return "", nil
	}

	if tokens.Valid() {
		return tokens.AccessToken, nil
	}

	// Token expired — try refresh.
	if tokens.RefreshToken == "" {
		return "", fmt.Errorf("access token expired and no refresh token available; run 'boss login'")
	}

	refreshed, err := RefreshAccessToken(ctx, m.config, tokens.RefreshToken)
	if err != nil {
		return "", fmt.Errorf("refresh token: %w (run 'boss login' to re-authenticate)", err)
	}

	if err := m.store.Save(refreshed); err != nil {
		return "", fmt.Errorf("save refreshed tokens: %w", err)
	}

	return refreshed.AccessToken, nil
}

// Login performs the WorkOS device code flow and stores the resulting tokens.
func (m *Manager) Login(ctx context.Context) error {
	result, err := Login(ctx, m.config)
	if err != nil {
		return err
	}
	return m.store.Save(result.Tokens)
}

// StartLogin initiates the device code flow and returns the device code
// response without printing to stdout (safe for TUI use).
func (m *Manager) StartLogin(ctx context.Context) (*DeviceCodeResponse, error) {
	return RequestDeviceCode(ctx, m.config)
}

// PollLogin polls for token completion and saves the resulting tokens.
func (m *Manager) PollLogin(ctx context.Context, deviceCode string, interval int) error {
	result, err := PollForToken(ctx, m.config, deviceCode, interval)
	if err != nil {
		return err
	}
	return m.store.Save(result.Tokens)
}

// Logout removes stored tokens.
func (m *Manager) Logout() error {
	return m.store.Delete()
}

// Status returns the current login status for display.
type Status struct {
	LoggedIn  bool
	ExpiresAt time.Time
	Email     string
}

// Status reports whether the user is logged in.
// A user is considered logged in if they have stored tokens — even if the
// access token has expired — as long as a refresh token is available.
func (m *Manager) Status() *Status {
	tokens, err := m.store.Load()
	if err != nil {
		return &Status{LoggedIn: false}
	}

	loggedIn := tokens.Valid() || tokens.RefreshToken != ""

	return &Status{
		LoggedIn:  loggedIn,
		ExpiresAt: tokens.ExpiresAt,
		Email:     tokens.Email,
	}
}
