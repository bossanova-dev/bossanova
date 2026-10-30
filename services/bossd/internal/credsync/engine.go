package credsync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// Engine keeps plaintext and credential keys entirely within the daemon.
// A mutex serializes receive/publish/roll so local received-version checks cannot race.
type Engine struct {
	config              Config
	remote              Remote
	local               Local
	trust               Trust
	mu                  sync.Mutex
	key                 []byte
	generation          int64
	observedRevocations map[string]bool
	admitted            map[string]Device
}

// NewEngine constructs a daemon-local distribution engine; no key reaches Remote.
func NewEngine(config Config, remote Remote, local Local, trust Trust) *Engine {
	return &Engine{config: config, remote: remote, local: local, trust: trust, observedRevocations: map[string]bool{}, admitted: map[string]Device{}}
}

// EnsureCredentialKey opens the daemon's seal and supplies missing approved recipients.
// It also rotates on newly observed revocations, including reconnect discovery.
func (e *Engine) EnsureCredentialKey(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ensure(ctx, true)
}
func (e *Engine) ensure(ctx context.Context, checkRevocations bool) error {
	if err := e.resumePendingRotation(ctx); err != nil {
		return err
	}
	devices, err := e.remote.ListDevices(ctx)
	if err != nil {
		return err
	}
	verified, err := e.verifiedDevices(devices)
	if err != nil {
		return err
	}
	gen, sealed, err := e.remote.GetKeySeal(ctx)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if err = e.loadOrCreateKey(ctx, gen, sealed, verified, devices); err != nil {
		return err
	}
	if checkRevocations {
		for _, d := range devices {
			if d.Status == "revoked" && !e.observedRevocations[d.DaemonID] {
				if err = e.roll(ctx, d.DaemonID, verified); err != nil {
					return err
				}
				e.observedRevocations[d.DaemonID] = true
			}
		}
	}
	return nil
}
func (e *Engine) loadOrCreateKey(ctx context.Context, gen int64, sealed []byte, verified, devices []Device) error {
	expected := gen
	var key []byte
	var err error
	if gen == 0 {
		if e.config.DaemonID != e.config.BootstrapDaemonID {
			return errors.New("credential sync: only bootstrap device may create first generation")
		}
		key, err = NewCredentialKey()
		gen = 1
	} else {
		if gen < e.generation {
			return ErrRollback
		}
		key, err = e.openSignedKeySeal(gen, sealed, devices)
	}
	if err != nil {
		return fmt.Errorf("credential sync: open generation %d: %w", gen, err)
	}
	seals, err := e.seals(key, gen, verified)
	if err != nil {
		return err
	}
	if expected == 0 {
		if err = e.rememberOwnSeal(gen, seals); err != nil {
			return err
		}
	}
	if err = e.remote.PutKeySeals(ctx, expected, gen, seals); err != nil {
		return err
	}
	e.key = key
	e.generation = gen
	return nil
}

func (e *Engine) seals(key []byte, generation int64, devices []Device) ([]KeySeal, error) {
	seals := make([]KeySeal, 0, len(devices))
	for _, d := range devices {
		box, err := (DeviceRecipient{DaemonID: d.DaemonID, PublicKey: d.DevicePublicKey}).SealCredentialKey(key, RecipientContext{UserID: e.config.UserID, KeyGeneration: generation})
		if err != nil {
			return nil, err
		}
		encoded, err := e.signKeySeal(d, generation, box)
		if err != nil {
			return nil, err
		}
		seals = append(seals, KeySeal{DaemonID: d.DaemonID, SealedCredentialKey: encoded})
	}
	return seals, nil
}
func (e *Engine) aad(blob Blob) CredentialAAD {
	return CredentialAAD{UserID: e.config.UserID, SyncedAgentAccountID: blob.SyncedAgentAccountID, CredentialVersion: blob.CredentialVersion, KeyGeneration: blob.KeyGeneration}
}

// Publish encrypts a local credential with optimistic versioning. A winning peer
// is received rather than overwritten when the compare-and-set loses.
func (e *Engine) Publish(ctx context.Context, syncedID, localID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.ensure(ctx, true); err != nil {
		return err
	}
	current, err := e.remote.GetBlob(ctx, syncedID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if err == nil && current.SyncedAgentAccountID != syncedID {
		return errors.New("credential sync: mismatched account blob")
	}
	local, localErr := e.local.Lookup(ctx, syncedID)
	if localErr != nil && !errors.Is(localErr, ErrNotFound) {
		return localErr
	}
	if current.CredentialVersion < local.SyncedCredentialVersion {
		return ErrRollback
	}
	plain, err := e.local.Read(ctx, localID)
	if err != nil {
		return err
	}
	blob := Blob{SyncedAgentAccountID: syncedID, CredentialVersion: local.SyncedCredentialVersion + 1, KeyGeneration: e.generation}
	blob.Ciphertext, err = EncryptCredential(e.key, plain, e.aad(blob))
	if err != nil {
		return err
	}
	if err = e.remote.PutBlob(ctx, local.SyncedCredentialVersion, blob); errors.Is(err, ErrConflict) {
		return e.receive(ctx, syncedID)
	} else if err != nil {
		return err
	}
	return e.local.SetVersion(ctx, localID, blob.CredentialVersion)
}

// Receive installs missing, invalid, or older distributed copies while preserving
// independently added working credentials. A replay below the local version fails.
func (e *Engine) Receive(ctx context.Context, syncedID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.ensure(ctx, true); err != nil {
		return err
	}
	return e.receive(ctx, syncedID)
}
func (e *Engine) receive(ctx context.Context, syncedID string) error {
	blob, err := e.remote.GetBlob(ctx, syncedID)
	if err != nil {
		return err
	}
	if blob.SyncedAgentAccountID != syncedID {
		return errors.New("credential sync: mismatched account blob")
	}
	if blob.KeyGeneration != e.generation {
		return ErrRollback
	}
	local, err := e.local.Lookup(ctx, syncedID)
	missing := errors.Is(err, ErrNotFound)
	if err != nil && !missing {
		return err
	}
	if blob.CredentialVersion < local.SyncedCredentialVersion {
		return ErrRollback
	}
	if !missing && !local.IsAuthInvalid && (local.SyncedCredentialVersion == 0 || local.SyncedCredentialVersion == blob.CredentialVersion) {
		return nil
	}
	plain, err := DecryptCredential(e.key, blob.Ciphertext, e.aad(blob))
	if err != nil {
		return err
	}
	metadata, err := e.metadata(ctx, syncedID)
	if err != nil {
		return err
	}
	return e.local.Install(ctx, metadata, plain, blob.CredentialVersion)
}
func (e *Engine) metadata(ctx context.Context, syncedID string) (AgentAccountMetadata, error) {
	rows, err := e.remote.ListAgentAccounts(ctx)
	if err != nil {
		return AgentAccountMetadata{}, err
	}
	for _, row := range rows {
		if row.SyncedAgentAccountID == syncedID {
			if !row.IsInScope {
				return AgentAccountMetadata{}, errors.New("credential sync: account is out of scope")
			}
			return row, nil
		}
	}
	return AgentAccountMetadata{}, ErrNotFound
}

// Roll replaces every encrypted blob and all survivor seals in one atomic CAS.
func (e *Engine) Roll(ctx context.Context, revokedDaemonID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.ensure(ctx, false); err != nil {
		return err
	}
	devices, err := e.remote.ListDevices(ctx)
	if err != nil {
		return err
	}
	revoked := false
	for _, d := range devices {
		if d.DaemonID == revokedDaemonID && d.Status == "revoked" {
			revoked = true
		}
	}
	if !revoked {
		return errors.New("credential sync: device is not revoked")
	}
	verified, err := e.verifiedDevices(devices)
	if err != nil {
		return err
	}
	if err = e.roll(ctx, revokedDaemonID, verified); err != nil {
		return err
	}
	e.observedRevocations[revokedDaemonID] = true
	return nil
}
func (e *Engine) roll(ctx context.Context, revokedID string, devices []Device) error {
	key, err := NewCredentialKey()
	if err != nil {
		return err
	}
	generation := e.generation + 1
	var survivors []Device
	for _, d := range devices {
		if d.DaemonID != revokedID {
			survivors = append(survivors, d)
		}
	}
	seals, err := e.seals(key, generation, survivors)
	if err != nil {
		return err
	}
	rows, err := e.remote.ListAgentAccounts(ctx)
	if err != nil {
		return err
	}
	var blobs []Blob
	for _, row := range rows {
		blob, err := e.remote.GetBlob(ctx, row.SyncedAgentAccountID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if blob.SyncedAgentAccountID != row.SyncedAgentAccountID {
			return errors.New("credential sync: mismatched account blob")
		}
		if blob.KeyGeneration != e.generation {
			return ErrRollback
		}
		plain, err := DecryptCredential(e.key, blob.Ciphertext, e.aad(blob))
		if err != nil {
			return err
		}
		blob.KeyGeneration = generation
		blob.CredentialVersion++
		blob.Ciphertext, err = EncryptCredential(key, plain, e.aad(blob))
		if err != nil {
			return err
		}
		blobs = append(blobs, blob)
	}
	proposal := KeyRotation{ExpectedGeneration: e.generation, KeyGeneration: generation, Seals: seals, Blobs: blobs}
	for _, seal := range seals {
		if seal.DaemonID == e.config.DaemonID {
			proposal.OwnSeal = seal.SealedCredentialKey
		}
	}
	if len(proposal.OwnSeal) == 0 {
		return errors.New("credential sync: own rotation seal missing")
	}
	if err = e.trust.SavePendingRotation(proposal); err != nil {
		return err
	}
	return e.resumePendingRotation(ctx)
}

// PublishRefresh publishes a known provider exchange result. Unlike Publish,
// losing a CAS never installs the remote winner over that result: a key roll
// may have advanced the version while retaining the now-spent refresh secret.
// The caller retains its durable result journal and retries after conflicts.
func (e *Engine) PublishRefresh(ctx context.Context, syncedID, localID string, plain, consumedFingerprint []byte) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.ensure(ctx, true); err != nil {
		return err
	}
	current, err := e.remote.GetBlob(ctx, syncedID)
	if err != nil {
		return err
	}
	if current.SyncedAgentAccountID != syncedID || current.KeyGeneration != e.generation {
		return ErrRollback
	}
	local, err := e.local.Lookup(ctx, syncedID)
	if err != nil {
		return err
	}
	if current.CredentialVersion < local.SyncedCredentialVersion {
		return ErrRollback
	}
	remotePlain, err := DecryptCredential(e.key, current.Ciphertext, e.aad(current))
	if err != nil {
		return err
	}
	remoteFingerprint, err := refreshFingerprint(remotePlain)
	if err != nil {
		return err
	}
	resultFingerprint, err := refreshFingerprint(plain)
	if err != nil {
		return err
	}
	if len(consumedFingerprint) != sha256.Size {
		return errors.New("credential sync: invalid consumed refresh fingerprint")
	}
	if !bytes.Equal(remoteFingerprint, consumedFingerprint) && !bytes.Equal(remoteFingerprint, resultFingerprint) {
		return ErrCredentialReplaced
	}
	blob := Blob{SyncedAgentAccountID: syncedID, CredentialVersion: current.CredentialVersion + 1, KeyGeneration: e.generation}
	blob.Ciphertext, err = EncryptCredential(e.key, plain, e.aad(blob))
	if err != nil {
		return err
	}
	if err = e.remote.PutBlob(ctx, current.CredentialVersion, blob); err != nil {
		return err
	}
	return e.local.SetVersion(ctx, localID, blob.CredentialVersion)
}

// ErrCredentialReplaced means a pending exchange belongs to an obsolete login.
// The caller must discard the result and receive the new credential outside its lock.
var ErrCredentialReplaced = errors.New("credential sync: refresh credential replaced")

// ResolveRefreshVersion identifies a distributed copy without adopting a different
// independently authenticated credential. It returns the remote CAS guard either way.
func (e *Engine) ResolveRefreshVersion(ctx context.Context, syncedID, localID string) (int64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.ensure(ctx, true); err != nil {
		return 0, err
	}
	current, err := e.remote.GetBlob(ctx, syncedID)
	if err != nil {
		return 0, err
	}
	if current.SyncedAgentAccountID != syncedID || current.KeyGeneration != e.generation {
		return 0, ErrRollback
	}
	local, err := e.local.Lookup(ctx, syncedID)
	if err != nil {
		return 0, err
	}
	if current.CredentialVersion < local.SyncedCredentialVersion {
		return 0, ErrRollback
	}
	remotePlain, err := DecryptCredential(e.key, current.Ciphertext, e.aad(current))
	if err != nil {
		return 0, err
	}
	remoteFingerprint, err := refreshFingerprint(remotePlain)
	if err != nil {
		return 0, err
	}
	localPlain, err := e.local.Read(ctx, localID)
	if err != nil {
		return 0, err
	}
	localFingerprint, err := refreshFingerprint(localPlain)
	if err != nil {
		return 0, err
	}
	if local.SyncedCredentialVersion == 0 && bytes.Equal(remoteFingerprint, localFingerprint) {
		if err := e.local.SetVersion(ctx, localID, current.CredentialVersion); err != nil {
			return 0, err
		}
	}
	return current.CredentialVersion, nil
}

func refreshFingerprint(plain []byte) ([]byte, error) {
	var top map[string]json.RawMessage
	if json.Unmarshal(plain, &top) != nil {
		return nil, errors.New("credential sync: malformed refresh credential")
	}
	var tokens, openai map[string]json.RawMessage
	_ = json.Unmarshal(top["tokens"], &tokens)
	_ = json.Unmarshal(top["openai"], &openai)
	if len(tokens) == 0 {
		tokens = openai
	}
	for _, raw := range []json.RawMessage{tokens["refresh_token"], top["refresh"], openai["refresh_token"]} {
		var token string
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		if json.Unmarshal(raw, &token) != nil {
			return nil, errors.New("credential sync: malformed refresh credential")
		}
		if token != "" {
			fingerprint := sha256.Sum256([]byte(token))
			return fingerprint[:], nil
		}
	}
	return nil, errors.New("credential sync: missing refresh credential")
}
