// Package oskeyring adapts the shared "bossanova" OS keyring to
// reposecrets.Ring. It is split out of package reposecrets so that only the
// daemon binary (cmd/bossd) links github.com/99designs/keyring and its
// transitive dbus / keychain dependencies, exactly as proofenvkeyring is split
// from proofenv.
package oskeyring

import (
	"errors"
	"io/fs"

	"github.com/99designs/keyring"

	"github.com/recurser/bossalib/contract/daemon/keyringutil"

	"github.com/recurser/bossd/internal/adapter/reposecrets"
)

// Open opens the shared bossanova keyring with the same config as
// services/bossd/internal/upstream/tokens.go and proofenvkeyring.
// allowInsecure is hard-wired false: bossd is a daemon with no flag plumbing,
// so a broken keyring surfaces as an error (reposecrets reports it as
// ErrUnavailable) rather than silently reverting to a hardcoded passphrase.
func Open() (reposecrets.Ring, error) {
	ring, err := keyring.Open(keyring.Config{
		ServiceName:              "bossanova",
		KeychainTrustApplication: true,
		FileDir:                  "~/.config/bossanova/keyring",
		FilePasswordFunc:         keyring.PromptFunc(keyringutil.New(false)),
		AllowedBackends:          keyringutil.Backends(),
	})
	if err != nil {
		return nil, err
	}
	return adapter{ring: ring}, nil
}

type adapter struct {
	ring keyring.Keyring
}

func (a adapter) Get(key string) ([]byte, error) {
	item, err := a.ring.Get(key)
	if errors.Is(err, keyring.ErrKeyNotFound) {
		return nil, reposecrets.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return item.Data, nil
}

func (a adapter) Set(key string, data []byte) error {
	return a.ring.Set(keyring.Item{
		Key: key,
		// Defensive copy so a backend that stores Data by reference never
		// aliases the caller's buffer.
		Data:        append([]byte(nil), data...),
		Label:       "bossanova",
		Description: "repo API keys",
	})
}

// Remove maps an absent item to reposecrets.ErrNotFound. The file backend
// (headless Linux's default, or BOSS_KEYRING_BACKEND=file) removes the item's
// file with os.Remove and returns its fs.ErrNotExist unwrapped rather than
// keyring.ErrKeyNotFound, so both mean "no such item".
func (a adapter) Remove(key string) error {
	if err := a.ring.Remove(key); err != nil {
		if errors.Is(err, keyring.ErrKeyNotFound) || errors.Is(err, fs.ErrNotExist) {
			return reposecrets.ErrNotFound
		}
		return err
	}
	return nil
}
