// Package auth provides WorkOS device code authentication for the boss CLI.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/99designs/keyring"
	"github.com/recurser/bossalib/keyringutil"
)

// ErrCredentialsUnreadable wraps any Load() failure that isn't "no tokens
// stored" — typically a decryption mismatch after the keyring passphrase
// was upgraded from the old static value. Callers can check this with
// errors.Is and surface a re-login hint to the user.
var ErrCredentialsUnreadable = errors.New("stored credentials can't be decrypted — run 'boss login' to reset")

const (
	serviceName = "bossanova"
	tokenKey    = "workos-tokens"
)

// Tokens holds the OAuth2 token set persisted in the keychain.
type Tokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Email        string    `json:"email,omitempty"`
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

// NewKeychainStore opens a keyring backed by the OS credential store. The
// allowInsecure flag is plumbed through to the file-backend password helper:
// when true, the legacy hardcoded passphrase is used if a per-install random
// passphrase can't be materialized (broken XDG_RUNTIME_DIR + no writable home).
func NewKeychainStore(allowInsecure bool) (*KeychainStore, error) {
	ring, err := keyring.Open(keyring.Config{
		ServiceName: serviceName,
		// macOS: use the system keychain.
		KeychainTrustApplication: true,
		// Linux: try secret-service, fall back to file-based with a
		// per-install random passphrase supplied by keyringutil.
		FileDir:          "~/.config/bossanova/keyring",
		FilePasswordFunc: keyring.PromptFunc(keyringutil.New(allowInsecure)),
		// Optional override via BOSS_KEYRING_BACKEND (e.g. "file" on macOS
		// for local dev to skip the system Keychain prompt). Nil means
		// platform default.
		AllowedBackends: keyringutil.Backends(),
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
		Key:         tokenKey,
		Data:        data,
		Label:       "Bossanova",
		Description: "WorkOS authentication tokens",
	})
}

// Load reads tokens from the keychain.
//
// When the item is missing, returns keyring.ErrKeyNotFound unwrapped so
// callers can distinguish "no tokens" from "can't decrypt". Any other Get
// failure (typically a passphrase mismatch after the keyringutil rollout)
// is wrapped with ErrCredentialsUnreadable.
func (s *KeychainStore) Load() (*Tokens, error) {
	item, err := s.ring.Get(tokenKey)
	if err != nil {
		if errors.Is(err, keyring.ErrKeyNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %w", ErrCredentialsUnreadable, err)
	}
	var tokens Tokens
	if err := json.Unmarshal(item.Data, &tokens); err != nil {
		return nil, fmt.Errorf("%w: unmarshal tokens: %w", ErrCredentialsUnreadable, err)
	}
	return &tokens, nil
}

// Delete removes tokens from the keychain.
func (s *KeychainStore) Delete() error {
	return s.ring.Remove(tokenKey)
}
