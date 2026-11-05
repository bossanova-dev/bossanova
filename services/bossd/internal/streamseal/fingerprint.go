package streamseal

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base32"
	"strings"
)

// ViewerFingerprint binds a viewer device id to its X25519 public key.
func ViewerFingerprint(viewerDeviceID string, pub []byte) string {
	return fingerprint("bossanova viewer device v1\x00", []byte(viewerDeviceID), pub)
}

// DaemonFingerprint identifies the daemon Ed25519 key to compare locally before pinning.
func DaemonFingerprint(identity ed25519.PublicKey) string {
	return fingerprint("bossanova daemon identity v1\x00", identity, nil)
}
func fingerprint(domain string, first, second []byte) string {
	h := sha256.New()
	h.Write([]byte(domain))
	h.Write(first)
	h.Write(second)
	digest := h.Sum(nil)
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:10])
	return strings.Join([]string{encoded[:4], encoded[4:8], encoded[8:12], encoded[12:16]}, "-")
}
