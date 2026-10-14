package upstream

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/99designs/keyring"
)

// daemonIdentityKeyPrefix namespaces the per-machine key inside the shared
// bossanova keyring; the daemon id follows it.
const daemonIdentityKeyPrefix = "daemon-identity-key:"

// LoadOrCreateDaemonKey returns this machine's Ed25519 identity key for
// daemonID, generating and storing it on first use (BOS-1580).
//
// bosso pins a daemon id to the public half the first time the daemon proves
// possession, so the key must stay stable for as long as the id does: it is
// stored per daemon id, and a stored item that is not a valid key is an error
// rather than something to overwrite, because replacing it would lock the
// machine out of its own pinned id for any other account.
//
// The key lives in the keyring rather than beside the daemon-id file so it
// never sits in the data directory that backups and diagnostic bundles copy.
func LoadOrCreateDaemonKey(ring keyring.Keyring, daemonID string) (ed25519.PrivateKey, error) {
	if daemonID == "" {
		return nil, errors.New("daemon identity key: daemon id is required")
	}
	itemKey := daemonIdentityKeyPrefix + daemonID
	item, err := ring.Get(itemKey)
	switch {
	case err == nil:
		if len(item.Data) != ed25519.PrivateKeySize {
			return nil, fmt.Errorf("daemon identity key: stored item is %d bytes, want %d", len(item.Data), ed25519.PrivateKeySize)
		}
		return ed25519.PrivateKey(append([]byte(nil), item.Data...)), nil
	case !tokenKeyMissing(err):
		return nil, fmt.Errorf("daemon identity key: read keyring: %w", err)
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("daemon identity key: generate: %w", err)
	}
	if err := ring.Set(keyring.Item{
		Key:         itemKey,
		Data:        priv,
		Label:       "Bossanova",
		Description: "Bossanova daemon machine identity key",
	}); err != nil {
		return nil, fmt.Errorf("daemon identity key: write keyring: %w", err)
	}
	return priv, nil
}

// LoadDaemonIdentityKey opens the shared bossanova keyring and returns the
// identity key for daemonID, creating it on first use. A failure means the
// daemon registers without a possession proof.
func LoadDaemonIdentityKey(daemonID string) (ed25519.PrivateKey, error) {
	ring, err := openKeyring()
	if err != nil {
		return nil, fmt.Errorf("daemon identity key: open keyring: %w", err)
	}
	return LoadOrCreateDaemonKey(ring, daemonID)
}
