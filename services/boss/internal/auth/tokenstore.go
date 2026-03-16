// Package auth provides OIDC PKCE authentication for the boss CLI.
package auth

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/99designs/keyring"
)

const (
	serviceName = "bossanova"
	tokenKey    = "oauth-tokens"
)

// Tokens holds the OAuth2 token set persisted in the keychain.
type Tokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	IDToken      string    `json:"id_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// Valid returns true if the access token is present and not expired.
func (t *Tokens) Valid() bool {
	return t.AccessToken != "" && time.Now().Before(t.ExpiresAt)
}

// TokenStore abstracts token persistence for testing.
type TokenStore interface {
	Save(tokens *Tokens) error
	Load() (*Tokens, error)
	Delete() error
}

// KeychainStore persists tokens in the OS keychain (macOS Keychain,
// Linux secret-service, Windows credential manager).
type KeychainStore struct {
	ring keyring.Keyring
}

// NewKeychainStore opens a keyring backed by the OS credential store.
func NewKeychainStore() (*KeychainStore, error) {
	ring, err := keyring.Open(keyring.Config{
		ServiceName: serviceName,
		// macOS: use the system keychain.
		KeychainTrustApplication: true,
		// Linux: try secret-service, fall back to file-based.
		FileDir:          "~/.config/bossanova/keyring",
		FilePasswordFunc: keyring.FixedStringPrompt("bossanova"),
	})
	if err != nil {
		return nil, fmt.Errorf("open keyring: %w", err)
	}
	return &KeychainStore{ring: ring}, nil
}

// Save serializes tokens to JSON and stores them in the keychain.
func (s *KeychainStore) Save(tokens *Tokens) error {
	data, err := json.Marshal(tokens)
	if err != nil {
		return fmt.Errorf("marshal tokens: %w", err)
	}
	return s.ring.Set(keyring.Item{
		Key:  tokenKey,
		Data: data,
	})
}

// Load reads tokens from the keychain.
func (s *KeychainStore) Load() (*Tokens, error) {
	item, err := s.ring.Get(tokenKey)
	if err != nil {
		return nil, fmt.Errorf("get token: %w", err)
	}
	var tokens Tokens
	if err := json.Unmarshal(item.Data, &tokens); err != nil {
		return nil, fmt.Errorf("unmarshal tokens: %w", err)
	}
	return &tokens, nil
}

// Delete removes tokens from the keychain.
func (s *KeychainStore) Delete() error {
	return s.ring.Remove(tokenKey)
}
