package upstream

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/recurser/bossd/internal/credsync"
	"github.com/recurser/bossd/internal/devicekey"
)

// One keyring write records both the accepted seal and the generation high-water
// mark, so a crash cannot leave a new accepted seal with an old rollback floor.
type credentialSealRecord struct {
	Highest int64                 `json:"highest"`
	Seals   map[int64][]byte      `json:"seals"`
	Pending *credsync.KeyRotation `json:"pending,omitempty"`
}

var credentialSealCacheMu sync.Mutex

var _ credsync.CredentialSealCache = CredentialTrust{}

func (t CredentialTrust) sealRecord() (DeviceKeyring, string, credentialSealRecord, error) {
	if t.Namespace == "" {
		return DeviceKeyring{}, "", credentialSealRecord{}, errors.New("credential seal cache namespace is required")
	}
	digest := sha256.Sum256([]byte(t.Namespace))
	id := "credential-seal-cache-" + hex.EncodeToString(digest[:])
	ring, err := openKeyring()
	if err != nil {
		return DeviceKeyring{}, "", credentialSealRecord{}, err
	}
	store := DeviceKeyring{Ring: ring}
	raw, err := store.Get(id)
	if errors.Is(err, devicekey.ErrNotFound) {
		return store, id, credentialSealRecord{Seals: map[int64][]byte{}}, nil
	}
	if err != nil {
		return store, id, credentialSealRecord{}, err
	}
	var record credentialSealRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return store, id, record, errors.New("invalid credential seal cache")
	}
	if record.Highest <= 0 || len(record.Seals[record.Highest]) == 0 {
		return store, id, record, errors.New("incomplete credential seal cache")
	}
	return store, id, record, nil
}

// AcceptedKeySeal loads the exact signed box previously verified for a generation.
func (t CredentialTrust) AcceptedKeySeal(generation int64) ([]byte, error) {
	credentialSealCacheMu.Lock()
	defer credentialSealCacheMu.Unlock()
	_, _, record, err := t.sealRecord()
	if err != nil {
		return nil, err
	}
	seal, ok := record.Seals[generation]
	if !ok {
		return nil, credsync.ErrNotFound
	}
	return append([]byte(nil), seal...), nil
}

// HighestKeyGeneration keeps the rollback floor across daemon restarts.
func (t CredentialTrust) HighestKeyGeneration() (int64, error) {
	credentialSealCacheMu.Lock()
	defer credentialSealCacheMu.Unlock()
	_, _, record, err := t.sealRecord()
	if err != nil {
		return 0, err
	}
	return record.Highest, nil
}

// SaveAcceptedKeySeal pins a verified box and generation in a single keyring item.
// The engine authenticates replacements from enrolled pinned signers before
// calling this method; revoked signers require the previously cached exact box.
func (t CredentialTrust) SaveAcceptedKeySeal(generation int64, seal []byte) error {
	if generation <= 0 || len(seal) == 0 {
		return errors.New("invalid accepted credential key seal")
	}
	credentialSealCacheMu.Lock()
	defer credentialSealCacheMu.Unlock()
	store, id, record, err := t.sealRecord()
	if err != nil {
		return err
	}
	if generation < record.Highest {
		return credsync.ErrRollback
	}
	record.Highest = generation
	record.Seals[generation] = append([]byte(nil), seal...)
	raw, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode accepted credential seal: %w", err)
	}
	return store.Set(id, raw)
}

// PendingRotation restores the exact opaque proposal saved before uploading it.
// Decoding the keyring record on each read keeps returned byte slices independent.
func (t CredentialTrust) PendingRotation() (*credsync.KeyRotation, error) {
	credentialSealCacheMu.Lock()
	defer credentialSealCacheMu.Unlock()
	_, _, record, err := t.sealRecord()
	if err != nil {
		return nil, err
	}
	return record.Pending, nil
}

// SavePendingRotation atomically records the proposal, its own signed box and
// the rollback floor, so retries after a failed or response-lost RPC reuse it.
func (t CredentialTrust) SavePendingRotation(rotation credsync.KeyRotation) error {
	if rotation.ExpectedGeneration < 0 || rotation.KeyGeneration != rotation.ExpectedGeneration+1 || len(rotation.OwnSeal) == 0 {
		return errors.New("invalid pending credential rotation")
	}
	credentialSealCacheMu.Lock()
	defer credentialSealCacheMu.Unlock()
	store, id, record, err := t.sealRecord()
	if err != nil {
		return err
	}
	if rotation.KeyGeneration < record.Highest {
		return credsync.ErrRollback
	}
	record.Highest = rotation.KeyGeneration
	record.Seals[rotation.KeyGeneration] = append([]byte(nil), rotation.OwnSeal...)
	record.Pending = &rotation
	raw, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode pending credential rotation: %w", err)
	}
	return store.Set(id, raw)
}

// ClearPendingRotation acknowledges publication without lowering the rollback floor.
func (t CredentialTrust) ClearPendingRotation() error {
	credentialSealCacheMu.Lock()
	defer credentialSealCacheMu.Unlock()
	store, id, record, err := t.sealRecord()
	if err != nil {
		return err
	}
	if record.Pending == nil {
		return nil
	}
	record.Pending = nil
	raw, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode completed credential rotation: %w", err)
	}
	return store.Set(id, raw)
}
