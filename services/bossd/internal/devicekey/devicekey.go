// Package devicekey manages daemon encryption keys and authenticated enrollment statements.
package devicekey

import (
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"fmt"
)

const keyPrefix = "agent-credential-device-key:"

// ErrNotFound identifies a keyring item that has not been created.
var ErrNotFound = errors.New("device key not found")

// Keyring supplies private key storage without exposing a keyring implementation.
type Keyring interface {
	Get(string) ([]byte, error)
	Set(string, []byte) error
}

// LoadOrCreate returns the daemon's persistent X25519 key, creating it only if absent.
func LoadOrCreate(ring Keyring, daemonID string) (*ecdh.PrivateKey, error) {
	if daemonID == "" {
		return nil, errors.New("device key: daemon id is required")
	}
	key, err := load(ring, daemonID)
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	return generate(ring, daemonID)
}

// Rotate replaces a valid key after the daemon learns it has been revoked.
// A malformed stored item is preserved for diagnosis rather than overwritten.
func Rotate(ring Keyring, daemonID string) (*ecdh.PrivateKey, error) {
	if daemonID == "" {
		return nil, errors.New("device key: daemon id is required")
	}
	if _, err := load(ring, daemonID); err != nil {
		return nil, err
	}
	return generate(ring, daemonID)
}

func load(ring Keyring, daemonID string) (*ecdh.PrivateKey, error) {
	data, err := ring.Get(keyPrefix + daemonID)
	if err != nil {
		return nil, fmt.Errorf("device key: read keyring: %w", err)
	}
	key, err := ecdh.X25519().NewPrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("device key: invalid stored item: %w", err)
	}
	return key, nil
}
func generate(ring Keyring, daemonID string) (*ecdh.PrivateKey, error) {
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("device key: generate: %w", err)
	}
	if err := ring.Set(keyPrefix+daemonID, key.Bytes()); err != nil {
		return nil, fmt.Errorf("device key: write keyring: %w", err)
	}
	return key, nil
}
