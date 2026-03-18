package sqlutil

import (
	"crypto/rand"
	"encoding/hex"
)

// NewID generates a random 16-character hex ID suitable for use as a
// database primary key.
func NewID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}
