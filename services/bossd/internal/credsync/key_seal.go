package credsync

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json"
	"errors"
)

// Signed envelopes authenticate the origin of an HPKE box. Base-mode HPKE alone
// would let an untrusted storage server inject its own known credential key.
type signedKeySeal struct {
	Version        int    `json:"version"`
	SignerDaemonID string `json:"signer_daemon_id"`
	Box            []byte `json:"box"`
	Signature      []byte `json:"signature"`
}

func keySealStatement(userID string, generation int64, recipientID string, recipientKey []byte, signerID string, box []byte) []byte {
	data := []byte("bossanova signed credential-key seal v1\x00")
	data = binary.BigEndian.AppendUint64(data, uint64(generation))
	for _, field := range [][]byte{[]byte(userID), []byte(recipientID), recipientKey, []byte(signerID), box} {
		data = binary.BigEndian.AppendUint32(data, uint32(len(field)))
		data = append(data, field...)
	}
	return data
}
func (e *Engine) signKeySeal(recipient Device, generation int64, box []byte) ([]byte, error) {
	if len(e.config.IdentityKey) != ed25519.PrivateKeySize {
		return nil, errors.New("credential sync: local signing identity unavailable")
	}
	pin, err := e.trust.PinnedIdentity(e.config.DaemonID)
	if err != nil || !bytes.Equal(pin, ed25519.PublicKey(e.config.IdentityKey[ed25519.SeedSize:])) {
		return nil, errors.New("credential sync: local signer does not match trusted identity")
	}
	seal := signedKeySeal{Version: 1, SignerDaemonID: e.config.DaemonID, Box: box}
	seal.Signature = ed25519.Sign(e.config.IdentityKey, keySealStatement(e.config.UserID, generation, recipient.DaemonID, recipient.DevicePublicKey, seal.SignerDaemonID, box))
	return json.Marshal(seal)
}
func (e *Engine) openSignedKeySeal(generation int64, encoded []byte, devices []Device) ([]byte, error) {
	highest, err := e.trust.HighestKeyGeneration()
	if err != nil {
		return nil, err
	}
	if generation < highest {
		return nil, ErrRollback
	}

	var seal signedKeySeal
	if len(encoded) > 1<<20 || json.Unmarshal(encoded, &seal) != nil || seal.Version != 1 {
		return nil, errors.New("credential sync: invalid signed key seal")
	}
	pin, err := e.trust.PinnedIdentity(seal.SignerDaemonID)
	if err != nil {
		return nil, errors.New("credential sync: key seal signer is not locally trusted")
	}
	signerVerified := false
	for _, d := range devices {
		if d.DaemonID != seal.SignerDaemonID || !bytes.Equal(d.IdentityPublicKey, pin) {
			continue
		}
		if d.Status == "enrolled" && e.previouslyAdmitted(d) {
			signerVerified = true
		}
		if d.Status == "revoked" && e.previouslyAdmitted(d) {
			previous, readErr := e.trust.AcceptedKeySeal(generation)
			signerVerified = readErr == nil && bytes.Equal(previous, encoded)
		}
	}
	if !signerVerified || !ed25519.Verify(pin, keySealStatement(e.config.UserID, generation, e.config.DaemonID, e.config.DeviceKey.PublicKey().Bytes(), seal.SignerDaemonID, seal.Box), seal.Signature) {
		return nil, errors.New("credential sync: invalid key seal origin")
	}
	key, err := OpenCredentialKey(e.config.DeviceKey, seal.Box, e.config.UserID, generation)
	if err != nil {
		return nil, err
	}
	if err = e.rememberKeySeal(generation, encoded); err != nil {
		return nil, err
	}
	return key, nil
}
func (e *Engine) rememberKeySeal(generation int64, encoded []byte) error {
	return e.trust.SaveAcceptedKeySeal(generation, encoded)
}
func (e *Engine) rememberOwnSeal(generation int64, seals []KeySeal) error {
	for _, seal := range seals {
		if seal.DaemonID == e.config.DaemonID {
			return e.rememberKeySeal(generation, seal.SealedCredentialKey)
		}
	}
	return errors.New("credential sync: own key seal missing")
}
