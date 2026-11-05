// Package streamseal authenticates attach keys and encrypts terminal frames.
package streamseal

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hpke"
	"crypto/rand"
	"encoding/binary"
	"errors"
)

// AttachContext binds a key to both endpoints and one terminal attach.
type AttachContext struct {
	DaemonID       string
	ViewerDeviceID string
	AttachID       string
	ChatID         string
}

func (ac AttachContext) fields(data []byte) []byte {
	for _, field := range []string{ac.DaemonID, ac.ViewerDeviceID, ac.AttachID, ac.ChatID} {
		data = appendField(data, []byte(field))
	}
	return data
}
func appendField(data, field []byte) []byte {
	data = binary.BigEndian.AppendUint32(data, uint32(len(field)))
	return append(data, field...)
}
func (ac AttachContext) info() []byte {
	return ac.fields([]byte("bossanova terminal attach key v1\x00"))
}
func (ac AttachContext) statement(viewerPub, box []byte) []byte {
	data := ac.fields([]byte("bossanova signed terminal attach key v1\x00"))
	return appendField(appendField(data, viewerPub), box)
}
func (ac AttachContext) valid() bool {
	return ac.DaemonID != "" && ac.ViewerDeviceID != "" && ac.AttachID != "" && ac.ChatID != ""
}

// SealedAttachKey carries the HPKE box (32-byte enc followed by ciphertext) and its signature.
type SealedAttachKey struct{ Box, Signature []byte }

// NewAttachKey generates a fresh 32-byte key for one attach.
func NewAttachKey() ([]byte, error) {
	key := make([]byte, 32)
	_, err := rand.Read(key)
	return key, err
}

// SealAttachKey seals to the viewer and authenticates the box with the daemon identity.
func SealAttachKey(viewerPub, key []byte, ac AttachContext, identity ed25519.PrivateKey) (SealedAttachKey, error) {
	if len(key) != 32 || len(identity) != ed25519.PrivateKeySize || !ac.valid() {
		return SealedAttachKey{}, errors.New("terminal seal: invalid key context")
	}
	pub, err := ecdh.X25519().NewPublicKey(viewerPub)
	if err != nil {
		return SealedAttachKey{}, err
	}
	hp, err := hpke.NewDHKEMPublicKey(pub)
	if err != nil {
		return SealedAttachKey{}, err
	}
	box, err := hpke.Seal(hp, hpke.HKDFSHA256(), hpke.AES256GCM(), ac.info(), key)
	if err != nil {
		return SealedAttachKey{}, err
	}
	return SealedAttachKey{box, ed25519.Sign(identity, ac.statement(viewerPub, box))}, nil
}

// OpenAttachKey verifies the pinned daemon signature before opening the viewer's box.
func OpenAttachKey(viewerPriv *ecdh.PrivateKey, daemonIdentity ed25519.PublicKey, ac AttachContext, sealed SealedAttachKey) ([]byte, error) {
	if viewerPriv == nil || viewerPriv.Curve() != ecdh.X25519() || len(daemonIdentity) != ed25519.PublicKeySize || !ac.valid() {
		return nil, errors.New("terminal seal: invalid viewer context")
	}
	if !ed25519.Verify(daemonIdentity, ac.statement(viewerPriv.PublicKey().Bytes(), sealed.Box), sealed.Signature) {
		return nil, errors.New("terminal seal: invalid daemon signature")
	}
	hp, err := hpke.NewDHKEMPrivateKey(viewerPriv)
	if err != nil {
		return nil, err
	}
	key, err := hpke.Open(hp, hpke.HKDFSHA256(), hpke.AES256GCM(), ac.info(), sealed.Box)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("terminal seal: invalid opened key")
	}
	return key, nil
}
