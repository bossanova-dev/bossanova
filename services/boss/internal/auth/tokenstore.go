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

// ErrReloginRequired is returned when the stored credentials are retained but
// flagged unusable: the daemon's last WorkOS refresh either was rejected or
// never confirmed its outcome, so the one-shot refresh token must not be
// replayed. The entry (and the email in it) is kept; only `boss login` clears
// the flag. See BOS-659.
var ErrReloginRequired = errors.New("stored credentials need a new sign-in — run 'boss login'")

const (
	serviceName = "bossanova"
	tokenKey    = "workos-tokens"
)

// Enumerated, non-secret re-login reasons persisted on the shared
// "workos-tokens" keychain record. bossd writes them
// (services/bossd/internal/upstream tokens.go reloginReason*); the CLI reads
// them. Keep the two lists in step — the record is one shared JSON document.
const (
	// ReloginReasonRefreshOutcomeUnknown means a refresh request was
	// dispatched but its outcome was never confirmed, so WorkOS may already
	// have consumed and rotated the refresh token.
	ReloginReasonRefreshOutcomeUnknown = "refresh_outcome_unknown"
	// ReloginReasonRefreshTokenRejected means WorkOS authoritatively rejected
	// the refresh token.
	ReloginReasonRefreshTokenRejected = "refresh_token_rejected"
)

// Tokens holds the OAuth2 token set persisted in the keychain.
// NeedsRelogin/ReloginReason are the BOS-659 re-login marker: both are
// omitempty so a record written here still round-trips through an older bossd,
// and a record with no marker keeps behaving exactly as it always has.
type Tokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Email        string    `json:"email,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	// NeedsRelogin marks credentials retained for their identity (email) that
	// must not be used for another WorkOS exchange.
	NeedsRelogin bool `json:"needs_relogin,omitempty"`
	// ReloginReason is one of the ReloginReason* values above. It never
	// carries token material or raw upstream payloads.
	ReloginReason string `json:"relogin_reason,omitempty"`
}

// Valid returns true if the access token is present, not expired, and not
// flagged for re-login.
func (t *Tokens) Valid() bool {
	return t.AccessToken != "" && !t.NeedsRelogin && time.Now().Before(t.ExpiresAt)
}

// ReloginReasonOrEmpty normalizes the persisted marker: a record flagged
// without a recognized reason is reported as an authoritative rejection, the
// conservative reading that never invites a retry.
func (t *Tokens) ReloginReasonOrEmpty() string {
	if t == nil || !t.NeedsRelogin {
		return ""
	}
	if t.ReloginReason == ReloginReasonRefreshOutcomeUnknown {
		return ReloginReasonRefreshOutcomeUnknown
	}
	return ReloginReasonRefreshTokenRejected
}

// clearReloginMarker drops any re-login flag before persisting. Every
// successful login or refresh writes a clean record.
func (t *Tokens) clearReloginMarker() {
	if t == nil {
		return
	}
	t.NeedsRelogin = false
	t.ReloginReason = ""
}

// ReloginRequiredError builds the user-facing error for a retained,
// re-login-flagged record. The text is safe to log and display: it names the
// reason and the remedy, never the credentials.
func ReloginRequiredError(reason string) error {
	return fmt.Errorf("%w: %s", ErrReloginRequired, ReloginReasonDescription(reason))
}

// ReloginReasonDescription renders an enumerated reason as a short,
// non-secret explanation suitable for CLI and TUI output.
func ReloginReasonDescription(reason string) string {
	if reason == ReloginReasonRefreshOutcomeUnknown {
		return "the last refresh outcome could not be confirmed"
	}
	return "the stored refresh token was rejected"
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
