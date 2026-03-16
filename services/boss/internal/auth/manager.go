package auth

import (
	"context"
	"fmt"
	"time"
)

// Manager coordinates token loading, refresh, and persistence.
type Manager struct {
	store  TokenStore
	config Config
}

// NewManager creates a Manager with the given store and OIDC config.
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

	// Add a buffer so we refresh a bit before actual expiry.
	refreshed, err := RefreshAccessToken(ctx, m.config, tokens.RefreshToken)
	if err != nil {
		return "", fmt.Errorf("refresh token: %w (run 'boss login' to re-authenticate)", err)
	}

	if err := m.store.Save(refreshed); err != nil {
		return "", fmt.Errorf("save refreshed tokens: %w", err)
	}

	return refreshed.AccessToken, nil
}

// Login performs the PKCE flow and stores the resulting tokens.
func (m *Manager) Login(ctx context.Context) error {
	tokens, err := Login(ctx, m.config)
	if err != nil {
		return err
	}
	return m.store.Save(tokens)
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
func (m *Manager) Status() *Status {
	tokens, err := m.store.Load()
	if err != nil {
		return &Status{LoggedIn: false}
	}

	s := &Status{
		LoggedIn:  tokens.Valid(),
		ExpiresAt: tokens.ExpiresAt,
	}

	// Try to extract email from ID token claims (JWT payload).
	if tokens.IDToken != "" {
		if claims, err := parseIDTokenClaims(tokens.IDToken); err == nil {
			s.Email = claims.Email
		}
	}

	return s
}
