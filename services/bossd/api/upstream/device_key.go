package upstream

import (
	"crypto/ecdh"
	"fmt"

	"github.com/99designs/keyring"
	"github.com/recurser/bossd/internal/devicekey"
)

// DeviceKeyring adapts the shared OS keyring to the device key consumer interface.
type DeviceKeyring struct{ Ring keyring.Keyring }

// Get reads a private key item and translates the keyring's missing-item sentinel.
func (r DeviceKeyring) Get(id string) ([]byte, error) {
	item, err := r.Ring.Get(id)
	if tokenKeyMissing(err) {
		return nil, devicekey.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), item.Data...), nil
}

// Set persists a device private key in the shared keyring.
func (r DeviceKeyring) Set(id string, data []byte) error {
	return r.Ring.Set(keyring.Item{Key: id, Data: append([]byte(nil), data...), Label: "Bossanova", Description: "Bossanova agent credential device key"})
}

// LoadDeviceKey opens the shared keyring and loads or creates a daemon's X25519 key.
func LoadDeviceKey(daemonID string) (*ecdh.PrivateKey, error) {
	ring, err := openKeyring()
	if err != nil {
		return nil, fmt.Errorf("device key: open keyring: %w", err)
	}
	return devicekey.LoadOrCreate(DeviceKeyring{Ring: ring}, daemonID)
}

// RotateDeviceKey replaces a revoked daemon's X25519 key in the shared keyring.
func RotateDeviceKey(daemonID string) (*ecdh.PrivateKey, error) {
	ring, err := openKeyring()
	if err != nil {
		return nil, fmt.Errorf("device key: open keyring: %w", err)
	}
	return devicekey.Rotate(DeviceKeyring{Ring: ring}, daemonID)
}
