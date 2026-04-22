//go:build e2e

package main

import (
	"errors"
	"os"
	"sync"
	"time"

	"github.com/recurser/boss/internal/auth"
)

// errE2ENoTokens is returned from the e2e memory store when no tokens are
// present. Using a distinct error avoids the "run 'boss login' to reset"
// migration warning that maybeWarnCredentialsUnreadable would otherwise
// print to stderr during tests.
var errE2ENoTokens = errors.New("e2e memory store: no tokens")

// resolveE2ETokenStore returns an in-memory TokenStore seeded from the
// BOSS_AUTH_E2E_EMAIL env var when set. This lets the tuitest harness fake
// a logged-in user in the boss subprocess without touching the OS keychain.
// Only compiled into builds tagged `e2e` — the production variant in
// authstore_prod.go always returns nil.
func resolveE2ETokenStore() auth.TokenStore {
	email := os.Getenv("BOSS_AUTH_E2E_EMAIL")
	if email == "" {
		return nil
	}
	return &memoryTokenStore{
		tokens: &auth.Tokens{
			AccessToken:  "e2e-access-token",
			RefreshToken: "e2e-refresh-token",
			Email:        email,
			ExpiresAt:    time.Now().Add(1 * time.Hour),
		},
	}
}

// memoryTokenStore is a minimal in-process TokenStore. It satisfies the
// auth.TokenStore interface and is only reachable under the e2e build tag.
type memoryTokenStore struct {
	mu     sync.Mutex
	tokens *auth.Tokens
}

func (m *memoryTokenStore) Save(tokens *auth.Tokens) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens = tokens
	return nil
}

func (m *memoryTokenStore) Load() (*auth.Tokens, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tokens == nil {
		return nil, errE2ENoTokens
	}
	return m.tokens, nil
}

func (m *memoryTokenStore) Delete() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens = nil
	return nil
}
