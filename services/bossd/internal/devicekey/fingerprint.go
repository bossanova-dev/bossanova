package devicekey

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base32"
	"strings"
)

// Fingerprint binds a daemon's pinned identity to its encryption public key.
func Fingerprint(identityPub ed25519.PublicKey, devicePub []byte) string {
	h := sha256.New()
	h.Write([]byte("bossanova agent-credential device v1\x00"))
	h.Write(identityPub)
	h.Write(devicePub)
	digest := h.Sum(nil)
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:10])
	return strings.Join([]string{encoded[:4], encoded[4:8], encoded[8:12], encoded[12:16]}, "-")
}
