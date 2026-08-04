// Package accountcred is the keyring-backed at-rest store for per-account
// credential blobs used by account rotation (Claude setup-token strings; Codex
// auth.json blobs). It is deliberately split into its own package — like
// services/bossd/internal/proofenvkeyring — so that ONLY the daemon binary
// (cmd/bossd) links github.com/99designs/keyring and its transitive
// github.com/godbus/dbus dependency. Packages that hold account METADATA (e.g.
// internal/db) never import this package, so their test binaries never link
// dbus and cannot leak the SecretService goroutine goleak flags on Linux.
//
// Blobs are opaque []byte here — this package does not parse Claude vs Codex
// payloads (that is credential materialization, a later ticket). SQLite never
// stores a secret (decision D3); credential bytes flow ONLY through this store.
// Blob contents are never logged and never appear in returned error strings.
package accountcred

import (
	"errors"
	"fmt"
	"sync"

	"github.com/99designs/keyring"

	"github.com/recurser/bossalib/keyringutil"
)

// ErrCredentialNotFound is returned when no credential blob is stored for an
// account (Load) or when deleting a credential that does not exist (Delete), so
// callers can distinguish a missing entry from a keyring failure.
var ErrCredentialNotFound = errors.New("account credential not found")

// credKeyring is the minimal read/write/delete surface of a keyring the store
// needs. keyring.Keyring satisfies it; unit tests inject a fake with no real
// keyring.
type credKeyring interface {
	Get(key string) (keyring.Item, error)
	Set(item keyring.Item) error
	Remove(key string) error
}

// Store is the keyring-backed at-rest store for per-account credential blobs.
// The keyring accessor is a func field so tests inject a fake store without a
// real keyring; the default opens the shared "bossanova" keyring exactly as
// proofenvkeyring does.
type Store struct {
	open        func() (credKeyring, error)
	locks       credentialLocks
	generations credentialGenerations
}

// credentialLocks lazily creates one mutex per account. Account credentials
// are touched infrequently and account IDs are durable, so retaining the mutex
// for an account's lifetime avoids a delete/recreate race in lock cleanup.
type credentialLocks struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// credentialGenerations records successful credential mutations. Consumers can
// use the value as an optimistic guard around long-lived reads: a result based
// on a credential whose generation changed must not update account metadata.
type credentialGenerations struct {
	mu          sync.Mutex
	generations map[string]uint64
}

func (g *credentialGenerations) current(accountID string) uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.generations[accountID]
}

func (g *credentialGenerations) increment(accountID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.generations == nil {
		g.generations = make(map[string]uint64)
	}
	g.generations[accountID]++
}

func (l *credentialLocks) lock(accountID string) func() {
	l.mu.Lock()
	if l.locks == nil {
		l.locks = make(map[string]*sync.Mutex)
	}
	accountLock := l.locks[accountID]
	if accountLock == nil {
		accountLock = &sync.Mutex{}
		l.locks[accountID] = accountLock
	}
	l.mu.Unlock()

	accountLock.Lock()
	return accountLock.Unlock
}

// New returns a Store backed by the real shared "bossanova" keyring.
func New() *Store {
	return &Store{open: openSharedKeyring}
}

// WithCredentialLock serializes a compound operation on one account's
// credential. The account server uses it for explicit refreshes, while the
// materializer uses it around load-merge-save reconciliation and usage probes;
// sharing this lock prevents writers or stale probe results from overwriting a
// refreshed credential or its healthy state.
func (s *Store) WithCredentialLock(accountID string, fn func() error) error {
	unlock := s.locks.lock(accountID)
	defer unlock()
	return fn()
}

// CredentialGeneration returns the current successful-mutation generation for
// an account credential. It is intended to be read while WithCredentialLock is
// held when callers need an atomic check before committing probe results.
func (s *Store) CredentialGeneration(accountID string) uint64 {
	return s.generations.current(accountID)
}

// itemKey is the stable keyring item key a credential blob is stored under. The
// fixed prefix parallels proofenvkeyring's fixed keys.
func itemKey(accountID string) string {
	return "account-credential-" + accountID
}

// openSharedKeyring opens the shared bossanova keyring with the same config as
// services/bossd/internal/proofenvkeyring's openSharedKeyring. allowInsecure is
// hard-wired false: bossd is a daemon with no flag plumbing, so a broken keyring
// should surface a real error rather than silently reverting to a hardcoded
// passphrase.
func openSharedKeyring() (credKeyring, error) {
	return keyring.Open(keyring.Config{
		ServiceName:              "bossanova",
		KeychainTrustApplication: true,
		FileDir:                  "~/.config/bossanova/keyring",
		FilePasswordFunc:         keyring.PromptFunc(keyringutil.New(false)),
		AllowedBackends:          keyringutil.Backends(),
	})
}

// Save stores blob as the credential for accountID, overwriting any existing
// entry. The blob is opaque and never appears in a returned error string.
func (s *Store) Save(accountID string, blob []byte) error {
	ring, err := s.open()
	if err != nil {
		return fmt.Errorf("open keyring: %w", err)
	}
	if err := ring.Set(keyring.Item{
		Key: itemKey(accountID),
		// Defensive copy: never retain the caller's slice, so a caller that
		// reuses/mutates its buffer after Save cannot corrupt stored credential
		// bytes under a backend that stores Data by reference.
		Data:        append([]byte(nil), blob...),
		Label:       "bossanova",
		Description: "account credential",
	}); err != nil {
		return fmt.Errorf("save account credential: %w", err)
	}
	s.generations.increment(accountID)
	return nil
}

// Load returns the credential blob for accountID. It returns
// ErrCredentialNotFound when no entry exists. The blob never appears in a
// returned error string.
func (s *Store) Load(accountID string) ([]byte, error) {
	ring, err := s.open()
	if err != nil {
		return nil, fmt.Errorf("open keyring: %w", err)
	}
	item, err := ring.Get(itemKey(accountID))
	if err != nil {
		if errors.Is(err, keyring.ErrKeyNotFound) {
			return nil, ErrCredentialNotFound
		}
		return nil, fmt.Errorf("load account credential: %w", err)
	}
	// Defensive copy: return a caller-owned slice so mutating it can never reach
	// back into a backend that hands out its stored buffer by reference.
	return append([]byte(nil), item.Data...), nil
}

// Delete removes the credential for accountID. It returns ErrCredentialNotFound
// when no entry exists so callers can distinguish a missing entry from a
// keyring failure.
func (s *Store) Delete(accountID string) error {
	unlock := s.locks.lock(accountID)
	defer unlock()

	ring, err := s.open()
	if err != nil {
		return fmt.Errorf("open keyring: %w", err)
	}
	if err := ring.Remove(itemKey(accountID)); err != nil {
		if errors.Is(err, keyring.ErrKeyNotFound) {
			return ErrCredentialNotFound
		}
		return fmt.Errorf("delete account credential: %w", err)
	}
	s.generations.increment(accountID)
	return nil
}
