package upstream

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"

	"github.com/recurser/bossd/internal/credsync"
)

// PinCredentialIdentity persists an identity confirmed on a device's local display.
func PinCredentialIdentity(id string, key ed25519.PublicKey) error {
	if len(key) != ed25519.PublicKeySize {
		return fmt.Errorf("invalid credential identity key")
	}
	ring, err := openKeyring()
	if err != nil {
		return err
	}
	return (DeviceKeyring{Ring: ring}).Set("credential-identity-pin-"+id, key)
}

// CredentialTrust uses only locally confirmed identity pins, never remote TOFU.
type CredentialTrust struct{ Namespace string }

// PinnedIdentity reads the locally approved signing key for a daemon.
func (CredentialTrust) PinnedIdentity(id string) (ed25519.PublicKey, error) {
	ring, err := openKeyring()
	if err != nil {
		return nil, err
	}
	key, err := (DeviceKeyring{Ring: ring}).Get("credential-identity-pin-" + id)
	if err != nil {
		return nil, err
	}
	if len(key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid credential identity pin")
	}
	return ed25519.PublicKey(key), nil
}

// ApprovedDevice loads historical enrollment evidence verified on this daemon.
func (CredentialTrust) ApprovedDevice(id string) (credsync.Device, error) {
	ring, err := openKeyring()
	if err != nil {
		return credsync.Device{}, err
	}
	raw, err := (DeviceKeyring{Ring: ring}).Get("credential-approval-pin-" + id)
	if err != nil {
		return credsync.Device{}, err
	}
	var d credsync.Device
	if err := json.Unmarshal(raw, &d); err != nil {
		return credsync.Device{}, err
	}
	if d.DaemonID != id {
		return credsync.Device{}, fmt.Errorf("credential approval identity mismatch")
	}
	return d, nil
}

// SaveApprovedDevice persists verified public enrollment evidence for revocation continuity.
func (CredentialTrust) SaveApprovedDevice(d credsync.Device) error {
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	ring, err := openKeyring()
	if err != nil {
		return err
	}
	return (DeviceKeyring{Ring: ring}).Set("credential-approval-pin-"+d.DaemonID, raw)
}
