//go:build e2e

package main

import (
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/recurser/boss/internal/auth"
)

// errE2ENoTokens is returned from the e2e memory store when no tokens are
// present. Using a distinct error avoids the "run 'boss login' to reset"
// migration warning that maybeWarnCredentialsUnreadable would otherwise
// print to stderr during tests.
var errE2ENoTokens = errors.New("e2e memory store: no tokens")

// e2eReloginEmail is the identity a re-login seed retains when no
// BOSS_AUTH_E2E_EMAIL was supplied. A flagged record keeps its email (that is
// the point of BOS-659), so the seed must never produce an empty one.
const e2eReloginEmail = "relogin@example.com"

// resolveE2ETokenStore returns an in-memory TokenStore. When
// BOSS_AUTH_E2E_EMAIL is set, the store is pre-seeded so the boss
// subprocess behaves as if that user is already logged in; otherwise an
// empty store is returned and the CLI behaves as "not logged in". Either
// way, e2e-tagged builds never reach NewKeychainStore — which would pop
// the macOS "allow access to Bossanova keychain" prompt on every test
// run. The production variant in authstore_prod.go always returns nil
// so the CLI uses the real OS keychain as intended.
//
// BOSS_AUTH_E2E_NEEDS_RELOGIN (BOS-659) additionally flags the seeded record
// as retained-but-unusable, so a harness or proof scenario can stage the
// re-login-required state the daemon writes after an ambiguous WorkOS refresh.
// It only ever sets the two non-secret marker fields on the obviously-fake
// tokens above; it never seeds real credentials, and this file compiles only
// under the e2e build tag.
func resolveE2ETokenStore() auth.TokenStore {
	email := os.Getenv("BOSS_AUTH_E2E_EMAIL")
	reloginReason := resolveE2EReloginReason()
	if email == "" {
		if reloginReason == "" {
			return &memoryTokenStore{}
		}
		email = e2eReloginEmail
	}
	tokens := e2eTokensForEmail(email)
	if reloginReason != "" {
		tokens.NeedsRelogin = true
		tokens.ReloginReason = reloginReason
	}
	return &memoryTokenStore{tokens: tokens}
}

// resolveE2EReloginReason maps BOSS_AUTH_E2E_NEEDS_RELOGIN to an enumerated
// auth.ReloginReason*. Unset, empty, or an explicit falsey value means no
// marker — an operator who exports the var as "0" or "false" plainly means
// off, and silently treating that as on is the kind of surprise an e2e seed
// must not have. The exact reason name selects that reason; any other
// non-empty value means the unknown-outcome reason, which is the conservative
// default and the one the BOS-659 proof scenario stages.
func resolveE2EReloginReason() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("BOSS_AUTH_E2E_NEEDS_RELOGIN"))) {
	case "", "0", "false", "no", "off":
		return ""
	case auth.ReloginReasonRefreshTokenRejected:
		return auth.ReloginReasonRefreshTokenRejected
	default:
		return auth.ReloginReasonRefreshOutcomeUnknown
	}
}

func resolveE2ELoginEmail() string {
	return os.Getenv("BOSS_AUTH_E2E_LOGIN_EMAIL")
}

func e2eTokensForEmail(email string) *auth.Tokens {
	return &auth.Tokens{
		AccessToken:  "e2e-access-token",
		RefreshToken: "e2e-refresh-token",
		Email:        email,
		ExpiresAt:    time.Now().Add(1 * time.Hour),
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
