// Package daemonproof is the shared byte layout for a daemon's proof that it
// holds the machine key pinned to its daemon id (BOS-1580).
//
// bossd signs the message with a per-machine Ed25519 key when it calls
// RegisterDaemon; bosso recomputes the same message from the request and
// verifies the signature. Both sides use this package so the layout cannot
// drift between them.
//
// The message binds the daemon id, the SHA-256 of the bearer token the request
// is sent with, and the signing time. Binding the bearer hash makes a captured
// proof useless to anyone who does not also hold that user's access token, and
// the signing time bounds how long a captured proof stays usable at all.
package daemonproof

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"time"
)

// Domain separates this signature from any other use of the same key.
const Domain = "bossanova.daemon-register.v1"

// MaxSkew is how far the signing time may sit from the verifier's clock, in
// either direction, before a proof is refused as stale.
const MaxSkew = 5 * time.Minute

var (
	// ErrInvalidPublicKey reports a public key that is not an Ed25519 key.
	ErrInvalidPublicKey = errors.New("daemonproof: public key is not a valid ed25519 key")
	// ErrStale reports a signing time outside MaxSkew of the verifier's clock.
	ErrStale = errors.New("daemonproof: signing time is outside the allowed window")
	// ErrBadSignature reports a signature that does not verify.
	ErrBadSignature = errors.New("daemonproof: signature does not verify")
)

// Message returns the exact bytes a proof signs:
//
//	Domain 0x00 daemonID 0x00 hex(sha256(bearer)) 0x00 decimal(signedAt)
//
// bearer is the raw token, without the "Bearer " scheme prefix.
func Message(daemonID, bearer string, signedAt int64) []byte {
	sum := sha256.Sum256([]byte(bearer))
	buf := make([]byte, 0, len(Domain)+len(daemonID)+hex.EncodedLen(len(sum))+24)
	buf = append(buf, Domain...)
	buf = append(buf, 0)
	buf = append(buf, daemonID...)
	buf = append(buf, 0)
	buf = hex.AppendEncode(buf, sum[:])
	buf = append(buf, 0)
	buf = strconv.AppendInt(buf, signedAt, 10)
	return buf
}

// Sign returns the signature of Message(daemonID, bearer, signedAt) under key.
func Sign(key ed25519.PrivateKey, daemonID, bearer string, signedAt int64) []byte {
	return ed25519.Sign(key, Message(daemonID, bearer, signedAt))
}

// Verify checks that signature is publicKey's signature over
// Message(daemonID, bearer, signedAt) and that signedAt is within MaxSkew of
// now. It returns ErrInvalidPublicKey, ErrStale or ErrBadSignature.
func Verify(publicKey []byte, daemonID, bearer string, signedAt int64, signature []byte, now time.Time) error {
	if len(publicKey) != ed25519.PublicKeySize {
		return ErrInvalidPublicKey
	}
	skew := now.Sub(time.Unix(signedAt, 0))
	if skew > MaxSkew || skew < -MaxSkew {
		return ErrStale
	}
	if !ed25519.Verify(ed25519.PublicKey(publicKey), Message(daemonID, bearer, signedAt), signature) {
		return ErrBadSignature
	}
	return nil
}
