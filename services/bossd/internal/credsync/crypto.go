// Package credsync distributes end-to-end encrypted agent credentials.
package credsync

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hpke"
	"crypto/rand"
	"encoding/binary"
	"errors"
)

// CredentialAAD binds ciphertext to its owner, registry row and monotonic versions.
type CredentialAAD struct {
	UserID               string
	SyncedAgentAccountID string
	CredentialVersion    int64
	KeyGeneration        int64
}

// NewCredentialKey creates a per-user AES-256 credential key.
func NewCredentialKey() ([]byte, error) {
	key := make([]byte, 32)
	_, err := rand.Read(key)
	return key, err
}

func credentialInfo(userID string, generation int64) []byte {
	info := []byte("bossanova agent-credential key v1\x00")
	info = binary.BigEndian.AppendUint32(info, uint32(len(userID)))
	info = append(info, userID...)
	return binary.BigEndian.AppendUint64(info, uint64(generation))
}

// SealCredentialKey uses RFC 9180 DHKEM(X25519), HKDF-SHA256 and AES-256-GCM.
func SealCredentialKey(devicePub, ck []byte, userID string, generation int64) ([]byte, error) {
	if len(ck) != 32 || generation < 1 || userID == "" {
		return nil, errors.New("credential seal: invalid key context")
	}
	pub, err := ecdh.X25519().NewPublicKey(devicePub)
	if err != nil {
		return nil, err
	}
	hp, err := hpke.NewDHKEMPublicKey(pub)
	if err != nil {
		return nil, err
	}
	return hpke.Seal(hp, hpke.HKDFSHA256(), hpke.AES256GCM(), credentialInfo(userID, generation), ck)
}

// OpenCredentialKey opens a credential-key box only with its recipient's private key.
func OpenCredentialKey(devicePriv *ecdh.PrivateKey, sealed []byte, userID string, generation int64) ([]byte, error) {
	if devicePriv == nil || generation < 1 || userID == "" {
		return nil, errors.New("credential seal: invalid device context")
	}
	key, err := hpke.NewDHKEMPrivateKey(devicePriv)
	if err != nil {
		return nil, err
	}
	plain, err := hpke.Open(key, hpke.HKDFSHA256(), hpke.AES256GCM(), credentialInfo(userID, generation), sealed)
	if err == nil && len(plain) != 32 {
		return nil, errors.New("credential seal: invalid opened key")
	}
	return plain, err
}
func (a CredentialAAD) bytes() []byte {
	data := []byte("bossanova agent-credential blob v1\x00")
	for _, field := range []string{a.UserID, a.SyncedAgentAccountID} {
		data = binary.BigEndian.AppendUint32(data, uint32(len(field)))
		data = append(data, field...)
	}
	data = binary.BigEndian.AppendUint64(data, uint64(a.CredentialVersion))
	return binary.BigEndian.AppendUint64(data, uint64(a.KeyGeneration))
}
func credentialCipher(ck []byte) (cipher.AEAD, error) {
	if len(ck) != 32 {
		return nil, errors.New("credential cipher: requires 32-byte key")
	}
	block, err := aes.NewCipher(ck)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// EncryptCredential prepends a fresh 96-bit nonce to authenticated ciphertext.
func EncryptCredential(ck, plaintext []byte, aad CredentialAAD) ([]byte, error) {
	aead, err := credentialCipher(ck)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, plaintext, aad.bytes()), nil
}

// DecryptCredential rejects altered ciphertext or context.
func DecryptCredential(ck, blob []byte, aad CredentialAAD) ([]byte, error) {
	aead, err := credentialCipher(ck)
	if err != nil {
		return nil, err
	}
	if len(blob) < aead.NonceSize() {
		return nil, errors.New("credential cipher: truncated nonce")
	}
	return aead.Open(nil, blob[:aead.NonceSize()], blob[aead.NonceSize():], aad.bytes())
}
