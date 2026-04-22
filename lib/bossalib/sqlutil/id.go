package sqlutil

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// NewID generates a random 16-character hex ID suitable for use as a
// database primary key. It returns an error if the system's entropy
// source is unavailable.
func NewID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return hex.EncodeToString(b), nil
}
