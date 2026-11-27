// Package proofseal encrypts hosted proof page files for the browser. Each
// page has its own 32-byte key; every content file is sealed with AES-256-GCM
// under a fresh random nonce, with the object name it is stored under as
// associated data, so a ciphertext moved to another name does not open. The
// key reaches the viewer only in the page link's #k= fragment, which browsers
// never send to a server.
//
// A sealed object is Version || nonce || ciphertext+tag, Overhead bytes longer
// than its plaintext. The loader index.html and proof-shim.js stay plaintext
// and are byte-identical on every page.
package proofseal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
)

const (
	// KeySize is the length of a page key.
	KeySize = 32
	// NonceSize is the length of the random GCM nonce each object carries.
	NonceSize = 12
	// Version is the first byte of every sealed object.
	Version byte = 0x01
	// Overhead is how many bytes sealing adds to a plaintext: the version
	// byte, the nonce and the 16-byte GCM tag.
	Overhead = 1 + NonceSize + 16
)

// ErrOpen reports a sealed object that is malformed, of another version, or
// fails authentication under the key and name it was opened with.
var ErrOpen = errors.New("proof seal: cannot open sealed object")

var errKey = errors.New("proof seal: page key must be 32 bytes")

// NewPageKey draws a fresh page key from crypto/rand.
func NewPageKey() ([]byte, error) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return key, nil
}

// Seal encrypts plain for the object stored under name, with a fresh random
// nonce.
func Seal(key []byte, name string, plain []byte) ([]byte, error) {
	nonce := make([]byte, NonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return sealWithNonce(key, nonce, name, plain)
}

func sealWithNonce(key, nonce []byte, name string, plain []byte) ([]byte, error) {
	aead, err := pageAEAD(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(plain)+Overhead)
	out = append(out, Version)
	out = append(out, nonce...)
	return aead.Seal(out, nonce, plain, []byte(name)), nil
}

// Open decrypts an object Seal produced for name under key. The browser shim
// is the production reader; Open is the Go reference for tests.
func Open(key []byte, name string, sealed []byte) ([]byte, error) {
	aead, err := pageAEAD(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < Overhead || sealed[0] != Version {
		return nil, ErrOpen
	}
	nonce, body := sealed[1:1+NonceSize], sealed[1+NonceSize:]
	plain, err := aead.Open(nil, nonce, body, []byte(name))
	if err != nil {
		return nil, ErrOpen
	}
	return plain, nil
}

func pageAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, errKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// FragmentKey encodes key as the value of a page link's #k= fragment:
// unpadded base64url.
func FragmentKey(key []byte) string {
	return base64.RawURLEncoding.EncodeToString(key)
}

// SealedSize is the stored size of a plaintext of n bytes.
func SealedSize(n int64) int64 {
	return n + Overhead
}
